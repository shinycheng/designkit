//go:build unit

package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/require"
)

// TestClassify_RetryDecision 「这一张还要不要再试」的全部判据。
//
// 判错的代价：不该重试的判成可重试，每重试一次就是**重新出一张图、重新收一次钱**，
// 而结果一模一样。所以这张表只放「换个时间点可能就好了」的错误。
func TestClassify_RetryDecision(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		attempt      int
		maxAttempts  int
		wantCode     string
		wantTerminal bool
	}{
		{
			name:         "上游排队：等一会儿就好，而且没出图没花钱",
			err:          dkdomain.NewError(dkdomain.ErrCodeUpstreamBusy),
			attempt:      1,
			maxAttempts:  3,
			wantCode:     dkdomain.ErrCodeUpstreamBusy,
			wantTerminal: false,
		},
		{
			name:         "上游限流",
			err:          dkdomain.NewError(dkdomain.ErrCodeRateLimited),
			attempt:      1,
			maxAttempts:  3,
			wantCode:     dkdomain.ErrCodeRateLimited,
			wantTerminal: false,
		},
		{
			name:         "内容审核拦截：重试三次也是同一个结果，白烧三张图的钱",
			err:          dkdomain.NewError(dkdomain.ErrCodeContentBlocked),
			attempt:      1,
			maxAttempts:  3,
			wantCode:     dkdomain.ErrCodeContentBlocked,
			wantTerminal: true,
		},
		{
			name:         "分组没开生图：配置问题，得管理员去改",
			err:          dkdomain.NewError(dkdomain.ErrCodeImageNotEnabled),
			attempt:      1,
			maxAttempts:  3,
			wantCode:     dkdomain.ErrCodeImageNotEnabled,
			wantTerminal: true,
		},
		{
			name:         "余额不足：这一批都出不完了，重试只会一直失败",
			err:          dkdomain.NewError(dkdomain.ErrCodeInsufficientBalance),
			attempt:      1,
			maxAttempts:  3,
			wantCode:     dkdomain.ErrCodeInsufficientBalance,
			wantTerminal: true,
		},
		{
			name:         "预处理失败：纯函数，重试还是同样的失败",
			err:          failFinal(dkdomain.ErrCodePreprocessFailed),
			attempt:      1,
			maxAttempts:  3,
			wantCode:     dkdomain.ErrCodePreprocessFailed,
			wantTerminal: true,
		},
		{
			// 2026-08-13 改：原来判可重试，那会让一张图最坏扣 3 份钱。
			name:         "超时：上游多半已经出图并扣过钱了，不许自动重试",
			err:          context.DeadlineExceeded,
			attempt:      1,
			maxAttempts:  3,
			wantCode:     dkdomain.ErrCodeTimeout,
			wantTerminal: true,
		},
		{
			name:         "上游 504：同样是超时，同样不自动重试",
			err:          dkdomain.MapUpstreamError(504, "upstream request timed out"),
			attempt:      1,
			maxAttempts:  3,
			wantCode:     dkdomain.ErrCodeTimeout,
			wantTerminal: true,
		},
		{
			name:         "max_attempts 是 0（脏数据）：按默认 3 兜底，第一次尝试不能被判成超限",
			err:          dkdomain.NewError(dkdomain.ErrCodeUpstreamBusy),
			attempt:      1,
			maxAttempts:  0,
			wantCode:     dkdomain.ErrCodeUpstreamBusy,
			wantTerminal: false,
		},
		{
			name:         "次数用完：可重试的错误也要换成「重试到上限」的文案",
			err:          dkdomain.NewError(dkdomain.ErrCodeUpstreamBusy),
			attempt:      3,
			maxAttempts:  3,
			wantCode:     dkdomain.ErrCodeMaxAttemptsExceeded,
			wantTerminal: true,
		},
		{
			name:         "不认识的错误：当我们自己的内部错误，可重试",
			err:          errors.New("dial tcp: connection refused"),
			attempt:      1,
			maxAttempts:  3,
			wantCode:     dkdomain.ErrCodeInternal,
			wantTerminal: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dkErr, terminal := classify(tc.err, tc.attempt, tc.maxAttempts)
			require.NotNil(t, dkErr)
			require.Equal(t, tc.wantCode, dkErr.Code)
			require.Equal(t, tc.wantTerminal, terminal)
			require.NotEmpty(t, dkErr.Message, "界面上给运营看的中文不能是空的")
			require.NotContains(t, dkErr.Message, "元", "金额一律美元，文案里不许出现「元」")
		})
	}
}

// TestClassify_KeepsUpstreamText 上游英文原文要一路带到 last_error_message，
// 但**界面只显示中文**。
func TestClassify_KeepsUpstreamText(t *testing.T) {
	upstream := "Image generation is not enabled for this group"
	dkErr, terminal := classify(dkdomain.MapUpstreamError(403, upstream), 1, 3)

	require.True(t, terminal)
	require.Equal(t, dkdomain.ErrCodeImageNotEnabled, dkErr.Code)
	require.Equal(t, upstream, dkErr.Upstream)
	require.NotContains(t, dkErr.Message, "not enabled", "运营看到的必须是中文")
}

// TestClassify_MaxAttemptsKeepsUpstreamText 换成「重试到上限」之后，
// 上游原文仍然要留着给管理员看。
func TestClassify_MaxAttemptsKeepsUpstreamText(t *testing.T) {
	upstream := "Too many pending requests, please retry later"
	dkErr, terminal := classify(dkdomain.MapUpstreamError(429, upstream), 3, 3)

	require.True(t, terminal)
	require.Equal(t, dkdomain.ErrCodeMaxAttemptsExceeded, dkErr.Code)
	require.Equal(t, upstream, dkErr.Upstream)
}

