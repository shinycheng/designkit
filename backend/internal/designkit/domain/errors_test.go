//go:build unit

package domain

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestErrorCatalog_AllChinese 运营看到的必须是中文。
// 上游给的是英文原文，直接透出去运营只会截图问「这是啥」。
func TestErrorCatalog_AllChinese(t *testing.T) {
	for code, e := range ErrorCatalog() {
		require.NotEmptyf(t, e.Message, "%s 缺中文文案", code)
		require.Truef(t, containsHan(e.Message), "%s 的文案必须是中文：%q", code, e.Message)
		require.NotZerof(t, e.HTTPStatus, "%s 缺 HTTP 状态码", code)
	}
}

// TestErrorCatalog_NoCNYWording 金额一律美元，文案里不许出现「元」（决策 18）。
// 底层余额和账单全是美元，出现「元」会让运营按人民币理解，充错钱。
func TestErrorCatalog_NoCNYWording(t *testing.T) {
	for code, e := range ErrorCatalog() {
		require.NotContainsf(t, e.Message, "元", "%s 的文案里不许出现「元」：%q", code, e.Message)
		require.NotContainsf(t, e.Message, "人民币", "%s 的文案里不许出现「人民币」：%q", code, e.Message)
		require.NotContainsf(t, e.Message, "￥", "%s 的文案里不许出现「￥」：%q", code, e.Message)
	}
}

// TestErrorCodes_UpperSnakeWithPrefix 错误码是发给 ERP 的对外契约的一部分。
func TestErrorCodes_UpperSnakeWithPrefix(t *testing.T) {
	for code := range ErrorCatalog() {
		require.Truef(t, strings.HasPrefix(code, "DK_"), "%s 必须以 DK_ 开头", code)
		require.Equalf(t, strings.ToUpper(code), code, "%s 必须全大写", code)
		for _, r := range code {
			require.Truef(t, r == '_' || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'),
				"%s 只能由大写字母、数字和下划线组成", code)
		}
	}
}

func TestNewError(t *testing.T) {
	e := NewError(ErrCodeInsufficientBalance)
	require.Equal(t, ErrCodeInsufficientBalance, e.Code)
	require.Equal(t, http.StatusPaymentRequired, e.HTTPStatus)
	require.Contains(t, e.Message, "余额不足")

	// 未知错误码退化成 DK_INTERNAL，绝不 panic
	unknown := NewError("NOT_A_REAL_CODE")
	require.Equal(t, ErrCodeInternal, unknown.Code)
	require.Equal(t, http.StatusInternalServerError, unknown.HTTPStatus)
}

// TestNewError_DoesNotMutateCatalog With* 系列必须返回副本，
// 就地改掉目录里的模板会让下一个撞上同样错误的运营看到别人的上游报文。
func TestNewError_DoesNotMutateCatalog(t *testing.T) {
	original := NewError(ErrCodeTimeout).Message

	tainted := NewError(ErrCodeTimeout).
		WithMessage("被改过的文案").
		WithUpstream("upstream raw text").
		WithCause(errors.New("boom"))
	require.Equal(t, "被改过的文案", tainted.Message)

	fresh := NewError(ErrCodeTimeout)
	require.Equal(t, original, fresh.Message)
	require.Empty(t, fresh.Upstream)
	require.Nil(t, fresh.Cause)
}

func TestDesignkitError_UnwrapAndAs(t *testing.T) {
	cause := errors.New("boom")
	e := NewError(ErrCodeStorageError).WithCause(cause)

	require.ErrorIs(t, e, cause)

	var dkErr *DesignkitError
	require.True(t, errors.As(error(e), &dkErr))
	require.Equal(t, ErrCodeStorageError, dkErr.Code)

	got, ok := AsDesignkitError(e)
	require.True(t, ok)
	require.Equal(t, ErrCodeStorageError, got.Code)

	_, ok = AsDesignkitError(errors.New("plain"))
	require.False(t, ok)
}

