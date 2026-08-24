package service

// prompt_suggest.go —— 「AI 挑提示词」：读商品图 → 在分类里挑 5 条 → 合成 1 条最终提示词。
//
// monica 2026-08-14 的原话：
//
//	「用户从灵感库的分类里先选择一个大致分类比如电商主图、网页设计等等，
//	 如果用户不知道可以选择全部这个分类。然后使用 gpt 去读取用户上传的图片，
//	 然后从该分类下的所有模版的简介选择合适的五个出来，然后再结合这五个的提示词
//	 和用户上传的图片、用户自定义输入内容、要求，给出合适的提示词。」
//
// 她拍板的两条：**5 条合成 1 条、出 1 张**（不是 5 条各出一张）；
// 工作台的「自己写一条」改成「输入商品特点」。
//
// # 为什么是三步，而不是一步把所有词丢给模型
//
// 「该分类下的所有模板」这句话直接做会爆：社交媒体帖子有 3978 条，正文平均 1616 字，
// 全塞进去是六百万字。所以：
//
//	第 0 步：把 11 个分类名 + 商品图发过去，让模型判归哪类，**并顺带给出英文检索词**。
//	         检索词是必须的：库里 15045 条标题有 15016 条是纯英文，
//	         拿运营打的中文词去搜一条都搜不到（实测「汽水」「开衫」「饮料」命中数全是 0）。
//	         运营自己选了分类时这一趟仍然要发 —— 因为还要拿检索词。
//	第 1 步（粗筛）：把该分类下**全部**提示词的标题发过去，挑出 100 条。
//	                 全量塞得下：标题平均 33 字符，最大分类 3978 条 ≈ 3.3 万 token。
//	第 2 步（细选）：只把这 100 条的**标题 + 简介**发过去，仔细挑 5 条。
//	                 简介 = JSON 形态取 "type" 字段，散文形态取开头一句。
//	第 3 步：把这 5 条的 **正文全文** + 商品图 + 运营填的商品特点发过去，合成一条。
//
// ⚠ 「简介」不是一个数据库字段，是从正文开头提炼的（见 promptBrief）：
// 1707 条正文是 JSON 结构、开头带 "type": "..."，那是作者自己写的一句话说明；
// 其余 13338 条是散文，取开头一句。
//
// # 模型的输出一律当不可信输入
//
// 三步里有两步要模型回结构化结果（分类 slug、5 个序号）。模型会跑偏：
// 回一段解释、回 markdown 代码块、回超出范围的序号、只回 3 个。
// **每一处都必须有确定性兜底**，绝不能因为模型不听话就让运营看到一个系统错误——
// 他不知道那是什么意思，也没有任何办法自救。兜底策略见各步注释。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

const (
	// suggestDigestLimit 粗筛最多看多少条简介（防炸上限，不是业务规则）。
	//
	// 最大的分类是社交媒体帖子 3978 条、标题共 13 万字符 ≈ 3.3 万 token，
	// 给 5000 足够覆盖。真有一天单个分类几万条，粗筛这条路本身要重新设计。
	suggestDigestLimit = 5000

	// suggestShortlistCount 粗筛留多少条进细选。
	//
	// monica 2026-08-14：「可以从简介里面选择 100，然后再仔细选 5 出来嘛？」
	// 100 条 × （标题 + 简介 120 字）≈ 1.5 万字符，细选那一趟塞得下。
	suggestShortlistCount = 100

	// suggestPickCount 挑几条。monica 要的是 5。
	suggestPickCount = 5

	// suggestMaxImages 一次推荐最多把几张商品图发给模型看。
	//
	// 2026-08-14 改：原来只发第一张。运营一次传 5 张时，模型只看见 1 张就写提示词，
	// 而那条词要用在全部 5 张上 —— 后面几张是别的角度甚至别的颜色时就不贴了。
	// 现在把整批都发过去（封顶 3 张），让它写一条**适合这一批**的。
	// 封顶 3 是成本考虑：图片按像素计 token，第 4 张之后边际信息很少了。
	suggestMaxImages = 3

	// suggestMaxPromptRunes 合成出来的提示词长度上限（按字符数，不是字节）。
	//
	// 超长本身不报错，但它会被原样存进 designkit_job_items.prompt_text 并发给出图模型。
	// 定 4000 是个手感值：正文平均 1616 字，合成一条通常比单条短，
	// 真到 4000 说明模型开始复述素材了，截断比原样发出去好。
	suggestMaxPromptRunes = 4000

	// suggestAllCategories 前端传空串表示「全部分类」。
	// 单独起个名字是为了让判断处读起来是「选了全部」而不是「没填」。
	suggestAllCategories = ""
)

