package service

// chat_invoke_stream.go —— 对话的**流式**调用：边生成边把正文一段段吐给调用方。
//
// 它是 chat_invoke.go 的兄弟文件：Chat() 一次拿全文（推荐功能还在用它，一行没动），
// StreamChat() 走同一条进程内重入的路（同一个 ChatCompletions()、同一把 Key、
// 同一套计费纪律），只是请求体多一个 "stream": true，响应从「读 recorder」
// 变成「边写边解析 SSE」。
//
// # 上游是怎么把流写出来的（照 openai_gateway_chat_completions.go 读出来的事实）
//
// 上游拿到 stream:true 的请求后，往 c.Writer 上：
//  1. 设头（Content-Type: text/event-stream 等）+ WriteHeader(200)
//     ——由 newStreamHeaderWriter 延迟到第一个事件前才提交；
//  2. 逐帧 fmt.Fprint(c.Writer, "data: {chat.completion.chunk JSON}\n\n")，
//     每帧后 c.Writer.Flush()；
//  3. 中途出错写 "data: {\"error\":{...}}\n\n"，收尾写 "data: [DONE]\n\n"；
//  4. 流开始**之前**出错则回退成普通 JSON 错误响应（非 200 状态码）。
//
// 所以这里把合成 gin.Context 的 Writer 换成 chatStreamSink：
// 一个实现了 gin.ResponseWriter（含 http.Flusher）的自定义 writer，
// Write 进来的字节按行切、认出 data: 帧、把 delta.content 逐段回调出去。
//
// # ⚠ 计费三纪律与非流式完全一致，一条都不放松
//
//   - RequestID 复用 resolveChatBillingRequestID（长度守卫，超长 = 钱扣了账单没有）；
//   - scope 由调用方每次现生成（BuildConversationBillingRequestID 的注释）；
//   - 前缀不变（对话 dkc:、推荐 dks:）——本文件不发明任何新前缀。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ChatStreamInvoker 是「发一趟对话、边生成边拿正文」这一个能力。
//
// 单独一个接口而不是塞进 ChatInvoker：推荐（prompt_suggest.go）只要全文，
// 它的假实现不该被逼着实现流式；对话服务按需断言这个接口，断言不到就回落非流式。
type ChatStreamInvoker interface {
	// StreamChat 发一趟流式对话。onDelta 每收到一段正文调一次
	// （同一 goroutine、按到达顺序，可为 nil）；返回值与 Chat 相同：
	// [DONE] 之后把累积的全文装进 ChatResult。
	// 上游中途报错（SSE 里的 error 帧或非 200）翻译成中文 *DesignkitError。
	StreamChat(ctx context.Context, req ChatRequest, onDelta func(text string)) (*ChatResult, error)
}

var _ ChatStreamInvoker = (*GatewayChatInvoker)(nil)

// StreamChat 发一趟流式对话。
//
// 前置校验、计费 id、取 Key、拼请求体都跟 Chat() 逐步对应——
// 哪天那边加了新校验，这边也要加（同 loadChatAPIKey 的约定）。
func (g *GatewayChatInvoker) StreamChat(ctx context.Context, req ChatRequest, onDelta func(text string)) (*ChatResult, error) {
	if g == nil || g.chat == nil || g.keys == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithCause(errors.New("designkit: 对话调用器没有装配"))
	}
	if strings.TrimSpace(req.UserText) == "" {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithCause(errors.New("designkit: 对话内容为空"))
	}

	// 计费 id：复用非流式那一条长度守卫（纪律，见文件头）。
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
	streamBody, err := withStreamFlag(body)
	if err != nil {
		return nil, err
	}

	// 合成上下文复用非流式那一份（每一步都不是可选的，理由见那边的注释），
	// 然后把 Writer 换成我们的 SSE 解析器。recorder 从此不再被写，弃用即可。
	taskCtx, _, cancel, err := newSyntheticChatContext(ctx, apiKey, req, streamBody)
	if err != nil {
		return nil, err
	}
	defer cancel()

	sink := newChatStreamSink(onDelta)
	taskCtx.Writer = sink
	// SSE 语义下 Accept 跟着换。上游按 body 里的 stream 字段判流式，
	// 这个头只是把请求描述得诚实一点，不承担分支职责。
	taskCtx.Request.Header.Set("Accept", "text/event-stream")

	g.chat.ChatCompletions(taskCtx)

	return g.readChatStreamResult(sink, req)
}

