package worker

import (
	"context"
	"log/slog"
	"strings"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ============================================================================
// 结算
// ============================================================================
//
// 「settling」这个状态存在的唯一理由：**上游写 usage_logs 是异步的。**
// 它把写账单的活丢进一个 worker 池就返回了（只有池满时才同步兜底），
// 所以最后一张图返回的那一刻立刻汇总，很可能少算、甚至全算 0。
//
// 流程：
//
//	所有 item 到终态 → job 置 settling（由出图 worker 那条带守卫的 UPDATE 抢）
//	→ 结算 worker 枚举每张图**每一次 attempt** 的 request_id，
//	  一次性去 SUM(usage_logs.actual_cost)
//	→ 「上游一定扣过钱」的那几笔还没到齐就指数退避重试（上限 5 次）
//	→ 写 actual_cost + 终态 + settled_at，并把剩余冻结额置 released
//
// 四条铁律：
//
//	① actual_cost = SUM(items.billed_cost)，**禁止用「成功张数 × 单价」** ——
//	   同一批 10 张可能落不同计费档、单价不同。
//	② **不许写 assert actual - hold <= 1e-8**。钱已经被上游扣走了，
//	   报错只会让任务永远卡在 settling。改成分级告警。
//	③ 「停止排队」的批次走**跟正常完成完全相同**的结算路径，
//	   终态记 cancelled 但 actual_cost 照实填 —— 不填就成了凭空多出的无归属扣款。
//	④ 判据是「上游有没有为它扣过钱」，**不是「我们这边成没成功」**。
//	   见下面 billingCandidates 那段。

// ---------------------------------------------------------------------------
// 这一轮修掉的三个亏钱缺陷（改动理由，别删）
// ---------------------------------------------------------------------------
//
// 【一】actual_cost 系统性少算：重试过的钱全丢了。
// 旧代码只拿 job_items.billing_request_id 这**一个**值去反查，而那一列只存
// 最新一次 attempt 的 id；ApplyItemBilling 又是覆盖语义。于是每一张重试过的图，
// 前一次 attempt 真花掉的钱既不进 actual_cost、也不从冻结额里减 ——
// 而设计定型 6.2 写死「actual_cost = 所有 attempt 合计」。
// 修法：request_id 是**可推导**的（StoredBillingRequestID(jobUID, seq, attempt)），
// 所以枚举 attempt = 1..attempt_count 生成全部 id 一次查完，不需要任何新的存储。
//
// 【二】失败但已扣费的图完全不入账。
// 旧代码只统计 Status == succeeded 的 item。而「上游已出图扣钱、我们存图失败 /
// 写回失败 / 超时判死」这几种情况钱是真花了的（决策 15：网关跟浏览器连接**故意**
// 脱钩，我们这边算不算成功跟上游扣不扣钱毫无关系）。
// 修法：不管最终成功还是失败，**每一张的每一次 attempt 都去查**，查到多少算多少。
//
// 【三】settling 卡住会永久吃掉用户可用额。
// 当时僵尸巡检（repository.ReapStaleJobs）的状态白名单只有 ('created','holding','running')，
// **不含 settling**，而 FinalizeJob 一律把 job 写成 settling。于是结算这条路上
// 任何一次「卡住」都没有兜底：hold 永远是 open、用户可用额被永久吃掉，
// 而且没有任何报错 —— 表现出来正是「明明没任务却提示余额不足」。
// 修法见 settleStaleAfter。
//
// ⚠ 这个兜底只在**结算 worker 还在跑**时有效，而 worker 缺对象存储 / 图像服务 /
// 出图网关任意一个就整个不启动（module.go 的 newWorker）—— 那恰恰是最需要兜底的场景。
// 所以第二道兜底在巡检那一侧：repository 的 ReapStaleJobs 会扫
// `status='settling' 且 finished_at 超过 staleSettlingAfter（60 分钟）`，
// 按四个计数判真实终态、用已回填的 billed_cost 封账、释放冻结额。
// 那一档故意比这里的 30 分钟长一倍：结算 worker 只要活着就该先动手，它算得准。

// 超预估告警的两条线。
var (
	// billingAlertWarnFactor 实际 > 预估 × 1.2 → warn。
	billingAlertWarnFactor = dkdomain.MoneyFromFloat(1.2)
	// billingAlertCriticalFactor 实际 > 预估 × 2 → critical。
	billingAlertCriticalFactor = dkdomain.MoneyFromInt(2)
)

const (
	// settleStaleAfter 批次进 settling 超过这么久还没结清，就按已知金额强制结算
	// 并释放冻结额，同时记一条 critical 告警。
	//
	// 为什么要有：结算路径上任何一次卡住（写库一直失败、进程反复重启、
	// 队列头部被几个结不掉的批次堵住）都会让 hold 永远停在 open，
	// 用户可用额被永久吃掉且**没有任何报错**。
	//
	// 为什么是 30 分钟而不是 running 那档的 10 分钟：正常退避 5 次要约 2.5 分钟，
	// 上游账单写晚了几分钟也属正常，阈值太短会把还在正常等待的批次误判成卡住。
	//
	// 刻意不做成配置项：它是「兜底」不是「调参」，运营看不懂，
	// 调错了（比如调成 10 秒）会把所有批次都变成强制结算、天天刷 critical 告警。
	settleStaleAfter = 30 * time.Minute

	// maxEnumeratedAttempts 枚举 request_id 时最多往回数几次 attempt。
	// job_items.max_attempts 默认 3、上限也才 99（BillingRequestIDMaxAttempt），
	// 这个数只是给 attempt_count 被写坏时兜一个底，免得拼出几十万个 id 去查库。
	maxEnumeratedAttempts = dkdomain.BillingRequestIDMaxAttempt
)

// settleState 是一个批次的对账重试状态（只在结算 goroutine 里读写，不用加锁）。
type settleState struct {
	attempts int
	nextAt   time.Time
	lastSeen time.Time
}

type settler struct {
	pool   *Pool
	states map[int64]*settleState
}

func newSettler(p *Pool) *settler {
	return &settler{pool: p, states: make(map[int64]*settleState)}
}

// runSettlement 是结算的循环。
func (p *Pool) runSettlement(ctx context.Context) {
	defer p.wg.Done()

	ticker := time.NewTicker(p.cfg.SettleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.settleOnce(ctx)
		}
	}
}