func TestErrorTypeForStatus(t *testing.T) {
	require.Equal(t, ErrorTypeAuthentication, ErrorTypeForStatus(http.StatusUnauthorized))
	require.Equal(t, ErrorTypeAuthentication, ErrorTypeForStatus(http.StatusForbidden))
	require.Equal(t, ErrorTypeRateLimit, ErrorTypeForStatus(http.StatusTooManyRequests))
	require.Equal(t, ErrorTypeInvalidRequest, ErrorTypeForStatus(http.StatusBadRequest))
	require.Equal(t, ErrorTypeInvalidRequest, ErrorTypeForStatus(http.StatusNotFound))
	require.Equal(t, ErrorTypeAPI, ErrorTypeForStatus(http.StatusBadGateway))
	require.Equal(t, ErrorTypeAPI, ErrorTypeForStatus(http.StatusInternalServerError))
}

// TestErrorType_OnlyFourValues 对外 JSON 的 type 字段只承诺四个取值。
// ERP 用强类型语言反序列化，多一个取值就是一次线上解析异常。
func TestErrorType_OnlyFourValues(t *testing.T) {
	allowed := map[string]bool{
		ErrorTypeInvalidRequest: true,
		ErrorTypeAuthentication: true,
		ErrorTypeRateLimit:      true,
		ErrorTypeAPI:            true,
	}
	for code := range ErrorCatalog() {
		got := NewError(code).ErrorType()
		require.Truef(t, allowed[got], "%s 推导出的 type=%q 不在四个允许值里", code, got)
	}
}

// TestMapUpstreamError 上游英文原文 → 我方错误码。
// 每条关键词都是从上游源码里 grep 出来的原文，不是编的。
func TestMapUpstreamError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		msg    string
		want   string
	}{
		{
			name:   "余额不足（api_key_auth.go:264 的 403 INSUFFICIENT_BALANCE）",
			status: http.StatusForbidden,
			msg:    "Insufficient account balance",
			want:   ErrCodeInsufficientBalance,
		},
		{
			name:   "余额不足（上游 402 的说法）",
			status: http.StatusPaymentRequired,
			msg:    "Upstream payment required: insufficient balance or billing issue",
			want:   ErrCodeInsufficientBalance,
		},
		{
			name:   "并发排队（openai_gateway_handler.go:2475）",
			status: http.StatusTooManyRequests,
			msg:    "Image generation concurrency limit exceeded, please retry later",
			want:   ErrCodeUpstreamBusy,
		},
		{
			name:   "排队满了（concurrency_error_response.go:16）",
			status: http.StatusTooManyRequests,
			msg:    "Too many pending requests, please retry later",
			want:   ErrCodeUpstreamBusy,
		},
		{
			name:   "等并发槽位超时——含 timeout 但必须归到「排队」而不是「超时」",
			status: 0,
			msg:    "timeout waiting for openai concurrency slot",
			want:   ErrCodeUpstreamBusy,
		},
		{
			name:   "无可用账号（gateway_handler.go:385）",
			status: http.StatusServiceUnavailable,
			msg:    "No available accounts",
			want:   ErrCodeNoAvailableAccount,
		},
		{
			name:   "无可用账号（带原因的变体）",
			status: http.StatusServiceUnavailable,
			msg:    "No available accounts: all candidates rejected by group profit control",
			want:   ErrCodeNoAvailableAccount,
		},
		{
			name:   "分组未开生图（image_generation_intent.go:15）",
			status: http.StatusForbidden,
			msg:    "Image generation is not enabled for this group",
			want:   ErrCodeImageNotEnabled,
		},
		{
			name:   "分组未开生图（openai_gateway_forward.go:276 的另一种说法）",
			status: http.StatusForbidden,
			msg:    "image generation disabled for group",
			want:   ErrCodeImageNotEnabled,
		},
		{
			name:   "模型不在白名单（ops_error_logger.go:1646）",
			status: http.StatusForbidden,
			msg:    "model gpt-image-2 not in whitelist",
			want:   ErrCodeModelNotAllowed,
		},
		{
			name:   "平台不支持出图接口（routes/gateway.go:90）",
			status: http.StatusNotFound,
			msg:    "Images API is not supported for this platform",
			want:   ErrCodeImagesAPIUnsupported,
		},
		{
			name:   "base_url 配错（gateway_upstream_request.go:906）",
			status: 0,
			msg:    "invalid base_url: parse \"::\": missing protocol scheme",
			want:   ErrCodeUpstreamBaseURL,
		},
		{
			name:   "开了 url_allowlist 之后的报错（urlvalidator/validator.go:49）",
			status: 0,
			msg:    "invalid base_url: host is not allowed: relay.example.com",
			want:   ErrCodeUpstreamBaseURL,
		},
		{
			name:   "内容审核拦截（grok_upstream_errors.go 的短语表）",
			status: http.StatusBadRequest,
			msg:    "request blocked by content moderation",
			want:   ErrCodeContentBlocked,
		},
		{
			name:   "内容审核拦截（另一种措辞）",
			status: http.StatusBadRequest,
			msg:    "prompt violates content policy",
			want:   ErrCodeContentBlocked,
		},
		{
			name:   "超时",
			status: http.StatusGatewayTimeout,
			msg:    "image generation task timed out",
			want:   ErrCodeTimeout,
		},
		{
			name:   "本地 context 超时",
			status: 0,
			msg:    "context deadline exceeded",
			want:   ErrCodeTimeout,
		},
		{
			name:   "Key 额度用完",
			status: http.StatusForbidden,
			msg:    "daily usage limit exceeded",
			want:   ErrCodeQuotaExhausted,
		},
		{
			name:   "RPM 限速",
			status: http.StatusTooManyRequests,
			msg:    "requests-per-minute limit exceeded",
			want:   ErrCodeRateLimited,
		},
		{
			name:   "认不出来的原文 + 502 → 兜底",
			status: http.StatusBadGateway,
			msg:    "something nobody has ever seen",
			want:   ErrCodeUpstreamError,
		},
		{
			name:   "什么都没有 → 兜底",
			status: 0,
			msg:    "",
			want:   ErrCodeUpstreamError,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MapUpstreamError(c.status, c.msg)
			require.Equal(t, c.want, got.Code)
			require.Equal(t, strings.TrimSpace(c.msg), got.Upstream,
				"上游原文必须原样留在 Upstream 里，落 last_error_message 给管理员看")
			require.Truef(t, containsHan(got.Message),
				"运营看到的必须是中文，实际 %q", got.Message)
			if c.msg != "" {
				require.NotContains(t, got.Message, c.msg,
					"英文原文不许出现在给运营看的文案里")
			}
		})
	}
}

