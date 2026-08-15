package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// runItemWorker 是一个出图 worker 的主循环：领一张 → 出一张 → 写回 → 再领。
//
// 领到活就立刻接着领（不睡），队列空了才睡 IdleDelay。
// 这样一批 50 张会被 4 个 worker 连续吃掉，而不是每张之间插 2 秒空转。
func (p *Pool) runItemWorker(ctx context.Context, workerID string) {
	defer p.wg.Done()

	log := p.log.With(slog.String("worker_id", workerID))
	log.Debug("designkit 出图 worker 已启动")

	for {
		if ctx.Err() != nil {
			log.Debug("designkit 出图 worker 退出")
			return
		}

		item, err := p.deps.Repo.ClaimNextItem(ctx, dkdomain.ClaimItemParams{
			WorkerID: workerID,
			LeaseFor: p.cfg.LeaseFor,
		})

		switch {
		case err == nil && item != nil:
			p.handleItem(ctx, workerID, item)

		case errors.Is(err, dkdomain.ErrNotFound) || (err == nil && item == nil):
			// 没活干。这是最常见的分支，不打日志。
			if !sleepCtx(ctx, p.cfg.IdleDelay) {
				return
			}

		default:
			// 领取失败通常是数据库抖了。退避久一点，别让 4 个 worker 一起猛敲。
			if ctx.Err() == nil {
				log.Warn("designkit 领取出图任务失败", slog.Any("error", err))
			}
			if !sleepCtx(ctx, p.cfg.ErrorDelay) {
				return
			}
		}
	}
}

// handleItem 处理一张已经领到手的图：出图 → 写回 → 顺手判一下整批是否收尾。
func (p *Pool) handleItem(ctx context.Context, workerID string, item *dkdomain.JobItem) {
	log := p.log.With(
		slog.String("worker_id", workerID),
		slog.Int64("job_id", item.JobID),
		slog.Int64("item_id", item.ID),
		slog.Int("seq", item.Seq),
		slog.Int("attempt", item.AttemptCount),
	)

	// **领到手第一件事：判重试上限。判完才允许做任何会花钱的事。**
	//
	// 原来错在哪：整条出图链路（handleItem → processItem）从头到尾没判过
	// attempt 上限，而领取 SQL 的僵尸回收分支
	// （status='running' AND lease_expires_at < NOW()）**也刻意不看 attempt** ——
	// 不看才能把崩溃残留的 running 行捞回来判死。两边都不看 = 没人看。
	// 于是一张已经试满 3 次、卡在 running 的图，租约一过期就被重新领走，
	// **先真调一次网关花掉一张图的钱**，才在写回时由 classify 判成
	// DK_MAX_ATTEMPTS_EXCEEDED。租约每过期一次就白烧一次。
	if attemptBudgetExceeded(item.AttemptCount, item.MaxAttempts) {
		p.failExhaustedItem(item, workerID,
			fmt.Sprintf("attempt %d exceeds max_attempts %d; upstream call skipped",
				item.AttemptCount, normalizeMaxAttempts(item.MaxAttempts)),
			log)
		return
	}

	p.inflight.add(item.JobID)
	defer p.inflight.remove(item.JobID)

	// 领到第一张就先给这个批次续一次心跳。
	// 不然「刚建好、heartbeat_at 还是空」的批次要等到下一个心跳周期才被登记。
	p.touchHeartbeat(ctx, []int64{item.JobID})

	// 出图的超时：单张超时 + 一点余量（预处理、存图也在这条链路上）。
	itemCtx, cancelItem := context.WithTimeout(ctx, p.settings.ItemTimeout+p.cfg.WriteBackTimeout)
	defer cancelItem()

	// 租约续期。丢了租约就立刻掐掉出图 —— 别人已经在出同一张了，
	// 我们再跑下去就是白花一份钱。
	var leaseLost atomic.Bool
	stopRenew := p.startLeaseRenewal(itemCtx, cancelItem, item, workerID, &leaseLost, log)
	defer stopRenew()

	// 先把批次读出来：出图要用它的 uid（拼计费 request_id）、比例、模型、
	// user_id 和 api_key_id。取不到就当一次可重试的内部错误。
	var (
		result *itemResult
		jobUID string
	)
	job, err := p.deps.Repo.GetJobByID(itemCtx, item.JobID)
	if err != nil {
		err = failRetry(dkdomain.ErrCodeInternal).withCause(err)
	} else {
		jobUID = job.UID
		result, err = p.processItem(itemCtx, job, item, log)
	}

	// 进程要退出了：把租约还回去，让别人接手。
	// **这一张的钱可能已经花了**（上游出图跟我们的 context 是故意脱钩的），
	// 日志里带上 billing_request_id 方便管理员去 usage_logs 对账。
	if err != nil && p.isStopping() && errors.Is(err, context.Canceled) {
		p.releaseLease(item, jobUID, workerID, log)
		return
	}

	if leaseLost.Load() {
		log.Warn("designkit 租约已被别人接管，本次结果丢弃",
			slog.Bool("had_images", result != nil))
		return
	}

	writeCtx, cancelWrite := p.writeBackContext()
	defer cancelWrite()

	if err != nil {
		p.writeFailure(writeCtx, item, workerID, err, log)
		return
	}
	p.writeSuccess(writeCtx, item, workerID, result, log)
}

