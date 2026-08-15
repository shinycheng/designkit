//go:build unit

package worker

import (
	"context"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/require"
)

// settlingFixture 造一个「所有 item 都成功了、批次已转 settling、等着对账」的场景。
//
// attempt_count 一律置 1：真实的 item 在**被领取那一刻**就 +1，
// 所以任何跑过的图 attempt_count 至少是 1。结算靠它枚举 request_id。
func settlingFixture(t *testing.T, itemCount int) *fixture {
	f := newFixture(t, itemCount, nil)

	f.repo.mu.Lock()
	defer f.repo.mu.Unlock()

	job := f.repo.jobs[1]
	job.Status = dkdomain.JobStatusSettling
	job.SuccessCount = itemCount
	finished := f.clock.Now()
	job.FinishedAt = &finished

	for i := 1; i <= itemCount; i++ {
		item := f.repo.items[int64(i)]
		item.Status = dkdomain.ItemStatusSucceeded
		item.AttemptCount = 1
		requestID := dkdomain.StoredBillingRequestID(testJobUID, i, 1)
		item.BillingRequestID = &requestID
	}
	f.repo.settlingQueue = []*dkdomain.Job{job}
	return f
}

// putUsageLog 往假的 usage_logs 里塞一条账单（第 seq 张的第 attempt 次尝试）。
func (f *fixture) putUsageLog(seq, attempt int, amount string) {
	money, err := dkdomain.MoneyFromString(amount)
	if err != nil {
		panic(err)
	}
	f.repo.mu.Lock()
	f.repo.usageLogs[dkdomain.StoredBillingRequestID(testJobUID, seq, attempt)] = money
	f.repo.mu.Unlock()
}

// TestSettlement_WaitsUntilUsageLogsAreWritten 账单没写完就退避重试，不许提前结算。
//
// 这是 settling 状态存在的**唯一理由**：上游写 usage_logs 是异步的
// （丢进 worker 池就返回），最后一张图返回的那一刻立刻汇总，很可能少算甚至全算 0。
func TestSettlement_WaitsUntilUsageLogsAreWritten(t *testing.T) {
	f := settlingFixture(t, 3)
	ctx := context.Background()

	// 第一轮：一条账单都还没写进来。
	f.pool.settler.run(ctx)
	require.Empty(t, f.repo.settled, "账单没写完就结算 = 少算钱")
	require.Equal(t, dkdomain.JobStatusSettling, f.repo.job(1).Status)

	// 第二轮：只写进来 2 条，还是不能结算。
	f.putUsageLog(1, 1, "0.02")
	f.putUsageLog(2, 1, "0.04")
	f.clock.Advance(time.Minute)
	f.pool.settler.run(ctx)
	require.Empty(t, f.repo.settled, "命中条数 < 成功张数就得继续等")

	// 第三轮：齐了。
	f.putUsageLog(3, 1, "0.08")
	f.clock.Advance(time.Minute)
	f.pool.settler.run(ctx)

	require.Len(t, f.repo.settled, 1)
	settled := f.repo.settled[0]
	require.Equal(t, "0.14", dkdomain.MoneyString(settled.ActualCost),
		"actual_cost = SUM(items.billed_cost)，**不是**成功张数 × 单价")
	require.Equal(t, dkdomain.JobStatusSucceeded, settled.Status)
	require.False(t, settled.SettledAt.IsZero())
	require.Len(t, f.repo.billed, 3, "每一张都要回填 billed_cost 并按张递减冻结额")
}

