package domain

// ============================================================================
// 状态机
// ============================================================================
//
// 任务状态和单张图状态**只能**通过这里的白名单函数迁移。
// 禁止在 repository / service 里随手写 `UPDATE ... SET status = 'xxx'`：
// 每一条 UPDATE 都要带 `WHERE status = <from>` 的守卫，并且先问一句
// CanTransition(from, to)。
//
// 为什么这么严：任务状态直接决定「钱怎么走」。
// 一次错误迁移（比如把还在跑的任务标成 cancelled）的后果是：上游照样出图照样扣钱，
// 我们这边显示已取消、actual_cost 为空，凭空多出无归属扣款；
// ERP 看到 cancelled 会以为什么都没发生，直接重新下单，**再花一遍钱**。

// JobStatus 是一次提交（一个批次）的状态。对应 designkit_jobs.status。
type JobStatus string

const (
	// JobStatusCreated 事务里刚 INSERT 出来，还没冻结额度。
	JobStatusCreated JobStatus = "created"
	// JobStatusHolding 已经在 designkit_holds 里占住可用额，等待 worker 开跑。
	JobStatusHolding JobStatus = "holding"
	// JobStatusRunning 至少有一张图在跑或在排队。
	JobStatusRunning JobStatus = "running"
	// JobStatusSettling 所有 item 都到终态了，正在按 billing_request_id
	// 回 usage_logs 对账。
	//
	// 这个状态必须存在：上游写 usage_logs 是**异步**的（丢进 worker 池就返回），
	// 最后一张图返回的那一刻立刻汇总，很可能少算甚至全算 0。
	JobStatusSettling JobStatus = "settling"
	// JobStatusSucceeded 全部成功。
	JobStatusSucceeded JobStatus = "succeeded"
	// JobStatusPartiallyFailed 有成功也有失败。
	JobStatusPartiallyFailed JobStatus = "partially_failed"
	// JobStatusFailed 一张都没成功，或者被巡检判定为僵尸（last_error_code=DK_STALE）。
	JobStatusFailed JobStatus = "failed"
	// JobStatusCancelled 运营点过「停止排队」，在途图跑完后落到这里。
	//
	// ⚠ cancelled **不代表没花钱**：已经在跑的那几张照样出图照样扣费，
	// actual_cost 照实填。ERP 文档写死：判断有没有图一律看 success_count，不看 status。
	JobStatusCancelled JobStatus = "cancelled"
)

// ItemStatus 是单张图（一个「商品图 × 提示词」）的状态。
// 对应 designkit_job_items.status。
type ItemStatus string

const (
	// ItemStatusPending 等待被 worker 领取。失败后退避重试也是回到这个状态
	// （把 available_at 往后推）。
	ItemStatusPending ItemStatus = "pending"
	// ItemStatusRunning 已被某个 worker 领走，worker_id + lease_expires_at 有值。
	ItemStatusRunning ItemStatus = "running"
	// ItemStatusSucceeded 出图成功，结果已入 designkit_images。
	ItemStatusSucceeded ItemStatus = "succeeded"
	// ItemStatusFailed 出图失败且已用尽 max_attempts。
	ItemStatusFailed ItemStatus = "failed"
	// ItemStatusCancelled 「还没开始就被停掉」。只有 pending 的 item 会落到这里，
	// 正在跑的不算（cancelled_count 的口径就是这个）。
	ItemStatusCancelled ItemStatus = "cancelled"
)

// String 让状态能直接拼进日志和 SQL 参数。
func (s JobStatus) String() string { return string(s) }

// String 让状态能直接拼进日志和 SQL 参数。
func (s ItemStatus) String() string { return string(s) }

// AllJobStatuses 返回全部合法的任务状态，顺序固定（测试和校验用）。
func AllJobStatuses() []JobStatus {
	return []JobStatus{
		JobStatusCreated,
		JobStatusHolding,
		JobStatusRunning,
		JobStatusSettling,
		JobStatusSucceeded,
		JobStatusPartiallyFailed,
		JobStatusFailed,
		JobStatusCancelled,
	}
}

