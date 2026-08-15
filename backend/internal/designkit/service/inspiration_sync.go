package service

// ============================================================================
// 灵感库同步
// ============================================================================
//
// 做的事：每 12 小时（或者管理员手动点一下）从 YouMind 开源库拉 1.4 万条提示词，
// 转换成本项目的格式，按 source_ref 幂等写进 designkit_prompts。
//
// 三条必须守住的规矩，改这个文件之前先看一遍：
//
// 【1】**手动同步和自动同步共用同一把锁**，而且是 PostgreSQL 的咨询锁
//
//	（pg_try_advisory_lock，实现在 repository/sync_setting_repo.go）。
//	不用 Go 侧的 mutex —— 多副本部署时进程内的锁根本不起作用。
//	老系统就是手动一把、自动一把，两路同时导入，把 1.4 万条写成了重复的两份，
//	是代码审查抓出来的 critical。
//	抢不到锁**不排队也不重试**：记一条 skipped 的同步记录，
//	给调用方一个 DK_SYNC_IN_PROGRESS（「灵感库正在同步，请等这一次跑完再点。」）。
//	这不是故障，是正常的并发保护。
//
// 【2】**代理只给同步用，绝不设成全局。**
//
//	老代码的注释原话：「不能用全局 urllib 代理或环境变量——那会把生图请求也带进代理」。
//	生图网关是局域网地址，走代理必然连不上。
//	Go 侧同理：**不碰 http.DefaultTransport，不设 HTTP_PROXY / HTTPS_PROXY 环境变量**，
//	只给这一个同步用的 http.Client 单独配代理（httpclient.GetClient）。
//
// 【3】**代理连不上就停下，绝不静默回退直连。**
//
//	运营以为走了代理、实际走的直连，比直接报错糟糕得多：
//	出口 IP 不是他以为的那个，而且没有任何提示。
//	上游的 httpclient.GetClient 本身也是这个约定（代理建不出来直接返错，不回退）。
//
// 【这套东西不挂在出图队列上】
//
//	module.go 的 newWorker() 在缺对象存储 / 图像处理服务 / 出图网关时**整个不启动**。
//	灵感库同步只需要数据库和一个 HTTP 客户端 —— 没配出图网关的机器也该能同步，
//	不然运营连提示词都浏览不了。所以这里自带一个 goroutine 定时器，
//	由 Module.Start 直接启动，跟出图队列没有依赖关系。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
)

const (
	// SettingKeyPromptSyncProxyID 同步走哪个代理，存在 designkit_settings。
	//
	// 值的形态（列是 JSONB）：
	//   null → 不走代理，直连
	//   3    → 走 id=3 的那个代理（就是「IP管理」里添加的那些）
	//
	// 为什么存 id 而不是存代理地址：代理地址里带账号密码，落到我们自己的表里
	// 等于多存了一份凭据；存 id 的话密码始终只在上游的代理表里。
	//
	// ⚠ **这个字面量在两个包里各写了一份**：另一份是
	// internal/designkit/module.go 的 SettingKeyPromptSyncProxyID
	// （管理界面就是通过它把管理员选的代理写进这张表的）。
	// 两边不一致的后果最难查：界面上显示「已选代理 3」，同步却读不到、
	// 老老实实走直连 —— 正是「以为走了代理其实没走」这个最糟的状态。
	// 之所以不合并成一个常量：service 不能 import 上层的 designkit 包
	// （那个包 import handler、handler import service，反过来就是循环依赖）。
	// inspiration_sync_test.go 里有一条测试直接去读 module.go 的源码比对，
	// 谁改了一边而没改另一边，测试当场就会红。
	SettingKeyPromptSyncProxyID = "prompt_sync_proxy_id"

	// DefaultInspirationSyncInterval 自动同步间隔。上游每天更新两次，12 小时够了。
	DefaultInspirationSyncInterval = 12 * time.Hour

	// inspirationStartupDelay 进程起来后隔多久做第一次「补同步」。
	// 不立刻跑：启动那几十秒机器最忙（上游网关自己也在初始化）。
	inspirationStartupDelay = 60 * time.Second

	// inspirationSyncTimeout 单轮同步的总超时。正常十几秒，
	// 给到 15 分钟是为了兜住「代理很慢但能通」的情况。
	inspirationSyncTimeout = 15 * time.Minute

	// inspirationHTTPTimeout 单个文件的下载超时。最大的分类文件有 20MB。
	inspirationHTTPTimeout = 120 * time.Second

	// inspirationUpsertChunk 每多少条提交一次。
	//
	// 为什么要分批：repository 的 UpsertSyncedPrompts 是「一个事务里逐条 upsert」。
	// 1.4 万条塞进一个事务，就是一个持续十几秒的长事务，
	// 期间的 WAL 和锁都压在一次提交上。分成 500 条一批，每批一个短事务，
	// 中途还能响应取消（进程要退出时不用干等）。
	inspirationUpsertChunk = 500

	// inspirationCloseBudget 进程退出时等同步收尾的时间。
	// 等的是「把同步记录写成 failed + 把咨询锁还掉」，不是等这一轮跑完。
	inspirationCloseBudget = 3 * time.Second

	// maxSyncErrorRunes 写进 designkit_sync_runs.error 的长度上限。
	maxSyncErrorRunes = 800
)