// PromptCatalog 是本服务要用到的灵感库读能力（domain.Repository 的子集）。
//
// 刻意只声明用得上的三个方法：单测里造假对象时不用实现整个 Repository，
// 而且以后谁想扩大依赖面，得先在这里显式加一行，看得见。
type PromptCatalog interface {
	ListCategories(ctx context.Context) ([]*dkdomain.PromptCategory, error)
	// ListPromptDigests 取整个分类的简介（不拉正文全文）。粗筛靠它。
	ListPromptDigests(ctx context.Context, categoryID *int64, limit int) ([]*dkdomain.PromptDigest, error)
	// GetPromptByUID 回查正文全文。合成那一步要用。
	GetPromptByUID(ctx context.Context, uid string) (*dkdomain.Prompt, error)
}

// AssetContentLoader 取一张商品图的字节。AssetService 已经实现了这个形状。
type AssetContentLoader interface {
	AssetContent(ctx context.Context, userID int64, uid string) ([]byte, string, error)
}

// PromptSuggestDeps 造服务要的零件。
type PromptSuggestDeps struct {
	// Prompts 灵感库。
	Prompts PromptCatalog
	// Assets 取商品图字节。
	Assets AssetContentLoader
	// Chat 对话调用器（chat_invoke.go）。
	Chat ChatInvoker
	// Keys 取内部专用 Key。为 nil 时 Suggest 直接返回中文的「还没准备好」。
	Keys *InternalKeyService
	// Logger 可空。
	Logger *slog.Logger
}

// SuggestInput 一次推荐请求。
type SuggestInput struct {
	// UserID 谁在推荐。
	UserID int64
	// AssetUID 主商品图。必填——整套流程的前提就是「让模型看见这张图」。
	AssetUID string
	// ExtraAssetUIDs 这一批里的其余商品图，可空。
	//
	// 2026-08-14 加：原来只发第一张，而那条提示词要用在整批上，
	// 后面几张是别的角度甚至别的颜色时就不贴了。现在整批一起发（连主图封顶
	// suggestMaxImages 张），让模型写一条**适合这一批**的。
	ExtraAssetUIDs []string
	// CategorySlug 分类；空串 = 全部（走第 0 步让模型自己判）。
	CategorySlug string
	// Features 运营填的「商品特点」，可空。
	Features string
	// Force 跳过缓存、强制重新推荐（「重新推荐」按钮 = 运营明确要新答案）。
	// 新结果照样回写缓存，下一次相同输入命中的是这份最新的。
	Force bool
}

// SuggestCategory 最终用的是哪个分类。
//
// ⚠ 字段名对外必须序列化成 `slug` / `name`。分类**没有 uid**——
// 对外标识一直是 slug（handler/dto.go 的 promptCategoryDTO 明写「没有 id 字段」）。
// 而 `name` 要取 NameZH：表列名叫 name_zh，图省事直接用列名会让界面上分类名变成空串，
// 且不报错。
type SuggestCategory struct {
	Slug string
	Name string
}

// SuggestCandidate 模型参考过的一条。给运营看依据，不让推荐变成黑盒。
type SuggestCandidate struct {
	UID   string
	Title string
}

// SuggestResult 一次推荐的产出。
type SuggestResult struct {
	// Prompt 合成出来的最终提示词，直接可以拿去出图。
	Prompt string
	// Category 实际用的分类。选「全部」时这里是模型判出来的那个。
	Category SuggestCategory
	// Candidates 参考过的 5 条。
	Candidates []SuggestCandidate
	// Note 给运营的一句中文说明，可空（比如「没找到贴合的词，用的是这一类里的前几条」）。
	Note string
	// CachedAt 非零 = 这条来自缓存，值是它最初生成的时间。
	// 命中缓存没有调模型、没有产生新计费；零值 = 本次现算的。
	CachedAt time.Time
}

// PromptSuggestService 干活的。
type PromptSuggestService struct {
	prompts PromptCatalog
	assets  AssetContentLoader
	chat    ChatInvoker
	keys    *InternalKeyService
	log     *slog.Logger
	// cache 推荐结果的进程内缓存（见 prompt_suggest_cache.go）。
	// 一次推荐是真金白银（$0.09~$0.34），相同输入直接复用上次的答案。
	cache *suggestCache
}