// AllItemStatuses 返回全部合法的单张图状态，顺序固定。
func AllItemStatuses() []ItemStatus {
	return []ItemStatus{
		ItemStatusPending,
		ItemStatusRunning,
		ItemStatusSucceeded,
		ItemStatusFailed,
		ItemStatusCancelled,
	}
}

// IsValid 判断是不是合法的任务状态取值。
func (s JobStatus) IsValid() bool {
	for _, v := range AllJobStatuses() {
		if v == s {
			return true
		}
	}
	return false
}

// IsValid 判断是不是合法的单张图状态取值。
func (s ItemStatus) IsValid() bool {
	for _, v := range AllItemStatuses() {
		if v == s {
			return true
		}
	}
	return false
}

// IsTerminal 判断任务是不是已经到终态。
//
// ⚠ 终态**不是**永久的：运营点「重试」会把 succeeded / partially_failed 拉回
// running（同时 revision + 1）。所以别拿 IsTerminal 当「这行以后不会再变」的依据，
// 要判断「账已经算清、不能再动」看的是 settled_at 是否为空。
func (s JobStatus) IsTerminal() bool {
	switch s {
	case JobStatusSucceeded, JobStatusPartiallyFailed, JobStatusFailed, JobStatusCancelled:
		return true
	default:
		return false
	}
}

// IsTerminal 判断单张图是不是到终态了。
func (s ItemStatus) IsTerminal() bool {
	switch s {
	case ItemStatusSucceeded, ItemStatusFailed, ItemStatusCancelled:
		return true
	default:
		return false
	}
}

// jobTransitions 是任务状态迁移白名单。
//
//	created          → holding
//	holding          → running
//	running          → settling
//	settling         → succeeded | partially_failed | failed | cancelled
//	succeeded        → running        ← 仅「重试」触发，同时 revision + 1
//	partially_failed → running        ← 同上
//	任意             → failed         ← 冻结失败、僵尸回收（统一在下面补）
//
// **刻意没有 running → cancelled。**
// 运营点的按钮叫「停止排队」不叫「取消」，它只做两件事：把 pending 的 item 转
// cancelled、给 job 打 cancel_requested_at。已经在跑的那几张**必须等它自然落地**，
// 走跟正常完成完全相同的结算路径，然后才从 settling 落到 cancelled。
// 直接 running → cancelled 就会跳过结算：在途的图出来了、上游扣了钱，
// 我们的 actual_cost 永远是空的，钱对不上账。
var jobTransitions = map[JobStatus][]JobStatus{
	JobStatusCreated:  {JobStatusHolding},
	JobStatusHolding:  {JobStatusRunning},
	JobStatusRunning:  {JobStatusSettling},
	JobStatusSettling: {JobStatusSucceeded, JobStatusPartiallyFailed, JobStatusCancelled},

	// 重试：把已经结束的批次拉回 running。
	// 调用方同事务里必须对 job 行 FOR UPDATE、清 finished_at、revision + 1，
	// 并且拒绝 settled_at IS NOT NULL 的任务（409「这批任务已结算，请重新提交」）。
	JobStatusSucceeded:       {JobStatusRunning},
	JobStatusPartiallyFailed: {JobStatusRunning},

	JobStatusFailed:    {},
	JobStatusCancelled: {},
}

