//go:build unit

package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	upstreammw "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// 灵感库
// ============================================================================
//
// 这一组守四条底线：
//  1. 浏览端点对所有登录用户开放，同步那四个**只有管理员**能进，且 403 是中文的；
//  2. 同步是长任务 —— POST 立刻回 202，不挂着等；
//  3. 代理下拉框的字段名跟上游 ProxySelector 对得上，而且**永远不带密码**；
//  4. 对外 JSON 任何层级都不出现名为 code 的字段。

// ---- 假实现 ----

type fakePromptService struct {
	categories    []*PromptCategoryView
	categoriesErr error

	page     *PromptPage
	listErr  error
	lastList ListPromptsInput

	view       *PromptView
	getErr     error
	lastGetUID string
}

func (f *fakePromptService) ListCategories(_ context.Context) ([]*PromptCategoryView, error) {
	if f.categoriesErr != nil {
		return nil, f.categoriesErr
	}
	return f.categories, nil
}

func (f *fakePromptService) ListPrompts(_ context.Context, in ListPromptsInput) (*PromptPage, error) {
	f.lastList = in
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.page, nil
}

func (f *fakePromptService) GetPrompt(_ context.Context, uid string) (*PromptView, error) {
	f.lastGetUID = uid
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.view, nil
}

type fakePromptSyncService struct {
	run       *dkdomain.SyncRun
	startErr  error
	startCall int

	status    *SyncStatusView
	statusErr error

	settings    *SyncSettings
	settingsErr error

	putErr     error
	putCall    int
	putProxyID *int64
	putSawNil  bool
}

func (f *fakePromptSyncService) StartSync(_ context.Context) (*dkdomain.SyncRun, error) {
	f.startCall++
	if f.startErr != nil {
		return nil, f.startErr
	}
	return f.run, nil
}

func (f *fakePromptSyncService) SyncStatus(_ context.Context) (*SyncStatusView, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return f.status, nil
}

func (f *fakePromptSyncService) GetSyncSettings(_ context.Context) (*SyncSettings, error) {
	if f.settingsErr != nil {
		return nil, f.settingsErr
	}
	return f.settings, nil
}

func (f *fakePromptSyncService) PutSyncSettings(_ context.Context, proxyID *int64) (*SyncSettings, error) {
	f.putCall++
	f.putProxyID = proxyID
	f.putSawNil = proxyID == nil
	if f.putErr != nil {
		return nil, f.putErr
	}
	if f.settings != nil {
		f.settings.ProxyID = proxyID
	}
	return f.settings, nil
}

// ---- 造数据 ----

func testCategories() []*PromptCategoryView {
	return []*PromptCategoryView{
		{Category: &dkdomain.PromptCategory{ID: 1, Slug: "product-shot", NameZH: "商品实拍", NameEN: "Product Shot", SortOrder: 1}, PromptCount: 4200},
		// 中文名空着时要退到英文名，界面上绝不能留一行空白让运营点不中。
		{Category: &dkdomain.PromptCategory{ID: 2, Slug: "lifestyle", NameEN: "Lifestyle", SortOrder: 2}, PromptCount: 1500},
	}
}

func testPromptView(id int64, uid string) *PromptView {
	return &PromptView{
		Prompt: &dkdomain.Prompt{
			ID:    id,
			UID:   uid,
			Title: "纯白背景棚拍",
			Body:  "在纯白背景上拍摄 {品类}，柔光，正面视角",
			Variables: []dkdomain.PromptVariable{
				{Name: "品类", Example: "连衣裙"},
			},
			Source:    dkdomain.PromptSourceYouMind,
			IsEnabled: true,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
		CategorySlug: "product-shot",
		CategoryName: "商品实拍",
	}
}

func testSyncRun(status dkdomain.SyncStatus) *dkdomain.SyncRun {
	run := &dkdomain.SyncRun{
		ID:        9,
		Kind:      dkdomain.SyncKindManual,
		Status:    status,
		StartedAt: testTime(),
		Fetched:   14700,
		Inserted:  12,
		Updated:   3,
		Skipped:   14685,
	}
	if status != dkdomain.SyncStatusRunning {
		finished := testTime().Add(16 * time.Second)
		run.FinishedAt = &finished
	}
	return run
}

func testProxyOption() ProxyOption {
	return ProxyOption{
		ID:        3,
		Name:      "香港节点",
		Protocol:  "http",
		Host:      "10.0.0.9",
		Port:      8080,
		Username:  "dk",
		Status:    "active",
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}
}

// ---- 引擎 ----

// fakeAuthWithRole 模拟上游 jwtAuth：塞 AuthSubject **和角色**。
// 角色是 RequireAdmin 的唯一判据（跟上游 middleware.AdminOnly 同一个键）。
func fakeAuthWithRole(userID int64, role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if userID > 0 {
			c.Set(string(upstreammw.ContextKeyUser), upstreammw.AuthSubject{UserID: userID})
			if role != "" {
				c.Set(string(upstreammw.ContextKeyUserRole), role)
			}
		}
		c.Next()
	}
}

