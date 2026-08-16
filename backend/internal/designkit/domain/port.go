package domain

import (
	"context"
	"errors"
	"time"
)

// ============================================================================
// 出站接口（端口）
// ============================================================================
//
// 本文件**只定义接口和参数结构体，不写任何实现**。
// service 层只依赖这里的接口；repository / objectstore / imgsvc / gateway
// 四个包各自提供实现。这样 service 的单测可以塞假实现，不需要数据库和网络。
//
// 依赖方向：handler → service → domain（接口）← repository / 各适配器。
// **domain 不 import 上游任何包**，所以这些接口里不出现 *gin.Context、*sql.DB、
// *ent.Client、上游的 service 类型。

// 仓储层的通用错误。repository 实现必须把底层错误翻译成这几个，
// service 层靠 errors.Is 判断，不去解析 SQL 驱动的错误串。
var (
	// ErrNotFound 查不到。
	ErrNotFound = errors.New("designkit: not found")
	// ErrConflict 唯一约束冲突 / 状态守卫没命中（影响行数 0）。
	ErrConflict = errors.New("designkit: conflict")
	// ErrInsufficientBalance 可用额不够。附带的差额在 InsufficientBalanceError 里。
	ErrInsufficientBalance = errors.New("designkit: insufficient balance")
	// ErrLeaseLost 写回结果时租约已被别人抢走（WHERE worker_id=? 没命中）。
	// **拿到它必须丢弃结果**，不要重试写入。
	ErrLeaseLost = errors.New("designkit: lease lost")
	// ErrIllegalTransition 状态迁移不在白名单里。
	ErrIllegalTransition = errors.New("designkit: illegal status transition")
	// ErrObjectNotFound 对象存储里没有这个 key。
	ErrObjectNotFound = errors.New("designkit: object not found")
)

// InsufficientBalanceError 带上「还差多少」，界面要显示出来（决策 19）。
type InsufficientBalanceError struct {
	// Required 这一单需要多少。
	Required Money
	// Available 当前可用额 = balance - SUM(open holds)。
	Available Money
	// Shortfall 还差多少 = Required - Available。
	Shortfall Money
}

// Error 实现 error。
func (e *InsufficientBalanceError) Error() string {
	return "designkit: insufficient balance, shortfall " + MoneyString(e.Shortfall)
}

// Unwrap 让 errors.Is(err, ErrInsufficientBalance) 成立。
func (e *InsufficientBalanceError) Unwrap() error { return ErrInsufficientBalance }

// ----------------------------------------------------------------------------
// Repository
// ----------------------------------------------------------------------------

// Repository 是 designkit 的全部持久化能力。
//
// 实现约束（CLAUDE.md 第二·五节 A2）：
//   - **只依赖 *sql.DB，完全不碰 ent**，形态照抄上游 repository/batch_image_repo.go
//   - 状态流转用悲观锁（SELECT ... FOR UPDATE），不要用乐观锁的 version 模式
//   - 所有返回列表的查询**必须显式 ORDER BY**（ERP 契约靠它）
//   - 事务边界在 repository 内部，service 不感知
type Repository interface {
	JobRepository
	BillingRepository
	AssetRepository
	PromptRepository
	SyncRepository
	SettingRepository
}

// ---- 任务 ----

// CreateJobParams 建一个批次要的全部输入。
type CreateJobParams struct {
	// UserID 提交人。
	UserID int64
	// APIKeyID 记在哪把 Key 上（浏览器端用内部专用 Key）。
	APIKeyID int64
	// Origin web / erp。
	Origin Origin
	// JobUID 调用方生成好的 26 位 ULID。
	JobUID string
	// Name 批次名，可为空串。
	Name string
	// Ratio 整批统一的出图比例。
	Ratio Ratio
	// Model 出图模型。
	Model string
	// IdempotencyKey 客户端送来的防重复标识，浏览器端也必须带。
	IdempotencyKey *string
	// CallbackURL 完成回调地址，空 = 不回调。
	CallbackURL *string
	// EstimatedCost 预估花费，冻结额按它算。
	EstimatedCost Money
	// Pricing 定价快照。
	Pricing PricingSnapshot
	// Items 展开好的每一张，**顺序必须是「商品图外层、提示词内层」**，
	// Seq 从 1 连续递增。repository 按传进来的顺序插。
	Items []CreateJobItemParams
}

// CreateJobItemParams 批次里的一张图。
type CreateJobItemParams struct {
	// Seq 批次内序号，从 1 开始。
	Seq int
	// AssetID 用哪张原图。
	AssetID *int64
	// VariantID 实际发给网关的预处理产物。
	VariantID *int64
	// PromptID 来自灵感库的哪条；手打的为 nil。
	PromptID *int64
	// PromptText 提交那一刻的提示词原文快照。
	PromptText string
	// MaxAttempts 累计尝试上限，默认 DefaultMaxAttempts。
	MaxAttempts int
}

// PricingSnapshot 是提交那一刻的价格参数快照。
// 之后管理员改分组定价也不影响这批的账。
type PricingSnapshot struct {
	// BaseUnitPrice 基础单价。
	BaseUnitPrice Money
	// GroupRateMultiplier 分组/用户专属倍率。
	GroupRateMultiplier Multiplier
	// AccountRateMultiplier 上游账号倍率。
	AccountRateMultiplier Multiplier
	// BillingMode 计价方式。解析成 "token" 就是配错了，报价必然失准。
	BillingMode string
	// PricingTier 预估时假定的档位 1K/2K/4K。
	PricingTier string
	// PriceSource 价格来自 channel/group/default 哪一层。
	PriceSource string
	// Version 快照结构版本号。
	Version int
}

