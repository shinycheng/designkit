//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	upstreammw "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// 错误格式改写
// ============================================================================

// 上游 middleware.AbortWithError 吐的是 {"code":"INSUFFICIENT_BALANCE",...}，
// 必须被改写成我方格式，**并且保留原始 HTTP 状态码**。
func TestErrorEnvelopeRewritesUpstreamAbort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v1/designkit")
	g.Use(ErrorEnvelope())
	g.GET("/probe", func(c *gin.Context) {
		upstreammw.AbortWithError(c, http.StatusForbidden, "INSUFFICIENT_BALANCE", "Insufficient account balance")
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/designkit/probe", nil))

	require.Equal(t, http.StatusForbidden, rec.Code, "状态码要保持上游给的那个")

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	errObj := errorObject(t, rec.Body.Bytes())
	assert.Equal(t, dkdomain.ErrCodeInsufficientBalance, errObj["error_code"])
	assert.NotEmpty(t, errObj["request_id"])
	assert.NotContains(t, rec.Body.String(), "Insufficient account balance", "上游英文原文不进响应体")

	// 顶层的 code 字段必须消失：留着会被上游幂等协调器脱敏成 ***。
	_, hasCode := body["code"]
	assert.False(t, hasCode)
	assert.NotEmpty(t, rec.Header().Get(RequestIDHeader))
}

// pkg/response 那种「code 是整数」的信封同样要被改写。
func TestErrorEnvelopeRewritesNumericCodeBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v1/designkit")
	g.Use(ErrorEnvelope())
	g.GET("/probe", func(c *gin.Context) {
		c.JSON(http.StatusTooManyRequests, gin.H{"code": 429, "message": "slow down", "reason": "RATE_LIMITED"})
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/designkit/probe", nil))

	require.Equal(t, http.StatusTooManyRequests, rec.Code)

	errObj := errorObject(t, rec.Body.Bytes())
	assert.Equal(t, dkdomain.ErrCodeRateLimited, errObj["error_code"])
	assert.Equal(t, dkdomain.ErrorTypeRateLimit, errObj["type"])
}

// 认不出来的错误体按状态码兜底，绝不把上游原文透出去。
func TestErrorEnvelopeFallsBackByStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v1/designkit")
	g.Use(ErrorEnvelope())
	g.GET("/probe", func(c *gin.Context) {
		c.String(http.StatusBadGateway, "upstream exploded at 10.0.0.7")
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/designkit/probe", nil))

	require.Equal(t, http.StatusBadGateway, rec.Code)
	assert.NotContains(t, rec.Body.String(), "10.0.0.7")

	errObj := errorObject(t, rec.Body.Bytes())
	assert.Equal(t, dkdomain.ErrCodeInternal, errObj["error_code"])
	assert.Equal(t, dkdomain.ErrorTypeAPI, errObj["type"])
}

// 成功响应必须原样透传：/content 端点回的是图片字节，不能被缓冲或改写。
func TestErrorEnvelopePassesThroughSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v1/designkit")
	g.Use(ErrorEnvelope())
	g.GET("/probe", func(c *gin.Context) {
		c.Data(http.StatusOK, "image/png", []byte{0x89, 'P', 'N', 'G'})
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/designkit/probe", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []byte{0x89, 'P', 'N', 'G'}, rec.Body.Bytes())
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
}

// 已经是我方格式的错误体不做二次包装。
func TestErrorEnvelopeKeepsDesignkitBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v1/designkit")
	g.Use(ErrorEnvelope())
	g.GET("/probe", func(c *gin.Context) {
		failCode(c, dkdomain.ErrCodeBatchTooLarge)
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/designkit/probe", nil))

	require.Equal(t, http.StatusBadRequest, rec.Code)

	errObj := errorObject(t, rec.Body.Bytes())
	assert.Equal(t, dkdomain.ErrCodeBatchTooLarge, errObj["error_code"])
	assert.Equal(t, dkdomain.NewError(dkdomain.ErrCodeBatchTooLarge).Message, errObj["message"])
}

// ============================================================================
// 轻量 Key 鉴权
// ============================================================================

type fakeKeyLookup struct {
	key       *upstreamservice.APIKey
	err       error
	touchedID int64
}

func (f *fakeKeyLookup) GetByKey(_ context.Context, _ string) (*upstreamservice.APIKey, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.key, nil
}

func (f *fakeKeyLookup) TouchLastUsed(_ context.Context, id int64) error {
	f.touchedID = id
	return nil
}

func testAPIKey(status string) *upstreamservice.APIKey {
	return &upstreamservice.APIKey{
		ID:     11,
		UserID: 7,
		Status: status,
		User: &upstreamservice.User{
			ID:          7,
			Status:      upstreamservice.StatusActive,
			Role:        "user",
			Concurrency: 5,
		},
	}
}

func keyAuthEngine(lookup APIKeyLookup) (*gin.Engine, *int64) {
	gin.SetMode(gin.TestMode)
	var seenUserID int64

	r := gin.New()
	g := r.Group("/v1/designkit")
	g.Use(ErrorEnvelope(), KeyAuth(lookup, nil))
	g.GET("/probe", func(c *gin.Context) {
		if subject, ok := upstreammw.GetAuthSubjectFromContext(c); ok {
			seenUserID = subject.UserID
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r, &seenUserID
}

// **本轮最重要的一条**：额度耗尽的 Key 仍然能查任务、取图。
//
// 上游 apiKeyAuth 会在这一步把请求拦掉（它的计费准入只对三条路径跳过，
// /v1/designkit/* 不在里面），而我们恰恰建议给 ERP Key 设日消费封顶 ——
// 挂上游那套等于「花完钱就再也拿不到已经付过钱的图」。
func TestKeyAuthAllowsQuotaExhaustedKey(t *testing.T) {
	for _, status := range []string{
		upstreamservice.StatusAPIKeyActive,
		upstreamservice.StatusAPIKeyQuotaExhausted,
		upstreamservice.StatusAPIKeyExpired,
	} {
		lookup := &fakeKeyLookup{key: testAPIKey(status)}
		engine, seen := keyAuthEngine(lookup)

		req := httptest.NewRequest(http.MethodGet, "/v1/designkit/probe", nil)
		req.Header.Set("Authorization", "Bearer sk-test")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code, "status=%s 时不该被拦：%s", status, rec.Body.String())
		assert.Equal(t, int64(7), *seen, "要把 UserID 放进上下文，handler 只认这个")
		assert.Equal(t, int64(11), lookup.touchedID)
	}
}

// 停用的 Key 必须拒。
func TestKeyAuthRejectsDisabledKey(t *testing.T) {
	lookup := &fakeKeyLookup{key: testAPIKey(upstreamservice.StatusAPIKeyDisabled)}
	engine, _ := keyAuthEngine(lookup)

	req := httptest.NewRequest(http.MethodGet, "/v1/designkit/probe", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)

	assert.Equal(t, dkdomain.ErrCodeUnauthorized, errorObject(t, rec.Body.Bytes())["error_code"])
}

// 没带 Key 时给一句 ERP 看得懂的话，而不是「请重新登录」。
func TestKeyAuthRejectsMissingKey(t *testing.T) {
	lookup := &fakeKeyLookup{key: testAPIKey(upstreamservice.StatusAPIKeyActive)}
	engine, _ := keyAuthEngine(lookup)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/designkit/probe", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)

	errObj := errorObject(t, rec.Body.Bytes())
	assert.Equal(t, dkdomain.ErrCodeUnauthorized, errObj["error_code"])
	assert.Contains(t, errObj["message"], "Authorization")
}

// Key 放在网址参数里一律拒：它会被反代和各级日志原样记下来。
func TestKeyAuthRejectsKeyInQuery(t *testing.T) {
	lookup := &fakeKeyLookup{key: testAPIKey(upstreamservice.StatusAPIKeyActive)}
	engine, _ := keyAuthEngine(lookup)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/designkit/probe?api_key=sk-test", nil))

	require.Equal(t, http.StatusBadRequest, rec.Code)

	assert.Equal(t, dkdomain.ErrCodeInvalidRequest, errorObject(t, rec.Body.Bytes())["error_code"])
}

// ============================================================================
// 单个 Context 级别的行为（gin.CreateTestContext）
// ============================================================================

func TestAbortWithDesignkitErrorUsesSameRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/designkit/jobs", nil)

	requestID := ensureRequestID(c)
	require.NotEmpty(t, requestID)
	assert.Equal(t, requestID, ensureRequestID(c), "同一次请求里必须稳定")

	abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeJobNotFound))

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.True(t, c.IsAborted(), "写完错误必须中断后续 handler")

	errObj := errorObject(t, rec.Body.Bytes())
	assert.Equal(t, dkdomain.ErrCodeJobNotFound, errObj["error_code"])
	assert.Equal(t, requestID, errObj["request_id"], "响应体里的号要跟响应头一致")
	assert.Equal(t, dkdomain.ErrorTypeInvalidRequest, errObj["type"])
}

// 上游原文只进日志，绝不进响应体。
func TestAbortWithDesignkitErrorHidesUpstreamText(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/designkit/jobs", nil)

	dkErr := dkdomain.MapUpstreamError(http.StatusForbidden, "Image generation is not enabled for this group")
	abortWithDesignkitError(c, dkErr)

	assert.Equal(t, dkdomain.ErrCodeImageNotEnabled, dkErr.Code)
	assert.NotContains(t, rec.Body.String(), "Image generation is not enabled")
	assert.Contains(t, rec.Body.String(), "分组没有开通出图功能")
}

// MountContext 决定 origin 和 content_url 的前缀 —— 这是路由事实，不是鉴权事实。
func TestMountContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/designkit/jobs", nil)

	assert.Equal(t, dkdomain.OriginWeb, originOf(c), "没设过时兜底 web")
	assert.Equal(t, BrowserMountPrefix, mountPrefixOf(c))

	MountContext(dkdomain.OriginERP, MachineMountPrefix)(c)

	assert.Equal(t, dkdomain.OriginERP, originOf(c))
	assert.Equal(t, MachineMountPrefix, mountPrefixOf(c))
}