// newPromptEngine 按 RegisterBusinessRoutes 的真实结构搭引擎，并指定登录者的角色。
func newPromptEngine(t *testing.T, prompts PromptService, sync PromptSyncService, role string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	r := gin.New()
	v1 := r.Group("/api/v1")
	browser := v1.Group("/designkit")
	browser.Use(fakeAuthWithRole(testUserID, role))
	machine := r.Group("/v1/designkit")

	services := testServices(&fakeJobService{})
	services.Prompts = prompts
	services.PromptSync = sync

	RegisterBusinessRoutes(BusinessRouteOptions{
		Browser:    browser,
		Machine:    machine,
		Services:   services,
		KeyAuth:    fakeAuthWithRole(testUserID, role),
		APIKeyAuth: upstreammw.APIKeyAuthMiddleware(fakeAuthWithRole(testUserID, role)),
	})
	return r
}

// ============================================================================
// 浏览
// ============================================================================

// 分类列表：顺序照搬 service，**不返回自增主键**，中文名空着时退到英文名。
func TestPromptCategoriesShapeAndFallbackName(t *testing.T) {
	prompts := &fakePromptService{categories: testCategories()}
	engine := newPromptEngine(t, prompts, nil, upstreamservice.RoleUser)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/prompt-categories", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := decodeJSON(t, rec.Body.Bytes())

	assert.Equal(t, float64(5700), body["total_prompt_count"], "界面上「全部（N）」那个数")

	items, ok := body["categories"].([]any)
	require.True(t, ok, "categories 必须是数组：%s", rec.Body.String())
	require.Len(t, items, 2)

	first, _ := items[0].(map[string]any)
	assert.Equal(t, "product-shot", first["slug"], "对外标识是 slug，不是自增主键")
	assert.Equal(t, "商品实拍", first["name"])
	assert.Equal(t, float64(4200), first["prompt_count"])
	_, hasID := first["id"]
	assert.False(t, hasID, "分类的自增主键对外一律不暴露")

	second, _ := items[1].(map[string]any)
	assert.Equal(t, "Lifestyle", second["name"], "中文名空着时要退到英文名，不能是空串")

	assertNoCodeKey(t, body)
}

// 检索：查询参数原样传给 service，游标能来回转，两个挂载前缀都通。
func TestPromptListPassesFiltersAndRoundTripsCursor(t *testing.T) {
	prompts := &fakePromptService{page: &PromptPage{
		Items:        []*PromptView{testPromptView(11, "01J8ZK7Q9X2M4N6P8R0T2V4W6Y")},
		Total:        14700,
		HasMore:      true,
		NextCursorID: 11,
	}}
	engine := newPromptEngine(t, prompts, nil, upstreamservice.RoleUser)

	for _, prefix := range []string{"/api/v1/designkit", "/v1/designkit"} {
		rec := doRequest(t, engine, http.MethodGet,
			prefix+"/prompts?category=product-shot&keyword=%E8%BF%9E%E8%A1%A3%E8%A3%99&limit=5", "", nil)

		require.Equal(t, http.StatusOK, rec.Code, "前缀 %s，响应体：%s", prefix, rec.Body.String())

		assert.Equal(t, "product-shot", prompts.lastList.CategorySlug)
		assert.Equal(t, "连衣裙", prompts.lastList.Keyword)
		assert.Equal(t, 5, prompts.lastList.Limit)
		assert.Zero(t, prompts.lastList.CursorID, "没带游标就是第一页")

		body := decodeJSON(t, rec.Body.Bytes())
		assert.Equal(t, float64(14700), body["total"], "一万四千条，不告诉总数运营不知道该搜还是该翻")
		assert.Equal(t, true, body["has_more"])

		cursor, ok := body["next_cursor"].(string)
		require.True(t, ok, "还有下一页时必须给游标：%s", rec.Body.String())

		// 把游标原样送回去，service 应该收到那一页最后一行的 id。
		rec2 := doRequest(t, engine, http.MethodGet, prefix+"/prompts?cursor="+cursor, "", nil)
		require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
		assert.Equal(t, int64(11), prompts.lastList.CursorID, "游标要能来回转")

		assertNoCodeKey(t, body)
	}
}

