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
// 用户记录（管理端）
// ============================================================================
//
// 这一组守三条底线：
//  1. 七个端点**只有管理员**能进 —— service 侧刻意不做归属校验，
//     RequireAdmin 是唯一的门，非管理员必须 403 且 service 一次都不被调到；
//  2. user_id 筛选原样透传（省略 = 0 = 全部账户），拼错直接 400 不静默；
//  3. limit 默认 50、封顶 200（对外契约）。

// testRecordUID 一个合法的 26 位 ULID（跟 testJob 的 uid 同一个）。
const testRecordUID = "01J8ZK7Q9X2M4N6P8R0T2V4W6Y"

// ---- 假的用户记录服务 ----

type fakeAdminRecordsService struct {
	users     []*dkdomain.RecordUser
	usersErr  error
	usersCall int

	sessions     []*dkdomain.ChatSessionAdminView
	sessionsErr  error
	sessionsCall int
	lastSessions struct {
		userID        int64
		limit, offset int
	}

	session        *dkdomain.ChatSessionAdminView
	messages       []*dkdomain.ChatMessage
	sessionErr     error
	sessionCall    int
	lastSessionUID string

	jobs     []*dkdomain.JobAdminView
	jobsErr  error
	jobsCall int
	lastJobs struct {
		userID        int64
		limit, offset int
	}

	job     *dkdomain.JobAdminView
	items   []*dkdomain.JobItemAdminView
	jobErr  error
	jobCall int

	blob        *ContentBlob
	contentErr  error
	contentCall int
	lastContent struct {
		jobUID string
		seq    int
	}

	assetBlob    *ContentBlob
	assetErr     error
	assetCall    int
	lastAssetUID string
}

func (f *fakeAdminRecordsService) ListRecordUsers(_ context.Context) ([]*dkdomain.RecordUser, error) {
	f.usersCall++
	if f.usersErr != nil {
		return nil, f.usersErr
	}
	return f.users, nil
}

func (f *fakeAdminRecordsService) ListChatSessionRecords(_ context.Context, userID int64, limit, offset int) ([]*dkdomain.ChatSessionAdminView, error) {
	f.sessionsCall++
	f.lastSessions.userID = userID
	f.lastSessions.limit = limit
	f.lastSessions.offset = offset
	if f.sessionsErr != nil {
		return nil, f.sessionsErr
	}
	return f.sessions, nil
}

func (f *fakeAdminRecordsService) GetChatSessionRecord(_ context.Context, uid string) (*dkdomain.ChatSessionAdminView, []*dkdomain.ChatMessage, error) {
	f.sessionCall++
	f.lastSessionUID = uid
	if f.sessionErr != nil {
		return nil, nil, f.sessionErr
	}
	return f.session, f.messages, nil
}

func (f *fakeAdminRecordsService) ListJobRecords(_ context.Context, userID int64, limit, offset int) ([]*dkdomain.JobAdminView, error) {
	f.jobsCall++
	f.lastJobs.userID = userID
	f.lastJobs.limit = limit
	f.lastJobs.offset = offset
	if f.jobsErr != nil {
		return nil, f.jobsErr
	}
	return f.jobs, nil
}

func (f *fakeAdminRecordsService) GetJobRecord(_ context.Context, _ string) (*dkdomain.JobAdminView, []*dkdomain.JobItemAdminView, error) {
	f.jobCall++
	if f.jobErr != nil {
		return nil, nil, f.jobErr
	}
	return f.job, f.items, nil
}

func (f *fakeAdminRecordsService) OpenJobRecordItemContent(_ context.Context, jobUID string, seq int) (*ContentBlob, error) {
	f.contentCall++
	f.lastContent.jobUID = jobUID
	f.lastContent.seq = seq
	if f.contentErr != nil {
		return nil, f.contentErr
	}
	return f.blob, nil
}

func (f *fakeAdminRecordsService) OpenAssetRecordContent(_ context.Context, uid string) (*ContentBlob, error) {
	f.assetCall++
	f.lastAssetUID = uid
	if f.assetErr != nil {
		return nil, f.assetErr
	}
	return f.assetBlob, nil
}

func (f *fakeAdminRecordsService) totalCalls() int {
	return f.usersCall + f.sessionsCall + f.sessionCall + f.jobsCall + f.jobCall + f.contentCall + f.assetCall
}