// CanTransition 判断任务能不能从 from 迁到 to。**这是唯一的判据。**
//
// 除白名单外还有一条兜底规则：**任意合法状态 → failed 永远允许**。
// 它是给两个场景留的口子：
//   - 冻结额度失败 / 建 items 失败，任务在 created 或 holding 上就得原地判死；
//   - 僵尸回收：巡检扫到 holding/running 且 heartbeat_at 超过 10 分钟没动静，
//     原子转 failed，last_error_code = DK_STALE，释放我们的 hold 行。
//     进程随时可能被杀，任何一个状态上都可能留下僵尸，所以不能只对某几个状态开。
//
// from 或 to 不是合法取值时一律返回 false（不要靠调用方保证）。
func CanTransition(from, to JobStatus) bool {
	if !from.IsValid() || !to.IsValid() {
		return false
	}
	// 任意 → failed。包含 failed → failed：巡检和结算 worker 可能同时判死同一个
	// 任务，允许这个自迁移让第二次变成一次无害的空转（带守卫的 UPDATE 影响 0 行），
	// 而不是抛一个没人处理的「非法迁移」错误。
	if to == JobStatusFailed {
		return true
	}
	for _, allowed := range jobTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// itemTransitions 是单张图的状态迁移白名单。
//
//	pending   → running          ← worker 领取（那条 FOR UPDATE SKIP LOCKED 的 UPDATE）
//	pending   → cancelled        ← 「停止排队」，还没开始就被砍掉
//	pending   → failed           ← 整批被判死，或退避后发现 attempt_count 已达上限
//	running   → succeeded        ← 出图成功，结果入库
//	running   → failed           ← 出图失败且没有剩余尝试次数
//	running   → pending          ← 出图失败但还能再试：available_at 往后推，退避重试
//	running   → running          ← 僵尸回收：租约过期后被另一个 worker 重新领走
//	succeeded → pending          ← 运营点「重试」（重试 = 重新出一张 = 重新收一次钱）
//	failed    → pending          ← 同上
//	cancelled → pending          ← 同上：停掉之后又想把这几张补出来
//
// **刻意没有 running → cancelled**，理由同 jobTransitions。
// **刻意没有 succeeded → failed / cancelled → succeeded** 这类跨终态跳变：
// 一张已经出图并扣过钱的图被改成 failed，billed_cost 就成了无主的钱。
//
// 注意「重试」的目标是 pending 不是 running：worker 的领取 SQL 只认 pending
// （`WHERE status='pending' AND available_at <= NOW()`），直接置 running
// 会造出一张永远没人跑、租约又立刻过期、然后被僵尸回收无限重跑的图。
var itemTransitions = map[ItemStatus][]ItemStatus{
	ItemStatusPending:   {ItemStatusRunning, ItemStatusCancelled, ItemStatusFailed},
	ItemStatusRunning:   {ItemStatusSucceeded, ItemStatusFailed, ItemStatusPending, ItemStatusRunning},
	ItemStatusSucceeded: {ItemStatusPending},
	ItemStatusFailed:    {ItemStatusPending},
	ItemStatusCancelled: {ItemStatusPending},
}

// CanTransitionItem 判断单张图能不能从 from 迁到 to。**这是唯一的判据。**
//
// 跟 CanTransition 不同，这里**没有**「任意 → failed」的兜底：
// 已经 succeeded 的图不许被任何巡检改成 failed（钱扣了、图在库里，改状态只会让账对不上）。
// 僵尸回收对 item 的做法是「重新领取」（running → running）或「退回重排」
// （running → pending），不是判死。
func CanTransitionItem(from, to ItemStatus) bool {
	if !from.IsValid() || !to.IsValid() {
		return false
	}
	for _, allowed := range itemTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// TerminalJobStatus 由四个计数决定批次的终态。
//
// 调用点只有一处：结算 worker 那条带守卫的 UPDATE
//
//	UPDATE designkit_jobs SET status=?, finished_at=NOW()
//	WHERE id=? AND finished_at IS NULL
//	  AND success_count + fail_count + cancelled_count = item_count
//
// 只有 RowsAffected = 1 的那个 worker 去结算和发回调，否则 5 个 worker 都以为
// 自己是最后一张，回调会发 5 次。
//
// stopRequested 是 cancel_requested_at 是否非空。
// 只要运营点过「停止排队」并且真的砍掉了至少一张（cancelled > 0），终态就记
// cancelled——哪怕有几张已经跑完了。这是有意的：运营的心智是「我停了」，
// 状态得跟他的动作对上。已经出的图和已经花的钱一分不少地留在
// success_count / actual_cost 里，ERP 文档写死「判断有没有图一律看 success_count」。
func TerminalJobStatus(itemCount, success, fail, cancelled int, stopRequested bool) JobStatus {
	if stopRequested && cancelled > 0 {
		return JobStatusCancelled
	}
	if itemCount > 0 && success == itemCount {
		return JobStatusSucceeded
	}
	if success > 0 {
		return JobStatusPartiallyFailed
	}
	if cancelled > 0 && fail == 0 {
		return JobStatusCancelled
	}
	return JobStatusFailed
}