// 一条提示词的形状：正文完整、变量是数组不是 null。
func TestPromptDetailKeepsFullBodyAndVariables(t *testing.T) {
	view := testPromptView(11, "01J8ZK7Q9X2M4N6P8R0T2V4W6Y")
	prompts := &fakePromptService{view: view}
	engine := newPromptEngine(t, prompts, nil, upstreamservice.RoleUser)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/prompts/01J8ZK7Q9X2M4N6P8R0T2V4W6Y", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", prompts.lastGetUID)

	body := decodeJSON(t, rec.Body.Bytes())
	assert.Equal(t, view.Prompt.Body, body["body"], "正文一个字都不能少：运营点「用这条」带走的就是它")
	assert.Equal(t, "product-shot", body["category_slug"])
	assert.Equal(t, "商品实拍", body["category_name"])

	vars, ok := body["variables"].([]any)
	require.True(t, ok, "variables 必须是数组（哪怕是空的），前端要直接 v-for")
	require.Len(t, vars, 1)
	first, _ := vars[0].(map[string]any)
	assert.Equal(t, "品类", first["name"])
	assert.Equal(t, "连衣裙", first["example"])

	assertNoCodeKey(t, body)
}

// 没有变量时也要给空数组，不能给 null。
func TestPromptVariablesAreNeverNull(t *testing.T) {
	view := testPromptView(11, "01J8ZK7Q9X2M4N6P8R0T2V4W6Y")
	view.Prompt.Variables = nil
	prompts := &fakePromptService{view: view}
	engine := newPromptEngine(t, prompts, nil, upstreamservice.RoleUser)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/prompts/01J8ZK7Q9X2M4N6P8R0T2V4W6Y", "", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"variables":[]`)
}

func TestPromptNotFoundReturnsChinese404(t *testing.T) {
	prompts := &fakePromptService{getErr: dkdomain.ErrNotFound}
	engine := newPromptEngine(t, prompts, nil, upstreamservice.RoleUser)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/prompts/nope", "", nil)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodePromptNotFound)
}

func TestPromptListRejectsBrokenCursor(t *testing.T) {
	prompts := &fakePromptService{page: &PromptPage{}}
	engine := newPromptEngine(t, prompts, nil, upstreamservice.RoleUser)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/prompts?cursor=!!!", "", nil)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
}

// 别的列表的游标粘到这里必须被认出来，而不是解析成一个巨大的 id 后给一页空列表 ——
// 空列表不报错，运营只会以为灵感库坏了。
func TestPromptListRejectsForeignCursor(t *testing.T) {
	prompts := &fakePromptService{page: &PromptPage{}}
	engine := newPromptEngine(t, prompts, nil, upstreamservice.RoleUser)

	foreign := encodeCursor(testTime(), 42)
	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/prompts?cursor="+foreign, "", nil)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
}

// 关键词太长直接拒绝，**不静默截断** —— 截断之后搜出来的是另一个词的结果。
func TestPromptListRejectsOverlongKeyword(t *testing.T) {
	prompts := &fakePromptService{page: &PromptPage{}}
	engine := newPromptEngine(t, prompts, nil, upstreamservice.RoleUser)

	long := strings.Repeat("裙", maxPromptKeywordRunes+1)
	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/prompts?keyword="+long, "", nil)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
}

// ============================================================================
// 同步（仅管理员）
// ============================================================================