// TestSettlement_ActualCostAddsMixedTiers 同一批可能落不同计费档，
// 单价不一样，所以只能逐张加。
func TestSettlement_ActualCostAddsMixedTiers(t *testing.T) {
	f := settlingFixture(t, 3)

	// 三张分别落 1K / 2K / 4K：单价差很远。
	f.putUsageLog(1, 1, "0.011")
	f.putUsageLog(2, 1, "0.042")
	f.putUsageLog(3, 1, "0.167")

	f.pool.settler.run(context.Background())

	require.Len(t, f.repo.settled, 1)
	require.Equal(t, "0.22", dkdomain.MoneyString(f.repo.settled[0].ActualCost))

	byItem := map[int64]string{}
	for _, applied := range f.repo.billed {
		byItem[applied.itemID] = dkdomain.MoneyString(applied.cost)
	}
	require.Equal(t, map[int64]string{1: "0.011", 2: "0.042", 3: "0.167"}, byItem)
}

// TestSettlement_CountsEveryAttemptOfARetriedItem 重试过的那一张，
// actual_cost 必须是**两次 attempt 之和**。
//
// 原来错在哪：job_items.billing_request_id 只存最新一次 attempt 的值，
// 结算只拿这一个 id 去反查，于是第一次 attempt 真花掉的钱既不进 actual_cost、
// 也不从冻结额里减 —— 每一张重试过的图都在系统性少算。
// 设计定型 6.2 写死「actual_cost = 所有 attempt 合计」。
func TestSettlement_CountsEveryAttemptOfARetriedItem(t *testing.T) {
	f := settlingFixture(t, 1)

	// 第 1 张重试过一次：第 1 次 attempt 失败（钱照扣），第 2 次成功。
	f.repo.mu.Lock()
	item := f.repo.items[1]
	item.AttemptCount = 2
	latest := dkdomain.StoredBillingRequestID(testJobUID, 1, 2)
	item.BillingRequestID = &latest
	f.repo.mu.Unlock()

	f.putUsageLog(1, 1, "0.042") // 第一次尝试，上游照样扣了钱
	f.putUsageLog(1, 2, "0.042") // 重试那一次

	f.pool.settler.run(context.Background())

	require.Len(t, f.repo.settled, 1)
	require.Equal(t, "0.084", dkdomain.MoneyString(f.repo.settled[0].ActualCost),
		"重试一张 = 重新出一张图 = 重新收一次钱，两次的钱都得算")
	require.Len(t, f.repo.billed, 1)
	require.Equal(t, "0.084", dkdomain.MoneyString(f.repo.billed[0].cost),
		"billed_cost 记的是这一张所有 attempt 的合计")
}

// TestSettlement_CountsFailedItemsThatWereStillCharged 失败但上游已经扣过钱的图
// 也必须入账。
//
// 原来错在哪：结算只统计 Status == succeeded 的 item。而「上游已出图扣钱、
// 我们存图失败 / 写回失败 / 超时判死」这几种情况钱是真花了的
// —— 决策 15：网关跟浏览器连接是**故意**脱钩的，我们这边算不算成功
// 跟上游扣不扣钱毫无关系。判据是「上游有没有为它扣过钱」。
func TestSettlement_CountsFailedItemsThatWereStillCharged(t *testing.T) {
	f := settlingFixture(t, 2)

	f.repo.mu.Lock()
	job := f.repo.jobs[1]
	job.SuccessCount = 1
	job.FailCount = 1
	// 第 2 张：上游出了图、扣了钱，我们存图失败判死，request_id 一列都没来得及写。
	failed := f.repo.items[2]
	failed.Status = dkdomain.ItemStatusFailed
	failed.AttemptCount = 1
	failed.BillingRequestID = nil
	f.repo.mu.Unlock()

	f.putUsageLog(1, 1, "0.042")
	f.putUsageLog(2, 1, "0.042") // 失败那一张的钱

	f.pool.settler.run(context.Background())

	require.Len(t, f.repo.settled, 1)
	require.Equal(t, "0.084", dkdomain.MoneyString(f.repo.settled[0].ActualCost),
		"失败但已扣费的那一张不入账 = 凭空多出无归属扣款")
	require.Equal(t, dkdomain.JobStatusPartiallyFailed, f.repo.settled[0].Status)

	byItem := map[int64]string{}
	for _, applied := range f.repo.billed {
		byItem[applied.itemID] = dkdomain.MoneyString(applied.cost)
	}
	require.Equal(t, map[int64]string{1: "0.042", 2: "0.042"}, byItem,
		"失败的那一张也要回填 billed_cost，冻结额才会跟着减")
}

