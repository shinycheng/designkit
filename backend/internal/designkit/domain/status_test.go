//go:build unit

package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCanTransition_AllowedEdges 把设计定型 2.5 那张图上的每一条边都断言一遍。
// 少一条边，某个流程就会在运行时被白名单挡住，任务卡在中间状态不动。
func TestCanTransition_AllowedEdges(t *testing.T) {
	allowed := []struct {
		from JobStatus
		to   JobStatus
		why  string
	}{
		{JobStatusCreated, JobStatusHolding, "冻结额度成功"},
		{JobStatusHolding, JobStatusRunning, "worker 开始领取"},
		{JobStatusRunning, JobStatusSettling, "所有 item 到终态，进入对账"},
		{JobStatusSettling, JobStatusSucceeded, "全部成功"},
		{JobStatusSettling, JobStatusPartiallyFailed, "有成功有失败"},
		{JobStatusSettling, JobStatusFailed, "一张都没成功"},
		{JobStatusSettling, JobStatusCancelled, "停止排队后在途清空，走同一条结算路径"},
		{JobStatusSucceeded, JobStatusRunning, "重试：仅 retry 可触发，同时 revision+1"},
		{JobStatusPartiallyFailed, JobStatusRunning, "重试：仅 retry 可触发，同时 revision+1"},
	}

	for _, c := range allowed {
		require.Truef(t, CanTransition(c.from, c.to),
			"%s → %s 必须允许（%s）", c.from, c.to, c.why)
	}
}

// TestCanTransition_AnyToFailed 「任意 → failed」：冻结失败、僵尸回收都靠它。
// 进程随时可能被杀，任何一个状态上都可能留下僵尸，所以不能只对某几个状态开。
func TestCanTransition_AnyToFailed(t *testing.T) {
	for _, from := range AllJobStatuses() {
		require.Truef(t, CanTransition(from, JobStatusFailed),
			"%s → failed 必须允许（僵尸回收要能在任何状态上判死）", from)
	}
}

// TestCanTransition_ForbiddenEdges 典型的非法边。
//
// 第一条是最贵的：**running 且存在在途 item → cancelled 绝对禁止。**
// 直接跳过 settling 就等于跳过结算：在途的图照样出、上游照样扣钱，
// 我们的 actual_cost 永远是空的 —— usage_logs 里有扣款、我们这边显示已取消，
// 凭空多出无归属扣款；ERP 看到 cancelled 会以为什么都没发生，直接重新下单再花一遍钱。
func TestCanTransition_ForbiddenEdges(t *testing.T) {
	forbidden := []struct {
		from JobStatus
		to   JobStatus
		why  string
	}{
		{JobStatusRunning, JobStatusCancelled, "必须先 settling 把在途的账结清"},
		{JobStatusCreated, JobStatusRunning, "跳过 holding = 没冻结就开跑，会透支"},
		{JobStatusCreated, JobStatusSettling, "跳级"},
		{JobStatusHolding, JobStatusSettling, "跳过 running"},
		{JobStatusHolding, JobStatusSucceeded, "一张都没跑就算成功"},
		{JobStatusRunning, JobStatusSucceeded, "跳过 settling，上游 usage_logs 是异步写的，立刻汇总会算成 0"},
		{JobStatusRunning, JobStatusPartiallyFailed, "同上"},
		{JobStatusSettling, JobStatusRunning, "结算中不许倒回去跑；要重跑得先落终态再走 retry"},
		{JobStatusCancelled, JobStatusRunning, "只有 succeeded / partially_failed 能被 retry 拉回"},
		{JobStatusFailed, JobStatusRunning, "同上"},
		{JobStatusSucceeded, JobStatusSettling, "retry 的目标是 running 不是 settling"},
		{JobStatusSucceeded, JobStatusCancelled, "跨终态跳变"},
		{JobStatusCancelled, JobStatusSucceeded, "跨终态跳变"},
	}

	for _, c := range forbidden {
		require.Falsef(t, CanTransition(c.from, c.to),
			"%s → %s 必须禁止（%s）", c.from, c.to, c.why)
	}
}

// TestCanTransition_RejectsUnknownStatus 非法取值一律 false，不要靠调用方保证。
func TestCanTransition_RejectsUnknownStatus(t *testing.T) {
	require.False(t, CanTransition("", JobStatusRunning))
	require.False(t, CanTransition(JobStatusRunning, ""))
	require.False(t, CanTransition("pending", JobStatusRunning), "pending 是 item 的状态，不是 job 的")
	require.False(t, CanTransition(JobStatusRunning, "done"))
}

// TestCanTransition_NoSelfLoopExceptFailed 除 failed 外不允许自迁移。
// 自迁移一旦放开，「带 WHERE status=<from> 守卫的 UPDATE」就失去意义了。
func TestCanTransition_NoSelfLoopExceptFailed(t *testing.T) {
	for _, s := range AllJobStatuses() {
		if s == JobStatusFailed {
			require.True(t, CanTransition(s, s), "failed → failed 允许（并发判死时的无害空转）")
			continue
		}
		require.Falsef(t, CanTransition(s, s), "%s → %s 自迁移必须禁止", s, s)
	}
}

func TestJobStatus_IsTerminal(t *testing.T) {
	terminal := map[JobStatus]bool{
		JobStatusCreated:         false,
		JobStatusHolding:         false,
		JobStatusRunning:         false,
		JobStatusSettling:        false,
		JobStatusSucceeded:       true,
		JobStatusPartiallyFailed: true,
		JobStatusFailed:          true,
		JobStatusCancelled:       true,
	}
	for s, want := range terminal {
		require.Equalf(t, want, s.IsTerminal(), "%s.IsTerminal()", s)
	}
}

