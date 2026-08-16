package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 额度申请（管理端，决策 19 的闭环）
// ============================================================================
//
// 两个端点，只挂浏览器组、只放管理员（挂载见 register_business.go 的
// mountQuotaAdminRoutes）：
//
//	GET  /admin/quota-requests?status=pending|handled   两个 tab 的列表
//	POST /admin/quota-requests/:id/handle               通过（加余额）/ 驳回
//
// 为什么必须有：运营点「申请额度」之后，那条记录进的是一张管理员看不见的表 ——
// 决策 19 想解决的「余额不足是死路」只解决了一半。这一页就是另一半。
//
// 「通过」会真的给申请人加余额（走上游 AdminService.UpdateUserBalance 的
// 原子通道，见 module.go 的 quotaAdminServiceAdapter）—— 所以金额校验必须严：
// 0 和负数直接拒，解析不了直接拒，绝不默认成任何数。

// maxQuotaHandleNoteRunes 处理备注的长度上限（按字数算），跟申请说明一致。
const maxQuotaHandleNoteRunes = 200

// quotaAdminListDefaultLimit 列表默认条数。管理页不做翻页（待办清完就空了），
// 一次给到 repository 的上限（100），超过 100 条待办说明流程早就出问题了。
const quotaAdminListDefaultLimit = 100

// QuotaAdminHandler 额度申请的管理端。
type QuotaAdminHandler struct {
	svc QuotaAdminService
}

// NewQuotaAdminHandler 建 handler。
func NewQuotaAdminHandler(svc QuotaAdminService) *QuotaAdminHandler {
	return &QuotaAdminHandler{svc: svc}
}

// quotaRequestAdminDTO 管理端列表里的一行。
//
// **这里暴露自增 id**：管理员要拿它去点「通过/驳回」，跟上游用户管理页
// 用数字 id 是同一个惯例。运营侧接口仍然不暴露（见 quotaRequestDTO 的注释）。
type quotaRequestAdminDTO struct {
	ID int64 `json:"id"`
	// RequesterEmail 申请人邮箱；账号已删时是空串，前端显示「已删除的账号」。
	RequesterEmail string `json:"requester_email"`
	// Note 运营自己填的申请说明，没填是 null。
	Note *string `json:"note"`
	// Status pending / handled / rejected。
	Status string `json:"status"`
	// HandleNote 管理员处理时写的备注，没写是 null。
	HandleNote *string `json:"handle_note"`
	// ApprovedAmount 通过时加的金额（美元，十进制字符串）；驳回或未处理是 null。
	ApprovedAmount *string `json:"approved_amount"`
	// HandledByEmail 处理人邮箱，未处理是 null。
	HandledByEmail *string `json:"handled_by_email"`
	// HandledAt 处理时间，未处理是 null。
	HandledAt *string `json:"handled_at"`
	// CreatedAt 申请时间。
	CreatedAt string `json:"created_at"`
}

func newQuotaRequestAdminDTO(d *dkdomain.QuotaRequestDetail) quotaRequestAdminDTO {
	dto := quotaRequestAdminDTO{
		ID:             d.ID,
		RequesterEmail: d.RequesterEmail,
		Note:           d.Note,
		Status:         string(d.Status),
		HandleNote:     d.HandleNote,
		HandledByEmail: d.HandledByEmail,
		HandledAt:      timeStringPtr(d.HandledAt),
		CreatedAt:      timeString(d.CreatedAt),
	}
	if d.ApprovedAmount != nil {
		amount := dkdomain.MoneyString(*d.ApprovedAmount)
		dto.ApprovedAmount = &amount
	}
	return dto
}

// quotaRequestAdminListDTO 是 GET /admin/quota-requests 的响应。
type quotaRequestAdminListDTO struct {
	Items []quotaRequestAdminDTO `json:"items"`
	// PendingCount 全部待处理条数。侧边栏红点也用这个接口
	// （?status=pending&limit=1），跟 Items 无关，看哪个 tab 都照给。
	PendingCount int `json:"pending_count"`
}

// List 处理 GET /admin/quota-requests。
//
// status=pending（默认）看待处理，status=handled 看处理过的（通过 + 驳回都算）。
// 别的值直接 400 —— 静默当成默认值的话，前端拼错参数会一直看着「待处理」
// 却以为在看「已处理」。
func (h *QuotaAdminHandler) List(c *gin.Context) {
	if _, ok := userIDOf(c); !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}

	var pendingOnly bool
	switch c.DefaultQuery("status", "pending") {
	case "pending":
		pendingOnly = true
	case "handled":
		pendingOnly = false
	default:
		failCodef(c, dkdomain.ErrCodeInvalidRequest,
			"status 只能是 pending 或 handled。")
		return
	}

	limit := quotaAdminListDefaultLimit
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			failCodef(c, dkdomain.ErrCodeInvalidRequest, "limit 要是大于 0 的整数。")
			return
		}
		limit = parsed
	}
	offset := 0
	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			failCodef(c, dkdomain.ErrCodeInvalidRequest, "offset 要是不小于 0 的整数。")
			return
		}
		offset = parsed
	}

	list, err := h.svc.ListQuotaRequests(c.Request.Context(), pendingOnly, limit, offset)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeQuotaRequestNotFound)
		return
	}
	if list == nil {
		failCode(c, dkdomain.ErrCodeInternal)
		return
	}

	// 空列表返回 []，不返回 null —— 前端不用多写一层判空。
	items := make([]quotaRequestAdminDTO, 0, len(list.Items))
	for _, item := range list.Items {
		if item == nil {
			continue
		}
		items = append(items, newQuotaRequestAdminDTO(item))
	}
	c.JSON(http.StatusOK, quotaRequestAdminListDTO{
		Items:        items,
		PendingCount: list.PendingCount,
	})
}