// TestSettlement_AllItemsFailedWithoutBillsSettlesAtZero 整批一张都没成功、
// 上游一次都没扣过钱：宽限一轮之后照样结算，金额是 0。
//
// 「宽限一轮」是给上游异步写账单留的窗口 —— 这种批次一条「必须命中」的账单都没有，
// 第一轮就直接结算的话，「上游扣了钱、我们判失败」那一类会被记成 0。
func TestSettlement_AllItemsFailedWithoutBillsSettlesAtZero(t *testing.T) {
	f := settlingFixture(t, 1)
	ctx := context.Background()

	f.repo.mu.Lock()
	job := f.repo.jobs[1]
	job.SuccessCount = 0
	job.FailCount = 1
	item := f.repo.items[1]
	item.Status = dkdomain.ItemStatusFailed
	item.AttemptCount = 1
	item.BillingRequestID = nil
	f.repo.mu.Unlock()

	f.pool.settler.run(ctx)
	require.Empty(t, f.repo.settled, "第一轮先宽限一轮，等上游把账单写完")

	f.clock.Advance(time.Minute)
	f.pool.settler.run(ctx)
	require.Len(t, f.repo.settled, 1, "宽限过一轮就不能再等了，否则冻结额一直占着")
	require.Equal(t, "0", dkdomain.MoneyString(f.repo.settled[0].ActualCost))
	require.Equal(t, dkdomain.JobStatusFailed, f.repo.settled[0].Status)
}

// TestSettlement_ForcedAfterMaxAttempts 退避到上限还等不到账单，也必须把账结掉。
//
// 让批次永远停在 settling 更糟：冻结额一直占着用户的可用额，
// 运营看到的是一个永远转圈的任务，而且没有任何地方会报错。
func TestSettlement_ForcedAfterMaxAttempts(t *testing.T) {
	f := settlingFixture(t, 2)
	ctx := context.Background()

	// 一条账单都不会出现。每轮只推进 1 分钟：别推到 settleStaleAfter 那一档，
	// 那一档走的是另一条（卡太久）路径，这个用例要验的是退避到上限。
	for i := 0; i < DefaultSettleMaxAttempts; i++ {
		f.pool.settler.run(ctx)
		require.Empty(t, f.repo.settled, "第 %d 轮不该结算", i+1)
		f.clock.Advance(time.Minute)
	}

	f.pool.settler.run(ctx)
	require.Len(t, f.repo.settled, 1, "退避 5 次之后必须强制结算，不能让任务永远卡住")
	require.Equal(t, "0", dkdomain.MoneyString(f.repo.settled[0].ActualCost))

	// 强制结算 = actual_cost 不可信，必须留一条 critical 让管理员回头核。
	// 冻结额这里是「释放 + 留痕」，不是「留着 open 等人工」——
	// 留 open 就等于再造一遍「明明没任务却提示余额不足」。
	require.Len(t, f.repo.alerts, 1, "强制结算必须留下一条可追查的记录")
	require.Equal(t, dkdomain.BillingAlertLevelCritical, f.repo.alerts[0].Level)
	require.Equal(t, int64(1), f.repo.alerts[0].JobID)
}

// TestSettlement_StuckInSettlingIsForceSettled 卡在 settling 太久的批次要被强制结清。
//
// 原来错在哪：僵尸巡检的状态白名单是 ('created','holding','running')，**不含 settling**，
// 而 FinalizeJob 一律把 job 写成 settling。结算这条路上任何一次卡住都没有兜底：
// hold 永远是 open、用户可用额被永久吃掉，而且没有任何报错 ——
// 正是「明明没任务却提示余额不足」。
func TestSettlement_StuckInSettlingIsForceSettled(t *testing.T) {
	f := settlingFixture(t, 1)
	ctx := context.Background()

	// 账单永远等不到，而且这个批次已经在 settling 里泡了很久。
	f.clock.Advance(settleStaleAfter + time.Minute)

	f.pool.settler.run(ctx)

	require.Len(t, f.repo.settled, 1,
		"卡过 %s 就不能再等退避了，冻结额得放出来", settleStaleAfter)
	require.Len(t, f.repo.alerts, 1, "强制结清必须留一条 critical")
	require.Equal(t, dkdomain.BillingAlertLevelCritical, f.repo.alerts[0].Level)
}

