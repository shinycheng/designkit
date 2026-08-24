package handler

// ============================================================================
// 灵感库（设计定型 第三节「灵感库」）
// ============================================================================
//
// 七个端点，分成两组：
//
//	所有登录用户
//	  GET  /prompt-categories        分类列表（带每类条数）
//	  GET  /prompts                  按分类 / 关键词检索，游标分页
//	  GET  /prompts/:uid             看一条
//
//	仅管理员
//	  POST /prompts/sync             手动同步（长任务，立刻返回）
//	  GET  /prompts/sync/latest      同步状态
//	  GET  /prompts/sync/settings    同步走哪个代理（含可选代理列表）
//	  PUT  /prompts/sync/settings    换一个代理
//
// 为什么同步这组只给管理员：同步会去外网拉一万四千条覆盖整个灵感库，
// 而且要用到代理配置（那是管理员才看得到的东西）。运营点得到它，
// 就等于给了每个运营一个「随时把全站灵感库刷一遍」的按钮。
//
// ⚠ gin 的路由树允许静态段和参数段同层共存（上游 /admin/proxies/all 和
// /admin/proxies/:id 就是这么挂的），所以 GET /prompts/sync/latest 和
// GET /prompts/:uid 不会打架。

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	upstreammw "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// maxPromptKeywordRunes 关键词长度上限（按字数算）。
//
// 超了直接告诉他精简，**不静默截断** —— 截断之后运营搜出来的是另一个词的结果，
// 而他以为搜的是自己打的那个。
const maxPromptKeywordRunes = 64

// ----------------------------------------------------------------------------
// 管理员判定
// ----------------------------------------------------------------------------

// RequireAdmin 只放管理员过。**必须挂在鉴权中间件之后**。
//
// 判据跟上游 middleware.AdminOnly 完全一致（middleware/admin_only.go）：
// 从上下文取角色，跟 service.RoleAdmin 比。两边不一致的话，
// 会出现「上游后台进得去、designkit 这几个端点进不去」这种没人说得清的现象。
//
// 拿不到角色时按**非管理员**处理：这几个端点前面已经挂了 RequireAuthenticated，
// 走到这里一定是登录着的，角色取不到只可能是鉴权中间件没塞 —— 那也不该放行。
//
// 文案是中文的（上游那句是 "Admin access required"，运营看不懂），
// 而且**不说「你不是管理员」**，只说该找谁：运营看到前一句只会以为自己账号坏了。
//
// message 可选：不传时用灵感库同步那句话（本函数最早的唯一调用方）。
// 传一句的话，说的是「这一组端点」在做什么 —— 拦下来的时候必须让人知道
// 自己点的是什么，只说「没权限」等于让他去猜。
//
// 为什么放在本文件而不是 middleware.go：最早只有灵感库同步这一组用到它，
// 放在用它的地方，删的时候不会漏。（现在「商品图设置」那两个端点也用它。）
func RequireAdmin(message ...string) gin.HandlerFunc {
	text := "同步灵感库是管理员才能做的操作，需要的话请联系管理员。"
	if len(message) > 0 && strings.TrimSpace(message[0]) != "" {
		text = strings.TrimSpace(message[0])
	}
	return func(c *gin.Context) {
		role, ok := upstreammw.GetUserRoleFromContext(c)
		if !ok || role != upstreamservice.RoleAdmin {
			abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeForbidden).
				WithMessage(text))
			return
		}
		c.Next()
	}
}

// ----------------------------------------------------------------------------
// 游标
// ----------------------------------------------------------------------------
//
// 灵感库是稳定的目录型数据（同步之外几乎不写），游标只需要一个 id，
// 不需要 jobs 那种 (created_at, id) 复合游标。
// 但**仍然是不透明字符串**：客户端不许解析它，我们保留随时换编码的自由。

// promptCursorTag 编在游标里的一个字节，用来认出「这是灵感库的游标」。
// 不带它的话，把批次列表的游标粘到这里会被解析成一个巨大的 id，
// 结果是一页空列表 —— 空列表不报错，运营只会以为灵感库坏了。
const promptCursorTag = 'p'

func encodePromptCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString(
		append([]byte{promptCursorTag}, strconv.FormatInt(id, 10)...))
}

// decodePromptCursor 解游标。空串表示第一页，返回 0 且不报错。
func decodePromptCursor(cursor string) (int64, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(decoded) < 2 || decoded[0] != promptCursorTag {
		return 0, ErrInvalidCursor
	}
	id, err := strconv.ParseInt(string(decoded[1:]), 10, 64)
	if err != nil || id < 0 {
		return 0, ErrInvalidCursor
	}
	return id, nil
}

// ----------------------------------------------------------------------------
// handler
// ----------------------------------------------------------------------------