// newAdminRecordsEngine 按 RegisterBusinessRoutes 的真实结构搭引擎，并指定登录者的角色。
func newAdminRecordsEngine(t *testing.T, svc AdminRecordsService, role string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	r := gin.New()
	v1 := r.Group("/api/v1")
	browser := v1.Group("/designkit")
	browser.Use(fakeAuthWithRole(testUserID, role))
	machine := r.Group("/v1/designkit")

	services := testServices(&fakeJobService{})
	services.AdminRecords = svc

	RegisterBusinessRoutes(BusinessRouteOptions{
		Browser:    browser,
		Machine:    machine,
		Services:   services,
		KeyAuth:    fakeAuthWithRole(testUserID, role),
		APIKeyAuth: upstreammw.APIKeyAuthMiddleware(fakeAuthWithRole(testUserID, role)),
	})
	return r
}

func testChatSessionView(uid string, userID int64) *dkdomain.ChatSessionAdminView {
	return &dkdomain.ChatSessionAdminView{
		ChatSession: dkdomain.ChatSession{
			ID:        11,
			UID:       uid,
			UserID:    userID,
			Title:     "夏季主图怎么配色",
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
		UserEmail:    "yunying@example.com",
		MessageCount: 4,
	}
}

func testJobAdminView(uid string, userID int64) *dkdomain.JobAdminView {
	actual := dkdomain.MoneyFromFloat(2)
	return &dkdomain.JobAdminView{
		Job: dkdomain.Job{
			ID:            21,
			UID:           uid,
			UserID:        userID,
			Status:        dkdomain.JobStatusHolding,
			Name:          "夏季连衣裙",
			Ratio:         dkdomain.Ratio3x4,
			ItemCount:     6,
			SuccessCount:  2,
			FailCount:     1,
			Currency:      dkdomain.CurrencyUSD,
			EstimatedCost: dkdomain.MoneyFromFloat(6),
			ActualCost:    &actual,
			CreatedAt:     testTime(),
			UpdatedAt:     testTime(),
		},
		UserEmail: "yunying@example.com",
	}
}

// ---- 管理员判定 ----

// 非管理员碰六个端点一律 403，文案是中文，service 一次都不被调到。
func TestAdminRecordsEndpointsRejectNonAdmin(t *testing.T) {
	svc := &fakeAdminRecordsService{}
	engine := newAdminRecordsEngine(t, svc, upstreamservice.RoleUser)

	paths := []string{
		"/api/v1/designkit/admin/records/users",
		"/api/v1/designkit/admin/records/chat/sessions",
		"/api/v1/designkit/admin/records/chat/sessions/" + testRecordUID,
		"/api/v1/designkit/admin/records/jobs",
		"/api/v1/designkit/admin/records/jobs/" + testRecordUID,
		"/api/v1/designkit/admin/records/jobs/" + testRecordUID + "/items/1/content",
		"/api/v1/designkit/admin/records/assets/" + testRecordUID + "/content",
	}
	for _, path := range paths {
		rec := doRequest(t, engine, http.MethodGet, path, "", nil)

		require.Equal(t, http.StatusForbidden, rec.Code, "GET %s：%s", path, rec.Body.String())
		assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeForbidden)

		message, _ := errorObject(t, rec.Body.Bytes())["message"].(string)
		assert.NotContains(t, message, "Admin", "403 的文案必须是中文")
		assert.Contains(t, message, "管理员")
	}
	assert.Zero(t, svc.totalCalls(),
		"非管理员绝不能碰到 service —— 这一层刻意不做归属校验，RequireAdmin 是唯一的门")
}

// 登录了但上下文里没有角色（鉴权中间件没塞）时，按**非管理员**处理。
func TestAdminRecordsRejectsMissingRole(t *testing.T) {
	svc := &fakeAdminRecordsService{}
	engine := newAdminRecordsEngine(t, svc, "")

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/admin/records/users", "", nil)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Zero(t, svc.totalCalls())
}

// ---- 有记录的账户 ----

func TestAdminRecordsUsersShape(t *testing.T) {
	svc := &fakeAdminRecordsService{users: []*dkdomain.RecordUser{
		{ID: 7, Email: "yunying@example.com", SessionCount: 3, JobCount: 12},
		{ID: 9, Email: "", SessionCount: 0, JobCount: 1}, // 账号已删：邮箱空串，行照给
	}}
	engine := newAdminRecordsEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/admin/records/users", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := decodeJSON(t, rec.Body.Bytes())
	users, ok := body["users"].([]any)
	require.True(t, ok, "users 必须是数组：%s", rec.Body.String())
	require.Len(t, users, 2)

	first, _ := users[0].(map[string]any)
	assert.Equal(t, float64(7), first["id"])
	assert.Equal(t, "yunying@example.com", first["email"])
	assert.Equal(t, float64(3), first["session_count"])
	assert.Equal(t, float64(12), first["job_count"])
	assertNoCodeKey(t, body)

	// 空列表返回 []，不返回 null。
	svc.users = nil
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/designkit/admin/records/users", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"users":[]`)
}