// TestClassify_TimeoutIsNeverAutoRetried 超时**绝不能**进自动重试队列。
//
// 超时那张多半上游已经出图、已经扣过钱，只是结果没回到我们手上。
// 自动重试 = attempt+1 = 新的 request_id = 上游再收一次钱，最坏一张扣 3 份。
// 这条断言是那笔钱唯一的守卫，别为了「运营总想拿到图」把它改回去。
func TestClassify_TimeoutIsNeverAutoRetried(t *testing.T) {
	require.False(t, retryableCodes[dkdomain.ErrCodeTimeout],
		"DK_TIMEOUT 一旦回到可重试表里，一张图最坏会被扣 3 份钱")

	for _, attempt := range []int{1, 2, 3} {
		dkErr, terminal := classify(context.DeadlineExceeded, attempt, 3)
		require.True(t, terminal, "第 %d 次尝试超时也必须直接判死", attempt)
		require.Equal(t, dkdomain.ErrCodeTimeout, dkErr.Code,
			"错误码要保持 DK_TIMEOUT，不能被换成 DK_MAX_ATTEMPTS_EXCEEDED —— "+
				"运营要能一眼看出这是超时、钱可能已经花了")
	}
}

// TestClassify_TimeoutMessageWarnsAboutMoney 超时的中文必须写明「钱可能已经花了」。
//
// 只写「超时了」，运营会默认「失败 = 没扣钱」，然后一顿猛点重试。
func TestClassify_TimeoutMessageWarnsAboutMoney(t *testing.T) {
	dkErr, _ := classify(context.DeadlineExceeded, 1, 3)
	require.Equal(t, timeoutMessage, dkErr.Message)
	require.Contains(t, dkErr.Message, "计费")
	require.Contains(t, dkErr.Message, "重新收费")
	require.NotContains(t, dkErr.Message, "元", "金额一律美元")
}

// TestTimeoutMessage_MatchesDomainCatalog 超时文案的两份副本必须**一字不差**。
//
// 设计定型 8.1 写死了这一条。为什么值得单开一条测试：
// 界面和对外 JSON 真正显示的是 domain/errors.go 那一份（按 last_error_code 查表），
// 本包这一份只作用于 worker 内部的错误对象和日志。改了这边没改那边，
// **运营看到的还是旧文案**（旧文案写的是「可以点重试」，只字不提「可能已经计费」），
// 而 worker 侧的行为已经改成不自动重试了 —— 说的和做的对不上，
// 运营会照着旧提示一顿猛点重试，一张图扣好几份钱。
func TestTimeoutMessage_MatchesDomainCatalog(t *testing.T) {
	require.Equal(t, dkdomain.NewError(dkdomain.ErrCodeTimeout).Message, timeoutMessage,
		"domain/errors.go 的 DK_TIMEOUT 文案和 worker/errors.go 的 timeoutMessage 必须一字不差（设计定型 8.1）")
	require.Contains(t, timeoutMessage, "可能已经生成并计费")
	require.Contains(t, timeoutMessage, "重新收费")
	require.NotContains(t, timeoutMessage, "自动", "不许再暗示系统会自动重试")
}

// TestAttemptBudget 两个判据差一个等号，用错任何一个都要么白扣钱、要么白少一次机会。
func TestAttemptBudget(t *testing.T) {
	// attempt_count 在领取那一刻就 +1，所以 1..3 都是合法的尝试。
	for _, attempt := range []int{1, 2, 3} {
		require.False(t, attemptBudgetExceeded(attempt, 3),
			"第 %d 次是合法尝试，不能被拒绝出图", attempt)
	}
	require.True(t, attemptBudgetExceeded(4, 3), "第 4 次已经越界，绝不能调网关")

	// 「还能不能再排一次」比上面严一格。
	require.False(t, attemptBudgetExhausted(2, 3), "试到第 2 次，还剩一次机会")
	require.True(t, attemptBudgetExhausted(3, 3), "第 3 次跑完就没机会了，不能再排回队列")
	require.True(t, attemptBudgetExhausted(4, 3))

	// max_attempts 是脏数据时按默认 3 兜底，否则第一次尝试就会被判成超限。
	require.False(t, attemptBudgetExceeded(1, 0))
	require.False(t, attemptBudgetExhausted(1, -1))
	require.True(t, attemptBudgetExceeded(dkdomain.DefaultMaxAttempts+1, 0))
}

// TestRetryBackoff 退避是指数的、有上限的。
func TestRetryBackoff(t *testing.T) {
	base := 15 * time.Second
	require.Equal(t, 15*time.Second, retryBackoff(base, 1))
	require.Equal(t, 30*time.Second, retryBackoff(base, 2))
	require.Equal(t, 60*time.Second, retryBackoff(base, 3))
	require.Equal(t, DefaultRetryBackoffMax, retryBackoff(base, 99), "必须封顶，不能移位溢出成负数")
	require.Equal(t, DefaultRetryBackoffBase, retryBackoff(0, 0), "参数非法时退回默认值")
}

// TestItemErrorWrapping itemError 要能被 errors.As / errors.Is 穿透。
func TestItemErrorWrapping(t *testing.T) {
	cause := errors.New("boom")
	err := failRetry(dkdomain.ErrCodeStorageError).withCause(cause).withUpstream("  s3 timeout  ")

	require.True(t, err.retryable)
	require.ErrorIs(t, err, cause)

	dkErr, ok := dkdomain.AsDesignkitError(err)
	require.True(t, ok)
	require.Equal(t, dkdomain.ErrCodeStorageError, dkErr.Code)
	require.Equal(t, "s3 timeout", dkErr.Upstream, "上游原文要 trim")
}