// ----------------------------------------------------------------------------
// 依赖
// ----------------------------------------------------------------------------

// ProxyResolver 按 id 取代理地址。
//
// 上游的 *service.ProxyService 天然满足这个接口（GetURL(ctx, id) (string, error)），
// 装配时直接传它即可。这里只声明用得上的那一个方法，
// 好处是单元测试塞个五行的假实现就行，不用把整个上游服务拖进来。
type ProxyResolver interface {
	GetURL(ctx context.Context, id int64) (string, error)

	// IsActive 这个代理现在还可用吗（没被删、没停用、没过期）。
	//
	// ⚠ 为什么必须有这个：上游 ProxyService.GetURL 走的是 GetByID，
	// **不过滤 status**。代理过期或被停用之后它照样返回一个能用的地址，
	// 于是会出现「设置页显示直连、同步实际还在走那个停用的代理」——
	// 正是这一轮最该避免的那类「界面说 A、实际做 B」。
	// 实现放在 module.go（那里能拿到上游的 ListActive）。
	IsActive(ctx context.Context, id int64) (bool, error)
}

// inspirationPromptStore 同步要用到的提示词仓储能力（dkdomain.Repository 的子集）。
//
// 只声明用得上的两个方法：单测塞假实现时不用把十来个方法全实现一遍。
// 「灵感库现在有多少条」「最近一次同步怎么样」那些读接口不在这里 ——
// 那是 module.go 的 promptSyncAdapter 直接问 repository 的事。
type inspirationPromptStore interface {
	UpsertCategory(ctx context.Context, category *dkdomain.PromptCategory) (*dkdomain.PromptCategory, error)
	UpsertSyncedPrompts(ctx context.Context, rows []dkdomain.SyncPromptRow) (*dkdomain.SyncPromptsResult, error)
}

// inspirationSyncStore 同步的锁与记录（dkdomain.SyncRepository 的全部）。
type inspirationSyncStore interface {
	TryLockSync(ctx context.Context) (bool, func(), error)
	StartSyncRun(ctx context.Context, kind dkdomain.SyncKind) (*dkdomain.SyncRun, error)
	FinishSyncRun(ctx context.Context, run *dkdomain.SyncRun) error
	LatestSyncRun(ctx context.Context) (*dkdomain.SyncRun, error)
}

// inspirationSettingStore 读 designkit_settings。
//
// **只读**：这一项由管理界面（module.go 的 promptSyncAdapter.PutSyncSettings）写，
// 同步器只负责按它办事。两个地方都能写的话，迟早出现「谁覆盖了谁」的悬案。
type inspirationSettingStore interface {
	GetSetting(ctx context.Context, key string) (*dkdomain.Setting, error)
}