// withStreamFlag 在已经拼好的 JSON 请求体末尾补 "stream": true。
//
// 为什么不改 buildChatCompletionsBody：那是非流式在用的函数（推荐三趟全靠它），
// 这里一个字节都不碰它。也不解包重编——请求体里可能有几 MB 的 base64 图片，
// Unmarshal/Marshal 一个来回是纯浪费。json.Marshal 的输出恒以 '}' 结尾且无尾随
// 空白，直接在收尾大括号前拼一个成员是安全的。
func withStreamFlag(body []byte) ([]byte, error) {
	if len(body) < 2 || body[len(body)-1] != '}' {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithCause(errors.New("designkit: 对话请求体不是 JSON 对象，无法追加 stream 标记"))
	}
	out := make([]byte, 0, len(body)+len(`,"stream":true}`))
	out = append(out, body[:len(body)-1]...)
	out = append(out, `,"stream":true}`...)
	return out, nil
}

// readChatStreamResult 收尾：把 sink 里累积的状态翻译成 ChatResult 或中文错误。
func (g *GatewayChatInvoker) readChatStreamResult(sink *chatStreamSink, req ChatRequest) (*ChatResult, error) {
	if sink == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithCause(errors.New("designkit: 对话流没有拿到任何响应"))
	}
	sink.finishPending()

	raw := sink.rawBytes()

	// 流开始之前失败：上游写的是普通 JSON 错误 + 非 200 状态码，
	// 跟非流式的 readChatResult 同一套翻译。
	if sink.Status() != http.StatusOK {
		var payload chatCompletionsResponse
		_ = json.Unmarshal(raw, &payload)
		msg := ""
		if payload.Error != nil {
			msg = strings.TrimSpace(payload.Error.Message)
		}
		if msg == "" {
			msg = gatewaySnippet(raw)
		}
		g.logger().Warn("designkit 对话流式调用失败",
			slog.Int("status", sink.Status()),
			slog.String("request_id", req.RequestID),
			slog.String("upstream", msg))
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpstreamError).
			WithMessage("AI 回复失败，请重发；一直失败就把错误码发给管理员。").
			WithCause(fmt.Errorf("designkit: 对话流返回 %d：%s", sink.Status(), msg))
	}

	// 流中途的 error 帧：钱可能已经在花，但全文不完整，如实报错、不落库
	//（落不落库是上层的事，这里只保证不把半截文本当成功）。
	if streamErr := sink.streamError(); streamErr != "" {
		g.logger().Warn("designkit 对话流中途报错",
			slog.String("request_id", req.RequestID),
			slog.String("upstream", streamErr))
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpstreamError).
			WithMessage("AI 回复中断，请重发；一直这样就把错误码发给管理员。").
			WithCause(fmt.Errorf("designkit: 对话流中途报错：%s", streamErr))
	}

	// 上游没走流式（比如以后有人把 stream 字段改写掉了）：200 + application/json。
	// 按非流式解析整包，正文一次性回调出去——调用方看到的仍是「一段 delta + 完整结果」。
	if !sink.sawSSE() {
		var payload chatCompletionsResponse
		_ = json.Unmarshal(raw, &payload)
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
		sink.emit(text)
		return &ChatResult{Text: text}, nil
	}

	text := strings.TrimSpace(sink.fullText())
	if text == "" {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpstreamError).
			WithCause(fmt.Errorf("designkit: 对话流结束但没有正文：%s", gatewaySnippet(raw)))
	}
	return &ChatResult{Text: text}, nil
}

// ---------------------------------------------------------------------------
// chatStreamSink：装在合成 gin.Context 上的自定义 ResponseWriter
// ---------------------------------------------------------------------------

// chatStreamRawCap 原始响应字节的保留上限。
//
// 保留原始字节只有两个用途：非 200 时解析错误体、200 非 SSE 时解析整包 JSON——
// 两者都不会大。正常流式时全文另有 builder 在攒，raw 超过上限就停止追加，
// 免得一场长对话把整条 SSE 流复制一份白占内存。
const chatStreamRawCap = 1 << 20

