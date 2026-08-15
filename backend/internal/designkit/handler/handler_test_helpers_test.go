//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	upstreammw "github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 单测公用件
// ============================================================================
//
// 这些假实现只满足 ports.go 里的接口，不碰数据库、对象存储和上游网关 ——
// 本机没有 Go 编译器也没有 Docker，单测必须能在 GitHub Actions 上裸跑。

// ---- 假的素材服务 ----

type fakeAssetService struct {
	lastInput  CreateAssetInput
	createCall int
	asset      *dkdomain.Asset
	createErr  error
	getErr     error
	blob       *ContentBlob
	contentErr error
}

func (f *fakeAssetService) CreateAsset(_ context.Context, in CreateAssetInput) (*dkdomain.Asset, error) {
	f.createCall++
	f.lastInput = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.asset, nil
}

func (f *fakeAssetService) GetAsset(_ context.Context, _ int64, _ string) (*dkdomain.Asset, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.asset, nil
}

func (f *fakeAssetService) OpenAssetContent(_ context.Context, _ int64, _ string) (*ContentBlob, error) {
	if f.contentErr != nil {
		return nil, f.contentErr
	}
	return f.blob, nil
}

// ---- 假的配置服务 ----

type fakeCatalogService struct {
	options []RatioOption
	err     error
}

func (f *fakeCatalogService) ListRatios(_ context.Context) ([]RatioOption, error) {
	return f.options, f.err
}

// ---- 假的批次服务 ----

type fakeJobService struct {
	estimate    *EstimateResult
	estimateErr error

	job        *dkdomain.Job
	createErr  error
	createCall int
	lastCreate CreateJobInput

	page     *dkdomain.JobPage
	listErr  error
	getErr   error
	items    []*JobItemView
	itemsErr error
	item     *JobItemView
	itemErr  error

	blob       *ContentBlob
	contentErr error

	stop      *StopJobResult
	stopErr   error
	stopCall  int
	retry     *dkdomain.JobItem
	retryErr  error
	retryCall int
	lastRetry RetryItemInput
}

func (f *fakeJobService) EstimateJob(_ context.Context, _ JobSpec) (*EstimateResult, error) {
	if f.estimateErr != nil {
		return nil, f.estimateErr
	}
	return f.estimate, nil
}

func (f *fakeJobService) CreateJob(_ context.Context, in CreateJobInput) (*dkdomain.Job, error) {
	f.createCall++
	f.lastCreate = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.job, nil
}

func (f *fakeJobService) GetJob(_ context.Context, _ int64, _ string) (*dkdomain.Job, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.job, nil
}

func (f *fakeJobService) ListJobs(_ context.Context, _ dkdomain.ListJobsQuery) (*dkdomain.JobPage, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.page, nil
}

func (f *fakeJobService) ListJobItems(_ context.Context, _ int64, _ string) ([]*JobItemView, error) {
	if f.itemsErr != nil {
		return nil, f.itemsErr
	}
	return f.items, nil
}

func (f *fakeJobService) GetJobItem(_ context.Context, _ int64, _ string, _ int) (*JobItemView, error) {
	if f.itemErr != nil {
		return nil, f.itemErr
	}
	return f.item, nil
}

func (f *fakeJobService) OpenJobItemContent(_ context.Context, _ int64, _ string, _, _ int) (*ContentBlob, error) {
	if f.contentErr != nil {
		return nil, f.contentErr
	}
	return f.blob, nil
}

func (f *fakeJobService) StopJob(_ context.Context, _ int64, _ string) (*StopJobResult, error) {
	f.stopCall++
	if f.stopErr != nil {
		return nil, f.stopErr
	}
	return f.stop, nil
}

func (f *fakeJobService) RetryJobItem(_ context.Context, in RetryItemInput) (*dkdomain.JobItem, error) {
	f.retryCall++
	f.lastRetry = in
	if f.retryErr != nil {
		return nil, f.retryErr
	}
	return f.retry, nil
}

// ---- 假的消费服务 ----

type fakeMeService struct {
	summary    *UsageSummary
	summaryErr error

	quota     *QuotaRequestResult
	quotaErr  error
	quotaCall int
	lastNote  string
}

