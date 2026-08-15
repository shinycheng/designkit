package worker

import (
	"context"
	"log/slog"
	"sort"
	"time"
)

// runHeartbeat 每 30 秒给「还活着的批次」续一次心跳。
//
// 心跳是崩溃恢复的唯一依据：巡检扫的就是
// `status IN ('holding','running') AND heartbeat_at < NOW() - 10 分钟`。
// 心跳停了 10 分钟，那批任务就会被判定成「进程被杀留下的残骸」，
// 转 failed 并把冻结额还给用户。
//
// ⚠ **心跳覆盖不到的批次会被误杀。**
// 默认只覆盖本进程正在出图的批次。4 个 worker 面对 3 个批次时，
// 排在后面的批次一张也没开始跑 —— 排队超过 10 分钟就会被判死，而它其实好好的。
// 解决办法是给 Deps.ActiveJobIDs 接上「还有 pending/running item 的批次」查询，
// 见 config.go 上的说明。留空只有在「同时只会有一个批次」时才安全。
func (p *Pool) runHeartbeat(ctx context.Context) {
	defer p.wg.Done()

	ticker := time.NewTicker(p.cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ids := p.heartbeatTargets(ctx)
			if len(ids) == 0 {
				continue
			}
			p.touchHeartbeat(ctx, ids)
		}
	}
}

// heartbeatTargets 收集这一轮要续心跳的批次 id：
// 本进程在途的 + （如果接了回调）队列里还有活没干完的。
func (p *Pool) heartbeatTargets(ctx context.Context) []int64 {
	ids := p.inflight.ids()

	if p.deps.ActiveJobIDs != nil {
		active, err := p.deps.ActiveJobIDs(ctx)
		if err != nil {
			// 查不到就只续在途的。少续几个的后果是排队中的批次可能被误判成僵尸，
			// 所以这条日志是 Warn 不是 Debug。
			p.log.Warn("designkit 查询待办批次失败，本轮心跳只覆盖在途任务", slog.Any("error", err))
		} else {
			ids = mergeIDs(ids, active)
		}
	}
	return ids
}

// touchHeartbeat 续心跳。失败只记日志 —— 单次失败不致命，
// 连续失败 20 次（10 分钟）才会真的被巡检判死。
func (p *Pool) touchHeartbeat(ctx context.Context, ids []int64) {
	if len(ids) == 0 {
		return
	}
	touchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.cfg.WriteBackTimeout)
	defer cancel()

	if err := p.deps.Repo.TouchJobHeartbeat(touchCtx, ids); err != nil {
		p.log.Warn("designkit 续任务心跳失败",
			slog.Int("jobs", len(ids)), slog.Any("error", err))
	}
}

// mergeIDs 合并两组 id，去重并升序（顺序固定，测试才好断言）。
func mergeIDs(a, b []int64) []int64 {
	seen := make(map[int64]struct{}, len(a)+len(b))
	out := make([]int64, 0, len(a)+len(b))
	for _, group := range [][]int64{a, b} {
		for _, id := range group {
			if id <= 0 {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
