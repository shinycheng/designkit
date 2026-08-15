package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// 本包的默认值。**唯一真相仍然是 designkit_settings 里的值**，
// 这里只是读不到配置时的兜底（跟 domain 里的 Default* 常量口径一致）。
const (
	// DefaultIdleDelay 队列空了以后隔多久再问一次。
	// 2 秒是「运营点了提交、几乎立刻看到第一张开始跑」和「别把数据库问烂」的折中：
	// 4 个 worker × 每 2 秒一次 = 2 QPS，可以忽略不计。
	DefaultIdleDelay = 2 * time.Second

	// DefaultErrorDelay 领取时数据库报错后的退避。比空转久一点，
	// 免得数据库正抖的时候 4 个 worker 一起猛敲。
	DefaultErrorDelay = 5 * time.Second

	// DefaultWriteBackTimeout 写回结果的超时。
	// 写回**绝不能**用出图那个已经超时的 context —— 图出了、钱花了，
	// 写回却因为 context 过期没落库，这一张就成了无主的钱。
	DefaultWriteBackTimeout = 10 * time.Second

	// DefaultReapInterval 僵尸巡检间隔。
	DefaultReapInterval = time.Minute

	// DefaultReapLimit 一次巡检最多回收多少个批次（照抄上游 recovery 的 100）。
	DefaultReapLimit = 100

	// DefaultSettleInterval 结算 worker 的扫描间隔。
	DefaultSettleInterval = 10 * time.Second

	// DefaultSettleBatch 一轮最多结算多少个批次。
	DefaultSettleBatch = 20

	// DefaultSettleMaxAttempts 账单没写完时最多退避重试几次（设计定型 第五节）。
	DefaultSettleMaxAttempts = 5

	// DefaultSettleBackoffBase 结算退避基数：5s → 10s → 20s → 40s → 80s，合计约 2.5 分钟。
	// 上游写 usage_logs 是丢进 worker 池的异步动作，正常是毫秒级，
	// 给到 2.5 分钟已经是极其宽松的窗口。
	DefaultSettleBackoffBase = 5 * time.Second

	// DefaultSettleBackoffMax 单次退避上限。
	DefaultSettleBackoffMax = 2 * time.Minute

	// DefaultRetryBackoffBase 单张图失败后的退避基数：15s → 30s → 60s …
	DefaultRetryBackoffBase = 15 * time.Second

	// DefaultRetryBackoffMax 单张图退避上限。
	DefaultRetryBackoffMax = 5 * time.Minute

	// DefaultShutdownWait 收到停止信号后，最多等在途 worker 收摊多久。
	// 上游给整个进程的优雅关闭窗口只有 5 秒，所以这里必须小于 5 秒 ——
	// 我们要做的只是「把租约还回去」这一条 UPDATE，不是「把图跑完」。
	DefaultShutdownWait = 4 * time.Second

	// MaxConcurrency 并发上限的硬顶。
	// designkit 自己的连接池只开 16 条连接（db/pool.go），并发再高也没有意义；
	// 而且上游每用户默认只放 5 个并发，超出的会排队 25 个、等 30 秒，再多直接 429。
	MaxConcurrency = 16

	// SafeConcurrency 超过这个数就会开始跟运营的单张操作抢上游的并发格子。
	// 不阻止，但会在日志里说清楚。
	SafeConcurrency = 4
)

// advisory lock 的键。
//
// 取值刻意带上 9001（我们的迁移编号）做前缀，跟上游以及灵感库同步用的锁错开。
// 同一个 PostgreSQL 实例里 advisory lock 是全局的，撞键的后果是两个毫不相干的
// 后台任务互相饿死，而且不报错、只表现为「某个任务好像不跑了」。
const (
	// AdvisoryLockKeyReap 僵尸巡检的锁。
	AdvisoryLockKeyReap int64 = 9001001
	// AdvisoryLockKeySettle 结算的锁。
	AdvisoryLockKeySettle int64 = 9001002
)