// TestStuckInSettling 卡住的判据：以 finished_at 为准，没有才退回 updated_at。
// **不能用 heartbeat_at** —— 进了 settling 就没有在途 item 了，心跳本来就不会再续。
func TestStuckInSettling(t *testing.T) {
	f := newFixture(t, 1, nil)
	now := f.clock.Now()

	fresh := now.Add(-time.Minute)
	old := now.Add(-settleStaleAfter - time.Second)

	require.False(t, f.pool.settler.stuckInSettling(&dkdomain.Job{FinishedAt: &fresh}, now))
	require.True(t, f.pool.settler.stuckInSettling(&dkdomain.Job{FinishedAt: &old}, now))
	require.True(t, f.pool.settler.stuckInSettling(&dkdomain.Job{UpdatedAt: old}, now),
		"finished_at 为空时退回 updated_at")
	require.False(t, f.pool.settler.stuckInSettling(&dkdomain.Job{}, now),
		"两个时间都没有就不判卡住，不然新建的批次会被误杀")
	require.False(t, f.pool.settler.stuckInSettling(nil, now))
}

// TestSettlement_DoesNotApplyBillingTwice 已经回填过同样金额的就不再回填。
//
// ApplyItemBilling 会**按张递减冻结额**，重复调用会把冻结额多减一次 ——
// 表现为用户的可用额凭空少一截，而且没有任何报错。
func TestSettlement_DoesNotApplyBillingTwice(t *testing.T) {
	f := settlingFixture(t, 1)
	f.putUsageLog(1, 1, "0.05")

	f.repo.mu.Lock()
	already := dkdomain.MoneyFromFloat(0.05)
	f.repo.items[1].BilledCost = &already
	f.repo.mu.Unlock()

	f.pool.settler.run(context.Background())

	require.Empty(t, f.repo.billed, "金额没变就不该再回填一次")
	require.Len(t, f.repo.settled, 1)
	require.Equal(t, "0.05", dkdomain.MoneyString(f.repo.settled[0].ActualCost),
		"已经回填过的钱仍然要算进 actual_cost")
}

// TestSettlement_NeverLowersBilledCost 反查回来的金额比已经回填的还小时，
// **不许**改写 —— 冻结额按「新值 - 旧值」递减，负差额会把冻结额加回去，
// 凭空放大用户的可用额，接着就能透支。
func TestSettlement_NeverLowersBilledCost(t *testing.T) {
	f := settlingFixture(t, 1)
	f.putUsageLog(1, 1, "0.01")

	f.repo.mu.Lock()
	bigger := dkdomain.MoneyFromFloat(0.05)
	f.repo.items[1].BilledCost = &bigger
	f.repo.mu.Unlock()

	f.pool.settler.run(context.Background())

	require.Empty(t, f.repo.billed, "金额变小时一条都不该写")
	require.Len(t, f.repo.settled, 1)
	require.Equal(t, "0.05", dkdomain.MoneyString(f.repo.settled[0].ActualCost),
		"保守起见按已经记下的那个（较大的）值算")
}

