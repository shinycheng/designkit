//go:build unit

package service

import (
	"context"
	"strings"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// 我的提示词
// ============================================================================
//
// 这一组守四条底线：
//  1. 越权改 / 删别人的自建词一律「找不到」（不泄露编号存在）；
//  2. youmind 来源的词不可改删，而且报错是**看得懂的中文**（不是 404）；
//  3. 每人 200 条的上限拦得住；
//  4. 标题 / 正文超长、正文为空直接拒绝。

// fakeUserPromptRepo 只实现 UserPromptRepository。
type fakeUserPromptRepo struct {
	byUID map[string]*dkdomain.Prompt

	count    int
	countErr error

	created *dkdomain.Prompt
	updated *dkdomain.Prompt

	deletedOwner int64
	deletedUID   string
	deleteCalls  int
}

func (f *fakeUserPromptRepo) GetPromptByUID(_ context.Context, uid string) (*dkdomain.Prompt, error) {
	p, ok := f.byUID[uid]
	if !ok {
		return nil, dkdomain.ErrNotFound
	}
	clone := *p
	return &clone, nil
}

func (f *fakeUserPromptRepo) CreatePrompt(_ context.Context, prompt *dkdomain.Prompt) error {
	clone := *prompt
	f.created = &clone
	return nil
}

func (f *fakeUserPromptRepo) UpdatePrompt(_ context.Context, prompt *dkdomain.Prompt) error {
	clone := *prompt
	f.updated = &clone
	return nil
}

func (f *fakeUserPromptRepo) DeletePrompt(_ context.Context, ownerUserID int64, uid string) error {
	f.deleteCalls++
	f.deletedOwner = ownerUserID
	f.deletedUID = uid
	return nil
}

func (f *fakeUserPromptRepo) CountPrompts(_ context.Context, _ dkdomain.ListPromptsQuery) (int, error) {
	return f.count, f.countErr
}

func ownedPrompt(uid string, owner int64) *dkdomain.Prompt {
	return &dkdomain.Prompt{
		ID:          11,
		UID:         uid,
		Title:       "旧标题",
		Body:        "旧正文",
		Source:      dkdomain.PromptSourceUser,
		OwnerUserID: &owner,
		IsEnabled:   true,
	}
}

func newUserPromptService(t *testing.T, repo *fakeUserPromptRepo) *UserPromptService {
	t.Helper()
	svc, err := NewUserPromptService(UserPromptServiceDeps{Prompts: repo})
	require.NoError(t, err)
	return svc
}

// ---- 新建 ----

func TestCreateMyPromptFillsUserSourceAndOwner(t *testing.T) {
	repo := &fakeUserPromptRepo{}
	svc := newUserPromptService(t, repo)

	p, err := svc.CreateMyPrompt(context.Background(), 7, "  白底主图  ", "  纯白背景，柔和顶光  ")
	require.NoError(t, err)
	require.NotNil(t, repo.created)

	assert.Equal(t, dkdomain.PromptSourceUser, p.Source, "自建词的来源必须是 user")
	require.NotNil(t, p.OwnerUserID)
	assert.Equal(t, int64(7), *p.OwnerUserID, "归属必须是当前用户")
	assert.Equal(t, "白底主图", p.Title, "首尾空白要清掉")
	assert.Equal(t, "纯白背景，柔和顶光", p.Body)
	assert.True(t, p.IsEnabled)
	assert.Nil(t, p.CategoryID, "自建词没有分类")
	assert.NotEmpty(t, p.UID, "对外编号必须生成好")
}

// 上限拦截：第 201 条要被拒绝，而且报错里写着上限数和下一步（删几条再存）。
func TestCreateMyPromptRejectsWhenLimitReached(t *testing.T) {
	repo := &fakeUserPromptRepo{count: MaxUserPromptsPerUser}
	svc := newUserPromptService(t, repo)

	_, err := svc.CreateMyPrompt(context.Background(), 7, "t", "b")
	require.Error(t, err)

	dkErr, ok := dkdomain.AsDesignkitError(err)
	require.True(t, ok, "要给我方错误码，不能是裸 error")
	assert.Equal(t, dkdomain.ErrCodeInvalidRequest, dkErr.Code)
	assert.Contains(t, dkErr.Message, "200", "上限数要写进文案")
	assert.Contains(t, dkErr.Message, "删", "要说下一步怎么办")
	assert.Nil(t, repo.created, "拦下来就不该写库")
}

// 差一条没到上限时照常能存。
func TestCreateMyPromptAllowsUnderLimit(t *testing.T) {
	repo := &fakeUserPromptRepo{count: MaxUserPromptsPerUser - 1}
	svc := newUserPromptService(t, repo)

	_, err := svc.CreateMyPrompt(context.Background(), 7, "t", "b")
	require.NoError(t, err)
	require.NotNil(t, repo.created)
}

func TestCreateMyPromptValidatesFields(t *testing.T) {
	repo := &fakeUserPromptRepo{}
	svc := newUserPromptService(t, repo)

	cases := []struct {
		name  string
		title string
		body  string
	}{
		{"正文为空", "标题", "   "},
		{"标题超长", strings.Repeat("题", MaxUserPromptTitleRunes+1), "正文"},
		{"正文超长", "标题", strings.Repeat("文", MaxUserPromptBodyRunes+1)},
	}
	for _, tc := range cases {
		_, err := svc.CreateMyPrompt(context.Background(), 7, tc.title, tc.body)
		require.Error(t, err, tc.name)

		dkErr, ok := dkdomain.AsDesignkitError(err)
		require.True(t, ok, tc.name)
		assert.Equal(t, dkdomain.ErrCodeInvalidRequest, dkErr.Code, tc.name)
	}
	assert.Nil(t, repo.created)
}

// 标题可以为空（灵感库里也有大量只有正文的词，卡片会拿正文开头顶上）。
func TestCreateMyPromptAllowsEmptyTitle(t *testing.T) {
	repo := &fakeUserPromptRepo{}
	svc := newUserPromptService(t, repo)

	p, err := svc.CreateMyPrompt(context.Background(), 7, "", "正文")
	require.NoError(t, err)
	assert.Empty(t, p.Title)
}

// ---- 修改 ----

func TestUpdateMyPromptRewritesTitleAndBodyOnly(t *testing.T) {
	repo := &fakeUserPromptRepo{byUID: map[string]*dkdomain.Prompt{
		"U1": ownedPrompt("U1", 7),
	}}
	svc := newUserPromptService(t, repo)

	p, err := svc.UpdateMyPrompt(context.Background(), 7, "U1", "新标题", "新正文")
	require.NoError(t, err)
	require.NotNil(t, repo.updated)

	assert.Equal(t, "新标题", p.Title)
	assert.Equal(t, "新正文", p.Body)
	require.NotNil(t, repo.updated.OwnerUserID, "必须带归属去更新，repository 靠它做 WHERE 守卫")
	assert.Equal(t, int64(7), *repo.updated.OwnerUserID)
	assert.Equal(t, dkdomain.PromptSourceUser, repo.updated.Source)
}

// 越权：别人的自建词一律「找不到」，不泄露这条编号存在。
func TestUpdateMyPromptRejectsOthersPromptAsNotFound(t *testing.T) {
	repo := &fakeUserPromptRepo{byUID: map[string]*dkdomain.Prompt{
		"U1": ownedPrompt("U1", 99),
	}}
	svc := newUserPromptService(t, repo)

	_, err := svc.UpdateMyPrompt(context.Background(), 7, "U1", "t", "b")
	require.Error(t, err)
	assert.ErrorIs(t, err, dkdomain.ErrNotFound, "越权必须是「找不到」，不能是 403")
	assert.Nil(t, repo.updated, "越权时绝不能写库")
}

// youmind 来源不可改：这条词他看得见（共享目录），要给中文说明而不是 404。
func TestUpdateMyPromptRejectsYouMindPrompt(t *testing.T) {
	repo := &fakeUserPromptRepo{byUID: map[string]*dkdomain.Prompt{
		"Y1": {UID: "Y1", Title: "共享词", Body: "b", Source: dkdomain.PromptSourceYouMind},
	}}
	svc := newUserPromptService(t, repo)

	_, err := svc.UpdateMyPrompt(context.Background(), 7, "Y1", "t", "b")
	require.Error(t, err)

	dkErr, ok := dkdomain.AsDesignkitError(err)
	require.True(t, ok)
	assert.Equal(t, dkdomain.ErrCodeInvalidRequest, dkErr.Code, "看得见的词不能报「找不到」")
	assert.Contains(t, dkErr.Message, "灵感库", "要说清为什么不能改")
	assert.Nil(t, repo.updated)
}

// ---- 删除 ----

func TestDeleteMyPromptPassesOwnerGuard(t *testing.T) {
	repo := &fakeUserPromptRepo{byUID: map[string]*dkdomain.Prompt{
		"U1": ownedPrompt("U1", 7),
	}}
	svc := newUserPromptService(t, repo)

	require.NoError(t, svc.DeleteMyPrompt(context.Background(), 7, "U1"))
	assert.Equal(t, 1, repo.deleteCalls)
	assert.Equal(t, int64(7), repo.deletedOwner, "repository 的软删 WHERE 里要带归属")
	assert.Equal(t, "U1", repo.deletedUID)
}

func TestDeleteMyPromptRejectsOthersPromptAsNotFound(t *testing.T) {
	repo := &fakeUserPromptRepo{byUID: map[string]*dkdomain.Prompt{
		"U1": ownedPrompt("U1", 99),
	}}
	svc := newUserPromptService(t, repo)

	err := svc.DeleteMyPrompt(context.Background(), 7, "U1")
	require.Error(t, err)
	assert.ErrorIs(t, err, dkdomain.ErrNotFound)
	assert.Zero(t, repo.deleteCalls, "越权时绝不能删")
}

func TestDeleteMyPromptRejectsYouMindPrompt(t *testing.T) {
	repo := &fakeUserPromptRepo{byUID: map[string]*dkdomain.Prompt{
		"Y1": {UID: "Y1", Body: "b", Source: dkdomain.PromptSourceYouMind},
	}}
	svc := newUserPromptService(t, repo)

	err := svc.DeleteMyPrompt(context.Background(), 7, "Y1")
	require.Error(t, err)

	dkErr, ok := dkdomain.AsDesignkitError(err)
	require.True(t, ok)
	assert.Equal(t, dkdomain.ErrCodeInvalidRequest, dkErr.Code)
	assert.Zero(t, repo.deleteCalls, "同步来的词绝不能删")
}

// 不存在的编号：原样透出「找不到」。
func TestUpdateMyPromptUnknownUID(t *testing.T) {
	svc := newUserPromptService(t, &fakeUserPromptRepo{})

	_, err := svc.UpdateMyPrompt(context.Background(), 7, "NOPE", "t", "b")
	assert.ErrorIs(t, err, dkdomain.ErrNotFound)
}

// 缺登录人（装配被改坏）时明确拒绝，不静默按 0 号用户处理。
func TestUserPromptRequiresUserID(t *testing.T) {
	svc := newUserPromptService(t, &fakeUserPromptRepo{})

	_, err := svc.CreateMyPrompt(context.Background(), 0, "t", "b")
	require.Error(t, err)
	dkErr, ok := dkdomain.AsDesignkitError(err)
	require.True(t, ok)
	assert.Equal(t, dkdomain.ErrCodeUnauthorized, dkErr.Code)
}
