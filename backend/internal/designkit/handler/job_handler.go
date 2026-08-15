package handler

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	upstreammw "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

const (
	// idempotencyHeader 提交任务必须带的防重复标识。
	idempotencyHeader = "Idempotency-Key"

	// idempotencyScopePrefix 幂等作用域前缀，后面拼 user id。
	//
	// ⚠ **必须把用户揉进 scope**：上游协调器的唯一键是 (Scope, keyHash)，**不含用户**。
	// 两个用户凑巧用了同一个 key（ERP 用订单号、浏览器用 UUID，撞的概率不为零）
	// 会直接撞成 409，第二个人的任务提交不上去。
	idempotencyScopePrefix = "designkit.jobs.create:user:"

	// idempotencyRoute 幂等指纹里的 route 字段。
	//
	// 刻意用常量而不是 c.FullPath()：同一个 handler 挂在两个前缀上
	// （/api/v1/designkit/jobs 和 /v1/designkit/jobs），用 FullPath 会让
	// 同一个人拿同一个 key、同样的内容，从两条路进来算出两个不同的指纹。
	idempotencyRoute = "/designkit/jobs"

	// idempotencyRetryScopePrefix 重试的幂等作用域前缀，后面拼 user id。
	//
	// 跟提交分开一个作用域：运营在同一秒里既提交又重试时，两把 key 撞不上。
	idempotencyRetryScopePrefix = "designkit.jobs.retry:user:"

	// idempotencyRetryRoute 重试的幂等指纹里的 route 字段（理由同 idempotencyRoute）。
	idempotencyRetryRoute = "/designkit/jobs/items/retry"

	// maxSelectionEntries 一次请求里允许出现的商品图 / 提示词条数上限。
	//
	// 真正的批量上限是 designkit_settings.max_batch_items（默认 50，由 service 校验，
	// 因为只有它读得到配置）。这里这条是**入口处的防爆闸**：
	// 挡住「一次发十万个 uid 把内存和数据库打满」这种明显异常的请求。
	maxSelectionEntries = 500
)

// JobHandler 批次的报价、提交与查询。
type JobHandler struct {
	jobs JobService
}

// NewJobHandler 建 handler。
func NewJobHandler(jobs JobService) *JobHandler {
	return &JobHandler{jobs: jobs}
}

// jobSpecRequest 是「这一批要出哪些图」的请求体，报价和提交共用。
type jobSpecRequest struct {
	// Ratio 出图比例，形如 "3:4"。
	Ratio string `json:"ratio"`
	// AssetUIDs 选中的商品图，顺序即展开时的**外层**顺序。
	AssetUIDs []string `json:"asset_uids"`
	// PromptUIDs 选中的灵感库提示词，顺序即展开时的**内层**顺序。
	PromptUIDs []string `json:"prompt_uids"`
	// Prompts 运营手打的提示词，接在 PromptUIDs 后面。
	Prompts []string `json:"prompts"`
	// Model 出图模型；不填用系统默认。
	Model string `json:"model"`
	// KeepTransparency 预处理时保留透明底；不填 = 合成白底。
	KeepTransparency bool `json:"keep_transparency"`
}

// createJobRequest 是 POST /jobs 的请求体。
type createJobRequest struct {
	jobSpecRequest

	// Name 给这批起的名字，可以不填。
	Name string `json:"name"`
	// CallbackURL 出完回调这个地址；主要给 ERP 用，不填 = 不回调。
	CallbackURL string `json:"callback_url"`
}

// idempotencyPayload 是算幂等指纹用的内容快照。
//
// 刻意**不含** Idempotency-Key 本身和 user id：
// key 是唯一键的一半，user 已经揉进 scope，指纹只该表示「这一单要出什么」。
// 同 key 同内容 → 返回原结果；同 key 不同内容 → 409。
type idempotencyPayload struct {
	Ratio            string   `json:"ratio"`
	AssetUIDs        []string `json:"asset_uids"`
	PromptUIDs       []string `json:"prompt_uids"`
	Prompts          []string `json:"prompts"`
	Model            string   `json:"model"`
	KeepTransparency bool     `json:"keep_transparency"`
	Name             string   `json:"name"`
	CallbackURL      string   `json:"callback_url"`
	Origin           string   `json:"origin"`
}

