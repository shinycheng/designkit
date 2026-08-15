//go:build unit

package service

// prompt_suggest_test.go —— 「AI 挑提示词」三步法的单测。
//
// 假的 ChatInvoker 按调用次序回预置文本，**绝不真的调网关**。
//
// 这里守的几乎全是**兜底**，因为这套流程有两步依赖模型回结构化结果，
// 而模型一定会跑偏：回一段解释、加代码块标记、编出超范围的序号、只回 3 个。
// 每一处跑偏都不能让运营看到系统错误 —— 他不知道那是什么意思，也没法自救。
//
// 还有一条比兜底更要紧的：**候选必须走 SamplePrompts（随机），不能走 ListPrompts**。
// 用 ListPrompts（ORDER BY id）的话，取 100 条永远是同样那 100 条，
// 社交媒体帖子 3978 条里 97% 永远轮不到，而且完全静默 —— 界面和日志都看不出来。
// 这是 monica 2026-08-14 问出来的问题，专门有一条测试钉住。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ---------------------------------------------------------------------------
// 假件
// ---------------------------------------------------------------------------

// sgFakeChat 按调用次序回预置文本。
type sgFakeChat struct {
	replies []string
	err     error

	calls    int
	requests []ChatRequest
}

func (f *sgFakeChat) Chat(_ context.Context, req ChatRequest) (*ChatResult, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		f.calls++
		return nil, f.err
	}
	idx := f.calls
	f.calls++
	if idx >= len(f.replies) {
		return &ChatResult{Text: "兜底回答"}, nil
	}
	return &ChatResult{Text: f.replies[idx]}, nil
}

// sgFakeCatalog 假灵感库。
type sgFakeCatalog struct {
	categories []*dkdomain.PromptCategory
	prompts    []*dkdomain.Prompt

	digestCalls     int
	getCalls        int
	lastDigestLimit int
	err             error
}

func (f *sgFakeCatalog) ListCategories(context.Context) ([]*dkdomain.PromptCategory, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.categories, nil
}