// TestApplyItemBilling_NegativeDeltaNeverInflatesHold
// **反查回来的金额比已经回填的小时，冻结额一个子儿都不许加回去。**
//
// 真 SQL 减冻结额用的是「新值 − 旧值」这个差额，负差额会让
// GREATEST(amount − 负数, 0) = amount + |差额| —— 冻结额被**放大**，
// 而可用额 = balance − SUM(open holds)，冻结额变大意味着可用额变小……
// 反过来说，只要这条守卫失效（比如有人把 billed_cost 改成覆盖语义），
// 一次账单回退就能把冻结额算错，接着整条透支防线就废了。
//
// 这条测的是**假实现本身**：fakes_test.go 开头承诺「刻意照着真实 SQL 的语义写」，
// 假实现漂了，上面那些用它的测试就都在自己骗自己。
func TestApplyItemBilling_NegativeDeltaNeverInflatesHold(t *testing.T) {
	f := settlingFixture(t, 1)
	ctx := context.Background()

	before := dkdomain.MoneyFromFloat(0.5)
	f.repo.mu.Lock()
	f.repo.holds[1] = before
	already := dkdomain.MoneyFromFloat(0.3)
	f.repo.items[1].BilledCost = &already
	f.repo.mu.Unlock()

	require.NoError(t, f.repo.ApplyItemBilling(ctx, 1, dkdomain.MoneyFromFloat(0.1), "per_request", "2K"))

	require.Empty(t, f.repo.billed, "金额变小时一条都不该写")
	require.Equal(t, dkdomain.MoneyString(already), dkdomain.MoneyString(*f.repo.item(1).BilledCost),
		"billed_cost 只增不减")
	require.Equal(t, dkdomain.MoneyString(before), dkdomain.MoneyString(f.repo.hold(1)),
		"负差额绝不能把冻结额加回去 —— 那是凭空放大可用额")

	// 相同金额重投一次：幂等，冻结额也不能再动。
	require.NoError(t, f.repo.ApplyItemBilling(ctx, 1, already, "per_request", "2K"))
	require.Empty(t, f.repo.billed)
	require.Equal(t, dkdomain.MoneyString(before), dkdomain.MoneyString(f.repo.hold(1)))
}

// TestApplyItemBilling_DecrementsHoldByTheDelta 金额变大时按**差额**减冻结额，
// 不是按新值全额减 —— 全额减会把同一笔钱扣两遍，用户的可用额凭空少一截。
func TestApplyItemBilling_DecrementsHoldByTheDelta(t *testing.T) {
	f := settlingFixture(t, 1)
	ctx := context.Background()

	f.repo.mu.Lock()
	f.repo.holds[1] = dkdomain.MoneyFromFloat(1)
	first := dkdomain.MoneyFromFloat(0.2)
	f.repo.items[1].BilledCost = &first
	f.repo.mu.Unlock()

	require.NoError(t, f.repo.ApplyItemBilling(ctx, 1, dkdomain.MoneyFromFloat(0.3), "per_request", "2K"))

	require.Len(t, f.repo.billed, 1)
	require.Equal(t, dkdomain.MoneyString(dkdomain.MoneyFromFloat(0.9)),
		dkdomain.MoneyString(f.repo.hold(1)), "只该减掉差额 0.1")

	// 减到 0 就停：实际花费高于冻结额是常态（计费档会向上漂），
	// amount 变成负数就会去放大**别的**任务的可用额。
	require.NoError(t, f.repo.ApplyItemBilling(ctx, 1, dkdomain.MoneyFromFloat(99), "per_request", "4K"))
	require.Equal(t, dkdomain.MoneyString(dkdomain.ZeroMoney), dkdomain.MoneyString(f.repo.hold(1)))
}

