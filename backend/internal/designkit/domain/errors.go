package domain

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ============================================================================
// 错误码与中文文案
// ============================================================================
//
// 「技术上对但人用不了 = 白做」。使用者是非技术背景的电商运营，
// 出图失败时上游给的是英文原文，例如：
//
//	Image generation is not enabled for this group
//	Image generation concurrency limit exceeded, please retry later
//	Insufficient account balance
//	No available accounts
//
// 运营看到这些只会截图问「这是啥」。所以：
//
//	designkit_job_items.last_error_code    ← 我方错误码（UPPER_SNAKE），界面显示中文时带上它方便截图报障
//	designkit_job_items.last_error_message ← **上游英文原文，只给管理员看，界面不显示**
//
// 界面上只出现 Message 里的中文。
//
// 对 ERP 只承诺一种错误格式（设计定型 3.1）：
//
//	{"error":{"type":"...","error_code":"UPPER_SNAKE","message":"人话","request_id":"..."}}
//
// 字段名用 error_code **不用 code** —— 上游幂等协调器存库前会把名为 code 的字段
// 脱敏成 ***，对外 JSON 里不许出现名为 code 的字段。

// DesignkitError 是本模块唯一的错误类型。
//
// 一律用指针传（*DesignkitError），因为 Unwrap / errors.As 都按指针接收者实现。
type DesignkitError struct {
	// Code 我方错误码，UPPER_SNAKE，全部以 DK_ 开头。
	// 落 last_error_code，也是对外 JSON 的 error_code 字段。定了就不要改字面量 ——
	// ERP 那边会按它做分支。
	Code string

	// Message 给运营看的中文。必须是「发生了什么 + 该怎么办」，不要只说现象。
	Message string

	// HTTPStatus 返回给客户端的 HTTP 状态码。
	HTTPStatus int

	// Upstream 上游返回的英文原文。
	// **只写进 last_error_message 给管理员看，绝不出现在界面上、也不进对外 JSON。**
	Upstream string

	// Cause 原始 error，用于 errors.Is / errors.As 链式判断。日志里打，接口里不返。
	Cause error
}

// Error 实现 error。给日志看的，不是给运营看的。
func (e *DesignkitError) Error() string {
	if e == nil {
		return "<nil designkit error>"
	}
	var b strings.Builder
	b.WriteString(e.Code)
	b.WriteString(": ")
	b.WriteString(e.Message)
	if e.Upstream != "" {
		b.WriteString(" (upstream: ")
		b.WriteString(e.Upstream)
		b.WriteString(")")
	}
	if e.Cause != nil {
		b.WriteString(" (cause: ")
		b.WriteString(e.Cause.Error())
		b.WriteString(")")
	}
	return b.String()
}

// Unwrap 让 errors.Is / errors.As 能穿透到 Cause。
func (e *DesignkitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ErrorType 返回对外 JSON 里的 type 字段（设计定型 3.1 只允许这四个取值）。
// 由 HTTPStatus 推导，不用额外维护一列。
func (e *DesignkitError) ErrorType() string {
	if e == nil {
		return ErrorTypeAPI
	}
	return ErrorTypeForStatus(e.HTTPStatus)
}

// 对外 JSON 的 type 字段取值。ERP 用强类型语言反序列化，这四个字面量定了别改。
const (
	ErrorTypeInvalidRequest = "invalid_request_error"
	ErrorTypeAuthentication = "authentication_error"
	ErrorTypeRateLimit      = "rate_limit_error"
	ErrorTypeAPI            = "api_error"
)

// ErrorTypeForStatus 由 HTTP 状态码推导 type。
func ErrorTypeForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrorTypeAuthentication
	case http.StatusTooManyRequests:
		return ErrorTypeRateLimit
	}
	if status >= 400 && status < 500 {
		return ErrorTypeInvalidRequest
	}
	return ErrorTypeAPI
}

// WithCause 挂上原始 error，返回**副本**（目录里的模板绝不能被就地改掉）。
func (e *DesignkitError) WithCause(cause error) *DesignkitError {
	c := e.clone()
	c.Cause = cause
	return c
}