// TestMapUpstreamError_CaseInsensitive 上游同一句话在不同代码路径里大小写不一致
// （"No available accounts" vs "no available account"），匹配必须不区分大小写。
func TestMapUpstreamError_CaseInsensitive(t *testing.T) {
	require.Equal(t, ErrCodeNoAvailableAccount, MapUpstreamError(503, "NO AVAILABLE ACCOUNTS").Code)
	require.Equal(t, ErrCodeImageNotEnabled,
		MapUpstreamError(403, "IMAGE GENERATION IS NOT ENABLED FOR THIS GROUP").Code)
}

// TestMapUpstreamError_StatusFallback 认不出原文时按状态码兜底。
func TestMapUpstreamError_StatusFallback(t *testing.T) {
	require.Equal(t, ErrCodeUpstreamUnauthorized, MapUpstreamError(http.StatusUnauthorized, "???").Code)
	require.Equal(t, ErrCodeInsufficientBalance, MapUpstreamError(http.StatusPaymentRequired, "???").Code)
	require.Equal(t, ErrCodeUpstreamBusy, MapUpstreamError(http.StatusTooManyRequests, "???").Code)
	require.Equal(t, ErrCodeTimeout, MapUpstreamError(http.StatusGatewayTimeout, "???").Code)
	require.Equal(t, ErrCodeContentBlocked, MapUpstreamError(http.StatusUnprocessableEntity, "???").Code)
	require.Equal(t, ErrCodeUpstreamError, MapUpstreamError(http.StatusInternalServerError, "???").Code)
}

// TestMapUpstreamError_CoversRequiredCases 设计定型 第八节点名要求的八类
// 必须全部有对应错误码和中文文案。
func TestMapUpstreamError_CoversRequiredCases(t *testing.T) {
	required := []string{
		ErrCodeInsufficientBalance,
		ErrCodeUpstreamBusy,
		ErrCodeNoAvailableAccount,
		ErrCodeImageNotEnabled,
		ErrCodeModelNotAllowed,
		ErrCodeUpstreamBaseURL,
		ErrCodeContentBlocked,
		ErrCodeTimeout,
	}
	catalog := ErrorCatalog()
	for _, code := range required {
		e, ok := catalog[code]
		require.Truef(t, ok, "%s 必须在错误码目录里", code)
		require.NotEmpty(t, e.Message)
	}
}

// containsHan 判断字符串里有没有汉字。
func containsHan(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}
