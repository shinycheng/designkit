//go:build unit

package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testUserID int64 = 7

func testServices(jobs *fakeJobService) Services {
	return testServicesWithMe(jobs, &fakeMeService{})
}

// testServicesWithMe 同上，但可以指定「我的消费」的假实现。
func testServicesWithMe(jobs *fakeJobService, me MeService) Services {
	return Services{
		Assets:  &fakeAssetService{},
		Catalog: &fakeCatalogService{},
		Jobs:    jobs,
		Images:  &fakeImageService{},
		Me:      me,
	}
}

// decodeJSON 把响应体解成 map，顺便断言它确实是 JSON。
func decodeJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out), "响应体不是合法 JSON：%s", string(raw))
	return out
}

// errorObject 取出错误响应里的 error 对象，顺便断言外层形状。
func errorObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body struct {
		Error map[string]any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &body), "响应体不是合法 JSON：%s", string(raw))
	require.NotNil(t, body.Error, "错误响应必须是 {\"error\":{...}}：%s", string(raw))
	return body.Error
}

// assertErrorEnvelope 断言这是 designkit 唯一承诺的那种错误格式。
func assertErrorEnvelope(t *testing.T, raw []byte, wantCode string) {
	t.Helper()

	body := decodeJSON(t, raw)
	errObj := errorObject(t, raw)

	assert.Equal(t, wantCode, errObj["error_code"], "error_code 不对：%s", string(raw))
	assert.NotEmpty(t, errObj["type"], "type 不能为空")
	assert.NotEmpty(t, errObj["message"], "message 不能为空（运营要看中文）")
	assert.NotEmpty(t, errObj["request_id"], "request_id 不能为空（排障靠它）")

	// 对外 JSON 任何层级都不许出现名为 code 的字段：
	// 上游幂等协调器落库前会把它脱敏成 ***，重放时 ERP 就拿到 *** 了。
	assertNoCodeKey(t, body)
}

func assertNoCodeKey(t *testing.T, value any) {
	t.Helper()
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			assert.NotEqual(t, "code", key, "对外 JSON 里不许出现名为 code 的字段")
			assertNoCodeKey(t, child)
		}
	case []any:
		for _, child := range v {
			assertNoCodeKey(t, child)
		}
	}
}

const validCreateBody = `{"ratio":"3:4","asset_uids":["01J8ZK7Q9X2M4N6P8R0T2V4W6Y"],"prompt_uids":["01J8ZK7Q9X2M4N6P8R0T2V4W6Z"],"name":"夏季连衣裙"}`

// 缺 Idempotency-Key 必须由**我们的 handler**直接 400。
//
// 不能依赖上游协调器的 RequireKey：它受 ObserveOnly 开关控制，而那个开关默认 true，
// 不带 key 会直接放行执行 —— ERP 一次超时重发就是重复下单、重复扣钱。
func TestCreateJobRejectsMissingIdempotencyKey(t *testing.T) {
	jobs := &fakeJobService{job: testJob()}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	for _, path := range []string{"/api/v1/designkit/jobs", "/v1/designkit/jobs"} {
		rec := doRequest(t, engine, http.MethodPost, path, validCreateBody, nil)

		require.Equal(t, http.StatusBadRequest, rec.Code, "路径 %s", path)
		assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeIdempotencyKeyRequired)
		assert.NotEmpty(t, rec.Header().Get(RequestIDHeader), "每个响应都要回 X-Designkit-Request-Id")
		assert.Zero(t, jobs.createCall, "缺 key 时绝不能真的去建任务")
	}
}

// 空白字符串也算没带（有些 HTTP 客户端会自动补一个空头）。
func TestCreateJobRejectsBlankIdempotencyKey(t *testing.T) {
	jobs := &fakeJobService{job: testJob()}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/jobs", validCreateBody,
		map[string]string{idempotencyHeader: "   "})

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeIdempotencyKeyRequired)
	assert.Zero(t, jobs.createCall)
}