// chatStreamChunk 只挑流式帧里我们用得上的字段（多余字段被忽略，上游加字段不破坏）。
type chatStreamChunk struct {
	Choices []struct {
		Delta struct {
			// Content 用指针区分「没有这个字段」（role 帧、finish 帧）和空串。
			Content *string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// chatStreamSink 实现 gin.ResponseWriter 的最小面 + http.Flusher。
//
// 状态机全按 gin 自己的 responseWriter 摹写（status 缓存、size 从 -1 起、
// WriteHeaderNow 才算提交）——上游代码里有 `c.Writer.Written()`、`c.Writer.Size()`
// 的判断，行为对不上会让它选错分支（比如误以为还能写 JSON 错误）。
type chatStreamSink struct {
	header http.Header

	status    int
	size      int // -1 = 还没写过（gin 的 noWritten 语义）
	committed bool
	isSSE     bool

	onDelta func(string)

	lineBuf bytes.Buffer    // 未凑满一行的残片（SSE 帧可能被拆在多次 Write 里）
	full    strings.Builder // 正文累计
	raw     bytes.Buffer    // 原始响应（截断保留，只用于报错和非 SSE 回落）

	done   bool   // 见过 data: [DONE]
	errMsg string // 流中途 error 帧里的第一条消息

	closeCh chan bool
}

var (
	_ gin.ResponseWriter = (*chatStreamSink)(nil)
	_ http.Flusher       = (*chatStreamSink)(nil)
)

func newChatStreamSink(onDelta func(string)) *chatStreamSink {
	return &chatStreamSink{
		header:  make(http.Header),
		status:  http.StatusOK,
		size:    -1,
		onDelta: onDelta,
		closeCh: make(chan bool),
	}
}

// ---- http.ResponseWriter ----

func (s *chatStreamSink) Header() http.Header { return s.header }

func (s *chatStreamSink) WriteHeader(code int) {
	// gin 语义：提交之前只缓存状态码，不落地。
	if code > 0 && !s.committed {
		s.status = code
	}
}

func (s *chatStreamSink) Write(b []byte) (int, error) {
	s.WriteHeaderNow()
	if s.raw.Len() < chatStreamRawCap {
		remain := chatStreamRawCap - s.raw.Len()
		if len(b) <= remain {
			_, _ = s.raw.Write(b)
		} else {
			_, _ = s.raw.Write(b[:remain])
		}
	}
	if s.isSSE {
		s.feed(b)
	}
	s.size += len(b)
	return len(b), nil
}

// ---- gin.ResponseWriter 余下的最小面 ----

func (s *chatStreamSink) WriteString(str string) (int, error) { return s.Write([]byte(str)) }

func (s *chatStreamSink) WriteHeaderNow() {
	if s.committed {
		return
	}
	s.committed = true
	if s.size < 0 {
		s.size = 0
	}
	// 提交那一刻按响应头定分支：SSE 逐帧解析，其余整包缓冲留到收尾再解析。
	ct := s.header.Get("Content-Type")
	s.isSSE = strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "text/event-stream")
}

func (s *chatStreamSink) Status() int { return s.status }

func (s *chatStreamSink) Size() int { return s.size }

func (s *chatStreamSink) Written() bool { return s.size != -1 }

// Flush 上游每帧后都会调。解析是跟着 Write 走的，这里无事可做，
// 但方法必须在：gin.ResponseWriter 含 http.Flusher，缺了编译不过。
func (s *chatStreamSink) Flush() { s.WriteHeaderNow() }

// Hijack 不支持：这是进程内的合成响应，没有底层连接可交。
func (s *chatStreamSink) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("designkit: 合成对话响应不支持 Hijack")
}

// CloseNotify 返回一个永不触发的通道：合成请求没有「客户端断开」这回事
// （断不断由外层 ctx 决定，而那个 ctx 已经 WithoutCancel 了）。
func (s *chatStreamSink) CloseNotify() <-chan bool { return s.closeCh }

func (s *chatStreamSink) Pusher() http.Pusher { return nil }

// ---- SSE 解析 ----

// feed 按行切。SSE 帧可能被拆在多次 Write 里，残片留在 lineBuf 等下一次拼齐。
func (s *chatStreamSink) feed(b []byte) {
	_, _ = s.lineBuf.Write(b)
	for {
		data := s.lineBuf.Bytes()
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			return
		}
		line := string(bytes.TrimRight(data[:idx], "\r"))
		s.lineBuf.Next(idx + 1)
		s.handleLine(line)
	}
}

// finishPending 把结尾没带换行的残行也处理掉（规范上不会发生，解析器不做假设）。
func (s *chatStreamSink) finishPending() {
	if s.lineBuf.Len() == 0 {
		return
	}
	line := strings.TrimRight(s.lineBuf.String(), "\r")
	s.lineBuf.Reset()
	s.handleLine(line)
}

func (s *chatStreamSink) handleLine(line string) {
	if s.done {
		return
	}
	// 上游的 chat SSE 只有 data: 行；空行是帧分隔符，别的行一律忽略。
	payload, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return
	}
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return
	}
	if payload == "[DONE]" {
		s.done = true
		return
	}

	var chunk chatStreamChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		// 认不出的帧跳过而不是报错：上游以后加新帧型不该把我们弄坏。
		return
	}
	if chunk.Error != nil {
		if msg := strings.TrimSpace(chunk.Error.Message); msg != "" && s.errMsg == "" {
			s.errMsg = msg
		}
		return
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Content == nil || *choice.Delta.Content == "" {
			continue
		}
		s.emit(*choice.Delta.Content)
	}
}

// emit 累积一段正文并回调出去。
func (s *chatStreamSink) emit(piece string) {
	_, _ = s.full.WriteString(piece)
	if s.onDelta != nil {
		s.onDelta(piece)
	}
}

// ---- 收尾读取 ----

func (s *chatStreamSink) rawBytes() []byte { return s.raw.Bytes() }

func (s *chatStreamSink) fullText() string { return s.full.String() }

func (s *chatStreamSink) streamError() string { return s.errMsg }

func (s *chatStreamSink) sawSSE() bool { return s.isSSE }