// CreateJobResult 建批次的结果。
type CreateJobResult struct {
	// Job 建好的批次（Reused 为 true 时是既有的那一个）。
	Job *Job
	// Items 建好的每一张，已按 seq 升序。
	Items []*JobItem
	// Reused 命中幂等、复用了已有批次时为 true。此时**没有**新建任何东西，
	// 也没有新冻结额度，handler 直接把原结果返回去。
	Reused bool
}

// ClaimItemParams 领取一张待办要的参数。
type ClaimItemParams struct {
	// WorkerID 领取者标识，写回结果时要拿它做守卫。
	WorkerID string
	// LeaseFor 租约时长，默认 DefaultLeaseSeconds 秒。
	LeaseFor time.Duration
	// Now 当前时间。传零值表示用数据库的 NOW()。
	Now time.Time
}

// CompleteItemSuccessParams 一张图出成功之后写回的全部内容。
//
// repository 必须在**同一个事务**里：
//  1. 带守卫更新 item（WHERE id=? AND worker_id=? AND status='running'），
//     影响行数不等于 1 就返回 ErrLeaseLost 并丢弃结果；
//  2. 插入 designkit_images，用
//     ON CONFLICT (item_id, image_index) WHERE is_current AND deleted_at IS NULL；
//  3. job.success_count 原子自增（SET x = x + 1）。
type CompleteItemSuccessParams struct {
	// ItemID 哪一张。
	ItemID int64
	// WorkerID 守卫用，必须跟领取时一致。
	WorkerID string
	// Attempt 这次是第几次尝试。
	Attempt int
	// BillingRequestID 带 client: 前缀的完整值。
	BillingRequestID string
	// Images 本次产出的图，ImageIndex 从 1 开始。
	Images []NewImage
	// FinishedAt 完成时刻。
	FinishedAt time.Time
}

// NewImage 一张待入库的结果图。
type NewImage struct {
	// UID 图片 ULID。
	UID string
	// ImageIndex 同一次尝试里的第几张，从 1 开始。
	ImageIndex int
	// ObjectKey 已经上传好的对象存储路径。
	ObjectKey string
	// ContentType MIME 类型。
	ContentType string
	// ByteSize 字节数。
	ByteSize int64
	// Width 实际像素宽（决定计费档，必须记）。
	Width *int
	// Height 实际像素高。
	Height *int
	// BillingTier 由像素分出来的档。
	BillingTier *string
}

// CompleteItemFailureParams 一张图失败后写回的内容。
type CompleteItemFailureParams struct {
	// ItemID 哪一张。
	ItemID int64
	// WorkerID 守卫用。
	WorkerID string
	// ErrorCode 我方错误码（界面显示中文时带上它）。
	ErrorCode string
	// ErrorMessage 上游英文原文，**只落库给管理员看**。
	ErrorMessage string
	// Terminal true = 不再重试，item 落 failed 并让 job.fail_count 自增；
	// false = 还能再试，item 退回 pending 并按 RetryAfter 退避。
	Terminal bool
	// RetryAfter Terminal=false 时的退避时长。
	RetryAfter time.Duration
	// FinishedAt 结束时刻。
	FinishedAt time.Time
}

// ListJobsQuery 批次列表查询条件。
//
// **一律游标分页**（created_at + id 复合游标），不用 offset ——
// offset 在持续写入时会漏数据。
type ListJobsQuery struct {
	// UserID 只看这个人的。
	UserID int64
	// Statuses 只看这些状态；空表示不过滤。
	Statuses []JobStatus
	// CursorCreatedAt 上一页最后一行的 created_at；零值表示第一页。
	CursorCreatedAt time.Time
	// CursorID 上一页最后一行的 id。
	CursorID int64
	// Limit 每页条数。
	Limit int
	// IncludeDeleted 是否包含用户软删掉的。
	IncludeDeleted bool
}

// JobPage 一页批次。
type JobPage struct {
	// Jobs 本页数据，按 (created_at DESC, id DESC) 排。
	Jobs []*Job
	// NextCursorCreatedAt 下一页游标；HasMore 为 false 时无意义。
	NextCursorCreatedAt time.Time
	// NextCursorID 下一页游标。
	NextCursorID int64
	// HasMore 还有没有下一页。
	HasMore bool
}

// FinalizeJobParams 终态判定 + 触发回调用的参数。
type FinalizeJobParams struct {
	// JobID 哪个批次。
	JobID int64
	// Status 算出来的终态（用 TerminalJobStatus 算）。
	Status JobStatus
	// FinishedAt 完成时刻。
	FinishedAt time.Time
}