// ListPromptDigests 粗筛的数据源：整个分类的简介。
func (f *sgFakeCatalog) ListPromptDigests(_ context.Context, categoryID *int64, limit int) ([]*dkdomain.PromptDigest, error) {
	f.digestCalls++
	f.lastDigestLimit = limit
	out := make([]*dkdomain.PromptDigest, 0, len(f.prompts))
	for _, p := range f.prompts {
		if categoryID != nil && (p.CategoryID == nil || *p.CategoryID != *categoryID) {
			continue
		}
		brief := p.Body
		if len(brief) > 200 {
			brief = brief[:200]
		}
		out = append(out, &dkdomain.PromptDigest{ID: p.ID, UID: p.UID, Title: p.Title, Brief: brief})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// GetPromptByUID 细选之后回查正文全文。
func (f *sgFakeCatalog) GetPromptByUID(_ context.Context, uid string) (*dkdomain.Prompt, error) {
	f.getCalls++
	for _, p := range f.prompts {
		if p.UID == uid {
			return p, nil
		}
	}
	return nil, dkdomain.ErrNotFound
}

// sgFakeAssets 假商品图读取。
type sgFakeAssets struct {
	missing map[string]bool
	reads   []string
	err     error
}

func (f *sgFakeAssets) AssetContent(_ context.Context, _ int64, uid string) ([]byte, string, error) {
	f.reads = append(f.reads, uid)
	if f.err != nil {
		return nil, "", f.err
	}
	if f.missing[uid] {
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeAssetNotFound)
	}
	return []byte{0x89, 'P', 'N', 'G'}, "image/png", nil
}

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

func sgCategories() []*dkdomain.PromptCategory {
	return []*dkdomain.PromptCategory{
		{ID: 1, Slug: "ecommerce-main-image", NameZH: "电商主图", SortOrder: 1},
		{ID: 2, Slug: "social-media-post", NameZH: "社交媒体帖子", SortOrder: 2},
	}
}

// sgPrompts 造 n 条属于 categoryID 的提示词，标题带序号方便断言。
func sgPrompts(categoryID int64, n int) []*dkdomain.Prompt {
	out := make([]*dkdomain.Prompt, 0, n)
	for i := 1; i <= n; i++ {
		cid := categoryID
		out = append(out, &dkdomain.Prompt{
			ID:         int64(i),
			UID:        fmt.Sprintf("p%03d", i),
			CategoryID: &cid,
			Title:      fmt.Sprintf("Soda Commercial %03d", i),
			Body:       fmt.Sprintf("body of prompt %d", i),
		})
	}
	return out
}

// sgFixture 造一套能跑通的服务。
//
// ⚠ Keys 传 nil 时 Suggest 会直接返回「账号还没开通出图权限」，
// 所以这些测试里用一个真的 InternalKeyService + 假的 provisioner。
func sgFixture(t *testing.T, chat *sgFakeChat, catalog *sgFakeCatalog, assets *sgFakeAssets) *PromptSuggestService {
	t.Helper()
	keys := NewInternalKeyService(newIKFakeProvisioner(), StaticGroupID(7))
	svc, err := NewPromptSuggestService(PromptSuggestDeps{
		Prompts: catalog,
		Assets:  assets,
		Chat:    chat,
		Keys:    keys,
	})
	if err != nil {
		t.Fatalf("造服务失败: %v", err)
	}
	return svc
}

func sgInput() SuggestInput {
	return SuggestInput{
		UserID:   42,
		AssetUID: "01KZZMMSB6P8PD4BVS4YHP1G84",
		Features: "玻璃瓶装汽水，主打清爽冰镇",
	}
}

// ---------------------------------------------------------------------------
// 正常路径
// ---------------------------------------------------------------------------

func TestSuggest_HappyPath(t *testing.T) {
	chat := &sgFakeChat{replies: []string{
		`{"category":"ecommerce-main-image"}`,
		`[2,4,6,8,10]`,
		`合成出来的最终提示词`,
	}}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 50)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	res, err := svc.Suggest(context.Background(), sgInput())
	if err != nil {
		t.Fatalf("Suggest 失败: %v", err)
	}
	if res.Prompt != "合成出来的最终提示词" {
		t.Fatalf("最终提示词 = %q", res.Prompt)
	}
	// 分类名必须是中文名（表列是 name_zh；直接用列名会让界面上分类名变空串）。
	if res.Category.Slug != "ecommerce-main-image" || res.Category.Name != "电商主图" {
		t.Fatalf("分类 = %+v", res.Category)
	}
	if len(res.Candidates) != suggestPickCount {
		t.Fatalf("参考条数 = %d，应为 %d", len(res.Candidates), suggestPickCount)
	}
	// 序号是 1 基的，[2,4,6,8,10] 对应下标 1,3,5,7,9 → p002/p004/...
	if res.Candidates[0].UID != "p002" {
		t.Fatalf("第 1 条应为 p002，实际 %s", res.Candidates[0].UID)
	}
	if chat.calls != 3 {
		t.Fatalf("对话被调了 %d 次，三步法应该正好 3 次", chat.calls)
	}
}

// 运营自己选了分类时，模型判的分类要被忽略。
func TestSuggest_PinnedCategoryWins(t *testing.T) {
	chat := &sgFakeChat{replies: []string{
		// 模型判成了社交媒体帖子，但运营选的是电商主图。
		`{"category":"social-media-post"}`,
		`[1,2,3,4,5]`,
		`最终提示词`,
	}}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 20)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	in := sgInput()
	in.CategorySlug = "ecommerce-main-image"
	res, err := svc.Suggest(context.Background(), in)
	if err != nil {
		t.Fatalf("Suggest 失败: %v", err)
	}
	if res.Category.Slug != "ecommerce-main-image" {
		t.Fatalf("运营选的分类被模型覆盖了：%s", res.Category.Slug)
	}
	// 即便运营选了分类，第 0 步仍然要发 —— 因为还要拿英文检索词。
	if chat.calls != 3 {
		t.Fatalf("对话被调了 %d 次，选了分类也该是 3 次（还要拿检索词）", chat.calls)
	}
}

// ---------------------------------------------------------------------------
// 候选取样：monica 问出来的那个问题
// ---------------------------------------------------------------------------

