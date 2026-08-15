package service

// chat_invoke.go —— 让 designkit 能用**同一个 ChatGPT 账号**做「读图 + 出文字」。
//
// 为什么需要它：「AI 挑提示词」那套流程要连发三趟对话——判分类、从标题里挑 5 条、
// 把 5 条合成 1 条。这三趟都要把运营上传的商品图带上，让模型真的看见图。
//
// # 它跟 gateway_invoke.go 是同一个模式
//
// 出图那条走的是「合成一个 gin.Context，重新进入上游 h.OpenAIGateway.Images()」。
// 对话这条一模一样，只是换成 ChatCompletions()、body 从 multipart 换成 JSON。
// **合成上下文的每一步都是照抄的**，因为那几步不是风格问题，是「少一步就出事」：
// 少了 WithoutCancel，运营关网页就把在跑的对话掐断；少了 c.Copy()，并发会串包；
// 少了那三个上下文键，上游直接 401。逐条理由见 newSyntheticChatContext 里的注释。
//
// # 实测确认过的两件事（2026-08-14）
//
//	对话模型是 gpt-5.6-sol（这个分组里只有它和 gpt-image-2 两个）。
//	通过网关发 image_url（data URL）读图**有效**，实测正确答出了测试图的内容。
//	一次读图对话约 $0.0008——相对出图 $1/张 可以忽略，但**不是不要钱**，
//	所以照样走计费、照样要有自己的 request_id。
//
// # ⚠ 计费 id 必须自己守长度
//
// 出图那条在 resolveGatewayBillingRequestID 里守了，**那条守卫不覆盖这里**。
// 对话同样会写 usage_logs，而 usage_logs.request_id 是 VARCHAR(64)，
// 上游还会自己拼 "client:" 前缀（7 字节）。超了的后果不是截断，是 PostgreSQL 抛 22001
// 让整条 INSERT 失败——而扣费和写账单是两个独立事务、扣费在前，
// 结果是**钱扣了、账单没有，且没有任何告警**（CLAUDE.md B1）。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"
)

// ChatHandler 是上游 *handler.OpenAIGatewayHandler 的最小切面。
//
// 只声明这一个方法、不 import internal/handler：那个包已经 import 了本包的上层，
// 反过来引就是循环依赖，编译不过。跟 ImagesHandler 同一个道理。
//
// 对应上游 backend/internal/handler/openai_chat_completions.go:23。
type ChatHandler interface {
	ChatCompletions(c *gin.Context)
}

const (
	// DefaultChatModel 默认对话模型。
	//
	// ⚠ 这个值必须真的在运营那把 Key 所属的分组里可用，否则上游返
	// "Model ... is not supported by any configured account in this group"。
	// 2026-08-14 实测：monica 的 chatgpt 分组里只有 gpt-5.6-sol 和 gpt-image-2 两个。
	// 哪天换模型改这里；调用方也可以在 ChatRequest.Model 里单独指定。
	DefaultChatModel = "gpt-5.6-sol"

	// chatCompletionsURL 合成请求的 URL。
	//
	// ⚠ 路径里必须含 /chat/completions 字面量：上游若干处是按路径判端点的。
	// 域名部分随便写，反正这个请求永远不会真的出网——它直接进 ChatCompletions()。
	chatCompletionsURL = "http://designkit.internal/v1/chat/completions"

	// chatBillingRequestIDPrefix 对话的计费 id 前缀。
	//
	// 跟出图的 dki: 分开，是为了「我的消费」那几个统计能把两者拆开看：
	// 出图按 client:dki:% 筛，对话按 client:dks:% 筛。
	// 混在一起的话，运营看到的「本月花费」里会掺进推荐的零头，
	// 而「本月出图 N 张」又不含它们，两个数对不上还查不出原因。
	chatBillingRequestIDPrefix = "dks:"

	// chatDefaultTimeout 单趟对话的超时。
	//
	// 三趟串起来最坏 3×，而前端把这个接口的超时放宽到了 120 秒
	// （api/inspiration.ts，绕过 axios 单例默认的 30 秒）。
	// 所以单趟必须明显小于 40 秒，否则前端先报超时、后端还在算还在花钱，
	// 运营看到「超时」却照样被扣钱。
	chatDefaultTimeout = 35 * time.Second

	// chatMaxImageBytes 单张图进对话的上限。
	//
	// base64 之后体积涨约 1/3，再加上 JSON 转义。商品图上传本身限 20MB，
	// 但那是给出图用的；对话只是让模型「看一眼」，2MB 足够看清，
	// 再大只是白烧 token（图片按像素计 token，跟字节数不完全相关，但传输和内存是实打实的）。
	chatMaxImageBytes = 2 << 20
)

