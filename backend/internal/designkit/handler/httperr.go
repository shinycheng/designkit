package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 对外唯一的错误格式
// ============================================================================
//
// 上游同一个端点会吐出**三种互不兼容**的错误 JSON：
//
//	{"code":"INVALID_API_KEY","message":"..."}   middleware.AbortWithError
//	{"code":401,"message":"...","reason":"..."}  pkg/response.ErrorFrom（code 是整数）
//	{"error":{"message":"...","type":"..."}}     OpenAI 兼容分支
//
// ERP 用强类型语言反序列化，会在 Key 过期那天偶发解析异常，而且**发布后改不了**。
// 所以 designkit 对外只承诺一种：
//
//	{"error":{"type":"...","error_code":"UPPER_SNAKE","message":"人话","request_id":"..."}}
//
// ⚠ 字段名是 error_code **不是 code**：上游幂等协调器存库前会调
// logredact.RedactText，它的敏感键表里有 "code"（精确匹配，不是包含匹配），
// 名为 code 的字段会被脱敏成 "***"，重放时 ERP 拿到的就是 ***。
// error_code / last_error_code 不在表里，安全。
// 对外 JSON **任何层级都不许出现名为 code 的字段**。

const (
	// RequestIDHeader designkit 自己的请求号响应头。每个响应都回，排障时让运营截图给管理员。
	RequestIDHeader = "X-Designkit-Request-Id"

	// upstreamRequestIDHeader 上游 RequestLogger 注入的请求号响应头。
	// 我们优先复用它的值，这样一次请求在两边的日志里是同一个号。
	upstreamRequestIDHeader = "X-Request-ID"

	// idempotencyReplayedHeader 命中幂等重放时回这个头，方便 ERP 排查「为什么没有新任务」。
	idempotencyReplayedHeader = "X-Idempotency-Replayed"
)

// errorEnvelope 是错误响应体。字段名定了就不要改 —— ERP 会按 error_code 做分支。
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	// Type 只有四个取值，见 domain.ErrorTypeXxx。
	Type string `json:"type"`
	// ErrorCode 我方错误码，全部 DK_ 前缀的 UPPER_SNAKE。
	ErrorCode string `json:"error_code"`
	// Message 给人看的中文。
	Message string `json:"message"`
	// RequestID 排障用。
	RequestID string `json:"request_id,omitempty"`
}

// requestIDOf 取当前请求的请求号。
//
// 优先级：本组中间件已经定好的值 → 上游 RequestLogger 注进 context 的值 →
// 入站请求头 → 已经回出去的响应头。
// 全都没有时返回空串（字段会被 omitempty 省掉，不要编一个假的）。
//
// 第一条最要紧：响应头 X-Designkit-Request-Id 和错误体里的 request_id
// **必须是同一个值**，否则运营截图报障时两个号对不上，等于没有。
func requestIDOf(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get(ctxKeyRequestID); ok {
		if id, ok := v.(string); ok && id != "" {
			return id
		}
	}
	if c.Request != nil {
		if id, ok := c.Request.Context().Value(ctxkey.RequestID).(string); ok {
			if id = strings.TrimSpace(id); id != "" {
				return id
			}
		}
		if id := strings.TrimSpace(c.GetHeader(upstreamRequestIDHeader)); id != "" {
			return id
		}
	}
	if c.Writer != nil {
		if id := strings.TrimSpace(c.Writer.Header().Get(upstreamRequestIDHeader)); id != "" {
			return id
		}
	}
	return ""
}

// newErrorEnvelope 把一个我方错误装进对外响应体。
func newErrorEnvelope(dkErr *dkdomain.DesignkitError, requestID string) errorEnvelope {
	if dkErr == nil {
		dkErr = dkdomain.NewError(dkdomain.ErrCodeInternal)
	}
	return errorEnvelope{Error: errorBody{
		Type:      dkErr.ErrorType(),
		ErrorCode: dkErr.Code,
		Message:   dkErr.Message,
		RequestID: requestID,
	}}
}