// InspirationSyncDeps 建同步服务要的依赖。
type InspirationSyncDeps struct {
	// Prompts 提示词仓储，必填。
	Prompts inspirationPromptStore
	// Sync 同步锁与记录，必填。
	Sync inspirationSyncStore
	// Settings 配置仓储，必填（代理选择存在这里）。
	Settings inspirationSettingStore

	// Proxies 代理地址解析器（上游 *service.ProxyService）。
	//
	// 允许为 nil：拿不到它时**只能直连**，而且一旦管理员选了代理就直接报错，
	// 绝不假装走了代理。
	Proxies ProxyResolver

	// Fetcher 数据源。为 nil 用 NewYouMindFetcher("")。测试塞假的。
	Fetcher InspirationFetcher

	// Interval 自动同步间隔。<=0 用 DefaultInspirationSyncInterval。
	Interval time.Duration

	// StartupDelay 启动后多久做第一次补同步。<=0 用 inspirationStartupDelay。
	StartupDelay time.Duration

	// Now 取当前时间，测试可以塞固定值。为 nil 用 time.Now。
	Now func() time.Time

	// NewUID 生成 26 位 ULID。为 nil 用本包内置实现（跟素材、批次同一个）。
	NewUID func() string
}

// InspirationSyncService 灵感库同步。
type InspirationSyncService struct {
	prompts  inspirationPromptStore
	runs     inspirationSyncStore
	settings inspirationSettingStore
	proxies  ProxyResolver
	fetcher  InspirationFetcher

	interval     time.Duration
	startupDelay time.Duration
	now          func() time.Time
	newUID       func() string

	mu sync.Mutex
	// baseCtx 后台定时器的生命周期（Start 建、Close 取消）。
	// 刻意存在结构体里：它**不是**请求级 ctx，不能由调用方传进来
	// （传请求 ctx 的后果是定时器在第一次请求结束时就停了）。
	baseCtx context.Context
	cancel  context.CancelFunc
	started bool
	closed  bool
	wg      sync.WaitGroup
}