// JobRepository 批次和单张图的读写。
type JobRepository interface {
	// CreateJobWithItemsAndHold 在**一个事务**里完成提交（设计定型 第五节方案 D）：
	//   SELECT balance FROM users WHERE id=? FOR UPDATE
	//   可用额 = balance - SUM(designkit_holds.amount WHERE user_id=? AND status='open')
	//   可用额 < 预估 → 返回 *InsufficientBalanceError，事务回滚，什么都不建
	//   INSERT designkit_jobs → INSERT designkit_job_items → INSERT designkit_holds
	// 那条 FOR UPDATE 就是全部的并发防护，它把上游「50 张并发全部放行」的窗口关死了。
	CreateJobWithItemsAndHold(ctx context.Context, params CreateJobParams) (*CreateJobResult, error)

	// GetJobByUID 按对外任务号取批次，同时校验归属（userID 不匹配一律 ErrNotFound，
	// 不要返回 403 —— 那会泄露「这个任务号存在」）。
	GetJobByUID(ctx context.Context, userID int64, uid string) (*Job, error)

	// GetJobByID 按内部主键取批次，给 worker 用（worker 没有 userID 上下文）。
	GetJobByID(ctx context.Context, id int64) (*Job, error)

	// ListJobs 批次列表，游标分页。
	ListJobs(ctx context.Context, query ListJobsQuery) (*JobPage, error)

	// ListJobItems 取一个批次的全部 item。**必须显式 ORDER BY seq ASC。**
	ListJobItems(ctx context.Context, jobID int64) ([]*JobItem, error)

	// GetJobItemBySeq 按序号取某一张。seq 从 1 开始。
	GetJobItemBySeq(ctx context.Context, jobID int64, seq int) (*JobItem, error)

	// ListImagesByItem 取某一张的结果图。
	// current=true 只给当前版本（is_current AND deleted_at IS NULL）。
	// **必须显式 ORDER BY image_index ASC。**
	ListImagesByItem(ctx context.Context, itemID int64, currentOnly bool) ([]*Image, error)

	// GetImageByUID 按图片编号取一张结果图，校验归属。
	GetImageByUID(ctx context.Context, userID int64, uid string) (*Image, error)

	// UpdateJobStatus 带白名单和守卫的状态迁移：
	// 调用前先 CanTransition(from, to)，SQL 里带 WHERE status = from，
	// 影响行数为 0 返回 ErrConflict。
	UpdateJobStatus(ctx context.Context, jobID int64, from, to JobStatus) error

	// ClaimNextItem 领取下一张待办。就是设计定型 第六节那条 SQL：
	// FOR UPDATE SKIP LOCKED，把僵尸回收写进同一个 WHERE，
	// attempt_count 在**领取时**就 +1。没有可领的返回 ErrNotFound。
	ClaimNextItem(ctx context.Context, params ClaimItemParams) (*JobItem, error)

	// RenewItemLease 续租约（每 60 秒一次）。租约已被别人抢走返回 ErrLeaseLost。
	RenewItemLease(ctx context.Context, itemID int64, workerID string, leaseFor time.Duration) error

	// CompleteItemSuccess 写回成功结果。守卫没命中返回 ErrLeaseLost。
	CompleteItemSuccess(ctx context.Context, params CompleteItemSuccessParams) error

	// CompleteItemFailure 写回失败结果（含退避重排）。守卫没命中返回 ErrLeaseLost。
	CompleteItemFailure(ctx context.Context, params CompleteItemFailureParams) error

	// RequestJobStop 「停止排队」：把 pending 的 item 转 cancelled、
	// 给 job 打 cancel_requested_at，返回砍掉了几张。
	// **已经 running 的 item 一律不动**，必须等它自然落地。
	RequestJobStop(ctx context.Context, jobID int64) (cancelled int, err error)

	// RetryItem 重试一张：走与提交同一套准入（单张预估 → FOR UPDATE 查可用额 →
	// 不够返回 *InsufficientBalanceError），把 item 退回 pending、
	// job 从终态拉回 running、清 finished_at、fail_count 减 1、revision +1、
	// 把 hold 额度加回去（ON CONFLICT (job_id) DO UPDATE，不要再插一行）。
	// settled_at 非空的 job 返回 ErrConflict（对应 DK_JOB_ALREADY_SETTLED）。
	RetryItem(ctx context.Context, params RetryItemParams) (*JobItem, error)

	// FinalizeJob 终态判定 + 回调触发合并成一条带守卫的 UPDATE：
	//   WHERE id=? AND finished_at IS NULL
	//     AND success_count + fail_count + cancelled_count = item_count
	// 只有影响行数 = 1 的那个 worker 返回 true 去结算和发回调，
	// 否则 5 个 worker 都以为自己是最后一张，**回调会发 5 次**。
	FinalizeJob(ctx context.Context, params FinalizeJobParams) (won bool, err error)

	// TouchJobHeartbeat 给这些批次续心跳（每 30 秒一次）。
	TouchJobHeartbeat(ctx context.Context, jobIDs []int64) error

	// ReapStaleJobs 僵尸回收。**三件事，都是「不做就有钱被永久冻住」**：
	//
	//	① pending 却已经没有尝试次数的 item —— 直接判 failed。
	//	   （这类 item 领不出来、到不了终态，而心跳按 status IN ('pending','running')
	//	   算，会让整批**永远续心跳、永远躲过 ②**，冻结额永远占着用户可用额。）
	//	② status IN ('created','holding','running') 且
	//	   COALESCE(heartbeat_at, created_at) < NOW() - staleAfter 的批次 ——
	//	   原子转 failed、last_error_code = DK_STALE、把名下没跑完的 item 一并停掉、
	//	   释放 hold。
	//	③ 卡在 settling 超过 60 分钟的批次 —— 按四个计数判**真实终态**
	//	   （不是一律 failed）、封账、释放 hold。
	//
	// 返回值是 ②③ 命中的 job id，**混在一起、调用方分不出是哪一种**（要区分只能
	// 回读一次 status，worker/reaper.go 就是这么做的）。①不返回：它没把批次判死，
	// 只是把死结拆开，让批次沿着正常的 settling → 结算路径继续走。
	ReapStaleJobs(ctx context.Context, staleAfter time.Duration, limit int) ([]int64, error)

	// ClearJobIdempotencyKey 把一个批次的 idempotency_key 置 NULL。
	//
	// **提交没走完、把这一批判死之前必须调它。** 否则 24 小时内同一个 key 重投，
	// 会被 CreateJobWithItemsAndHold 里那次「按 key 查已有批次」认成命中幂等，
	// 原样返回这个 failed 的批次 —— 运营看到的是「提交成功了，但状态是失败」，
	// 换几个名字重试都一样，完全无法自救。
	//
	// 找不到那一行不算错（幂等）。
	ClearJobIdempotencyKey(ctx context.Context, jobID int64) error

	// SoftDeleteJob 运营在界面删除任务（软删），账和图都还在。
	SoftDeleteJob(ctx context.Context, userID int64, uid string) error

	// ListJobsPendingSettlement 找出 status='settling' 且还没结算完的批次，
	// 交给结算 worker。**必须显式 ORDER BY**。
	ListJobsPendingSettlement(ctx context.Context, limit int) ([]*Job, error)
}

