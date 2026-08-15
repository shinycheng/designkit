package handler

import (
	"errors"
	"io"
	"net/http"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 我的消费（决策 16 / 决策 19，设计定型 第三节「消费」）
// ============================================================================
//
// 两个端点，都只读写我们自己的数据库，不碰对象存储也不碰出图网关：
//
//	GET  /me/usage/summary   工作台角落的「本月出图 N 张 / 花费 $X / 余额 $Y」
//	POST /me/quota-requests  余额不足时的「申请额度」按钮
//
// 为什么必须有这两个：上游原生的充值、兑换码、用量、订单菜单对普通运营是
// **隐藏**的（决策 10）。没有这两个端点，运营看到「余额不足」就是死路一条，
// 而管理员那边**没有任何机制会主动知道有人卡住了**。

// maxQuotaRequestNoteRunes 「申请额度」说明的长度上限（按字数算，不是字节）。
//
// 200 字够写清「双十一主图要赶工，还差大概 30 张」，又不至于让人把
// 整份需求文档粘进来。超了直接告诉他精简，不要静默截断 ——
// 截断之后管理员看到的是半句话，还不知道被截过。
const maxQuotaRequestNoteRunes = 200

// MeHandler 「我的消费」。
type MeHandler struct {
	me MeService
}

// NewMeHandler 建 handler。
func NewMeHandler(me MeService) *MeHandler {
	return &MeHandler{me: me}
}

// UsageSummary 处理 GET /me/usage/summary（决策 16）。
//
// 这是工作台**每次打开都会调**的端点，必须快、必须永远给得出数。
// 金额一律美元（决策 18），响应里带 currency=USD，界面渲染 `$`。
func (h *MeHandler) UsageSummary(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}

	summary, err := h.me.GetUsageSummary(c.Request.Context(), userID)
	if err != nil {
		// 这里查不到只可能是「用户行没了」（余额是从 users 表上取的），
		// 所以「找不到」翻成「登录已过期，请重新登录」而不是某个资源 404。
		failService(c, err, dkdomain.ErrCodeUnauthorized)
		return
	}
	if summary == nil {
		failCode(c, dkdomain.ErrCodeInternal)
		return
	}
	c.JSON(http.StatusOK, newUsageSummaryDTO(summary))
}

// quotaRequestBody 是 POST /me/quota-requests 的请求体。整个 body 都可以不传。
type quotaRequestBody struct {
	// Note 运营自己写的说明，可以不填。
	Note string `json:"note"`
}

// CreateQuotaRequest 处理 POST /me/quota-requests（决策 19）。
//
// 「余额不足」旁边那个「申请额度」按钮点下去就是这里：在管理员后台生成一条
// 待处理记录。**不需要填任何东西也能提交** —— 让一个已经卡住的人先填表单
// 才能求助，是最容易被放弃的一步。
//
// 同一个人已经有一条没处理完的申请时，数据库的部分唯一索引会挡下来
// （防止连点刷屏），这里翻成「不用重复提交」而不是一个技术味的冲突提示。
func (h *MeHandler) CreateQuotaRequest(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}

	var req quotaRequestBody
	// 空 body（io.EOF）是合法的：界面上那个按钮点一下就发，不带任何内容。
	// 其余解析错误照常报「提交的内容格式不对」。
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("提交的内容格式不对，请刷新页面重试。").
			WithCause(err))
		return
	}

	note := strings.TrimSpace(req.Note)
	if len([]rune(note)) > maxQuotaRequestNoteRunes {
		failCodef(c, dkdomain.ErrCodeInvalidRequest,
			"说明最多写 %d 个字，请精简一下再提交。", maxQuotaRequestNoteRunes)
		return
	}

	result, err := h.me.CreateQuotaRequest(c.Request.Context(), userID, note)
	if err != nil {
		if errors.Is(err, dkdomain.ErrConflict) {
			abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeIllegalStateTransition).
				WithMessage("你已经提交过一次申请了，管理员还没处理，不用重复提交。").
				WithCause(err))
			return
		}
		failService(c, err, dkdomain.ErrCodeUnauthorized)
		return
	}
	if result == nil || result.Request == nil {
		failCode(c, dkdomain.ErrCodeInternal)
		return
	}
	c.JSON(http.StatusCreated, newQuotaRequestDTO(result))
}