// settleOnce 跑一轮结算。
//
// **必须拿 advisory lock**：ApplyItemBilling 会按张递减冻结额，
// 两个副本同时结算同一个批次就会多减一次，用户的可用额凭空少一截。
func (p *Pool) settleOnce(ctx context.Context) {
	p.withAdvisoryLock(ctx, AdvisoryLockKeySettle, "settlement", func(runCtx context.Context) {
		p.settler.run(runCtx)
	})
}

// run 扫一轮待结算的批次。
func (s *settler) run(ctx context.Context) {
	p := s.pool
	jobs, err := p.deps.Repo.ListJobsPendingSettlement(ctx, p.cfg.SettleBatch)
	if err != nil {
		p.log.Warn("designkit 查询待结算批次失败", slog.Any("error", err))
		return
	}

	now := p.now()
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		st := s.stateFor(job.ID, now)
		// 退避窗口对「卡太久」的批次一样生效（settleJob 里才决定要不要强制结算）。
		// 不在这里放行是有意的：结算写库要是一直失败，放行就变成每 10 秒
		// 重试一次 + 刷一条 error 日志，而退避封顶 2 分钟，
		// 既能把卡住的批次结掉，又不会把日志冲垮。
		if now.Before(st.nextAt) {
			continue
		}
		if s.settleJob(ctx, job, st) {
			delete(s.states, job.ID)
		}
	}
	s.prune(now)
}