// ChatImage 一张要给模型看的图。
type ChatImage struct {
	// Data 原始字节。
	Data []byte
	// ContentType MIME 类型；空串时按 image/png 处理。
	//
	// ⚠ 这个值会原样进 data URL 的头部。填错（比如 webp 标成 png）不一定报错，
	// 但模型可能解不开图，表现为「它答得驴唇不对马嘴」而不是「报错」。
	ContentType string
}

// ChatRequest 一趟对话。
type ChatRequest struct {
	// UserID 归属人。用来校验 APIKey 是不是他的——不校验就是把钱记到别人头上。
	UserID int64
	// APIKeyID 用哪把 Key。**必填**：本包不替调用方去找内部 Key，
	// 那是 InternalKeyService 的职责，调用方拿到之后传进来。
	APIKeyID int64
	// Model 空 = DefaultChatModel。
	Model string
	// System 系统提示词，可空。
	System string
	// UserText 用户消息正文。
	UserText string
	// Images 要让模型看的图，可空。
	Images []ChatImage
	// RequestID 计费 id（**不带** client: 前缀，上游自己会拼）。
	// 空串时由 BuildChatBillingRequestID 现算一个。
	RequestID string
	// Timeout 单趟超时；<=0 用 chatDefaultTimeout。
	Timeout time.Duration
}

// ChatResult 一趟对话的结果。
type ChatResult struct {
	// Text 模型回的正文（已 TrimSpace）。
	Text string
}

// ChatInvoker 是「发一趟对话、拿回文字」这一个能力。
//
// 抽成接口是给 prompt_suggest.go 的单测用的：那边要断言三步法的编排逻辑，
// 不能真的去调网关烧钱（CLAUDE.md 第三节）。
type ChatInvoker interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResult, error)
}

// BuildChatBillingRequestID 拼一个对话用的计费 id。
//
// 格式 `dks:<scope>:<step 两位>`，例如 `dks:01KZZMNB43ZJWPY3CNMW5ZTPCH:01`。
// scope 一般传商品图的 uid（26 位 ULID），这样一次推荐的三趟能在账单里串起来。
//
// 长度：4 + 26 + 1 + 2 = 33，加上上游拼的 "client:"（7）= 40，安全落在 64 以内。
// **调用方不需要自己算长度**，Chat() 每次都会再断言一遍。
func BuildChatBillingRequestID(scope string, step int) string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = "anon"
	}
	if step < 0 {
		step = 0
	}
	if step > 99 {
		step = 99
	}
	return fmt.Sprintf("%s%s:%02d", chatBillingRequestIDPrefix, scope, step)
}

// GatewayChatInvoker 是 ChatInvoker 的真实实现：进程内重入上游 ChatCompletions()。
type GatewayChatInvoker struct {
	chat ChatHandler
	keys APIKeyLoader
	log  *slog.Logger
}

// ChatInvokerOption 可选项。
type ChatInvokerOption func(*GatewayChatInvoker)

// WithChatInvokerLogger 换一个 logger（测试里用）。
func WithChatInvokerLogger(l *slog.Logger) ChatInvokerOption {
	return func(g *GatewayChatInvoker) {
		if l != nil {
			g.log = l
		}
	}
}