// RetryItemParams 重试一张要的参数。
type RetryItemParams struct {
	// UserID 谁点的重试（校验归属）。
	UserID int64
	// JobID 哪个批次。
	JobID int64
	// Seq 第几张，从 1 开始。
	Seq int
	// EstimatedCost 这一张的预估花费，用来补冻结额。
	EstimatedCost Money
	// IdempotencyKey 运营双击必然发生，retry 也要支持幂等。
	IdempotencyKey *string
}

// ---- 钱 ----

// SettleJobParams 结算一个批次。
type SettleJobParams struct {
	// JobID 哪个批次。
	JobID int64
	// ActualCost SUM(job_items.billed_cost)，其中每一张的 billed_cost 又是
	// 它**所有 attempt 的合计**（设计定型 6.2）。
	// **禁止用「成功张数 × 单价」算**：同一批可能落不同计费档，单价不同；
	// 而且失败但上游已经扣过钱的那几张也必须算进来 ——
	// 判据是「上游有没有为它扣过钱」，不是「我们这边成没成功」。
	ActualCost Money
	// Status 终态。
	Status JobStatus
	// SettledAt 结算时刻。
	SettledAt time.Time
}

// UsageSummary 工作台角落显示的「本月出图 N 张 / 花费 $X / 余额 $Y」。
type UsageSummary struct {
	// ImageCount 时间窗内出成功的张数。
	ImageCount int
	// Cost 时间窗内的花费。
	Cost Money
	// Balance 当前余额（上游 users.balance）。
	Balance Money
	// Available 当前可用额 = Balance - SUM(open holds)。
	Available Money
	// Currency 固定 "USD"。
	Currency string
}