// WithUpstream 记下上游英文原文，返回副本。
// 这个值只会落 last_error_message，不会出现在界面和对外 JSON 里。
func (e *DesignkitError) WithUpstream(raw string) *DesignkitError {
	c := e.clone()
	c.Upstream = strings.TrimSpace(raw)
	return c
}

// WithMessage 换掉中文文案，返回副本。用于需要带数字的场景
// （「还差 $12.30」「单次最多 50 张」）。**换完仍然必须是中文。**
func (e *DesignkitError) WithMessage(msg string) *DesignkitError {
	c := e.clone()
	c.Message = msg
	return c
}

// WithMessagef 同 WithMessage，带格式化。
func (e *DesignkitError) WithMessagef(format string, a ...any) *DesignkitError {
	return e.WithMessage(fmt.Sprintf(format, a...))
}

func (e *DesignkitError) clone() *DesignkitError {
	if e == nil {
		return NewError(ErrCodeInternal)
	}
	c := *e
	return &c
}

// AsDesignkitError 从 error 链里取出 *DesignkitError。
// handler 层统一用它决定 HTTP 状态码和返回体；取不到就当 DK_INTERNAL。
func AsDesignkitError(err error) (*DesignkitError, bool) {
	var dkErr *DesignkitError
	if errors.As(err, &dkErr) {
		return dkErr, true
	}
	return nil, false
}

// ============================================================================
// 错误码全集
// ============================================================================
//
// 全部以 DK_ 开头、UPPER_SNAKE。**字面量定了就不要改**：
// ERP 文档会列出全集，改字面量等于改对外契约。

const (
	// —— 钱 ——
	ErrCodeInsufficientBalance = "DK_INSUFFICIENT_BALANCE"
	ErrCodeQuotaExhausted      = "DK_QUOTA_EXHAUSTED"

	// —— 排队 / 限流 ——
	ErrCodeUpstreamBusy = "DK_UPSTREAM_BUSY"
	ErrCodeRateLimited  = "DK_RATE_LIMITED"

	// —— 上游账号 / 分组配置 ——
	ErrCodeNoAvailableAccount   = "DK_NO_AVAILABLE_ACCOUNT"
	ErrCodeImageNotEnabled      = "DK_IMAGE_NOT_ENABLED"
	ErrCodeModelNotAllowed      = "DK_MODEL_NOT_ALLOWED"
	ErrCodeImagesAPIUnsupported = "DK_IMAGES_API_UNSUPPORTED"
	ErrCodeUpstreamBaseURL      = "DK_UPSTREAM_BASE_URL_INVALID"
	ErrCodeUpstreamUnauthorized = "DK_UPSTREAM_UNAUTHORIZED"

	// —— 出图过程 ——
	ErrCodeContentBlocked = "DK_CONTENT_BLOCKED"
	ErrCodeTimeout        = "DK_TIMEOUT"
	ErrCodeCanceled       = "DK_CANCELED"
	ErrCodeUpstreamError  = "DK_UPSTREAM_ERROR"
	// ErrCodeStale 进程重启 / 崩溃导致任务卡死，被巡检回收。
	//
	// ⚠ 9001 迁移的注释里把它写成了 "DK-STALE"（中划线）。
	// 那只是注释、不是约束（列是裸 VARCHAR(64)），而全模块的错误码统一
	// UPPER_SNAKE，所以**以这里的 DK_STALE 为准**。
	ErrCodeStale = "DK_STALE"

	// —— 请求参数 ——
	ErrCodeInvalidRequest         = "DK_INVALID_REQUEST"
	ErrCodeRatioNotAllowed        = "DK_RATIO_NOT_ALLOWED"
	ErrCodeBatchTooLarge          = "DK_BATCH_TOO_LARGE"
	ErrCodeIdempotencyKeyRequired = "DK_IDEMPOTENCY_KEY_REQUIRED"
	ErrCodeIdempotencyKeyConflict = "DK_IDEMPOTENCY_KEY_CONFLICT"
	ErrCodeUnsupportedImageFormat = "DK_UNSUPPORTED_IMAGE_FORMAT"
	ErrCodeImageTooLarge          = "DK_IMAGE_TOO_LARGE"
	ErrCodeMaxAttemptsExceeded    = "DK_MAX_ATTEMPTS_EXCEEDED"
	ErrCodeJobAlreadySettled      = "DK_JOB_ALREADY_SETTLED"
	ErrCodeIllegalStateTransition = "DK_ILLEGAL_STATE_TRANSITION"

	// —— 找不到 ——
	ErrCodeJobNotFound         = "DK_JOB_NOT_FOUND"
	ErrCodeItemNotFound        = "DK_ITEM_NOT_FOUND"
	ErrCodeAssetNotFound       = "DK_ASSET_NOT_FOUND"
	ErrCodeImageNotFound       = "DK_IMAGE_NOT_FOUND"
	ErrCodePromptNotFound      = "DK_PROMPT_NOT_FOUND"
	ErrCodeChatSessionNotFound = "DK_CHAT_SESSION_NOT_FOUND"

	// —— 鉴权 ——
	ErrCodeUnauthorized  = "DK_UNAUTHORIZED"
	ErrCodeForbidden     = "DK_FORBIDDEN"
	ErrCodeAPIKeyMissing = "DK_API_KEY_MISSING"

	// —— 高清放大 ——
	ErrCodeUpscaleUnavailable = "DK_UPSCALE_UNAVAILABLE"
	ErrCodeUpscaleQueueFull   = "DK_UPSCALE_QUEUE_FULL"
	ErrCodeUpscaleNotFound    = "DK_UPSCALE_NOT_FOUND"
	ErrCodeUpscaleFailed      = "DK_UPSCALE_FAILED"

	// —— 我们自己的基础设施 ——
	ErrCodePreprocessFailed = "DK_PREPROCESS_FAILED"
	ErrCodeStorageError     = "DK_STORAGE_ERROR"
	ErrCodeSyncInProgress   = "DK_SYNC_IN_PROGRESS"
	ErrCodeInternal         = "DK_INTERNAL"
)