var _ ChatInvoker = (*GatewayChatInvoker)(nil)

// NewChatInvoker 造一个对话调用器。
//
// keys 复用 gateway_invoke.go 里已有的 APIKeyLoader（InternalKeyService 就实现了它），
// 不要重新声明一个同名接口——同包重复声明编译不过。
func NewChatInvoker(chat ChatHandler, keys APIKeyLoader, opts ...ChatInvokerOption) *GatewayChatInvoker {
	g := &GatewayChatInvoker{chat: chat, keys: keys}
	for _, opt := range opts {
		if opt != nil {
			opt(g)
		}
	}
	return g
}

func (g *GatewayChatInvoker) logger() *slog.Logger {
	if g.log != nil {
		return g.log
	}
	return slog.Default()
}

// Chat 发一趟对话。
//
// 返回的 error 一律是 *domain.DesignkitError，Message 是给运营看的中文。
func (g *GatewayChatInvoker) Chat(ctx context.Context, req ChatRequest) (*ChatResult, error) {
	if g == nil || g.chat == nil || g.keys == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithCause(errors.New("designkit: 对话调用器没有装配"))
	}
	if strings.TrimSpace(req.UserText) == "" {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithCause(errors.New("designkit: 对话内容为空"))
	}

	billingID, err := resolveChatBillingRequestID(req)
	if err != nil {
		return nil, err
	}
	req.RequestID = billingID

	apiKey, err := g.loadChatAPIKey(ctx, req)
	if err != nil {
		return nil, err
	}

	body, err := buildChatCompletionsBody(req)
	if err != nil {
		return nil, err
	}

	taskCtx, recorder, cancel, err := newSyntheticChatContext(ctx, apiKey, req, body)
	if err != nil {
		return nil, err
	}
	defer cancel()

	g.chat.ChatCompletions(taskCtx)

	return g.readChatResult(recorder, req)
}

// loadChatAPIKey 取 Key 并校验归属和状态。跟 gateway_invoke.go 的 loadAPIKey 同一套判据。
//
// 单独写一份而不是复用，是因为那一份的入参类型是 GenerateRequest（出图专用）。
// 判据本身**必须保持一致**：哪天那边加了新校验，这边也要加。
func (g *GatewayChatInvoker) loadChatAPIKey(ctx context.Context, req ChatRequest) (*upstreamservice.APIKey, error) {
	if req.APIKeyID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
			WithCause(errors.New("designkit: 对话请求没有带 api_key_id"))
	}
	apiKey, err := g.keys.GetByID(ctx, req.APIKeyID)
	if err != nil || apiKey == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).WithCause(err)
	}
	// 归属校验：不匹配就是把钱记到别人头上，宁可这一趟失败。
	if apiKey.UserID != req.UserID {
		return nil, dkdomain.NewError(dkdomain.ErrCodeForbidden).
			WithCause(fmt.Errorf("designkit: api key %d 属于用户 %d，与请求用户 %d 不符",
				apiKey.ID, apiKey.UserID, req.UserID))
	}
	if !apiKey.IsActive() {
		return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
			WithCause(fmt.Errorf("designkit: api key %d 状态为 %s", apiKey.ID, apiKey.Status))
	}
	// 上游用 apiKey.User 做计费准入，为 nil 会在那边炸，这里提前拦下给中文提示。
	if apiKey.User == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
			WithCause(fmt.Errorf("designkit: api key %d 没有加载到用户信息", apiKey.ID))
	}
	return apiKey, nil
}

// resolveChatBillingRequestID 定下这一趟的计费 id 并守住长度。
//
// 长度那一条是硬约束，理由见文件头：超长 = 钱扣了账单没有，且无告警。
func resolveChatBillingRequestID(req ChatRequest) (string, error) {
	given := strings.TrimSpace(req.RequestID)
	if given == "" {
		given = BuildChatBillingRequestID("anon", 0)
	}
	if stored := dkdomain.UpstreamClientRequestIDPrefix + given; len(stored) > dkdomain.UsageLogRequestIDMaxLen {
		return "", dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithCause(fmt.Errorf("designkit: 对话的 request_id 落 usage_logs 会超长（%d > %d）：%q",
				len(stored), dkdomain.UsageLogRequestIDMaxLen, stored))
	}
	return given, nil
}