// PromptHandler 灵感库的浏览与同步。
//
// sync 允许为 nil：同步服务缺席时，管理端那四个端点不注册，
// 浏览照常 —— 运营那一页不受影响。
type PromptHandler struct {
	prompts PromptService
	sync    PromptSyncService
	// suggest 「AI 挑提示词」。允许为 nil —— 为 nil 时那条路由**根本不挂**，
	// 见 register_business.go 里的理由。
	suggest SuggestService
	// my 「我的提示词」的写入。允许为 nil —— 为 nil 时那三条写路由整组不挂
	// （裸 404 = 功能没上线，跟 suggest 同一套前端约定）。
	my MyPromptService
}

// NewPromptHandler 建 handler。suggest 和 my 都可以为 nil。
func NewPromptHandler(prompts PromptService, sync PromptSyncService, suggest SuggestService, my MyPromptService) *PromptHandler {
	return &PromptHandler{prompts: prompts, sync: sync, suggest: suggest, my: my}
}

// hasSuggest 「AI 挑提示词」这块能不能用。
//
// 挂路由那边（register_business.go）拿它决定要不要挂那条 POST ——
// 缺席时**整条不挂**，让它自然 404，前端才能区分「功能没上线」和「业务错误」。
func (h *PromptHandler) hasSuggest() bool {
	return h != nil && h.suggest != nil
}

// hasMyPrompts 「我的提示词」的写入能不能用。缺席时那三条写路由整组不挂，
// 判据和后果跟 hasSuggest 完全一致。
func (h *PromptHandler) hasMyPrompts() bool {
	return h != nil && h.my != nil
}

// ----------------------------------------------------------------------------
// AI 挑提示词（2026-08-14）
// ----------------------------------------------------------------------------

// suggestMaxFeaturesRunes 「商品特点」的长度上限（按字符，不是字节）。
// 跟前端那个 500 对齐；纯粹是防手滑粘一整篇进来，不是业务规则。
const suggestMaxFeaturesRunes = 500

// suggestRequestBody 是 POST /prompts/suggest 的请求体。
type suggestRequestBody struct {
	// AssetUID 读哪张商品图。
	AssetUID string `json:"asset_uid"`
	// ExtraAssetUIDs 这一批里的其余商品图，可空。整批一起看，写出来的词才适合整批。
	ExtraAssetUIDs []string `json:"extra_asset_uids"`
	// CategorySlug 分类标识；**空串 = 全部**，由后端让模型自己判。
	// ⚠ 是 slug 不是 uid：分类对外没有 uid（见 promptCategoryDTO）。
	CategorySlug string `json:"category_slug"`
	// Features 运营填的商品特点，可空。
	Features string `json:"features"`
	// Force 跳过缓存、强制重新推荐（前端在「重新推荐」按钮上传 true）。
	// 不带它时，输入完全相同的重复请求直接返回上次的结果，不再调模型、不产生新计费。
	Force bool `json:"force"`
}

type suggestCategoryDTO struct {
	// Slug 稳定标识，前端拿它跟分类下拉对上号。
	Slug string `json:"slug"`
	// Name 显示名。**字段名必须是 name**（前端只读这个）；
	// 值取的是 name_zh —— 图省事直接用列名会让界面上分类名变空串且不报错。
	Name string `json:"name"`
}

type suggestCandidateDTO struct {
	UID   string `json:"uid"`
	Title string `json:"title"`
}

type suggestResponseDTO struct {
	Prompt     string                `json:"prompt"`
	Category   suggestCategoryDTO    `json:"category"`
	Candidates []suggestCandidateDTO `json:"candidates"`
	Note       string                `json:"note"`
	// CachedAt 非空 = 这条来自缓存（RFC3339 UTC，值是它最初生成的时间），
	// 没调模型、没计费。前端据此在结果框顶部显示 designkit.suggest.cachedNote。
	CachedAt string `json:"cached_at,omitempty"`
}