// NewPromptSuggestService 造服务。三个必需零件缺一个就报错——
// 缺了还继续跑的话，失败会推迟到运营点按钮那一刻，而且报的是空指针。
func NewPromptSuggestService(deps PromptSuggestDeps) (*PromptSuggestService, error) {
	if deps.Prompts == nil {
		return nil, errors.New("designkit: 推荐服务缺少灵感库")
	}
	if deps.Assets == nil {
		return nil, errors.New("designkit: 推荐服务缺少商品图读取能力")
	}
	if deps.Chat == nil {
		return nil, errors.New("designkit: 推荐服务缺少对话调用器")
	}
	return &PromptSuggestService{
		prompts: deps.Prompts,
		assets:  deps.Assets,
		chat:    deps.Chat,
		keys:    deps.Keys,
		log:     deps.Logger,
		cache:   newSuggestCache(suggestCacheCapacity, suggestCacheTTL),
	}, nil
}

func (s *PromptSuggestService) logger() *slog.Logger {
	if s.log != nil {
		return s.log
	}
	return slog.Default()
}

// Suggest 走完三步，返回一条可以直接出图的提示词。
func (s *PromptSuggestService) Suggest(ctx context.Context, in SuggestInput) (*SuggestResult, error) {
	if s == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal)
	}
	if in.UserID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUnauthorized)
	}
	if strings.TrimSpace(in.AssetUID) == "" {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("请先上传一张商品图，AI 要看着图才能推荐。")
	}
	// ⚠ 没有 Key 就没法计费，而上游对话取不到 Key 直接 401。
	// 这里给中文出路，不要让它退化成「系统内部错误」。
	if s.keys == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
			WithMessage("账号还没开通出图权限，请联系管理员。")
	}

	// 缓存：输入完全相同（同一批图 + 同分类 + 同一句商品特点）时直接复用上次的答案，
	// 一趟模型都不调、一分钱不花。Force（「重新推荐」按钮）跳过查询，
	// 但算出来的新结果照样回写。命中的结果带 CachedAt，前端据此说明「这是缓存的」。
	cacheKey := suggestCacheKey(in.UserID,
		append([]string{in.AssetUID}, in.ExtraAssetUIDs...),
		in.CategorySlug, in.Features, DefaultChatModel)
	if !in.Force {
		if hit, ok := s.cache.Get(cacheKey); ok {
			return hit, nil
		}
	}

	apiKey, err := s.keys.EnsureInternalKey(ctx, in.UserID)
	if err != nil {
		return nil, err
	}

	// 商品图：三步共用同一批，只读一次。
	//
	// 主图读不出来直接失败（那是整套流程的前提）；其余几张读不出来只跳过——
	// 为一张配图把整次推荐打回去，运营完全不知道该怎么办。
	images := make([]ChatImage, 0, suggestMaxImages)
	imgData, imgType, err := s.assets.AssetContent(ctx, in.UserID, strings.TrimSpace(in.AssetUID))
	if err != nil {
		return nil, err
	}
	images = append(images, ChatImage{Data: imgData, ContentType: imgType})
	for _, uid := range in.ExtraAssetUIDs {
		if len(images) >= suggestMaxImages {
			break
		}
		uid = strings.TrimSpace(uid)
		if uid == "" || uid == strings.TrimSpace(in.AssetUID) {
			continue
		}
		data, contentType, readErr := s.assets.AssetContent(ctx, in.UserID, uid)
		if readErr != nil {
			s.logger().Warn("designkit 推荐时有一张商品图读不出来，已跳过",
				slog.String("asset_uid", uid), slog.Any("error", readErr))
			continue
		}
		images = append(images, ChatImage{Data: data, ContentType: contentType})
	}

	// 计费 id 的 scope 必须**每次推荐都不一样**。
	//
	// ⚠ 2026-08-14 踩过：原来用主图 uid 当 scope，于是同一张商品图重复推荐时
	// 三趟的 request_id 一模一样，上游幂等表（request_id, api_key_id 唯一键）
	// 直接判成重复调用，**第二次起一分钱不扣、也不写账单**。
	// 现象极隐蔽：功能完全正常，只是账目对不上 —— 运营点十次「重新推荐」，
	// 账单里只有第一次。所以这里必须现生成一个 ULID。
	scope := newAssetULID()
	newReq := func(step int, system, text string) ChatRequest {
		return ChatRequest{
			UserID:    in.UserID,
			APIKeyID:  apiKey.ID,
			System:    system,
			UserText:  text,
			Images:    images,
			RequestID: BuildChatBillingRequestID(scope, step),
		}
	}

	categories, err := s.prompts.ListCategories(ctx)
	if err != nil {
		return nil, mapJobRepoError(err, dkdomain.ErrCodeInternal)
	}
	if len(categories) == 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("灵感库还没有内容，请先让管理员同步一次。")
	}

	notes := make([]string, 0, 2)

	// ---- 第 0 步：看图，定分类 + 产出英文检索词 ----
	//
	// 分类和检索词在**同一趟**里要，不是两趟：模型反正要看这几张图，
	// 顺带多回一个 keywords 数组几乎不花钱，而分成两趟就是多一次读图的钱和等待。
	analysis, note, err := s.analyzeImage(ctx, in, categories, newReq)
	if err != nil {
		return nil, err
	}
	category := analysis.category
	if note != "" {
		notes = append(notes, note)
	}

	// ---- 第 1 步：挑 5 条 ----
	picked, note, err := s.pickCandidates(ctx, in, category, newReq)
	if err != nil {
		return nil, err
	}
	if note != "" {
		notes = append(notes, note)
	}

	// ---- 第 2 步：合成 1 条 ----
	final, err := s.composePrompt(ctx, in, picked, newReq)
	if err != nil {
		return nil, err
	}

	candidates := make([]SuggestCandidate, 0, len(picked))
	for _, p := range picked {
		candidates = append(candidates, SuggestCandidate{UID: p.UID, Title: p.Title})
	}

	result := &SuggestResult{
		Prompt: final,
		Category: SuggestCategory{
			Slug: category.Slug,
			Name: categoryDisplayName(category),
		},
		Candidates: candidates,
		Note:       strings.Join(notes, " "),
	}
	// 只缓存成功的结果；失败不缓存 —— 缓存一次失败等于让运营连错 24 小时。
	s.cache.Put(cacheKey, *result)
	return result, nil
}

