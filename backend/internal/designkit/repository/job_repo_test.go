//go:build unit

package repository

// 领取待办那条 SQL 的第二道守卫。
//
// 这个文件只放「错一次就是钱」的断言。辅助函数 mustContain / mustNotContain
// 复用 query_build_test.go 里的那两个（同一个包），不要再定义一遍。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// TestClaimItemSQLGuardsAttemptBudgetOnPendingBranch
// pending 那一支必须带 `attempt_count < max_attempts`。
//
// 这是防「把试满的 item 退回 pending」这类 bug 的第二道防线：
// 第一道在 worker（领到 item 第一件事判上限，超了不调网关）。
// 两道加起来才叫「有人看着」—— 原来是 worker 不看、SQL 也不看。
func TestClaimItemSQLGuardsAttemptBudgetOnPendingBranch(t *testing.T) {
	sqlText := buildClaimItemSQL(true)

	mustContain(t, sqlText,
		"(status = 'pending' AND available_at <= NOW() AND attempt_count < max_attempts)")
}

// TestClaimItemSQLNeverGuardsAttemptOnZombieBranch
// 僵尸回收那一支**绝对不能**带 attempt 条件。
//
// 一张 attempt_count 已经等于 max_attempts、正在跑的图，进程被 SIGKILL 之后
// 就永远停在 running。僵尸支要是也判 attempt，它**再也领不出来 = 再也判不了死**：
// 批次的 success+fail+cancelled 永远凑不满 item_count，收尾守卫命中不了，
// 冻结额永远占着用户余额（运营那边表现为「明明没任务却提示余额不足」）。
//
// 正确做法是照领不误，由 worker 领到手判上限、不调网关直接判 failed。
func TestClaimItemSQLNeverGuardsAttemptOnZombieBranch(t *testing.T) {
	for _, useDBNow := range []bool{true, false} {
		sqlText := buildClaimItemSQL(useDBNow)
		now := "NOW()"
		if !useDBNow {
			now = "$3::timestamptz"
		}
		zombie := "OR (status = 'running' AND lease_expires_at < " + now + ")"
		if !strings.Contains(sqlText, zombie) {
			t.Fatalf("僵尸回收那一支被改了（它必须保持无条件）:\n%s", sqlText)
		}
	}
}

// TestClaimItemSQLKeepsConcurrencyGuards 并发与顺序的三条保证，一条都不能少。
//
//	FOR UPDATE SKIP LOCKED → 多个 worker 抢同一批时互相跳过，同一张图只有一个人拿到；
//	                         少了它，同一张图会被两个 worker 各出一遍、上游收两次钱
//	ORDER BY job_id, seq   → 先来的批次先跑、批内按序号跑（对外契约「第 N 张」）
//	attempt_count + 1      → 在领取那一刻就加，不是成功后才加；
//	                         少了它，进程崩一次这张图会被永远重试下去
func TestClaimItemSQLKeepsConcurrencyGuards(t *testing.T) {
	sqlText := buildClaimItemSQL(true)
	mustContain(t, sqlText,
		"FOR UPDATE SKIP LOCKED",
		"LIMIT 1",
		"ORDER BY job_id, seq",
		"attempt_count = attempt_count + 1",
	)
}

// TestCompleteItemWriteBackGuardIsThreeConditions 写回守卫的三个条件。
//
// 三条缺任何一条，租约被抢走之后迟到的那份结果都会覆盖掉新 worker 的结果 ——
// 两次出图的结果混在一行上，success_count 还会被加两次。
// 影响行数不等于 1 时必须**丢弃结果**（调用方拿到 ErrLeaseLost 就返回，不重写）。
func TestCompleteItemWriteBackGuardIsThreeConditions(t *testing.T) {
	if completeItemGuardSuffix != ` WHERE id = $1 AND worker_id = $2 AND status = 'running'` {
		t.Fatalf("写回守卫被改了: %q", completeItemGuardSuffix)
	}
}

// ============================================================================
// 巡检：三段各自守什么
// ============================================================================

// TestReapStaleJobIDsSQLNeverTouchesSettling
// 心跳那一段的状态清单里**绝不能**出现 settling。
//
// 进了 settling 就没有在途 item 了，心跳本来就不会再续 ——
// 拿 heartbeat_at 判它等于必然误杀，而且会把一批图已经全出好的任务标成 failed，
// 运营照着 DK_STALE 的「可以重新提交」再花一遍钱。
// settling 走 reapStuckSettlingSQL，判据是 finished_at、阈值长得多。
func TestReapStaleJobIDsSQLNeverTouchesSettling(t *testing.T) {
	mustContain(t, reapStaleJobIDsSQL,
		"status IN ('created', 'holding', 'running')",
		"COALESCE(heartbeat_at, created_at)",
		"FOR UPDATE SKIP LOCKED",
	)
	mustNotContain(t, reapStaleJobIDsSQL, "settling")
}