// ---------------------------------------------------------------------------
// 请求体
// ---------------------------------------------------------------------------

// chatContentPart 是 messages[].content 数组里的一项。
//
// 纯文字消息本可以直接用字符串，这里一律用数组形式：
// 两种形状混着写，将来加图片时很容易漏改一处，而漏改的表现是「模型看不见图」——
// 它不会报错，只会答得驴唇不对马嘴，极难往「请求体形状」上想。
type chatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

type chatMessage struct {
	Role    string            `json:"role"`
	Content []chatContentPart `json:"content"`
}

type chatCompletionsBody struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

// buildChatCompletionsBody 拼 JSON 请求体。
//
// 故意**不写** stream / temperature / max_tokens：
//   - stream 会让上游走流式分支，我们这里只要一段完整文本，多一条路径就多一处会坏；
//   - 另外两个交给上游和模型的默认值。写死它们等于把「以后模型换了怎么调」
//     藏进代码里，而这本该是配置。
func buildChatCompletionsBody(req ChatRequest) ([]byte, error) {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = DefaultChatModel
	}

	messages := make([]chatMessage, 0, 2)
	if system := strings.TrimSpace(req.System); system != "" {
		messages = append(messages, chatMessage{
			Role:    "system",
			Content: []chatContentPart{{Type: "text", Text: system}},
		})
	}

	// 顺序刻意是「先文字、后图片」：文字里写的是「看着这张图做什么」，
	// 放在图片前面读起来才是一句完整的指令。
	parts := make([]chatContentPart, 0, len(req.Images)+1)
	parts = append(parts, chatContentPart{Type: "text", Text: req.UserText})
	for i, img := range req.Images {
		if len(img.Data) == 0 {
			continue
		}
		if len(img.Data) > chatMaxImageBytes {
			return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
				WithMessage("这张商品图太大，AI 读不了，请换一张小一点的。").
				WithCause(fmt.Errorf("designkit: 第 %d 张图 %d 字节，超过对话上限 %d",
					i+1, len(img.Data), chatMaxImageBytes))
		}
		mime := strings.TrimSpace(img.ContentType)
		if mime == "" {
			mime = "image/png"
		}
		parts = append(parts, chatContentPart{
			Type: "image_url",
			ImageURL: &chatImageURL{
				URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data),
			},
		})
	}
	messages = append(messages, chatMessage{Role: "user", Content: parts})

	body, err := json.Marshal(chatCompletionsBody{Model: model, Messages: messages})
	if err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
	return body, nil
}

// ---------------------------------------------------------------------------
// 合成 gin.Context
// ---------------------------------------------------------------------------

