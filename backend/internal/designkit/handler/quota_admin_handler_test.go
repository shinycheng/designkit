//go:build unit

package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	upstreammw "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// 额度申请（管理端）
// ============================================================================
//
// 这一组守两条底线（任务要求点名的）：
//  1. 两个端点**只有管理员**能进，非管理员一律 403 且文案是中文；
//  2. 「通过」的金额校验拦得死 —— 0、负数、非数字、没填，一律 400，
//     **service 一次都不能被调到**（通过 = 真金白银加余额）。

// ---- 假的管理端服务 ----

type fakeQuotaAdminService struct {
	list     *QuotaRequestAdminList
	listErr  error
	listCall int
	lastList struct {
		pendingOnly   bool
		limit, offset int
	}

	handled    *dkdomain.QuotaRequest
	handleErr  error
	handleCall int
	lastHandle HandleQuotaRequestInput
}

func (f *fakeQuotaAdminService) ListQuotaRequests(_ context.Context, pendingOnly bool, limit, offset int) (*QuotaRequestAdminList, error) {
	f.listCall++
	f.lastList.pendingOnly = pendingOnly
	f.lastList.limit = limit
	f.lastList.offset = offset
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list, nil
}

func (f *fakeQuotaAdminService) HandleQuotaRequest(_ context.Context, in HandleQuotaRequestInput) (*dkdomain.QuotaRequest, error) {
	f.handleCall++
	f.lastHandle = in
	if f.handleErr != nil {
		return nil, f.handleErr
	}
	return f.handled, nil
}

func testQuotaRequestDetail(id int64) *dkdomain.QuotaRequestDetail {
	note := "双十一要赶工"
	return &dkdomain.QuotaRequestDetail{
		QuotaRequest: dkdomain.QuotaRequest{
			ID:        id,
			UserID:    testUserID,
			Note:      &note,
			Status:    dkdomain.QuotaRequestPending,
			CreatedAt: testTime(),
		},
		RequesterEmail: "yunying@example.com",
	}
}

// newQuotaAdminEngine 按 RegisterBusinessRoutes 的真实结构搭引擎，并指定登录者的角色。
func newQuotaAdminEngine(t *testing.T, svc QuotaAdminService, role string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	r := gin.New()
	v1 := r.Group("/api/v1")
	browser := v1.Group("/designkit")
	browser.Use(fakeAuthWithRole(testUserID, role))
	machine := r.Group("/v1/designkit")

	services := testServices(&fakeJobService{})
	services.QuotaAdmin = svc

	RegisterBusinessRoutes(BusinessRouteOptions{
		Browser:    browser,
		Machine:    machine,
		Services:   services,
		KeyAuth:    fakeAuthWithRole(testUserID, role),
		APIKeyAuth: upstreammw.APIKeyAuthMiddleware(fakeAuthWithRole(testUserID, role)),
	})
	return r
}

// ---- 管理员判定 ----

// 非管理员碰这两个端点一律 403，而且**必须是中文**，service 一次都不被调到。
func TestQuotaAdminEndpointsRejectNonAdmin(t *testing.T) {
	svc := &fakeQuotaAdminService{
		list:    &QuotaRequestAdminList{PendingCount: 1},
		handled: &dkdomain.QuotaRequest{ID: 5, Status: dkdomain.QuotaRequestHandled},
	}
	engine := newQuotaAdminEngine(t, svc, upstreamservice.RoleUser)

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/designkit/admin/quota-requests", ""},
		{http.MethodPost, "/api/v1/designkit/admin/quota-requests/5/handle",
			`{"action":"approve","amount":"50"}`},
	}
	for _, tc := range cases {
		rec := doRequest(t, engine, tc.method, tc.path, tc.body, nil)

		require.Equal(t, http.StatusForbidden, rec.Code, "%s %s：%s", tc.method, tc.path, rec.Body.String())
		assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeForbidden)

		message, _ := errorObject(t, rec.Body.Bytes())["message"].(string)
		assert.NotContains(t, message, "Admin", "403 的文案必须是中文")
		assert.Contains(t, message, "管理员")
	}
	assert.Zero(t, svc.listCall, "非管理员绝不能看到申请列表")
	assert.Zero(t, svc.handleCall, "非管理员绝不能处理申请（通过 = 加余额）")
}

// 登录了但上下文里没有角色（鉴权中间件没塞）时，按**非管理员**处理。
func TestQuotaAdminRejectsMissingRole(t *testing.T) {
	svc := &fakeQuotaAdminService{handled: &dkdomain.QuotaRequest{ID: 5}}
	engine := newQuotaAdminEngine(t, svc, "")

	rec := doRequest(t, engine, http.MethodPost,
		"/api/v1/designkit/admin/quota-requests/5/handle", `{"action":"reject"}`, nil)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Zero(t, svc.handleCall)
}

// ---- 金额校验 ----

// 「通过」的金额：0、负数、非数字、没填，一律 400，service 不被调到。
func TestQuotaAdminApproveRejectsBadAmount(t *testing.T) {
	svc := &fakeQuotaAdminService{handled: &dkdomain.QuotaRequest{ID: 5}}
	engine := newQuotaAdminEngine(t, svc, upstreamservice.RoleAdmin)

	for _, body := range []string{
		`{"action":"approve","amount":"0"}`,
		`{"action":"approve","amount":"-5"}`,
		`{"action":"approve","amount":"abc"}`,
		`{"action":"approve","amount":""}`,
		`{"action":"approve"}`,
	} {
		rec := doRequest(t, engine, http.MethodPost,
			"/api/v1/designkit/admin/quota-requests/5/handle", body, nil)

		require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s：%s", body, rec.Body.String())
		assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
	}
	assert.Zero(t, svc.handleCall, "金额不合法时 service 一次都不能被调到 —— 通过 = 真金白银加余额")
}