// SuggestPrompt 处理 POST /prompts/suggest。
//
// 一次请求会在后端连发三趟对话（判分类 → 挑 5 条 → 合成 1 条），**十几秒起步**。
// 前端为它单独把超时放宽到了 120 秒（绕过 axios 单例默认的 30 秒）。
//
// 刻意**不要求 Idempotency-Key**：它不建任何东西、不冻结额度，
// 重复提交最坏是多花几厘钱的对话费（一次约 $0.0025，出图是 $1/张）。
// 为它加一道幂等只会让前端多一件必须做对的事。
func (h *PromptHandler) SuggestPrompt(c *gin.Context) {
	// 纯防御：服务缺席时这条路由**根本不会挂**（register_business.go），
	// 走到这里说明装配逻辑被改坏了。
	if h == nil || h.suggest == nil {
		failCode(c, dkdomain.ErrCodeInternal)
		return
	}
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}

	var req suggestRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("提交的内容格式不对，请刷新页面重试。").
			WithCause(err))
		return
	}

	features := strings.TrimSpace(req.Features)
	if len([]rune(features)) > suggestMaxFeaturesRunes {
		failCodef(c, dkdomain.ErrCodeInvalidRequest,
			"商品特点最多 %d 个字，写重点就行。", suggestMaxFeaturesRunes)
		return
	}

	result, err := h.suggest.SuggestPrompt(c.Request.Context(), SuggestPromptInput{
		UserID:         userID,
		AssetUID:       strings.TrimSpace(req.AssetUID),
		ExtraAssetUIDs: req.ExtraAssetUIDs,
		CategorySlug:   strings.TrimSpace(req.CategorySlug),
		Features:       features,
		Force:          req.Force,
	})
	if err != nil {
		failService(c, err, dkdomain.ErrCodeAssetNotFound)
		return
	}
	if result == nil {
		failCode(c, dkdomain.ErrCodeInternal)
		return
	}

	candidates := make([]suggestCandidateDTO, 0, len(result.Candidates))
	for _, one := range result.Candidates {
		candidates = append(candidates, suggestCandidateDTO(one))
	}
	cachedAt := ""
	if !result.CachedAt.IsZero() {
		cachedAt = timeString(result.CachedAt)
	}
	c.JSON(http.StatusOK, suggestResponseDTO{
		Prompt: result.Prompt,
		Category: suggestCategoryDTO{
			Slug: result.Category.Slug,
			Name: result.Category.Name,
		},
		Candidates: candidates,
		Note:       result.Note,
		CachedAt:   cachedAt,
	})
}

// ---- 浏览 ----

// Categories 处理 GET /prompt-categories。
//
// 顺序即界面显示顺序（按 sort_order），**不要在前端再排一遍** ——
// 排序是管理员在数据里定的，前端重排会让「置顶那个分类」失效。
func (h *PromptHandler) Categories(c *gin.Context) {
	views, err := h.prompts.ListCategories(c.Request.Context())
	if err != nil {
		failService(c, err, dkdomain.ErrCodeInternal)
		return
	}
	c.JSON(http.StatusOK, newPromptCategoryListDTO(views))
}

// List 处理 GET /prompts。
//
// 查询参数：
//
//	category  分类的 slug；不传 = 全部分类
//	keyword   关键词，标题和正文都搜；不传 = 不过滤
//	source    不传 = 共享目录；"user" = 只看自己存的（「我的提示词」）
//	cursor    上一页返回的 next_cursor；不传 = 第一页
//	limit     每页条数，默认 20，最大 100
//
// **只返回没下架也没删的**（OnlyEnabled 由 service 侧设死）。
// **任何取值下都带不出别人的自建词**（过滤在 service 侧，见 ListPromptsInput）。
func (h *PromptHandler) List(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}

	keyword := strings.TrimSpace(c.Query("keyword"))
	if len([]rune(keyword)) > maxPromptKeywordRunes {
		failCodef(c, dkdomain.ErrCodeInvalidRequest,
			"搜索词最多 %d 个字，换一个短一点的词试试。", maxPromptKeywordRunes)
		return
	}

	// source 只认两个值：空 = 共享目录，"user" = 我的提示词。
	// 别的值直接拒绝，不静默当成空 —— 静默的话，前端拼错参数会表现成
	// 「我的提示词永远是空的」，谁也想不到是参数名的问题。
	source := strings.TrimSpace(c.Query("source"))
	if source != "" && source != ListPromptSourceMine {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("筛选参数不对，请刷新页面重试。"))
		return
	}

	cursorID, err := decodePromptCursor(c.Query("cursor"))
	if err != nil {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("翻页参数不对，请回到第一页重新翻。"))
		return
	}

	page, err := h.prompts.ListPrompts(c.Request.Context(), ListPromptsInput{
		CategorySlug: strings.TrimSpace(c.Query("category")),
		Source:       source,
		ViewerUserID: userID,
		Keyword:      keyword,
		CursorID:     cursorID,
		Limit:        parseLimit(c.Query("limit")),
	})
	if err != nil {
		failService(c, err, dkdomain.ErrCodeInternal)
		return
	}
	c.JSON(http.StatusOK, newPromptListDTO(page))
}

// Get 处理 GET /prompts/:uid。
//
// **不过滤 is_enabled**：运营收藏夹里那条词后来被管理员下架了，
// 点开详情仍要看得到（响应里的 is_enabled 会是 false，界面据此提示一句）。
//
// 带上当前用户：别人的自建词（source=user）一律「找不到」，过滤在 service 侧。
func (h *PromptHandler) Get(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid := strings.TrimSpace(c.Param("uid"))
	if uid == "" {
		failCode(c, dkdomain.ErrCodePromptNotFound)
		return
	}

	view, err := h.prompts.GetPrompt(c.Request.Context(), userID, uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodePromptNotFound)
		return
	}
	if view == nil || view.Prompt == nil {
		failCode(c, dkdomain.ErrCodePromptNotFound)
		return
	}
	c.JSON(http.StatusOK, newPromptDTO(view))
}

