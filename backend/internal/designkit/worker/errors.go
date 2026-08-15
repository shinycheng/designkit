package worker

import (
	"context"
	"errors"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ============================================================================
// 「这一张还要不要再试」
// ============================================================================
//
// 判断错了两个方向都花钱：
//
//	该重试的判成终态 → 运营看到一张失败图，只能手动点重试（还是要重新花钱），
//	                    但至少钱没白花两遍；
//	不该重试的判成可重试 → 每次重试都是**重新出一张图、重新收一次钱**。
//	                    内容审核拦截、提示词违规、图片格式不支持这些，
//	                    重试 3 次就是白烧 3 张图的钱，而且 3 次的结果一模一样。
//
// 所以规矩是：**只有「换个时间点可能就好了」的错误才重试。**
// 配置错（分组没开生图、模型不在白名单、中转地址填错）、内容错（审核拦截、
// 图片格式不支持）、余额错，都直接判终态，让运营去找管理员，
// 而不是让系统替他把额度烧完。

// itemError 是本包内部用的错误包装，只多带一个「能不能重试」。
//
// 为什么需要它：domain 的错误码本身不带重试语义（同一个 DK_UPSTREAM_ERROR，
// 「上游 500」该重试、「上游返回 0 张图」不该重试 —— 后者钱多半已经花了）。
type itemError struct {
	err       *dkdomain.DesignkitError
	retryable bool
}

func (e *itemError) Error() string {
	if e == nil || e.err == nil {
		return "<nil designkit item error>"
	}
	return e.err.Error()
}

func (e *itemError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// failRetry 造一个「等会儿再试」的错误。
func failRetry(code string) *itemError {
	return &itemError{err: dkdomain.NewError(code), retryable: true}
}

// failFinal 造一个「别再试了」的错误。
func failFinal(code string) *itemError {
	return &itemError{err: dkdomain.NewError(code), retryable: false}
}

// withCause 挂上原始 error（日志和排障用，不进界面）。
func (e *itemError) withCause(cause error) *itemError {
	if e == nil {
		return failFinal(dkdomain.ErrCodeInternal).withCause(cause)
	}
	return &itemError{err: e.err.WithCause(cause), retryable: e.retryable}
}

// withUpstream 记下上游英文原文（落 last_error_message，只给管理员看）。
func (e *itemError) withUpstream(raw string) *itemError {
	if e == nil {
		return failFinal(dkdomain.ErrCodeInternal).withUpstream(raw)
	}
	return &itemError{err: e.err.WithUpstream(raw), retryable: e.retryable}
}

// retryableCodes 是「换个时间点可能就好了」的错误码。
// 不在这张表里的一律不重试。**加一条之前先问：重试一次要不要重新花钱？**
var retryableCodes = map[string]bool{
	// 上游排队 / 限流：等一会儿就好，而且这两种情况下图根本没出，不花钱。
	dkdomain.ErrCodeUpstreamBusy:  true,
	dkdomain.ErrCodeRateLimited:   true,
	dkdomain.ErrCodeUpstreamError: true,

	// 没有可用账号：多半是某个上游账号临时挂了，健康检查恢复后就好。
	dkdomain.ErrCodeNoAvailableAccount: true,

	// 我们自己这边的抖动：数据库连接断了、对象存储临时不可用。
	// 注意这些都发生在**调网关之前或之后**，重试不会多出一张图的钱。
	dkdomain.ErrCodeInternal: true,
}

// ⚠ **DK_TIMEOUT 故意不在上面那张表里**（2026-08-13 修，原来是 true）。
//
// 原来错在哪：超时那一张多半上游**已经出图、已经扣过钱**，只是结果没回到我们手上
// （`service/openai_images.go` 的 detachUpstreamContext 明写「上游调用跟调用方连接
// 故意脱钩」——我们这边超时不代表上游停手）。自动重试 = attempt +1 = 换一个新的
// request_id = 上游把它当成一次全新的请求**再收一次钱**。默认 max_attempts=3，
// 最坏情况一张图扣 3 份钱，而运营只看到「失败了」。
//
// 现在的做法：超时直接判 failed、错误码 DK_TIMEOUT，**由人来决定要不要再花一次钱**。
// 重试按钮仍然在（决策 20 的手动重试，点了会二次确认并显示单价）。
//
// 这条同时写进了 `designkit/docs/设计定型.md` 第八节。

// timeoutMessage 超时给运营看的中文：必须说清楚「钱可能已经花了」，
// 否则运营会以为「失败了 = 没扣钱」，一顿猛点重试。
//
// ⚠ **必须跟 `domain/errors.go` 里 DK_TIMEOUT 那条的 Message 一字不差。**
// 界面和对外 JSON 真正显示的是那一份（按 last_error_code 查表拿文案），
// 这里这一份只作用于 worker 内部的错误对象和日志。改一处必须同步另一处。
const timeoutMessage = "这张图可能已经生成并计费，但我们没收到结果。需要的话可以手动重试（会重新收费）。"

// ============================================================================
// 「这一张还有没有剩余的尝试次数」
// ============================================================================
//
// attempt_count 在**领取那一刻**就 +1（设计定型 第六节），所以：
//
//	attempt_count <  max_attempts → 还能再排一次
//	attempt_count == max_attempts → 正在跑的就是最后一次合法尝试
//	attempt_count >  max_attempts → **这次领取本身就越界了**，绝不能调网关
//
// 两个函数差一个等号，用错的代价：
// 用 >= 当 exceeded，会把最后一次合法尝试也毙掉，运营白少一次机会；
// 用 >  当 exhausted，会把一张已经试满的图再排一次，**真出图、真扣钱**。

// normalizeMaxAttempts 兜住 max_attempts 是 0 或负数的情况。
// 库里这一列是 NOT NULL DEFAULT 3，但结构体可能来自别处；取 0 会让
// 「attempt >= max」永远成立，第一次尝试就被判成超限，一张图都出不来。
func normalizeMaxAttempts(maxAttempts int) int {
	if maxAttempts <= 0 {
		return dkdomain.DefaultMaxAttempts
	}
	return maxAttempts
}

// attemptBudgetExceeded 这一次领取是不是已经越过上限。**领到 item 后第一件事就判它。**
func attemptBudgetExceeded(attemptCount, maxAttempts int) bool {
	return attemptCount > normalizeMaxAttempts(maxAttempts)
}

// attemptBudgetExhausted 还能不能**再排一次**。
// 退回队列（退避重试、停机交还租约）之前必须判它 —— 排下去就是下一次真出图。
func attemptBudgetExhausted(attemptCount, maxAttempts int) bool {
	return attemptCount >= normalizeMaxAttempts(maxAttempts)
}

// classify 把出图路上的任意 error 翻译成「落库的错误码 + 要不要重试」。
//
// attemptCount 是**领取时就已经 +1** 的那个值（domain 的约定），
// 「已经试满了」的判据统一走 attemptBudgetExhausted，不要在这里另写等号。
func classify(err error, attemptCount, maxAttempts int) (dkErr *dkdomain.DesignkitError, terminal bool) {
	if err == nil {
		return nil, true
	}

	var retryable bool

	var itemErr *itemError
	switch {
	case errors.As(err, &itemErr):
		dkErr, retryable = itemErr.err, itemErr.retryable

	case errors.Is(err, context.DeadlineExceeded):
		// 我们自己的单张超时（itemCtx 到期）。**不自动重试**，理由见 timeoutMessage
		// 上面那段：这一张多半上游已经出了、已经扣过钱了。
		dkErr, retryable = dkdomain.NewError(dkdomain.ErrCodeTimeout).WithCause(err), false

	case errors.Is(err, context.Canceled):
		// 出图被我们自己取消（进程要退出了）。不算失败也不该重试计数 ——
		// 调用方走的是「把租约还回去」那条路，不会走到这里；
		// 真走到了就当一次可重试的中断。
		dkErr, retryable = dkdomain.NewError(dkdomain.ErrCodeCanceled).WithCause(err), true

	default:
		if parsed, ok := dkdomain.AsDesignkitError(err); ok {
			dkErr = parsed
			retryable = retryableCodes[parsed.Code]
		} else {
			// 不认识的错误一律当我们自己的内部错误：可重试、但界面显示的是
			// 「系统开小差了」而不是把 Go 的 error 字符串糊到运营脸上。
			dkErr = dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
			retryable = true
		}
	}

	if dkErr == nil {
		dkErr = dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
	// 超时无论从哪条分支进来（我们自己的 deadline、上游 504、关键词命中），
	// 文案都要写明「钱可能已经花了」，统一在这里覆写一次。
	if dkErr.Code == dkdomain.ErrCodeTimeout {
		dkErr = dkErr.WithMessage(timeoutMessage)
	}
	if !retryable {
		return dkErr, true
	}
	// 还能重试的错误，但次数已经用完了 —— 换成「重试到上限」的文案，
	// 免得运营看到「稍后会自动重试」却永远等不到。
	if attemptBudgetExhausted(attemptCount, maxAttempts) {
		return dkdomain.NewError(dkdomain.ErrCodeMaxAttemptsExceeded).
			WithUpstream(dkErr.Upstream).
			WithCause(dkErr), true
	}
	return dkErr, false
}

// retryBackoff 单张图失败后的退避时长：base × 2^(attempt-1)，封顶 5 分钟。
//
// 没有加随机抖动，因为退避是**按 item 各自算**的，不存在一群 worker 同时醒来
// 猛敲同一个上游的问题；不加抖动换来的是这个函数完全可预测、可断言。
func retryBackoff(base time.Duration, attemptCount int) time.Duration {
	if base <= 0 {
		base = DefaultRetryBackoffBase
	}
	if attemptCount < 1 {
		attemptCount = 1
	}
	// 位移次数封顶，防止 attempt 很大时移出 int64 变成负数。
	shift := attemptCount - 1
	if shift > 16 {
		shift = 16
	}
	d := base * (1 << uint(shift))
	if d <= 0 || d > DefaultRetryBackoffMax {
		return DefaultRetryBackoffMax
	}
	return d
}

// settleBackoff 结算对账没命中时的退避：base × 2^(attempt-1)，封顶 2 分钟。
func settleBackoff(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		base = DefaultSettleBackoffBase
	}
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 16 {
		shift = 16
	}
	d := base * (1 << uint(shift))
	if d <= 0 || d > DefaultSettleBackoffMax {
		return DefaultSettleBackoffMax
	}
	return d
}