func (s *settler) stateFor(jobID int64, now time.Time) *settleState {
	st, ok := s.states[jobID]
	if !ok {
		st = &settleState{}
		s.states[jobID] = st
	}
	st.lastSeen = now
	return st
}

// prune 丢掉很久没再出现的状态，防止 map 无限长大。
func (s *settler) prune(now time.Time) {
	const keepFor = time.Hour
	for id, st := range s.states {
		if now.Sub(st.lastSeen) > keepFor {
			delete(s.states, id)
		}
	}
}

// stuckInSettling 判断这个批次是不是在 settling 里卡太久了。
//
// 基准取 finished_at（FinalizeJob 写 settling 的同一条 UPDATE 里落的时刻），
// 它没有才退回 updated_at。**不能用 heartbeat_at**：进了 settling 就没有在途 item 了，
// 心跳本来就不会再续。
func (s *settler) stuckInSettling(job *dkdomain.Job, now time.Time) bool {
	if job == nil {
		return false
	}
	since := job.UpdatedAt
	if job.FinishedAt != nil && !job.FinishedAt.IsZero() {
		since = *job.FinishedAt
	}
	if since.IsZero() {
		return false
	}
	return now.Sub(since) > settleStaleAfter
}

// ---------------------------------------------------------------------------
// 一张图的对账
// ---------------------------------------------------------------------------

// itemBilling 是一张图的对账中间结果。
type itemBilling struct {
	item *dkdomain.JobItem
	// ids 这一张**所有 attempt** 的完整 request_id（带 client: 前缀），attempt 升序。
	ids []string
	// requiredID 「上游一定为它扣过钱」的那一个 id；空串表示这张图没有这样的 id。
	requiredID string
	// total ids 里查到账单的那几笔的合计。
	total dkdomain.Money
	// hits ids 里有几笔查到了账单。
	hits int
}

// billingCandidates 算出一张图所有 attempt 的 request_id，以及其中哪一个是**必须命中**的。
//
// 【为什么可以枚举】
// request_id 是纯推导的：StoredBillingRequestID(jobUID, seq, attempt)，
// 规则写死在 domain/billing_request_id.go —— 同一个 (seq, attempt) 无论重投几次
// 都是同一个 id，attempt +1 就换新 id。所以不需要为「历史 attempt」额外存任何东西，
// 从 attempt_count 就能把这张图花过的每一笔都数出来。
// job_items.billing_request_id 里存的那个值也照样带上：万一哪天上游换了前缀，
// 落库的那个才是权威值，推导值会漏。
//
// 【为什么只有成功的那一次是「必须命中」】
// 我们能确定上游扣过钱的，只有「收到过上游成功响应」的那一次 attempt。
// 失败的 attempt 有两种，我们从这边分不出来：
//   - 压根没打到上游（预处理失败、429、参数错）→ 永远不会有账单，等也没用；
//   - 上游出了图扣了钱，我们这边存图/写回失败 → 有账单，要算进来。
//
// 所以：必须命中的只有成功那一笔，其余「查到多少算多少」。
// 拿失败的 attempt 当必须命中会让批次一路退避到上限再强制结算，白等 2.5 分钟。
func billingCandidates(jobUID string, item *dkdomain.JobItem) ([]string, string) {
	if item == nil {
		return nil, ""
	}
	attempts := item.AttemptCount
	if attempts > maxEnumeratedAttempts {
		attempts = maxEnumeratedAttempts
	}

	ids := make([]string, 0, attempts+1)
	for attempt := 1; attempt <= attempts; attempt++ {
		ids = append(ids, dkdomain.StoredBillingRequestID(jobUID, item.Seq, attempt))
	}

	stored := ""
	if item.BillingRequestID != nil {
		stored = strings.TrimSpace(*item.BillingRequestID)
	}
	if stored != "" && !containsStr(ids, stored) {
		ids = append(ids, stored)
	}

	if item.Status != dkdomain.ItemStatusSucceeded {
		return ids, ""
	}
	if stored != "" {
		return ids, stored
	}
	if len(ids) > 0 {
		return ids, ids[len(ids)-1]
	}
	return ids, ""
}

