package worker

import (
	"context"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ============================================================================
// 本包用到的持久化能力
// ============================================================================
//
// 这里的三个接口是 domain.Repository 的**子集**，方法签名逐字照抄，
// 一个字都没改（文件末尾有编译期断言把这件事钉死）。
//
// 为什么不直接用 domain.Repository：单测要塞假实现，
// 假实现没必要为了跑一个队列循环去实现灵感库同步和素材上传那几十个方法。

// QueueRepository 是出图队列那条路上用到的部分。
type QueueRepository interface {
	// GetJobByID 按内部主键取批次。worker 没有 userID 上下文，只能用这个。
	GetJobByID(ctx context.Context, id int64) (*dkdomain.Job, error)

	// UpdateJobStatus 带白名单和守卫的状态迁移。worker 只用它做 holding → running。
	UpdateJobStatus(ctx context.Context, jobID int64, from, to dkdomain.JobStatus) error

	// ClaimNextItem 领取下一张待办（FOR UPDATE SKIP LOCKED，attempt_count 在领取时就 +1）。
	// 没有可领的返回 domain.ErrNotFound。
	ClaimNextItem(ctx context.Context, params dkdomain.ClaimItemParams) (*dkdomain.JobItem, error)

	// RenewItemLease 续租约。租约已被别人抢走返回 domain.ErrLeaseLost。
	RenewItemLease(ctx context.Context, itemID int64, workerID string, leaseFor time.Duration) error

	// CompleteItemSuccess 写回成功结果。守卫没命中返回 domain.ErrLeaseLost。
	CompleteItemSuccess(ctx context.Context, params dkdomain.CompleteItemSuccessParams) error

	// CompleteItemFailure 写回失败结果（含退避重排）。守卫没命中返回 domain.ErrLeaseLost。
	CompleteItemFailure(ctx context.Context, params dkdomain.CompleteItemFailureParams) error

	// FinalizeJob 终态判定，只有影响行数 = 1 的那个 worker 拿到 won=true。
	FinalizeJob(ctx context.Context, params dkdomain.FinalizeJobParams) (won bool, err error)

	// TouchJobHeartbeat 给这些批次续心跳。
	TouchJobHeartbeat(ctx context.Context, jobIDs []int64) error

	// GetAssetsByIDs 取原图（预处理时要拿 object_key）。
	GetAssetsByIDs(ctx context.Context, userID int64, ids []int64) ([]*dkdomain.Asset, error)

	// GetVariant 查已有的预处理产物；没有返回 domain.ErrNotFound。
	GetVariant(ctx context.Context, assetID int64, ratio dkdomain.Ratio, keepTransparency bool, maxDimension int) (*dkdomain.AssetVariant, error)

	// UpsertVariant 写入预处理产物。
	UpsertVariant(ctx context.Context, params dkdomain.UpsertVariantParams) (*dkdomain.AssetVariant, error)

	// GetSetting 读 designkit 自己的配置（并发数、单张超时、最长边）。
	GetSetting(ctx context.Context, key string) (*dkdomain.Setting, error)
}

// RecoveryRepository 是僵尸巡检用到的部分。
type RecoveryRepository interface {
	// ReapStaleJobs 一个事务扫三样，返回被收走的 job id：
	//
	//	① status='pending' 但 attempt_count 已用满的 item → 判 failed 并补判一次收尾。
	//	   （领取 SQL 的 pending 支带 attempt 守卫，这类行永远领不出来，而心跳照样
	//	   把它所在的批次算成「还有活」→ 批次永远躲过②，冻结额永远挂着。）
	//	   **这一类不出现在返回值里**：它没把批次判死，只是把死结拆开。
	//	② 心跳过期的 created/holding/running 批次 → 转 failed（DK_STALE）、
	//	   停掉名下未完成的 item、释放 hold。
	//	③ 卡在 settling 超过 60 分钟的批次 → 按四个计数判真实终态、
	//	   用已回填的 billed_cost 封账、释放 hold。
	//	   给「结算 worker 根本没起来」准备的（缺存储/图像服务/网关就整个不启动）。
	//
	// ⚠ 返回值里②③是混在一起的，签名里没地方区分。要分辨得回读一次 status，
	// 见 reaper.go 的 reapedSummary。
	ReapStaleJobs(ctx context.Context, staleAfter time.Duration, limit int) ([]int64, error)
}

// SettlementRepository 是结算用到的部分。
type SettlementRepository interface {
	// ListJobsPendingSettlement 找出 status='settling' 还没结算完的批次。
	ListJobsPendingSettlement(ctx context.Context, limit int) ([]*dkdomain.Job, error)

	// ListJobItems 取一个批次的全部 item（已按 seq 升序）。
	ListJobItems(ctx context.Context, jobID int64) ([]*dkdomain.JobItem, error)

	// ListImagesByItem 取某一张的结果图（currentOnly=true 只给当前版本）。
	// 结算时用它定计费档：口径跟上游一致 —— 看**实际返回的像素**，
	// 一次多张时取最高那一档，而不是我们请求里写的 size。
	ListImagesByItem(ctx context.Context, itemID int64, currentOnly bool) ([]*dkdomain.Image, error)

	// SumUsageLogCosts 按 billing_request_id 去上游 usage_logs 反查实际扣费。
	// 查不到的 key 不出现在返回值里 —— 结算靠「命中条数 < 成功张数」判断账单还没写完。
	SumUsageLogCosts(ctx context.Context, billingRequestIDs []string) (map[string]dkdomain.Money, error)

	// ApplyItemBilling 回填某一张的实际扣费，并按张递减该 job 的 open 冻结额。
	ApplyItemBilling(ctx context.Context, itemID int64, cost dkdomain.Money, billingMode, billingTier string) error

	// SettleJob 写 actual_cost + 终态 + settled_at，并把剩余 open 冻结额置 released。
	SettleJob(ctx context.Context, params dkdomain.SettleJobParams) error

	// InsertBillingAlert 记一条「实际超预估」的告警。
	InsertBillingAlert(ctx context.Context, alert *dkdomain.BillingAlert) error
}

// Repository 是本包需要的全部持久化能力。domain.Repository 天然满足它。
type Repository interface {
	QueueRepository
	RecoveryRepository
	SettlementRepository
}

// 编译期断言：本包的接口必须是 domain.Repository 的子集。
// 谁改了 domain 里的签名而没同步这里，编译当场就炸 —— 这正是我们要的，
// 因为本机没有 Go 编译器，能在 CI 第一步就炸掉的错误是最便宜的错误。
var _ Repository = (dkdomain.Repository)(nil)

// AdvisoryLocker 是 PostgreSQL 的会话级咨询锁（pg_try_advisory_lock）。
//
// **僵尸巡检和结算必须靠它做多副本互斥，不能用应用内 mutex** ——
// 进程内的锁在另一台机器上根本不存在，两个副本会同时结算同一个批次，
// 而 ApplyItemBilling 会按张递减冻结额，跑两遍就把冻结额多减一次。
//
// ⚠ 实现注意（跟 domain.SyncRepository.TryLockSync 同一个坑）：
// advisory lock 是**会话级**的，实现必须从连接池里取一条连接一直握着，
// 在同一条连接上 pg_advisory_unlock，否则锁会跟着连接回池而失效。
//
// 允许为 nil：此时巡检和结算照常跑。ReapStaleJobs 那条 UPDATE 自带守卫，
// 多副本并发只会有一个真正命中；但结算路径没有这层保护，
// **单副本以外的部署一定要接上这个锁**。
type AdvisoryLocker interface {
	// TryAdvisoryLock 抢一把锁。抢不到返回 locked=false（不是错误）。
	// 抢到时返回的 unlock 必须被调用，否则锁一直握着。
	TryAdvisoryLock(ctx context.Context, key int64) (locked bool, unlock func(), err error)
}
