//go:build unit

package handler

// chat_stream_handler_test.go —— POST /chat/messages 流式分支的单测。
//
// 帧格式（event: delta / done / error）是对外契约（chat_handler.go 文件头），
// 这里逐帧断言，改坏了前端的打字机和 ERP 的解析都会静默失效。

import (
	"context"
	"net/http"
	"strings"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 假的对话服务
// ---------------------------------------------------------------------------

type fakeChatConversationService struct {
	deltas []string
	result *ChatSendResult
	err    error

	lastInput  ChatSendInput
	streamCall int
	sendCall   int
}

func (f *fakeChatConversationService) Send(_ context.Context, in ChatSendInput) (*ChatSendResult, error) {
	f.sendCall++
	f.lastInput = in
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeChatConversationService) SendStream(_ context.Context, in ChatSendInput, onDelta func(text string)) (*ChatSendResult, error) {
	f.streamCall++
	f.lastInput = in
	for _, d := range f.deltas {
		if onDelta != nil {
			onDelta(d)
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeChatConversationService) ListSessions(context.Context, int64) ([]*dkdomain.ChatSession, error) {
	return nil, nil
}

func (f *fakeChatConversationService) GetSession(context.Context, int64, string) (*dkdomain.ChatSession, []*dkdomain.ChatMessage, error) {
	return nil, nil, dkdomain.ErrNotFound
}

func (f *fakeChatConversationService) DeleteSession(context.Context, int64, string) error {
	return nil
}

func testChatSendResult() *ChatSendResult {
	return &ChatSendResult{
		Session: &dkdomain.ChatSession{
			ID: 1, UID: "01SESSIONULID00000000000000", UserID: testUserID,
			Title: "问个问题", CreatedAt: testTime(), UpdatedAt: testTime(),
		},
		UserMessage: &dkdomain.ChatMessage{
			ID: 10, SessionID: 1, Role: "user", Content: "问个问题",
			AssetUIDs: []string{"01ASSETULID0000000000000000"}, CreatedAt: testTime(),
		},
		AssistantMessage: &dkdomain.ChatMessage{
			ID: 11, SessionID: 1, Role: "assistant", Content: "这是回答", CreatedAt: testTime(),
		},
	}
}

func testServicesWithChat(chat ChatConversationService) Services {
	svcs := testServices(&fakeJobService{})
	svcs.Chat = chat
	return svcs
}

// sseEvents 把 SSE 响应体解成 [事件名, data 原文] 序列。
// 只认 "event:" / "data:" 两种行——多出别的行说明帧格式被改了。
func sseEvents(t *testing.T, body string) [][2]string {
	t.Helper()
	var events [][2]string
	current := ""
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "event: "):
			current = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			require.NotEmpty(t, current, "data 行前面必须有 event 行：%q", line)
			events = append(events, [2]string{current, strings.TrimPrefix(line, "data: ")})
			current = ""
		default:
			t.Fatalf("SSE 里出现认不出的行：%q", line)
		}
	}
	return events
}

// ---------------------------------------------------------------------------
// 三种帧
// ---------------------------------------------------------------------------

// 成功：若干 delta 帧按顺序在前，最后一帧是 done，data 与非流式响应同构。
func TestChatSendStream_DeltaThenDone(t *testing.T) {
	chat := &fakeChatConversationService{
		deltas: []string{"这是", "回答"},
		result: testChatSendResult(),
	}
	engine := newTestEngine(t, testServicesWithChat(chat), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/chat/messages",
		`{"session_uid":"","text":"问个问题","asset_uids":[],"stream":true}`, nil)

	require.Equal(t, http.StatusOK, rec.Code, "响应体：%s", rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
	// 反代把流攒成一整块 = 打字机失效，这两个头必须在。
	assert.Equal(t, "no", rec.Header().Get("X-Accel-Buffering"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))

	events := sseEvents(t, rec.Body.String())
	require.Len(t, events, 3)

	assert.Equal(t, "delta", events[0][0])
	assert.JSONEq(t, `{"text":"这是"}`, events[0][1])
	assert.Equal(t, "delta", events[1][0])
	assert.JSONEq(t, `{"text":"回答"}`, events[1][1])

	assert.Equal(t, "done", events[2][0])
	done := decodeJSON(t, []byte(events[2][1]))
	// done 帧 = 与非流式相同结构的完整响应（前端用它替换打字机内容）。
	assert.Equal(t, "01SESSIONULID00000000000000", done["session_uid"])
	assert.Equal(t, "问个问题", done["title"])
	assistant, ok := done["assistant_message"].(map[string]any)
	require.True(t, ok, "done 帧里要有 assistant_message 对象：%s", events[2][1])
	assert.Equal(t, "这是回答", assistant["content"])
	user, ok := done["user_message"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"01ASSETULID0000000000000000"}, user["asset_uids"])
	assertNoCodeKey(t, done)

	assert.Equal(t, 1, chat.streamCall)
	assert.Equal(t, 0, chat.sendCall, "stream:true 不该走非流式")
}

// 流开了之后失败：错误装进 error 帧（标准错误信封），状态码保持 200。
func TestChatSendStream_ErrorAfterDeltas(t *testing.T) {
	chat := &fakeChatConversationService{
		deltas: []string{"前半"},
		err:    dkdomain.NewError(dkdomain.ErrCodeUpstreamError),
	}
	engine := newTestEngine(t, testServicesWithChat(chat), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/chat/messages",
		`{"session_uid":"","text":"问个问题","asset_uids":[],"stream":true}`, nil)

	require.Equal(t, http.StatusOK, rec.Code, "流已经开了，状态码改不了，只能是 200")
	events := sseEvents(t, rec.Body.String())
	require.Len(t, events, 2)
	assert.Equal(t, "delta", events[0][0])

	assert.Equal(t, "error", events[1][0])
	envelope := decodeJSON(t, []byte(events[1][1]))
	errObj, ok := envelope["error"].(map[string]any)
	require.True(t, ok, "error 帧的 data 必须是标准错误信封：%s", events[1][1])
	assert.Equal(t, dkdomain.ErrCodeUpstreamError, errObj["error_code"])
	assert.NotEmpty(t, errObj["message"], "运营要看中文文案")
	assertNoCodeKey(t, envelope)
}

// 一帧都没发出去就失败：走普通 JSON 错误（状态码 + 信封），不硬凑 SSE。
func TestChatSendStream_ErrorBeforeFirstDelta(t *testing.T) {
	chat := &fakeChatConversationService{
		err: dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).WithMessage("消息不能为空。"),
	}
	engine := newTestEngine(t, testServicesWithChat(chat), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/chat/messages",
		`{"session_uid":"","text":"","asset_uids":[],"stream":true}`, nil)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.NotContains(t, rec.Header().Get("Content-Type"), "text/event-stream")
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
}

// 不带 stream（或 false）：老的一次性 JSON 响应，一个字节都不变。
func TestChatSendWithoutStreamUnchanged(t *testing.T) {
	chat := &fakeChatConversationService{result: testChatSendResult()}
	engine := newTestEngine(t, testServicesWithChat(chat), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/chat/messages",
		`{"session_uid":"","text":"问个问题","asset_uids":[]}`, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	body := decodeJSON(t, rec.Body.Bytes())
	assert.Equal(t, "01SESSIONULID00000000000000", body["session_uid"])
	assert.Equal(t, 1, chat.sendCall)
	assert.Equal(t, 0, chat.streamCall, "stream 缺省必须走非流式")
}