// collectBillings 把整批的 request_id 都摊出来。
func collectBillings(job *dkdomain.Job, items []*dkdomain.JobItem) ([]*itemBilling, []string) {
	billings := make([]*itemBilling, 0, len(items))
	all := make([]string, 0, len(items)*2)
	seen := make(map[string]struct{}, len(items)*2)

	for _, item := range items {
		if item == nil {
			continue
		}
		ids, requiredID := billingCandidates(job.UID, item)
		billings = append(billings, &itemBilling{item: item, ids: ids, requiredID: requiredID, total: dkdomain.ZeroMoney})
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			all = append(all, id)
		}
	}
	return billings, all
}

// fillCosts 把查回来的账单填进每一张的合计，并统计「必须命中的还差几笔」。
func fillCosts(billings []*itemBilling, costs map[string]dkdomain.Money) (required, requiredHits, optionalMissing int) {
	for _, b := range billings {
		total := dkdomain.ZeroMoney
		for _, id := range b.ids {
			cost, ok := costs[id]
			if ok {
				total = total.Add(cost)
				b.hits++
			}
			if id == b.requiredID && b.requiredID != "" {
				required++
				if ok {
					requiredHits++
				}
				continue
			}
			if !ok {
				optionalMissing++
			}
		}
		b.total = dkdomain.QuantizeMoney(total)
	}
	return required, requiredHits, optionalMissing
}

// ---------------------------------------------------------------------------
// 结算一个批次
// ---------------------------------------------------------------------------