// errorCatalog 错误码 → (中文文案, HTTP 状态)。
//
// 文案规矩：说清「发生了什么」+「运营该干什么」。
// 不许出现「元」（金额一律美元），不许把英文原文抄进来。
// 「请联系管理员」这类话要配套 designkit_settings.admin_contact 里的联系方式
// —— 界面渲染时把联系方式拼在后面（决策 19）。
var errorCatalog = map[string]DesignkitError{
	// —— 钱 ——
	ErrCodeInsufficientBalance: {
		Message:    "余额不足，这一批出不了。可以点「申请额度」通知管理员，或者少选几张再提交。",
		HTTPStatus: http.StatusPaymentRequired,
	},
	ErrCodeQuotaExhausted: {
		Message:    "这个账号的用量额度已经用完了，请联系管理员调整额度。",
		HTTPStatus: http.StatusForbidden,
	},

	// —— 排队 / 限流 ——
	ErrCodeUpstreamBusy: {
		Message:    "出图通道正忙，这张排在后面了，稍等会自动重试。",
		HTTPStatus: http.StatusTooManyRequests,
	},
	ErrCodeRateLimited: {
		Message:    "操作太频繁了，请等几秒再试。",
		HTTPStatus: http.StatusTooManyRequests,
	},

	// —— 上游账号 / 分组配置 ——
	ErrCodeNoAvailableAccount: {
		Message:    "暂时没有可用的出图账号，请稍后再试；一直这样就是账号出问题了，请联系管理员。",
		HTTPStatus: http.StatusServiceUnavailable,
	},
	ErrCodeImageNotEnabled: {
		Message:    "当前账号所在的分组没有开通出图功能，请联系管理员在分组设置里打开图片生成。",
		HTTPStatus: http.StatusForbidden,
	},
	ErrCodeModelNotAllowed: {
		Message:    "当前分组不允许使用这个出图模型，请联系管理员把模型加进白名单。",
		HTTPStatus: http.StatusForbidden,
	},
	ErrCodeImagesAPIUnsupported: {
		Message:    "当前分组绑定的账号类型不支持出图接口，请联系管理员换一个分组。",
		HTTPStatus: http.StatusNotFound,
	},
	ErrCodeUpstreamBaseURL: {
		Message:    "出图账号的「中转地址」填得不对，连不上，请联系管理员检查账号配置。",
		HTTPStatus: http.StatusBadGateway,
	},
	ErrCodeUpstreamUnauthorized: {
		Message:    "出图账号的密钥无效或已过期，请联系管理员更新。",
		HTTPStatus: http.StatusBadGateway,
	},

	// —— 出图过程 ——
	ErrCodeContentBlocked: {
		Message:    "这条提示词或这张商品图被内容审核拦下了，换个说法或换张图再试。",
		HTTPStatus: http.StatusUnprocessableEntity,
	},
	ErrCodeTimeout: {
		// ⚠ **这条文案有两处副本，必须一字不差**（设计定型 8.1）：
		// 这一份是界面和对外 JSON 真正显示的（按 last_error_code 查表拿文案），
		// 另一份是 worker/errors.go 的 timeoutMessage（worker 内部的错误对象和日志）。
		// worker 包里有一条单测把两份钉在一起，改一处不同步另一处会当场炸。
		//
		// 为什么不能写成「可以点重试」那种轻描淡写的话：超时那一张多半上游
		// **已经出图、已经扣过钱**了，只是结果没回到我们手上（上游调用跟调用方连接
		// 是故意脱钩的）。运营默认「失败 = 没扣钱」，一顿猛点重试，一张图能扣好几份。
		// 所以必须先说「可能已经计费」，再说「重试会重新收费」。
		Message:    "这张图可能已经生成并计费，但我们没收到结果。需要的话可以手动重试（会重新收费）。",
		HTTPStatus: http.StatusGatewayTimeout,
	},
	ErrCodeCanceled: {
		Message:    "这张图的请求被中断了。",
		HTTPStatus: http.StatusInternalServerError,
	},
	ErrCodeUpstreamError: {
		Message:    "出图失败，请稍后重试；一直失败就把这一行的错误码发给管理员。",
		HTTPStatus: http.StatusBadGateway,
	},
	ErrCodeStale: {
		Message:    "系统重启导致这批任务中断了，可以重新提交。已经出好的图都在作品库里。",
		HTTPStatus: http.StatusInternalServerError,
	},

	// —— 请求参数 ——
	ErrCodeInvalidRequest: {
		Message:    "提交的内容有问题，请检查后重试。",
		HTTPStatus: http.StatusBadRequest,
	},
	ErrCodeRatioNotAllowed: {
		Message:    "不支持这个出图比例，请从列表里选一个。",
		HTTPStatus: http.StatusBadRequest,
	},
	ErrCodeBatchTooLarge: {
		Message:    "一次提交的张数超过上限了，请少选几张商品图或提示词。",
		HTTPStatus: http.StatusBadRequest,
	},
	ErrCodeIdempotencyKeyRequired: {
		Message:    "提交请求缺少防重复标识，请刷新页面重试。",
		HTTPStatus: http.StatusBadRequest,
	},
	ErrCodeIdempotencyKeyConflict: {
		Message:    "这个提交标识刚才用过了，但内容不一样，请刷新页面重新提交。",
		HTTPStatus: http.StatusConflict,
	},
	ErrCodeUnsupportedImageFormat: {
		Message:    "这个图片格式不支持，请上传 JPG、PNG、WEBP 或 HEIC。",
		HTTPStatus: http.StatusUnsupportedMediaType,
	},
	ErrCodeImageTooLarge: {
		Message:    "图片太大了，请压到 20MB 以内再上传。",
		HTTPStatus: http.StatusRequestEntityTooLarge,
	},
	ErrCodeMaxAttemptsExceeded: {
		Message:    "这张图已经重试到上限了，不能再试。想继续可以重新提交一批。",
		HTTPStatus: http.StatusConflict,
	},
	ErrCodeJobAlreadySettled: {
		Message:    "这批任务已经结算，不能再重试了，请重新提交。",
		HTTPStatus: http.StatusConflict,
	},
	ErrCodeIllegalStateTransition: {
		Message:    "这个任务的状态已经变了，请刷新页面看最新进度。",
		HTTPStatus: http.StatusConflict,
	},

	// —— 找不到 ——
	ErrCodeJobNotFound: {
		Message:    "找不到这个任务，可能已经被删除了。",
		HTTPStatus: http.StatusNotFound,
	},
	ErrCodeChatSessionNotFound: {
		Message:    "找不到这组对话，可能已经被删除了。",
		HTTPStatus: http.StatusNotFound,
	},
	ErrCodeItemNotFound: {
		Message:    "找不到这一张图的记录。",
		HTTPStatus: http.StatusNotFound,
	},
	ErrCodeAssetNotFound: {
		Message:    "找不到这张商品图，可能已经被删除了。",
		HTTPStatus: http.StatusNotFound,
	},
	ErrCodeImageNotFound: {
		Message:    "找不到这张结果图，可能已经被删除了。",
		HTTPStatus: http.StatusNotFound,
	},
	ErrCodePromptNotFound: {
		Message:    "找不到这条提示词，可能已经从灵感库下架了。",
		HTTPStatus: http.StatusNotFound,
	},

	// —— 鉴权 ——
	ErrCodeUnauthorized: {
		Message:    "登录已过期，请重新登录。",
		HTTPStatus: http.StatusUnauthorized,
	},
	ErrCodeForbidden: {
		Message:    "没有权限做这个操作。",
		HTTPStatus: http.StatusForbidden,
	},
	ErrCodeAPIKeyMissing: {
		// 每个运营用户首次进工作台时服务端会自动建一把内部专用 Key；建失败时报这个。
		Message:    "账号还没开通出图权限，请联系管理员。",
		HTTPStatus: http.StatusForbidden,
	},

	// —— 高清放大 ——
	//
	// 文案与前端 i18n 的 designkit.upscale.* 逐字对齐（monica 定稿），改一边必须改另一边。
	ErrCodeUpscaleUnavailable: {
		Message:    "放大功能还没准备好，请联系管理员。",
		HTTPStatus: http.StatusServiceUnavailable,
	},
	ErrCodeUpscaleQueueFull: {
		Message:    "排队满了，等会儿再试。",
		HTTPStatus: http.StatusTooManyRequests,
	},
	ErrCodeUpscaleNotFound: {
		Message:    "没有这张图的放大任务，重新点「高清放大」。",
		HTTPStatus: http.StatusNotFound,
	},
	ErrCodeUpscaleFailed: {
		Message:    "放大失败，重试一次。",
		HTTPStatus: http.StatusBadGateway,
	},

	// —— 我们自己的基础设施 ——
	ErrCodePreprocessFailed: {
		// fail-closed：Python 预处理失败绝不回吐原图。
		// 回吐原图 = 运营选 3:4 拿回 4:3，钱照扣，只有一行 warning。
		Message:    "商品图处理失败（补白边或格式转换），请换一张图再试。",
		HTTPStatus: http.StatusBadGateway,
	},
	ErrCodeStorageError: {
		Message:    "图片存取失败，请稍后重试；一直失败请联系管理员。",
		HTTPStatus: http.StatusInternalServerError,
	},
	ErrCodeSyncInProgress: {
		Message:    "灵感库正在同步，请等这一次跑完再点。",
		HTTPStatus: http.StatusConflict,
	},
	ErrCodeInternal: {
		Message:    "系统开小差了，请稍后重试；一直这样请联系管理员。",
		HTTPStatus: http.StatusInternalServerError,
	},
}