// retryIdempotencyPayload 是重试的内容快照：哪个批次的第几张。
// 同 key 同内容 → 返回上一次的结果；同 key 换了一张图 → 409。
type retryIdempotencyPayload struct {
	JobUID string `json:"job_uid"`
	Seq    int    `json:"seq"`
	Origin string `json:"origin"`
}

// Estimate 处理 POST /jobs/estimate：只算账，不建任何东西、不冻结任何额度。
func (h *JobHandler) Estimate(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}

	var req jobSpecRequest
	if !bindJSON(c, &req) {
		return
	}
	spec, ok := h.buildSpec(c, userID, req)
	if !ok {
		return
	}

	result, err := h.jobs.EstimateJob(c.Request.Context(), spec)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeAssetNotFound)
		return
	}
	c.JSON(http.StatusOK, newEstimateDTO(result))
}

// Create 处理 POST /jobs：提交一个批次。
//
// 三件事按顺序做，一件都不能少：
//  1. **缺 Idempotency-Key 由我们直接 400**。不能依赖上游协调器的 RequireKey ——
//     它受 ObserveOnly 开关控制，而 ObserveOnly 默认 true，
//     不带 key 会**直接放行执行** = ERP 超时重发 = 重复下单重复扣钱。
//  2. 走上游协调器，但 scope 里揉进 user id（上游唯一键不含用户，会互相撞）。
//  3. 成功只返小对象，**绝不内联 items 数组**（单测断言 < 8KB）。
func (h *JobHandler) Create(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}

	idempotencyKey, ok := requireIdempotencyKey(c)
	if !ok {
		return
	}

	var req createJobRequest
	if !bindJSON(c, &req) {
		return
	}
	spec, ok := h.buildSpec(c, userID, req.jobSpecRequest)
	if !ok {
		return
	}
	callbackURL := strings.TrimSpace(req.CallbackURL)
	if !validCallbackURL(callbackURL) {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("回调地址必须是完整的 http:// 或 https:// 网址。"))
		return
	}

	input := CreateJobInput{
		JobSpec:        spec,
		Name:           strings.TrimSpace(req.Name),
		CallbackURL:    callbackURL,
		IdempotencyKey: idempotencyKey,
		APIKeyID:       apiKeyIDOf(c),
	}

	h.runIdempotent(c, idempotentWrite{
		UserID: userID,
		Scope:  idempotencyScopePrefix + strconv.FormatInt(userID, 10),
		Route:  idempotencyRoute,
		Key:    idempotencyKey,
		Payload: idempotencyPayload{
			Ratio:            spec.Ratio.String(),
			AssetUIDs:        spec.AssetUIDs,
			PromptUIDs:       spec.PromptUIDs,
			Prompts:          spec.PromptTexts,
			Model:            spec.Model,
			KeepTransparency: spec.KeepTransparency,
			Name:             input.Name,
			CallbackURL:      input.CallbackURL,
			Origin:           spec.Origin.String(),
		},
		SuccessStatus: http.StatusCreated,
		NotFoundCode:  dkdomain.ErrCodeAssetNotFound,
		Execute: func(ctx context.Context) (any, error) {
			job, err := h.jobs.CreateJob(ctx, input)
			if err != nil {
				return nil, err
			}
			return newJobCreatedDTO(job), nil
		},
	})
}

// Stop 处理 POST /jobs/:uid/stop —— 决策 21 的「停止排队」。
//
// **按钮叫「停止排队」不叫「取消」**，因为上游出图跟浏览器连接是**故意脱钩**的：
// 已经在生成的那几张停不下来，会跑完、会扣钱、图会存进作品库。
// 叫「取消」而照扣钱，运营的投诉是「我明明取消了还扣我钱」，而且他会是对的。
//
// 响应里的 stopped_count / still_running_count 就是弹窗里那两个数，
// message 是已经拼好的整句中文，界面直接显示即可。
func (h *JobHandler) Stop(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid, ok := requireUID(c, "uid", dkdomain.ErrCodeJobNotFound)
	if !ok {
		return
	}

	result, err := h.jobs.StopJob(c.Request.Context(), userID, uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeJobNotFound)
		return
	}
	if result == nil || result.Job == nil {
		failCode(c, dkdomain.ErrCodeJobNotFound)
		return
	}
	c.JSON(http.StatusOK, newStopJobDTO(mountPrefixOf(c), result))
}