// settleJob 结算一个批次。返回 true 表示这个批次已经处理完（成功或彻底放弃），
// 可以把重试状态丢掉；返回 false 表示还要再来一次。
func (s *settler) settleJob(ctx context.Context, job *dkdomain.Job, st *settleState) bool {
	p := s.pool
	log := p.log.With(slog.Int64("job_id", job.ID), slog.String("job_uid", job.UID))

	items, err := p.deps.Repo.ListJobItems(ctx, job.ID)
	if err != nil {
		log.Warn("designkit 结算时读取任务明细失败", slog.Any("error", err))
		s.scheduleRetry(st, p.now())
		return false
	}

	billings, requestIDs := collectBillings(job, items)

	costs := map[string]dkdomain.Money{}
	if len(requestIDs) > 0 {
		costs, err = p.deps.Repo.SumUsageLogCosts(ctx, requestIDs)
		if err != nil {
			log.Warn("designkit 结算时反查账单失败", slog.Any("error", err))
			s.scheduleRetry(st, p.now())
			return false
		}
	}
	required, requiredHits, optionalMissing := fillCosts(billings, costs)
	missingRequired := required - requiredHits

	stale := s.stuckInSettling(job, p.now())

	// 还该不该再等一轮？
	//   ① 「上游一定扣过钱」的那几笔还没到齐 —— 这不是错误，是**预期内的等待**；
	//   ② 一笔「必须命中」都没有（整批一张都没成功），但有 attempt 还查不到账单：
	//      宽限一轮再走。上游写 usage_logs 是异步的，那一笔很可能就差这几秒；
	//      不宽限的话「上游扣了钱、我们判失败」这一类会被直接记成 0。
	//      只宽限一轮，因为「打都没打到上游」的 attempt 等多久都不会有账单。
	waiting := missingRequired > 0 || (required == 0 && optionalMissing > 0 && st.attempts == 0)
	if waiting && !stale && st.attempts < p.cfg.SettleMaxAttempts {
		s.scheduleRetry(st, p.now())
		log.Info("designkit 账单还没写完，稍后重试结算",
			slog.Int("required_hits", requiredHits),
			slog.Int("required", required),
			slog.Int("optional_missing", optionalMissing),
			slog.Int("attempt", st.attempts),
			slog.Duration("next_in", settleBackoff(p.cfg.SettleBackoffBase, st.attempts)))
		return false
	}

	forced := missingRequired > 0
	if forced {
		// 等不到账单也**照样结算**：让批次永远停在 settling 更糟 ——
		// 冻结额一直占着用户的可用额，运营看到的是一个永远转圈的任务。
		//
		// ⚠ 冻结额这里是「释放 + 留 critical 告警」，不是「留着 open 等人工」。
		// 理由：留 open 就等于把上面【三】那个缺陷手工再造一遍 ——
		// 没有任何机制会再来收它，用户从此看到「明明没任务却提示余额不足」，
		// 而管理员那边一点动静都没有。少记一笔账（管理员按下面这行日志能在
		// usage_logs 里逐笔捞回来）远好过一个卡死的账号。
		log.Error("designkit 等不到全部账单，按已知金额强制结算（actual_cost 可能偏小，需人工核对）",
			slog.Int("missing_bills", missingRequired),
			slog.Int("required", required),
			slog.Int("attempts", st.attempts),
			slog.Any("missing_request_ids", missingRequiredIDs(billings, costs)))
	}
	if stale {
		log.Error("designkit 批次在 settling 里卡太久，按已知金额强制结算并释放冻结额",
			slog.Duration("stale_after", settleStaleAfter),
			slog.Any("finished_at", job.FinishedAt),
			slog.Int("attempts", st.attempts))
	}

	actual := s.applyItemBilling(ctx, job, billings, log)

	status := dkdomain.TerminalJobStatus(job.ItemCount, job.SuccessCount, job.FailCount, job.CancelledCount, job.IsStopRequested())
	if !dkdomain.CanTransition(job.Status, status) {
		if !job.Status.IsTerminal() {
			// 批次已经不在 settling 上、也还没到终态（例如被重试拉回了 running）。
			// 不强行改状态，交给下一轮或人工处理。
			log.Warn("designkit 结算时状态迁移不被允许，跳过",
				slog.String("from", job.Status.String()), slog.String("to", status.String()))
			return true
		}
		// 已经被别人（僵尸巡检）判到终态了。**仍然要把账写完**：
		// 不写的话这批的钱在库里没有任何记录，冻结额也没人释放。
		log.Warn("designkit 批次已被判到终态，按当前状态结算",
			slog.String("job_status", job.Status.String()),
			slog.String("computed", status.String()))
		status = job.Status
	}

	if err := p.deps.Repo.SettleJob(ctx, dkdomain.SettleJobParams{
		JobID:      job.ID,
		ActualCost: actual,
		Status:     status,
		SettledAt:  p.now(),
	}); err != nil {
		log.Error("designkit 结算写库失败，下一轮重试", slog.Any("error", err))
		s.scheduleRetry(st, p.now())
		return false
	}

	// ⚠ 告警必须记在 SettleJob **成功之后**。
	// 记在前面的话，SettleJob 失败会重来一轮，而 designkit_billing_alerts
	// 上没有任何唯一约束（9001 已定稿、不可改），同一个批次会被插好几条，
	// 管理员后台看到的是一堆重复待办。
	s.recordBillingAlert(ctx, job, actual, settlementAlertLevel(job, actual, forced || stale), log)

	log.Info("designkit 批次已结算",
		slog.String("status", status.String()),
		slog.String("actual_cost", dkdomain.MoneyString(actual)),
		slog.String("estimated_cost", dkdomain.MoneyString(job.EstimatedCost)),
		slog.String("currency", dkdomain.CurrencyUSD),
		slog.Int("success", job.SuccessCount),
		slog.Int("fail", job.FailCount),
		slog.Int("cancelled", job.CancelledCount),
		slog.Int("bills_found", totalHits(billings)),
		slog.Bool("forced", forced),
		slog.Bool("stale", stale))
	return true
}