// key 规格：1~128 个可见 ASCII（33~126）。中文、空格、超长一律 400。
func TestCreateJobRejectsMalformedIdempotencyKey(t *testing.T) {
	jobs := &fakeJobService{job: testJob()}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	longKey := ""
	for i := 0; i < 129; i++ {
		longKey += "a"
	}

	for _, key := range []string{"有中文的键", "带 空格", longKey} {
		rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/jobs", validCreateBody,
			map[string]string{idempotencyHeader: key})

		require.Equal(t, http.StatusBadRequest, rec.Code, "key=%q", key)
		assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
	}
	assert.Zero(t, jobs.createCall)
}

// 提交成功只返小对象：**绝不内联 items 数组**，而且要小于 8KB。
//
// 这个响应体会被上游幂等协调器整个存进数据库、重放时原样吐回来；
// 内联 50 条 item 既撑大存储，又会在重放时给出一份过时的进度。
func TestCreateJobReturnsSmallPayload(t *testing.T) {
	jobs := &fakeJobService{job: testJob()}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/jobs", validCreateBody,
		map[string]string{idempotencyHeader: "order-2026-08-13-0001"})

	require.Equal(t, http.StatusCreated, rec.Code, "响应体：%s", rec.Body.String())
	assert.Less(t, rec.Body.Len(), 8*1024, "POST /jobs 的响应必须小于 8KB")

	body := decodeJSON(t, rec.Body.Bytes())
	assert.Equal(t, "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", body["uid"])
	assert.Equal(t, "holding", body["status"])
	assert.Equal(t, float64(6), body["item_count"])
	assert.Equal(t, dkdomain.CurrencyUSD, body["currency"])
	assert.Equal(t, "0.42", body["estimated_cost"], "金额是字符串，不是浮点数")

	_, hasItems := body["items"]
	assert.False(t, hasItems, "成功响应绝不能内联 items 数组")
	assertNoCodeKey(t, body)

	require.Equal(t, 1, jobs.createCall)
	assert.Equal(t, "order-2026-08-13-0001", jobs.lastCreate.IdempotencyKey)
	assert.Equal(t, testUserID, jobs.lastCreate.UserID)
	assert.Equal(t, dkdomain.OriginWeb, jobs.lastCreate.Origin, "浏览器前缀进来的必须记成 web")
}

// 同一批 handler 挂两个前缀：ERP 那条进来的 origin 必须是 erp。
func TestCreateJobFromMachinePrefixIsERP(t *testing.T) {
	jobs := &fakeJobService{job: testJob()}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/v1/designkit/jobs", validCreateBody,
		map[string]string{idempotencyHeader: "erp-0001"})

	require.Equal(t, http.StatusCreated, rec.Code, "响应体：%s", rec.Body.String())
	require.Equal(t, 1, jobs.createCall)
	assert.Equal(t, dkdomain.OriginERP, jobs.lastCreate.Origin)
}

// 余额不够时把「还差多少」写进中文文案里，并且是 402。
func TestCreateJobInsufficientBalance(t *testing.T) {
	jobs := &fakeJobService{createErr: &dkdomain.InsufficientBalanceError{
		Required:  dkdomain.MoneyFromFloat(1.2),
		Available: dkdomain.MoneyFromFloat(0.3),
		Shortfall: dkdomain.MoneyFromFloat(0.9),
	}}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/jobs", validCreateBody,
		map[string]string{idempotencyHeader: "order-1"})

	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInsufficientBalance)

	message := errorObject(t, rec.Body.Bytes())["message"]
	assert.Contains(t, message, "$0.9000", "要告诉运营还差多少")
	assert.NotContains(t, message, "元", "金额一律美元，文案里不许出现「元」")
}

// 参数校验：一张商品图都没选。
func TestCreateJobRejectsEmptySelection(t *testing.T) {
	jobs := &fakeJobService{job: testJob()}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/jobs",
		`{"ratio":"3:4","asset_uids":[],"prompts":["纯白背景"]}`,
		map[string]string{idempotencyHeader: "order-1"})

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
	assert.Zero(t, jobs.createCall)
}

// 比例格式不对 → DK_RATIO_NOT_ALLOWED（不是笼统的参数错误，运营要知道去改比例）。
func TestCreateJobRejectsBadRatio(t *testing.T) {
	jobs := &fakeJobService{job: testJob()}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/jobs",
		`{"ratio":"1536x1024","asset_uids":["01J8ZK7Q9X2M4N6P8R0T2V4W6Y"],"prompts":["纯白背景"]}`,
		map[string]string{idempotencyHeader: "order-1"})

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeRatioNotAllowed)
}