// writeSuccess 写回一张出好的图。
func (p *Pool) writeSuccess(ctx context.Context, item *dkdomain.JobItem, workerID string, result *itemResult, log *slog.Logger) {
	if result == nil {
		return
	}
	err := p.deps.Repo.CompleteItemSuccess(ctx, dkdomain.CompleteItemSuccessParams{
		ItemID:           item.ID,
		WorkerID:         workerID,
		Attempt:          item.AttemptCount,
		BillingRequestID: result.storedBillingRequestID,
		Images:           result.images,
		FinishedAt:       p.now(),
	})
	switch {
	case err == nil:
		log.Info("designkit 出图成功",
			slog.Int("images", len(result.images)),
			slog.String("billing_request_id", result.storedBillingRequestID))
		p.finalizeJob(ctx, item.JobID, log)

	case errors.Is(err, dkdomain.ErrLeaseLost):
		// **拿到它必须丢弃结果，不要重试写入。**
		// 租约被抢走说明另一个 worker 正在（或已经）出同一张图，
		// 硬写会把两次结果混在一行上。
		log.Warn("designkit 写回成功结果时租约已失效，结果丢弃",
			slog.String("billing_request_id", result.storedBillingRequestID))

	default:
		// 图在对象存储里、钱在上游账上，但我们的库里没有这一行 —— 这是最难查的一种账。
		// 日志里必须带 billing_request_id 和 object_key，管理员照着能手工补。
		log.Error("designkit 写回成功结果失败，图已产生但未入库",
			slog.Any("error", err),
			slog.String("billing_request_id", result.storedBillingRequestID),
			slog.String("first_object_key", firstObjectKey(result.images)))
	}
}

// writeFailure 写回一张失败的图（含退避重排）。
func (p *Pool) writeFailure(ctx context.Context, item *dkdomain.JobItem, workerID string, cause error, log *slog.Logger) {
	if cause == nil {
		return
	}
	dkErr, terminal := classify(cause, item.AttemptCount, item.MaxAttempts)
	if dkErr == nil {
		dkErr = dkdomain.NewError(dkdomain.ErrCodeInternal)
	}

	var retryAfter time.Duration
	if !terminal {
		retryAfter = retryBackoff(p.cfg.RetryBackoffBase, item.AttemptCount)
	}

	// last_error_message 存上游英文原文（只给管理员看）；
	// 上游没给原文时退而求其次存我们自己的错误串，界面上一律只显示 dkErr.Message 的中文。
	upstream := dkErr.Upstream
	if upstream == "" {
		upstream = cause.Error()
	}

	err := p.deps.Repo.CompleteItemFailure(ctx, dkdomain.CompleteItemFailureParams{
		ItemID:       item.ID,
		WorkerID:     workerID,
		ErrorCode:    dkErr.Code,
		ErrorMessage: upstream,
		Terminal:     terminal,
		RetryAfter:   retryAfter,
		FinishedAt:   p.now(),
	})
	switch {
	case err == nil:
		if terminal {
			log.Warn("designkit 出图失败，不再重试",
				slog.String("error_code", dkErr.Code),
				slog.String("upstream", upstream))
			p.finalizeJob(ctx, item.JobID, log)
		} else {
			log.Info("designkit 出图失败，稍后重试",
				slog.String("error_code", dkErr.Code),
				slog.Duration("retry_after", retryAfter),
				slog.String("upstream", upstream))
		}

	case errors.Is(err, dkdomain.ErrLeaseLost):
		log.Warn("designkit 写回失败结果时租约已失效，结果丢弃",
			slog.String("error_code", dkErr.Code))

	default:
		log.Error("designkit 写回失败结果失败",
			slog.Any("error", err), slog.String("error_code", dkErr.Code))
	}
}

