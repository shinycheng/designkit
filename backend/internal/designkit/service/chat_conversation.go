package service

// chat_conversation.go —— 「AI 对话」页的业务编排（monica 2026-08-15 拍板：
// 会话列表保存 / 能发图提问 / 所有运营可用）。
//
// 它站在两个现成件上：
//   - chat_invoke.go 的 ChatInvoker：发一趟对话拿回文字（读图、合成上下文、
//     计费长度守卫都在那边）；
//   - repository.ChatRepo：会话与消息的持久化。
//
// 这一层自己只管四件事：归属校验、历史窗口、计费 id 的生成纪律、落库顺序。
//
// # 计费（跟推荐的三条纪律一致，一条都不能松）
//
//  1. scope **每次发送现生成 ULID**。复用会话 uid 之类的稳定值会踩上游幂等表：
//     第二次起静默不计费（CLAUDE.md 决策 27 踩过）。
//  2. 前缀 dkc:（对话）——跟 dki:（出图）、dks:（推荐）分开，统计能拆。
//  3. 长度由 chat_invoke.go 的 resolveChatBillingRequestID 再守一遍。
//
// # 落库顺序：**模型成功之后才落任何东西**
//
// 失败的发送什么都不留（会话也不建），运营在前端点「重发」即可。
// 这样表里不会积累半截会话；代价是「用户消息、AI 回复」两条 INSERT 之间
// 若进程崩溃会留下一条没有回复的用户消息——概率极小，重发一次即可恢复，
// 不为它上事务（repo 方法各自独立，跨方法事务要改接口形状，不值得）。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	// conversationMaxTextRunes 单条消息的正文上限（按字符数）。
	//
	// 4000 字够贴一整段商品描述；再长基本是误粘贴，白烧 token。
	conversationMaxTextRunes = 4000

	// conversationMaxImages 单条消息最多带几张图。跟推荐的 suggestMaxImages 同值。
	conversationMaxImages = 3

	// conversationHistoryWindow 发给模型的历史窗口（条数，user+assistant 都算）。
	//
	// 20 条 ≈ 10 轮问答。再往前的内容对当前问题的价值急剧下降，
	// 而历史是每次发送都要重新付 token 钱的。
	conversationHistoryWindow = 20

	// conversationTimeout 单次发送的超时。
	//
	// 比推荐单趟的 35 秒宽：对话的回答可以很长（写文案、列清单）。
	// 前端把这个接口的超时放到 120 秒，后端必须明显小于它，
	// 否则前端先报超时、后端还在算还在花钱。
	conversationTimeout = 60 * time.Second

	// conversationTitleRunes 自动标题取首条消息的前多少个字符。
	conversationTitleRunes = 24

	// conversationSystemPrompt 对话的系统提示词。
	//
	// 保持克制：定身份和口径，不塞长篇规则。运营的问题五花八门，
	// 提示词写太死反而答不好。
	conversationSystemPrompt = "你是电商运营的工作助手，擅长商品图、文案、店铺运营。" +
		"用中文回答，直接给可执行的建议，不要客套。运营发来商品图时，先看图再答。"
)

// ChatStore 是本服务对持久层的全部要求。repository.ChatRepo 实现了它。
//
// 抽成接口是给单测用的（不碰真数据库，CLAUDE.md 第三节）。
type ChatStore interface {
	CreateSession(ctx context.Context, userID int64, uid string, title string) (*dkdomain.ChatSession, error)
	GetSessionByUID(ctx context.Context, userID int64, uid string) (*dkdomain.ChatSession, error)
	ListSessions(ctx context.Context, userID int64, limit, offset int) ([]*dkdomain.ChatSession, error)
	TouchSession(ctx context.Context, sessionID int64, title string) error
	SoftDeleteSession(ctx context.Context, userID int64, uid string) error
	InsertMessage(ctx context.Context, msg *dkdomain.ChatMessage) (*dkdomain.ChatMessage, error)
	ListMessages(ctx context.Context, sessionID int64) ([]*dkdomain.ChatMessage, error)
}

// ConversationDeps 装配 ConversationService 要的东西。
type ConversationDeps struct {
	// Store 持久层。必填。
	Store ChatStore
	// Assets 取商品图字节（校验归属就在它里面——取不到别人的图）。必填。
	Assets AssetContentLoader
	// Chat 模型调用。必填。
	Chat ChatInvoker
	// Keys 内部专用 Key。为 nil 时发送直接返回中文的「还没准备好」。
	//
	// ⚠ typed-nil：装配处必须 `if m.keys != nil { deps.Keys = m.keys }`，
	// 直接赋值会把 nil 指针装进接口，这里的判空就失效了（module.go 有同样的注释）。
	Keys internalKeyEnsurer
	// Logger 可空。
	Logger *slog.Logger
}

// ConversationService 「AI 对话」页的业务实现。
type ConversationService struct {
	store  ChatStore
	assets AssetContentLoader
	chat   ChatInvoker
	keys   internalKeyEnsurer
	log    *slog.Logger
}