// BillingRepository 钱相关的读写。
type BillingRepository interface {
	// GetAvailableBalance 算可用额 = users.balance - SUM(open holds)。
	// **只读，不加锁**；真正要拦透支的地方必须走 CreateJobWithItemsAndHold /
	// RetryItem 里那条 FOR UPDATE，不能靠这个函数的返回值做判断。
	GetAvailableBalance(ctx context.Context, userID int64) (Money, error)

	// SumUsageLogCosts 按 billing_request_id 去上游 usage_logs 反查实际扣费。
	// 传进来的是**带 client: 前缀的完整值**。返回 map[requestID]cost，
	// 查不到的 key 直接不出现在返回值里。
	//
	// ⚠ 传的必须是**每一张图每一次 attempt** 的 id。request_id 是可推导的
	// （StoredBillingRequestID(jobUID, seq, attempt)），所以结算时枚举
	// attempt = 1..attempt_count 把全部 id 一次性查完。
	// 只传 job_items.billing_request_id 那一个值 = 只查最新一次 attempt =
	// **重试花掉的钱全丢**，而设计定型 6.2 写死 actual_cost = 所有 attempt 合计。
	//
	// 「查不到就不出现」这一点是调用方区分两件事的唯一依据：
	// 上游真写过账单的那一行即使 actual_cost 是 0，也会带 key 出现在返回值里。
	//
	// 为什么不能在最后一张图返回时立刻结算：上游写 usage_logs 是**异步**的
	// （丢进 worker 池就返回），立刻汇总很可能少算甚至全算 0。
	SumUsageLogCosts(ctx context.Context, billingRequestIDs []string) (map[string]Money, error)

	// ApplyItemBilling 把某一张的实际扣费回填进 job_items，
	// 并按张递减该 job 的 open 冻结额。
	//
	// cost 是这一张**所有 attempt 的合计**，不是当前这次 attempt 的花费。
	// 实现必须做到**幂等 + 单调**：重复回填同样的金额不再动冻结额；
	// billed_cost 只增不减 —— 冻结额按「新值 - 旧值」递减，负差额会把冻结额
	// 加回去，凭空放大用户可用额。
	ApplyItemBilling(ctx context.Context, itemID int64, cost Money, billingMode, billingTier string) error

	// SettleJob 写 actual_cost + 终态 + settled_at，并把剩余 open 冻结额置 released。
	//
	// **不管账单查全没查全都要走到这一步。** 让批次永远停在 settling 的代价是
	// 冻结额永久占着用户的可用额，而且没有任何报错 —— 运营看到的是
	// 「明明没任务却提示余额不足」。查不全的那几笔靠 InsertBillingAlert 留痕。
	SettleJob(ctx context.Context, params SettleJobParams) error

	// ReleaseHold 把某个批次的冻结整笔释放（提交失败、僵尸回收时用）。
	ReleaseHold(ctx context.Context, jobID int64) error

	// InsertBillingAlert 记一条结算告警（实际超预估，或者账单没查全 / 卡太久被强制结算）。
	// **绝不能因为实际 > 预估就报错回滚**——钱已经被上游扣走了，报错只会让任务卡死。
	//
	// ⚠ 表上没有任何唯一约束（9001 已定稿、永不可改），所以**调用方必须自己保证不重复插**：
	// 一次结算最多一条，且要等 SettleJob 成功之后再插 —— 插在前面的话，
	// SettleJob 失败重来一轮就会多一条，管理员后台看到的是一堆重复待办。
	InsertBillingAlert(ctx context.Context, alert *BillingAlert) error

	// CreateQuotaRequest 运营点「申请额度」。同一个人已有 pending 记录时
	// 返回 ErrConflict（部分唯一索引挡着，防止连点刷屏）。
	CreateQuotaRequest(ctx context.Context, userID int64, note *string) (*QuotaRequest, error)

	// ListQuotaRequests 管理员后台看待办。**必须显式 ORDER BY**。
	ListQuotaRequests(ctx context.Context, status QuotaRequestStatus, limit, offset int) ([]*QuotaRequest, error)

	// ListQuotaRequestDetails 管理端列表：本体 + 申请人邮箱 + 处理详情。
	// pendingOnly=true 只看待处理；false 只看**处理过的**（handled + rejected 都算）。
	// **必须显式 ORDER BY**。
	ListQuotaRequestDetails(ctx context.Context, pendingOnly bool, limit, offset int) ([]*QuotaRequestDetail, error)

	// CountPendingQuotaRequests 待处理条数（侧边栏红点用）。
	CountPendingQuotaRequests(ctx context.Context) (int, error)

	// HandleQuotaRequest 管理员处理一条申请：原子地把 pending 行置成
	// handled / rejected 并写处理详情，返回**处理后的那一行**（调用方要用
	// UserID 去加余额）。行不存在返回 ErrNotFound；已被处理过返回 ErrConflict ——
	// 这条 WHERE status='pending' 守卫就是「两个管理员同时点通过」的唯一防线，
	// 少了它同一条申请会被加两次钱。
	HandleQuotaRequest(ctx context.Context, id int64, handledBy int64, status QuotaRequestStatus, handleNote *string, approvedAmount *Money) (*QuotaRequest, error)

	// ReopenQuotaRequest 把一条刚置成 handled 的申请退回 pending（补偿用：
	// 标记成功但加余额失败时调它，让管理员能直接重试）。
	// 只认 handled 状态的行；退回会撞「同人只能有一条 pending」的部分唯一索引时
	// 返回 ErrConflict（说明申请人已另提了一条新申请）。
	ReopenQuotaRequest(ctx context.Context, id int64) error

	// GetUsageSummary 工作台角落那三个数。from/to 是时间窗（本月）。
	//
	// ⚠ 花费必须按 **usage_logs 自己的 created_at** 落窗口，
	// 不许按 job_items.finished_at —— 那一列会被重试刷新，
	// 上个月的钱会跟着重试挪到本月，月度对账对不上。
	GetUsageSummary(ctx context.Context, userID int64, from, to time.Time) (*UsageSummary, error)
}

// ---- 素材 ----

// ListAssetsQuery 商品图列表查询条件。
type ListAssetsQuery struct {
	// UserID 只看这个人的。
	UserID int64
	// CursorCreatedAt 游标：上一页最后一行的 created_at。
	CursorCreatedAt time.Time
	// CursorID 游标：上一页最后一行的 id。
	CursorID int64
	// Limit 每页条数。
	Limit int
}

// UpsertVariantParams 写入（或复用）一个预处理产物。
type UpsertVariantParams struct {
	// AssetID 哪张原图。
	AssetID int64
	// Ratio 目标比例。
	Ratio Ratio
	// KeepTransparency 是否保留透明。
	KeepTransparency bool
	// MaxDimension 最长边上限。
	MaxDimension int
	// ObjectKey 处理后图片的路径。
	ObjectKey string
	// Width 处理后像素宽。
	Width *int
	// Height 处理后像素高。
	Height *int
	// ContentType MIME 类型。
	ContentType string
}

// AssetRepository 商品图和预处理产物的读写。
type AssetRepository interface {
	// CreateAsset 记录一张刚上传的原图。
	CreateAsset(ctx context.Context, asset *Asset) error

	// GetAssetByUID 按对外编号取原图，校验归属。
	GetAssetByUID(ctx context.Context, userID int64, uid string) (*Asset, error)

	// GetAssetsByIDs 批量取原图（展开批次时用）。
	// **返回顺序必须跟传进来的 ids 一致**，不要指望数据库还你原顺序。
	GetAssetsByIDs(ctx context.Context, userID int64, ids []int64) ([]*Asset, error)

	// FindAssetBySHA256 认出「同一个人又传了同一张图」，直接复用不重复存。
	FindAssetBySHA256(ctx context.Context, userID int64, sha256 string) (*Asset, error)

	// ListAssets 商品图列表，游标分页，按 (created_at DESC, id DESC)。
	ListAssets(ctx context.Context, query ListAssetsQuery) ([]*Asset, error)

	// SoftDeleteAsset 软删除；历史任务仍能引用。
	SoftDeleteAsset(ctx context.Context, userID int64, uid string) error

	// GetVariant 按四个参数查已有产物；没有返回 ErrNotFound。
	GetVariant(ctx context.Context, assetID int64, ratio Ratio, keepTransparency bool, maxDimension int) (*AssetVariant, error)

	// UpsertVariant 写入产物。走 ON CONFLICT (asset_id, ratio, keep_transparency,
	// max_dimension) DO UPDATE，并发重复预处理时不炸。
	UpsertVariant(ctx context.Context, params UpsertVariantParams) (*AssetVariant, error)
}