// abortWithDesignkitError 用我方格式结束这次请求。
//
// dkErr.Upstream（上游英文原文）**只写日志，绝不进响应体**：
// 界面上只出现中文，英文原文是给管理员看的（CLAUDE.md 决策，设计定型 第八节）。
func abortWithDesignkitError(c *gin.Context, dkErr *dkdomain.DesignkitError) {
	if dkErr == nil {
		dkErr = dkdomain.NewError(dkdomain.ErrCodeInternal)
	}
	status := dkErr.HTTPStatus
	if status <= 0 {
		status = http.StatusInternalServerError
	}

	logAttrs := []any{
		slog.String("error_code", dkErr.Code),
		slog.Int("status", status),
	}
	if c != nil && c.Request != nil {
		logAttrs = append(logAttrs,
			slog.String("method", c.Request.Method),
			slog.String("path", c.Request.URL.Path),
		)
	}
	if dkErr.Upstream != "" {
		logAttrs = append(logAttrs, slog.String("upstream", dkErr.Upstream))
	}
	if dkErr.Cause != nil {
		logAttrs = append(logAttrs, slog.Any("cause", dkErr.Cause))
	}
	if status >= http.StatusInternalServerError {
		slog.Error("designkit 请求失败", logAttrs...)
	} else {
		slog.Debug("designkit 请求被拒绝", logAttrs...)
	}

	c.AbortWithStatusJSON(status, newErrorEnvelope(dkErr, requestIDOf(c)))
}

// failCode 按错误码直接失败（参数校验这类我们自己判定的错误走这条）。
func failCode(c *gin.Context, code string) {
	abortWithDesignkitError(c, dkdomain.NewError(code))
}

// failCodef 同 failCode，但换一句带数字的中文文案（「单次最多 50 张」这种）。
func failCodef(c *gin.Context, code, format string, a ...any) {
	abortWithDesignkitError(c, dkdomain.NewError(code).WithMessagef(format, a...))
}

// failService 把 service 返回的 error 翻译成对外响应。
//
// notFoundCode 是「这条记录不存在」时该用哪个错误码 ——
// domain.ErrNotFound 本身不带资源类型，由调用方按端点指定
// （DK_JOB_NOT_FOUND / DK_ASSET_NOT_FOUND / DK_IMAGE_NOT_FOUND …）。
//
// ⚠ 归属不匹配一律走「找不到」，**不要返回 403** —— 403 等于告诉对方
// 「这个任务号是存在的，只是不是你的」。
func failService(c *gin.Context, err error, notFoundCode string) {
	if err == nil {
		return
	}
	abortWithDesignkitError(c, serviceErrorToDesignkit(err, notFoundCode))
}

// serviceErrorToDesignkit 是 failService 的翻译部分，单独拆出来给
// 「不能再改状态码」的场景用（SSE 流已经开了，错误只能装进 error 帧，
// 见 chat_handler.go 的 sendStream）。翻译规则两边必须是同一份。
func serviceErrorToDesignkit(err error, notFoundCode string) *dkdomain.DesignkitError {
	// service 自己已经给出我方错误码时，原样用它的码、文案和状态。
	if dkErr, ok := dkdomain.AsDesignkitError(err); ok {
		return dkErr
	}

	switch {
	case errors.Is(err, dkdomain.ErrNotFound):
		return dkdomain.NewError(notFoundCode).WithCause(err)

	case errors.Is(err, dkdomain.ErrInsufficientBalance):
		return insufficientBalanceError(err)

	case errors.Is(err, dkdomain.ErrIllegalTransition):
		return dkdomain.NewError(dkdomain.ErrCodeIllegalStateTransition).WithCause(err)

	case errors.Is(err, dkdomain.ErrConflict):
		return dkdomain.NewError(dkdomain.ErrCodeIllegalStateTransition).WithCause(err)

	case errors.Is(err, dkdomain.ErrObjectNotFound):
		// 库里有记录、对象存储里没文件：这不是「找不到」，是存储出了问题，
		// 报成 404 会让运营以为图被自己删了，实际要查的是磁盘/桶。
		return dkdomain.NewError(dkdomain.ErrCodeStorageError).WithCause(err)

	case errors.Is(err, context.Canceled):
		return dkdomain.NewError(dkdomain.ErrCodeCanceled).WithCause(err)

	case errors.Is(err, context.DeadlineExceeded):
		return dkdomain.NewError(dkdomain.ErrCodeTimeout).WithCause(err)

	default:
		return dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
}

// insufficientBalanceError 把「还差多少」拼进中文文案（决策 19：余额不足要有出路）。
// 金额一律美元，文案里不许出现「元」。
func insufficientBalanceError(err error) *dkdomain.DesignkitError {
	base := dkdomain.NewError(dkdomain.ErrCodeInsufficientBalance).WithCause(err)

	var shortErr *dkdomain.InsufficientBalanceError
	if errors.As(err, &shortErr) && shortErr != nil {
		return base.WithMessagef(
			"余额不足，这一批还差 %s（需要 %s，当前可用 %s）。可以点「申请额度」通知管理员，或者少选几张再提交。",
			dkdomain.FormatMoneyUSD(shortErr.Shortfall),
			dkdomain.FormatMoneyUSD(shortErr.Required),
			dkdomain.FormatMoneyUSD(shortErr.Available),
		)
	}
	return base
}
