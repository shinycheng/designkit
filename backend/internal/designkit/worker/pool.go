package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Pool 是 designkit 的后台跑批器：出图 worker + 僵尸巡检 + 结算。
//
// 一个进程起一个就够。New 之后调 Start，退出前调 Stop。
type Pool struct {
	cfg      Config
	deps     Deps
	log      *slog.Logger
	now      func() time.Time
	newUID   func() string
	settings Settings

	inflight *inflightJobs
	settler  *settler

	mu       sync.Mutex
	started  bool
	stopping bool
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopOnce sync.Once

	// stopDeadline 停机预算的截止时刻（真实时钟）。Stop 里记一次，之后只读。
	//
	// 存在的唯一理由：写回用的是「不受取消影响」的独立 context
	// （writeBackContext，默认 10 秒），而调用方给 Stop 的预算只有 3 秒
	// （designkit/module.go 的 workerStopBudget）。Stop 一超时返回，
	// module.Close 立刻关连接池，那条还在飞的 UPDATE 就撞上
	// "sql: database is closed" —— **租约反而没交还成**，在途的那几张要挂满
	// 180 秒等租约自然过期，运营看到的是「重启完还卡在生成中」。
	// 所以停机路径上写回的预算必须缩到剩余预算之内，见 writeBackBudget。
	stopDeadline time.Time
}

// minWriteBackTimeout 写回预算的下限。
//
// 停机预算已经花光时也要把那条 UPDATE 发出去 —— 发出去还有一半机会赶在连接池
// 关掉之前落库，不发就一定要等 180 秒租约自然过期。给一个很短的下限，快成快败。
const minWriteBackTimeout = 250 * time.Millisecond

// New 建一个 Pool。依赖不全时返回错误（包装 ErrMissingDependency）。
func New(cfg Config, deps Deps) (*Pool, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	newUID := deps.NewUID
	if newUID == nil {
		newUID = NewULID
	}

	p := &Pool{
		cfg:      cfg.withDefaults(),
		deps:     deps,
		log:      log.With(slog.String("component", "designkit.worker")),
		now:      now,
		newUID:   newUID,
		inflight: newInflightJobs(),
	}
	p.settler = newSettler(p)
	return p, nil
}

// Start 起后台 goroutine，**立即返回**（不阻塞）。
//
// 传进来的 ctx 只用于「启动阶段读配置」，不决定 Pool 的生命周期 ——
// Pool 活到 Stop 被调用为止。这是有意的：注册路由的那个 ctx 往往是请求级的，
// 拿它当后台任务的生命周期会让队列在第一次请求结束时就停掉。
func (p *Pool) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return errors.New("designkit worker: Pool 已经启动过了")
	}
	p.started = true
	p.mu.Unlock()

	p.settings = LoadSettings(ctx, p.deps.Repo, p.log)
	if p.cfg.Concurrency > 0 {
		p.settings.Concurrency = p.cfg.Concurrency
	}
	if p.cfg.ItemTimeout > 0 {
		p.settings.ItemTimeout = p.cfg.ItemTimeout
	}
	if p.cfg.MaxDimension > 0 {
		p.settings.MaxDimension = p.cfg.MaxDimension
	}

	// 后台任务的 context 只继承值、不继承取消，取消权交给 Stop。
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p.mu.Lock()
	p.cancel = cancel
	p.mu.Unlock()

	prefix := workerIDPrefix()
	for i := 0; i < p.settings.Concurrency; i++ {
		workerID := truncateWorkerID(prefix + "-" + strconv.Itoa(i))
		p.wg.Add(1)
		go p.runItemWorker(runCtx, workerID)
	}

	p.wg.Add(1)
	go p.runHeartbeat(runCtx)

	if !p.cfg.DisableReaper {
		p.wg.Add(1)
		go p.runReaper(runCtx)
	}
	if !p.cfg.DisableSettlement {
		p.wg.Add(1)
		go p.runSettlement(runCtx)
	}

	p.log.Info("designkit 出图队列已启动",
		slog.Int("concurrency", p.settings.Concurrency),
		slog.Duration("item_timeout", p.settings.ItemTimeout),
		slog.Int("max_dimension", p.settings.MaxDimension),
		slog.Duration("lease", p.cfg.LeaseFor),
		slog.Duration("lease_renew", p.cfg.LeaseRenewInterval),
		slog.Bool("reaper", !p.cfg.DisableReaper),
		slog.Bool("settlement", !p.cfg.DisableSettlement),
		slog.Bool("advisory_lock", p.deps.Locker != nil),
	)
	return nil
}