// RetryItem 处理 POST /jobs/:uid/items/:seq/retry —— 决策 20 的「重试这一张」。
//
// **必须带 Idempotency-Key。** 运营双击必然发生，而重试一张 = 重新出一张图 =
// 重新收一次钱。没有它，第二下要么再花一次钱，要么撞上状态机报一个
// 「这个任务的状态已经变了」的 409 —— 两种都不该让人看到。
// 带上之后，第二下命中重放，原样返回第一下的结果（响应头 X-Idempotency-Replayed）。
func (h *JobHandler) RetryItem(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid, ok := requireUID(c, "uid", dkdomain.ErrCodeJobNotFound)
	if !ok {
		return
	}
	seq, ok := parsePositiveInt(c.Param("seq"), 0)
	if !ok {
		failCode(c, dkdomain.ErrCodeItemNotFound)
		return
	}
	idempotencyKey, ok := requireIdempotencyKey(c)
	if !ok {
		return
	}

	in := RetryItemInput{
		UserID:         userID,
		JobUID:         uid,
		Seq:            seq,
		IdempotencyKey: idempotencyKey,
	}
	prefix := mountPrefixOf(c)

	h.runIdempotent(c, idempotentWrite{
		UserID:        userID,
		Scope:         idempotencyRetryScopePrefix + strconv.FormatInt(userID, 10),
		Route:         idempotencyRetryRoute,
		Key:           idempotencyKey,
		Payload:       retryIdempotencyPayload{JobUID: uid, Seq: seq, Origin: originOf(c).String()},
		SuccessStatus: http.StatusOK,
		NotFoundCode:  dkdomain.ErrCodeItemNotFound,
		Execute: func(ctx context.Context) (any, error) {
			item, err := h.jobs.RetryJobItem(ctx, in)
			if err != nil {
				return nil, err
			}
			if item == nil {
				return nil, dkdomain.NewError(dkdomain.ErrCodeItemNotFound)
			}
			return newRetryAcceptedDTO(prefix, uid, item), nil
		},
	})
}

// List 处理 GET /jobs：游标分页的批次列表。
func (h *JobHandler) List(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}

	cursorCreatedAt, cursorID, err := decodeCursor(c.Query("cursor"))
	if err != nil {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("翻页参数不对，请回到第一页重新翻。"))
		return
	}

	statuses, ok := parseJobStatuses(c)
	if !ok {
		return
	}

	page, err := h.jobs.ListJobs(c.Request.Context(), dkdomain.ListJobsQuery{
		UserID:          userID,
		Statuses:        statuses,
		CursorCreatedAt: cursorCreatedAt,
		CursorID:        cursorID,
		Limit:           parseLimit(c.Query("limit")),
	})
	if err != nil {
		failService(c, err, dkdomain.ErrCodeJobNotFound)
		return
	}
	if page == nil {
		page = &dkdomain.JobPage{}
	}

	prefix := mountPrefixOf(c)
	items := make([]jobDTO, 0, len(page.Jobs))
	for _, job := range page.Jobs {
		items = append(items, newJobDTO(prefix, job))
	}

	var next *string
	if page.HasMore {
		cursor := encodeCursor(page.NextCursorCreatedAt, page.NextCursorID)
		next = &cursor
	}

	c.JSON(http.StatusOK, jobListDTO{
		Items:      items,
		HasMore:    page.HasMore,
		NextCursor: next,
	})
}