// TestReapStuckSettlingSQLReleasesMoneyWithoutTheWorker
// 卡在 settling 的批次必须能在**结算 worker 根本没起来**的情况下被收掉。
//
// FinalizeJob 一律把批次写成 settling，而把它推到终态、释放冻结额的只有结算 worker；
// worker 缺对象存储 / 图像服务 / 出图网关任意一个就整个不启动。
// 少了这一段，那几笔冻结额永远占着用户可用额，没有任何报错，
// 运营看到的是「明明没任务却提示余额不足」。
func TestReapStuckSettlingSQLReleasesMoneyWithoutTheWorker(t *testing.T) {
	mustContain(t, reapStuckSettlingSQL,
		"WHERE status = 'settling'",
		"AND settled_at IS NULL",
		"COALESCE(finished_at, updated_at, created_at) < NOW() - make_interval(secs => $1)",
		"FOR UPDATE SKIP LOCKED",
		// 封账：不填 actual_cost 就是「usage_logs 里有扣款、我们这边为空」＝无归属扣款
		"actual_cost = COALESCE(actual_cost, (",
		"SELECT COALESCE(SUM(i.billed_cost), 0)",
		"settled_at = COALESCE(settled_at, NOW())",
	)
	// **不许一律写 failed**：这批的图很可能全出好了、钱也扣了。
	mustNotContain(t, reapStuckSettlingSQL, "status = 'failed',")
	// 批次级错误码不写：所有 item 都到终态了，失败原因在 item 那一行上；
	// 出问题的是结算不是出图，写「系统重启中断了」是假的。
	mustNotContain(t, reapStuckSettlingSQL, "last_error_code")
}

// TestReapStuckSettlingCaseMirrorsTerminalJobStatus
// 那条 CASE 是 domain.TerminalJobStatus 的逐条镜像，**两边必须同时改**。
//
// 这里做两件事：
//  1. 断言 SQL 里五个分支的文本还在（顺序敏感，第一条命中即返回）；
//  2. 用一份「照抄 SQL 的 Go 版本」跟 TerminalJobStatus 逐格对，
//     谁动了 TerminalJobStatus 而没动 SQL，这条会当场炸。
//
// 判错的代价：把一批已经出好图的任务标成 failed，运营重新提交、再花一遍钱；
// 或者反过来把一批全失败的标成 succeeded，运营去作品库找一张不存在的图。
func TestReapStuckSettlingCaseMirrorsTerminalJobStatus(t *testing.T) {
	// SQL 里那几行是对齐过的（THEN 前面有多个空格），断言前先把空白压平，
	// 免得以后有人重排一下缩进就把这条测试弄红。
	normalized := strings.Join(strings.Fields(reapStuckSettlingSQL), " ")
	mustContain(t, normalized,
		"WHEN cancel_requested_at IS NOT NULL AND cancelled_count > 0 THEN 'cancelled'",
		"WHEN item_count > 0 AND success_count = item_count THEN 'succeeded'",
		"WHEN success_count > 0 THEN 'partially_failed'",
		"WHEN cancelled_count > 0 AND fail_count = 0 THEN 'cancelled'",
		"ELSE 'failed' END",
	)

	// 逐条照抄上面那个 CASE（顺序一致），下面拿它跟 domain 的实现对。
	sqlCase := func(itemCount, success, fail, cancelled int, stopRequested bool) string {
		switch {
		case stopRequested && cancelled > 0:
			return "cancelled"
		case itemCount > 0 && success == itemCount:
			return "succeeded"
		case success > 0:
			return "partially_failed"
		case cancelled > 0 && fail == 0:
			return "cancelled"
		default:
			return "failed"
		}
	}

	const itemCount = 3
	for success := 0; success <= itemCount; success++ {
		for fail := 0; fail+success <= itemCount; fail++ {
			cancelled := itemCount - success - fail
			for _, stopRequested := range []bool{false, true} {
				want := dkdomain.TerminalJobStatus(itemCount, success, fail, cancelled, stopRequested).String()
				got := sqlCase(itemCount, success, fail, cancelled, stopRequested)
				if want != got {
					t.Fatalf("SQL 的 CASE 跟 domain.TerminalJobStatus 对不上"+
						"（item=%d success=%d fail=%d cancelled=%d stop=%t）：SQL=%s domain=%s",
						itemCount, success, fail, cancelled, stopRequested, got, want)
				}
			}
		}
	}
}

