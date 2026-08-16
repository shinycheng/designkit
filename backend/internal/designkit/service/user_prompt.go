package service

// ============================================================================
// 我的提示词（运营自建，source='user'）
// ============================================================================
//
// 跟灵感库浏览（module.go 的 promptServiceAdapter，纯查询、无规则）不同，
// 这里有四条业务规则，所以单独一个 service：
//
//  1. **只有本人能改、能删自己的**：别人的自建词一律按「找不到」处理
//     （不是 403 —— 403 等于告诉对方「这条编号存在，只是不是你的」）；
//  2. **同步来的（youmind）不能改也不能删**：它们是全站共享目录，
//     且每 12 小时会被自动同步覆盖回去，改了也留不住 —— 明说，别让人白改；
//  3. **每人上限 200 条**：这是给「存着备用」设计的，不是第二个灵感库；
//  4. 标题最多 100 字、正文最多 5000 字，正文不能为空。
//
// 数据形态：自建词没有分类（category_id 为 NULL）、没有 source_ref、
// 恒为 is_enabled —— 「下架」是管理员对共享目录的动作，自己的词直接删。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// 上限。改这里之前先想清楚：前端 i18n 里的 limitReached 文案和对接文档都写着这些数。
const (
	// MaxUserPromptsPerUser 每人最多存多少条。
	MaxUserPromptsPerUser = 200
	// MaxUserPromptTitleRunes 标题上限（按字数，不是字节 —— 运营写中文）。
	MaxUserPromptTitleRunes = 100
	// MaxUserPromptBodyRunes 正文上限。灵感库正文平均 1600 字，5000 给足余量。
	MaxUserPromptBodyRunes = 5000
)

// UserPromptRepository 是本服务需要的持久化能力（dkdomain.Repository 的子集）。
type UserPromptRepository interface {
	GetPromptByUID(ctx context.Context, uid string) (*dkdomain.Prompt, error)
	CreatePrompt(ctx context.Context, prompt *dkdomain.Prompt) error
	UpdatePrompt(ctx context.Context, prompt *dkdomain.Prompt) error
	DeletePrompt(ctx context.Context, ownerUserID int64, uid string) error
	CountPrompts(ctx context.Context, query dkdomain.ListPromptsQuery) (int, error)
}

// UserPromptServiceDeps 建服务要的东西。
type UserPromptServiceDeps struct {
	// Prompts 仓储，必填。
	Prompts UserPromptRepository
	// NewUID 生成 26 位 ULID。为 nil 用内置实现（asset.go 的 newAssetULID）。
	NewUID func() string
}

// UserPromptService 「我的提示词」的增改删。读取走灵感库现有的列表 / 详情
// （module.go 的 promptServiceAdapter 按 source + owner 过滤）。
type UserPromptService struct {
	prompts UserPromptRepository
	newUID  func() string
}

// NewUserPromptService 建服务。缺仓储直接报错，不延迟到运行时 nil panic。
func NewUserPromptService(deps UserPromptServiceDeps) (*UserPromptService, error) {
	if deps.Prompts == nil {
		return nil, errors.New("designkit: UserPromptService 缺少 Prompts 仓储")
	}
	newUID := deps.NewUID
	if newUID == nil {
		newUID = newAssetULID
	}
	return &UserPromptService{prompts: deps.Prompts, newUID: newUID}, nil
}

// normalizeUserPromptFields 清洗并校验标题、正文。两处（新建 / 修改）共用，
// 规则改一处就够。
func normalizeUserPromptFields(title, body string) (string, string, error) {
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)

	if len([]rune(title)) > MaxUserPromptTitleRunes {
		return "", "", dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessagef("标题最多 %d 个字。", MaxUserPromptTitleRunes)
	}
	if body == "" {
		return "", "", dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("提示词正文不能为空。")
	}
	if len([]rune(body)) > MaxUserPromptBodyRunes {
		return "", "", dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessagef("提示词正文最多 %d 个字。", MaxUserPromptBodyRunes)
	}
	return title, body, nil
}

