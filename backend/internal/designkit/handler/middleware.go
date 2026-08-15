package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	upstreammw "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// gin.Context 里我们自己的键。加 designkit_ 前缀，避免跟上游撞名。
const (
	ctxKeyRequestID   = "designkit_request_id"
	ctxKeyOrigin      = "designkit_origin"
	ctxKeyMountPrefix = "designkit_mount_prefix"
)

// 两个挂载前缀。**/v1/designkit 是给 ERP 的外部契约，定了别改。**
const (
	// BrowserMountPrefix 浏览器（运营）走这条，挂上游 jwtAuth。
	BrowserMountPrefix = "/api/v1/designkit"
	// MachineMountPrefix 机器（ERP）走这条，跟 /v1/images/edits 同一个命名空间。
	MachineMountPrefix = "/v1/designkit"
)

// ----------------------------------------------------------------------------
// 请求号
// ----------------------------------------------------------------------------

// ensureRequestID 取（取不到就生成）本次请求的请求号，并记进 gin.Context，
// 保证响应头 X-Designkit-Request-Id 和错误体里的 request_id 是同一个值。
func ensureRequestID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get(ctxKeyRequestID); ok {
		if id, ok := v.(string); ok && id != "" {
			return id
		}
	}
	id := requestIDOf(c)
	if id == "" {
		// 上游 RequestLogger 是全局中间件，正常情况下一定有值；
		// 这里兜底是为了让单测和「有人绕过全局中间件」时 ERP 仍拿得到一个可追踪的号。
		id = uuid.NewString()
	}
	c.Set(ctxKeyRequestID, id)
	return id
}

// ----------------------------------------------------------------------------
// 挂载信息（来路 + 前缀）
// ----------------------------------------------------------------------------

// MountContext 记下这条路由是从哪个前缀进来的。
//
// handler **只从 GetAuthSubjectFromContext 取 UserID，不关心鉴权方式**；
// 但 designkit_jobs.origin（web/erp）和响应里的 content_url 需要知道挂载前缀，
// 而这是**路由事实**不是鉴权事实，所以由注册路由的地方直接告诉 handler。
func MountContext(origin dkdomain.Origin, prefix string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(ctxKeyOrigin, origin)
		c.Set(ctxKeyMountPrefix, prefix)
		c.Next()
	}
}

// originOf 取本次请求的来路；没设过时兜底 web。
func originOf(c *gin.Context) dkdomain.Origin {
	if c != nil {
		if v, ok := c.Get(ctxKeyOrigin); ok {
			if o, ok := v.(dkdomain.Origin); ok && o.IsValid() {
				return o
			}
		}
	}
	return dkdomain.OriginWeb
}

// mountPrefixOf 取本次请求的挂载前缀，用来拼 content_url。
func mountPrefixOf(c *gin.Context) string {
	if c != nil {
		if v, ok := c.Get(ctxKeyMountPrefix); ok {
			if p, ok := v.(string); ok && p != "" {
				return p
			}
		}
	}
	return BrowserMountPrefix
}

// userIDOf 取当前登录用户。取不到说明鉴权中间件缺位，一律当未登录处理。
func userIDOf(c *gin.Context) (int64, bool) {
	subject, ok := upstreammw.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		return 0, false
	}
	return subject.UserID, true
}

// ----------------------------------------------------------------------------
// 错误格式改写
// ----------------------------------------------------------------------------

// ErrorEnvelope 把这一组里所有的错误响应改写成 designkit 的唯一格式，
// 并给每个响应回 X-Designkit-Request-Id。
//
// 为什么必须有：这一组里除了我们自己的 handler，还挂着上游的 apiKeyAuth
// （POST /jobs 用），它吐的是 {"code":"INVALID_API_KEY",...}；
// 我们自己的 handler 吐的是 {"error":{...}}。两种格式混在同一个端点上，
// ERP 用强类型语言反序列化会在 Key 过期那天偶发解析异常，而且**发布后改不了**。
//
// 实现要点：
//   - 只在**状态码 >= 400** 时缓冲响应体，正常响应（含 /content 的图片字节）直接透传，
//     不额外占内存；
//   - 本来就是我方格式的响应原样放行，不做二次包装；
//   - **保留原始 HTTP 状态码**，只换body —— 状态码是上游按它的语义定的，
//     换掉会让 ERP 的重试策略失准。
func ErrorEnvelope() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := ensureRequestID(c)
		if requestID != "" {
			c.Header(RequestIDHeader, requestID)
		}

		original := c.Writer
		w := &envelopeWriter{ResponseWriter: original}
		c.Writer = w
		defer func() {
			w.flushRewritten(requestID)
			c.Writer = original
		}()

		c.Next()
	}
}

