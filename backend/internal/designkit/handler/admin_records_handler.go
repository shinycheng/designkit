package handler

import (
	"net/http"
	"strconv"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 用户记录（管理端）
// ============================================================================
//
// 六个端点，只挂浏览器组、只放管理员（挂载见 register_business.go 的
// mountAdminRecordsRoutes）：
//
//	GET /admin/records/users                          有记录的账户（筛选下拉）
//	GET /admin/records/chat/sessions                  会话列表（?user_id=&limit=&offset=）
//	GET /admin/records/chat/sessions/:uid             一个会话 + 全部消息
//	GET /admin/records/jobs                           批次列表（?user_id=&limit=&offset=）
//	GET /admin/records/jobs/:uid                      一个批次 + 每一张
//	GET /admin/records/jobs/:uid/items/:seq/content   某一张的图片字节（缩略图）
//
// 全部只读。service 侧不做归属校验（管理员通道），所以 RequireAdmin
// 是这一组唯一的门 —— 单测守着「非管理员 403、service 一次都不被调到」。

// 列表分页（对外契约：limit 默认 50、封顶 200）。
const (
	adminRecordsDefaultLimit = 50
	adminRecordsMaxLimit     = 200
)

// AdminRecordsHandler 「用户记录」的管理端。
type AdminRecordsHandler struct {
	svc AdminRecordsService
}

// NewAdminRecordsHandler 建 handler。
func NewAdminRecordsHandler(svc AdminRecordsService) *AdminRecordsHandler {
	return &AdminRecordsHandler{svc: svc}
}

// adminRecordsListQuery 解三个列表参数。
//
//   - user_id 省略 = 全部账户（返回 0）；给了就必须是正整数，解析不了直接 400
//     —— 静默当成「全部」的话，前端拼错参数会一直看着全量数据以为在看某个人。
//   - limit 默认 50；给了必须是正整数；**超过 200 收敛到 200**（封顶是收敛不是报错，
//     前端把「尽量多拉」写成 limit=1000 不该是个错误）。
//   - offset 默认 0；给了必须是不小于 0 的整数。
func adminRecordsListQuery(c *gin.Context) (userID int64, limit, offset int, ok bool) {
	if raw := strings.TrimSpace(c.Query("user_id")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			failCodef(c, dkdomain.ErrCodeInvalidRequest, "user_id 要是大于 0 的整数。")
			return 0, 0, 0, false
		}
		userID = parsed
	}

	limit = adminRecordsDefaultLimit
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			failCodef(c, dkdomain.ErrCodeInvalidRequest, "limit 要是大于 0 的整数。")
			return 0, 0, 0, false
		}
		limit = parsed
	}
	if limit > adminRecordsMaxLimit {
		limit = adminRecordsMaxLimit
	}

	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			failCodef(c, dkdomain.ErrCodeInvalidRequest, "offset 要是不小于 0 的整数。")
			return 0, 0, 0, false
		}
		offset = parsed
	}
	return userID, limit, offset, true
}

// ---- DTO ----

// recordUserDTO 「有记录的账户」下拉框里的一项。
// **这里暴露上游 users 的数字 id**：它就是筛选参数 user_id 的取值，
// 跟额度申请管理页暴露自增 id 是同一个惯例。
type recordUserDTO struct {
	ID int64 `json:"id"`
	// Email 账号已删时是空串，前端显示「已删除的账号」。
	Email        string `json:"email"`
	SessionCount int    `json:"session_count"`
	JobCount     int    `json:"job_count"`
}

func newRecordUserDTO(u *dkdomain.RecordUser) recordUserDTO {
	if u == nil {
		return recordUserDTO{}
	}
	return recordUserDTO{
		ID:           u.ID,
		Email:        u.Email,
		SessionCount: u.SessionCount,
		JobCount:     u.JobCount,
	}
}