// Get 处理 GET /jobs/:uid。
func (h *JobHandler) Get(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid, ok := requireUID(c, "uid", dkdomain.ErrCodeJobNotFound)
	if !ok {
		return
	}

	job, err := h.jobs.GetJob(c.Request.Context(), userID, uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeJobNotFound)
		return
	}
	c.JSON(http.StatusOK, newJobDTO(mountPrefixOf(c), job))
}

// Items 处理 GET /jobs/:uid/items。
//
// **返回顺序必须是 seq 升序**：这是发给 ERP 的对外契约，
// 「按序号取第几张图」得跟返回顺序严格对应。service 侧已经 ORDER BY seq，
// 这里再排一次是**第二道保险** —— 这一条契约破了，ERP 会把图配错商品，
// 而且不会有任何报错。
func (h *JobHandler) Items(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid, ok := requireUID(c, "uid", dkdomain.ErrCodeJobNotFound)
	if !ok {
		return
	}

	views, err := h.jobs.ListJobItems(c.Request.Context(), userID, uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeJobNotFound)
		return
	}

	c.JSON(http.StatusOK, jobItemListDTO{Items: buildItemDTOs(mountPrefixOf(c), uid, views)})
}

// Item 处理 GET /jobs/:uid/items/:seq。
func (h *JobHandler) Item(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid, ok := requireUID(c, "uid", dkdomain.ErrCodeJobNotFound)
	if !ok {
		return
	}
	seq, ok := parsePositiveInt(c.Param("seq"), 0)
	if !ok {
		failCode(c, dkdomain.ErrCodeItemNotFound)
		return
	}

	view, err := h.jobs.GetJobItem(c.Request.Context(), userID, uid, seq)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeItemNotFound)
		return
	}
	if view == nil || view.Item == nil {
		failCode(c, dkdomain.ErrCodeItemNotFound)
		return
	}

	sortImages(view.Images)
	c.JSON(http.StatusOK, newJobItemDTO(mountPrefixOf(c), uid, view))
}

// ItemContent 处理 GET /jobs/:uid/items/:seq/content，直接回这一张的图片字节。
//
// image_index 查询参数默认 1；只有「网关一次返回多张」时才会用到大于 1 的值
// （还没实测，字段先留着，成本为零）。
func (h *JobHandler) ItemContent(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid, ok := requireUID(c, "uid", dkdomain.ErrCodeJobNotFound)
	if !ok {
		return
	}
	seq, ok := parsePositiveInt(c.Param("seq"), 0)
	if !ok {
		failCode(c, dkdomain.ErrCodeItemNotFound)
		return
	}
	imageIndex, ok := parsePositiveInt(c.Query("image_index"), 1)
	if !ok {
		failCode(c, dkdomain.ErrCodeImageNotFound)
		return
	}

	blob, err := h.jobs.OpenJobItemContent(c.Request.Context(), userID, uid, seq, imageIndex)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeImageNotFound)
		return
	}
	writeContent(c, blob, dkdomain.ErrCodeImageNotFound)
}

// ----------------------------------------------------------------------------
// 内部工具
// ----------------------------------------------------------------------------

// buildItemDTOs 把 item 视图转成响应体，并**强制按 seq 升序**。
func buildItemDTOs(prefix, jobUID string, views []*JobItemView) []jobItemDTO {
	ordered := make([]*JobItemView, 0, len(views))
	for _, v := range views {
		if v == nil || v.Item == nil {
			continue
		}
		sortImages(v.Images)
		ordered = append(ordered, v)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Item.Seq < ordered[j].Item.Seq
	})

	items := make([]jobItemDTO, 0, len(ordered))
	for _, v := range ordered {
		items = append(items, newJobItemDTO(prefix, jobUID, v))
	}
	return items
}

// sortImages 结果图按 image_index 升序。
func sortImages(images []*dkdomain.Image) {
	sort.SliceStable(images, func(i, j int) bool {
		if images[i] == nil || images[j] == nil {
			return images[j] == nil
		}
		return images[i].ImageIndex < images[j].ImageIndex
	})
}

// bindJSON 解析请求体，失败时直接写好错误响应并返回 false。
func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("提交的内容格式不对，请刷新页面重试。").
			WithCause(err))
		return false
	}
	return true
}