// handleQuotaRequestBody 是 POST /admin/quota-requests/:id/handle 的请求体。
type handleQuotaRequestBody struct {
	// Action approve=通过并加额，reject=驳回。
	Action string `json:"action"`
	// Note 处理备注，可以不填。
	Note string `json:"note"`
	// Amount 通过时加多少（美元，十进制字符串，例如 "50"）。
	// 用字符串不用数字：金额走 JSON 浮点数会带来 0.1+0.2 那类精度噪音。
	Amount string `json:"amount"`
}

// handledQuotaRequestDTO 是处理成功的响应。前端提示完会重新拉列表，
// 这里只回「刚发生了什么」，不回整行详情。
type handledQuotaRequestDTO struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	// ApprovedAmount 通过时实际加的金额（美元，十进制字符串）；驳回是 null。
	ApprovedAmount *string `json:"approved_amount"`
	// HandledAt 处理时间。
	HandledAt *string `json:"handled_at"`
}

// Handle 处理 POST /admin/quota-requests/:id/handle。
//
// 金额校验必须在这里拦死：**通过 = 真金白银给申请人加余额**。
// 0、负数、解析不了、没填，一律 400，绝不默认成任何数。
func (h *QuotaAdminHandler) Handle(c *gin.Context) {
	adminID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}

	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		failCode(c, dkdomain.ErrCodeQuotaRequestNotFound)
		return
	}

	var req handleQuotaRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("提交的内容格式不对，请刷新页面重试。").
			WithCause(err))
		return
	}

	var approve bool
	switch req.Action {
	case "approve":
		approve = true
	case "reject":
		approve = false
	default:
		failCodef(c, dkdomain.ErrCodeInvalidRequest,
			"action 只能是 approve（通过）或 reject（驳回）。")
		return
	}

	note := strings.TrimSpace(req.Note)
	if len([]rune(note)) > maxQuotaHandleNoteRunes {
		failCodef(c, dkdomain.ErrCodeInvalidRequest,
			"备注最多写 %d 个字，请精简一下再提交。", maxQuotaHandleNoteRunes)
		return
	}

	amount := dkdomain.ZeroMoney
	if approve {
		raw := strings.TrimSpace(req.Amount)
		if raw == "" {
			failCodef(c, dkdomain.ErrCodeInvalidRequest,
				"通过前先填「加多少（美元）」，例如 50。")
			return
		}
		parsed, parseErr := dkdomain.MoneyFromString(raw)
		if parseErr != nil {
			failCodef(c, dkdomain.ErrCodeInvalidRequest,
				"金额「%s」不是数字。填一个大于 0 的数，例如 50。", raw)
			return
		}
		if !parsed.IsPositive() {
			failCodef(c, dkdomain.ErrCodeInvalidRequest,
				"金额要大于 0（收到 %s）。0 或负数不会给申请人加余额。", raw)
			return
		}
		amount = parsed
	}

	handled, err := h.svc.HandleQuotaRequest(c.Request.Context(), HandleQuotaRequestInput{
		ID:          id,
		AdminUserID: adminID,
		Approve:     approve,
		Note:        note,
		Amount:      amount,
	})
	if err != nil {
		// service 自己带了中文 message 的错误（加余额失败那两种）原样透传；
		// 裸的 ErrConflict（两个管理员同时点）换成说清情况的一句。
		if _, isOurs := dkdomain.AsDesignkitError(err); !isOurs && errors.Is(err, dkdomain.ErrConflict) {
			abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeIllegalStateTransition).
				WithMessage("这条申请已经被处理过了（可能是另一位管理员），刷新列表看结果。").
				WithCause(err))
			return
		}
		failService(c, err, dkdomain.ErrCodeQuotaRequestNotFound)
		return
	}
	if handled == nil {
		failCode(c, dkdomain.ErrCodeInternal)
		return
	}

	dto := handledQuotaRequestDTO{
		ID:        handled.ID,
		Status:    string(handled.Status),
		HandledAt: timeStringPtr(handled.HandledAt),
	}
	if approve {
		amountText := dkdomain.MoneyString(amount)
		dto.ApprovedAmount = &amountText
	}
	c.JSON(http.StatusOK, dto)
}
