package handler

// ============================================================================
// 我的提示词（运营自建）
// ============================================================================
//
// 三条写端点，读取走灵感库现有的列表 / 详情（GET /prompts?source=user）：
//
//	POST   /prompts        存一条（标题 + 正文）
//	PUT    /prompts/:uid   改一条自己的
//	DELETE /prompts/:uid   删一条自己的（软删，历史任务里的快照不受影响）
//
// 三条都**不花钱**，挂在 mountCommonRoutes 那一组（额度耗尽照样能整理自己的词）。
// 服务缺席时整组不挂（裸 404 = 功能没上线，跟「AI 挑提示词」同一套前端约定）。
//
// 权限规则都在 service 侧（handler 只传 userID）：
//   - 别人的自建词 → 404（不泄露编号存在）；
//   - youmind 来源 → 400 + 中文说明（那条词他看得见，报 404 只会让他反复重试）；
//   - 每人上限 200 条、标题 100 字、正文 5000 字。

import (
	"net/http"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/gin-gonic/gin"
)

// myPromptRequestBody 是 POST /prompts 和 PUT /prompts/:uid 的请求体。
type myPromptRequestBody struct {
	// Title 标题，可为空串（灵感库里也有大量只有正文的词，卡片拿正文开头顶上）。
	Title string `json:"title"`
	// Body 提示词正文，必填。
	Body string `json:"body"`
}

// bindMyPromptBody 解请求体。返回 false 时错误响应已经写好，调用方直接 return。
func bindMyPromptBody(c *gin.Context) (myPromptRequestBody, bool) {
	var req myPromptRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("提交的内容格式不对，请刷新页面重试。").
			WithCause(err))
		return myPromptRequestBody{}, false
	}
	return req, true
}

// CreateMine 处理 POST /prompts。
func (h *PromptHandler) CreateMine(c *gin.Context) {
	// 纯防御：服务缺席时这条路由根本不会挂（register_business.go），
	// 走到这里说明装配逻辑被改坏了。
	if h == nil || h.my == nil {
		failCode(c, dkdomain.ErrCodeInternal)
		return
	}
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	req, ok := bindMyPromptBody(c)
	if !ok {
		return
	}

	prompt, err := h.my.CreateMyPrompt(c.Request.Context(), userID, req.Title, req.Body)
	if err != nil {
		failService(c, err, dkdomain.ErrCodePromptNotFound)
		return
	}
	// 复用灵感库的 promptDTO（同一个形状，前端不用第二套类型）。
	// 自建词没有分类，CategorySlug / CategoryName 就是空串。
	c.JSON(http.StatusCreated, newPromptDTO(&PromptView{Prompt: prompt}))
}

// UpdateMine 处理 PUT /prompts/:uid。
func (h *PromptHandler) UpdateMine(c *gin.Context) {
	if h == nil || h.my == nil {
		failCode(c, dkdomain.ErrCodeInternal)
		return
	}
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
	req, ok := bindMyPromptBody(c)
	if !ok {
		return
	}

	prompt, err := h.my.UpdateMyPrompt(c.Request.Context(), userID, uid, req.Title, req.Body)
	if err != nil {
		failService(c, err, dkdomain.ErrCodePromptNotFound)
		return
	}
	c.JSON(http.StatusOK, newPromptDTO(&PromptView{Prompt: prompt}))
}

// DeleteMine 处理 DELETE /prompts/:uid。
func (h *PromptHandler) DeleteMine(c *gin.Context) {
	if h == nil || h.my == nil {
		failCode(c, dkdomain.ErrCodeInternal)
		return
	}
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

	if err := h.my.DeleteMyPrompt(c.Request.Context(), userID, uid); err != nil {
		failService(c, err, dkdomain.ErrCodePromptNotFound)
		return
	}
	// 跟 DELETE /chat/sessions/:uid 一个形状（{"ok":true}），不发明第三种。
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