// finalizeJob 把「所有 item 都到终态了」这件事落到 job 上：转 settling。
//
// 判定和抢占合并在一条带守卫的 UPDATE 里（repository 负责），
// **只有影响行数 = 1 的那个 worker 拿到 won=true**。
// 不这么做，5 个 worker 会同时认为自己是最后一张，回调就发 5 次。
//
// 注意这里落的是 settling **不是终态**：上游写 usage_logs 是异步的
// （丢进 worker 池就返回），最后一张图返回的那一刻立刻汇总，很可能少算甚至全算 0。
// 真正的终态由结算 worker 在对完账之后写。
func (p *Pool) finalizeJob(ctx context.Context, jobID int64, log *slog.Logger) {
	won, err := p.deps.Repo.FinalizeJob(ctx, dkdomain.FinalizeJobParams{
		JobID:      jobID,
		Status:     dkdomain.JobStatusSettling,
		FinishedAt: p.now(),
	})
	if err != nil {
		// 没能收尾不是灾难：下一张图落地时还会再试一次；
		// 真的一张都不剩了，僵尸巡检会在 10 分钟后把它判死并释放冻结。
		log.Warn("designkit 判定批次收尾失败", slog.Any("error", err))
		return
	}
	if won {
		log.Info("designkit 批次全部落地，转入结算")
	}
}

// startLeaseRenewal 起一个续租 goroutine，返回停止它的函数。
//
// 租约 180 秒、每 60 秒续一次：漏掉一次续期还有两次机会。
// 续租发现租约已经是别人的（ErrLeaseLost）时，立刻取消出图 ——
// 继续跑下去就是同一张图出两遍、收两次钱。
func (p *Pool) startLeaseRenewal(
	ctx context.Context,
	cancelItem context.CancelFunc,
	item *dkdomain.JobItem,
	workerID string,
	leaseLost *atomic.Bool,
	log *slog.Logger,
) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		ticker := time.NewTicker(p.cfg.LeaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// 续租用独立的短超时 context：出图那个 ctx 可能正卡在网关上，
				// 但续租本身必须按时发出去。
				renewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.cfg.WriteBackTimeout)
				err := p.deps.Repo.RenewItemLease(renewCtx, item.ID, workerID, p.cfg.LeaseFor)
				cancel()

				if err == nil {
					// 正常续上，不打日志（一张图最多续两三次，打了全是噪音）。
					continue
				}
				if errors.Is(err, dkdomain.ErrLeaseLost) {
					leaseLost.Store(true)
					log.Warn("designkit 续租失败：租约已被别人接管，立刻中止本次出图")
					cancelItem()
					return
				}
				// 数据库抖了一下。不中止 —— 还有后面几次续期机会，
				// 中止反而白扔一张已经在跑的图。
				log.Warn("designkit 续租失败", slog.Any("error", err))
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