// internalKeyEnsurer 是对 InternalKeyService 的最小切面。
type internalKeyEnsurer interface {
	EnsureInternalKey(ctx context.Context, userID int64) (*upstreamservice.APIKey, error)
}

// NewConversationService 装配。必填项缺一个就报错——启动期就该炸，别拖到运营点的时候。
func NewConversationService(deps ConversationDeps) (*ConversationService, error) {
	if deps.Store == nil {
		return nil, errors.New("designkit: 对话服务缺持久层")
	}
	if deps.Assets == nil {
		return nil, errors.New("designkit: 对话服务缺商品图读取")
	}
	if deps.Chat == nil {
		return nil, errors.New("designkit: 对话服务缺模型调用器")
	}
	return &ConversationService{
		store:  deps.Store,
		assets: deps.Assets,
		chat:   deps.Chat,
		keys:   deps.Keys,
		log:    deps.Logger,
	}, nil
}

func (s *ConversationService) logger() *slog.Logger {
	if s.log != nil {
		return s.log
	}
	return slog.Default()
}

// SendInput 一次发送。
type SendInput struct {
	// UserID 谁在聊。
	UserID int64
	// SessionUID 目标会话；空串 = 新建一个。
	SessionUID string
	// Text 正文。必填，≤ conversationMaxTextRunes 字符。
	Text string
	// AssetUIDs 附带的商品图，≤ conversationMaxImages 张。
	AssetUIDs []string
}

// SendResult 一次发送的产出。
type SendResult struct {
	Session          *dkdomain.ChatSession
	UserMessage      *dkdomain.ChatMessage
	AssistantMessage *dkdomain.ChatMessage
}

// Send 发一条消息并等 AI 回复（一次拿全文）。
func (s *ConversationService) Send(ctx context.Context, in SendInput) (*SendResult, error) {
	return s.send(ctx, in, func(req ChatRequest) (*ChatResult, error) {
		return s.chat.Chat(ctx, req)
	})
}

// SendStream 发一条消息并把 AI 回复**边生成边**通过 onDelta 回调出去。
//
// 编排跟 Send 完全一致（历史窗口、图片、上限、计费纪律、落库顺序）——
// 两条路走的就是同一个 send()，只是模型调用换成流式那一条。
// **流完且拿到全文才落库**：中途断流（上游 error 帧、超时）什么都不留，
// 运营点「重发」即可，跟 Send 失败时的语义一模一样。
//
// onDelta 在模型生成期间被逐段调用（同一 goroutine、按顺序）；
// 调用器不支持流式时回落成非流式：全文一次性作为唯一一段回调出去，
// 调用方不需要为这种情况写第二套处理。
func (s *ConversationService) SendStream(ctx context.Context, in SendInput, onDelta func(text string)) (*SendResult, error) {
	return s.send(ctx, in, func(req ChatRequest) (*ChatResult, error) {
		if streamer, ok := s.chat.(ChatStreamInvoker); ok {
			return streamer.StreamChat(ctx, req, onDelta)
		}
		result, err := s.chat.Chat(ctx, req)
		if err == nil && result != nil && onDelta != nil && result.Text != "" {
			onDelta(result.Text)
		}
		return result, err
	})
}