// ---------------------------------------------------------------------------
// 第 0 步：定分类
// ---------------------------------------------------------------------------

// imageAnalysis 是第 0 步看图看出来的东西。
type imageAnalysis struct {
	// category 这次按哪个分类挑。
	category *dkdomain.PromptCategory
}

// analyzeImage 第 0 步：看图，同时定下分类和检索词。
//
// 运营选了具体分类时**这一趟仍然要发** —— 因为还要拿检索词。
// 只是那种情况下模型回的分类会被忽略，用运营选的。
//
// 模型答不上来一律兜底，绝不报错：
//   - 分类判不出 → 用排序第一的分类（分类是按 sort_order 排的，
//     第一个是管理员认为最该先看到的那个），并在界面上说明；
//   - 检索词给不出 → 返回空数组，候选池整个走随机取。随机取本来就是兜底路径，
//     不是降级，只是没那么贴。
func (s *PromptSuggestService) analyzeImage(
	ctx context.Context,
	in SuggestInput,
	categories []*dkdomain.PromptCategory,
	newReq func(step int, system, text string) ChatRequest,
) (imageAnalysis, string, error) {
	chosen, pinned := findCategoryBySlug(categories, in.CategorySlug)
	if pinned && chosen == nil {
		return imageAnalysis{}, "", dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("选中的分类不存在，请刷新页面重新选。")
	}

	names := make([]string, 0, len(categories))
	for _, c := range categories {
		if c == nil {
			continue
		}
		names = append(names, fmt.Sprintf("- %s（标识 %s）", categoryDisplayName(c), c.Slug))
	}

	text := fmt.Sprintf(`看这些商品图，从下面的分类里选出最适合给它做图的一个：
%s


%s

只回一个 JSON 对象，形如：{"category":"分类标识"}
不要解释、不要代码块标记。`,
		strings.Join(names, "\n"), featuresBlock(in.Features))

	fallback := func(reason string) (imageAnalysis, string, error) {
		if chosen != nil {
			// 运营自己选了分类，那就只是没拿到检索词，不用向他解释什么。
			return imageAnalysis{category: chosen}, "", nil
		}
		guess := widestCategory(categories)
		return imageAnalysis{category: guess},
			fmt.Sprintf("没能自动判断分类，按「%s」推荐的。", categoryDisplayName(guess)), nil
	}

	res, err := s.chat.Chat(ctx, newReq(0, suggestSystemPrompt, text))
	if err != nil {
		s.logger().Warn("designkit 看图分析失败，退回默认分类和随机候选", slog.Any("error", err))
		return fallback("chat failed")
	}

	parsed := parseAnalysis(res.Text)

	// 分类：运营选了就用运营的；没选就用模型判的；模型也没判出来才兜底。
	category := chosen
	note := ""
	if category == nil {
		if c, ok := findCategoryBySlug(categories, parsed.Category); ok && c != nil {
			category = c
		} else {
			guess := widestCategory(categories)
			category = guess
			note = fmt.Sprintf("没能自动判断分类，按「%s」推荐的。", categoryDisplayName(guess))
			s.logger().Warn("designkit 判分类的回答对不上任何分类，退回默认分类",
				slog.String("answer", gatewaySnippet([]byte(res.Text))))
		}
	}

	return imageAnalysis{category: category}, note, nil
}