// releaseLease 把在途 item 的租约还回队列（进程要退出时走这条路）。
//
// **还有剩余次数**才走失败写回的非终态分支：item 退回 pending、available_at
// 不推迟，另一个副本（或重启后的自己）立刻就能领走。
// 错误码用 DK_STALE，界面显示「系统重启导致这批任务中断了」——
// 这跟事实一致，而且这一条会在重试成功后被新结果覆盖。
//
// **次数已经用完的走终态**（见函数里第一段注释），否则重启一次就多扣一次钱。
func (p *Pool) releaseLease(item *dkdomain.JobItem, jobUID, workerID string, log *slog.Logger) {
	// **交还之前先判上限。**
	//
	// 原来错在哪：这里无条件写 Terminal=false、RetryAfter=0，不看 attempt。
	// 于是一张已经试满 3 次的图，进程一重启就被立刻退回 pending、立刻被领走、
	// **再真出一次图、再扣一次钱**。重启几次就扣几次，max_attempts 形同虚设。
	// （运维滚动更新、OOM 重启都会踩到，不是罕见路径。）
	if attemptBudgetExhausted(item.AttemptCount, item.MaxAttempts) {
		log.Warn("designkit 进程退出，但这一张的尝试次数已经用完，判失败不再排队；这一张可能已经产生费用",
			slog.Int("max_attempts", normalizeMaxAttempts(item.MaxAttempts)),
			slog.String("billing_request_id",
				dkdomain.StoredBillingRequestID(jobUID, item.Seq, item.AttemptCount)))
		p.failExhaustedItem(item, workerID,
			fmt.Sprintf("worker shutting down at attempt %d of max_attempts %d; no retry budget left",
				item.AttemptCount, normalizeMaxAttempts(item.MaxAttempts)),
			log)
		return
	}

	ctx, cancel := p.writeBackContext()
	defer cancel()

	err := p.deps.Repo.CompleteItemFailure(ctx, dkdomain.CompleteItemFailureParams{
		ItemID:       item.ID,
		WorkerID:     workerID,
		ErrorCode:    dkdomain.ErrCodeStale,
		ErrorMessage: "designkit worker shutting down, lease returned to queue",
		Terminal:     false,
		RetryAfter:   0,
		FinishedAt:   p.now(),
	})
	if err == nil {
		log.Warn("designkit 进程退出，在途出图的租约已交还队列；这一张可能已经产生费用",
			slog.String("billing_request_id",
				dkdomain.StoredBillingRequestID(jobUID, item.Seq, item.AttemptCount)))
		return
	}
	if errors.Is(err, dkdomain.ErrLeaseLost) {
		// 已经被别人接管了，正是我们想要的结果。
		return
	}
	// 还不回去也没事：租约 180 秒后自然过期，会被僵尸回收那条 WHERE 捞起来。
	log.Warn("designkit 交还租约失败，等租约自然过期", slog.Any("error", err))
}

// failExhaustedItem 把「尝试次数已经用完却还在手上」的那一张直接判死。
//
// **绝不调网关**：调一次就是一张图的钱，而它注定会在写回时被判成
// DK_MAX_ATTEMPTS_EXCEEDED —— 花了钱换一个必然的失败。
//
// 落的是终态（Terminal=true），所以 fail_count 会自增，必须接着判一次收尾：
// 不判的话这可能正好是最后一张，批次会永远停在 running，冻结额一直占着
// （表现为「明明没任务却提示余额不足」），只能等 10 分钟后的僵尸巡检兜底。
func (p *Pool) failExhaustedItem(item *dkdomain.JobItem, workerID, upstream string, log *slog.Logger) {
	log.Warn("designkit 这一张已经用完尝试次数，直接判失败，不调网关",
		slog.String("error_code", dkdomain.ErrCodeMaxAttemptsExceeded),
		slog.String("upstream", upstream))

	ctx, cancel := p.writeBackContext()
	defer cancel()

	err := p.deps.Repo.CompleteItemFailure(ctx, dkdomain.CompleteItemFailureParams{
		ItemID:       item.ID,
		WorkerID:     workerID,
		ErrorCode:    dkdomain.ErrCodeMaxAttemptsExceeded,
		ErrorMessage: upstream,
		Terminal:     true,
		RetryAfter:   0,
		FinishedAt:   p.now(),
	})
	switch {
	case err == nil:
		p.finalizeJob(ctx, item.JobID, log)

	case errors.Is(err, dkdomain.ErrLeaseLost):
		// 租约已经被别人接管了，这一张由那个 worker 负责，我们什么都不做。
		log.Warn("designkit 判超限时租约已失效，交给接手的 worker")

	default:
		log.Error("designkit 判超限时写回失败", slog.Any("error", err))
	}
}

func firstObjectKey(images []dkdomain.NewImage) string {
	if len(images) == 0 {
		return ""
	}
	return images[0].ObjectKey
}