// NewError 按错误码造一个错误。未知错误码退化成 DK_INTERNAL（绝不 panic）。
func NewError(code string) *DesignkitError {
	tmpl, ok := errorCatalog[code]
	if !ok {
		tmpl = errorCatalog[ErrCodeInternal]
		code = ErrCodeInternal
	}
	return &DesignkitError{
		Code:       code,
		Message:    tmpl.Message,
		HTTPStatus: tmpl.HTTPStatus,
	}
}

// ErrorCatalog 返回错误码 → 模板的**副本**，给接口文档生成和契约测试用。
func ErrorCatalog() map[string]DesignkitError {
	out := make(map[string]DesignkitError, len(errorCatalog))
	for code, tmpl := range errorCatalog {
		tmpl.Code = code
		out[code] = tmpl
	}
	return out
}

// ============================================================================
// 上游英文错误 → 我方错误码
// ============================================================================

// upstreamErrorRule 一条匹配规则。
type upstreamErrorRule struct {
	// keywords 全小写。上游原文里**包含任意一个**就算命中。
	keywords []string
	// code 命中后用哪个我方错误码。
	code string
}

// upstreamErrorRules 顺序敏感，**从具体到笼统**，第一条命中即返回。
//
// 每一条的关键词都是从上游源码里 grep 出来的原文，不是编的：
//
//	internal/handler/ops_error_logger.go:1622-1651        （上游自己的关键词清单）
//	internal/handler/openai_gateway_handler.go:2475       "Image generation concurrency limit exceeded, please retry later"
//	internal/handler/concurrency_error_response.go:16,24  "Too many pending requests, please retry later" / "Concurrency limit exceeded for %s, please retry later"
//	internal/service/image_generation_intent.go:15        "Image generation is not enabled for this group"
//	internal/service/openai_gateway_forward.go:276,325    "image generation disabled for group"
//	internal/server/middleware/api_key_auth.go:264        403 INSUFFICIENT_BALANCE "Insufficient account balance"
//	internal/handler/gateway_handler.go:385               503 "No available accounts"
//	internal/server/routes/gateway.go:90                  404 "Images API is not supported for this platform"
//	internal/util/urlvalidator/validator.go:49,64         "host is not allowed: %s"
//	internal/service/gateway_upstream_request.go:906,916  "invalid base_url: %w"
//	internal/service/grok_upstream_errors.go:150-172      内容审核短语表
//
// ⚠ 跟版检查清单里要加一条：上游改了这些字面量，我们这里会**静默退化**成
// DK_UPSTREAM_ERROR（运营看到的是笼统的「出图失败」而不是「分组没开生图」）。
// 不会报错，只会变难用。
var upstreamErrorRules = []upstreamErrorRule{
	// 内容审核放最前：它的措辞里也常带 "not available"，被别的规则抢走就不好排查了。
	{code: ErrCodeContentBlocked, keywords: []string{
		"content policy", "content_policy", "content moderation", "content_moderation",
		"content filter", "content_filter", "moderation_blocked", "moderation blocked",
		"image is sensitive", "text is sensitive", "prohibited content", "forbidden content",
		"violates policy", "violates content policy", "safety system", "new_sensitive",
	}},

	// 分组没开生图。跟「并发超限」区分开，两条都含 "image generation"。
	{code: ErrCodeImageNotEnabled, keywords: []string{
		"image generation is not enabled for this group",
		"image generation disabled for group",
	}},

	// 排队/并发。必须排在超时规则前面 ——
	// ConcurrencyError 超时时的原文是 "timeout waiting for openai concurrency slot"，
	// 含 "timeout"，被超时规则抢走就会给运营看错方向的提示。
	{code: ErrCodeUpstreamBusy, keywords: []string{
		"image generation concurrency limit exceeded",
		"concurrency limit exceeded",
		"concurrency limit reached",
		"concurrency slot",
		"too many pending requests",
	}},

	{code: ErrCodeRateLimited, keywords: []string{
		"requests-per-minute limit exceeded",
		"rate limit exceeded",
	}},

	// Key / 分组 / 用户的用量额度用完。要排在「余额不足」前面：
	// 两者对运营的动作不同（一个是加额度，一个是充钱）。
	{code: ErrCodeQuotaExhausted, keywords: []string{
		"api key 额度已用完",
		"api key 5小时限额已用完",
		"api key 日限额已用完",
		"api key 7天限额已用完",
		"daily usage limit exceeded",
		"weekly usage limit exceeded",
		"monthly usage limit exceeded",
		"usage quota exhausted for this platform",
		"quota exceeded",
		"insufficient_quota",
	}},

	{code: ErrCodeInsufficientBalance, keywords: []string{
		"insufficient balance",
		"insufficient account balance",
		"run out of credits",
		"billing hard limit",
	}},

	{code: ErrCodeNoAvailableAccount, keywords: []string{
		"no available account", // 覆盖 "no available accounts" 和各家的变体
		"no healthy account",
		"no healthy upstream account",
		"account pool",
	}},

	{code: ErrCodeModelNotAllowed, keywords: []string{
		"not in whitelist",
		"model not supported",
		"unsupported model",
		"does not support the requested model",
		"not supported by any configured account",
	}},

	{code: ErrCodeImagesAPIUnsupported, keywords: []string{
		"images api is not supported for this platform",
	}},

	{code: ErrCodeUpstreamBaseURL, keywords: []string{
		"invalid base_url",
		"host is not allowed",
	}},

	{code: ErrCodeUpstreamUnauthorized, keywords: []string{
		"invalid_api_key",
		"invalid api key",
		"incorrect api key",
		"unauthorized",
		"authentication_error",
	}},

	// 取消要排在超时前面：context.Canceled 的文案里不含 timeout，但顺序写清楚更安全。
	{code: ErrCodeCanceled, keywords: []string{
		"context canceled",
		"client disconnected",
	}},

	{code: ErrCodeTimeout, keywords: []string{
		"context deadline exceeded",
		"timed out",
		"timeout",
	}},
}