// envelopeWriter 是 gin.ResponseWriter 的包装：
// 状态码 >= 400 时把响应体缓冲下来，等 handler 链跑完再统一改写。
type envelopeWriter struct {
	gin.ResponseWriter

	status      int
	wroteHeader bool
	capturing   bool
	buf         bytes.Buffer
}

func (w *envelopeWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	if code >= http.StatusBadRequest {
		// 先不落地：等 flushRewritten 决定写什么。
		w.capturing = true
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *envelopeWriter) WriteHeaderNow() {
	if w.capturing {
		return
	}
	if !w.wroteHeader {
		w.wroteHeader = true
		w.status = http.StatusOK
	}
	w.ResponseWriter.WriteHeaderNow()
}

func (w *envelopeWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.capturing {
		return w.buf.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *envelopeWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *envelopeWriter) Status() int {
	if w.wroteHeader {
		return w.status
	}
	return w.ResponseWriter.Status()
}

func (w *envelopeWriter) Size() int {
	if w.capturing {
		return w.buf.Len()
	}
	return w.ResponseWriter.Size()
}

func (w *envelopeWriter) Written() bool {
	return w.wroteHeader || w.ResponseWriter.Written()
}

// flushRewritten 把缓冲下来的错误响应改写后真正写出去。
func (w *envelopeWriter) flushRewritten(requestID string) {
	if !w.capturing {
		return
	}
	w.capturing = false

	status := w.status
	if status <= 0 {
		status = http.StatusInternalServerError
	}

	header := w.ResponseWriter.Header()
	// 原响应可能已经写过 Content-Length（长度对不上会让客户端读到一半就断），
	// 一律删掉让 net/http 重算。
	header.Del("Content-Length")

	raw := w.buf.Bytes()
	if isDesignkitErrorBody(raw) {
		// 已经是我方格式，原样放行（这是我们自己的 handler 写的）。
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(raw)
		return
	}

	dkErr := translateForeignErrorBody(status, raw)
	header.Set("Content-Type", "application/json; charset=utf-8")

	body, err := json.Marshal(newErrorEnvelope(dkErr, requestID))
	if err != nil {
		// 走不到：errorEnvelope 全是字符串字段。真走到了也不能把原始 body 漏出去。
		body = []byte(`{"error":{"type":"api_error","error_code":"DK_INTERNAL","message":"系统开小差了，请稍后重试；一直这样请联系管理员。"}}`)
	}
	w.ResponseWriter.WriteHeader(status)
	_, _ = w.ResponseWriter.Write(body)
}

// isDesignkitErrorBody 判断这段响应体是不是已经是我方格式。
func isDesignkitErrorBody(raw []byte) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var probe struct {
		Error *struct {
			ErrorCode string `json:"error_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return probe.Error != nil && strings.HasPrefix(probe.Error.ErrorCode, "DK_")
}

// upstreamCodeToDesignkitCode 把上游的字符串错误码翻译成我方错误码。
//
// 每一条的字面量都是从上游源码里 grep 出来的（middleware/api_key_auth.go、
// middleware/panel_rate_limit.go、service/idempotency.go），不是编的。
//
// ⚠ 跟版检查清单里要加一条：上游改了这些字面量，这里会**静默退化**成按状态码兜底，
// 运营/ERP 看到的会是笼统的提示。不会报错，只会变难用。
func upstreamCodeToDesignkitCode(code string) (string, bool) {
	switch code {
	// —— API Key 鉴权 ——
	case "API_KEY_REQUIRED":
		return dkdomain.ErrCodeAPIKeyMissing, true
	case "INVALID_API_KEY", "API_KEY_DISABLED", "USER_NOT_FOUND":
		return dkdomain.ErrCodeUnauthorized, true
	case "USER_INACTIVE", "ACCESS_DENIED", "GROUP_DELETED", "GROUP_DISABLED", "GROUP_NOT_ALLOWED":
		return dkdomain.ErrCodeForbidden, true
	case "API_KEY_AUTH_OVERLOADED":
		return dkdomain.ErrCodeUpstreamBusy, true

	// —— 钱 / 额度 ——
	case "INSUFFICIENT_BALANCE":
		return dkdomain.ErrCodeInsufficientBalance, true
	case "API_KEY_QUOTA_EXHAUSTED", "USAGE_LIMIT_EXCEEDED", "SUBSCRIPTION_NOT_FOUND",
		"SUBSCRIPTION_INVALID", "insufficient_quota":
		return dkdomain.ErrCodeQuotaExhausted, true

	// —— 限流 ——
	case "RATE_LIMITED", "INVALID_AUTH_RATE_LIMITED":
		return dkdomain.ErrCodeRateLimited, true

	// —— 幂等协调器 ——
	case "IDEMPOTENCY_KEY_REQUIRED":
		return dkdomain.ErrCodeIdempotencyKeyRequired, true
	case "IDEMPOTENCY_KEY_INVALID":
		return dkdomain.ErrCodeInvalidRequest, true
	case "IDEMPOTENCY_KEY_CONFLICT", "IDEMPOTENCY_IN_PROGRESS", "IDEMPOTENCY_RETRY_BACKOFF":
		return dkdomain.ErrCodeIdempotencyKeyConflict, true
	case "IDEMPOTENCY_STORE_UNAVAILABLE":
		return dkdomain.ErrCodeInternal, true
	}
	return "", false
}

// upstreamCodeMessageOverride 少数几条：错误码能对上，但目录里的中文放在这个场景不合适。
func upstreamCodeMessageOverride(code string) (string, bool) {
	if code == "API_KEY_EXPIRED" {
		return "这把 API Key 已经过期，请联系管理员更换。", true
	}
	return "", false
}

// translateForeignErrorBody 把「不是我方格式」的错误响应翻译成我方错误。
//
// 认得出三种上游形状：
//
//	{"code":"INVALID_API_KEY","message":"..."}    middleware.AbortWithError
//	{"code":401,"message":"...","reason":"..."}   pkg/response.ErrorFrom
//	{"error":{"message":"...","code":"..."}}      OpenAI 兼容分支
//
// 认不出来就按 HTTP 状态码兜底。**宁可笼统也不要猜错方向** ——
// 猜错会让运营照着错误的提示去改一个跟故障无关的地方。
func translateForeignErrorBody(status int, raw []byte) *dkdomain.DesignkitError {
	code := extractForeignErrorCode(raw)

	if code != "" {
		dkCode, mapped := upstreamCodeToDesignkitCode(code)
		if override, ok := upstreamCodeMessageOverride(code); ok {
			if !mapped {
				dkCode = designkitCodeForStatus(status)
			}
			return dkdomain.NewError(dkCode).WithMessage(override)
		}
		if mapped {
			return dkdomain.NewError(dkCode)
		}
	}

	return dkdomain.NewError(designkitCodeForStatus(status))
}

// extractForeignErrorCode 从上游响应体里挖出字符串错误码；挖不到返回空串。
// 整数形式的 code（pkg/response 的信封）没有语义，直接忽略。
func extractForeignErrorCode(raw []byte) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var probe struct {
		Code  json.RawMessage `json:"code"`
		Error *struct {
			Code json.RawMessage `json:"code"`
			Type string          `json:"type"`
		} `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}

	if code := stringOrEmpty(probe.Code); code != "" {
		return code
	}
	if probe.Error != nil {
		if code := stringOrEmpty(probe.Error.Code); code != "" {
			return code
		}
		if probe.Error.Type != "" {
			return probe.Error.Type
		}
	}
	// pkg/response 的信封把语义放在 reason 里（code 是整数）。
	return strings.TrimSpace(probe.Reason)
}

// stringOrEmpty 只接受 JSON 字符串；数字、对象、null 一律当没有。
func stringOrEmpty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// designkitCodeForStatus 按 HTTP 状态码兜底选一个我方错误码。
func designkitCodeForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return dkdomain.ErrCodeUnauthorized
	case http.StatusPaymentRequired:
		return dkdomain.ErrCodeInsufficientBalance
	case http.StatusForbidden:
		return dkdomain.ErrCodeForbidden
	case http.StatusNotFound:
		return dkdomain.ErrCodeInvalidRequest
	case http.StatusConflict:
		return dkdomain.ErrCodeIllegalStateTransition
	case http.StatusRequestEntityTooLarge:
		return dkdomain.ErrCodeImageTooLarge
	case http.StatusUnsupportedMediaType:
		return dkdomain.ErrCodeUnsupportedImageFormat
	case http.StatusTooManyRequests:
		return dkdomain.ErrCodeRateLimited
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return dkdomain.ErrCodeTimeout
	}
	if status >= 400 && status < 500 {
		return dkdomain.ErrCodeInvalidRequest
	}
	return dkdomain.ErrCodeInternal
}