// ---- 会话列表 ----

// user_id / limit / offset 原样透传；省略 user_id = 0（全部账户）；limit 默认 50、封顶 200。
func TestAdminRecordsSessionsQueryPassthrough(t *testing.T) {
	svc := &fakeAdminRecordsService{}
	engine := newAdminRecordsEngine(t, svc, upstreamservice.RoleAdmin)

	// 什么参数都不带：全部账户 + 默认分页。
	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/admin/records/chat/sessions", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, int64(0), svc.lastSessions.userID, "user_id 省略 = 全部账户")
	assert.Equal(t, adminRecordsDefaultLimit, svc.lastSessions.limit)
	assert.Equal(t, 0, svc.lastSessions.offset)

	// 三个参数齐上：逐个透传。
	rec = doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/admin/records/chat/sessions?user_id=42&limit=10&offset=5", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, int64(42), svc.lastSessions.userID)
	assert.Equal(t, 10, svc.lastSessions.limit)
	assert.Equal(t, 5, svc.lastSessions.offset)

	// limit 超过 200 收敛到 200（封顶是收敛不是报错）。
	rec = doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/admin/records/chat/sessions?limit=999", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, adminRecordsMaxLimit, svc.lastSessions.limit)
}

// user_id / limit / offset 拼错直接 400，不静默当成默认值。
func TestAdminRecordsSessionsRejectsBadQuery(t *testing.T) {
	svc := &fakeAdminRecordsService{}
	engine := newAdminRecordsEngine(t, svc, upstreamservice.RoleAdmin)

	for _, query := range []string{
		"?user_id=abc", "?user_id=0", "?user_id=-1",
		"?limit=0", "?limit=-3", "?limit=abc",
		"?offset=-1", "?offset=abc",
	} {
		rec := doRequest(t, engine, http.MethodGet,
			"/api/v1/designkit/admin/records/chat/sessions"+query, "", nil)

		require.Equal(t, http.StatusBadRequest, rec.Code, "query=%s：%s", query, rec.Body.String())
		assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
	}
	assert.Zero(t, svc.sessionsCall, "参数不合法时 service 一次都不能被调到")
}

func TestAdminRecordsSessionsShape(t *testing.T) {
	svc := &fakeAdminRecordsService{sessions: []*dkdomain.ChatSessionAdminView{
		testChatSessionView(testRecordUID, 42),
	}}
	engine := newAdminRecordsEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/admin/records/chat/sessions", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := decodeJSON(t, rec.Body.Bytes())
	sessions, ok := body["sessions"].([]any)
	require.True(t, ok, "sessions 必须是数组：%s", rec.Body.String())
	require.Len(t, sessions, 1)

	first, _ := sessions[0].(map[string]any)
	assert.Equal(t, testRecordUID, first["uid"])
	assert.Equal(t, "夏季主图怎么配色", first["title"])
	assert.Equal(t, float64(42), first["user_id"])
	assert.Equal(t, "yunying@example.com", first["user_email"])
	assert.Equal(t, float64(4), first["message_count"])
	assert.NotEmpty(t, first["created_at"])
	assert.NotEmpty(t, first["updated_at"])
	assert.NotContains(t, first, "id", "内部自增主键不进响应，会话对外只有 uid")
	assertNoCodeKey(t, body)
}

// ---- 会话详情 ----