// TestSettlement_StopRequestedSettlesAsCancelled 「停止排队」的批次走的是
// **跟正常完成完全相同**的结算路径：终态记 cancelled，但 actual_cost 照实填。
//
// 不填的话，usage_logs 里有扣款、我们的 actual_cost 是空的 ——
// 凭空多出无归属扣款；ERP 只看 cancelled 还会误判成「什么都没发生」，直接重新下单。
func TestSettlement_StopRequestedSettlesAsCancelled(t *testing.T) {
	f := newFixture(t, 3, nil)
	ctx := context.Background()

	f.repo.mu.Lock()
	job := f.repo.jobs[1]
	job.Status = dkdomain.JobStatusSettling
	job.SuccessCount = 2
	job.CancelledCount = 1
	requested := f.clock.Now()
	job.CancelRequestedAt = &requested
	finished := f.clock.Now()
	job.FinishedAt = &finished
	for i := 1; i <= 2; i++ {
		item := f.repo.items[int64(i)]
		item.Status = dkdomain.ItemStatusSucceeded
		item.AttemptCount = 1
		requestID := dkdomain.StoredBillingRequestID(testJobUID, i, 1)
		item.BillingRequestID = &requestID
	}
	// 第 3 张压根没开始跑：attempt_count 还是 0，不会有任何 request_id。
	f.repo.items[3].Status = dkdomain.ItemStatusCancelled
	f.repo.settlingQueue = []*dkdomain.Job{job}
	f.repo.mu.Unlock()

	f.putUsageLog(1, 1, "0.02")
	f.putUsageLog(2, 1, "0.02")

	f.pool.settler.run(ctx)

	require.Len(t, f.repo.settled, 1)
	require.Equal(t, dkdomain.JobStatusCancelled, f.repo.settled[0].Status)
	require.Equal(t, "0.04", dkdomain.MoneyString(f.repo.settled[0].ActualCost),
		"已经跑掉的那两张照样扣了钱，必须如实记账")
}

// TestSettlement_AlertOnlyAfterSettleJobSucceeds 告警必须记在写库成功之后。
//
// 原来错在哪：告警排在 SettleJob 之前，SettleJob 失败会重来一轮，
// 而 designkit_billing_alerts 上没有任何唯一约束（9001 已定稿、不可改），
// 同一个批次会被插好几条，管理员后台看到的是一堆重复待办。
func TestSettlement_AlertOnlyAfterSettleJobSucceeds(t *testing.T) {
	f := settlingFixture(t, 1)
	ctx := context.Background()

	// 预估 0.5，实际 1.5 → 超 2 倍 → critical。
	f.putUsageLog(1, 1, "1.5")

	f.repo.mu.Lock()
	f.repo.settleJobErr = dkdomain.ErrConflict // 写库一直失败
	f.repo.mu.Unlock()

	for i := 0; i < 3; i++ {
		f.pool.settler.run(ctx)
		f.clock.Advance(time.Minute)
	}
	require.Empty(t, f.repo.alerts, "结算还没落库就不该有告警，否则每重试一轮多一条")

	f.repo.mu.Lock()
	f.repo.settleJobErr = nil
	f.repo.mu.Unlock()

	f.pool.settler.run(ctx)
	f.clock.Advance(time.Minute)
	f.pool.settler.run(ctx)

	require.Len(t, f.repo.settled, 1)
	require.Len(t, f.repo.alerts, 1, "结算成功之后有且只有一条告警")
	require.Equal(t, dkdomain.BillingAlertLevelCritical, f.repo.alerts[0].Level)
}

// TestSettlementAlertLevel 实际超预估只记告警，**绝不报错回滚**。
func TestSettlementAlertLevel(t *testing.T) {
	cases := []struct {
		name      string
		estimated string
		actual    string
		forced    bool
		wantLevel string
	}{
		{"没超就不记", "0.5", "0.55", false, ""},
		{"刚好 1.2 倍不记（要严格大于）", "0.5", "0.6", false, ""},
		{"超 1.2 倍记 warn", "0.5", "0.7", false, dkdomain.BillingAlertLevelWarn},
		{"超 2 倍记 critical", "0.5", "1.5", false, dkdomain.BillingAlertLevelCritical},
		{"预估是 0 时没有可比基准", "0", "1.5", false, ""},
		{"强制结算一律 critical", "0.5", "0.1", true, dkdomain.BillingAlertLevelCritical},
		{"预估是 0 也挡不住强制结算的告警", "0", "0", true, dkdomain.BillingAlertLevelCritical},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			estimated, err := dkdomain.MoneyFromString(tc.estimated)
			require.NoError(t, err)
			actual, err := dkdomain.MoneyFromString(tc.actual)
			require.NoError(t, err)

			job := &dkdomain.Job{ID: 1, UID: testJobUID, EstimatedCost: estimated}
			require.Equal(t, tc.wantLevel, settlementAlertLevel(job, actual, tc.forced))
		})
	}
}