// MapUpstreamError 把上游的（HTTP 状态 + 英文原文）翻译成我方错误。
//
// 返回的 *DesignkitError 里：
//   - Code    → 落 designkit_job_items.last_error_code
//   - Message → 中文，界面显示这个
//   - Upstream→ 上游原文，落 last_error_message，**只给管理员看**
//
// upstreamStatus 传 0 表示没有 HTTP 状态（比如本地超时、连接失败）。
// 关键词优先于状态码：状态码太笼统（上游把「分组没开生图」和「模型不在白名单」
// 都返 403），只靠状态码翻译出来的中文会指错方向。
func MapUpstreamError(upstreamStatus int, upstreamMessage string) *DesignkitError {
	raw := strings.TrimSpace(upstreamMessage)
	lower := strings.ToLower(raw)

	if lower != "" {
		for _, rule := range upstreamErrorRules {
			for _, kw := range rule.keywords {
				if strings.Contains(lower, kw) {
					return NewError(rule.code).WithUpstream(raw)
				}
			}
		}
	}

	return NewError(codeForUpstreamStatus(upstreamStatus)).WithUpstream(raw)
}

// codeForUpstreamStatus 关键词都没命中时，按 HTTP 状态兜底。
func codeForUpstreamStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return ErrCodeUpstreamUnauthorized
	case http.StatusPaymentRequired:
		return ErrCodeInsufficientBalance
	case http.StatusTooManyRequests:
		return ErrCodeUpstreamBusy
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return ErrCodeTimeout
	case http.StatusUnprocessableEntity:
		return ErrCodeContentBlocked
	}
	// 其余情况（含 status = 0：本地超时、连接失败、上游根本没应答）一律给笼统的
	// DK_UPSTREAM_ERROR。宁可笼统也不要猜错方向 —— 猜错会让运营照着错误的提示
	// 去改一个跟故障无关的地方。
	return ErrCodeUpstreamError
}