func TestAdminRecordsSessionDetailShape(t *testing.T) {
	svc := &fakeAdminRecordsService{
		session: testChatSessionView(testRecordUID, 42),
		messages: []*dkdomain.ChatMessage{
			{ID: 1, Role: dkdomain.ChatRoleUser, Content: "这张图配什么背景", AssetUIDs: []string{"01J8ZK7Q9X2M4N6P8R0T2V4W6Z"}, CreatedAt: testTime()},
			{ID: 2, Role: dkdomain.ChatRoleAssistant, Content: "建议浅灰渐变", AssetUIDs: nil, CreatedAt: testTime()},
		},
	}
	engine := newAdminRecordsEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/admin/records/chat/sessions/"+testRecordUID, "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, svc.sessionCall)
	assert.Equal(t, testRecordUID, svc.lastSessionUID)

	body := decodeJSON(t, rec.Body.Bytes())
	session, _ := body["session"].(map[string]any)
	require.NotNil(t, session)
	assert.Equal(t, testRecordUID, session["uid"])

	messages, ok := body["messages"].([]any)
	require.True(t, ok, "messages 必须是数组")
	require.Len(t, messages, 2)
	first, _ := messages[0].(map[string]any)
	assert.Equal(t, float64(1), first["id"])
	assert.Equal(t, "user", first["role"])
	assert.Equal(t, "这张图配什么背景", first["content"])
	second, _ := messages[1].(map[string]any)
	uids, ok := second["asset_uids"].([]any)
	require.True(t, ok, "asset_uids 必须是数组不是 null：%s", rec.Body.String())
	assert.Empty(t, uids)
	assertNoCodeKey(t, body)
}

// uid 不是合法 ULID 时直接 404，service 不被调到。
func TestAdminRecordsSessionDetailRejectsBadUID(t *testing.T) {
	svc := &fakeAdminRecordsService{}
	engine := newAdminRecordsEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/admin/records/chat/sessions/not-a-ulid", "", nil)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeChatSessionNotFound)
	assert.Zero(t, svc.sessionCall)
}

// ---- 批次列表 ----

func TestAdminRecordsJobsQueryPassthroughAndShape(t *testing.T) {
	svc := &fakeAdminRecordsService{jobs: []*dkdomain.JobAdminView{
		testJobAdminView(testRecordUID, 42),
	}}
	engine := newAdminRecordsEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/admin/records/jobs?user_id=42", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, svc.jobsCall)
	assert.Equal(t, int64(42), svc.lastJobs.userID, "user_id 筛选要原样透传")
	assert.Equal(t, adminRecordsDefaultLimit, svc.lastJobs.limit)

	body := decodeJSON(t, rec.Body.Bytes())
	jobs, ok := body["jobs"].([]any)
	require.True(t, ok, "jobs 必须是数组：%s", rec.Body.String())
	require.Len(t, jobs, 1)

	first, _ := jobs[0].(map[string]any)
	assert.Equal(t, testRecordUID, first["uid"])
	assert.Equal(t, "夏季连衣裙", first["name"])
	assert.Equal(t, "holding", first["status"])
	assert.Equal(t, float64(42), first["user_id"])
	assert.Equal(t, "yunying@example.com", first["user_email"])
	assert.Equal(t, float64(6), first["item_count"])
	assert.Equal(t, float64(2), first["success_count"])
	assert.Equal(t, float64(1), first["fail_count"])
	assert.Equal(t, dkdomain.MoneyString(dkdomain.MoneyFromFloat(2)), first["actual_cost"],
		"金额必须是十进制字符串，不能走浮点")
	assert.Equal(t, dkdomain.CurrencyUSD, first["currency"])
	assert.Equal(t, "3:4", first["ratio"])
	assert.NotEmpty(t, first["created_at"])
	assertNoCodeKey(t, body)

	// 没结算完 actual_cost 是 null。
	svc.jobs[0].ActualCost = nil
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/designkit/admin/records/jobs", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, int64(0), svc.lastJobs.userID, "user_id 省略 = 全部账户")
	jobs, ok = decodeJSON(t, rec.Body.Bytes())["jobs"].([]any)
	require.True(t, ok)
	require.Len(t, jobs, 1)
	first, _ = jobs[0].(map[string]any)
	assert.Nil(t, first["actual_cost"])
}

// ---- 批次详情 ----

func TestAdminRecordsJobDetailShape(t *testing.T) {
	billed := dkdomain.MoneyFromFloat(1)
	svc := &fakeAdminRecordsService{
		job: testJobAdminView(testRecordUID, 42),
		items: []*dkdomain.JobItemAdminView{
			{
				JobItem: dkdomain.JobItem{
					Seq: 1, Status: dkdomain.ItemStatusSucceeded,
					PromptText: "纯白背景棚拍", BilledCost: &billed,
				},
				HasImage: true,
			},
			{
				JobItem: dkdomain.JobItem{
					Seq: 2, Status: dkdomain.ItemStatusPending,
					PromptText: "户外草地",
				},
				HasImage: false,
			},
		},
	}
	engine := newAdminRecordsEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/admin/records/jobs/"+testRecordUID, "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := decodeJSON(t, rec.Body.Bytes())
	job, _ := body["job"].(map[string]any)
	require.NotNil(t, job)
	assert.Equal(t, testRecordUID, job["uid"])

	items, ok := body["items"].([]any)
	require.True(t, ok, "items 必须是数组")
	require.Len(t, items, 2)
	first, _ := items[0].(map[string]any)
	assert.Equal(t, float64(1), first["seq"])
	assert.Equal(t, "succeeded", first["status"])
	assert.Equal(t, "纯白背景棚拍", first["prompt"])
	assert.Equal(t, dkdomain.MoneyString(billed), first["billed_cost"])
	assert.Equal(t, true, first["has_image"])
	second, _ := items[1].(map[string]any)
	assert.Nil(t, second["billed_cost"], "还没回填的账单是 null")
	assert.Equal(t, false, second["has_image"])
	assertNoCodeKey(t, body)
}

