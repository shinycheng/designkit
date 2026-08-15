//go:build unit

package handler

import (
	"net/http"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// 停止排队（决策 21）
// ============================================================================

// 停止排队的响应必须把「哪些不扣费、哪些照扣费」说清楚。
//
// 这是决策 21 的全部理由：上游出图跟浏览器连接是**故意脱钩**的，已经在生成的
// 那几张停不下来、照样扣钱。响应里只给一个「已停止」而不说钱，
// 运营的下一句就是「我明明取消了还扣我钱」，而且他是对的。
func TestStopJobTellsWhoStillCostsMoney(t *testing.T) {
	jobs := &fakeJobService{stop: &StopJobResult{
		Job:          testJob(),
		Cancelled:    4,
		StillRunning: 2,
	}}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	for _, prefix := range []string{"/api/v1/designkit", "/v1/designkit"} {
		path := prefix + "/jobs/01J8ZK7Q9X2M4N6P8R0T2V4W6Y/stop"
		rec := doRequest(t, engine, http.MethodPost, path, "", nil)

		require.Equal(t, http.StatusOK, rec.Code, "路径 %s，响应体：%s", path, rec.Body.String())

		body := decodeJSON(t, rec.Body.Bytes())
		assert.Equal(t, float64(4), body["stopped_count"])
		assert.Equal(t, float64(2), body["still_running_count"])

		message, _ := body["message"].(string)
		assert.Contains(t, message, "不扣费", "必须说清哪些不花钱")
		assert.Contains(t, message, "正常扣费", "必须说清哪些照样花钱")
		assert.Contains(t, message, "作品库", "已经在跑的图会存下来，要告诉运营去哪儿找")
		assert.NotContains(t, message, "元", "金额一律美元，文案里不许出现「元」")
		assert.NotContains(t, message, "取消", "按钮叫「停止排队」，文案里不要再出现「取消」")

		// 批次本体也要回，界面拿它直接刷新那一行。
		job, ok := body["job"].(map[string]any)
		require.True(t, ok, "响应里要带上停完之后的批次：%s", rec.Body.String())
		assert.Equal(t, "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", job["uid"])

		assertNoCodeKey(t, body)
	}
	assert.Equal(t, 2, jobs.stopCall)
}

// 一张在跑的都没有时也要给一句完整的话，不能是半句。
func TestStopJobWithNothingRunning(t *testing.T) {
	jobs := &fakeJobService{stop: &StopJobResult{
		Job:          testJob(),
		Cancelled:    6,
		StillRunning: 0,
	}}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodPost,
		"/api/v1/designkit/jobs/01J8ZK7Q9X2M4N6P8R0T2V4W6Y/stop", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, "响应体：%s", rec.Body.String())
	message, _ := decodeJSON(t, rec.Body.Bytes())["message"].(string)
	assert.Contains(t, message, "6 张")
	assert.Contains(t, message, "不扣费")
	assert.NotContains(t, message, "正常扣费", "没有在跑的图就别提扣费，免得吓人")
}

// 已经结束的批次不能再停：409 + 一句人话。
func TestStopJobOnFinishedJobConflicts(t *testing.T) {
	jobs := &fakeJobService{stopErr: dkdomain.NewError(dkdomain.ErrCodeIllegalStateTransition).
		WithMessage("这批任务已经结束了，不用再停。")}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodPost,
		"/api/v1/designkit/jobs/01J8ZK7Q9X2M4N6P8R0T2V4W6Y/stop", "", nil)

	require.Equal(t, http.StatusConflict, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeIllegalStateTransition)
	assert.Equal(t, "这批任务已经结束了，不用再停。", errorObject(t, rec.Body.Bytes())["message"])
}

// 任务号不合法一律走「找不到」，不能泄露「这个号存在但不是你的」。
func TestStopJobRejectsBadUID(t *testing.T) {
	jobs := &fakeJobService{stop: &StopJobResult{Job: testJob()}}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/jobs/not-a-ulid/stop", "", nil)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeJobNotFound)
	assert.Zero(t, jobs.stopCall)
}

// ============================================================================
// 重试一张（决策 20）
// ============================================================================

const retryPath = "/jobs/01J8ZK7Q9X2M4N6P8R0T2V4W6Y/items/3/retry"

// testRetriedItem 是一张刚被重试、已经退回排队中的 item。
func testRetriedItem() *dkdomain.JobItem {
	return &dkdomain.JobItem{
		ID:           30,
		JobID:        1,
		Seq:          3,
		PromptText:   "纯白背景棚拍",
		Status:       dkdomain.ItemStatusPending,
		AttemptCount: 1,
		MaxAttempts:  dkdomain.DefaultMaxAttempts,
		AvailableAt:  testTime(),
		CreatedAt:    testTime(),
		UpdatedAt:    testTime(),
	}
}

