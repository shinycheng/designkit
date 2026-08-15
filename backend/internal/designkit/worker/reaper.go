package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// runReaper 是僵尸巡检的循环。
//
// v1 完全没有这一段，那是个致命缺口：进程被 kill / 容器被重启之后，
// 那批任务永远停在 running，冻结额永远占着用户的可用额 ——
// 表现出来就是「明明没有任务在跑，却提示余额不足」，而且没有任何地方会报错。
//
// 做的事（SQL 全在 repository.ReapStaleJobs 里，一个事务三段）：
//
//	① status='pending' 但 attempt_count 已经用满的 item → 直接判死。
//	   领取 SQL 的 pending 支带 attempt 守卫，这类行永远领不出来，而心跳
//	   照样把它所在的批次算成「还有活」，于是批次永远躲过下面的 ② —— 卡死。
//	② status IN ('created','holding','running') 且 heartbeat_at 超过 10 分钟
//	   → 转 failed、last_error_code = DK_STALE、释放 hold。
//	③ status='settling' 卡过 60 分钟 → 按四个计数判真实终态、封账、释放 hold。
//	   这一段是**给「结算 worker 根本没起来」准备的**：worker 缺对象存储 /
//	   图像服务 / 出图网关任意一个就整个不启动，settlement.go 里那个 30 分钟的
//	   兜底也就一起没了，冻结额会永远占着用户可用额且没有任何报错。
//
// ②的界面文案是「系统重启导致这批任务中断了，可以重新提交。
// 已经出好的图都在作品库里。」（errors.go 里 DK_STALE 的文案）；
// ③不写批次级错误码 —— 出图本身没出问题，出问题的是结算。
func (p *Pool) runReaper(ctx context.Context) {
	defer p.wg.Done()

	ticker := time.NewTicker(p.cfg.ReapInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.reapOnce(ctx)
		}
	}
}

// reapOnce 跑一轮巡检。多副本互斥靠 advisory lock，抢不到就跳过这一轮。
func (p *Pool) reapOnce(ctx context.Context) {
	p.withAdvisoryLock(ctx, AdvisoryLockKeyReap, "reaper", func(runCtx context.Context) {
		ids, err := p.deps.Repo.ReapStaleJobs(runCtx, p.cfg.StaleAfter, p.cfg.ReapLimit)
		if err != nil {
			p.log.Warn("designkit 僵尸任务巡检失败", slog.Any("error", err))
			return
		}
		if len(ids) == 0 {
			return
		}
		// 这条日志级别是 Warn 而不是 Info：有批次被巡检收走就意味着**发生过一次
		// 非正常退出、或者结算根本没在跑**，而且很可能有运营正对着一个卡住的进度条。
		// 运维应该看得见。
		p.log.Warn("designkit 巡检收走了卡住的批次（已判终态并释放冻结额）",
			slog.Int("count", len(ids)),
			slog.Any("jobs", p.reapedSummary(runCtx, ids)),
			slog.Duration("stale_after", p.cfg.StaleAfter),
			slog.String("note", "落 failed + DK_STALE 的是心跳超时；落 succeeded / "+
				"partially_failed / cancelled 的是卡在结算里被兜底封账的，actual_cost 可能偏小，请人工核对"),
		)
	})
}

// reapedSummary 把回收结果整理成「job_id=最终状态」的形式，供上面那条日志用。
//
// 为什么要多查一遍：ReapStaleJobs 返回的是「心跳超时」和「结算卡死」两种命中的
// **并集**，签名（domain/port.go）里没有地方区分它们。而这两种的处置完全不同 ——
// 一种是任务真的中断了，一种是图都出好了只是账没算完 —— 运维必须能一眼分开。
//
// 只在真的收到了批次时才会跑（正常情况下永远不跑），条数上限就是 cfg.ReapLimit，
// 所以这几条额外查询不值得心疼。查不到就写 `?`，绝不让一条日志把巡检搞挂。
func (p *Pool) reapedSummary(ctx context.Context, ids []int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		job, err := p.deps.Repo.GetJobByID(ctx, id)
		if err != nil || job == nil {
			out = append(out, fmt.Sprintf("%d=?", id))
			continue
		}
		out = append(out, fmt.Sprintf("%d=%s", id, job.Status))
	}
	return out
}

// withAdvisoryLock 抢到锁才执行 fn。
//
// **一定要用数据库的锁，不能用应用内 mutex** —— 进程内的锁在另一台机器上根本不存在，
// 两个副本会同时结算同一个批次，而按张递减冻结额的动作跑两遍就会多减一次。
//
// Locker 为 nil 时**照常执行**并只在第一次留一行说明：
// 巡检那条 UPDATE 自带守卫，重复执行最多是空转；
// 但结算路径没有这层保护，多副本部署一定要把锁接上。
func (p *Pool) withAdvisoryLock(ctx context.Context, key int64, name string, fn func(context.Context)) {
	if p.deps.Locker == nil {
		fn(ctx)
		return
	}

	locked, unlock, err := p.deps.Locker.TryAdvisoryLock(ctx, key)
	if err != nil {
		p.log.Warn("designkit 抢咨询锁失败，本轮跳过",
			slog.String("task", name), slog.Int64("lock_key", key), slog.Any("error", err))
		return
	}
	if !locked {
		// 另一个副本正在跑同一件事，这是正常状态，不打日志（每分钟一条会刷屏）。
		return
	}
	if unlock != nil {
		defer unlock()
	}
	fn(ctx)
}