// Stop 停止领新 item、取消在途出图、把租约还回去，然后等 worker 收摊。
//
// **不要指望在途的图能做完。** 上游给整个进程的优雅关闭窗口只有 5 秒
// （cmd/server/main.go），而且那 5 秒只等 HTTP server，后台 goroutine 压根不在
// 等待范围内；一张图要 20 秒。所以这里做的是「把活还回队列」而不是「把活干完」：
// 在途 item 走 CompleteItemFailure(Terminal=false, RetryAfter=0)，
// 立刻退回 pending，另一个副本或重启后的自己会重新领走。
//
// 可以重复调用。ctx 到期时返回 ctx.Err()，但 goroutine 仍在后台自己收尾。
func (p *Pool) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.stopOnce.Do(func() {
		// 停机预算 = min(ShutdownWait, 调用方 ctx 的 deadline)，记成**绝对时刻**，
		// writeBackContext 才算得出「还剩多久」。
		// 实际生效的通常是调用方那一个：module.Close 给 3 秒，而 ShutdownWait 默认 4 秒。
		deadline := time.Now().Add(p.cfg.ShutdownWait)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}

		p.mu.Lock()
		p.stopping = true
		p.stopDeadline = deadline
		cancel := p.cancel
		p.mu.Unlock()

		p.log.Info("designkit 出图队列开始停止：不再领新任务，在途任务的租约会被交还",
			slog.Duration("budget", time.Until(deadline)))
		if cancel != nil {
			cancel()
		}
	})

	waitCtx, cancelWait := context.WithTimeout(ctx, p.cfg.ShutdownWait)
	defer cancelWait()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.log.Info("designkit 出图队列已停止")
		return nil
	case <-waitCtx.Done():
		// 没等到不是灾难：租约到期后别的 worker 会接手，最坏情况是这几张多等 3 分钟。
		p.log.Warn("designkit 出图队列没能在窗口内收摊，剩下的交给租约过期回收",
			slog.Duration("waited", p.cfg.ShutdownWait))
		return waitCtx.Err()
	}
}

// StopOnSignal 把 Stop 挂到 SIGINT / SIGTERM 上，进程内可多次调用但只装一次。
//
// signal.Notify 是按 channel 广播的，**不会**抢走 main.go 自己那一份信号。
// 之所以要自己听：上游 main.go 的关闭流程是 wire 生成的（wire_gen.go 不在
// 允许触碰的文件里），我们挂不进去。
func (p *Pool) StopOnSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		ctx, cancel := context.WithTimeout(context.Background(), p.cfg.ShutdownWait)
		defer cancel()
		if err := p.Stop(ctx); err != nil {
			p.log.Warn("designkit 出图队列停止时超时", slog.Any("error", err))
		}
	}()
}

func (p *Pool) isStopping() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopping
}

// writeBackContext 造一个「不受出图超时和停止信号影响」的 context。
//
// 写回必须用它：出图那个 context 可能已经超时或被取消，而这时候图已经出了、
// 钱已经花了 —— 用一个死掉的 context 去写回，结果就是钱花了、库里没记录，
// 结算永远等不到这一张的账单。
//
// **但「不受取消影响」不等于「可以慢慢来」**：停机时它必须缩进剩余预算内，
// 见 writeBackBudget。
func (p *Pool) writeBackContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), p.writeBackBudget())
}

// writeBackBudget 算这次写回能用多久。
//
// 平时就是 cfg.WriteBackTimeout（默认 10 秒）。
// 一旦进了停机流程，就压到 stopDeadline 之前 —— 原因见 stopDeadline 上的注释：
// 调用方（module.Close）只等 3 秒就去关连接池，10 秒的写回必然被
// "sql: database is closed" 打断，租约白等 180 秒。
//
// ⚠ 这里必须用 time.Now / time.Until，**不能用可注入的 p.now**：
// context 的超时走的是真实时钟，拿假时钟算出来的差值跟 context 对不上，
// 测试里会算出一个几十年的预算或者当场过期。
func (p *Pool) writeBackBudget() time.Duration {
	budget := p.cfg.WriteBackTimeout
	if budget <= 0 {
		budget = DefaultWriteBackTimeout
	}

	p.mu.Lock()
	deadline := p.stopDeadline
	p.mu.Unlock()
	if deadline.IsZero() {
		return budget
	}

	if remaining := time.Until(deadline); remaining < budget {
		budget = remaining
	}
	if budget < minWriteBackTimeout {
		budget = minWriteBackTimeout
	}
	return budget
}

// sleepCtx 睡一会儿，ctx 被取消就提前醒。返回 false 表示该退出了。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// workerIDPrefix 拼一个跨副本唯一的前缀：主机名 + 进程号。
// 多副本部署时两台机器的 worker_id 必须不同，否则 A 的写回守卫会误命中 B 的租约。
func workerIDPrefix() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("dk-%s-%d", host, os.Getpid())
}

// truncateWorkerID 把 worker_id 压进 VARCHAR(64)。
// 超长的后果是 PostgreSQL 抛 22001 让整条 UPDATE 失败 —— 一张图都领不出来。
func truncateWorkerID(id string) string {
	const maxWorkerIDLen = 64
	if len(id) <= maxWorkerIDLen {
		return id
	}
	// 保留尾部（尾部是序号，前缀里的主机名截掉一截仍然能认出是哪台机器）。
	return id[len(id)-maxWorkerIDLen:]
}

// ----------------------------------------------------------------------------
// 在途批次登记（给心跳用）
// ----------------------------------------------------------------------------

type inflightJobs struct {
	mu     sync.Mutex
	counts map[int64]int
}

func newInflightJobs() *inflightJobs {
	return &inflightJobs{counts: make(map[int64]int)}
}

func (i *inflightJobs) add(jobID int64) {
	if jobID <= 0 {
		return
	}
	i.mu.Lock()
	i.counts[jobID]++
	i.mu.Unlock()
}

func (i *inflightJobs) remove(jobID int64) {
	if jobID <= 0 {
		return
	}
	i.mu.Lock()
	if n := i.counts[jobID]; n <= 1 {
		delete(i.counts, jobID)
	} else {
		i.counts[jobID] = n - 1
	}
	i.mu.Unlock()
}

// ids 返回当前在途的批次 id，升序（顺序固定，测试才好断言）。
func (i *inflightJobs) ids() []int64 {
	i.mu.Lock()
	out := make([]int64, 0, len(i.counts))
	for id := range i.counts {
		out = append(out, id)
	}
	i.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}
