package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
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
//
// POST /chat/messages 支持可选的 "stream": true：响应变成 text/event-stream，
// 回复边生成边推（打字机）。帧格式是对外契约，定了别改：
//
//	event: delta   data: {"text":"…"}                  正文的一段
//	event: done    data: {与非流式相同结构的完整响应}     成功收尾（此后关流）
//	event: error   data: {标准错误信封 JSON}             失败收尾（此后关流）
//
// 流开始**之前**的失败仍走普通 JSON 错误（状态码 + 错误信封）——
// 那时状态码还改得了，改得了就不该藏进 200 的流里。

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
	// Stream true = 用 SSE 边生成边推（帧格式见文件头）。默认 false，响应不变。
	Stream bool `json:"stream"`
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
	in := ChatSendInput{
		UserID:     userID,
		SessionUID: strings.TrimSpace(body.SessionUID),
		Text:       body.Text,
		AssetUIDs:  body.AssetUIDs,
	}

	if body.Stream {
		h.sendStream(c, in)
		return
	}

	result, err := h.chat.Send(c.Request.Context(), in)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeChatSessionNotFound)
		return
	}
	c.JSON(http.StatusOK, chatSendResponse(result))
}

// chatSendResponse 发送成功的响应体。非流式直接回它；流式把它装进 done 帧——
// **两边必须是同一个结构**，前端「done 后用服务端定稿替换」全靠这一点。
func chatSendResponse(result *ChatSendResult) gin.H {
	return gin.H{
		"session_uid":       result.Session.UID,
		"title":             result.Session.Title,
		"user_message":      newChatMessageDTO(result.UserMessage),
		"assistant_message": newChatMessageDTO(result.AssistantMessage),
	}
}

// sendStream 流式发送：SSE 三种帧（delta / done / error），格式见文件头。
//
// 几条只能在这一层守的事：
//
//   - **响应头延迟到第一帧前才写**：流开始之前失败还能回普通 JSON 错误
//     （状态码还改得了）；一旦 200 + SSE 写出去了，错误只能装进 error 帧。
//   - X-Accel-Buffering: no 和 Cache-Control: no-cache 必须有——中间的反代
//     （nginx / 群晖反向代理）默认会把响应攒成一整块，打字机就变成一次全出。
//   - 客户端断开只是「不看了」：写帧失败后停止再写，但 service 的调用继续走完
//     ——钱已经在花（上游调用是故意跟连接脱钩的，决策 15），流完照常落库，
//     运营刷新页面还能看到这条回复。
func (h *ChatHandler) sendStream(c *gin.Context, in ChatSendInput) {
	started := false
	disconnected := false

	writeFrame := func(event string, payload any) {
		if disconnected {
			return
		}
		data, err := json.Marshal(payload)
		if err != nil {
			// payload 全是字符串字段，走不到；真走到也不能写半截 JSON。
			return
		}
		if !started {
			started = true
			header := c.Writer.Header()
			header.Set("Content-Type", "text/event-stream")
			header.Set("Cache-Control", "no-cache")
			header.Set("Connection", "keep-alive")
			header.Set("X-Accel-Buffering", "no")
			c.Writer.WriteHeader(http.StatusOK)
		}
		if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, data); err != nil {
			disconnected = true
			return
		}
		c.Writer.Flush()
	}

	result, err := h.chat.SendStream(c.Request.Context(), in, func(text string) {
		writeFrame("delta", gin.H{"text": text})
	})
	if err != nil {
		dkErr := serviceErrorToDesignkit(err, dkdomain.ErrCodeChatSessionNotFound)
		if !started {
			// 一帧都没发出去：普通 JSON 错误，状态码和信封由既有链路处理。
			abortWithDesignkitError(c, dkErr)
			return
		}
		// 流已经开了，状态码定死在 200：错误装进 error 帧后关流。
		// 日志照 abortWithDesignkitError 的口径记，别让流式错误从日志里消失。
		slog.Warn("designkit 对话流式发送失败",
			slog.String("error_code", dkErr.Code),
			slog.Any("cause", dkErr.Cause))
		writeFrame("error", newErrorEnvelope(dkErr, requestIDOf(c)))
		return
	}
	writeFrame("done", chatSendResponse(result))
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
