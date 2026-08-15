//go:build unit

package handler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// 我的消费（决策 16 / 19）
// ============================================================================

func testUsageSummary() *UsageSummary {
	return &UsageSummary{
		PeriodStart:  testTime(),
		PeriodEnd:    testTime().AddDate(0, 1, 0),
		ImageCount:   132,
		Cost:         dkdomain.MoneyFromFloat(4.56),
		Balance:      dkdomain.MoneyFromFloat(12.3),
		Available:    dkdomain.MoneyFromFloat(9.8),
		AdminContact: "运营群 @小王",
	}
}

// 工作台角落那三个数：本月出图 N 张 / 花费 $X / 余额 $Y。
//
// **金额一律美元**（决策 18）：底层余额和账单全是美元，换算会导致
// 「工作台说还差 12.3 元、个人资料显示 $12.30」这种充错钱的情况。
func TestUsageSummaryIsAlwaysUSD(t *testing.T) {
	me := &fakeMeService{summary: testUsageSummary()}
	engine := newTestEngine(t, testServicesWithMe(&fakeJobService{}, me), testUserID)

	for _, prefix := range []string{"/api/v1/designkit", "/v1/designkit"} {
		rec := doRequest(t, engine, http.MethodGet, prefix+"/me/usage/summary", "", nil)

		require.Equal(t, http.StatusOK, rec.Code, "前缀 %s，响应体：%s", prefix, rec.Body.String())

		body := decodeJSON(t, rec.Body.Bytes())
		assert.Equal(t, dkdomain.CurrencyUSD, body["currency"])
		assert.Equal(t, float64(132), body["image_count"])
		assert.Equal(t, "4.56", body["cost"], "金额是字符串，不是浮点数")
		assert.Equal(t, "12.3", body["balance"])
		assert.Equal(t, "9.8", body["available"])
		assert.Equal(t, "运营群 @小王", body["admin_contact"])
		assert.NotEmpty(t, body["period_start"], "要说清统计的是哪一段时间")
		assert.NotEmpty(t, body["period_end"])

		assert.NotContains(t, rec.Body.String(), "元", "接口里不许出现「元」")
		assertNoCodeKey(t, body)
	}
}

// 没配管理员联系方式时也要正常返回，只是那一行为空 —— 绝不能因此报错。
func TestUsageSummaryWithoutAdminContact(t *testing.T) {
	summary := testUsageSummary()
	summary.AdminContact = ""
	me := &fakeMeService{summary: summary}
	engine := newTestEngine(t, testServicesWithMe(&fakeJobService{}, me), testUserID)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/me/usage/summary", "", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "", decodeJSON(t, rec.Body.Bytes())["admin_contact"])
}

// service 报错时给的是我方格式的错误，不是一个半截的 200。
func TestUsageSummaryServiceError(t *testing.T) {
	me := &fakeMeService{summaryErr: dkdomain.ErrNotFound}
	engine := newTestEngine(t, testServicesWithMe(&fakeJobService{}, me), testUserID)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/me/usage/summary", "", nil)

	// 这里「查不到」只可能是用户行没了，所以翻成「登录已过期，请重新登录」。
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeUnauthorized)
}

// 「我的消费」缺席时只关掉它自己那两个端点，出图和查询照常。
func TestMeRoutesAbsentWhenServiceMissing(t *testing.T) {
	services := testServicesWithMe(&fakeJobService{}, nil)
	engine := newTestEngine(t, services, testUserID)

	rec := doRequest(t, engine, http.MethodGet, "/api/v1/designkit/me/usage/summary", "", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// 出图那条线必须还在。
	require.True(t, services.Ready(), "Me 不该影响 Ready()")
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/designkit/jobs", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "响应体：%s", rec.Body.String())
}

// ---- 申请额度 ----

func testQuotaRequest(note *string) *QuotaRequestResult {
	return &QuotaRequestResult{
		Request: &dkdomain.QuotaRequest{
			ID:        9,
			UserID:    testUserID,
			Note:      note,
			Status:    dkdomain.QuotaRequestPending,
			CreatedAt: testTime(),
		},
		AdminContact: "运营群 @小王",
	}
}

// 「申请额度」按钮点一下就能发：**不填任何东西也要能提交**。
//
// 让一个已经被余额挡住的人先填表单才能求助，是最容易被放弃的一步。
func TestCreateQuotaRequestAcceptsEmptyBody(t *testing.T) {
	me := &fakeMeService{quota: testQuotaRequest(nil)}
	engine := newTestEngine(t, testServicesWithMe(&fakeJobService{}, me), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/me/quota-requests", "", nil)

	require.Equal(t, http.StatusCreated, rec.Code, "响应体：%s", rec.Body.String())

	body := decodeJSON(t, rec.Body.Bytes())
	assert.Equal(t, "pending", body["status"])
	assert.Equal(t, "运营群 @小王", body["admin_contact"])
	assert.Contains(t, body["message"], "运营群 @小王", "要告诉运营去找谁")
	assert.NotContains(t, rec.Body.String(), "元")

	// 内部自增主键一律不对外暴露。
	_, hasID := body["id"]
	assert.False(t, hasID, "不要把数据库自增 id 发出去")
	assertNoCodeKey(t, body)

	require.Equal(t, 1, me.quotaCall)
	assert.Equal(t, "", me.lastNote)
}

// 填了说明就原样带给 service（去掉首尾空白）。
func TestCreateQuotaRequestKeepsNote(t *testing.T) {
	note := "双十一主图要赶工，还差大概 30 张"
	me := &fakeMeService{quota: testQuotaRequest(&note)}
	engine := newTestEngine(t, testServicesWithMe(&fakeJobService{}, me), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/me/quota-requests",
		`{"note":"  双十一主图要赶工，还差大概 30 张  "}`, nil)

	require.Equal(t, http.StatusCreated, rec.Code, "响应体：%s", rec.Body.String())
	require.Equal(t, 1, me.quotaCall)
	assert.Equal(t, note, me.lastNote)
	assert.Equal(t, note, decodeJSON(t, rec.Body.Bytes())["note"])
}

// 说明太长就明说「精简一下」，**不要静默截断** ——
// 截断之后管理员看到的是半句话，还不知道被截过。
func TestCreateQuotaRequestRejectsTooLongNote(t *testing.T) {
	me := &fakeMeService{quota: testQuotaRequest(nil)}
	engine := newTestEngine(t, testServicesWithMe(&fakeJobService{}, me), testUserID)

	long := strings.Repeat("图", maxQuotaRequestNoteRunes+1)
	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/me/quota-requests",
		fmt.Sprintf(`{"note":%q}`, long), nil)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeInvalidRequest)
	assert.Contains(t, errorObject(t, rec.Body.Bytes())["message"], "精简")
	assert.Zero(t, me.quotaCall)
}

// 已经有一条没处理完的申请时，说「不用重复提交」，不要甩一句技术黑话。
func TestCreateQuotaRequestDuplicateIsFriendly(t *testing.T) {
	me := &fakeMeService{quotaErr: fmt.Errorf("designkit: 额度申请 唯一约束冲突: %w", dkdomain.ErrConflict)}
	engine := newTestEngine(t, testServicesWithMe(&fakeJobService{}, me), testUserID)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/me/quota-requests", "", nil)

	require.Equal(t, http.StatusConflict, rec.Code)
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeIllegalStateTransition)

	message := errorObject(t, rec.Body.Bytes())["message"]
	assert.Contains(t, message, "不用重复提交")
	assert.NotContains(t, message, "唯一约束", "数据库的话不要给运营看")
}