// applyItemBilling 把每一张的实际扣费回填进 job_items（同时按张递减冻结额），
// 并算出整批的 actual_cost。
//
// **actual_cost 一律是这里逐张加出来的**，禁止用「成功张数 × 单价」——
// 同一批 10 张可能落不同计费档、单价不同。
//
// 每一张写进 billed_cost 的是「这张图**所有 attempt** 的合计」，
// 跟设计定型 6.2「actual_cost = 所有 attempt 合计」对齐。
func (s *settler) applyItemBilling(
	ctx context.Context,
	job *dkdomain.Job,
	billings []*itemBilling,
	log *slog.Logger,
) dkdomain.Money {
	p := s.pool
	total := dkdomain.ZeroMoney

	for _, b := range billings {
		item := b.item
		old := dkdomain.MoneyOrZero(item.BilledCost)

		if b.hits == 0 {
			// 这一轮一条账单都没查到：如果之前已经回填过，用回填过的值，别把它算丢。
			total = total.Add(old)
			continue
		}

		if item.BilledCost != nil && !b.total.GreaterThan(old) {
			// **billed_cost 只增不减。**
			// ApplyItemBilling 动冻结额用的是「新值 - 旧值」这个差额，
			// 负差额会把冻结额**加**回去（GREATEST(amount - 负数, 0) = amount + |差额|），
			// 凭空放大用户的可用额，接着就能透支。
			// 而账单只会越查越全、不会消失，所以变小一定是别的地方出了问题，
			// 保守起见按已经记下的那个（较大的）值算。
			if b.total.LessThan(old) {
				log.Warn("designkit 这一张反查回来的金额比已回填的还小，保留已回填的值",
					slog.Int64("item_id", item.ID), slog.Int("seq", item.Seq),
					slog.String("billed_cost", dkdomain.MoneyString(old)),
					slog.String("recomputed", dkdomain.MoneyString(b.total)))
			}
			total = total.Add(old)
			continue
		}

		total = total.Add(b.total)

		mode := derefString(item.BillingMode, job.BillingMode)
		tier := s.resolveBillingTier(ctx, item, job)
		if err := p.deps.Repo.ApplyItemBilling(ctx, item.ID, b.total, mode, tier); err != nil {
			log.Warn("designkit 回填单张扣费失败",
				slog.Int64("item_id", item.ID), slog.Int("seq", item.Seq), slog.Any("error", err))
		}
	}
	return dkdomain.QuantizeMoney(total)
}

// resolveBillingTier 算这一张实际落的计费档。
//
// 口径跟上游一致：**看上游实际返回的图片像素**，一次返回多张时取最高的那一档
// （不是我们请求里写的 size —— 实测请求 1:1 拿回 1254×1254，落的是 2K 不是 1K）。
func (s *settler) resolveBillingTier(ctx context.Context, item *dkdomain.JobItem, job *dkdomain.Job) string {
	if item.BillingTier != nil && *item.BillingTier != "" {
		return *item.BillingTier
	}
	images, err := s.pool.deps.Repo.ListImagesByItem(ctx, item.ID, true)
	if err == nil && len(images) > 0 {
		tiers := make([]string, 0, len(images))
		for _, img := range images {
			if img == nil {
				continue
			}
			if img.BillingTier != nil && *img.BillingTier != "" {
				tiers = append(tiers, *img.BillingTier)
				continue
			}
			tiers = append(tiers, dkdomain.ClassifyBillingTier(intOrZero(img.Width), intOrZero(img.Height)))
		}
		if tier := dkdomain.HighestBillingTier(tiers); tier != "" {
			return tier
		}
	}
	// 兜底用提交时快照的档位。它只是「预估时假定的档」，标它总比留空好排查。
	return job.PricingTier
}