// ----------------------------------------------------------------------------
// ERP 读接口的轻量 Key 鉴权
// ----------------------------------------------------------------------------

// APIKeyLookup 是轻量鉴权需要的那一点点上游能力。
// *service.APIKeyService 天然满足它；单测塞假实现。
type APIKeyLookup interface {
	GetByKey(ctx context.Context, key string) (*upstreamservice.APIKey, error)
	TouchLastUsed(ctx context.Context, keyID int64) error
}

// KeyAuth 是 ERP **读接口**专用的轻量 API Key 鉴权。
//
// 为什么不能直接用上游的 apiKeyAuth：它的计费准入只对三条路径跳过
// （/v1/usage、/v1/sub2api/billing、GET /v1/images/tasks/，见 middleware/api_key_auth.go）。
// /v1/designkit/* 不在里面 → **Key 额度耗尽或余额 ≤0 时，
// 连「查任务」「取已经付过钱的图」都会被拦掉**，
// 而我们恰恰建议给 ERP Key 设日消费封顶（CLAUDE.md B7）——等于自己给自己埋雷。
// api_key_auth.go 不在可改文件白名单里，跟版后也改不了。
//
// 所以这里只做三件事：查 Key、判 Key/用户是不是被停用、IP 白黑名单。
// **不做任何计费准入**：额度用尽只影响提交新任务（POST /jobs 仍挂上游 apiKeyAuth），
// 不影响查询和取图 —— 这一条要写死进 ERP 文档。
//
// ⚠ 「判 Key 是不是被停用」刻意**不是**简单的 apiKey.IsActive()：
// IsActive() 的判据是 status == "active"，而额度耗尽的 Key 状态是 "quota_exhausted"、
// 过期的是 "expired"。照字面写 IsActive() 就会把这两种 Key 挡在门外，
// 正好是本中间件要避免的那个坑。判据跟上游的**鉴权层**保持一致：
// 只有 disabled / 未知状态才拒绝。
func KeyAuth(lookup APIKeyLookup, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if lookup == nil {
			abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInternal))
			return
		}

		// Key 出现在 query 里会被各级日志和反代记下来，直接拒绝（跟上游同一个立场）。
		if strings.TrimSpace(c.Query("key")) != "" || strings.TrimSpace(c.Query("api_key")) != "" {
			abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
				WithMessage("请把 API Key 放在 Authorization: Bearer 请求头里，不要放在网址参数里。"))
			return
		}

		keyString := extractAPIKey(c)
		if keyString == "" {
			abortWithDesignkitError(c, errAPIKeyMissing())
			return
		}
		if len(keyString) > upstreamservice.MaxAPIKeyCredentialBytes {
			abortWithDesignkitError(c, errAPIKeyInvalid(nil))
			return
		}

		apiKey, err := lookup.GetByKey(c.Request.Context(), keyString)
		if err != nil {
			switch {
			case errors.Is(err, upstreamservice.ErrAPIKeyNotFound):
				abortWithDesignkitError(c, errAPIKeyInvalid(err))
			case errors.Is(err, upstreamservice.ErrAPIKeyAuthOverloaded):
				failCode(c, dkdomain.ErrCodeUpstreamBusy)
			default:
				abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err))
			}
			return
		}
		if apiKey == nil {
			abortWithDesignkitError(c, errAPIKeyInvalid(nil))
			return
		}

		// disabled / 未知状态无条件拒绝；expired 和 quota_exhausted 放行（见函数注释）。
		if !apiKey.IsActive() &&
			apiKey.Status != upstreamservice.StatusAPIKeyExpired &&
			apiKey.Status != upstreamservice.StatusAPIKeyQuotaExhausted {
			abortWithDesignkitError(c, errAPIKeyInvalid(nil))
			return
		}

		if apiKey.User == nil {
			abortWithDesignkitError(c, errAPIKeyInvalid(nil))
			return
		}
		if !apiKey.User.IsActive() {
			abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeForbidden).
				WithMessage("这把 API Key 对应的账号已被停用，请联系管理员。"))
			return
		}

		if len(apiKey.IPWhitelist) > 0 || len(apiKey.IPBlacklist) > 0 {
			trustForwarded := false
			if cfg != nil {
				trustForwarded = cfg.TrustForwardedIPForAPIKeyACL()
			}
			clientIP := ip.GetSecurityClientIP(c, trustForwarded)
			allowed, _ := ip.CheckIPRestrictionWithCompiledRules(clientIP, apiKey.CompiledIPWhitelist, apiKey.CompiledIPBlacklist)
			if !allowed {
				// 不回显客户端 IP：这是免额度准入的公开端点，回显等于给探测者送信息。
				failCode(c, dkdomain.ErrCodeForbidden)
				return
			}
		}

		c.Set(string(upstreammw.ContextKeyAPIKey), apiKey)
		c.Set(string(upstreammw.ContextKeyUser), upstreammw.AuthSubject{
			UserID:      apiKey.User.ID,
			Concurrency: apiKey.User.Concurrency,
		})
		c.Set(string(upstreammw.ContextKeyUserRole), apiKey.User.Role)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.UserID, apiKey.User.ID))

		// 尽力而为：上游这个方法自带防抖（同一把 Key 一段时间内只写一次），
		// ERP 每 2 秒轮询也不会把 api_keys 表写爆。
		if err := lookup.TouchLastUsed(c.Request.Context(), apiKey.ID); err != nil {
			slog.Debug("designkit 更新 API Key 最后使用时间失败", slog.Any("error", err))
		}

		c.Next()
	}
}

