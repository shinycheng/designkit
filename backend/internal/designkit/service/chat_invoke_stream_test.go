//go:build unit

package service

// chat_invoke_stream_test.go —— 流式对话调用器的单测。
//
// 跟 chat_invoke_test.go 同一套立场：**绝不真的调网关**（CLAUDE.md 第三节），
// 假的 ChatHandler 往 c.Writer 上按上游的真实写法吐 SSE（设头 → WriteHeader →
// 逐帧 Write + Flush）。重点守的都是「坏了不当场报错、只会静默出事」的：
//
//  1. delta 按到达顺序回调、全文与各段拼起来一致——顺序错了打字机会乱跳字；
//  2. SSE 帧被拆在多次 Write 里也要解析对——真实网络里这是常态不是异常；
//  3. 上游中途的 error 帧和流前的非 200 都要变成中文 DK_ 错误，不能把裸 JSON 抛给运营；
//  4. 计费 request_id 的长度守卫对流式同样生效（超长 = 钱扣了账单没有）。

import (
	"context"
	"net/http"
	"strings"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// 假件
// ---------------------------------------------------------------------------

// chatStreamFakeHandler 冒充上游：按 writes 里的分片逐次 Write（每次后 Flush），
// 模拟 SSE 帧被拆开到达。status != 200 时按非流式错误响应写 JSON。
type chatStreamFakeHandler struct {
	status      int
	contentType string
	writes      []string

	calls    int
	lastBody []byte
}

func (f *chatStreamFakeHandler) ChatCompletions(c *gin.Context) {
	f.calls++
	if c.Request != nil && c.Request.Body != nil {
		buf := make([]byte, c.Request.ContentLength)
		_, _ = c.Request.Body.Read(buf)
		f.lastBody = buf
	}

	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	contentType := f.contentType
	if contentType == "" {
		contentType = "text/event-stream"
	}

	// 照上游 newStreamHeaderWriter 的顺序：先设头，再 WriteHeader，再逐帧写。
	c.Writer.Header().Set("Content-Type", contentType)
	c.Writer.WriteHeader(status)
	for _, chunk := range f.writes {
		_, _ = c.Writer.WriteString(chunk)
		c.Writer.Flush()
	}
}

func newChatStreamFixture(h *chatStreamFakeHandler) *GatewayChatInvoker {
	keys := chatFakeKeys{key: &upstreamservice.APIKey{
		ID:     7,
		UserID: 42,
		Status: upstreamservice.StatusActive,
		User: &upstreamservice.User{
			ID: 42, Role: "user", Concurrency: 5, Status: upstreamservice.StatusActive,
		},
	}}
	return NewChatInvoker(h, keys)
}

func sseDelta(text string) string {
	return `data: {"choices":[{"delta":{"content":"` + text + `"}}]}` + "\n\n"
}

// ---------------------------------------------------------------------------
// 正常路径
// ---------------------------------------------------------------------------

// delta 序列按到达顺序回调，全文 = 各段拼接（再 TrimSpace）。
func TestStreamChat_DeltaSequenceAndFullText(t *testing.T) {
	handler := &chatStreamFakeHandler{writes: []string{
		// role 帧：没有 content，不该触发回调。
		`data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n",
		sseDelta("你"),
		sseDelta("好"),
		sseDelta("！"),
		"data: [DONE]\n\n",
	}}
	invoker := newChatStreamFixture(handler)

	var got []string
	res, err := invoker.StreamChat(context.Background(), baseChatRequest(), func(text string) {
		got = append(got, text)
	})
	if err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	if strings.Join(got, "|") != "你|好|！" {
		t.Fatalf("delta 序列 = %v，应为 [你 好 ！]", got)
	}
	if res.Text != "你好！" {
		t.Fatalf("全文 = %q，应为 %q", res.Text, "你好！")
	}
	if handler.calls != 1 {
		t.Fatalf("上游被调了 %d 次，应该是 1 次", handler.calls)
	}
}

// 请求体里必须带 stream:true——漏了上游走非流式分支，打字机整个失效且不报错。
func TestStreamChat_RequestBodyCarriesStreamFlag(t *testing.T) {
	handler := &chatStreamFakeHandler{writes: []string{sseDelta("好"), "data: [DONE]\n\n"}}
	invoker := newChatStreamFixture(handler)

	if _, err := invoker.StreamChat(context.Background(), baseChatRequest(), nil); err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	if !strings.Contains(string(handler.lastBody), `"stream":true`) {
		t.Fatalf("请求体没带 stream:true：%s", handler.lastBody)
	}
}

// SSE 帧被拆在多次 Write 里（连一行 JSON 都被剖成两半）也要解析对。
func TestStreamChat_FramesSplitAcrossWrites(t *testing.T) {
	handler := &chatStreamFakeHandler{writes: []string{
		`data: {"choices":[{"delta":{"content":"第一"}}]}` + "\n\n" + `data: {"choi`,
		`ces":[{"delta":{"content":"第二"}}]}` + "\n\n",
		"data: [DONE]",
	}}
	invoker := newChatStreamFixture(handler)

	var got []string
	res, err := invoker.StreamChat(context.Background(), baseChatRequest(), func(text string) {
		got = append(got, text)
	})
	if err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	if strings.Join(got, "|") != "第一|第二" {
		t.Fatalf("delta 序列 = %v，应为 [第一 第二]", got)
	}
	if res.Text != "第一第二" {
		t.Fatalf("全文 = %q，应为 %q", res.Text, "第一第二")
	}
}

// 上游没走流式（200 + application/json 整包）：回落成非流式解析，
// 全文一次性作为唯一一段回调出去——调用方不需要第二套处理。
func TestStreamChat_FallsBackToPlainJSON(t *testing.T) {
	handler := &chatStreamFakeHandler{
		contentType: "application/json",
		writes:      []string{chatOKBody},
	}
	invoker := newChatStreamFixture(handler)

	var got []string
	res, err := invoker.StreamChat(context.Background(), baseChatRequest(), func(text string) {
		got = append(got, text)
	})
	if err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	if res.Text != "这是回答" {
		t.Fatalf("全文 = %q，应为 %q", res.Text, "这是回答")
	}
	if len(got) != 1 || got[0] != "这是回答" {
		t.Fatalf("回落时应把全文作为唯一一段回调，实际 = %v", got)
	}
}

// ---------------------------------------------------------------------------
// 出错路径：一律中文 DK_ 错误
// ---------------------------------------------------------------------------

func assertUpstreamDKError(t *testing.T, err error) *dkdomain.DesignkitError {
	t.Helper()
	if err == nil {
		t.Fatal("应该报错，实际成功了")
	}
	dkErr, ok := dkdomain.AsDesignkitError(err)
	if !ok {
		t.Fatalf("错误不是 *DesignkitError：%v", err)
	}
	if dkErr.Code != dkdomain.ErrCodeUpstreamError {
		t.Fatalf("错误码 = %s，应为 %s", dkErr.Code, dkdomain.ErrCodeUpstreamError)
	}
	// 给运营看的必须是中文，不能把上游的英文 JSON 原样抛出去。
	if !strings.ContainsFunc(dkErr.Message, func(r rune) bool { return r >= 0x4e00 && r <= 0x9fff }) {
		t.Fatalf("错误文案不是中文：%q", dkErr.Message)
	}
	return dkErr
}

// 流中途的 error 帧 → 中文 DK_UPSTREAM_ERROR，且已收到的 delta 不算成功。
func TestStreamChat_ErrorEventMidStream(t *testing.T) {
	handler := &chatStreamFakeHandler{writes: []string{
		sseDelta("前半"),
		`data: {"error":{"type":"server_error","message":"upstream exploded"}}` + "\n\n",
		"data: [DONE]\n\n",
	}}
	invoker := newChatStreamFixture(handler)

	var got []string
	_, err := invoker.StreamChat(context.Background(), baseChatRequest(), func(text string) {
		got = append(got, text)
	})
	dkErr := assertUpstreamDKError(t, err)
	// 上游原文只进日志和 Cause，绝不进 Message。
	if strings.Contains(dkErr.Message, "exploded") {
		t.Fatalf("上游英文原文漏进了给运营看的文案：%q", dkErr.Message)
	}
	if len(got) != 1 || got[0] != "前半" {
		t.Fatalf("error 帧之前的 delta 仍应回调出去（前端要留着显示），实际 = %v", got)
	}
}

// 流开始之前上游就 4xx/5xx（普通 JSON 错误响应）→ 中文 DK_UPSTREAM_ERROR。
func TestStreamChat_Non200BeforeStream(t *testing.T) {
	handler := &chatStreamFakeHandler{
		status:      http.StatusBadGateway,
		contentType: "application/json",
		writes:      []string{`{"error":{"type":"upstream_error","message":"bad gateway"}}`},
	}
	invoker := newChatStreamFixture(handler)

	_, err := invoker.StreamChat(context.Background(), baseChatRequest(), nil)
	assertUpstreamDKError(t, err)
}

// 流走完但一个字都没有 → 报错，不能把空串当回答落库。
func TestStreamChat_EmptyStreamIsError(t *testing.T) {
	handler := &chatStreamFakeHandler{writes: []string{"data: [DONE]\n\n"}}
	invoker := newChatStreamFixture(handler)

	_, err := invoker.StreamChat(context.Background(), baseChatRequest(), nil)
	assertUpstreamDKError(t, err)
}

// ---------------------------------------------------------------------------
// 计费纪律
// ---------------------------------------------------------------------------

// 长度守卫对流式同样生效：超长 request_id 必须在调上游**之前**拦下。
// 放过去的后果是钱扣了账单没有，且没有任何告警（CLAUDE.md B1）。
func TestStreamChat_RequestIDLengthGuard(t *testing.T) {
	handler := &chatStreamFakeHandler{writes: []string{sseDelta("x"), "data: [DONE]\n\n"}}
	invoker := newChatStreamFixture(handler)

	req := baseChatRequest()
	req.RequestID = strings.Repeat("a", dkdomain.UsageLogRequestIDMaxLen)

	_, err := invoker.StreamChat(context.Background(), req, nil)
	if err == nil {
		t.Fatal("超长 request_id 应该被拦下")
	}
	if handler.calls != 0 {
		t.Fatalf("超长 request_id 不该打到上游（打了 %d 次）——打了就已经扣钱了", handler.calls)
	}
}