// NewInspirationSyncService 建同步服务。缺必填依赖直接报错，不留到运行时 nil panic。
func NewInspirationSyncService(deps InspirationSyncDeps) (*InspirationSyncService, error) {
	if deps.Prompts == nil {
		return nil, errors.New("designkit: 灵感库同步缺少提示词仓储")
	}
	if deps.Sync == nil {
		return nil, errors.New("designkit: 灵感库同步缺少同步记录仓储")
	}
	if deps.Settings == nil {
		return nil, errors.New("designkit: 灵感库同步缺少配置仓储")
	}

	fetcher := deps.Fetcher
	if fetcher == nil {
		fetcher = NewYouMindFetcher("")
	}
	interval := deps.Interval
	if interval <= 0 {
		interval = DefaultInspirationSyncInterval
	}
	startupDelay := deps.StartupDelay
	if startupDelay <= 0 {
		startupDelay = inspirationStartupDelay
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	newUID := deps.NewUID
	if newUID == nil {
		newUID = newAssetULID
	}

	return &InspirationSyncService{
		prompts:      deps.Prompts,
		runs:         deps.Sync,
		settings:     deps.Settings,
		proxies:      deps.Proxies,
		fetcher:      fetcher,
		interval:     interval,
		startupDelay: startupDelay,
		now:          now,
		newUID:       newUID,
	}, nil
}

// ----------------------------------------------------------------------------
// 代理选择（读 designkit_settings 的 prompt_sync_proxy_id，写在管理界面那边）
// ----------------------------------------------------------------------------

// ProxyID 读「同步走哪个代理」。返回 nil 表示直连。
//
// 值读不出来（配置项还没建过）当直连；值存在但不是数字也不是 null 时**报错**，
// 不当成直连 —— 静默直连正是这一节要避免的事。
func (s *InspirationSyncService) ProxyID(ctx context.Context) (*int64, error) {
	setting, err := s.settings.GetSetting(ctx, SettingKeyPromptSyncProxyID)
	if err != nil {
		if errors.Is(err, dkdomain.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if setting == nil || len(setting.Value) == 0 {
		return nil, nil
	}
	raw := strings.TrimSpace(string(setting.Value))
	if raw == "" || raw == "null" {
		return nil, nil
	}
	var id int64
	if err := json.Unmarshal([]byte(raw), &id); err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("同步代理这一项的值不对，请到设置里重新选一次代理（或者选「不使用代理」）。").
			WithCause(err)
	}
	if id <= 0 {
		return nil, nil
	}
	return &id, nil
}

// proxyURL 按 id 取代理地址并校验。
//
// **错误信息里绝不带代理地址原文** —— 那里面有账号密码。只带 id。
func (s *InspirationSyncService) proxyURL(ctx context.Context, id int64) (string, error) {
	if s.proxies == nil {
		return "", dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithMessage("系统没能读到代理列表，暂时只能直连同步。请联系管理员。")
	}
	// 先问「还能用吗」再取地址。顺序不能反：GetURL 对停用的代理照样返回地址，
	// 拿到就用的话，同步会静默走一个已经停用的代理。
	active, err := s.proxies.IsActive(ctx, id)
	if err != nil {
		return "", dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithMessage("没能确认代理是否可用，同步已经停下，没有改成直连。请稍后再试。").
			WithCause(err)
	}
	if !active {
		return "", dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessagef("选中的代理（编号 %d）已经停用或被删除，请到设置里重新选一个。"+
				"同步已经停下，没有改成直连。", id)
	}

	raw, err := s.proxies.GetURL(ctx, id)
	if err != nil {
		return "", dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessagef("选中的代理（编号 %d）已经不在了，请到「IP管理」里重新选一个。", id).
			WithCause(err)
	}
	// proxyurl.Parse 是上游统一的校验入口：协议白名单、socks5 自动升级成 socks5h。
	// 它的错误里用的是 Redacted()，但我们仍然不往界面上抛原文。
	trimmed, _, err := proxyurl.Parse(raw)
	if err != nil || trimmed == "" {
		return "", dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessagef("选中的代理（编号 %d）地址填得不对，请到「IP管理」里检查。", id).
			WithCause(err)
	}
	return trimmed, nil
}

// syncHTTPClient 建**同步专用**的 HTTP 客户端。
//
// ⚠ 这是本文件最要紧的一段。它只影响这一个 client：
//   - 不改 http.DefaultTransport
//   - 不设 HTTP_PROXY / HTTPS_PROXY 环境变量
//   - 不改任何全局状态
//
// 因为生图请求走的是局域网地址的自建网关，一旦被代理带走就必然连不上。
//
// 第二个返回值是给日志和界面用的说明（「直连」/「经代理 3」），
// **不含代理地址**，避免账号密码进日志。
func (s *InspirationSyncService) syncHTTPClient(ctx context.Context) (*http.Client, string, error) {
	proxyID, err := s.ProxyID(ctx)
	if err != nil {
		return nil, "", err
	}

	opts := httpclient.Options{Timeout: inspirationHTTPTimeout}
	via := "直连"
	if proxyID != nil {
		url, proxyErr := s.proxyURL(ctx, *proxyID)
		if proxyErr != nil {
			return nil, "", proxyErr
		}
		opts.ProxyURL = url
		via = fmt.Sprintf("经代理（编号 %d）", *proxyID)
	}

	client, err := httpclient.GetClient(opts)
	if err != nil {
		if proxyID != nil {
			// **绝不回退直连。** 运营以为走了代理、实际走直连，
			// 比直接报错糟糕得多。
			return nil, "", dkdomain.NewError(dkdomain.ErrCodeInternal).
				WithMessage("代理连不上，请到「IP管理」里检查这个代理。同步已经停下，没有改成直连。").
				WithCause(err)
		}
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithMessage("建立网络连接失败，请稍后重试。").
			WithCause(err)
	}
	return client, via, nil
}

// ProbeUpstream 测试「能不能连上上游提示词库」，只拉一个很小的文件，几秒出结果。
// 给设置页的「测试连接」按钮用。返回一句可以直接显示给管理员看的中文。
func (s *InspirationSyncService) ProbeUpstream(ctx context.Context) (string, error) {
	client, via, err := s.syncHTTPClient(ctx)
	if err != nil {
		return "", err
	}
	updatedAt, err := s.fetcher.Probe(ctx, client)
	if err != nil {
		return "", s.networkError(ctx, err)
	}
	return fmt.Sprintf("连接正常（%s），上游最近更新时间：%s", via, updatedAt), nil
}

// networkError 把下载失败翻译成运营看得懂的中文。
//
// 配了代理时**一律指向代理**：绝大多数下载失败都是代理的问题，
// 而且这时候最需要强调的是「我们没有偷偷改成直连」。
func (s *InspirationSyncService) networkError(ctx context.Context, cause error) error {
	proxyID, err := s.ProxyID(ctx)
	if err == nil && proxyID != nil {
		return dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithMessage("代理连不上，请到「IP管理」里检查这个代理。同步已经停下，没有改成直连。").
			WithUpstream(cause.Error()).
			WithCause(cause)
	}
	return dkdomain.NewError(dkdomain.ErrCodeInternal).
		WithMessage("连不上提示词库（这个库在境外）。如果服务器访问不了外网，" +
			"可以在灵感库设置里选一个代理再同步。").
		WithUpstream(cause.Error()).
		WithCause(cause)
}

// ----------------------------------------------------------------------------
// 一轮同步
// ----------------------------------------------------------------------------

// SyncSkippedMessage 抢不到锁时记进同步记录的说明。
//
// 界面上显示的是 DK_SYNC_IN_PROGRESS 那句「灵感库正在同步，请等这一次跑完再点。」，
// 这一句只落在 designkit_sync_runs.error 里，供管理员回头看历史。
const SyncSkippedMessage = "已经有一次同步在跑，这一次跳过了（不用重复点）。"

// StartSync 开一次同步，**立刻返回**那条已经建好的 running 记录，真正的活在后台跑。
//
// 这是 internal/designkit/module.go 里 PromptSyncRunner 接口要的那个方法，
// 方法名和签名**必须跟它一致**（那边 import 我们，我们不能 import 那边 ——
// 会形成 service ← handler ← designkit 的循环依赖，所以只能靠这条注释约束）。
//
// 为什么不能同步等：一轮一万四千条要十几秒，HTTP 请求挂着等，
// 中间任何一个反代超时都会让管理员看到「失败」，然后他会再点一次。
//
// 抢不到锁时返回 DK_SYNC_IN_PROGRESS（「灵感库正在同步，请等这一次跑完再点。」），
// 同时记一条 status='skipped' 的记录 —— 这不是故障，是正常的并发保护：
// **不排队、不重试**，让管理员过一会儿再看状态就行。
func (s *InspirationSyncService) StartSync(ctx context.Context, kind dkdomain.SyncKind) (*dkdomain.SyncRun, error) {
	if kind == "" {
		kind = dkdomain.SyncKindManual
	}
	// 进程正在退出时不要再开新的一轮：既没意义（马上会被取消），
	// 也会让 Close 那边的等待计数出问题。
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithMessage("服务正在重启，请过一会儿再点一次同步。")
	}

	// 抢锁这一步是同步做的（毫秒级），这样调用方立刻就知道「抢到没有」。
	locked, unlock, err := s.runs.TryLockSync(ctx)
	if err != nil {
		return nil, err
	}
	if !locked {
		s.recordSkipped(ctx, kind)
		return nil, dkdomain.NewError(dkdomain.ErrCodeSyncInProgress)
	}

	run, err := s.runs.StartSyncRun(ctx, kind)
	if err != nil {
		unlock()
		return nil, err
	}

	// 后台跑。**不能用调用方的 ctx**：手动同步时它是请求级的，
	// 响应一发出去就被取消，同步会在第二秒死掉。
	workCtx, cancel := s.workContext()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer unlock()
		s.execute(workCtx, run, kind)
	}()

	// 返回**副本**：run 这个对象归后台那个 goroutine 所有，它会一路改写状态和计数。
	// 把同一个指针交给调用方，两边就会同时读写同一块内存（数据竞争）。
	snapshot := *run
	return &snapshot, nil
}