// errAPIKeyMissing / errAPIKeyInvalid 是 ERP 侧鉴权失败的两条文案。
//
// 目录里 DK_UNAUTHORIZED 的默认文案是「登录已过期，请重新登录」——那是给浏览器端的，
// 对着 ERP 说「请重新登录」等于让人去点一个不存在的按钮。这里换成对接方看得懂的话。
// 两条都刻意**不区分**「Key 不存在」「Key 被停用」「Key 太长」：
// 区分开等于给探测者送信息。
func errAPIKeyMissing() *dkdomain.DesignkitError {
	return dkdomain.NewError(dkdomain.ErrCodeUnauthorized).
		WithMessage("没有提供 API Key。请在请求头里带上 Authorization: Bearer <你的 Key>。")
}

func errAPIKeyInvalid(cause error) *dkdomain.DesignkitError {
	err := dkdomain.NewError(dkdomain.ErrCodeUnauthorized).
		WithMessage("API Key 无效或已停用，请联系管理员核对。")
	if cause != nil {
		err = err.WithCause(cause)
	}
	return err
}

// extractAPIKey 按上游同样的顺序找 Key：Authorization: Bearer → x-api-key → x-goog-api-key。
func extractAPIKey(c *gin.Context) string {
	if authHeader := c.GetHeader("Authorization"); authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			if key := strings.TrimSpace(parts[1]); key != "" {
				return key
			}
		}
	}
	if key := strings.TrimSpace(c.GetHeader("x-api-key")); key != "" {
		return key
	}
	return strings.TrimSpace(c.GetHeader("x-goog-api-key"))
}

// RequireAuthenticated 兜底：鉴权中间件缺位时不要让 handler 拿着 userID=0 去查数据。
func RequireAuthenticated() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := userIDOf(c); !ok {
			failCode(c, dkdomain.ErrCodeUnauthorized)
			return
		}
		c.Next()
	}
}