// buildSpec 校验并归一化「这一批要出哪些图」。
//
// 这里只做**跟配置无关**的校验（格式、非空、条数防爆闸）；
// 比例白名单和单次批量上限要读 designkit_settings，由 service 负责。
func (h *JobHandler) buildSpec(c *gin.Context, userID int64, req jobSpecRequest) (JobSpec, bool) {
	ratio, err := dkdomain.ParseRatio(req.Ratio)
	if err != nil {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeRatioNotAllowed).WithCause(err))
		return JobSpec{}, false
	}

	assetUIDs := trimAll(req.AssetUIDs)
	if len(assetUIDs) == 0 {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("请至少选择一张商品图。"))
		return JobSpec{}, false
	}

	promptUIDs := trimAll(req.PromptUIDs)
	promptTexts := trimAll(req.Prompts)
	if len(promptUIDs)+len(promptTexts) == 0 {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("请至少选择一条提示词，或者自己写一条。"))
		return JobSpec{}, false
	}

	if len(assetUIDs) > maxSelectionEntries || len(promptUIDs)+len(promptTexts) > maxSelectionEntries {
		failCodef(c, dkdomain.ErrCodeBatchTooLarge,
			"一次最多选 %d 张商品图和 %d 条提示词，请分几批提交。",
			maxSelectionEntries, maxSelectionEntries)
		return JobSpec{}, false
	}

	return JobSpec{
		UserID:           userID,
		Origin:           originOf(c),
		Ratio:            ratio,
		AssetUIDs:        assetUIDs,
		PromptUIDs:       promptUIDs,
		PromptTexts:      promptTexts,
		Model:            strings.TrimSpace(req.Model),
		KeepTransparency: req.KeepTransparency,
	}, true
}

// trimAll 去掉首尾空白并丢掉空串，保持原顺序（顺序就是展开顺序，不能排序也不能去重）。
func trimAll(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// validCallbackURL 回调地址要么不填，要么是完整的 http/https 网址。
func validCallbackURL(raw string) bool {
	if raw == "" {
		return true
	}
	if len(raw) > 1024 {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if parsed.Host == "" {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

// parseJobStatuses 解析 ?status=running&status=succeeded（也支持逗号分隔）。
func parseJobStatuses(c *gin.Context) ([]dkdomain.JobStatus, bool) {
	raw := c.QueryArray("status")
	statuses := make([]dkdomain.JobStatus, 0, len(raw))
	for _, group := range raw {
		for _, piece := range strings.Split(group, ",") {
			piece = strings.TrimSpace(piece)
			if piece == "" {
				continue
			}
			status := dkdomain.JobStatus(piece)
			if !status.IsValid() {
				abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
					WithMessagef("不认识的任务状态 %q，请从列表里选。", piece))
				return nil, false
			}
			statuses = append(statuses, status)
		}
	}
	return statuses, true
}

// apiKeyIDOf 取上下文里的 API Key id；没有返回 0，表示让 service 用该用户的内部专用 Key。
//
// ERP 走上游 apiKeyAuth，上下文里有真实的 Key（费用记在 ERP 自己名下）；
// 浏览器端走 jwtAuth，没有 Key，由 service 兜底。
// designkit_jobs.api_key_id 是 NOT NULL —— 出图时上游 Images() 取不到 Key 直接 401。
func apiKeyIDOf(c *gin.Context) int64 {
	apiKey, ok := upstreammw.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil {
		return 0
	}
	return apiKey.ID
}

// failIdempotency 把幂等协调器返回的错误翻成我方格式。
//
// 注意：协调器会把**业务错误原样透传**回来（execute 的返回值），
// 所以这里先看它自己的 reason，认不出来再交给 failService 按业务错误处理。
func (h *JobHandler) failIdempotency(c *gin.Context, err error, notFoundCode string) {
	if retryAfter := upstreamservice.RetryAfterSecondsFromError(err); retryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(retryAfter))
	}
	if code, ok := upstreamCodeToDesignkitCode(infraerrors.Reason(err)); ok {
		abortWithDesignkitError(c, dkdomain.NewError(code).WithCause(err))
		return
	}
	failService(c, err, notFoundCode)
}

// requireIdempotencyKey 取出并归一化 Idempotency-Key；缺失或不合法时
// 已经写好错误响应，调用方直接 return 即可。
//
// **缺 key 一律由我们自己 400**，不能依赖上游协调器的 RequireKey ——
// 它受 ObserveOnly 开关控制，而那个开关默认 true，不带 key 会**直接放行执行**：
// ERP 一次超时重发就是重复下单、重复扣钱。
func requireIdempotencyKey(c *gin.Context) (string, bool) {
	rawKey := strings.TrimSpace(c.GetHeader(idempotencyHeader))
	if rawKey == "" {
		failCode(c, dkdomain.ErrCodeIdempotencyKeyRequired)
		return "", false
	}
	key, err := upstreamservice.NormalizeIdempotencyKey(rawKey)
	if err != nil || key == "" {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("防重复标识不合法：只能是 1~128 个可见的英文字符（不能有空格和中文）。").
			WithCause(err))
		return "", false
	}
	return key, true
}

