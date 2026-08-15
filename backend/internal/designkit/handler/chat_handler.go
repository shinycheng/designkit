package handler

import (
	"net/http"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// AI 对话（monica 2026-08-15 拍板：会话保存 / 能发图 / 所有运营可用）
// ============================================================================
//
// 四个端点：
//
//	POST   /chat/messages            发一条消息并等 AI 回复（这一条**花钱**，按 token 计费）
//	GET    /chat/sessions            会话列表
//	GET    /chat/sessions/:uid       一个会话 + 全部消息
//	DELETE /chat/sessions/:uid       删除会话
//
// 发消息是同步等回复的：AI 回答要几秒到几十秒，前端把这一条的超时放宽到
// 120 秒（api/chat.ts），服务端单趟 60 秒（service 的 conversationTimeout），
// 后端必须明显小于前端，否则前端先报超时、后端还在算还在花钱。

// ChatHandler 「AI 对话」页。
type ChatHandler struct {
	chat ChatConversationService
}

// NewChatHandler 建 handler。
func NewChatHandler(chat ChatConversationService) *ChatHandler {
	return &ChatHandler{chat: chat}
}

// chatSendBody 是 POST /chat/messages 的请求体。
type chatSendBody struct {
	// SessionUID 目标会话；空 = 新建。
	SessionUID string `json:"session_uid"`
	// Text 正文。
	Text string `json:"text"`
	// AssetUIDs 附带的商品图 uid。
	AssetUIDs []string `json:"asset_uids"`
}

// Send 处理 POST /chat/messages。
func (h *ChatHandler) Send(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	var body chatSendBody
	if err := c.ShouldBindJSON(&body); err != nil {
		failCodef(c, dkdomain.ErrCodeInvalidRequest, "请求体不是合法的 JSON：%v", err)
		return
	}

	result, err := h.chat.Send(c.Request.Context(), ChatSendInput{
		UserID:     userID,
		SessionUID: strings.TrimSpace(body.SessionUID),
		Text:       body.Text,
		AssetUIDs:  body.AssetUIDs,
	})
	if err != nil {
		failService(c, err, dkdomain.ErrCodeChatSessionNotFound)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"session_uid":       result.Session.UID,
		"title":             result.Session.Title,
		"user_message":      newChatMessageDTO(result.UserMessage),
		"assistant_message": newChatMessageDTO(result.AssistantMessage),
	})
}

// ListSessions 处理 GET /chat/sessions。
func (h *ChatHandler) ListSessions(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	sessions, err := h.chat.ListSessions(c.Request.Context(), userID)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeChatSessionNotFound)
		return
	}
	out := make([]gin.H, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, newChatSessionDTO(s))
	}
	c.JSON(http.StatusOK, gin.H{"sessions": out})
}

// GetSession 处理 GET /chat/sessions/:uid。
func (h *ChatHandler) GetSession(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid := strings.TrimSpace(c.Param("uid"))
	if uid == "" {
		failCode(c, dkdomain.ErrCodeChatSessionNotFound)
		return
	}
	session, msgs, err := h.chat.GetSession(c.Request.Context(), userID, uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeChatSessionNotFound)
		return
	}
	out := make([]gin.H, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, newChatMessageDTO(m))
	}
	c.JSON(http.StatusOK, gin.H{
		"session":  newChatSessionDTO(session),
		"messages": out,
	})
}

// DeleteSession 处理 DELETE /chat/sessions/:uid。
func (h *ChatHandler) DeleteSession(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid := strings.TrimSpace(c.Param("uid"))
	if uid == "" {
		failCode(c, dkdomain.ErrCodeChatSessionNotFound)
		return
	}
	if err := h.chat.DeleteSession(c.Request.Context(), userID, uid); err != nil {
		failService(c, err, dkdomain.ErrCodeChatSessionNotFound)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// newChatSessionDTO 会话 → JSON。
func newChatSessionDTO(s *dkdomain.ChatSession) gin.H {
	return gin.H{
		"uid":        s.UID,
		"title":      s.Title,
		"created_at": timeString(s.CreatedAt),
		"updated_at": timeString(s.UpdatedAt),
	}
}

// newChatMessageDTO 消息 → JSON。asset_uids 永远给数组，不给 null——
// 前端少一个判空分支就少一个崩法。
func newChatMessageDTO(m *dkdomain.ChatMessage) gin.H {
	uids := m.AssetUIDs
	if uids == nil {
		uids = []string{}
	}
	return gin.H{
		"id":         m.ID,
		"role":       m.Role,
		"content":    m.Content,
		"asset_uids": uids,
		"created_at": timeString(m.CreatedAt),
	}
}