// chatSessionRecordDTO 管理端会话列表里的一行。
type chatSessionRecordDTO struct {
	UID    string `json:"uid"`
	Title  string `json:"title"`
	UserID int64  `json:"user_id"`
	// UserEmail 归属人邮箱；账号已删时是空串。
	UserEmail    string `json:"user_email"`
	MessageCount int    `json:"message_count"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func newChatSessionRecordDTO(v *dkdomain.ChatSessionAdminView) chatSessionRecordDTO {
	if v == nil {
		return chatSessionRecordDTO{}
	}
	return chatSessionRecordDTO{
		UID:          v.UID,
		Title:        v.Title,
		UserID:       v.UserID,
		UserEmail:    v.UserEmail,
		MessageCount: v.MessageCount,
		CreatedAt:    timeString(v.CreatedAt),
		UpdatedAt:    timeString(v.UpdatedAt),
	}
}

// chatMessageRecordDTO 会话详情里的一条消息。
// 形状跟运营侧 newChatMessageDTO 一致（id / role / content / asset_uids / created_at）。
type chatMessageRecordDTO struct {
	ID      int64  `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
	// AssetUIDs 永远给数组，不给 null —— 前端少一个判空分支就少一个崩法。
	AssetUIDs []string `json:"asset_uids"`
	CreatedAt string   `json:"created_at"`
}

func newChatMessageRecordDTO(m *dkdomain.ChatMessage) chatMessageRecordDTO {
	if m == nil {
		return chatMessageRecordDTO{AssetUIDs: []string{}}
	}
	uids := m.AssetUIDs
	if uids == nil {
		uids = []string{}
	}
	return chatMessageRecordDTO{
		ID:        m.ID,
		Role:      m.Role,
		Content:   m.Content,
		AssetUIDs: uids,
		CreatedAt: timeString(m.CreatedAt),
	}
}

// jobRecordDTO 管理端批次列表里的一行。
type jobRecordDTO struct {
	UID    string `json:"uid"`
	Name   string `json:"name"`
	Status string `json:"status"`
	UserID int64  `json:"user_id"`
	// UserEmail 归属人邮箱；账号已删时是空串。
	UserEmail    string `json:"user_email"`
	ItemCount    int    `json:"item_count"`
	SuccessCount int    `json:"success_count"`
	FailCount    int    `json:"fail_count"`
	// ActualCost 结算后的实际花费（美元，十进制字符串）；没结算完是 null。
	ActualCost *string `json:"actual_cost"`
	Currency   string  `json:"currency"`
	Ratio      string  `json:"ratio"`
	CreatedAt  string  `json:"created_at"`
}

func newJobRecordDTO(v *dkdomain.JobAdminView) jobRecordDTO {
	if v == nil {
		return jobRecordDTO{Currency: dkdomain.CurrencyUSD}
	}
	currency := v.Currency
	if currency == "" {
		currency = dkdomain.CurrencyUSD
	}
	return jobRecordDTO{
		UID:          v.UID,
		Name:         v.Name,
		Status:       v.Status.String(),
		UserID:       v.UserID,
		UserEmail:    v.UserEmail,
		ItemCount:    v.ItemCount,
		SuccessCount: v.SuccessCount,
		FailCount:    v.FailCount,
		ActualCost:   moneyStringPtr(v.ActualCost),
		Currency:     currency,
		Ratio:        v.Ratio.String(),
		CreatedAt:    timeString(v.CreatedAt),
	}
}

// jobItemRecordDTO 批次详情里的一张。
type jobItemRecordDTO struct {
	Seq    int    `json:"seq"`
	Status string `json:"status"`
	// Prompt 提交那一刻的提示词快照原文（灵感库后来改了措辞也显示当时的）。
	Prompt string `json:"prompt"`
	// BilledCost 这一张实际扣了多少（美元，十进制字符串）；还没回填是 null。
	BilledCost *string `json:"billed_cost"`
	// HasImage 有没有当前版本的结果图。前端靠它决定要不要去取缩略图。
	HasImage bool `json:"has_image"`
}

func newJobItemRecordDTO(v *dkdomain.JobItemAdminView) jobItemRecordDTO {
	if v == nil {
		return jobItemRecordDTO{}
	}
	return jobItemRecordDTO{
		Seq:        v.Seq,
		Status:     v.Status.String(),
		Prompt:     v.PromptText,
		BilledCost: moneyStringPtr(v.BilledCost),
		HasImage:   v.HasImage,
	}
}

// ---- 端点 ----