// CreateMyPrompt 存一条自己的提示词。
//
// 上限检查和插入不在同一个事务里 —— 连点两下最多多存一条，这是「软上限」，
// 为它加锁不值得（超的那一条下次删掉就是了，钱一分不涉及）。
func (s *UserPromptService) CreateMyPrompt(ctx context.Context, userID int64, title, body string) (*dkdomain.Prompt, error) {
	if userID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUnauthorized)
	}
	title, body, err := normalizeUserPromptFields(title, body)
	if err != nil {
		return nil, err
	}

	source := dkdomain.PromptSourceUser
	owner := userID
	count, err := s.prompts.CountPrompts(ctx, dkdomain.ListPromptsQuery{
		Source:      &source,
		OwnerUserID: &owner,
	})
	if err != nil {
		return nil, err
	}
	if count >= MaxUserPromptsPerUser {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessagef("最多保存 %d 条，删几条再存。", MaxUserPromptsPerUser)
	}

	prompt := &dkdomain.Prompt{
		UID:   s.newUID(),
		Title: title,
		Body:  body,
		// 自建词没有分类、没有 source_ref；恒为可用（「下架」是共享目录的概念）。
		Source:      dkdomain.PromptSourceUser,
		OwnerUserID: &owner,
		IsEnabled:   true,
	}
	if err := s.prompts.CreatePrompt(ctx, prompt); err != nil {
		return nil, err
	}
	return prompt, nil
}

// UpdateMyPrompt 改一条自己的（只能改标题和正文）。
func (s *UserPromptService) UpdateMyPrompt(ctx context.Context, userID int64, uid, title, body string) (*dkdomain.Prompt, error) {
	prompt, err := s.loadOwnPrompt(ctx, userID, uid, "修改")
	if err != nil {
		return nil, err
	}
	title, body, err = normalizeUserPromptFields(title, body)
	if err != nil {
		return nil, err
	}

	prompt.Title = title
	prompt.Body = body
	// OwnerUserID 已经等于 userID（loadOwnPrompt 验过）。带着它调 UpdatePrompt，
	// repository 会把归属再钉进 WHERE 里 —— 双保险，防止这中间有人改了库。
	if err := s.prompts.UpdatePrompt(ctx, prompt); err != nil {
		return nil, err
	}
	return prompt, nil
}

// DeleteMyPrompt 删一条自己的（软删；历史任务里的快照不受影响）。
func (s *UserPromptService) DeleteMyPrompt(ctx context.Context, userID int64, uid string) error {
	if _, err := s.loadOwnPrompt(ctx, userID, uid, "删除"); err != nil {
		return err
	}
	return s.prompts.DeletePrompt(ctx, userID, uid)
}

// loadOwnPrompt 取一条并验「是自建的、而且是他自己的」。
//
// 两种拒绝刻意用不同的说法：
//   - youmind 来源 → 400 + 中文说明。这条词他**看得见**（共享目录），
//     报「找不到」他只会反复重试；
//   - 别人的自建词 → 「找不到」。他本来就**看不见**这条，
//     不能让报错替他确认「这个编号存在」。
func (s *UserPromptService) loadOwnPrompt(ctx context.Context, userID int64, uid, action string) (*dkdomain.Prompt, error) {
	if userID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUnauthorized)
	}
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil, fmt.Errorf("designkit: 提示词缺少编号: %w", dkdomain.ErrNotFound)
	}

	prompt, err := s.prompts.GetPromptByUID(ctx, uid)
	if err != nil {
		return nil, err
	}
	if prompt == nil {
		return nil, fmt.Errorf("designkit: 提示词 %s 不存在: %w", uid, dkdomain.ErrNotFound)
	}
	if prompt.Source != dkdomain.PromptSourceUser {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessagef("灵感库的提示词不能%s。要改内容，先「存为我的提示词」再改。", action)
	}
	if prompt.OwnerUserID == nil || *prompt.OwnerUserID != userID {
		return nil, fmt.Errorf("designkit: 提示词 %s 不属于用户 %d: %w", uid, userID, dkdomain.ErrNotFound)
	}
	return prompt, nil
}