// workContext 给后台同步一个独立的 ctx：进程退出时能被取消，请求结束时不会。
func (s *InspirationSyncService) workContext() (context.Context, context.CancelFunc) {
	s.mu.Lock()
	parent := s.baseCtx
	s.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, inspirationSyncTimeout)
}

// recordSkipped 记一条「没抢到锁」的同步记录。
//
// 为什么要记：不记的话历史里完全看不出「12 点那次自动同步被跳过了」，
// 排查「灵感库怎么两天没更新」时无从下手。
//
// 记不进去也只是警告 —— 调用方要的是 DK_SYNC_IN_PROGRESS 那句提示，
// 不能因为记录写不进去就把它变成一个「系统内部错误」。
func (s *InspirationSyncService) recordSkipped(ctx context.Context, kind dkdomain.SyncKind) {
	run, err := s.runs.StartSyncRun(ctx, kind)
	if err != nil || run == nil {
		slog.Warn("designkit 灵感库同步：没抢到锁，跳过这一次（这条记录没写进去）",
			slog.Any("error", err))
		return
	}
	finished := s.now()
	msg := SyncSkippedMessage
	run.Status = dkdomain.SyncStatusSkipped
	run.FinishedAt = &finished
	run.Error = &msg
	if err := s.runs.FinishSyncRun(ctx, run); err != nil {
		slog.Warn("designkit 灵感库同步：没抢到锁，但这条记录没写进去",
			slog.Any("error", err))
	}
}