// ListUsers 处理 GET /admin/records/users。
func (h *AdminRecordsHandler) ListUsers(c *gin.Context) {
	users, err := h.svc.ListRecordUsers(c.Request.Context())
	if err != nil {
		failService(c, err, dkdomain.ErrCodeInvalidRequest)
		return
	}
	// 空列表返回 []，不返回 null —— 前端不用多写一层判空（全组同此）。
	out := make([]recordUserDTO, 0, len(users))
	for _, u := range users {
		if u == nil {
			continue
		}
		out = append(out, newRecordUserDTO(u))
	}
	c.JSON(http.StatusOK, gin.H{"users": out})
}

// ListChatSessions 处理 GET /admin/records/chat/sessions。
func (h *AdminRecordsHandler) ListChatSessions(c *gin.Context) {
	userID, limit, offset, ok := adminRecordsListQuery(c)
	if !ok {
		return
	}
	sessions, err := h.svc.ListChatSessionRecords(c.Request.Context(), userID, limit, offset)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeChatSessionNotFound)
		return
	}
	out := make([]chatSessionRecordDTO, 0, len(sessions))
	for _, s := range sessions {
		if s == nil {
			continue
		}
		out = append(out, newChatSessionRecordDTO(s))
	}
	c.JSON(http.StatusOK, gin.H{"sessions": out})
}

// GetChatSession 处理 GET /admin/records/chat/sessions/:uid。
func (h *AdminRecordsHandler) GetChatSession(c *gin.Context) {
	uid, ok := requireUID(c, "uid", dkdomain.ErrCodeChatSessionNotFound)
	if !ok {
		return
	}
	session, messages, err := h.svc.GetChatSessionRecord(c.Request.Context(), uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeChatSessionNotFound)
		return
	}
	if session == nil {
		failCode(c, dkdomain.ErrCodeChatSessionNotFound)
		return
	}
	out := make([]chatMessageRecordDTO, 0, len(messages))
	for _, m := range messages {
		if m == nil {
			continue
		}
		out = append(out, newChatMessageRecordDTO(m))
	}
	c.JSON(http.StatusOK, gin.H{
		"session":  newChatSessionRecordDTO(session),
		"messages": out,
	})
}

// ListJobs 处理 GET /admin/records/jobs。
func (h *AdminRecordsHandler) ListJobs(c *gin.Context) {
	userID, limit, offset, ok := adminRecordsListQuery(c)
	if !ok {
		return
	}
	jobs, err := h.svc.ListJobRecords(c.Request.Context(), userID, limit, offset)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeJobNotFound)
		return
	}
	out := make([]jobRecordDTO, 0, len(jobs))
	for _, j := range jobs {
		if j == nil {
			continue
		}
		out = append(out, newJobRecordDTO(j))
	}
	c.JSON(http.StatusOK, gin.H{"jobs": out})
}

// GetJob 处理 GET /admin/records/jobs/:uid。
func (h *AdminRecordsHandler) GetJob(c *gin.Context) {
	uid, ok := requireUID(c, "uid", dkdomain.ErrCodeJobNotFound)
	if !ok {
		return
	}
	job, items, err := h.svc.GetJobRecord(c.Request.Context(), uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeJobNotFound)
		return
	}
	if job == nil {
		failCode(c, dkdomain.ErrCodeJobNotFound)
		return
	}
	out := make([]jobItemRecordDTO, 0, len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		out = append(out, newJobItemRecordDTO(it))
	}
	c.JSON(http.StatusOK, gin.H{
		"job":   newJobRecordDTO(job),
		"items": out,
	})
}

// ItemContent 处理 GET /admin/records/jobs/:uid/items/:seq/content。
// 返回图片字节，供前端带凭证加载缩略图。
func (h *AdminRecordsHandler) ItemContent(c *gin.Context) {
	uid, ok := requireUID(c, "uid", dkdomain.ErrCodeJobNotFound)
	if !ok {
		return
	}
	seq, ok := parsePositiveInt(c.Param("seq"), 0)
	if !ok {
		failCode(c, dkdomain.ErrCodeItemNotFound)
		return
	}
	blob, err := h.svc.OpenJobRecordItemContent(c.Request.Context(), uid, seq)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeImageNotFound)
		return
	}
	writeContent(c, blob, dkdomain.ErrCodeImageNotFound)
}