// Config 是 Pool 的可调参数。零值可用：New 会补上全部默认值。
//
// 跟 designkit_settings 的关系：**并发数、单张超时、最长边三项以数据库为准**
// （Start 时读一次），这里的同名字段只有在数据库读不到时才生效。
// 其余字段是纯运行时参数，不进数据库 —— 它们改的是「怎么跑」，不是「跑什么」，
// 运营看不懂也不该改。
type Config struct {
	// Concurrency 出图并发数。> 0 时**覆盖** designkit_settings.worker_concurrency，
	// 一般只在测试里用。
	Concurrency int

	// ItemTimeout 单张图的超时。0 = 读 designkit_settings.item_timeout_seconds。
	ItemTimeout time.Duration

	// MaxDimension 预处理最长边。0 = 读 designkit_settings.max_dimension。
	MaxDimension int

	// KeepTransparency 预处理时是否保留透明底。默认 false = 合成白底。
	//
	// ⚠ 它是 variant 唯一键的一部分（asset_id, ratio, keep_transparency, max_dimension），
	// 改了这个值等于换一套预处理产物，老的不会被复用。
	KeepTransparency bool

	// LeaseFor 租约时长。默认 domain.DefaultLeaseSeconds（180 秒）。
	LeaseFor time.Duration

	// LeaseRenewInterval 续租间隔。默认 domain.DefaultLeaseRenewSeconds（60 秒）。
	LeaseRenewInterval time.Duration

	// HeartbeatInterval job 心跳间隔。默认 domain.DefaultHeartbeatSeconds（30 秒）。
	HeartbeatInterval time.Duration

	// StaleAfter 心跳超过多久判定为僵尸。默认 domain.DefaultStaleJobMinutes（10 分钟）。
	StaleAfter time.Duration

	// ReapInterval 僵尸巡检间隔。
	ReapInterval time.Duration

	// ReapLimit 一次巡检最多回收多少个批次。
	ReapLimit int

	// SettleInterval 结算扫描间隔。
	SettleInterval time.Duration

	// SettleBatch 一轮最多结算多少个批次。
	SettleBatch int

	// SettleMaxAttempts 账单没写完时最多退避重试几次。
	SettleMaxAttempts int

	// SettleBackoffBase 结算退避基数。
	SettleBackoffBase time.Duration

	// RetryBackoffBase 单张图失败后的退避基数。
	RetryBackoffBase time.Duration

	// IdleDelay 队列空转间隔。
	IdleDelay time.Duration

	// ErrorDelay 领取报错后的退避。
	ErrorDelay time.Duration

	// WriteBackTimeout 写回结果的超时。
	WriteBackTimeout time.Duration

	// ShutdownWait 停止时最多等多久。
	ShutdownWait time.Duration

	// DisableReaper 关掉僵尸巡检（只给测试和单机排障用，生产别关：
	// 关了之后进程被杀一次，那批任务的冻结额就永久占着用户的可用额）。
	DisableReaper bool

	// DisableSettlement 关掉结算 worker（同上，关了任务会永远停在 settling）。
	DisableSettlement bool
}

// Deps 是 Pool 的外部依赖。除了标注「可选」的，其余都必须给。
type Deps struct {
	// Repo 持久化。
	Repo Repository

	// Store 对象存储（原图、预处理产物、结果图都在这里）。
	Store dkdomain.ObjectStore

	// Preprocessor Python 预处理服务。**fail-closed**：它报错就把这一张判失败，
	// 绝不拿原图顶上去 —— 那会让运营选 3:4 拿回 4:3，钱照扣。
	Preprocessor dkdomain.ImagePreprocessor

	// Gateway 出图（进程内重入上游 Images()）。
	Gateway dkdomain.GatewayInvoker

	// Locker 可选：PostgreSQL advisory lock。多副本部署必须给。
	Locker AdvisoryLocker

	// ActiveJobIDs 可选：返回「还有活没干完」的批次 id，用于续心跳。
	//
	// ⚠ 为什么需要它：心跳默认只覆盖**本进程正在出图**的那些批次。
	// 但 4 个 worker 面对 3 个批次时，排在后面的批次一张也没开始跑，
	// 心跳就不会被续 —— 排队超过 10 分钟会被僵尸巡检当成崩溃残留判死，
	// 而它其实好好的。接上这个回调（实现就是一句
	// `SELECT DISTINCT job_id FROM designkit_job_items WHERE status IN ('pending','running')`）
	// 就没这个问题。留空只有在「同时只会有一个批次」时才安全。
	ActiveJobIDs func(ctx context.Context) ([]int64, error)

	// NewUID 可选：生成 26 位 ULID（结果图的 uid）。留空用本包自带的实现。
	NewUID func() string

	// Now 可选：取当前时间，测试用。留空用 time.Now。
	Now func() time.Time

	// Logger 可选：留空用 slog.Default()。
	Logger *slog.Logger
}

// ErrMissingDependency 少了必须的依赖。
var ErrMissingDependency = errors.New("designkit worker: 缺少必须的依赖")

