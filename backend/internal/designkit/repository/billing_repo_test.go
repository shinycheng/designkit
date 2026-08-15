//go:build unit

package repository

// 钱这一路的 SQL 单测。本机没有 Go 编译器也没有 Docker，
// 所有能不连数据库就验的东西都必须在这里验掉。
// mustContain / mustNotContain 在 query_build_test.go。

import (
	"strings"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ---- 反查账单 ----

func TestSumUsageLogCostsQueriesAllIDsInOneStatement(t *testing.T) {
	// 结算一次要查「每张图 × 每次 attempt」，50 张 × 3 次 = 150 个 id。
	// 在 Go 里循环发 150 条语句会把结算拖成分钟级。
	mustContain(t, sumUsageLogCostsSQL,
		"WHERE request_id = ANY($1)",
		"COALESCE(SUM(actual_cost), 0)",
		"GROUP BY request_id",
		// 列表查询一律显式 ORDER BY
		"ORDER BY request_id",
	)
	// GROUP BY 决定了「没扣过钱」的 id 压根不会出现在结果里 ——
	// 结算靠这一点区分「上游没扣过」和「上游扣了 0 元」。别改成 LEFT JOIN 补 0。
	mustNotContain(t, sumUsageLogCostsSQL, "LEFT JOIN")
}

// ---- 回填单张扣费 ----

func TestApplyItemBillingSQL(t *testing.T) {
	// 读旧值必须 FOR UPDATE：两个副本同时结算同一批时，
	// 「读旧值 → 算差额 → 减冻结额」这三步之间被插进去就会多减一次。
	mustContain(t, selectItemBillingForUpdateSQL, "FOR UPDATE", "billed_cost")

	mustContain(t, updateItemBillingSQL,
		"billed_cost = $2",
		"billing_mode = $3",
		"billing_tier = $4",
		"WHERE id = $1",
	)

	// 冻结额只减不越界：减到 0 就停，绝不让 amount 变成负数去放大别的任务的可用额。
	mustContain(t, decrementHoldSQL,
		"GREATEST(amount - $2, 0)",
		"WHERE job_id = $1 AND status = 'open'",
	)
}

// ---- 工作台角落那三个数 ----

// TestUsageSummaryWindowsCostOnUsageLogs 花费必须按 usage_logs 自己的时间落窗口。
//
// 原来错在哪：窗口落在 job_items.finished_at 上，而 finished_at **会被重试刷新**。
// 8 月出的图、9 月点一次重试，那一整张（含 8 月那次 attempt 的钱）就整个跑到 9 月去了，
// 月度对账永远对不上。
func TestUsageSummaryWindowsCostOnUsageLogs(t *testing.T) {
	mustContain(t, usageSummarySQL,
		"FROM usage_logs ul",
		"ul.user_id = $1",
		"ul.request_id LIKE $4",
		"ul.created_at >= $2 AND ul.created_at < $3",
		"COALESCE(SUM(ul.actual_cost), 0)",
	)
	// 这两个是老写法的痕迹，出现任何一个都说明又落回 job_items 那条路了。
	mustNotContain(t, usageSummarySQL,
		"designkit_job_items",
		"finished_at",
		"billed_cost",
	)
}

// TestUsageSummaryCountsImagesByTheirOwnCreatedAt 张数按结果图算。
// designkit_images 是「一次 attempt 一行、created_at 之后再也不会变」，
// 所以重试出的历史版本天然留在它自己的月份里（那些钱也确实花过）。
func TestUsageSummaryCountsImagesByTheirOwnCreatedAt(t *testing.T) {
	mustContain(t, usageSummarySQL,
		"FROM designkit_images i",
		"i.created_at >= $2 AND i.created_at < $3",
		"i.deleted_at IS NULL",
	)
}

// TestUsageSummaryAvailableIsBalanceMinusOpenHolds 可用额的口径只有这一个。
func TestUsageSummaryAvailableIsBalanceMinusOpenHolds(t *testing.T) {
	mustContain(t, usageSummarySQL,
		"FROM designkit_holds h",
		"h.user_id = u.id AND h.status = 'open'",
		"u.deleted_at IS NULL",
	)
}

// TestDesignkitUsageLogRequestIDPatternMatchesRealIDs 「这条账单是 designkit 出的图」
// 这个判据必须跟真正落进 usage_logs 的值对得上。
//
// 前缀哪天被改了，手写的字面量不会报错，只会让工作台上的「本月花费」悄悄变成 0 ——
// 这条断言就是防这个的。
func TestDesignkitUsageLogRequestIDPatternMatchesRealIDs(t *testing.T) {
	if designkitUsageLogRequestIDPattern != "client:dki:%" {
		t.Fatalf("前缀模式变了: %q", designkitUsageLogRequestIDPattern)
	}
	prefix := strings.TrimSuffix(designkitUsageLogRequestIDPattern, "%")

	for _, id := range []string{
		dkdomain.StoredBillingRequestID("01J8ZK7Q9X2M4N6P8R0T2V4W6Y", 1, 1),
		dkdomain.StoredBillingRequestID("01J8ZK7Q9X2M4N6P8R0T2V4W6Y", 9999, 99),
	} {
		if !strings.HasPrefix(id, prefix) {
			t.Fatalf("落进 usage_logs 的 %q 不以 %q 开头，本月花费会算成 0", id, prefix)
		}
	}

	// LIKE 的元字符只有 % 和 _。前缀里出现任何一个都得加 ESCAPE，否则匹配范围会悄悄变大。
	if strings.ContainsAny(prefix, "%_") {
		t.Fatalf("前缀 %q 含 LIKE 元字符，必须改用 ESCAPE 或换成 starts_with", prefix)
	}
}

// ---- 结算 ----

func TestSettleSourceStatusesAllowsRerun(t *testing.T) {
	// 结算 worker 重跑一轮时不能因为「状态已经是终态」就卡死，
	// 真正防重复的是 settled_at IS NULL 那个守卫。
	for _, to := range []dkdomain.JobStatus{
		dkdomain.JobStatusSucceeded,
		dkdomain.JobStatusPartiallyFailed,
		dkdomain.JobStatusCancelled,
		dkdomain.JobStatusFailed,
	} {
		got := settleSourceStatuses(to)
		if !containsString(got, to.String()) {
			t.Fatalf("结算到 %s 时来源白名单里必须有它自己，实际 %v", to, got)
		}
	}
	if !containsString(settleSourceStatuses(dkdomain.JobStatusSucceeded), "settling") {
		t.Fatal("结算必须能从 settling 落终态")
	}
}