// analysisPayload 是第 0 步期望的 JSON 形状。
type analysisPayload struct {
	Category string `json:"category"`
}

// parseAnalysis 从模型的回答里抠出 JSON 对象。
//
// 解不开就返回空值——调用方对「分类空」和「检索词空」都有兜底，
// 这里不需要区分「模型没回」和「回了但解不开」。
func parseAnalysis(answer string) analysisPayload {
	answer = stripCodeFence(answer)
	start := strings.Index(answer, "{")
	end := strings.LastIndex(answer, "}")
	if start < 0 || end <= start {
		return analysisPayload{}
	}
	var out analysisPayload
	if err := json.Unmarshal([]byte(answer[start:end+1]), &out); err != nil {
		return analysisPayload{}
	}
	return out
}

// findCategoryBySlug 按 slug 找分类。
// 第二个返回值表示「调用方确实指定了一个 slug」（用来区分「没选」和「选了但找不到」）。
func findCategoryBySlug(categories []*dkdomain.PromptCategory, slug string) (*dkdomain.PromptCategory, bool) {
	slug = strings.TrimSpace(slug)
	if slug == suggestAllCategories {
		return nil, false
	}
	for _, c := range categories {
		if c != nil && strings.EqualFold(c.Slug, slug) {
			return c, true
		}
	}
	return nil, true
}

// ---------------------------------------------------------------------------
// 第 1 步：挑 5 条
// ---------------------------------------------------------------------------

// pickCandidates 从整个分类里挑出 5 条。**两段式**（monica 2026-08-14 定的）。
//
//	粗筛：把该分类下**全部**提示词的简介（标题 + 正文开头一小段）发给模型，
//	      让它挑出 suggestShortlistCount 条。
//	细选：只把这几条的简介发过去，让它仔细挑 5 条。
//
// 为什么不是「随机取 100 条再挑 5」（那是 2026-08-14 早些时候的做法，已废弃）：
// 随机取是碰运气 —— 社交媒体帖子 3978 条，随机 100 条里可能一条贴合的都没有。
// 而且更早那一版更糟：用 ListPrompts（ORDER BY id）取前 100 条，
// **每次都是同样那 100 条**，另外 3878 条永远不可能被选中，且完全静默。
//
// 为什么全量粗筛塞得下：标题平均 33 字符，最大的分类 3978 条 = 13 万字符 ≈ 3.3 万 token。
// 简介（正文前 200 字符）只在细选阶段给那几十条用，不会在粗筛阶段乘以 3978。
//
// 分类本来就不足 suggestShortlistCount 条时**跳过粗筛**，直接进细选 ——
// 省一趟对话的钱和十来秒等待。
func (s *PromptSuggestService) pickCandidates(
	ctx context.Context,
	in SuggestInput,
	category *dkdomain.PromptCategory,
	newReq func(step int, system, text string) ChatRequest,
) ([]*dkdomain.Prompt, string, error) {
	note := ""
	categoryID := category.ID

	digests, err := s.prompts.ListPromptDigests(ctx, &categoryID, suggestDigestLimit)
	if err != nil {
		return nil, "", mapJobRepoError(err, dkdomain.ErrCodeInternal)
	}
	if len(digests) == 0 {
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessagef("「%s」这个分类下还没有提示词，换一个分类试试。", categoryDisplayName(category))
	}

	// ---- 粗筛：全量简介 → shortlist ----
	shortlist := digests
	if len(digests) > suggestShortlistCount {
		picked, pickNote := s.shortlist(ctx, in, digests, newReq)
		shortlist = picked
		if pickNote != "" {
			note = pickNote
		}
	}

	// 细选之前就已经不足 5 条：全给，不再花一趟对话。
	if len(shortlist) <= suggestPickCount {
		return s.loadPrompts(ctx, shortlist)
	}

	// ---- 细选：shortlist 的简介 → 5 条 ----
	lines := make([]string, 0, len(shortlist))
	for i, d := range shortlist {
		lines = append(lines, fmt.Sprintf("%d. %s\n   %s", i+1, promptDigestTitle(d), promptBrief(d)))
	}
	text := fmt.Sprintf(`看这些商品图，从下面这些提示词模板里挑出**最适合**它的 %d 条。

模板（每条两行：标题 + 简介）：
%s

%s

只回一个 JSON 数组，里面是 %d 个序号，例如 [3,17,25,41,58]。不要解释、不要代码块标记。`,
		suggestPickCount, strings.Join(lines, "\n"), featuresBlock(in.Features), suggestPickCount)

	res, err := s.chat.Chat(ctx, newReq(2, suggestSystemPrompt, text))
	if err != nil {
		s.logger().Warn("designkit 细选提示词失败，退回粗筛结果的前几条", slog.Any("error", err))
		return s.loadPromptsNoted(ctx, shortlist[:suggestPickCount],
			"AI 没能挑出最合适的几条，用的是初筛结果里靠前的几条。")
	}

	idx := parseIndexList(res.Text, len(shortlist))
	if len(idx) < suggestPickCount {
		seen := make(map[int]bool, len(idx))
		for _, i := range idx {
			seen[i] = true
		}
		for i := 0; i < len(shortlist) && len(idx) < suggestPickCount; i++ {
			if !seen[i] {
				idx = append(idx, i)
				seen[i] = true
			}
		}
		note = "AI 只挑出了几条，其余是按顺序补的。"
	}
	if len(idx) > suggestPickCount {
		idx = idx[:suggestPickCount]
	}

	chosen := make([]*dkdomain.PromptDigest, 0, len(idx))
	for _, i := range idx {
		if i >= 0 && i < len(shortlist) {
			chosen = append(chosen, shortlist[i])
		}
	}
	if len(chosen) == 0 {
		chosen = shortlist[:suggestPickCount]
		note = "AI 没能挑出最合适的几条，用的是初筛结果里靠前的几条。"
	}

	prompts, _, err := s.loadPrompts(ctx, chosen)
	return prompts, note, err
}