// execute 真正跑一轮：下载 → 建分类 → 分批入库 → 写同步记录。
//
// 它不返回 error：调用方是 goroutine，错误的唯一去处就是同步记录和日志。
func (s *InspirationSyncService) execute(ctx context.Context, run *dkdomain.SyncRun, kind dkdomain.SyncKind) {
	started := s.now()
	err := s.doSync(ctx, run)
	finished := s.now()
	run.FinishedAt = &finished

	if err != nil {
		run.Status = dkdomain.SyncStatusFailed
		msg := truncateRunes(humanSyncError(err), maxSyncErrorRunes)
		run.Error = &msg
		slog.Error("designkit 灵感库同步失败",
			slog.String("kind", string(kind)),
			slog.Duration("elapsed", finished.Sub(started)),
			slog.Any("error", err))
	} else {
		run.Status = dkdomain.SyncStatusSucceeded
		run.Error = nil
		slog.Info("designkit 灵感库同步完成",
			slog.String("kind", string(kind)),
			slog.Duration("elapsed", finished.Sub(started)),
			slog.Int("fetched", run.Fetched),
			slog.Int("inserted", run.Inserted),
			slog.Int("updated", run.Updated),
			slog.Int("skipped", run.Skipped))
	}

	// 收尾用独立 ctx：上面那个可能已经因为超时或进程退出被取消了，
	// 拿它去写「这轮失败了」必然也失败，同步记录会永远卡在 running。
	finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.runs.FinishSyncRun(finishCtx, run); err != nil {
		slog.Error("designkit 灵感库同步：结果没能写进同步记录，界面上会一直显示「正在同步」",
			slog.Any("error", err))
	}
}

// doSync 下载 + 入库。计数直接写进 run。
func (s *InspirationSyncService) doSync(ctx context.Context, run *dkdomain.SyncRun) error {
	client, via, err := s.syncHTTPClient(ctx)
	if err != nil {
		return err
	}
	slog.Info("designkit 灵感库开始同步", slog.String("via", via))

	snapshot, err := s.fetcher.Fetch(ctx, client)
	if err != nil {
		return s.networkError(ctx, err)
	}
	if snapshot == nil {
		return errors.New("上游没有返回任何数据，这一轮不写库")
	}
	run.Fetched = len(snapshot.Prompts)

	// 分类要先建：repository 的 upsert 按 slug 找分类 id，找不到就把
	// category_id 写成空，运营按分类什么都翻不到。
	if err := s.ensureCategories(ctx); err != nil {
		return err
	}

	rows := BuildSyncRows(snapshot.Prompts, s.newUID)
	for start := 0; start < len(rows); start += inspirationUpsertChunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := start + inspirationUpsertChunk
		if end > len(rows) {
			end = len(rows)
		}
		result, err := s.prompts.UpsertSyncedPrompts(ctx, rows[start:end])
		if err != nil {
			return err
		}
		if result == nil {
			continue
		}
		run.Inserted += result.Inserted
		run.Updated += result.Updated
		run.Skipped += result.Skipped
	}
	return nil
}

// ensureCategories 确保 11 个分类都在。按 slug 幂等，中文名改了也不会多出一条。
func (s *InspirationSyncService) ensureCategories(ctx context.Context) error {
	for i, category := range inspirationCategories {
		_, err := s.prompts.UpsertCategory(ctx, &dkdomain.PromptCategory{
			Slug:   category.Slug,
			NameZH: category.NameZH,
			NameEN: category.NameEN,
			// 留出间隔，以后想在中间插一个分类不用重排全部。
			SortOrder: (i + 1) * 10,
		})
		if err != nil {
			return fmt.Errorf("建灵感库分类「%s」失败：%w", category.NameZH, err)
		}
	}
	return nil
}