// TestRecordBillingAlert 落库的那几个字段要对，而且预估为 0 时不许做除法
// （decimal 除 0 会 panic，整个结算 goroutine 当场炸掉）。
func TestRecordBillingAlert(t *testing.T) {
	f := newFixture(t, 1, nil)
	ctx := context.Background()

	f.pool.settler.recordBillingAlert(ctx, &dkdomain.Job{ID: 1, UID: testJobUID}, dkdomain.ZeroMoney, "", discardLogger())
	require.Empty(t, f.repo.alerts, "level 为空就什么都不做")

	estimated := dkdomain.MoneyFromFloat(0.5)
	actual := dkdomain.MoneyFromFloat(0.7)
	f.pool.settler.recordBillingAlert(ctx,
		&dkdomain.Job{ID: 1, UID: testJobUID, EstimatedCost: estimated}, actual,
		dkdomain.BillingAlertLevelWarn, discardLogger())

	require.Len(t, f.repo.alerts, 1)
	require.Equal(t, int64(1), f.repo.alerts[0].JobID)
	require.Equal(t, dkdomain.BillingAlertLevelWarn, f.repo.alerts[0].Level)
	require.True(t, f.repo.alerts[0].Actual.Equal(actual))
	require.True(t, f.repo.alerts[0].RatioOver.GreaterThan(dkdomain.MoneyFromInt(1)))

	// 预估 0：比例记 0，不能 panic。
	f.pool.settler.recordBillingAlert(ctx,
		&dkdomain.Job{ID: 2, UID: testJobUID}, actual,
		dkdomain.BillingAlertLevelCritical, discardLogger())
	require.Len(t, f.repo.alerts, 2)
	require.True(t, f.repo.alerts[1].RatioOver.IsZero())
}

// TestBillingCandidates request_id 的枚举规则：
// 每一次 attempt 一个，落库的那个值也带上，「必须命中」只认成功的那一次。
func TestBillingCandidates(t *testing.T) {
	stored := dkdomain.StoredBillingRequestID(testJobUID, 2, 3)

	t.Run("成功的图枚举全部 attempt", func(t *testing.T) {
		ids, required := billingCandidates(testJobUID, &dkdomain.JobItem{
			Seq: 2, AttemptCount: 3, Status: dkdomain.ItemStatusSucceeded,
			BillingRequestID: &stored,
		})
		require.Equal(t, []string{
			dkdomain.StoredBillingRequestID(testJobUID, 2, 1),
			dkdomain.StoredBillingRequestID(testJobUID, 2, 2),
			dkdomain.StoredBillingRequestID(testJobUID, 2, 3),
		}, ids, "重试过的每一次都花过钱，一个都不能漏")
		require.Equal(t, stored, required)
	})

	t.Run("失败的图照样枚举，但没有必须命中的", func(t *testing.T) {
		ids, required := billingCandidates(testJobUID, &dkdomain.JobItem{
			Seq: 2, AttemptCount: 2, Status: dkdomain.ItemStatusFailed,
		})
		require.Len(t, ids, 2, "失败的图上游也可能扣过钱，要去查")
		require.Empty(t, required,
			"失败的 attempt 有没有账单我们分不出来，拿它当必须命中会白等到退避上限")
	})

	t.Run("没跑过的图一个 id 都没有", func(t *testing.T) {
		ids, required := billingCandidates(testJobUID, &dkdomain.JobItem{
			Seq: 3, AttemptCount: 0, Status: dkdomain.ItemStatusCancelled,
		})
		require.Empty(t, ids)
		require.Empty(t, required)
	})

	t.Run("落库的值跟推导值不一样时两个都查", func(t *testing.T) {
		odd := "client:other:xxx"
		ids, required := billingCandidates(testJobUID, &dkdomain.JobItem{
			Seq: 1, AttemptCount: 1, Status: dkdomain.ItemStatusSucceeded,
			BillingRequestID: &odd,
		})
		require.Equal(t, []string{dkdomain.StoredBillingRequestID(testJobUID, 1, 1), odd}, ids)
		require.Equal(t, odd, required, "落库的那个才是权威值")
	})

	t.Run("attempt_count 被写坏时不会拼出天量 id", func(t *testing.T) {
		ids, _ := billingCandidates(testJobUID, &dkdomain.JobItem{
			Seq: 1, AttemptCount: 1_000_000, Status: dkdomain.ItemStatusFailed,
		})
		require.Len(t, ids, maxEnumeratedAttempts)
	})

	t.Run("nil 不炸", func(t *testing.T) {
		ids, required := billingCandidates(testJobUID, nil)
		require.Empty(t, ids)
		require.Empty(t, required)
	})
}