// shortlist 粗筛：把全分类的简介发过去，挑出 suggestShortlistCount 条。
//
// 失败时**退回按顺序取前 N 条**并给一句说明 —— 粗筛挂了不该让整次推荐失败，
// 它后面还有细选那一关兜着。
func (s *PromptSuggestService) shortlist(
	ctx context.Context,
	in SuggestInput,
	digests []*dkdomain.PromptDigest,
	newReq func(step int, system, text string) ChatRequest,
) ([]*dkdomain.PromptDigest, string) {
	// ⚠ 粗筛**只发标题**，不发简介：简介是正文前 200 字符，
	// 3978 条乘上去就是 80 万字符，粗筛的意义（便宜地过一遍全量）就没了。
	// 简介留到细选阶段用，那时候只剩几十条。
	lines := make([]string, 0, len(digests))
	for i, d := range digests {
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, promptDigestTitle(d)))
	}

	text := fmt.Sprintf(`看这些商品图，从下面这一整份提示词模板清单里，初步挑出**可能合适**的 %d 条。
这一步是粗筛，宁可多留几条也不要漏掉可能合适的。

清单：
%s

%s

只回一个 JSON 数组，里面是不超过 %d 个序号。不要解释、不要代码块标记。`,
		suggestShortlistCount, strings.Join(lines, "\n"),
		featuresBlock(in.Features), suggestShortlistCount)

	res, err := s.chat.Chat(ctx, newReq(1, suggestSystemPrompt, text))
	if err != nil {
		s.logger().Warn("designkit 粗筛失败，退回按顺序取前若干条", slog.Any("error", err))
		return digests[:suggestShortlistCount], "AI 初筛没成功，是从这一类靠前的一批里选的。"
	}

	idx := parseIndexList(res.Text, len(digests))
	if len(idx) == 0 {
		s.logger().Warn("designkit 粗筛的回答里没有可用序号，退回按顺序取前若干条",
			slog.String("answer", gatewaySnippet([]byte(res.Text))))
		return digests[:suggestShortlistCount], "AI 初筛没成功，是从这一类靠前的一批里选的。"
	}

	out := make([]*dkdomain.PromptDigest, 0, len(idx))
	for _, i := range idx {
		if i >= 0 && i < len(digests) {
			out = append(out, digests[i])
		}
	}
	// 粗筛只挑出两三条也没关系：下面细选那一步会因为「不足 5 条」直接全给。
	if len(out) == 0 {
		return digests[:suggestShortlistCount], "AI 初筛没成功，是从这一类靠前的一批里选的。"
	}
	return out, ""
}

// loadPrompts 把简介回查成完整的提示词（合成那一步要正文全文）。
func (s *PromptSuggestService) loadPrompts(ctx context.Context, digests []*dkdomain.PromptDigest) ([]*dkdomain.Prompt, string, error) {
	return s.loadPromptsNoted(ctx, digests, "")
}

