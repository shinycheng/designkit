//go:build unit

package service

// chat_invoke_test.go —— 对话调用器的单测。
//
// 这些测试**绝不真的调网关**（CLAUDE.md 第三节：自动化测试不许烧钱）。
// 假的 ChatHandler 直接往 recorder 里写预置 JSON。
//
// 重点守三件事，都是「坏了不会当场报错、只会静默出事」的那种：
//   1. 计费 request_id 加上 client: 前缀之后不能超过 usage_logs 的 64 —— 超了会
//      让上游写账单的 INSERT 抛 22001，而扣费在前，结果是钱扣了账单没有且无告警；
//   2. 图片确实被编码进了 content 数组 —— 漏了不会报错，模型只是看不见图，
//      答得驴唇不对马嘴，极难往「请求体形状」上想；
//   3. 上游报错要变成中文业务错误，不能把裸 JSON 抛给运营。

import (
	"context"
	"encoding/json"
	"errors"
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

// chatFakeHandler 冒充上游的 OpenAIGatewayHandler。
type chatFakeHandler struct {
	// status / body 这次要回什么。
	status int
	body   string

	// 下面几个是抓下来给断言用的。
	calls       int
	lastBody    []byte
	lastCtxUser any
	lastAPIKey  any
}

func (f *chatFakeHandler) ChatCompletions(c *gin.Context) {
	f.calls++
	if c.Request != nil && c.Request.Body != nil {
		buf := make([]byte, c.Request.ContentLength)
		_, _ = c.Request.Body.Read(buf)
		f.lastBody = buf
	}
	// apiKeyAuth 平时写进去的那几个键，缺了上游会 401 —— 抓下来断言我们补齐了。
	f.lastAPIKey, _ = c.Get("api_key")
	f.lastCtxUser, _ = c.Get("user")

	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	c.Data(status, "application/json", []byte(f.body))
}

// chatFakeKeys 冒充 InternalKeyService（只要 GetByID）。
type chatFakeKeys struct {
	key *upstreamservice.APIKey
	err error
}

func (f chatFakeKeys) GetByID(_ context.Context, _ int64) (*upstreamservice.APIKey, error) {
	return f.key, f.err
}

// chatOKBody 一段正常的 chat/completions 响应。
const chatOKBody = `{"choices":[{"message":{"role":"assistant","content":"  这是回答  "}}]}`

// newChatFixture 造一套能跑通的假件。
func newChatFixture(status int, body string) (*GatewayChatInvoker, *chatFakeHandler) {
	handler := &chatFakeHandler{status: status, body: body}
	keys := chatFakeKeys{key: &upstreamservice.APIKey{
		ID:     7,
		UserID: 42,
		Status: upstreamservice.StatusActive,
		User: &upstreamservice.User{
			ID: 42, Role: "user", Concurrency: 5, Status: upstreamservice.StatusActive,
		},
	}}
	return NewChatInvoker(handler, keys), handler
}

func baseChatRequest() ChatRequest {
	return ChatRequest{
		UserID:    42,
		APIKeyID:  7,
		UserText:  "看看这张图",
		RequestID: BuildChatBillingRequestID("01KZZMMSB6P8PD4BVS4YHP1G84", 1),
	}
}

// ---------------------------------------------------------------------------
// 正常路径
// ---------------------------------------------------------------------------

func TestChat_ReturnsTrimmedText(t *testing.T) {
	invoker, handler := newChatFixture(http.StatusOK, chatOKBody)

	res, err := invoker.Chat(context.Background(), baseChatRequest())
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	// 前后空白必须去掉：模型很爱在正文前后带空行，
	// 这段字会被原样存进 job item 再发给出图模型。
	if res.Text != "这是回答" {
		t.Fatalf("正文 = %q，应为 %q", res.Text, "这是回答")
	}
	if handler.calls != 1 {
		t.Fatalf("上游被调了 %d 次，应该是 1 次", handler.calls)
	}
}

// 缺了这三个上下文键，上游 ChatCompletions 直接 401。
func TestChat_FillsAuthContextKeys(t *testing.T) {
	invoker, handler := newChatFixture(http.StatusOK, chatOKBody)

	if _, err := invoker.Chat(context.Background(), baseChatRequest()); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if handler.lastAPIKey == nil {
		t.Fatal("合成的上下文里没有 api_key，上游会直接 401")
	}
	if handler.lastCtxUser == nil {
		t.Fatal("合成的上下文里没有 user，上游计费准入会拿不到主体")
	}
}

// ---------------------------------------------------------------------------
// 图片
// ---------------------------------------------------------------------------

// 图片必须真的进 content 数组。漏了不会报错，只是模型看不见图。
func TestChat_EncodesImagesIntoContent(t *testing.T) {
	invoker, handler := newChatFixture(http.StatusOK, chatOKBody)

	req := baseChatRequest()
	req.Images = []ChatImage{
		{Data: []byte{0x89, 'P', 'N', 'G'}, ContentType: "image/png"},
		{Data: []byte{0xFF, 0xD8, 0xFF}, ContentType: "image/jpeg"},
	}
	if _, err := invoker.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}

	var payload struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL *struct {
					URL string `json:"url"`
				} `json:"image_url"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(handler.lastBody, &payload); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v\n%s", err, handler.lastBody)
	}
	if payload.Model != DefaultChatModel {
		t.Fatalf("模型 = %q，应为 %q（分组里只有这一个对话模型）", payload.Model, DefaultChatModel)
	}
	if len(payload.Messages) != 1 {
		t.Fatalf("消息条数 = %d，没写 System 时应该只有 1 条", len(payload.Messages))
	}

	parts := payload.Messages[0].Content
	if len(parts) != 3 {
		t.Fatalf("content 项数 = %d，应为 1 段文字 + 2 张图", len(parts))
	}
	// 顺序刻意是「先文字后图片」：文字是「看着这张图做什么」，放前面才读得通。
	if parts[0].Type != "text" || parts[0].Text != "看看这张图" {
		t.Fatalf("第 1 项应该是文字，实际: %+v", parts[0])
	}
	for i, want := range []string{"data:image/png;base64,", "data:image/jpeg;base64,"} {
		part := parts[i+1]
		if part.Type != "image_url" || part.ImageURL == nil {
			t.Fatalf("第 %d 项应该是图片，实际: %+v", i+2, part)
		}
		if !strings.HasPrefix(part.ImageURL.URL, want) {
			t.Fatalf("第 %d 张图的 data URL 前缀 = %q，应以 %q 开头",
				i+1, part.ImageURL.URL, want)
		}
	}
}

// 超大图直接拦下并给中文提示，不要发出去等上游报一句英文。
func TestChat_RejectsOversizedImage(t *testing.T) {
	invoker, handler := newChatFixture(http.StatusOK, chatOKBody)

	req := baseChatRequest()
	req.Images = []ChatImage{{Data: make([]byte, chatMaxImageBytes+1), ContentType: "image/png"}}

	_, err := invoker.Chat(context.Background(), req)
	if err == nil {
		t.Fatal("超大图应该被拦下")
	}
	if handler.calls != 0 {
		t.Fatal("超大图不该真的发给上游")
	}
	var dkErr *dkdomain.DesignkitError
	if !errors.As(err, &dkErr) || dkErr.Message == "" {
		t.Fatalf("应该是带中文提示的业务错误，实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 计费 id 的长度（钱的事故，最要紧的一条）
// ---------------------------------------------------------------------------

// 落 usage_logs 的完整值必须 ≤64。超了会让上游写账单失败，
// 而扣费在前 —— 钱扣了、账单没有、没有任何告警。
func TestChatBillingRequestID_FitsUsageLogColumn(t *testing.T) {
	// 26 位 ULID 是真实场景里最长的 scope。
	for step := 0; step <= 99; step++ {
		id := BuildChatBillingRequestID("01KZZMMSB6P8PD4BVS4YHP1G84", step)
		stored := dkdomain.UpstreamClientRequestIDPrefix + id
		if len(stored) > dkdomain.UsageLogRequestIDMaxLen {
			t.Fatalf("step=%d 时落库值超长（%d > %d）：%q",
				step, len(stored), dkdomain.UsageLogRequestIDMaxLen, stored)
		}
	}
}

// 调用方硬塞一个超长 id 时必须当场失败，不能发出去。
func TestChat_RejectsOverlongRequestID(t *testing.T) {
	invoker, handler := newChatFixture(http.StatusOK, chatOKBody)

	req := baseChatRequest()
	req.RequestID = "dks:" + strings.Repeat("x", dkdomain.UsageLogRequestIDMaxLen)

	if _, err := invoker.Chat(context.Background(), req); err == nil {
		t.Fatal("超长的 request_id 应该被拦下")
	}
	if handler.calls != 0 {
		t.Fatal("超长的 request_id 不该真的发给上游（发了就是钱扣了账单没有）")
	}
}

// ---------------------------------------------------------------------------
// 失败路径
// ---------------------------------------------------------------------------

func TestChat_UpstreamErrorStatus(t *testing.T) {
	invoker, _ := newChatFixture(http.StatusBadGateway,
		`{"error":{"message":"upstream exploded","type":"server_error"}}`)

	_, err := invoker.Chat(context.Background(), baseChatRequest())
	if err == nil {
		t.Fatal("上游 502 应该报错")
	}
	var dkErr *dkdomain.DesignkitError
	if !errors.As(err, &dkErr) || dkErr.Code != dkdomain.ErrCodeUpstreamError {
		t.Fatalf("错误码应为 %s，实际: %v", dkdomain.ErrCodeUpstreamError, err)
	}
}

// 200 但 body 里带 error —— 上游会把网关的错误体原样透传，这种真实存在。
func TestChat_UpstreamErrorInsideOKBody(t *testing.T) {
	invoker, _ := newChatFixture(http.StatusOK,
		`{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`)

	if _, err := invoker.Chat(context.Background(), baseChatRequest()); err == nil {
		t.Fatal("200 但 body 带 error 时也应该报错，不能当成成功")
	}
}

// 200 但没有正文：当成失败，否则会把空字符串当提示词发去出图。
func TestChat_EmptyContentIsFailure(t *testing.T) {
	invoker, _ := newChatFixture(http.StatusOK, `{"choices":[{"message":{"content":"   "}}]}`)

	if _, err := invoker.Chat(context.Background(), baseChatRequest()); err == nil {
		t.Fatal("正文是空白时应该报错")
	}
}

// Key 不属于这个用户 = 会把钱记到别人头上，必须拒。
func TestChat_RejectsKeyOwnedByAnotherUser(t *testing.T) {
	handler := &chatFakeHandler{status: http.StatusOK, body: chatOKBody}
	keys := chatFakeKeys{key: &upstreamservice.APIKey{
		ID: 7, UserID: 999, Status: upstreamservice.StatusActive,
		User: &upstreamservice.User{ID: 999, Status: upstreamservice.StatusActive},
	}}
	invoker := NewChatInvoker(handler, keys)

	_, err := invoker.Chat(context.Background(), baseChatRequest())
	if err == nil {
		t.Fatal("Key 归属不符时应该拒绝")
	}
	if handler.calls != 0 {
		t.Fatal("归属不符时不该真的发出去（发了就是把钱记到别人头上）")
	}
}