// 非管理员碰这四个端点一律 403，而且**必须是中文**
// （上游那句是 "Admin access required"，运营看不懂）。
func TestPromptSyncEndpointsRejectNonAdmin(t *testing.T) {
	sync := &fakePromptSyncService{run: testSyncRun(dkdomain.SyncStatusRunning)}
	engine := newPromptEngine(t, &fakePromptService{}, sync, upstreamservice.RoleUser)

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/designkit/prompts/sync", ""},
		{http.MethodGet, "/api/v1/designkit/prompts/sync/latest", ""},
		{http.MethodGet, "/api/v1/designkit/prompts/sync/settings", ""},
		{http.MethodPut, "/api/v1/designkit/prompts/sync/settings", `{"proxy_id":null}`},
	}
	for _, tc := range cases {
		rec := doRequest(t, engine, tc.method, tc.path, tc.body, nil)

		require.Equal(t, http.StatusForbidden, rec.Code, "%s %s：%s", tc.method, tc.path, rec.Body.String())
		assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeForbidden)

		message, _ := errorObject(t, rec.Body.Bytes())["message"].(string)
		assert.NotContains(t, message, "Admin", "403 的文案必须是中文")
		assert.Contains(t, message, "管理员")
	}
	assert.Zero(t, sync.startCall, "非管理员绝不能真的触发一次同步")
	assert.Zero(t, sync.putCall, "非管理员绝不能改同步设置")
}

// 登录了但上下文里没有角色（鉴权中间件没塞）时，按**非管理员**处理。
func TestPromptSyncRejectsMissingRole(t *testing.T) {
	sync := &fakePromptSyncService{run: testSyncRun(dkdomain.SyncStatusRunning)}
	engine := newPromptEngine(t, &fakePromptService{}, sync, "")

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/prompts/sync", "", nil)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Zero(t, sync.startCall)
}