func (s *PromptSuggestService) loadPromptsNoted(ctx context.Context, digests []*dkdomain.PromptDigest, note string) ([]*dkdomain.Prompt, string, error) {
	out := make([]*dkdomain.Prompt, 0, len(digests))
	for _, d := range digests {
		if d == nil {
			continue
		}
		p, err := s.prompts.GetPromptByUID(ctx, d.UID)
		if err != nil || p == nil {
			// 少一条不该让整次推荐失败：合成那一步用 4 条参考照样写得出来。
			s.logger().Warn("designkit 回查提示词正文失败，跳过这一条",
				slog.String("uid", d.UID), slog.Any("error", err))
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithMessage("挑出来的提示词都读不出来了，请再试一次。")
	}
	return out, note, nil
}

// promptDigestTitle 标题；空的话退到简介开头。给模型一个空标题等于让它盲挑。
func promptDigestTitle(d *dkdomain.PromptDigest) string {
	if d == nil {
		return ""
	}
	if title := strings.TrimSpace(d.Title); title != "" {
		return title
	}
	return truncateRunes(strings.Join(strings.Fields(d.Brief), " "), 40)
}

// promptBrief 把正文开头提炼成一句「这条大概是干什么的」。
//
// 库里两种形态（2026-08-14 实测：1707 条 JSON、13338 条散文）：
//
//	JSON：  {"type": "SaaS landing page hero graphic", "style": ...
//	        → 取 type 字段，它就是作者自己写的一句话说明，比标题还准。
//	散文：  A {art_style} of {main_character} in mid-combat, wearing ornate...
//	        → 取开头一句。
//
// 占位符（{art_style} 这种）**原样保留**：它们本身就在告诉模型
// 「这条模板是可以换主体/风格的」，抹掉反而丢信息。
func promptBrief(d *dkdomain.PromptDigest) string {
	if d == nil {
		return ""
	}
	brief := strings.TrimSpace(d.Brief)
	if brief == "" {
		return ""
	}
	if strings.HasPrefix(brief, "{") {
		if t := extractJSONType(brief); t != "" {
			return t
		}
	}
	return truncateRunes(strings.Join(strings.Fields(brief), " "), 120)
}

// jsonTypePattern 从被截断的 JSON 片段里抠 "type": "..."。
//
// ⚠ 不能用 json.Unmarshal：Brief 是正文的**前 200 个字符**，一定是残缺的 JSON。
// 正则在这里不是偷懒，是唯一可行的做法。
var jsonTypePattern = regexp.MustCompile(`"type"\s*:\s*"([^"]{1,120})"`)

func extractJSONType(brief string) string {
	if m := jsonTypePattern.FindStringSubmatch(brief); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// ---------------------------------------------------------------------------
// 第 2 步：合成 1 条
// ---------------------------------------------------------------------------

// composePrompt 把 5 条正文 + 商品图 + 商品特点合成一条最终提示词。
func (s *PromptSuggestService) composePrompt(
	ctx context.Context,
	in SuggestInput,
	picked []*dkdomain.Prompt,
	newReq func(step int, system, text string) ChatRequest,
) (string, error) {
	blocks := make([]string, 0, len(picked))
	for i, p := range picked {
		if p == nil {
			continue
		}
		blocks = append(blocks, fmt.Sprintf("【参考 %d：%s】\n%s",
			i+1, promptDisplayTitle(p), strings.TrimSpace(p.Body)))
	}

	text := fmt.Sprintf(`看这张商品图，参考下面这些提示词模板，写出**一条**用来给这个商品出图的提示词。

%s

%s

要求：
- 只输出提示词本身，不要标题、不要解释、不要代码块标记、不要分点。
- 贴着图里这个商品的实际样子写，不要凭空加图里没有的东西。
- 把上面几条参考的长处融进去，但**不要照抄**，也不要保留 {这种} 占位符。
- **绝对不要写画面比例、构图尺寸相关的话**（正方形、方形、竖版、横版、1:1、16:9、
  square、portrait 这一类都不要）。比例由运营在下一步单独选，系统会自己加上去。
  你在这里写了比例，就会和运营选的那个**互相打架**。
- 用中文写。`,
		strings.Join(blocks, "\n\n"), featuresBlock(in.Features))

	res, err := s.chat.Chat(ctx, newReq(3, suggestSystemPrompt, text))
	if err != nil {
		return "", err
	}

	final := strings.TrimSpace(stripCodeFence(res.Text))
	if final == "" {
		return "", dkdomain.NewError(dkdomain.ErrCodeUpstreamError).
			WithMessage("AI 这次没给出提示词，请再点一次「让 AI 推荐」。")
	}
	if runes := []rune(final); len(runes) > suggestMaxPromptRunes {
		// 截断而不是报错：一条太长的提示词照样能出图，而报错等于让运营白等一趟。
		final = strings.TrimSpace(string(runes[:suggestMaxPromptRunes]))
		s.logger().Warn("designkit 合成的提示词超长，已截断",
			slog.Int("limit", suggestMaxPromptRunes))
	}
	return final, nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// suggestSystemPrompt 三步共用的系统提示词。
//
// 只说两件事：你是干什么的、**严格按格式回**。
// 后者是重点——三步里有两步要解析结构化结果，模型爱加解释和代码块标记。
// 即便这么说了也仍然会跑偏，所以解析侧还有兜底（parseIndexList / stripCodeFence）。
const suggestSystemPrompt = "你是电商视觉设计的资深助理。回答必须严格按用户要求的格式，" +
	"不要寒暄、不要解释、不要加代码块标记。"

// featuresBlock 把运营填的商品特点拼成一段；没填就返回空串。
func featuresBlock(features string) string {
	features = strings.TrimSpace(features)
	if features == "" {
		return ""
	}
	return "运营补充的商品特点和要求：\n" + features
}

// categoryDisplayName 分类显示名。
//
// 中文名 → 英文名 → slug 三级兜底，跟 handler/dto.go 的口径一致：
// 宁可显示一个英文标识，也不要在界面上留一行空白让运营点不中。
func categoryDisplayName(c *dkdomain.PromptCategory) string {
	if c == nil {
		return ""
	}
	if name := strings.TrimSpace(c.NameZH); name != "" {
		return name
	}
	if name := strings.TrimSpace(c.NameEN); name != "" {
		return name
	}
	return c.Slug
}

// promptDisplayTitle 提示词的标题。
//
// 标题空着时退到正文前 40 个字符——这是「简介」这件事的最后一道兜底：
// 给模型一个空标题等于让它盲挑。
func promptDisplayTitle(p *dkdomain.Prompt) string {
	if p == nil {
		return ""
	}
	if title := strings.TrimSpace(p.Title); title != "" {
		return title
	}
	body := strings.Join(strings.Fields(p.Body), " ")
	if runes := []rune(body); len(runes) > 40 {
		return string(runes[:40]) + "…"
	}
	return body
}

// widestCategory 条数最多的分类。这里没有条数信息，退化成「第一个」——
// 分类本身是按 sort_order 排的，第一个就是管理员认为最该先看到的那个。
func widestCategory(categories []*dkdomain.PromptCategory) *dkdomain.PromptCategory {
	for _, c := range categories {
		if c != nil {
			return c
		}
	}
	return nil
}

// stripCodeFence 去掉模型爱加的 ```json ... ``` 包裹。
//
// 系统提示词里已经说了不要加，但它照样会加——这是实测过的普遍现象，
// 不能指望「说了就不会」。
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = s[idx+1:]
	}
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// parseIndexList 从模型的回答里抠出序号，转成 0 基下标并去重。
//
// 两条路：先按 JSON 数组解；解不开就用正则把所有数字抓出来。
// 后者是必须的——模型经常回「我选择第 3、17、25、41、58 条」这种。
//
// 越界的一律丢掉（模型会编出 120 这种超出候选数的序号）。
func parseIndexList(answer string, poolSize int) []int {
	answer = stripCodeFence(answer)

	seen := make(map[int]bool, suggestPickCount)
	out := make([]int, 0, suggestPickCount)
	add := func(oneBased int) {
		i := oneBased - 1
		if i < 0 || i >= poolSize || seen[i] {
			return
		}
		seen[i] = true
		out = append(out, i)
	}

	// 先试 JSON 数组。
	if start := strings.Index(answer, "["); start >= 0 {
		if end := strings.Index(answer[start:], "]"); end >= 0 {
			var nums []int
			if err := json.Unmarshal([]byte(answer[start:start+end+1]), &nums); err == nil {
				for _, n := range nums {
					add(n)
				}
				if len(out) > 0 {
					return out
				}
			}
		}
	}

	// 退回抓数字。
	for _, m := range regexp.MustCompile(`\d+`).FindAllString(answer, -1) {
		n, err := strconv.Atoi(m)
		if err != nil {
			continue
		}
		add(n)
		if len(out) >= suggestPickCount {
			break
		}
	}
	return out
}