// idempotentWrite 描述一次「花钱的写操作」，交给 runIdempotent 执行。
type idempotentWrite struct {
	// UserID 操作人。**必须揉进 Scope**：上游协调器的唯一键是 (Scope, keyHash)、
	// 不含用户，两个人凑巧用了同一个 key 会互相撞成 409。
	UserID int64
	// Scope 幂等作用域（已经拼好 user id）。
	Scope string
	// Route 幂等指纹里的 route，用常量不用 c.FullPath()（同一 handler 挂两个前缀）。
	Route string
	// Key 归一化之后的 Idempotency-Key。
	Key string
	// Payload 内容快照。同 key 同内容 → 返回原结果；同 key 不同内容 → 409。
	Payload any
	// SuccessStatus 成功时的 HTTP 状态码（提交 201、重试 200）。
	SuccessStatus int
	// NotFoundCode 业务错误是「查不到」时该用哪个错误码。
	NotFoundCode string
	// Execute 真正干活的那一下。
	Execute func(ctx context.Context) (any, error)
}

// runIdempotent 走上游幂等协调器执行一次写操作。
func (h *JobHandler) runIdempotent(c *gin.Context, w idempotentWrite) {
	coordinator := upstreamservice.DefaultIdempotencyCoordinator()
	if coordinator == nil {
		// 协调器没装好时不能悄悄退化成「不做幂等」——那正是重复扣钱的场景。
		// 但也不能直接拒绝服务，所以：照常执行 + 一条明确的告警日志。
		slog.Warn("designkit 幂等协调器未初始化，本次写操作没有防重复保护",
			slog.Int64("user_id", w.UserID),
			slog.String("route", w.Route))
		data, err := w.Execute(c.Request.Context())
		if err != nil {
			failService(c, err, w.NotFoundCode)
			return
		}
		c.JSON(w.SuccessStatus, data)
		return
	}

	result, err := coordinator.Execute(c.Request.Context(), upstreamservice.IdempotencyExecuteOptions{
		Scope:          w.Scope,
		ActorScope:     "user:" + strconv.FormatInt(w.UserID, 10),
		Method:         http.MethodPost,
		Route:          w.Route,
		IdempotencyKey: w.Key,
		Payload:        w.Payload,
		RequireKey:     true,
		TTL:            upstreamservice.DefaultWriteIdempotencyTTL(),
	}, w.Execute)
	if err != nil {
		h.failIdempotency(c, err, w.NotFoundCode)
		return
	}
	if result == nil {
		// err 为 nil 时协调器一定给结果；真拿到 nil 说明上游行为变了。
		// 宁可报一个明确的错，也不能回一个空 body 让 ERP 以为提交成功了。
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInternal))
		return
	}
	if result.Replayed {
		// 命中重放：这次没有新建任务、没有新冻结额度，返回的是上一次的结果。
		c.Header(idempotencyReplayedHeader, "true")
	}
	c.JSON(w.SuccessStatus, result.Data)
}