// ---- 同步（仅管理员）----

// StartSync 处理 POST /prompts/sync。
//
// **立刻返回 202**，不等同步跑完：一万四千条要十几秒，让 HTTP 请求挂着等，
// 中间任何一个反代超时都会让管理员看到「失败」，而同步其实跑得好好的
// —— 然后他会再点一次，于是两次同步撞在一起。
//
// 已经有一次在跑时 service 返回 DK_SYNC_IN_PROGRESS（409，
// 「灵感库正在同步，请等这一次跑完再点。」）。
// 那把锁是 PostgreSQL 的 advisory lock，手动和自动共用同一把 ——
// 老系统各用一把，并发把 1.4 万条写重复了。
func (h *PromptHandler) StartSync(c *gin.Context) {
	run, err := h.sync.StartSync(c.Request.Context())
	if err != nil {
		failService(c, err, dkdomain.ErrCodeInternal)
		return
	}
	c.JSON(http.StatusAccepted, newSyncStartedDTO(mountPrefixOf(c), run))
}

// SyncLatest 处理 GET /prompts/sync/latest。
//
// 前端点完「立即同步」之后就靠轮询它看进度：running 为 true 时继续轮询，
// 变成 false 就停下来并显示 message。
func (h *PromptHandler) SyncLatest(c *gin.Context) {
	status, err := h.sync.SyncStatus(c.Request.Context())
	if err != nil {
		failService(c, err, dkdomain.ErrCodeInternal)
		return
	}
	c.JSON(http.StatusOK, newSyncStatusDTO(status))
}

// SyncSettings 处理 GET /prompts/sync/settings。
//
// **同时返回「选中的是谁」和「能选谁」**：前端那个下拉框
// （复用上游 components/common/ProxySelector.vue）两样都要才渲染得出来。
func (h *PromptHandler) SyncSettings(c *gin.Context) {
	settings, err := h.sync.GetSyncSettings(c.Request.Context())
	if err != nil {
		failService(c, err, dkdomain.ErrCodeInternal)
		return
	}
	c.JSON(http.StatusOK, newSyncSettingsDTO(settings))
}

// syncSettingsBodyField 是 PUT /prompts/sync/settings 里那个字段的名字。
const syncSettingsBodyField = "proxy_id"

// UpdateSyncSettings 处理 PUT /prompts/sync/settings。
//
// 请求体：{"proxy_id": 3} 或 {"proxy_id": null}（null = 不走代理，直连）。
//
// **proxy_id 必须显式出现**，缺字段直接 400 而不是当成 null：
// 前端少传一个字段就悄悄把代理清掉的话，表现是「同步昨天还好好的，今天拉不动了」，
// 而设置页面上看不出任何异常（因为它确实被改成了直连）。
//
// ⚠ 这里配的代理**只给灵感库同步用**，不影响出图 —— 响应的 message 里写着这句话，
// 界面必须显示出来。
func (h *PromptHandler) UpdateSyncSettings(c *gin.Context) {
	proxyID, ok := parseSyncSettingsBody(c)
	if !ok {
		return
	}

	settings, err := h.sync.PutSyncSettings(c.Request.Context(), proxyID)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeInvalidRequest)
		return
	}
	c.JSON(http.StatusOK, newSyncSettingsDTO(settings))
}

// parseSyncSettingsBody 解出 proxy_id。
//
// 用 map[string]json.RawMessage 而不是结构体，是为了能区分
// 「没传 proxy_id」和「明确传了 null」—— 结构体里 *int64 两种情况都是 nil。
// 返回 false 时已经写好错误响应，调用方直接 return。
func parseSyncSettingsBody(c *gin.Context) (*int64, bool) {
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil {
		msg := "提交的内容格式不对，请刷新页面重试。"
		if errors.Is(err, io.EOF) {
			msg = "没有提交任何内容。请选一个代理，或者选「不使用代理」。"
		}
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage(msg).WithCause(err))
		return nil, false
	}

	raw, present := body[syncSettingsBodyField]
	if !present {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("请选一个代理，或者选「不使用代理」。"))
		return nil, false
	}
	if string(raw) == "null" {
		return nil, true
	}

	var id int64
	if err := json.Unmarshal(raw, &id); err != nil || id <= 0 {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("选中的代理不对，请从列表里重新选一个。").WithCause(err))
		return nil, false
	}
	return &id, true
}