// 同步是长任务：POST **立刻**回 202，并给出可以轮询的地址。
func TestStartSyncReturnsAcceptedImmediately(t *testing.T) {
	sync := &fakePromptSyncService{run: testSyncRun(dkdomain.SyncStatusRunning)}
	engine := newPromptEngine(t, &fakePromptService{}, sync, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/prompts/sync", "", nil)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, 1, sync.startCall)

	body := decodeJSON(t, rec.Body.Bytes())
	assert.Equal(t, true, body["accepted"])
	assert.Equal(t, "/api/v1/designkit/prompts/sync/latest", body["poll_url"], "前端靠它轮询进度")
	assert.NotEmpty(t, body["message"], "要有一句中文告诉管理员现在在干什么")

	run, ok := body["sync"].(map[string]any)
	require.True(t, ok, "要把刚建好的那条记录带回去：%s", rec.Body.String())
	assert.Equal(t, "running", run["status"])
	assert.Equal(t, "manual", run["kind"])

	assertNoCodeKey(t, body)
}

// 已经有一次在跑时返回 409，文案是中文的「等这一次跑完再点」。
func TestStartSyncWhenAlreadyRunningReturns409(t *testing.T) {
	sync := &fakePromptSyncService{startErr: dkdomain.NewError(dkdomain.ErrCodeSyncInProgress)}
	engine := newPromptEngine(t, &fakePromptService{}, sync, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/prompts/sync", "", nil)

	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeSyncInProgress)
}

// 同步状态：running 决定前端要不要继续轮询；失败原因不能叫 error（会跟顶层错误对象撞名）。
func TestSyncLatestShape(t *testing.T) {
	reason := "拉取超时"
	failed := testSyncRun(dkdomain.SyncStatusFailed)
	failed.Error = &reason

	sync := &fakePromptSyncService{status: &SyncStatusView{
		Latest:        failed,
		Running:       false,
		PromptCount:   14700,
		CategoryCount: 11,
	}}
	engine := newPromptEngine(t, &fakePromptService{}, sync, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/prompts/sync/latest", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := decodeJSON(t, rec.Body.Bytes())

	assert.Equal(t, false, body["running"])
	assert.Equal(t, float64(14700), body["prompt_count"])
	assert.Equal(t, float64(11), body["category_count"])
	assert.NotEmpty(t, body["message"], "管理员要看的是一句中文，不是几个数字")

	run, ok := body["sync"].(map[string]any)
	require.True(t, ok, rec.Body.String())
	assert.Equal(t, "failed", run["status"])
	assert.Equal(t, reason, run["error_message"], "失败原因叫 error_message，不叫 error")
	_, hasError := run["error"]
	assert.False(t, hasError, "叫 error 会跟错误响应的顶层 error 对象撞名")
	assert.Equal(t, float64(16), run["duration_seconds"])

	assertNoCodeKey(t, body)
}

// 一次都没同步过时不能报错：全新部署本来就是这样。
func TestSyncLatestWithoutAnyRun(t *testing.T) {
	sync := &fakePromptSyncService{status: &SyncStatusView{}}
	engine := newPromptEngine(t, &fakePromptService{}, sync, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/prompts/sync/latest", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := decodeJSON(t, rec.Body.Bytes())
	assert.Nil(t, body["sync"])
	assert.Equal(t, false, body["running"])
	assert.NotEmpty(t, body["message"])
}

// ============================================================================
// 同步走哪个代理
// ============================================================================

// 下拉框要的字段一个都不能少（前端复用上游 ProxySelector.vue，
// 它按 proxy.id / name / protocol / host / port 渲染），**密码一个字都不能出现**。
func TestSyncSettingsMatchesProxySelectorContract(t *testing.T) {
	selected := int64(3)
	sync := &fakePromptSyncService{settings: &SyncSettings{
		ProxyID: &selected,
		Proxies: []ProxyOption{testProxyOption()},
	}}
	engine := newPromptEngine(t, &fakePromptService{}, sync, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/prompts/sync/settings", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := decodeJSON(t, rec.Body.Bytes())

	assert.Equal(t, float64(3), body["proxy_id"], "ProxySelector 的 v-model 绑的就是它")
	assert.Equal(t, "香港节点", body["proxy_name"])
	assert.Contains(t, body["message"], "不影响出图", "必须说清这个代理只管同步")

	proxies, ok := body["proxies"].([]any)
	require.True(t, ok, "proxies 必须是数组：%s", rec.Body.String())
	require.Len(t, proxies, 1)

	first, _ := proxies[0].(map[string]any)
	for _, key := range []string{"id", "name", "protocol", "host", "port", "status"} {
		_, has := first[key]
		assert.True(t, has, "ProxySelector 要读 %s，少一个就是个空白下拉框", key)
	}
	assert.Equal(t, float64(3), first["id"])
	assert.Equal(t, "香港节点", first["name"])
	assert.Equal(t, float64(8080), first["port"])

	_, hasPassword := first["password"]
	assert.False(t, hasPassword, "代理密码绝不能出接口")
	assert.NotContains(t, rec.Body.String(), "password")

	assertNoCodeKey(t, body)
}

// 一个代理都没有时给空数组，不能给 null（前端要直接 v-for）。
func TestSyncSettingsProxiesAreNeverNull(t *testing.T) {
	sync := &fakePromptSyncService{settings: &SyncSettings{}}
	engine := newPromptEngine(t, &fakePromptService{}, sync, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/prompts/sync/settings", "", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"proxies":[]`)
	assert.Contains(t, rec.Body.String(), `"proxy_id":null`)
}

// 明确传 null = 不走代理。
func TestUpdateSyncSettingsAcceptsExplicitNull(t *testing.T) {
	sync := &fakePromptSyncService{settings: &SyncSettings{Proxies: []ProxyOption{testProxyOption()}}}
	engine := newPromptEngine(t, &fakePromptService{}, sync, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodPut, "/api/v1/designkit/prompts/sync/settings",
		`{"proxy_id":null}`, nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, sync.putCall)
	assert.True(t, sync.putSawNil, "明确传 null 就是「不使用代理」")
}

func TestUpdateSyncSettingsAcceptsProxyID(t *testing.T) {
	sync := &fakePromptSyncService{settings: &SyncSettings{Proxies: []ProxyOption{testProxyOption()}}}
	engine := newPromptEngine(t, &fakePromptService{}, sync, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodPut, "/api/v1/designkit/prompts/sync/settings",
		`{"proxy_id":3}`, nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, sync.putProxyID)
	assert.Equal(t, int64(3), *sync.putProxyID)

	body := decodeJSON(t, rec.Body.Bytes())
	assert.Equal(t, float64(3), body["proxy_id"], "改完要把最新状态回给界面")
}

// **缺字段直接 400，不当成 null。**
// 前端少传一个字段就悄悄把代理清掉的话，表现是「同步昨天还好好的，今天拉不动了」，
// 而设置页面上看不出任何异常。
func TestUpdateSyncSettingsRejectsMissingField(t *testing.T) {
	sync := &fakePromptSyncService{settings: &SyncSettings{}}
	engine := newPromptEngine(t, &fakePromptService{}, sync, upstreamservice.RoleAdmin)

	for _, body := range []string{`{}`, `{"proxy":3}`} {
		rec := doRequest(t, engine, http.MethodPut, "/api/v1/designkit/prompts/sync/settings", body, nil)

		require.Equal(t, http.StatusBadRequest, rec.Code, "请求体 %s：%s", body, rec.Body.String())
		assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
	}
	assert.Zero(t, sync.putCall, "字段都没传就不该动配置")
}

func TestUpdateSyncSettingsRejectsBadProxyID(t *testing.T) {
	sync := &fakePromptSyncService{settings: &SyncSettings{}}
	engine := newPromptEngine(t, &fakePromptService{}, sync, upstreamservice.RoleAdmin)

	for _, body := range []string{`{"proxy_id":0}`, `{"proxy_id":-1}`, `{"proxy_id":"3"}`} {
		rec := doRequest(t, engine, http.MethodPut, "/api/v1/designkit/prompts/sync/settings", body, nil)

		require.Equal(t, http.StatusBadRequest, rec.Code, "请求体 %s：%s", body, rec.Body.String())
		assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
	}
	assert.Zero(t, sync.putCall)
}

// ============================================================================
// 挂载
// ============================================================================

// 同步是运维动作，**不挂 ERP 组**：给外部系统一个「把全站灵感库刷一遍」的按钮
// 没有任何好处，只多一个误触面。
func TestPromptSyncIsNotMountedOnMachinePrefix(t *testing.T) {
	sync := &fakePromptSyncService{run: testSyncRun(dkdomain.SyncStatusRunning)}
	engine := newPromptEngine(t, &fakePromptService{}, sync, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodPost, "/v1/designkit/prompts/sync", "", nil)

	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Zero(t, sync.startCall)
}

// 同步服务缺席时只关掉同步那四个端点，**浏览照常** ——
// 运营翻灵感库跟同步跑没跑过没有任何关系。
func TestPromptBrowsingWorksWithoutSyncService(t *testing.T) {
	prompts := &fakePromptService{categories: testCategories(), page: &PromptPage{}}
	engine := newPromptEngine(t, prompts, nil, upstreamservice.RoleAdmin)

	ok := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/prompt-categories", "", nil)
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())

	gone := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/prompts/sync", "", nil)
	assert.Equal(t, http.StatusNotFound, gone.Code)
}

// 灵感库整个缺席时，出图那几个端点必须照常 —— 它不进 Services.Ready()。
func TestMissingPromptServiceDoesNotTakeDownImageEndpoints(t *testing.T) {
	engine := newPromptEngine(t, nil, nil, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/ratios", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "灵感库缺席不该把出图端点一起拖下水")

	gone := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/prompts", "", nil)
	assert.Equal(t, http.StatusNotFound, gone.Code)
}

// 游标是不透明的：客户端不许解析，但我们自己必须能来回转。
func TestPromptCursorRoundTrip(t *testing.T) {
	for _, id := range []int64{1, 42, 14700, 1 << 40} {
		got, err := decodePromptCursor(encodePromptCursor(id))
		require.NoError(t, err)
		assert.Equal(t, id, got)
	}

	empty, err := decodePromptCursor("")
	require.NoError(t, err, "空游标就是第一页，不该报错")
	assert.Zero(t, empty)

	for _, bad := range []string{"!!!", "YWJj", encodeCursor(testTime(), 9)} {
		_, err := decodePromptCursor(bad)
		assert.ErrorIs(t, err, ErrInvalidCursor, "游标 %q 应该被拒绝", bad)
	}
}