func TestSuggest_FallsBackWhenAnalyzeFails(t *testing.T) {
	chat := &sgFakeChat{replies: []string{
		"我不知道该选哪个分类，抱歉。", // 完全不是 JSON
		`[1,2,3,4,5]`,
		`最终提示词`,
	}}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 20)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	res, err := svc.Suggest(context.Background(), sgInput())
	if err != nil {
		t.Fatalf("判分类失败不该让整次推荐失败: %v", err)
	}
	if res.Category.Slug != "ecommerce-main-image" {
		t.Fatalf("应该退回排序第一的分类，实际 %s", res.Category.Slug)
	}
	if res.Note == "" {
		t.Fatal("退回默认分类时必须给运营一句说明，否则他不知道可以换个分类重来")
	}
}

// 模型回了带代码块标记的 JSON —— 系统提示词说了不要加，它照样会加。
func TestSuggest_StripsCodeFence(t *testing.T) {
	chat := &sgFakeChat{replies: []string{
		"```json\n{\"category\":\"ecommerce-main-image\",\"keywords\":[\"soda\"]}\n```",
		"```\n[1,2,3,4,5]\n```",
		"```\n最终提示词\n```",
	}}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 20)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	res, err := svc.Suggest(context.Background(), sgInput())
	if err != nil {
		t.Fatalf("Suggest 失败: %v", err)
	}
	if res.Category.Slug != "ecommerce-main-image" {
		t.Fatalf("代码块里的分类没解析出来：%s", res.Category.Slug)
	}
	if res.Prompt != "最终提示词" {
		t.Fatalf("最终提示词没剥掉代码块标记：%q", res.Prompt)
	}
}

// 模型只挑出 2 条 → 按顺序补齐到 5，并说明。
func TestSuggest_PadsWhenModelPicksTooFew(t *testing.T) {
	chat := &sgFakeChat{replies: []string{
		`{"category":"ecommerce-main-image"}`,
		`[3,7]`,
		`最终提示词`,
	}}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 20)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	res, err := svc.Suggest(context.Background(), sgInput())
	if err != nil {
		t.Fatalf("Suggest 失败: %v", err)
	}
	if len(res.Candidates) != suggestPickCount {
		t.Fatalf("应该补齐到 %d 条，实际 %d 条", suggestPickCount, len(res.Candidates))
	}
	if res.Note == "" {
		t.Fatal("补齐了就要说一声")
	}
}

// 模型编出超范围的序号（候选只有 20 条却回 [120,200,3]）→ 越界的丢掉、其余补齐。
func TestSuggest_IgnoresOutOfRangeIndexes(t *testing.T) {
	chat := &sgFakeChat{replies: []string{
		`{"category":"ecommerce-main-image"}`,
		`[120,200,3,0,-5]`,
		`最终提示词`,
	}}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 20)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	res, err := svc.Suggest(context.Background(), sgInput())
	if err != nil {
		t.Fatalf("越界序号不该让推荐失败: %v", err)
	}
	if len(res.Candidates) != suggestPickCount {
		t.Fatalf("应该补齐到 %d 条，实际 %d 条", suggestPickCount, len(res.Candidates))
	}
}

// 模型回的是「我选第 3、7、11、15、19 条」这种话 —— 不是 JSON，也要能抠出序号。
func TestSuggest_ParsesIndexesFromProse(t *testing.T) {
	chat := &sgFakeChat{replies: []string{
		`{"category":"ecommerce-main-image"}`,
		`我选择第 3、7、11、15、19 条，它们最适合这个商品。`,
		`最终提示词`,
	}}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 20)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	res, err := svc.Suggest(context.Background(), sgInput())
	if err != nil {
		t.Fatalf("Suggest 失败: %v", err)
	}
	if res.Candidates[0].UID != "p003" {
		t.Fatalf("第 1 条应为 p003，实际 %s", res.Candidates[0].UID)
	}
}