// ---- 灵感库 ----

// ListPromptsQuery 提示词检索条件。
type ListPromptsQuery struct {
	// CategoryID 按分类过滤；nil = 不过滤。
	CategoryID *int64
	// Keyword 关键词。**一律用 ILIKE**（PostgreSQL 的 LIKE 区分大小写）。
	Keyword string
	// Source 只看这个来源；nil = 不过滤。
	//
	// ⚠ 对外的列表必须带上它：不带的话，运营 A 存的提示词会混进
	// 运营 B 的灵感库列表里 —— 自建词只有本人可见（「我的提示词」）。
	// 共享目录一律 Source=youmind；「我的提示词」一律 Source=user + OwnerUserID。
	Source *PromptSource
	// OwnerUserID 只看某人自己存的；nil = 不过滤。
	OwnerUserID *int64
	// OnlyEnabled 只看没下架的。
	OnlyEnabled bool
	// CursorID 游标：上一页最后一行的 id。
	CursorID int64
	// Limit 每页条数。
	Limit int
}

// SyncPromptRow 一条待同步进来的提示词。
type SyncPromptRow struct {
	// UID 新增时用的 ULID（命中已有 source_ref 时忽略）。
	UID string
	// CategorySlug 分类的稳定标识，靠它找分类。
	CategorySlug string
	// Title 标题。
	Title string
	// Body 正文。
	Body string
	// Variables 占位变量。
	Variables []PromptVariable
	// PreviewURL 预览图外链。
	PreviewURL *string
	// SourceRef 上游库里的唯一标识，同步幂等键。
	SourceRef string
}

// SyncPromptsResult 一次同步写入的统计。
type SyncPromptsResult struct {
	// Inserted 新增多少条。
	Inserted int
	// Updated 更新多少条。
	Updated int
	// Skipped 内容没变、跳过多少条。
	Skipped int
}

// PromptRepository 灵感库的读写。
type PromptRepository interface {
	// ListCategories 分类列表，按 (sort_order, id) 排。
	ListCategories(ctx context.Context) ([]*PromptCategory, error)

	// UpsertCategory 按 slug 幂等写入分类。
	UpsertCategory(ctx context.Context, category *PromptCategory) (*PromptCategory, error)

	// ListPrompts 提示词检索，游标分页。**必须显式 ORDER BY id**。
	ListPrompts(ctx context.Context, query ListPromptsQuery) ([]*Prompt, error)

	// ListPromptDigests 取一个分类下**全部**提示词的「简介」（不拉正文全文）。
	//
	// 专给「AI 挑提示词」的粗筛用：要让模型从整个分类里挑，就得把整个分类的
	// 简介都给它看。而正文平均 1616 字，3978 条拉全文是 640 万字符 ——
	// 既传不动也没必要，粗筛阶段只需要「这条大概是干什么的」。
	//
	// Brief 由 SQL 截取正文开头若干字符，调用方再提炼（见 repository 的实现）。
	ListPromptDigests(ctx context.Context, categoryID *int64, limit int) ([]*PromptDigest, error)

	// SamplePrompts 按同样的筛选条件**随机**取若干条（ORDER BY random()）。
	//
	// 专给「AI 挑提示词」取候选用。跟 ListPrompts 的区别只有排序，
	// 而这个区别是必须的：ListPrompts 按 id 升序，取 100 条永远是同样那 100 条 ——
	// 社交媒体帖子 3978 条里有 97% 永远轮不到，且完全静默。
	// 详见 repository/prompt_repo.go 的 buildSamplePromptsQuery。
	//
	// **不要拿它做分页**：每次顺序都不一样，翻页会重复和遗漏。
	SamplePrompts(ctx context.Context, query ListPromptsQuery) ([]*Prompt, error)

	// CountPrompts 命中条数，给界面显示总数用。
	CountPrompts(ctx context.Context, query ListPromptsQuery) (int, error)

	// CountPromptsByCategory 每个分类下有多少条，返回 category_id → 条数。
	// 没有分类的（category_id 为空）归到键 0。
	//
	// 为什么单独开一个方法而不是让界面按分类查 15 次：分类页一进去就要显示
	// 「电商主图 416」这样的条数，一次 GROUP BY 就够了。
	CountPromptsByCategory(ctx context.Context, onlyEnabled bool) (map[int64]int, error)

	// GetPromptByUID 按对外编号取一条。
	GetPromptByUID(ctx context.Context, uid string) (*Prompt, error)

	// GetPromptsByIDs 批量取（展开批次时用）。
	// **返回顺序必须跟传进来的 ids 一致** —— 展开顺序是对外契约的一部分。
	GetPromptsByIDs(ctx context.Context, ids []int64) ([]*Prompt, error)

	// CreatePrompt 运营自己存一条（source=user，owner_user_id 必填）。
	CreatePrompt(ctx context.Context, prompt *Prompt) error

	// UpdatePrompt 改一条自己的。
	UpdatePrompt(ctx context.Context, prompt *Prompt) error

	// DeletePrompt 软删一条自己的。
	DeletePrompt(ctx context.Context, ownerUserID int64, uid string) error

	// UpsertSyncedPrompts 批量写入同步来的提示词。
	// ON CONFLICT (source_ref) **必须带上部分索引的 WHERE 条件**：
	//   ON CONFLICT (source_ref) WHERE source_ref IS NOT NULL AND source_ref <> ''
	UpsertSyncedPrompts(ctx context.Context, rows []SyncPromptRow) (*SyncPromptsResult, error)
}