// humanSyncError 取出能直接显示给运营看的中文。
// 不是我们自己造的错误（比如数据库驱动报的）时，退回一句笼统但不吓人的话。
func humanSyncError(err error) string {
	if dkErr, ok := dkdomain.AsDesignkitError(err); ok && dkErr.Message != "" {
		return dkErr.Message
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "同步用时太久，已经中断。网络慢的话可以稍后再试一次。"
	}
	if errors.Is(err, context.Canceled) {
		return "同步被中断了（多半是服务重启），稍后会自动再跑一次。"
	}
	return "同步没能完成：" + collapseSpaces(err.Error())
}

// ----------------------------------------------------------------------------
// 定时器
// ----------------------------------------------------------------------------

// Start 启动每 12 小时一次的自动同步。
//
// 传进来的 ctx **只用于启动阶段**，不决定定时器的生命周期 ——
// 装配时手上那个 ctx 往往是请求级的，拿它当后台任务的生命周期，
// 定时器会在第一次请求结束时就停掉（module.go 的 Start 也是这个约定）。
// 定时器活到 Close 为止。
//
// 可以重复调用，第二次起是空操作。
func (s *InspirationSyncService) Start(_ context.Context) error {
	s.mu.Lock()
	if s.started || s.closed {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	ctx, cancel := context.WithCancel(context.Background())
	s.baseCtx = ctx
	s.cancel = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(ctx)
	}()
	slog.Info("designkit 灵感库自动同步已启动",
		slog.Duration("interval", s.interval))
	return nil
}

// loop 定时器主体。
func (s *InspirationSyncService) loop(ctx context.Context) {
	// 先做一次「补同步」：新装的机器灵感库是空的，等 12 小时太久；
	// 群晖半夜断电重启一次，也不该让运营第二天早上看到空的灵感库。
	first := time.NewTimer(s.startupDelay)
	select {
	case <-ctx.Done():
		first.Stop()
		return
	case <-first.C:
		if s.needsCatchUp(ctx) {
			s.runAuto(ctx)
		}
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runAuto(ctx)
		}
	}
}

// runAuto 跑一轮自动同步。
//
// 抢不到锁是正常情况（管理员正好在手动同步，或者另一个副本在跑）：
// StartSync 会记一条 skipped 记录并返回 DK_SYNC_IN_PROGRESS，
// 这里**不重试也不报警**，等下一个 12 小时就是了。
func (s *InspirationSyncService) runAuto(ctx context.Context) {
	_, err := s.StartSync(ctx, dkdomain.SyncKindAuto)
	if err == nil {
		return
	}
	if dkErr, ok := dkdomain.AsDesignkitError(err); ok && dkErr.Code == dkdomain.ErrCodeSyncInProgress {
		slog.Info("designkit 灵感库自动同步跳过：已经有一次在跑")
		return
	}
	slog.Error("designkit 灵感库自动同步没能开始", slog.Any("error", err))
}

// needsCatchUp 判断「是不是该补一次同步」。
//
// 三种情况要补：从来没同步过、上一次不是成功、上一次成功已经超过一个间隔。
// 判断错了也不严重 —— 咨询锁保证不会重复导入，最坏就是多跑一轮十几秒的同步。
func (s *InspirationSyncService) needsCatchUp(ctx context.Context) bool {
	run, err := s.runs.LatestSyncRun(ctx)
	if err != nil {
		if !errors.Is(err, dkdomain.ErrNotFound) {
			slog.Warn("designkit 读不到上次同步记录，先补跑一次", slog.Any("error", err))
		}
		return true
	}
	if run == nil || run.Status != dkdomain.SyncStatusSucceeded {
		return true
	}
	at := run.StartedAt
	if run.FinishedAt != nil {
		at = *run.FinishedAt
	}
	return s.now().Sub(at) >= s.interval
}

// Close 停定时器，并给在途的那一轮同步一点时间收尾
// （把同步记录写成失败、把数据库咨询锁还掉）。可以重复调用。
//
// 不等这一轮跑完：下载一半的数据本来就不入库，等它没有意义。
func (s *InspirationSyncService) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cancel := s.cancel
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(inspirationCloseBudget):
		slog.Warn("designkit 灵感库同步没能在窗口内收尾，" +
			"数据库咨询锁会等连接断开后自动释放")
	}
	return nil
}
