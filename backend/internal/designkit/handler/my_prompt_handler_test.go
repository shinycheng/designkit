//go:build unit

package handler

import (
	"context"
	"net/http"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	upstreammw "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// 我的提示词
// ============================================================================
//
// 这一组守四条底线：
//  1. 三条写端点都传对 userID（归属规则全在 service 侧，handler 传错人一切白搭）；
//  2. 服务缺席时整组不挂（裸 404 = 功能没上线，跟 suggest 同一套前端约定）；
//  3. 列表的 source 参数只认 "" 和 "user"，且 viewer 一定带到 service；
//  4. service 的 ErrNotFound（越权 / 不存在）翻成 DK_PROMPT_NOT_FOUND 404。

// ---- 假实现 ----

type fakeMyPromptService struct {
	created *dkdomain.Prompt
	updated *dkdomain.Prompt

	createErr error
	updateErr error
	deleteErr error

	lastUserID int64
	lastUID    string
	lastTitle  string
	lastBody   string

	createCalls int
	updateCalls int
	deleteCalls int
}

func (f *fakeMyPromptService) CreateMyPrompt(_ context.Context, userID int64, title, body string) (*dkdomain.Prompt, error) {
	f.createCalls++
	f.lastUserID = userID
	f.lastTitle = title
	f.lastBody = body
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.created, nil
}

func (f *fakeMyPromptService) UpdateMyPrompt(_ context.Context, userID int64, uid, title, body string) (*dkdomain.Prompt, error) {
	f.updateCalls++
	f.lastUserID = userID
	f.lastUID = uid
	f.lastTitle = title
	f.lastBody = body
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return f.updated, nil
}

func (f *fakeMyPromptService) DeleteMyPrompt(_ context.Context, userID int64, uid string) error {
	f.deleteCalls++
	f.lastUserID = userID
	f.lastUID = uid
	return f.deleteErr
}

func testUserPrompt(uid string) *dkdomain.Prompt {
	owner := testUserID
	return &dkdomain.Prompt{
		ID:          21,
		UID:         uid,
		Title:       "我的白底词",
		Body:        "纯白背景，柔和顶光，产品居中",
		Source:      dkdomain.PromptSourceUser,
		OwnerUserID: &owner,
		IsEnabled:   true,
		CreatedAt:   testTime(),
		UpdatedAt:   testTime(),
	}
}

// newMyPromptEngine 搭引擎：灵感库浏览 + 我的提示词写入。
func newMyPromptEngine(t *testing.T, prompts PromptService, my MyPromptService) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	r := gin.New()
	v1 := r.Group("/api/v1")
	browser := v1.Group("/designkit")
	browser.Use(fakeAuthWithRole(testUserID, upstreamservice.RoleUser))
	machine := r.Group("/v1/designkit")

	services := testServices(&fakeJobService{})
	services.Prompts = prompts
	services.MyPrompts = my

	RegisterBusinessRoutes(BusinessRouteOptions{
		Browser:    browser,
		Machine:    machine,
		Services:   services,
		KeyAuth:    fakeAuthWithRole(testUserID, upstreamservice.RoleUser),
		APIKeyAuth: upstreammw.APIKeyAuthMiddleware(fakeAuthWithRole(testUserID, upstreamservice.RoleUser)),
	})
	return r
}

// ---- 挂载 ----

// 服务缺席时三条写路由整组不挂：裸 404（没有错误信封），
// 前端据此显示「还没准备好」，浏览照常。
func TestMyPromptRoutesAbsentWithoutService(t *testing.T) {
	engine := newMyPromptEngine(t, &fakePromptService{page: &PromptPage{}}, nil)

	browse := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/prompts", "", nil)
	require.Equal(t, http.StatusOK, browse.Code, "写服务缺席不该影响浏览")

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/designkit/prompts"},
		{http.MethodPut, "/api/v1/designkit/prompts/U1"},
		{http.MethodDelete, "/api/v1/designkit/prompts/U1"},
	}
	for _, tc := range cases {
		rec := doRequest(t, engine, tc.method, tc.path, `{"title":"t","body":"b"}`, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s 应该是裸 404", tc.method, tc.path)
		assert.NotContains(t, rec.Body.String(), "DK_",
			"缺席 = 裸 404，不能带 DK_ 错误码（前端靠这个区分「没上线」和「业务错误」）")
	}
}

// ---- 新建 ----

func TestCreateMyPromptPassesUserAndReturnsDTO(t *testing.T) {
	my := &fakeMyPromptService{created: testUserPrompt("01J8ZK7Q9X2M4N6P8R0T2V4W6Y")}
	engine := newMyPromptEngine(t, &fakePromptService{}, my)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/prompts",
		`{"title":"我的白底词","body":"纯白背景，柔和顶光，产品居中"}`, nil)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, testUserID, my.lastUserID, "必须记在当前登录用户名下")
	assert.Equal(t, "我的白底词", my.lastTitle)
	assert.Equal(t, "纯白背景，柔和顶光，产品居中", my.lastBody)

	body := decodeJSON(t, rec.Body.Bytes())
	assert.Equal(t, "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", body["uid"])
	assert.Equal(t, "user", body["source"], "自建词的来源必须回 user，前端靠它认「我的」")
	assert.Equal(t, "", body["category_slug"], "自建词没有分类")
	assertNoCodeKey(t, body)
}