func (c Config) withDefaults() Config {
	if c.LeaseFor <= 0 {
		c.LeaseFor = time.Duration(dkdomain.DefaultLeaseSeconds) * time.Second
	}
	if c.LeaseRenewInterval <= 0 {
		c.LeaseRenewInterval = time.Duration(dkdomain.DefaultLeaseRenewSeconds) * time.Second
	}
	// 续租间隔必须真的小于租约，否则每次都是「租约刚过期才去续」，
	// 等于没有租约保护，同一张图会被两个 worker 各出一遍（= 收两次钱）。
	if c.LeaseRenewInterval >= c.LeaseFor {
		c.LeaseRenewInterval = c.LeaseFor / 3
		if c.LeaseRenewInterval <= 0 {
			c.LeaseRenewInterval = time.Second
		}
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = time.Duration(dkdomain.DefaultHeartbeatSeconds) * time.Second
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = time.Duration(dkdomain.DefaultStaleJobMinutes) * time.Minute
	}
	if c.ReapInterval <= 0 {
		c.ReapInterval = DefaultReapInterval
	}
	if c.ReapLimit <= 0 {
		c.ReapLimit = DefaultReapLimit
	}
	if c.SettleInterval <= 0 {
		c.SettleInterval = DefaultSettleInterval
	}
	if c.SettleBatch <= 0 {
		c.SettleBatch = DefaultSettleBatch
	}
	if c.SettleMaxAttempts <= 0 {
		c.SettleMaxAttempts = DefaultSettleMaxAttempts
	}
	if c.SettleBackoffBase <= 0 {
		c.SettleBackoffBase = DefaultSettleBackoffBase
	}
	if c.RetryBackoffBase <= 0 {
		c.RetryBackoffBase = DefaultRetryBackoffBase
	}
	if c.IdleDelay <= 0 {
		c.IdleDelay = DefaultIdleDelay
	}
	if c.ErrorDelay <= 0 {
		c.ErrorDelay = DefaultErrorDelay
	}
	if c.WriteBackTimeout <= 0 {
		c.WriteBackTimeout = DefaultWriteBackTimeout
	}
	if c.ShutdownWait <= 0 {
		c.ShutdownWait = DefaultShutdownWait
	}
	return c
}

func (d Deps) validate() error {
	switch {
	case d.Repo == nil:
		return fmt.Errorf("%w: Repo（持久化）", ErrMissingDependency)
	case d.Store == nil:
		return fmt.Errorf("%w: Store（对象存储）", ErrMissingDependency)
	case d.Preprocessor == nil:
		return fmt.Errorf("%w: Preprocessor（图片预处理服务）", ErrMissingDependency)
	case d.Gateway == nil:
		return fmt.Errorf("%w: Gateway（出图网关）", ErrMissingDependency)
	}
	return nil
}

// Settings 是从 designkit_settings 读出来的那几项运行参数。
type Settings struct {
	// Concurrency 出图并发数。
	Concurrency int
	// ItemTimeout 单张图超时。
	ItemTimeout time.Duration
	// MaxDimension 预处理最长边（**每次调 Python 都要显式带上**）。
	MaxDimension int
}

// LoadSettings 从 designkit_settings 读并发数、单张超时、最长边。
//
// 读不到、读坏了都不报错 —— 一律退回默认值并留一行日志。
// 理由：这三项都是「跑得快慢」的参数，不是「钱怎么算」的参数，
// 为了一个配置项读不出来就让整个出图队列起不来，得不偿失。
func LoadSettings(ctx context.Context, repo QueueRepository, log *slog.Logger) Settings {
	if log == nil {
		log = slog.Default()
	}
	concurrency := readIntSetting(ctx, repo, log, dkdomain.SettingKeyWorkerConcurrency, dkdomain.DefaultWorkerConcurrency, 1, MaxConcurrency)
	timeoutSeconds := readIntSetting(ctx, repo, log, dkdomain.SettingKeyItemTimeoutSeconds, dkdomain.DefaultItemTimeoutSeconds, 10, 3600)
	maxDimension := readIntSetting(ctx, repo, log, dkdomain.SettingKeyMaxDimension, dkdomain.DefaultMaxDimension, 256, 8192)

	if concurrency > SafeConcurrency {
		log.Warn("designkit 出图并发数超过安全值，运营在界面上的单张操作可能要排队",
			slog.Int("worker_concurrency", concurrency),
			slog.Int("safe", SafeConcurrency),
			slog.String("why", "上游每用户默认只放 5 个并发，超出的排队最多 25 个、等 30 秒，再多直接 429"),
		)
	}

	return Settings{
		Concurrency:  concurrency,
		ItemTimeout:  time.Duration(timeoutSeconds) * time.Second,
		MaxDimension: maxDimension,
	}
}

// readIntSetting 读一个整数配置项，并把值收敛到 [lo, hi]。
// designkit_settings.value 是 JSONB，标量直接存成 `4` / `2048`。
func readIntSetting(ctx context.Context, repo QueueRepository, log *slog.Logger, key string, def, lo, hi int) int {
	if repo == nil {
		return def
	}
	setting, err := repo.GetSetting(ctx, key)
	if err != nil {
		if !errors.Is(err, dkdomain.ErrNotFound) {
			log.Warn("designkit 读配置失败，用默认值",
				slog.String("key", key), slog.Int("default", def), slog.Any("error", err))
		}
		return def
	}
	if setting == nil || len(setting.Value) == 0 {
		return def
	}
	var v int
	if err := json.Unmarshal(setting.Value, &v); err != nil {
		log.Warn("designkit 配置值不是整数，用默认值",
			slog.String("key", key), slog.String("value", string(setting.Value)),
			slog.Int("default", def), slog.Any("error", err))
		return def
	}
	if v < lo || v > hi {
		log.Warn("designkit 配置值超出允许范围，已收敛",
			slog.String("key", key), slog.Int("configured", v),
			slog.Int("min", lo), slog.Int("max", hi))
		if v < lo {
			return lo
		}
		return hi
	}
	return v
}