// TestStaleSettlingIsLongerThanTheHeartbeatWindow
// settling 那一档必须明显长于心跳那一档。
//
// 结算 worker 只要活着就该先动手（worker/settlement.go 里 30 分钟就强制结算，
// 而且会按已经查到的账单把 actual_cost 填准）。巡检这一档是「worker 根本没起来」
// 时的最后一道，抢在它前面只会把本来能对准的账变成粗算。
func TestStaleSettlingIsLongerThanTheHeartbeatWindow(t *testing.T) {
	heartbeatWindow := time.Duration(dkdomain.DefaultStaleJobMinutes) * time.Minute
	if staleSettlingAfter <= heartbeatWindow {
		t.Fatalf("settling 兜底阈值（%s）必须长于心跳阈值（%s）", staleSettlingAfter, heartbeatWindow)
	}
	// worker/settlement.go 的 settleStaleAfter 是 30 分钟（那个常量不导出，
	// 这里只能钉一个下限，改动那边时请一起看这条）。
	if staleSettlingAfter < 60*time.Minute {
		t.Fatalf("settling 兜底阈值（%s）必须 >= 60 分钟，给结算 worker 的 30 分钟强制结算留出余量",
			staleSettlingAfter)
	}
}

// TestDeadPendingItemsSQLCannotKillAFreshItem
// 「pending 但没次数了」这条巡检**绝不能**误伤一张还没跑过的图。
//
// max_attempts 要是被写成 0 或负数，光看 `attempt_count >= max_attempts`
// 会把一整批刚提交、attempt_count 还是 0 的新任务全判死 ——
// 运营点了提交，一张图没出，全部显示「已经重试到上限」。
// 所以 `attempt_count > 0` 是硬保险，兜底值跟 worker 的 normalizeMaxAttempts 一致。
func TestDeadPendingItemsSQLCannotKillAFreshItem(t *testing.T) {
	mustContain(t, deadPendingItemsSQL,
		"WHERE status = 'pending'",
		"AND attempt_count > 0",
		fmt.Sprintf("AND attempt_count >= CASE WHEN max_attempts > 0 THEN max_attempts ELSE %d END",
			dkdomain.DefaultMaxAttempts),
		"FOR UPDATE SKIP LOCKED",
		// 判死要落终态并记到 fail_count 上，否则三个计数永远凑不满 item_count
		"status = 'failed'",
		"fail_count = j.fail_count + agg.dead",
	)
	// 这条巡检拆的是死结，不是「顺手把整批毙掉」：只动 item，绝不改 job.status。
	mustNotContain(t, deadPendingItemsSQL, "cancelled_count")
}

// TestDeadPendingItemsPredicateIsTheComplementOfTheClaimGuard
// 这条巡检的判据必须正好是领取 SQL 那条 pending 守卫的补集 ——
// 多一点会误杀还能跑的图，少一点就漏掉死结、冻结额继续挂着。
func TestDeadPendingItemsPredicateIsTheComplementOfTheClaimGuard(t *testing.T) {
	// 领取：pending 且 attempt_count <  max_attempts
	mustContain(t, buildClaimItemSQL(true), "AND attempt_count < max_attempts)")
	// 巡检：pending 且 attempt_count >= max_attempts
	mustContain(t, deadPendingItemsSQL, "AND attempt_count >= CASE WHEN max_attempts > 0")
}

// TestFinalizeAfterSweepUsesTheSameGuardAsFinalizeJob
// 扫完之后判收尾，守卫必须跟 FinalizeJob 一模一样。
//
// 松一格 → 两条路径都以为自己是最后一张，结算和回调各跑一遍；
// 紧一格 → 这一批永远不收尾，冻结额永远挂着。
//
// 而且它必须是**独立的一条语句**：PostgreSQL 里数据修改型 CTE 全部看同一个快照，
// 跟 deadPendingItemsSQL 揉进同一条语句的话，读到的 fail_count 还是加之前的值，
// 三个计数永远凑不满 item_count。
func TestFinalizeAfterSweepUsesTheSameGuardAsFinalizeJob(t *testing.T) {
	mustContain(t, finalizeAfterSweepSQL,
		"status = 'settling'",
		"AND finished_at IS NULL",
		"AND success_count + fail_count + cancelled_count = item_count",
		"AND status = ANY($2)",
	)
	// 来源状态清单跟 FinalizeJob 共用同一个函数，不许在 SQL 里手写第二份。
	sources := finalizeSourceStatuses()
	for _, want := range []string{"running", "created", "holding"} {
		if !containsString(sources, want) {
			t.Fatalf("finalizeSourceStatuses 少了 %q：%v", want, sources)
		}
	}
	if containsString(sources, "settling") {
		t.Fatalf("settling 不该在收尾来源里，会让同一批被推两次：%v", sources)
	}
}