// TestCanTransitionItem_AllowedEdges 单张图的每一条允许边。
func TestCanTransitionItem_AllowedEdges(t *testing.T) {
	allowed := []struct {
		from ItemStatus
		to   ItemStatus
		why  string
	}{
		{ItemStatusPending, ItemStatusRunning, "worker 领取"},
		{ItemStatusPending, ItemStatusCancelled, "停止排队：还没开始就砍掉"},
		{ItemStatusPending, ItemStatusFailed, "整批被判死 / 尝试次数已用尽"},
		{ItemStatusRunning, ItemStatusSucceeded, "出图成功"},
		{ItemStatusRunning, ItemStatusFailed, "出图失败且没有剩余尝试次数"},
		{ItemStatusRunning, ItemStatusPending, "失败但还能再试：available_at 往后推，退避重试"},
		{ItemStatusRunning, ItemStatusRunning, "僵尸回收：租约过期后被另一个 worker 重新领走"},
		{ItemStatusSucceeded, ItemStatusPending, "运营点重试"},
		{ItemStatusFailed, ItemStatusPending, "运营点重试"},
		{ItemStatusCancelled, ItemStatusPending, "停掉之后又想把这几张补出来"},
	}
	for _, c := range allowed {
		require.Truef(t, CanTransitionItem(c.from, c.to),
			"item %s → %s 必须允许（%s）", c.from, c.to, c.why)
	}
}

// TestCanTransitionItem_ForbiddenEdges 单张图的典型非法边。
func TestCanTransitionItem_ForbiddenEdges(t *testing.T) {
	forbidden := []struct {
		from ItemStatus
		to   ItemStatus
		why  string
	}{
		{ItemStatusRunning, ItemStatusCancelled, "已经在跑的停不下来（上游跟浏览器连接是故意脱钩的），必须等它自然落地"},
		{ItemStatusPending, ItemStatusSucceeded, "没跑过不可能成功"},
		{ItemStatusSucceeded, ItemStatusFailed, "已经出图并扣过钱，改成 failed 会让 billed_cost 变成无主的钱"},
		{ItemStatusSucceeded, ItemStatusCancelled, "同上"},
		{ItemStatusFailed, ItemStatusSucceeded, "跨终态跳变"},
		{ItemStatusCancelled, ItemStatusSucceeded, "跨终态跳变"},
		{ItemStatusCancelled, ItemStatusFailed, "跨终态跳变"},
		{ItemStatusPending, ItemStatusPending, "自迁移会让带 WHERE status 守卫的 UPDATE 失去意义"},
		{ItemStatusSucceeded, ItemStatusRunning, "重试的目标是 pending 不是 running：领取 SQL 只认 pending"},
		{ItemStatusFailed, ItemStatusRunning, "同上"},
	}
	for _, c := range forbidden {
		require.Falsef(t, CanTransitionItem(c.from, c.to),
			"item %s → %s 必须禁止（%s）", c.from, c.to, c.why)
	}
}

func TestCanTransitionItem_RejectsUnknownStatus(t *testing.T) {
	require.False(t, CanTransitionItem("", ItemStatusRunning))
	require.False(t, CanTransitionItem(ItemStatusRunning, ""))
	require.False(t, CanTransitionItem("holding", ItemStatusRunning), "holding 是 job 的状态，不是 item 的")
}

// TestCanTransitionItem_NoAnyToFailed item 没有「任意 → failed」的兜底。
func TestCanTransitionItem_NoAnyToFailed(t *testing.T) {
	require.False(t, CanTransitionItem(ItemStatusSucceeded, ItemStatusFailed))
	require.False(t, CanTransitionItem(ItemStatusFailed, ItemStatusFailed))
}

func TestTerminalJobStatus(t *testing.T) {
	cases := []struct {
		name          string
		itemCount     int
		success       int
		fail          int
		cancelled     int
		stopRequested bool
		want          JobStatus
	}{
		{"全成功", 5, 5, 0, 0, false, JobStatusSucceeded},
		{"部分成功", 5, 3, 2, 0, false, JobStatusPartiallyFailed},
		{"全失败", 5, 0, 5, 0, false, JobStatusFailed},
		{"停止排队后全被砍", 5, 0, 0, 5, true, JobStatusCancelled},
		{"停止排队但有几张已跑完", 5, 2, 0, 3, true, JobStatusCancelled},
		{"停止排队时在途已清空、一张都没砍到 → 按实际结果算", 5, 5, 0, 0, true, JobStatusSucceeded},
		{"没点停止但有 cancelled（整批判死的兜底路径）", 5, 0, 0, 5, false, JobStatusCancelled},
		{"既有失败又有取消", 5, 0, 2, 3, false, JobStatusFailed},
		{"item_count 为 0 的畸形数据", 0, 0, 0, 0, false, JobStatusFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TerminalJobStatus(c.itemCount, c.success, c.fail, c.cancelled, c.stopRequested)
			require.Equal(t, c.want, got)
			require.True(t, got.IsTerminal(), "TerminalJobStatus 必须返回终态")
		})
	}
}

// TestTerminalJobStatus_ReachableFromSettling 终态必须都能从 settling 迁过去，
// 否则结算 worker 会算出一个白名单不让它写的状态，任务永远卡在 settling。
func TestTerminalJobStatus_ReachableFromSettling(t *testing.T) {
	for _, s := range AllJobStatuses() {
		if !s.IsTerminal() {
			continue
		}
		require.Truef(t, CanTransition(JobStatusSettling, s),
			"settling → %s 必须允许，否则结算 worker 写不进去", s)
	}
}