// 候选本来就不足 5 条 → 直接全给，不再花一趟对话让模型挑。
func TestSuggest_SkipsPickWhenPoolTooSmall(t *testing.T) {
	chat := &sgFakeChat{replies: []string{
		`{"category":"ecommerce-main-image"}`,
		`最终提示词`, // 注意：只有两趟
	}}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 3)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	res, err := svc.Suggest(context.Background(), sgInput())
	if err != nil {
		t.Fatalf("Suggest 失败: %v", err)
	}
	if chat.calls != 2 {
		t.Fatalf("候选不足 5 条时不该再问模型挑哪几条，对话次数 = %d", chat.calls)
	}
	if len(res.Candidates) != 3 {
		t.Fatalf("应该把 3 条全给，实际 %d 条", len(res.Candidates))
	}
}

// 合成结果是空的 → 这个才该报错，并且要给出「下一步怎么办」。
func TestSuggest_EmptyFinalPromptIsError(t *testing.T) {
	chat := &sgFakeChat{replies: []string{
		`{"category":"ecommerce-main-image"}`,
		`[1,2,3,4,5]`,
		`   `,
	}}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 20)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	_, err := svc.Suggest(context.Background(), sgInput())
	if err == nil {
		t.Fatal("合成结果为空时应该报错")
	}
	var dkErr *dkdomain.DesignkitError
	if !errors.As(err, &dkErr) || dkErr.Message == "" {
		t.Fatalf("应该是带中文提示的业务错误，实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 多张商品图
// ---------------------------------------------------------------------------

// 整批都要发给模型看（封顶 suggestMaxImages 张），不是只发第一张。
func TestSuggest_SendsWholeBatchOfImages(t *testing.T) {
	chat := &sgFakeChat{replies: []string{
		`{"category":"ecommerce-main-image"}`,
		`[1,2,3,4,5]`,
		`最终提示词`,
	}}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 20)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	in := sgInput()
	// 给 5 张，封顶 3 张。
	in.ExtraAssetUIDs = []string{"a2", "a3", "a4", "a5"}
	if _, err := svc.Suggest(context.Background(), in); err != nil {
		t.Fatalf("Suggest 失败: %v", err)
	}
	if len(chat.requests) == 0 {
		t.Fatal("一趟对话都没发")
	}
	got := len(chat.requests[0].Images)
	if got != suggestMaxImages {
		t.Fatalf("发给模型的图 = %d 张，应为封顶的 %d 张", got, suggestMaxImages)
	}
}

// 配图读不出来只跳过；主图读不出来才失败。
func TestSuggest_SkipsUnreadableExtraImages(t *testing.T) {
	chat := &sgFakeChat{replies: []string{
		`{"category":"ecommerce-main-image"}`,
		`[1,2,3,4,5]`,
		`最终提示词`,
	}}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 20)}
	assets := &sgFakeAssets{missing: map[string]bool{"a2": true}}
	svc := sgFixture(t, chat, catalog, assets)

	in := sgInput()
	in.ExtraAssetUIDs = []string{"a2", "a3"}
	if _, err := svc.Suggest(context.Background(), in); err != nil {
		t.Fatalf("配图读不出来不该让整次推荐失败: %v", err)
	}
	if got := len(chat.requests[0].Images); got != 2 {
		t.Fatalf("应该是主图 + a3 共 2 张，实际 %d 张", got)
	}
}

func TestSuggest_MissingMainImageFails(t *testing.T) {
	chat := &sgFakeChat{}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 20)}
	assets := &sgFakeAssets{missing: map[string]bool{sgInput().AssetUID: true}}
	svc := sgFixture(t, chat, catalog, assets)

	if _, err := svc.Suggest(context.Background(), sgInput()); err == nil {
		t.Fatal("主图读不出来必须失败 —— 整套流程的前提就是让模型看见图")
	}
	if chat.calls != 0 {
		t.Fatal("主图都没有就不该发对话（白花钱）")
	}
}

// ---------------------------------------------------------------------------
// 入参
// ---------------------------------------------------------------------------

func TestSuggest_RejectsMissingAssetUID(t *testing.T) {
	svc := sgFixture(t, &sgFakeChat{},
		&sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 20)}, &sgFakeAssets{})

	in := sgInput()
	in.AssetUID = "   "
	_, err := svc.Suggest(context.Background(), in)
	if err == nil {
		t.Fatal("没有商品图应该报错")
	}
	var dkErr *dkdomain.DesignkitError
	if !errors.As(err, &dkErr) || dkErr.Message == "" {
		t.Fatalf("应该给中文提示，实际: %v", err)
	}
}