// action 只认 approve / reject，别的值 400。
func TestQuotaAdminHandleRejectsUnknownAction(t *testing.T) {
	svc := &fakeQuotaAdminService{handled: &dkdomain.QuotaRequest{ID: 5}}
	engine := newQuotaAdminEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodPost,
		"/api/v1/designkit/admin/quota-requests/5/handle", `{"action":"ok","amount":"50"}`, nil)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
	assert.Zero(t, svc.handleCall)
}

// 备注超长直接 400（跟运营侧申请说明同一个上限），不静默截断。
func TestQuotaAdminHandleRejectsTooLongNote(t *testing.T) {
	svc := &fakeQuotaAdminService{handled: &dkdomain.QuotaRequest{ID: 5}}
	engine := newQuotaAdminEngine(t, svc, upstreamservice.RoleAdmin)

	long := strings.Repeat("额", maxQuotaHandleNoteRunes+1)
	rec := doRequest(t, engine, http.MethodPost,
		"/api/v1/designkit/admin/quota-requests/5/handle",
		fmt.Sprintf(`{"action":"reject","note":"%s"}`, long), nil)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
	assert.Zero(t, svc.handleCall)
}

// ---- 通过 / 驳回的透传 ----

// 「通过」把解析好的金额、去空白的备注、当前管理员传给 service，响应带实际金额。
func TestQuotaAdminApprovePassesInput(t *testing.T) {
	handledAt := testTime()
	svc := &fakeQuotaAdminService{handled: &dkdomain.QuotaRequest{
		ID:        5,
		UserID:    99,
		Status:    dkdomain.QuotaRequestHandled,
		HandledAt: &handledAt,
	}}
	engine := newQuotaAdminEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodPost,
		"/api/v1/designkit/admin/quota-requests/5/handle",
		`{"action":"approve","amount":"50","note":"  按月度额度加  "}`, nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, svc.handleCall)
	assert.Equal(t, int64(5), svc.lastHandle.ID)
	assert.Equal(t, testUserID, svc.lastHandle.AdminUserID)
	assert.True(t, svc.lastHandle.Approve)
	assert.Equal(t, "按月度额度加", svc.lastHandle.Note, "备注要去掉首尾空白")
	assert.True(t, svc.lastHandle.Amount.Equal(dkdomain.MoneyFromInt(50)),
		"金额要按十进制字符串解析，不能走浮点")

	body := decodeJSON(t, rec.Body.Bytes())
	assert.Equal(t, "handled", body["status"])
	assert.Equal(t, dkdomain.MoneyString(dkdomain.MoneyFromInt(50)), body["approved_amount"])
	assertNoCodeKey(t, body)
}

// 「驳回」不要求金额，amount 传了也忽略（service 收到的 Approve=false）。
func TestQuotaAdminRejectNeedsNoAmount(t *testing.T) {
	svc := &fakeQuotaAdminService{handled: &dkdomain.QuotaRequest{
		ID:     5,
		Status: dkdomain.QuotaRequestRejected,
	}}
	engine := newQuotaAdminEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodPost,
		"/api/v1/designkit/admin/quota-requests/5/handle", `{"action":"reject","note":"先用完本月的"}`, nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, svc.handleCall)
	assert.False(t, svc.lastHandle.Approve)

	body := decodeJSON(t, rec.Body.Bytes())
	assert.Equal(t, "rejected", body["status"])
	assert.Nil(t, body["approved_amount"], "驳回不加钱，approved_amount 必须是 null")
}

// ---- 列表 ----

// 列表：status 翻成 pendingOnly，形状带 items + pending_count，空列表是 [] 不是 null。
func TestQuotaAdminListShapeAndStatusFilter(t *testing.T) {
	svc := &fakeQuotaAdminService{list: &QuotaRequestAdminList{
		Items:        []*dkdomain.QuotaRequestDetail{testQuotaRequestDetail(3)},
		PendingCount: 4,
	}}
	engine := newQuotaAdminEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/admin/quota-requests?status=handled", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, svc.listCall)
	assert.False(t, svc.lastList.pendingOnly, "status=handled 要翻成 pendingOnly=false")

	body := decodeJSON(t, rec.Body.Bytes())
	assert.Equal(t, float64(4), body["pending_count"])
	items, ok := body["items"].([]any)
	require.True(t, ok, "items 必须是数组：%s", rec.Body.String())
	require.Len(t, items, 1)
	first, _ := items[0].(map[string]any)
	assert.Equal(t, "yunying@example.com", first["requester_email"])
	assert.Equal(t, "pending", first["status"])
	assertNoCodeKey(t, body)

	// 空列表返回 []，不返回 null —— 前端不用多写一层判空。
	svc.list = &QuotaRequestAdminList{PendingCount: 0}
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/designkit/admin/quota-requests", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, svc.lastList.pendingOnly, "不带 status 默认看待处理")
	assert.Contains(t, rec.Body.String(), `"items":[]`)
}

// status 传了别的值直接 400，不静默当成默认值。
func TestQuotaAdminListRejectsUnknownStatus(t *testing.T) {
	svc := &fakeQuotaAdminService{list: &QuotaRequestAdminList{}}
	engine := newQuotaAdminEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/admin/quota-requests?status=all", "", nil)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
	assert.Zero(t, svc.listCall)
}