// ---- 同步 ----

// SyncRepository 灵感库同步的锁和记录。
type SyncRepository interface {
	// TryLockSync 抢 PostgreSQL advisory lock（pg_try_advisory_lock）。
	// **手动和自动共用同一把锁**，不要用应用内 mutex —— 多副本部署时它不起作用。
	// 抢不到返回 false，调用方记一条 status='skipped' 的 SyncRun。
	//
	// ⚠ advisory lock 是**会话级**的：实现必须从池里取一条连接并一直握着它，
	// 用完在同一条连接上 pg_advisory_unlock，否则锁会跟着连接回池而失效。
	TryLockSync(ctx context.Context) (locked bool, unlock func(), err error)

	// StartSyncRun 记一次同步开始。
	StartSyncRun(ctx context.Context, kind SyncKind) (*SyncRun, error)

	// FinishSyncRun 记一次同步结束。
	FinishSyncRun(ctx context.Context, run *SyncRun) error

	// LatestSyncRun 最近一次同步，按 (started_at DESC, id DESC)。
	LatestSyncRun(ctx context.Context) (*SyncRun, error)
}

// ---- 设置 ----

// SettingRepository designkit 自己的配置读写。**不碰上游 settings 表。**
type SettingRepository interface {
	// GetSetting 取一个配置项；不存在返回 ErrNotFound。
	GetSetting(ctx context.Context, key string) (*Setting, error)

	// ListSettings 取全部配置项，给界面和启动期缓存用。
	ListSettings(ctx context.Context) ([]*Setting, error)

	// PutSetting 写一个配置项（ON CONFLICT (key) DO UPDATE）。
	PutSetting(ctx context.Context, key string, value []byte) error
}

// ----------------------------------------------------------------------------
// ObjectStore
// ----------------------------------------------------------------------------

// ObjectStore 是我们自己的对象存储接口。
//
// 为什么不直接用上游的 service.ImageStorage：
//   - 它**只有一个 Save(ctx, key, contentType, data) (url, err)** —— 没有 Get、
//     没有 Delete、没有重新签名；
//   - 它**只返回 URL，key 是调用方拼的、上传完就丢弃**，上游任何地方都没把 key 落库；
//   - 唯一实现是 S3 兼容对象存储，**没有本地磁盘选项**；没配置的后果是
//     异步生图功能整个关闭（handler 直接 404），不是「兜底落本地目录」；
//   - 默认走 presigned 链接、24 小时失效，**过期后拼不回来**。
//
// 而 monica 的群晖 NAS 上没有 S3，所以**必须有本地磁盘实现**。
// 我们自己生成 object_key 并入库，URL 一律运行时生成、绝不入库；
// 「必须经我方鉴权才能看图」的下载 handler 也得有 key 才写得出来。
type ObjectStore interface {
	// Put 写入一个对象。同 key 重复写允许覆盖（重试出图会覆盖同一张）。
	Put(ctx context.Context, key string, contentType string, data []byte) error

	// Get 读出一个对象，返回字节和 contentType。不存在返回 ErrObjectNotFound。
	Get(ctx context.Context, key string) ([]byte, string, error)

	// Delete 删除一个对象。key 不存在时**返回 nil**（幂等），不要报错。
	Delete(ctx context.Context, key string) error

	// Exists 判断对象在不在。
	Exists(ctx context.Context, key string) (bool, error)
}

// ----------------------------------------------------------------------------
// ImagePreprocessor
// ----------------------------------------------------------------------------

// Python 预处理服务在响应头里回的元数据字段名（设计定型 第七节）。
const (
	// PreprocessHeaderChanged 有没有真的改动过图片。
	PreprocessHeaderChanged = "X-Dk-Changed"
	// PreprocessHeaderWidth 处理后像素宽。
	PreprocessHeaderWidth = "X-Dk-Width"
	// PreprocessHeaderHeight 处理后像素高。
	PreprocessHeaderHeight = "X-Dk-Height"
	// PreprocessHeaderActions 做了哪些动作，逗号分隔，排障用。
	PreprocessHeaderActions = "X-Dk-Actions"
)

// PreprocessRequest 调 POST /v1/preprocess 的入参（multipart 表单）。
type PreprocessRequest struct {
	// Data 原图字节。**走 body 直传，Python 服务不接触任何对象存储凭据。**
	Data []byte
	// Filename 原始文件名，只用于让 Python 那边认扩展名。
	Filename string
	// ContentType 原图 MIME 类型。
	ContentType string
	// Ratio 目标比例，表单字段 ratio，形如 "3:4"。
	//
	// ⚠ 必须传比例串，不能传 "1536x1024" 这种像素写法 ——
	// 老的 parse_ratio 只认像素写法，传 "3:4" 会**静默不补边**。
	Ratio Ratio
	// KeepTransparency 表单字段 keep_transparency。
	KeepTransparency bool
	// MaxDimension 表单字段 max_dimension。
	//
	// ⚠ **每次调用都必须显式带上**，值取自 designkit_settings.max_dimension。
	// Python 那边的环境变量只是「Go 忘了传」的兜底；不显式传，管理员改了设置
	// 却没改环境变量，就会「界面说 4K、实际按 2K 出图」，而且不报错。
	MaxDimension int
}