// **ERP 契约**：GET /jobs/:uid/items 必须按 seq 升序返回，图片按 image_index 升序。
//
// 这一条破了不会有任何报错，只会让 ERP 把图配错商品。
func TestJobItemsAreOrderedBySeq(t *testing.T) {
	jobs := &fakeJobService{items: []*JobItemView{
		testItemView(3),
		testItemView(1, 2, 1),
		testItemView(2),
	}}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/jobs/01J8ZK7Q9X2M4N6P8R0T2V4W6Y/items", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, "响应体：%s", rec.Body.String())

	var body struct {
		Items []struct {
			Seq    int `json:"seq"`
			Images []struct {
				ImageIndex int `json:"image_index"`
			} `json:"images"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Items, 3)

	assert.Equal(t, []int{1, 2, 3}, []int{body.Items[0].Seq, body.Items[1].Seq, body.Items[2].Seq})

	require.Len(t, body.Items[0].Images, 2)
	assert.Equal(t, 1, body.Items[0].Images[0].ImageIndex)
	assert.Equal(t, 2, body.Items[0].Images[1].ImageIndex)
}

// item 上只给我方错误码 + 中文；上游返回的英文原文绝不进响应体。
func TestJobItemHidesUpstreamErrorText(t *testing.T) {
	code := dkdomain.ErrCodeUpstreamBusy
	upstream := "Image generation concurrency limit exceeded, please retry later"

	view := testItemView(1)
	view.Item.Status = dkdomain.ItemStatusFailed
	view.Item.LastErrorCode = &code
	view.Item.LastErrorMessage = &upstream

	jobs := &fakeJobService{items: []*JobItemView{view}}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/jobs/01J8ZK7Q9X2M4N6P8R0T2V4W6Y/items", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	raw := rec.Body.String()
	assert.NotContains(t, raw, "concurrency limit exceeded", "上游英文原文只给管理员看，不能进接口")
	assert.Contains(t, raw, dkdomain.ErrCodeUpstreamBusy)
	assert.Contains(t, raw, "出图通道正忙", "运营看到的必须是中文")
}

// 找不到任务时统一走 DK_JOB_NOT_FOUND；归属不匹配也走这条，不返回 403。
func TestGetJobNotFound(t *testing.T) {
	jobs := &fakeJobService{getErr: dkdomain.ErrNotFound}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/jobs/01J8ZK7Q9X2M4N6P8R0T2V4W6Y", "", nil)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeJobNotFound)
}

// 没登录（鉴权中间件缺位）时不许拿着 userID=0 去查数据。
func TestRequireAuthenticated(t *testing.T) {
	jobs := &fakeJobService{job: testJob()}
	engine := newTestEngine(t, testServices(jobs), 0)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/jobs", "", nil)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeUnauthorized)
}

// 列表走游标分页，不返回 offset/page/total 这类会漏数据的字段。
func TestListJobsUsesCursor(t *testing.T) {
	jobs := &fakeJobService{page: &dkdomain.JobPage{
		Jobs:                []*dkdomain.Job{testJob()},
		HasMore:             true,
		NextCursorCreatedAt: testTime(),
		NextCursorID:        42,
	}}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/jobs?limit=1", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, "响应体：%s", rec.Body.String())

	body := decodeJSON(t, rec.Body.Bytes())
	assert.Equal(t, true, body["has_more"])
	cursor, ok := body["next_cursor"].(string)
	require.True(t, ok, "有下一页时必须给游标")

	createdAt, id, err := decodeCursor(cursor)
	require.NoError(t, err)
	assert.Equal(t, int64(42), id)
	assert.True(t, createdAt.Equal(testTime()))

	for _, forbidden := range []string{"page", "page_size", "total", "offset"} {
		_, exists := body[forbidden]
		assert.False(t, exists, "游标分页不该出现 %s 字段", forbidden)
	}
}

// 状态过滤只认合法取值，写错了要明确报错而不是静默返回全部。
func TestListJobsRejectsUnknownStatus(t *testing.T) {
	jobs := &fakeJobService{page: &dkdomain.JobPage{}}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/jobs?status=flying", "", nil)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
}

// 取图走我们自己的下载端点：直接回字节，不回 URL（对象存储直链 24 小时就失效）。
func TestItemContentReturnsRawBytes(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	jobs := &fakeJobService{blob: &ContentBlob{
		Data:        png,
		ContentType: "image/png",
		Filename:    "job-1-1.png",
	}}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/jobs/01J8ZK7Q9X2M4N6P8R0T2V4W6Y/items/1/content", "", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, png, rec.Body.Bytes(), "成功响应必须原样透传，不能被错误改写中间件碰")
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Cache-Control"), "private", "鉴权后的图不许进共享缓存")
	assert.Equal(t, `inline; filename="job-1-1.png"`, rec.Header().Get("Content-Disposition"))
}

// 图还没出来（服务返回空 blob）时给 DK_IMAGE_NOT_FOUND，不要回 200 + 空 body。
func TestItemContentMissing(t *testing.T) {
	jobs := &fakeJobService{blob: &ContentBlob{}}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/jobs/01J8ZK7Q9X2M4N6P8R0T2V4W6Y/items/1/content", "", nil)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeImageNotFound)
}

// 报价：价格待确认时必须把那句中文说明一起给出去。
//
// unit_price 和 estimated_cost 都是 null，界面上如果连一句解释都没有，
// 运营看到的就是一个空价格 —— 分不清「这一批不要钱」还是「系统坏了」，
// 只能不敢点。这个字段被适配器丢掉过一次，所以钉一条测试守着。
func TestEstimateCarriesPriceNote(t *testing.T) {
	const note = "出图单价还没实测确认，这一批的花费暂时算不出来。"
	jobs := &fakeJobService{estimate: &EstimateResult{
		ItemCount:     6,
		AssetCount:    2,
		PromptCount:   3,
		MaxBatchItems: 50,
		PricingTier:   dkdomain.BillingTier2K,
		PriceNote:     note,
		Balance:       dkdomain.MoneyFromFloat(12.3),
		Available:     dkdomain.MoneyFromFloat(9.8),
		Sufficient:    true,
		Shortfall:     dkdomain.ZeroMoney,
	}}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/jobs/estimate",
		`{"ratio":"3:4","asset_uids":["01J8ZK7Q9X2M4N6P8R0T2V4W6Y"],"prompts":["纯白背景棚拍"]}`, nil)

	require.Equal(t, http.StatusOK, rec.Code, "响应体：%s", rec.Body.String())

	body := decodeJSON(t, rec.Body.Bytes())
	assert.Equal(t, false, body["price_confirmed"])
	assert.Nil(t, body["unit_price"], "没实测出来之前不许给任何猜的单价")
	assert.Nil(t, body["estimated_cost"])
	assert.Equal(t, note, body["price_note"], "价格待确认时必须给运营一句中文说明")
	assert.Equal(t, dkdomain.CurrencyUSD, body["currency"])
	assert.NotContains(t, rec.Body.String(), "元", "金额一律美元")
	assertNoCodeKey(t, body)
}

// 比例列表：单价没实测出来之前必须是 null + price_confirmed=false。
func TestRatiosMarkUnconfirmedPrice(t *testing.T) {
	services := testServices(&fakeJobService{})
	services.Catalog = &fakeCatalogService{options: []RatioOption{
		{Ratio: dkdomain.Ratio1x1, TargetWidth: 2048, TargetHeight: 2048, PricingTier: dkdomain.BillingTier2K, IsDefault: true},
	}}
	engine := newTestEngine(t, services, testUserID)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/ratios", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Currency string           `json:"currency"`
		Ratios   []map[string]any `json:"ratios"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, dkdomain.CurrencyUSD, body.Currency)
	require.Len(t, body.Ratios, 1)

	first := body.Ratios[0]
	assert.Equal(t, "1:1", first["ratio"])
	assert.Nil(t, first["unit_price"], "没实测出来之前不许给任何猜的单价")
	assert.Equal(t, false, first["price_confirmed"])
	assert.Equal(t, float64(2048), first["target_width"])
}