// TestSettleOnce_RespectsAdvisoryLock 抢不到咨询锁就整轮跳过。
//
// 两个副本同时结算同一个批次，按张递减冻结额的动作会跑两遍。
func TestSettleOnce_RespectsAdvisoryLock(t *testing.T) {
	locker := &fakeLocker{allow: false}
	f := settlingFixture(t, 1)
	f.pool.deps.Locker = locker
	f.putUsageLog(1, 1, "0.02")

	f.pool.settleOnce(context.Background())
	require.Equal(t, 0, f.repo.settleListCalls, "没拿到锁就一行都不该查")
	require.Empty(t, f.repo.settled)

	locker.allow = true
	f.pool.settleOnce(context.Background())
	require.Equal(t, 1, f.repo.settleListCalls)
	require.Len(t, f.repo.settled, 1)
	require.Equal(t, 1, locker.unlocked, "拿到的锁必须还回去")
}

// TestReapOnce 僵尸巡检：拿到锁才跑，跑完把回收到的批次记进日志。
func TestReapOnce(t *testing.T) {
	f := newFixture(t, 1, nil)
	f.repo.staleJobIDs = []int64{11, 12}

	locker := &fakeLocker{allow: false}
	f.pool.deps.Locker = locker
	f.pool.reapOnce(context.Background())
	require.Equal(t, 0, f.repo.reapCalls, "没拿到锁就不该扫")

	locker.allow = true
	f.pool.reapOnce(context.Background())
	require.Equal(t, 1, f.repo.reapCalls)
	require.Equal(t, 1, locker.granted)
	require.Equal(t, 1, locker.unlocked)
}

// TestWithAdvisoryLock_NilLockerStillRuns 没接锁时照常跑（单副本部署）。
func TestWithAdvisoryLock_NilLockerStillRuns(t *testing.T) {
	f := newFixture(t, 1, nil)
	f.pool.deps.Locker = nil

	ran := false
	f.pool.withAdvisoryLock(context.Background(), AdvisoryLockKeyReap, "reaper", func(context.Context) {
		ran = true
	})
	require.True(t, ran)
}

// TestSettleBackoff 退避是指数的、有上限的。
func TestSettleBackoff(t *testing.T) {
	base := 5 * time.Second
	require.Equal(t, 5*time.Second, settleBackoff(base, 1))
	require.Equal(t, 10*time.Second, settleBackoff(base, 2))
	require.Equal(t, 20*time.Second, settleBackoff(base, 3))
	require.Equal(t, 40*time.Second, settleBackoff(base, 4))
	require.Equal(t, 80*time.Second, settleBackoff(base, 5))
	require.Equal(t, DefaultSettleBackoffMax, settleBackoff(base, 50), "必须封顶，不能溢出成负数")
	require.Equal(t, DefaultSettleBackoffBase, settleBackoff(0, 0), "参数非法时退回默认值")
}