// settlementAlertLevel 决定这次结算要不要记告警、记哪一级。
//
// forced=true（账单没查全 / 卡在 settling 太久被强制结算）一律 critical：
// 这两种情况下 actual_cost 是**不可信的**，必须有一条记录让管理员回头核，
// 否则「少算的钱」从此没有任何机制会把它捞回来。
//
// 一次结算最多记一条 —— 表上没有任何唯一约束（9001 已定稿），
// 多记就是管理员后台里的重复待办。
func settlementAlertLevel(job *dkdomain.Job, actual dkdomain.Money, forced bool) string {
	if forced {
		return dkdomain.BillingAlertLevelCritical
	}
	if job.EstimatedCost.Sign() <= 0 {
		return "" // 预估是 0（价目表还没实测出来），没有可比的基准
	}
	switch {
	case actual.GreaterThan(job.EstimatedCost.Mul(billingAlertCriticalFactor)):
		return dkdomain.BillingAlertLevelCritical
	case actual.GreaterThan(job.EstimatedCost.Mul(billingAlertWarnFactor)):
		return dkdomain.BillingAlertLevelWarn
	}
	return ""
}

// recordBillingAlert 落一条告警。level 为空就什么都不做。
//
// **绝不因为超预估就报错回滚** —— 钱已经被上游扣走了，报错只会让任务卡死。
//
// ⚠ designkit_billing_alerts 只有 (job_id, estimated, actual, ratio_over, level) 五列，
// 没有备注列，而 9001 迁移已经跑过、永不可改。所以「有几笔账单没查到」这类明细
// 只能进日志；表里这一行的作用是**让管理员在后台列表里看得见有这么一批要人工核**。
func (s *settler) recordBillingAlert(ctx context.Context, job *dkdomain.Job, actual dkdomain.Money, level string, log *slog.Logger) {
	if level == "" {
		return
	}

	// 预估是 0 时不能做除法（decimal 除 0 会 panic），比例记 0。
	ratioOver := dkdomain.ZeroMoney
	if job.EstimatedCost.Sign() > 0 {
		ratioOver = actual.Div(job.EstimatedCost).Round(dkdomain.MultiplierScale)
	}

	alert := &dkdomain.BillingAlert{
		JobID:     job.ID,
		Estimated: job.EstimatedCost,
		Actual:    actual,
		RatioOver: ratioOver,
		Level:     level,
		CreatedAt: s.pool.now(),
	}
	if err := s.pool.deps.Repo.InsertBillingAlert(ctx, alert); err != nil {
		log.Warn("designkit 写结算告警失败", slog.Any("error", err))
	}

	fields := []any{
		slog.String("level", level),
		slog.String("estimated", dkdomain.MoneyString(job.EstimatedCost)),
		slog.String("actual", dkdomain.MoneyString(actual)),
		slog.String("ratio_over", ratioOver.String()),
		slog.String("currency", dkdomain.CurrencyUSD),
	}
	if level == dkdomain.BillingAlertLevelCritical {
		log.Error("designkit 结算记了一条 critical 告警，请人工核对这批的账", fields...)
		return
	}
	log.Warn("designkit 实际花费超出预估", fields...)
}

func (s *settler) scheduleRetry(st *settleState, now time.Time) {
	st.attempts++
	st.nextAt = now.Add(settleBackoff(s.pool.cfg.SettleBackoffBase, st.attempts))
}

// missingRequiredIDs 列出「必须命中却没查到」的那几个 request_id，只给日志用。
// 管理员拿它直接去 usage_logs 里 WHERE request_id IN (...) 就能确认到底扣没扣。
func missingRequiredIDs(billings []*itemBilling, costs map[string]dkdomain.Money) []string {
	out := make([]string, 0, 4)
	for _, b := range billings {
		if b.requiredID == "" {
			continue
		}
		if _, ok := costs[b.requiredID]; !ok {
			out = append(out, b.requiredID)
		}
	}
	return out
}

func totalHits(billings []*itemBilling) int {
	n := 0
	for _, b := range billings {
		n += b.hits
	}
	return n
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func derefString(v *string, fallback string) string {
	if v != nil && *v != "" {
		return *v
	}
	return fallback
}

func intOrZero(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