// send Send / SendStream 共用的编排。invoke 是「怎么调模型」的那一步，
// 其余每一行对两条路都生效——流式若单独抄一份，迟早会跟这份漂移。
func (s *ConversationService) send(ctx context.Context, in SendInput, invoke func(req ChatRequest) (*ChatResult, error)) (*SendResult, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("消息不能为空。")
	}
	if utf8.RuneCountInString(text) > conversationMaxTextRunes {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage(fmt.Sprintf("单条消息最多 %d 个字。", conversationMaxTextRunes))
	}
	if len(in.AssetUIDs) > conversationMaxImages {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage(fmt.Sprintf("一次最多 %d 张图。", conversationMaxImages))
	}
	if s.keys == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
			WithMessage("AI 对话还没准备好，请联系管理员。")
	}

	// 已有会话：先取（顺带完成归属校验）；新会话此刻**只定 uid，不落库**——
	// 模型失败时不留半截会话。
	var session *dkdomain.ChatSession
	var history []ChatTurn
	if uid := strings.TrimSpace(in.SessionUID); uid != "" {
		existing, err := s.store.GetSessionByUID(ctx, in.UserID, uid)
		if err != nil {
			return nil, err
		}
		session = existing
		msgs, err := s.store.ListMessages(ctx, existing.ID)
		if err != nil {
			return nil, err
		}
		history = buildHistoryWindow(msgs)
	}

	// 取图（AssetContent 内部校验归属：取不到别人的图）。
	images := make([]ChatImage, 0, len(in.AssetUIDs))
	for _, uid := range in.AssetUIDs {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		data, contentType, err := s.assets.AssetContent(ctx, in.UserID, uid)
		if err != nil {
			return nil, err
		}
		images = append(images, ChatImage{Data: data, ContentType: contentType})
	}

	apiKey, err := s.keys.EnsureInternalKey(ctx, in.UserID)
	if err != nil {
		return nil, err
	}

	// 计费 id：每次发送现生成 ULID（纪律 1，见文件头）。
	requestID := BuildConversationBillingRequestID(newAssetULID())

	result, err := invoke(ChatRequest{
		UserID:    in.UserID,
		APIKeyID:  apiKey.ID,
		System:    conversationSystemPrompt,
		History:   history,
		UserText:  text,
		Images:    images,
		RequestID: requestID,
		Timeout:   conversationTimeout,
	})
	if err != nil {
		return nil, err
	}

	// —— 模型成功，现在才开始落库 ——
	if session == nil {
		session, err = s.store.CreateSession(ctx, in.UserID, newAssetULID(), autoTitle(text))
		if err != nil {
			return nil, err
		}
	}

	userMsg, err := s.store.InsertMessage(ctx, &dkdomain.ChatMessage{
		SessionID: session.ID,
		Role:      "user",
		Content:   text,
		AssetUIDs: normalizeAssetUIDs(in.AssetUIDs),
		RequestID: requestID,
	})
	if err != nil {
		return nil, err
	}
	assistantMsg, err := s.store.InsertMessage(ctx, &dkdomain.ChatMessage{
		SessionID: session.ID,
		Role:      "assistant",
		Content:   result.Text,
		RequestID: requestID,
	})
	if err != nil {
		// 用户消息已在库里而回复丢了：如实报错，运营重发一次即可（文件头有权衡）。
		s.logger().Error("designkit 对话：AI 回复落库失败",
			slog.Int64("session_id", session.ID), slog.String("request_id", requestID))
		return nil, err
	}
	if err := s.store.TouchSession(ctx, session.ID, ""); err != nil {
		// 只影响列表排序，不值得让整次发送失败。
		s.logger().Warn("designkit 对话：会话时间戳没刷新", slog.Int64("session_id", session.ID))
	}

	return &SendResult{Session: session, UserMessage: userMsg, AssistantMessage: assistantMsg}, nil
}

// ListSessions 会话列表（最近的在前，封顶 100 条）。
func (s *ConversationService) ListSessions(ctx context.Context, userID int64) ([]*dkdomain.ChatSession, error) {
	return s.store.ListSessions(ctx, userID, 100, 0)
}

// GetSession 一个会话和它的全部消息（按发生顺序）。
func (s *ConversationService) GetSession(ctx context.Context, userID int64, uid string) (*dkdomain.ChatSession, []*dkdomain.ChatMessage, error) {
	session, err := s.store.GetSessionByUID(ctx, userID, uid)
	if err != nil {
		return nil, nil, err
	}
	msgs, err := s.store.ListMessages(ctx, session.ID)
	if err != nil {
		return nil, nil, err
	}
	return session, msgs, nil
}

// DeleteSession 删除一个会话（软删；消息留在库里跟着会话一起不可见）。
func (s *ConversationService) DeleteSession(ctx context.Context, userID int64, uid string) error {
	return s.store.SoftDeleteSession(ctx, userID, uid)
}

// buildHistoryWindow 把库里的消息裁成发给模型的历史窗口。
//
// 只取**最后** conversationHistoryWindow 条——最近的上下文最值钱（理由见常量注释）。
// 带图的轮次加一个「[附图]」前缀：图本身不重发（ChatTurn 的注释），
// 但要让模型知道那一轮曾有图，否则它会把「上面那张图」理解成不存在的东西。
func buildHistoryWindow(msgs []*dkdomain.ChatMessage) []ChatTurn {
	start := 0
	if len(msgs) > conversationHistoryWindow {
		start = len(msgs) - conversationHistoryWindow
	}
	turns := make([]ChatTurn, 0, len(msgs)-start)
	for _, m := range msgs[start:] {
		text := m.Content
		if len(m.AssetUIDs) > 0 {
			text = "[附图] " + text
		}
		turns = append(turns, ChatTurn{Role: m.Role, Text: text})
	}
	return turns
}

// autoTitle 从首条消息取会话标题。
func autoTitle(text string) string {
	// 只取第一行——多行粘贴时标题不该带换行。
	if idx := strings.IndexAny(text, "\r\n"); idx >= 0 {
		text = text[:idx]
	}
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > conversationTitleRunes {
		return string(runes[:conversationTitleRunes]) + "…"
	}
	if len(runes) == 0 {
		return "新对话"
	}
	return string(runes)
}

// normalizeAssetUIDs 去空白、去空串、去重（保序）。
func normalizeAssetUIDs(uids []string) []string {
	seen := make(map[string]struct{}, len(uids))
	out := make([]string, 0, len(uids))
	for _, u := range uids {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}