func TestCreateMyPromptRejectsBrokenJSON(t *testing.T) {
	my := &fakeMyPromptService{}
	engine := newMyPromptEngine(t, &fakePromptService{}, my)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/prompts", `{broken`, nil)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
	assert.Zero(t, my.createCalls)
}

// 上限这类业务拒绝由 service 给出（带中文文案的 DK_INVALID_REQUEST），原样透出。
func TestCreateMyPromptPropagatesServiceRejection(t *testing.T) {
	my := &fakeMyPromptService{createErr: dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
		WithMessage("最多保存 200 条，删几条再存。")}
	engine := newMyPromptEngine(t, &fakePromptService{}, my)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/prompts",
		`{"title":"","body":"b"}`, nil)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
	message, _ := errorObject(t, rec.Body.Bytes())["message"].(string)
	assert.Contains(t, message, "200", "上限文案要原样到达界面")
}

// ---- 修改 / 删除 ----

func TestUpdateMyPromptPassesThrough(t *testing.T) {
	my := &fakeMyPromptService{updated: testUserPrompt("01J8ZK7Q9X2M4N6P8R0T2V4W6Y")}
	engine := newMyPromptEngine(t, &fakePromptService{}, my)

	rec := doRequest(t, engine, http.MethodPut,
		"/api/v1/designkit/prompts/01J8ZK7Q9X2M4N6P8R0T2V4W6Y",
		`{"title":"新标题","body":"新正文"}`, nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, testUserID, my.lastUserID)
	assert.Equal(t, "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", my.lastUID)
	assert.Equal(t, "新标题", my.lastTitle)
	assert.Equal(t, "新正文", my.lastBody)
}

// 越权 / 不存在：service 回 ErrNotFound，对外必须是 DK_PROMPT_NOT_FOUND 404。
func TestUpdateMyPromptNotFoundIsChinese404(t *testing.T) {
	my := &fakeMyPromptService{updateErr: dkdomain.ErrNotFound}
	engine := newMyPromptEngine(t, &fakePromptService{}, my)

	rec := doRequest(t, engine, http.MethodPut, "/api/v1/designkit/prompts/NOPE",
		`{"title":"t","body":"b"}`, nil)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodePromptNotFound)
}

func TestDeleteMyPromptPassesThrough(t *testing.T) {
	my := &fakeMyPromptService{}
	engine := newMyPromptEngine(t, &fakePromptService{}, my)

	rec := doRequest(t, engine, http.MethodDelete,
		"/api/v1/designkit/prompts/01J8ZK7Q9X2M4N6P8R0T2V4W6Y", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, my.deleteCalls)
	assert.Equal(t, testUserID, my.lastUserID)
	assert.Equal(t, "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", my.lastUID)

	body := decodeJSON(t, rec.Body.Bytes())
	assert.Equal(t, true, body["ok"], "跟 DELETE /chat/sessions/:uid 一个形状")
}

// youmind 来源：service 给 DK_INVALID_REQUEST（不是 404），原样透出 ——
// 那条词他看得见，报「找不到」他只会反复重试。
func TestDeleteMyPromptYouMindRejectionIsExplained(t *testing.T) {
	my := &fakeMyPromptService{deleteErr: dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
		WithMessage("灵感库的提示词不能删除。要改内容，先「存为我的提示词」再改。")}
	engine := newMyPromptEngine(t, &fakePromptService{}, my)

	rec := doRequest(t, engine, http.MethodDelete, "/api/v1/designkit/prompts/Y1", "", nil)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
	message, _ := errorObject(t, rec.Body.Bytes())["message"].(string)
	assert.Contains(t, message, "灵感库")
}

// ---- 列表的 source 参数 ----

// source=user：过滤条件和当前用户都要带到 service。
func TestPromptListMineSourcePassesViewer(t *testing.T) {
	prompts := &fakePromptService{page: &PromptPage{}}
	engine := newMyPromptEngine(t, prompts, &fakeMyPromptService{})

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/prompts?source=user", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, ListPromptSourceMine, prompts.lastList.Source)
	assert.Equal(t, testUserID, prompts.lastList.ViewerUserID,
		"少了 viewer，「我的提示词」就成了「所有人的提示词」")
}

// 不带 source = 共享目录，viewer 照样带（详情那条也要用）。
func TestPromptListDefaultSourceIsShared(t *testing.T) {
	prompts := &fakePromptService{page: &PromptPage{}}
	engine := newMyPromptEngine(t, prompts, &fakeMyPromptService{})

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/prompts", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, prompts.lastList.Source)
	assert.Equal(t, testUserID, prompts.lastList.ViewerUserID)
}

// source 拼错直接 400，不静默当成空 —— 静默的表现是「我的提示词永远是空的」。
func TestPromptListRejectsUnknownSource(t *testing.T) {
	prompts := &fakePromptService{page: &PromptPage{}}
	engine := newMyPromptEngine(t, prompts, &fakeMyPromptService{})

	for _, source := range []string{"mine", "youmind2", "USER"} {
		rec := doRequest(t, engine, http.MethodGet,
			"/api/v1/designkit/prompts?source="+source, "", nil)

		require.Equal(t, http.StatusBadRequest, rec.Code, "source=%s：%s", source, rec.Body.String())
		assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
	}
}