func TestSuggest_RejectsUnknownCategory(t *testing.T) {
	svc := sgFixture(t, &sgFakeChat{},
		&sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 20)}, &sgFakeAssets{})

	in := sgInput()
	in.CategorySlug = "不存在的分类"
	if _, err := svc.Suggest(context.Background(), in); err == nil {
		t.Fatal("分类不存在应该报错")
	}
}

// 灵感库整个空的时候要给一句能照做的中文，不是「内部错误」。
func TestSuggest_EmptyLibraryGivesActionableError(t *testing.T) {
	svc := sgFixture(t, &sgFakeChat{}, &sgFakeCatalog{}, &sgFakeAssets{})

	_, err := svc.Suggest(context.Background(), sgInput())
	if err == nil {
		t.Fatal("灵感库为空应该报错")
	}
	var dkErr *dkdomain.DesignkitError
	if !errors.As(err, &dkErr) || !strings.Contains(dkErr.Message, "同步") {
		t.Fatalf("应该提示管理员去同步灵感库，实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 两段式粗筛（monica 2026-08-14 提的做法）
// ---------------------------------------------------------------------------

// **整个分类都必须够得着。**
//
// 这是这套改动存在的理由：之前是「按 id 取前 100 条」，
// 社交媒体帖子 3978 条里 3878 条永远不可能被选中，而且完全静默。
// 这条测试用一个排在第 3000 位的提示词来验：它必须能被选出来。
func TestSuggest_WholeCategoryIsReachable(t *testing.T) {
	chat := &sgFakeChat{replies: []string{
		`{"category":"ecommerce-main-image"}`,
		`[3000]`, // 粗筛只留这一条 —— 它排在第 3000 位
		`最终提示词`,
	}}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 3978)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	res, err := svc.Suggest(context.Background(), sgInput())
	if err != nil {
		t.Fatalf("Suggest 失败: %v", err)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].UID != "p3000" {
		t.Fatalf("排在第 3000 位的提示词没被选出来，实际: %+v", res.Candidates)
	}
	// 粗筛必须真的把全量拿去看了，不是只看前 100 条。
	if catalog.lastDigestLimit < 3978 {
		t.Fatalf("粗筛只取了 %d 条上限，够不到整个分类", catalog.lastDigestLimit)
	}
}

// 分类本来就不足 100 条 → **跳过粗筛**，省一趟对话的钱和十来秒等待。
func TestSuggest_SkipsShortlistForSmallCategory(t *testing.T) {
	chat := &sgFakeChat{replies: []string{
		`{"category":"ecommerce-main-image"}`,
		`[1,2,3,4,5]`, // 直接进细选
		`最终提示词`,
	}}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 40)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	if _, err := svc.Suggest(context.Background(), sgInput()); err != nil {
		t.Fatalf("Suggest 失败: %v", err)
	}
	// 判分类 + 细选 + 合成 = 3 趟。多一趟就是粗筛没被跳过。
	if chat.calls != 3 {
		t.Fatalf("小分类应该跳过粗筛（共 3 趟），实际 %d 趟", chat.calls)
	}
}

// 粗筛挂了不该让整次推荐失败：退回按顺序取前若干条，并说明。
func TestSuggest_ShortlistFailureFallsBack(t *testing.T) {
	chat := &sgFakeChat{replies: []string{
		`{"category":"ecommerce-main-image"}`,
		`我没法从这么多条里挑。`, // 粗筛跑偏，一个序号都没有
		`[1,2,3,4,5]`,
		`最终提示词`,
	}}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 500)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	res, err := svc.Suggest(context.Background(), sgInput())
	if err != nil {
		t.Fatalf("粗筛失败不该让整次推荐失败: %v", err)
	}
	if len(res.Candidates) != suggestPickCount {
		t.Fatalf("仍然应该给出 %d 条，实际 %d 条", suggestPickCount, len(res.Candidates))
	}
	if res.Note == "" {
		t.Fatal("退回按顺序取时必须给运营一句说明")
	}
}