// newSyntheticChatContext 跟 newSyntheticImagesContext 逐步对应。
//
// **每一步都不是可选的**，逐条理由见 gateway_invoke.go 里那一份的注释；
// 这里只重复最容易被当成多余而删掉的两条：
//
//   - WithoutCancel：调用方（HTTP 请求）的 ctx 一旦取消，对话就断。
//     而运营刷新页面就会取消——钱已经在花了，断掉只是拿不到结果。
//   - 覆写 ClientRequestID/RequestID：不覆写的话，三趟对话会继承入站请求那一个
//     id，于是幂等表把后两趟判成重复，**静默不扣钱**。金额小归小，
//     但账目对不上比多花几厘钱麻烦得多。
func newSyntheticChatContext(
	parent context.Context,
	apiKey *upstreamservice.APIKey,
	req ChatRequest,
	body []byte,
) (*gin.Context, *httptest.ResponseRecorder, context.CancelFunc, error) {
	if parent == nil {
		parent = context.Background()
	}
	if apiKey == nil || apiKey.User == nil {
		return nil, nil, nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
			WithCause(errors.New("designkit: 合成对话请求缺少 API Key 或用户信息"))
	}

	base := context.WithoutCancel(parent)
	base = context.WithValue(base, ctxkey.ClientRequestID, req.RequestID)
	base = context.WithValue(base, ctxkey.RequestID, req.RequestID)
	base = context.WithValue(base, ctxkey.UserID, apiKey.User.ID)
	if upstreamservice.IsGroupContextValid(apiKey.Group) {
		base = context.WithValue(base, ctxkey.Group, apiKey.Group)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = chatDefaultTimeout
	}
	execCtx, cancel := context.WithTimeout(base, timeout)

	httpReq, err := http.NewRequestWithContext(execCtx, http.MethodPost, chatCompletionsURL, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, nil, nil, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}

	// Body / GetBody / ContentLength 三个都要设：上游按 ContentLength 预分配读 body，
	// 失败切换账号时会用 GetBody 重放。
	httpReq.Body = io.NopCloser(bytes.NewReader(body))
	httpReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	httpReq.ContentLength = int64(len(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.RemoteAddr = syntheticRemoteAddr
	stripStickySessionHeaders(httpReq.Header)

	recorder := httptest.NewRecorder()
	taskCtx, _ := gin.CreateTestContext(recorder)
	taskCtx.Request = httpReq

	// 这三个键是 apiKeyAuth 中间件平时写进去的，缺了上游直接 401。
	taskCtx.Set(string(middleware.ContextKeyAPIKey), apiKey)
	taskCtx.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{
		UserID:      apiKey.User.ID,
		Concurrency: apiKey.User.Concurrency,
	})
	taskCtx.Set(string(middleware.ContextKeyUserRole), apiKey.User.Role)

	return taskCtx, recorder, cancel, nil
}

// ---------------------------------------------------------------------------
// 结果读取
// ---------------------------------------------------------------------------

// chatCompletionsResponse 只挑我们用得上的字段。
//
// 上游把网关返回的 JSON 原样透传，字段比这里多得多；只声明用得上的，
// 多余字段被 encoding/json 忽略——这样上游加字段不会把我们弄坏。
type chatCompletionsResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// readChatResult 从 recorder 里取出文本。
func (g *GatewayChatInvoker) readChatResult(recorder *httptest.ResponseRecorder, req ChatRequest) (*ChatResult, error) {
	if recorder == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithCause(errors.New("designkit: 对话没有拿到任何响应"))
	}
	raw := recorder.Body.Bytes()

	var payload chatCompletionsResponse
	// 解不开也继续往下走：下面会用状态码和原文片段拼一条能查的错误。
	_ = json.Unmarshal(raw, &payload)

	if recorder.Code != http.StatusOK {
		msg := ""
		if payload.Error != nil {
			msg = strings.TrimSpace(payload.Error.Message)
		}
		if msg == "" {
			msg = gatewaySnippet(raw)
		}
		g.logger().Warn("designkit 对话调用失败",
			slog.Int("status", recorder.Code),
			slog.String("request_id", req.RequestID),
			slog.String("upstream", msg))
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpstreamError).
			WithCause(fmt.Errorf("designkit: 对话返回 %d：%s", recorder.Code, msg))
	}

	// 状态码 200 但 body 里带 error 的情况是存在的（上游把网关的错误体透传了）。
	if payload.Error != nil && strings.TrimSpace(payload.Error.Message) != "" {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpstreamError).
			WithCause(fmt.Errorf("designkit: 对话返回 200 但带错误：%s", payload.Error.Message))
	}

	text := ""
	if len(payload.Choices) > 0 {
		text = strings.TrimSpace(payload.Choices[0].Message.Content)
	}
	if text == "" {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpstreamError).
			WithCause(fmt.Errorf("designkit: 对话返回 200 但没有正文：%s", gatewaySnippet(raw)))
	}
	return &ChatResult{Text: text}, nil
}