func (f *fakeMeService) GetUsageSummary(_ context.Context, _ int64) (*UsageSummary, error) {
	if f.summaryErr != nil {
		return nil, f.summaryErr
	}
	return f.summary, nil
}

func (f *fakeMeService) CreateQuotaRequest(_ context.Context, _ int64, note string) (*QuotaRequestResult, error) {
	f.quotaCall++
	f.lastNote = note
	if f.quotaErr != nil {
		return nil, f.quotaErr
	}
	return f.quota, nil
}

// ---- 假的结果图服务 ----

type fakeImageService struct {
	view       *ImageView
	getErr     error
	blob       *ContentBlob
	contentErr error
}

func (f *fakeImageService) GetImage(_ context.Context, _ int64, _ string) (*ImageView, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.view, nil
}

func (f *fakeImageService) OpenImageContent(_ context.Context, _ int64, _ string) (*ContentBlob, error) {
	if f.contentErr != nil {
		return nil, f.contentErr
	}
	return f.blob, nil
}

// ---- 引擎 ----

// fakeAuth 模拟上游鉴权中间件：只往上下文里塞一个 AuthSubject。
// userID <= 0 表示「没登录」，用来验证 RequireAuthenticated 的兜底。
func fakeAuth(userID int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if userID > 0 {
			c.Set(string(upstreammw.ContextKeyUser), upstreammw.AuthSubject{UserID: userID})
		}
		c.Next()
	}
}

// newTestEngine 按 RegisterBusinessRoutes 的真实结构搭一个引擎：
// 浏览器组挂假 jwtAuth，机器组挂假 Key 鉴权。
func newTestEngine(t *testing.T, services Services, userID int64) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	r := gin.New()
	v1 := r.Group("/api/v1")
	browser := v1.Group("/designkit")
	browser.Use(fakeAuth(userID))
	machine := r.Group("/v1/designkit")

	RegisterBusinessRoutes(BusinessRouteOptions{
		Browser:  browser,
		Machine:  machine,
		Services: services,
		KeyAuth:  fakeAuth(userID),
		// POST /jobs 在真实环境里挂的是上游 apiKeyAuth；这里用同一个假鉴权顶替，
		// 免得测「ERP 提交任务」时被 RequireAuthenticated 挡在门外。
		APIKeyAuth: upstreammw.APIKeyAuthMiddleware(fakeAuth(userID)),
	})
	return r
}

// doRequest 发一个请求并把响应录下来。
func doRequest(t *testing.T, engine *gin.Engine, method, path string, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// ---- 造数据 ----

func testTime() time.Time {
	return time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC)
}

func testJob() *dkdomain.Job {
	return &dkdomain.Job{
		ID:            1,
		UID:           "01J8ZK7Q9X2M4N6P8R0T2V4W6Y",
		UserID:        7,
		APIKeyID:      3,
		Origin:        dkdomain.OriginWeb,
		Name:          "夏季连衣裙",
		Status:        dkdomain.JobStatusHolding,
		Ratio:         dkdomain.Ratio3x4,
		Model:         dkdomain.DefaultModel,
		ItemCount:     6,
		Currency:      dkdomain.CurrencyUSD,
		EstimatedCost: dkdomain.MoneyFromFloat(0.42),
		CreatedAt:     testTime(),
		UpdatedAt:     testTime(),
	}
}

func testItemView(seq int, imageIndexes ...int) *JobItemView {
	images := make([]*dkdomain.Image, 0, len(imageIndexes))
	for _, idx := range imageIndexes {
		images = append(images, &dkdomain.Image{
			ID:          int64(seq*10 + idx),
			UID:         "01J8ZK7Q9X2M4N6P8R0T2V4W6Y",
			ImageIndex:  idx,
			Attempt:     1,
			IsCurrent:   true,
			ContentType: "image/png",
			CreatedAt:   testTime(),
		})
	}
	return &JobItemView{
		Item: &dkdomain.JobItem{
			ID:          int64(seq),
			JobID:       1,
			Seq:         seq,
			PromptText:  "纯白背景棚拍",
			Status:      dkdomain.ItemStatusPending,
			MaxAttempts: dkdomain.DefaultMaxAttempts,
			AvailableAt: testTime(),
			CreatedAt:   testTime(),
			UpdatedAt:   testTime(),
		},
		Images: images,
	}
}