// ---- 图片字节 ----

func TestAdminRecordsItemContent(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G'}
	svc := &fakeAdminRecordsService{blob: &ContentBlob{
		Data:        png,
		ContentType: "image/png",
	}}
	engine := newAdminRecordsEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/admin/records/jobs/"+testRecordUID+"/items/3/content", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, svc.contentCall)
	assert.Equal(t, testRecordUID, svc.lastContent.jobUID)
	assert.Equal(t, 3, svc.lastContent.seq)
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Equal(t, png, rec.Body.Bytes())
}

// seq 不是正整数直接 404，service 不被调到。
func TestAdminRecordsItemContentRejectsBadSeq(t *testing.T) {
	svc := &fakeAdminRecordsService{}
	engine := newAdminRecordsEngine(t, svc, upstreamservice.RoleAdmin)

	for _, seq := range []string{"0", "-1", "abc"} {
		rec := doRequest(t, engine, http.MethodGet,
			"/api/v1/designkit/admin/records/jobs/"+testRecordUID+"/items/"+seq+"/content", "", nil)

		require.Equal(t, http.StatusNotFound, rec.Code, "seq=%s：%s", seq, rec.Body.String())
		assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeItemNotFound)
	}
	assert.Zero(t, svc.contentCall)
}

// service 报存储错误时原样透出中文（不误报成 404）。
func TestAdminRecordsItemContentStorageError(t *testing.T) {
	svc := &fakeAdminRecordsService{
		contentErr: dkdomain.NewError(dkdomain.ErrCodeStorageError),
	}
	engine := newAdminRecordsEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/admin/records/jobs/"+testRecordUID+"/items/1/content", "", nil)

	require.NotEqual(t, http.StatusNotFound, rec.Code,
		"存储坏了不能报 404 —— 管理员会以为记录没了，实际要查的是磁盘")
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeStorageError)
}

// ---- 对话附图字节 ----

// 管理员取对话附图：uid 原样透传，字节和 Content-Type 原样返回。
func TestAdminRecordsAssetContent(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G'}
	svc := &fakeAdminRecordsService{assetBlob: &ContentBlob{
		Data:        png,
		ContentType: "image/png",
	}}
	engine := newAdminRecordsEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/admin/records/assets/"+testRecordUID+"/content", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, svc.assetCall)
	assert.Equal(t, testRecordUID, svc.lastAssetUID)
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Equal(t, png, rec.Body.Bytes())
}

// uid 不是合法 ULID 直接 404，service 不被调到。
func TestAdminRecordsAssetContentRejectsBadUID(t *testing.T) {
	svc := &fakeAdminRecordsService{}
	engine := newAdminRecordsEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/admin/records/assets/not-a-ulid/content", "", nil)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeAssetNotFound)
	assert.Zero(t, svc.assetCall)
}

// service 报存储错误时原样透出（不误报成 404，口径同批次缩略图）。
func TestAdminRecordsAssetContentStorageError(t *testing.T) {
	svc := &fakeAdminRecordsService{
		assetErr: dkdomain.NewError(dkdomain.ErrCodeStorageError),
	}
	engine := newAdminRecordsEngine(t, svc, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/admin/records/assets/"+testRecordUID+"/content", "", nil)

	require.NotEqual(t, http.StatusNotFound, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeStorageError)
}

// 服务缺席时整组不挂：裸 404（没有 DK_ 错误码），前端据此显示「还没准备好」。
func TestAdminRecordsRoutesAbsentWhenServiceNil(t *testing.T) {
	engine := newAdminRecordsEngine(t, nil, upstreamservice.RoleAdmin)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/admin/records/users", "", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}