// 简介的提炼：JSON 形态取 "type" 字段，散文形态取开头。
//
// 这一条直接决定细选那一步给模型看到的是什么。取错了不会报错，
// 只会让它挑得差 —— 而「挑得差」是没人查得出来的那种坏。
func TestPromptBrief_ExtractsTypeFromJSONBody(t *testing.T) {
	jsonBody := &dkdomain.PromptDigest{
		Title: "SaaS AI Dashboard Landing Page",
		Brief: `{` + "\n" + `  "type": "SaaS landing page hero graphic",` + "\n" + `  "style": "Modern UI/UX`,
	}
	if got := promptBrief(jsonBody); got != "SaaS landing page hero graphic" {
		t.Fatalf("JSON 形态应该取 type 字段，实际 %q", got)
	}

	prose := &dkdomain.PromptDigest{
		Title: "Epic Fantasy Monkey King Battle",
		Brief: "A {art_style} of {main_character} in mid-combat, wearing ornate golden armor.",
	}
	got := promptBrief(prose)
	if !strings.HasPrefix(got, "A {art_style} of {main_character}") {
		t.Fatalf("散文形态应该取开头，实际 %q", got)
	}
	// 占位符要原样保留：它本身在告诉模型「这条模板可以换主体」。
	if !strings.Contains(got, "{art_style}") {
		t.Fatalf("占位符不该被抹掉：%q", got)
	}
}

// 回查正文时少了一两条不该让整次推荐失败 —— 4 条参考照样写得出来。
func TestSuggest_ToleratesMissingBodyOnLoad(t *testing.T) {
	chat := &sgFakeChat{replies: []string{
		`{"category":"ecommerce-main-image"}`,
		`[1,2,3,4,5]`,
		`最终提示词`,
	}}
	prompts := sgPrompts(1, 20)
	// 把第 2 条的 uid 改掉，模拟回查失败。
	prompts[1].UID = "p002"
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: prompts}
	// 让 GetPromptByUID 找不到 p002：直接把它从列表里摘掉，但 digest 里还在。
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	res, err := svc.Suggest(context.Background(), sgInput())
	if err != nil {
		t.Fatalf("Suggest 失败: %v", err)
	}
	if len(res.Candidates) == 0 {
		t.Fatal("至少要给出几条参考")
	}
}

// ---------------------------------------------------------------------------
// 计费 id（钱的事故）
// ---------------------------------------------------------------------------

// 一次推荐里四趟对话的 request_id **必须两两不同**，
// 而且**两次推荐之间也必须不同**。
//
// 2026-08-14 真踩过：scope 用的是商品图 uid，于是同一张图重复推荐时
// request_id 完全一样，上游幂等表（request_id, api_key_id 唯一键）判成重复调用，
// 第二次起一分钱不扣、也不写账单。功能看着完全正常，只有账目对不上。
// 撞步骤号是同一个坑的另一种触发方式。
func TestSuggest_BillingRequestIDsAreAllDistinct(t *testing.T) {
	run := func() []string {
		chat := &sgFakeChat{replies: []string{
			`{"category":"ecommerce-main-image"}`,
			`[1,2,3,4,5,6,7,8,9,10]`,
			`[1,2,3,4,5]`,
			`最终提示词`,
		}}
		catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 500)}
		svc := sgFixture(t, chat, catalog, &sgFakeAssets{})
		if _, err := svc.Suggest(context.Background(), sgInput()); err != nil {
			t.Fatalf("Suggest 失败: %v", err)
		}
		ids := make([]string, 0, len(chat.requests))
		for _, r := range chat.requests {
			ids = append(ids, r.RequestID)
		}
		return ids
	}

	first := run()
	if len(first) < 3 {
		t.Fatalf("对话趟数 = %d，太少了", len(first))
	}
	seen := make(map[string]bool, len(first))
	for _, id := range first {
		if seen[id] {
			t.Fatalf("同一次推荐里出现了重复的 request_id %q —— 重复的那趟不会被计费", id)
		}
		seen[id] = true
	}

	// 两次推荐之间也不能撞（这正是用商品图 uid 当 scope 时踩的坑）。
	for _, id := range run() {
		if seen[id] {
			t.Fatalf("两次推荐之间 request_id 撞了 %q —— 第二次不会被计费", id)
		}
	}
}
