//go:build unit

package domain

import (
	"fmt"
	"testing"

	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/stretchr/testify/require"
)

// TestClassifyBillingTier_MatchesUpstream 拿上游的 ClassifyImageBillingTier
// 逐点比对我们的 ClassifyBillingTier。
//
// 为什么值得为这一条测试破例 import 上游包：
// 计费档决定单价。上游哪天把分档阈值从 1024/2048 改掉（跟版每 2~4 周一次），
// 我们的预估会跟实际计费错档，而这种错**不会报错**，只会让报价失准、
// 运营抱怨「说好 $2 结果扣了 $6」。让它在 CI 里当场炸掉，比事后查账便宜得多。
//
// 这是本包唯一一处 import 上游代码的地方，而且只在测试里。
// 万一以后上游包变得 import 不动了，删掉本文件即可，其余测试不受影响。
func TestClassifyBillingTier_MatchesUpstream(t *testing.T) {
	dims := []int{1, 100, 512, 1023, 1024, 1025, 1152, 1254, 1536, 2047, 2048, 2049, 3000, 3840, 8000}

	for _, w := range dims {
		for _, h := range dims {
			size := fmt.Sprintf("%dx%d", w, h)
			want, ok := upstreamservice.ClassifyImageBillingTier(size)
			require.Truef(t, ok, "上游对 %s 应该能分出档", size)

			got := ClassifyBillingTier(w, h)
			require.Equalf(t, want, got,
				"%s：上游分 %s，我们分 %s —— 分档规则跟上游对不上，报价必然失准", size, want, got)
		}
	}
}

// TestBillingTierConstants_MatchUpstream 档位字面量也要一致。
// 我们的 job_items.billing_tier 是拿去跟 usage_logs 对账的，
// 字面量不一致（比如我们写 "2k" 上游写 "2K"）会让对账查不到。
func TestBillingTierConstants_MatchUpstream(t *testing.T) {
	require.Equal(t, upstreamservice.ImageBillingSize1K, BillingTier1K)
	require.Equal(t, upstreamservice.ImageBillingSize2K, BillingTier2K)
	require.Equal(t, upstreamservice.ImageBillingSize4K, BillingTier4K)
}

// TestClassifyBillingTierOrDefault_MatchesUpstreamFallback 兜底值也一致（2K）。
func TestClassifyBillingTierOrDefault_MatchesUpstreamFallback(t *testing.T) {
	require.Equal(t,
		upstreamservice.NormalizeImageBillingTierOrDefault("auto"),
		ClassifyBillingTierOrDefault(0, 0),
		"分不出档时的兜底档必须跟上游一致，否则预估会系统性偏低或偏高")
}