// PreprocessResult 预处理成功后的返回。
type PreprocessResult struct {
	// Data 处理后的图片字节（**直接返回字节，不是 JSON base64**）。
	Data []byte
	// ContentType 处理后的 MIME 类型。
	ContentType string
	// Width 处理后像素宽。
	Width int
	// Height 处理后像素高。
	Height int
	// Changed 有没有真的改动过。
	Changed bool
	// Actions 做了哪些动作，排障用。
	Actions []string
}

// ImagePreprocessor 对应独立容器 designkit-imgsvc（FastAPI）。
//
// **fail-closed**：Python 侧任何异常返 5xx、**绝不回吐原图**；
// Go 侧拿到非 200 就把该 item 置 failed（DK_PREPROCESS_FAILED）。
// 老代码是 fail-open（异常就返回原始字节），搬过来就是：补边失败 →
// 原图什么比例出什么比例 → **运营选 3:4 拿回 4:3，钱照扣**，只有一行 warning。
//
// 实现约束：地址走 os.Getenv（DESIGNKIT_IMGSVC_URL / _TOKEN / _TIMEOUT_SECONDS），
// 不碰 config.Config（那不在白名单里）；只对连接失败和 502/503/504 重试 2 次，
// 4xx 不重试。
type ImagePreprocessor interface {
	// Preprocess 补白边到目标比例（不裁产品）、按需合白底、限制最长边。
	Preprocess(ctx context.Context, req PreprocessRequest) (*PreprocessResult, error)

	// Health 探活，对应 GET /healthz（返回 pillow / heif 可用性）。
	Health(ctx context.Context) error
}

// ----------------------------------------------------------------------------
// GatewayInvoker
// ----------------------------------------------------------------------------

// GenerateRequest 出一张图要的全部输入。
type GenerateRequest struct {
	// UserID 记在谁头上。
	UserID int64
	// APIKeyID 记在哪把 Key 上。取不到 Key 上游直接 401。
	APIKeyID int64
	// JobUID 批次号，用来拼 request_id。
	JobUID string
	// Seq 批次内序号，从 1 开始。
	Seq int
	// Attempt 第几次尝试，从 1 开始。
	Attempt int
	// Model 出图模型。
	Model string
	// Prompt 提示词原文（用 item 里的快照，不要回灵感库重取）。
	Prompt string
	// Size 发给上游的 size 字段。
	//
	// ⚠ 上游不一定听：实测请求 1:1 拿回 1254×1254。真正的比例控制靠输入图补边。
	Size string
	// Ratio 目标比例，只用于日志和排障。
	Ratio Ratio
	// ImageData 预处理之后的图片字节。
	ImageData []byte
	// ImageFilename multipart 里的文件名。
	ImageFilename string
	// ImageContentType multipart 里的 MIME 类型。
	ImageContentType string
	// BillingRequestID **不带** client: 前缀的值，即 BillingRequestID(...) 的返回。
	// 实现必须在合成请求的 context 上**无条件覆写**（不是「没有才设」）
	// ctxkey.ClientRequestID 和 ctxkey.RequestID 为这个值，
	// 否则一单 N 张会共用入站请求的 id，第 2 张起静默不扣钱。
	BillingRequestID string
	// Timeout 单张超时。
	Timeout time.Duration
}

// GeneratedImage 上游返回的一张图。
type GeneratedImage struct {
	// Data 图片字节。
	Data []byte
	// ContentType MIME 类型。
	ContentType string
	// Width 实际像素宽 —— 计费落哪档由它决定。
	Width int
	// Height 实际像素高。
	Height int
}

// GenerateResult 出一张图的结果。
type GenerateResult struct {
	// Images 本次产出的图。**可能不止一张**（n 参数行为还没实测），
	// 调用方必须按切片处理，不要只取第一张 —— 少存一张就是钱花了图丢了。
	Images []GeneratedImage
	// UpstreamStatus 上游 HTTP 状态码，翻译错误时要用。
	UpstreamStatus int
	// RawResponse 上游原始响应体，排障用；不要写进接口返回。
	RawResponse []byte
	// StoredBillingRequestID 带 client: 前缀的完整值，直接落
	// designkit_job_items.billing_request_id。
	StoredBillingRequestID string
}

// GatewayInvoker 把一张图发给上游网关。
//
// 唯一实现是「在进程内构造合成 gin.Context，重入 h.OpenAIGateway.Images()」
// （设计定型 第一节）。六步一步都不能漏：
//
//  1. ctx = context.WithoutCancel(原始 ctx)  ← 漏了：运营一关页面整批 canceled，图已出、钱已扣
//  2. ctx, cancel = context.WithTimeout(ctx, 单张超时)
//  3. req = 新建 http.Request（不是 Clone 入站请求，我们没有入站 multipart）
//  4. 换 Body / GetBody / ContentLength
//  5. taskCtx = c.Copy()                     ← 每个 item 造一份，共用会互相覆盖账号选择
//  6. taskCtx.Writer = recorder.Writer       ← 漏了：往已归还对象池的 Writer 上写，高并发串包或 panic
//
// multipart 要自己拼：文件字段名**必须是 image**，另加 prompt / model / size 三个
// WriteField；URL.Path 必须含 /images/edits 字面量（上游从路径 strings.Contains
// 出端点）；**显式剔除 session_id / conversation_id / x-session-id**，
// 否则一批 50 张全粘到同一个上游账号，撞上 accounts.concurrency 默认 3，
// 批量退化成三张三张串行。
type GatewayInvoker interface {
	// Generate 出一张图。上游返回非 2xx 时，实现应当返回
	// MapUpstreamError(status, body) 得到的 *DesignkitError。
	Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error)
}