// 重试**必须**带 Idempotency-Key，缺了直接 400，绝不真的去出图。
//
// 运营双击必然发生，而重试一张 = 重新出一张图 = 重新收一次钱。
// 也不能指望上游协调器的 RequireKey：那个开关默认「只观察不拦截」，
// 不带 key 会直接放行执行。
func TestRetryItemRejectsMissingIdempotencyKey(t *testing.T) {
	jobs := &fakeJobService{retry: testRetriedItem()}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	for _, prefix := range []string{"/api/v1/designkit", "/v1/designkit"} {
		rec := doRequest(t, engine, http.MethodPost, prefix+retryPath, "", nil)

		require.Equal(t, http.StatusBadRequest, rec.Code, "前缀 %s", prefix)
		assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeIdempotencyKeyRequired)
	}
	assert.Zero(t, jobs.retryCall, "缺 key 时一次都不许真的去重试（那是要花钱的）")
}

// 重试成功：必须说明「会重新收一次费用」，并告诉运营还能再试几次。
func TestRetryItemSaysItCostsMoneyAgain(t *testing.T) {
	jobs := &fakeJobService{retry: testRetriedItem()}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit"+retryPath, "",
		map[string]string{idempotencyHeader: "retry-0001"})

	require.Equal(t, http.StatusOK, rec.Code, "响应体：%s", rec.Body.String())

	body := decodeJSON(t, rec.Body.Bytes())
	message, _ := body["message"].(string)
	assert.Contains(t, message, "重新收一次费用", "重试就是重新花一次钱，必须说出来")
	assert.NotContains(t, message, "元", "金额一律美元，文案里不许出现「元」")
	assert.Equal(t, float64(dkdomain.DefaultMaxAttempts-1), body["remaining_attempts"])

	item, ok := body["item"].(map[string]any)
	require.True(t, ok, "响应里要带上这一张的最新状态：%s", rec.Body.String())
	assert.Equal(t, float64(3), item["seq"])
	assert.Equal(t, "pending", item["status"])
	assert.Equal(t, float64(1), item["attempt_count"])
	assertNoCodeKey(t, body)

	require.Equal(t, 1, jobs.retryCall)
	assert.Equal(t, testUserID, jobs.lastRetry.UserID)
	assert.Equal(t, "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", jobs.lastRetry.JobUID)
	assert.Equal(t, 3, jobs.lastRetry.Seq)
	assert.Equal(t, "retry-0001", jobs.lastRetry.IdempotencyKey, "防重复标识要原样传给 service")
}

// 试满次数之后不能再试：409 + 一句人话，不是「状态不对」这种黑话。
func TestRetryItemMaxAttemptsExceeded(t *testing.T) {
	jobs := &fakeJobService{retryErr: dkdomain.NewError(dkdomain.ErrCodeMaxAttemptsExceeded)}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit"+retryPath, "",
		map[string]string{idempotencyHeader: "retry-0002"})

	require.Equal(t, http.StatusConflict, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeMaxAttemptsExceeded)
}

// 批次已经结算就不能再重试，且提示要告诉运营「重新提交」这条出路。
func TestRetryItemOnSettledJob(t *testing.T) {
	jobs := &fakeJobService{retryErr: dkdomain.NewError(dkdomain.ErrCodeJobAlreadySettled)}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit"+retryPath, "",
		map[string]string{idempotencyHeader: "retry-0003"})

	require.Equal(t, http.StatusConflict, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeJobAlreadySettled)
	assert.Contains(t, errorObject(t, rec.Body.Bytes())["message"], "重新提交")
}

// 余额不够时重试也要被拦下，并且是 402 + 「还差多少」。
func TestRetryItemInsufficientBalance(t *testing.T) {
	jobs := &fakeJobService{retryErr: &dkdomain.InsufficientBalanceError{
		Required:  dkdomain.MoneyFromFloat(0.12),
		Available: dkdomain.MoneyFromFloat(0.02),
		Shortfall: dkdomain.MoneyFromFloat(0.1),
	}}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit"+retryPath, "",
		map[string]string{idempotencyHeader: "retry-0004"})

	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInsufficientBalance)
	assert.NotContains(t, errorObject(t, rec.Body.Bytes())["message"], "元")
}

// 序号从 1 开始是对外契约：0 和负数一律「找不到这一张」，不许真去重试。
func TestRetryItemRejectsBadSeq(t *testing.T) {
	jobs := &fakeJobService{retry: testRetriedItem()}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	for _, seq := range []string{"0", "abc"} {
		path := "/api/v1/designkit/jobs/01J8ZK7Q9X2M4N6P8R0T2V4W6Y/items/" + seq + "/retry"
		rec := doRequest(t, engine, http.MethodPost, path, "",
			map[string]string{idempotencyHeader: "retry-bad-seq"})

		require.Equal(t, http.StatusNotFound, rec.Code, "seq=%s", seq)
		assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeItemNotFound)
	}
	assert.Zero(t, jobs.retryCall)
}
