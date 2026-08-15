package domain

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// 实体
// ============================================================================
//
// 每个结构体的字段跟 backend/migrations/9001_designkit_init.sql 的列**一一对应**，
// 顺序也跟 SQL 一致，方便逐行核对。那个迁移文件已经跑过、永不可改，列名以它为准。
//
// 约定（全模块统一，见 doc.go）：
//   - 可空列 → 指针（*string / *int64 / *int / *time.Time / *Money）
//   - 金额列 NUMERIC(20,10) → Money（decimal），倍率列 NUMERIC(10,4) → Multiplier
//   - 时间列 TIMESTAMPTZ → time.Time
//   - ID 是自增主键，只在内部用；UID 是 26 位 ULID，接口里只出现它

// Origin 标识记录是谁产生的：运营在网页上点的，还是外部系统调接口来的。
// 对应各表的 origin 列（VARCHAR(16)）。
type Origin string

const (
	// OriginWeb 运营在界面上操作产生的。
	OriginWeb Origin = "web"
	// OriginERP 外部系统（ERP）调 /v1/designkit/* 接口产生的。
	OriginERP Origin = "erp"
)

// String 让 Origin 能直接当 SQL 参数和日志字段用。
func (o Origin) String() string { return string(o) }

// IsValid 判断 origin 取值合不合法。
func (o Origin) IsValid() bool { return o == OriginWeb || o == OriginERP }

// ----------------------------------------------------------------------------
// 1. designkit_assets —— 运营上传的原始商品图
// ----------------------------------------------------------------------------

// Asset 是一张上传上来的原始商品图。
type Asset struct {
	// ID 自增主键，只在内部用。
	ID int64
	// UID 对外暴露的 ULID，接口里只出现它。
	UID string
	// UserID 上游 users.id（无外键，理由见迁移文件头）。
	UserID int64
	// ObjectKey 原图在对象存储里的相对路径。URL 一律运行时生成，绝不入库。
	ObjectKey string
	// ContentType image/png、image/jpeg、image/heic …
	ContentType string
	// ByteSize 原图字节数。
	ByteSize int64
	// Width 原图像素宽；读不出来时为 nil。
	Width *int
	// Height 原图像素高；读不出来时为 nil。
	Height *int
	// SHA256 原始字节的 SHA256，用来认出「同一个人又传了同一张图」。
	SHA256 *string
	// Origin web=界面上传；erp=外部系统走接口上传。
	Origin Origin
	// CreatedAt 上传时间。
	CreatedAt time.Time
	// DeletedAt 软删除：用户删了就不再列出，但历史任务仍能引用。
	DeletedAt *time.Time
}

// ----------------------------------------------------------------------------
// 2. designkit_asset_variants —— 预处理产物（补边到目标比例之后的图）
// ----------------------------------------------------------------------------

// AssetVariant 是一张原图按某组预处理参数产出的图。
//
// 为什么单独一张表而不是在 Asset 上加一列：预处理产物是
// (ratio, keep_transparency, max_dimension) 三个参数的函数 ——
// 同一张原图在 3:4 任务和 16:9 任务里是两个不同文件。
// 用单列会互相覆盖或被错误复用，**出图比例静默失控**。
type AssetVariant struct {
	// ID 自增主键。
	ID int64
	// AssetID 指向 designkit_assets.id。
	AssetID int64
	// Ratio 目标比例，例如 "3:4"。
	Ratio Ratio
	// KeepTransparency false = 透明底合成白底；true = 保留透明。
	KeepTransparency bool
	// MaxDimension 最长边上限像素。它直接决定出图落 2K 还是 4K 计费档，
	// 所以必须是显式参数并入库，不能埋在代码常量里。
	MaxDimension int
	// ObjectKey 处理后图片在对象存储里的相对路径。
	ObjectKey string
	// Width 处理后实际像素宽。
	Width *int
	// Height 处理后实际像素高。
	Height *int
	// ContentType 处理后的 MIME 类型。
	ContentType string
	// CreatedAt 产出时间。
	CreatedAt time.Time
}

// ----------------------------------------------------------------------------
// 3. designkit_prompt_categories —— 灵感库分类
// ----------------------------------------------------------------------------

// PromptDigest 是一条提示词的「简介」——粗筛阶段用的轻量视图。
//
// 为什么要有它：「AI 挑提示词」要让模型从**整个分类**里挑（社交媒体帖子 3978 条），
// 而正文平均 1616 字，全拉是 640 万字符。粗筛只需要「这条大概是干什么的」，
// 拿标题 + 正文开头一小段就够了。
//
// monica 2026-08-14：「我记得每一个提示词都有简短说明的，可以从简介里面选择 100，
// 然后再仔细选 5 出来嘛？」—— 就是这个东西。
type PromptDigest struct {
	// ID 自增主键，回查正文时用。
	ID int64
	// UID 对外编号。
	UID string
	// Title 标题（平均 33 字），本身就是一句不错的简介。
	Title string
	// Brief 正文开头截出来的一小段。
	//
	// 库里约 1707 条正文是 JSON 结构，开头带 "type": "..." —— 那比标题更具体；
	// 其余 13338 条是散文，开头第一句一般就是在说这条要画什么。
	// 两种形态的提炼都在 service 侧做（见 promptBrief）。
	Brief string
}

// PromptCategory 是灵感库的一个分类。
type PromptCategory struct {
	// ID 自增主键。
	ID int64
	// Slug 稳定的英文标识，同步时靠它认人，不随中文名变化。
	Slug string
	// NameZH 界面默认显示这个。
	NameZH string
	// NameEN 英文名，可为空串。
	NameEN string
	// SortOrder 界面排序，越小越靠前。
	SortOrder int
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// ----------------------------------------------------------------------------
// 4. designkit_prompts —— 灵感库提示词
// ----------------------------------------------------------------------------

// PromptSource 提示词的来源。
type PromptSource string

const (
	// PromptSourceYouMind 从 YouMind 开源库同步来的。
	PromptSourceYouMind PromptSource = "youmind"
	// PromptSourceUser 运营自己存的。
	PromptSourceUser PromptSource = "user"
)

// String 让 PromptSource 能直接当 SQL 参数用。
func (s PromptSource) String() string { return string(s) }

// PromptVariable 是提示词正文里的一个占位变量。
// 对应 designkit_prompts.variables 这一列 JSONB 里的一个元素，
// 形如 [{"name":"品类","example":"连衣裙"}]。
type PromptVariable struct {
	// Name 变量名，界面上显示成输入框的标签。
	Name string `json:"name"`
	// Example 示例值，做输入框的 placeholder。
	Example string `json:"example,omitempty"`
}

// Prompt 是灵感库里的一条提示词。
type Prompt struct {
	// ID 自增主键。
	ID int64
	// UID 对外暴露的 ULID。
	UID string
	// CategoryID 所属分类；分类被删时置空（ON DELETE SET NULL）。
	CategoryID *int64
	// Title 标题，可为空串。
	Title string
	// Body 提示词正文。
	Body string
	// Variables 正文里的占位变量定义。
	// repository 扫出来的是 []byte，用 json.Unmarshal 填进来。
	Variables []PromptVariable
	// PreviewURL 上游库给的效果预览图地址（外链，仅展示用）。
	PreviewURL *string
	// Source youmind=同步来的；user=运营自己存的。
	Source PromptSource
	// SourceRef 上游库里的唯一标识；同步去重、判断新增还是更新全靠它。
	//
	// ⚠ 它上面是**部分唯一索引**，写 ON CONFLICT 时必须把 WHERE 条件一起带上：
	// ON CONFLICT (source_ref) WHERE source_ref IS NOT NULL AND source_ref <> ''
	SourceRef *string
	// OwnerUserID source=user 时的归属人；youmind 来的为空。
	OwnerUserID *int64
	// IsEnabled 关掉之后灵感库不再列出，但历史任务里的快照不受影响。
	IsEnabled bool
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
	// DeletedAt 软删除。
	DeletedAt *time.Time
}

// ----------------------------------------------------------------------------
// 5. designkit_jobs —— 一次提交（一个批次）
// ----------------------------------------------------------------------------

// Job 是运营点一次「开始生成」产生的批次。
//
// item_count = 商品图数 × 提示词数，展开顺序固定为「商品图外层、提示词内层」。
type Job struct {
	// ID 自增主键。
	ID int64
	// UID 对外的任务号（ULID）。ERP 拿到的就是它。
	UID string
	// UserID 上游 users.id。
	UserID int64
	// APIKeyID 上游 api_keys.id。
	//
	// **绝不能为 0/空**：出图走上游网关 Images()，它第一件事就是从上下文取 API Key，
	// 取不到直接 401；usage_logs.api_key_id 也是 NOT NULL + 外键。
	// 每个运营用户首次进工作台时服务端会自动给他建一把内部专用 Key（界面不暴露），
	// 浏览器端的任务也记在那把 Key 上。没有「浏览器端为 NULL」这种情况。
	APIKeyID int64
	// Origin web=界面提交；erp=外部系统提交。
	Origin Origin
	// Name 运营给这批起的名字，可为空串。
	Name string
	// Status 见 JobStatus。改它必须先过 CanTransition。
	Status JobStatus
	// Ratio 这一批的出图比例，整批统一。
	Ratio Ratio
	// Model 出图模型名，例如 gpt-image-2。
	Model string
	// ItemCount 展开后的总张数。
	ItemCount int
	// SuccessCount 出成功的张数。**一律 SET x = x + 1 原子自增，绝不读回来再写。**
	SuccessCount int
	// FailCount 失败的张数。
	FailCount int
	// CancelledCount 「还没开始就被停掉」的张数；已经在跑的不算在这。
	CancelledCount int
	// IdempotencyKey 客户端送来的 Idempotency-Key（浏览器端也必须带，
	// 否则运营手滑双击 = 两个 job 两份钱）。
	IdempotencyKey *string
	// CallbackURL 任务完成回调地址，主要给 ERP 用；空 = 不回调。
	CallbackURL *string
	// Currency 固定 "USD"（决策 18：不做汇率换算，文案里不许出现「元」）。
	Currency string
	// EstimatedCost 提交时的预估花费，冻结额按它算。
	EstimatedCost Money
	// ActualCost 结算后的实际花费 = SUM(job_items.billed_cost)，含所有重试；
	// 未结算时为 nil。**禁止用「成功张数 × 单价」算**——同一批可能落不同计费档。
	ActualCost *Money
	// BaseUnitPrice 定价快照：基础单价。
	BaseUnitPrice Money
	// GroupRateMultiplier 定价快照：分组/用户专属倍率。
	GroupRateMultiplier Multiplier
	// AccountRateMultiplier 定价快照：上游账号倍率。
	AccountRateMultiplier Multiplier
	// BillingMode 定价快照：计价方式。
	//
	// ⚠ 解析成 "token" 就是后台配错了：只要某分组对该模型存在一条渠道定价且它的
	// billing_mode 是 token 或留空（空串会被默认成 token），整单就按 token 计价，
	// **按次价完全不参与**，极端情况算成 0 或算成文本价。
	// 上线前必须有巡检断言 gpt-image-2 在所有分组下解析出的 Mode 不为 token。
	BillingMode string
	// PricingTier 定价快照：预估时假定的档位 1K/2K/4K。
	// 真正计费的档由上游按**实际输出像素**定，两者可能不一致。
	PricingTier string
	// PriceSource 定价快照：价格来自 channel/group/default 哪一层。
	PriceSource string
	// PricingSnapshotVersion 快照结构版本号，以后改结构靠它区分。
	PricingSnapshotVersion int
	// Revision 每次「重试」把任务从终态拉回 running 时 +1。
	Revision int
	// LastErrorCode 整批失败的原因码，例如 DK_STALE。
	LastErrorCode *string
	// LastErrorMessage 上游英文原文，**只给管理员看，界面不显示**。
	LastErrorMessage *string
	// HeartbeatAt worker 每 30 秒续一次；超过 10 分钟没动静判定为僵尸。
	HeartbeatAt *time.Time
	// CancelRequestedAt 运营点了「停止排队」的时刻。
	CancelRequestedAt *time.Time
	// ExpiresAt 预留：图片过期时间。当前不自动删。
	ExpiresAt *time.Time
	// CreatedAt 提交时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
	// StartedAt 第一张开始跑的时刻。
	StartedAt *time.Time
	// FinishedAt 所有 item 到终态的时刻（终态判定那条 UPDATE 的守卫列）。
	FinishedAt *time.Time
	// SettledAt 账算清的时刻；非空的任务拒绝重试（409）。
	SettledAt *time.Time
	// UserDeletedAt 运营在界面删除任务（软删），账和图都还在。
	UserDeletedAt *time.Time
}

// IsAllItemsDone 判断四个计数是否已经填满 item_count。
// 只作为读侧的便利判断，**真正的终态判定必须用那条带守卫的 UPDATE**
// （WHERE finished_at IS NULL AND success+fail+cancelled = item_count），
// 否则 5 个 worker 都以为自己是最后一张，回调会发 5 次。
func (j *Job) IsAllItemsDone() bool {
	if j == nil {
		return false
	}
	return j.SuccessCount+j.FailCount+j.CancelledCount >= j.ItemCount
}

// IsSettled 账是不是已经算清。已结算的任务拒绝重试。
func (j *Job) IsSettled() bool {
	return j != nil && j.SettledAt != nil
}

// IsStopRequested 运营有没有点过「停止排队」。
func (j *Job) IsStopRequested() bool {
	return j != nil && j.CancelRequestedAt != nil
}

// ----------------------------------------------------------------------------
// 6. designkit_job_items —— 每一个「商品图 × 提示词」
// ----------------------------------------------------------------------------

// JobItem 是批次里的一张图。
//
// Seq 从 1 开始，展开顺序固定：图1×词1 → seq 1，图1×词2 → seq 2，图2×词1 → seq 3 …
// **所有查询必须显式 ORDER BY seq ASC**，有专门测试守着 ——
// 「按序号取第几张图」是发给 ERP 的对外契约。
type JobItem struct {
	// ID 自增主键。
	ID int64
	// JobID 所属批次。
	JobID int64
	// Seq 批次内序号，从 1 开始。
	Seq int
	// AssetID 用的哪张原图（无外键：原图删了历史照样能看）。
	AssetID *int64
	// VariantID 实际发给网关的是哪个预处理产物。
	VariantID *int64
	// PromptID 来自灵感库的哪条提示词；运营手打的为 nil。
	PromptID *int64
	// PromptText 提交那一刻的提示词原文快照。
	// 灵感库里的词以后被改措辞或删掉，历史记录仍显示当时用的原文。
	PromptText string
	// Status 见 ItemStatus。改它必须先过 CanTransitionItem。
	Status ItemStatus
	// AttemptCount 已经尝试过几次。**在「领取」那一刻就 +1**，不是成功后才加 ——
	// 否则进程崩一次，这一张会被永远重试下去。
	AttemptCount int
	// MaxAttempts 累计最多试几次；运营点重试也算在内。默认 3。
	MaxAttempts int
	// LastErrorCode 我方错误码（见 errors.go），界面显示中文时带上它方便截图报障。
	LastErrorCode *string
	// LastErrorMessage 上游返回的英文原文，**只给管理员看**。
	LastErrorMessage *string
	// BilledCost 这一张实际扣了多少钱（从 usage_logs 反查回填）。
	BilledCost *Money
	// BillingRequestID 存**带 client: 前缀的完整值**，即 StoredBillingRequestID 的返回值。
	// 少了前缀，拿它去 usage_logs 反查一条也查不到，结算会永远等不到账单。
	BillingRequestID *string
	// BillingMode 上游实际用的计价方式。
	BillingMode *string
	// BillingTier 实际落的计费档 1K/2K/4K（由上游返回图片的像素决定，不是我们请求的 size）。
	BillingTier *string
	// WorkerID 当前持有租约的 worker 标识。
	// 写回结果必须带 WHERE id=? AND worker_id=? AND status='running'，
	// 影响行数不等于 1 就**丢弃结果**（说明租约已被别人抢走）。
	WorkerID *string
	// LeaseExpiresAt 租约到期时间（默认 180 秒，每 60 秒续）。
	// 过期还停在 running 的就是僵尸，会被别的 worker 回收重跑。
	LeaseExpiresAt *time.Time
	// AvailableAt 最早可被领取的时间。失败后退避重试就是把它往后推。
	AvailableAt time.Time
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
	// StartedAt 第一次被领取的时刻。
	StartedAt *time.Time
	// FinishedAt 到终态的时刻。
	FinishedAt *time.Time
}

// CanRetry 判断这一张还能不能再试。
// 注意 max_attempts 是**累计**上限，运营手动点的重试也算在内（决策 20）。
func (it *JobItem) CanRetry() bool {
	if it == nil {
		return false
	}
	return it.AttemptCount < it.MaxAttempts
}

// ----------------------------------------------------------------------------
// 7. designkit_images —— 出好的结果图
// ----------------------------------------------------------------------------

// Image 是一张出好的结果图。
type Image struct {
	// ID 自增主键。
	ID int64
	// UID 对外的图片编号（ULID）。
	UID string
	// JobID 所属批次。
	JobID int64
	// ItemID 所属的那一张（图 × 词）。
	ItemID int64
	// Attempt 这张图是第几次尝试出的（跟 JobItem.AttemptCount 对应）。
	Attempt int
	// ImageIndex 同一次尝试里返回的第几张，从 1 开始。
	// 自建网关一次会不会返回多张还没实测，先留着，成本为零。
	ImageIndex int
	// IsCurrent 重试出了新图后，旧图置 false；界面/接口默认只给当前这版。
	IsCurrent bool
	// ObjectKey 相对 bucket 的路径，格式见 ImageObjectKey。
	// **URL 一律运行时生成，绝不入库**——对象存储默认签发 24 小时失效的链接，
	// 存进库过期后拼不回来。
	ObjectKey string
	// ContentType 默认 image/png。
	ContentType string
	// ByteSize 字节数。
	ByteSize int64
	// Width 上游实际返回的像素宽 —— **计费落哪档由它决定，必须记**。
	Width *int
	// Height 上游实际返回的像素高。
	Height *int
	// BillingTier 由像素分出来的档：1K / 2K / 4K。
	BillingTier *string
	// CreatedAt 入库时间。
	CreatedAt time.Time
	// DeletedAt 软删除。
	DeletedAt *time.Time
	// ExpiresAt 预留：图片永久保留，字段先建好，哪天硬盘紧了开开关就能清。
	ExpiresAt *time.Time
}

// ----------------------------------------------------------------------------
// 8. designkit_holds —— 我们自己的冻结账
// ----------------------------------------------------------------------------

// HoldStatus 冻结记录的状态。
type HoldStatus string

const (
	// HoldStatusOpen 还占着可用额。
	HoldStatusOpen HoldStatus = "open"
	// HoldStatusSettled 已按实际花费结清。
	HoldStatusSettled HoldStatus = "settled"
	// HoldStatusReleased 剩余部分已释放。
	HoldStatusReleased HoldStatus = "released"
)

// String 让 HoldStatus 能直接当 SQL 参数用。
func (s HoldStatus) String() string { return string(s) }

// Hold 是我们自己记的一笔冻结。
//
// **users.balance 全程只由上游扣一次，我们只在自己这张表上记账：**
//
//	可用额 = users.balance - SUM(designkit_holds.amount WHERE user_id=? AND status='open')
//
// 为什么不用上游自带的 Reserve/Capture/Release：四条全部验实、四条全部致命 ——
//  1. 上游 Reserve 是真的从 users.balance 里把钱划走的；
//  2. 上游每张图出图前的准入判据就是「balance <= 0」。余额 100 冻结 100，
//     本批次每一张都会被判余额不足，**一张也出不来**；
//  3. 上游 Release 的防幻影守卫把 hold 键硬编码成 batch_image_hold: 前缀，
//     我们的冻结它查不到，会「打一行日志、返回 nil」，钱永久卡在 frozen_balance 里，
//     **无任何报错**；
//  4. 上游 Capture 在「实际额 > 冻结额」时直接返错回滚，而实测计费档会向上漂，
//     实际额大于冻结额是常态。
type Hold struct {
	// ID 自增主键。
	ID int64
	// JobID 一个任务一行（UNIQUE）。重试时对同一行做 ON CONFLICT (job_id) DO UPDATE
	// 把额度加回去，**不要再插一行**。
	JobID int64
	// UserID 冻结记在谁头上。
	UserID int64
	// Amount 当前还冻着多少。每张图计费落地后按张递减。
	Amount Money
	// Status open / settled / released。
	Status HoldStatus
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// ----------------------------------------------------------------------------
// 9. designkit_billing_alerts —— 实际花费超出预估的告警
// ----------------------------------------------------------------------------

// 告警级别。
const (
	// BillingAlertLevelWarn 实际 > 预估 × 1.2。
	BillingAlertLevelWarn = "warn"
	// BillingAlertLevelCritical 实际 > 预估 × 2。
	BillingAlertLevelCritical = "critical"
)

// BillingAlert 是一条「实际花费超出预估」的记录。
//
// 结算时**绝不能**因为「实际 > 预估」就报错回滚 —— 钱已经被上游扣走了，
// 报错只会让任务卡死。改成分级记录。
type BillingAlert struct {
	// ID 自增主键。
	ID int64
	// JobID 哪个批次。
	JobID int64
	// Estimated 提交时的预估花费。
	Estimated Money
	// Actual 结算后的实际花费。
	Actual Money
	// RatioOver actual / estimated，例如 1.35。
	RatioOver Multiplier
	// Level warn / critical。
	Level string
	// CreatedAt 记录时间。
	CreatedAt time.Time
}

// ----------------------------------------------------------------------------
// 10. designkit_quota_requests —— 运营点「申请额度」留下的待办
// ----------------------------------------------------------------------------

// QuotaRequestStatus 额度申请的处理状态。
type QuotaRequestStatus string

const (
	// QuotaRequestPending 待处理。同一个人同时只能有一条（部分唯一索引挡着）。
	QuotaRequestPending QuotaRequestStatus = "pending"
	// QuotaRequestHandled 已加额度。
	QuotaRequestHandled QuotaRequestStatus = "handled"
	// QuotaRequestRejected 驳回。
	QuotaRequestRejected QuotaRequestStatus = "rejected"
)

// String 让 QuotaRequestStatus 能直接当 SQL 参数用。
func (s QuotaRequestStatus) String() string { return string(s) }

// QuotaRequest 是运营点「申请额度」留下的一条待办。
//
// 为什么要有这张表：充值和兑换码菜单对普通用户是隐藏的，运营看到「余额不足」
// 就是死路一条，而管理员这边没有任何机制会主动知道有人卡住了。
type QuotaRequest struct {
	// ID 自增主键。
	ID int64
	// UserID 申请人（上游 users.id）。
	UserID int64
	// Note 运营自己填的说明，可为空。
	Note *string
	// Status pending / handled / rejected。
	Status QuotaRequestStatus
	// HandledBy 处理人（上游 users.id）。
	HandledBy *int64
	// HandledAt 处理时间。
	HandledAt *time.Time
	// CreatedAt 申请时间。
	CreatedAt time.Time
}

// ----------------------------------------------------------------------------
// 11. designkit_sync_runs —— 灵感库每次同步的记录
// ----------------------------------------------------------------------------

// SyncKind 同步是谁触发的。
type SyncKind string

const (
	// SyncKindManual 管理员点的。
	SyncKindManual SyncKind = "manual"
	// SyncKindAuto 定时跑的。
	SyncKindAuto SyncKind = "auto"
)

// SyncStatus 一次同步的状态。
type SyncStatus string

const (
	// SyncStatusRunning 正在跑。
	SyncStatusRunning SyncStatus = "running"
	// SyncStatusSucceeded 跑完了。
	SyncStatusSucceeded SyncStatus = "succeeded"
	// SyncStatusFailed 跑挂了，原因在 Error 里。
	SyncStatusFailed SyncStatus = "failed"
	// SyncStatusSkipped 没抢到锁，说明已经有一次在跑。
	SyncStatusSkipped SyncStatus = "skipped"
)

// SyncRun 是灵感库同步的一次记录。
//
// 手动同步和自动同步**共用同一把 PostgreSQL advisory lock**（pg_try_advisory_lock），
// 不要用应用内 mutex —— 多副本部署时应用内锁根本不起作用。
// （老系统就是各用一把锁，并发把 1.4 万条提示词写重复了。）
type SyncRun struct {
	// ID 自增主键。
	ID int64
	// Kind manual / auto。
	Kind SyncKind
	// Status running / succeeded / failed / skipped。
	Status SyncStatus
	// StartedAt 开始时间。
	StartedAt time.Time
	// FinishedAt 结束时间；还在跑时为 nil。
	FinishedAt *time.Time
	// Fetched 从上游库拉回来多少条。
	Fetched int
	// Inserted 新增多少条。
	Inserted int
	// Updated 更新多少条。
	Updated int
	// Skipped 内容没变、跳过多少条。
	Skipped int
	// Error 失败原因；成功时为 nil。
	Error *string
}

// ----------------------------------------------------------------------------
// 12. designkit_settings —— designkit 自己的配置
// ----------------------------------------------------------------------------

// 配置项的键名。**唯一真相是数据库里的值**，代码里的默认值只是读不到时的兜底。
const (
	// SettingKeyRatios 可选出图比例，JSON 数组，界面按这个顺序显示。
	SettingKeyRatios = "ratios"
	// SettingKeyModel 默认出图模型。
	SettingKeyModel = "model"
	// SettingKeyMaxBatchItems 单次批量最多多少张，超过直接拒绝提交。
	SettingKeyMaxBatchItems = "max_batch_items"
	// SettingKeyItemTimeoutSeconds 单张图的超时秒数（队列租约也按这个量级）。
	SettingKeyItemTimeoutSeconds = "item_timeout_seconds"
	// SettingKeyMaxDimension 预处理后最长边上限像素。
	//
	// ⚠ 它直接决定出图落 2K 还是 4K 档。**Go 侧每次调 /v1/preprocess 都必须
	// 显式带上这个值**，Python 那边的环境变量只是「Go 忘了传」的兜底。
	// 不这么约定，管理员把它从 2048 改成 4096 而环境变量没跟着改，
	// 就会「界面说 4K、实际按 2K 出图」，而且不报错。
	SettingKeyMaxDimension = "max_dimension"
	// SettingKeyWorkerConcurrency 我们自己的出图并发上限，默认 4。
	// 上游每用户默认 5 并发，这里留一格给运营的单张操作。
	SettingKeyWorkerConcurrency = "worker_concurrency"
	// SettingKeyAdminContact 管理员联系方式，余额不足的提示里会显示，部署时填。
	SettingKeyAdminContact = "admin_contact"
)

// 配置项的兜底默认值。跟 9001 迁移里 INSERT 的预置值保持一致。
const (
	// DefaultMaxBatchItems 单次批量张数上限。
	DefaultMaxBatchItems = 50
	// DefaultItemTimeoutSeconds 单张超时秒数。
	DefaultItemTimeoutSeconds = 180
	// DefaultMaxDimension 预处理最长边上限。
	DefaultMaxDimension = 2048
	// DefaultWorkerConcurrency 出图并发上限。
	DefaultWorkerConcurrency = 4
	// DefaultModel 默认出图模型。
	DefaultModel = "gpt-image-2"
	// DefaultMaxAttempts 单张累计最多尝试几次（决策 20）。
	DefaultMaxAttempts = 3
	// DefaultLeaseSeconds 队列租约秒数。
	DefaultLeaseSeconds = 180
	// DefaultLeaseRenewSeconds 租约续期间隔秒数。
	DefaultLeaseRenewSeconds = 60
	// DefaultHeartbeatSeconds job 心跳间隔秒数。
	DefaultHeartbeatSeconds = 30
	// DefaultStaleJobMinutes 心跳超过这么久没动静就判定为僵尸。
	DefaultStaleJobMinutes = 10
)

// Setting 是 designkit_settings 的一行。
type Setting struct {
	// Key 配置项名字（主键）。
	Key string
	// Value 值。标量也用 JSONB 存（数字直接写 4，字符串写 "abc"）。
	Value json.RawMessage
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// ----------------------------------------------------------------------------
// 对象存储路径
// ----------------------------------------------------------------------------

// ObjectKeyPrefix 所有 designkit 对象的公共前缀（相对 bucket）。
const ObjectKeyPrefix = "designkit"

// ImageObjectKey 拼出结果图的 object_key，格式**定死**：
//
//	designkit/<YYYY>/<MM>/<DD>/<job_uid>-<seq>-<attempt>-<image_index>.png
//
// 相对 bucket、含 prefix、**不含域名不含签名**。
// URL 一律运行时生成，绝不入库。
func ImageObjectKey(createdAt time.Time, jobUID string, seq, attempt, imageIndex int) string {
	return ObjectKeyPrefix + "/" + datePath(createdAt) + "/" +
		jobUID + "-" + itoa(seq) + "-" + itoa(attempt) + "-" + itoa(imageIndex) + ".png"
}

// AssetObjectKey 拼出原始商品图的 object_key：
//
//	designkit/assets/<YYYY>/<MM>/<DD>/<asset_uid><ext>
//
// ext 带点，例如 ".jpg"；拿不到扩展名时传空串。
func AssetObjectKey(createdAt time.Time, assetUID, ext string) string {
	return ObjectKeyPrefix + "/assets/" + datePath(createdAt) + "/" + assetUID + ext
}

// VariantObjectKey 拼出预处理产物的 object_key：
//
//	designkit/variants/<YYYY>/<MM>/<DD>/<asset_uid>-<ratio>-<t|o>-<max_dimension><ext>
//
// 比例里的冒号换成 x（对象存储的键里带冒号会让一部分客户端和 CDN 出问题）。
// t = 保留透明，o = 合成白底（opaque）。
func VariantObjectKey(createdAt time.Time, assetUID string, ratio Ratio, keepTransparency bool, maxDimension int, ext string) string {
	transparency := "o"
	if keepTransparency {
		transparency = "t"
	}
	safeRatio := strings.ReplaceAll(string(ratio), ":", "x")
	return ObjectKeyPrefix + "/variants/" + datePath(createdAt) + "/" +
		assetUID + "-" + safeRatio + "-" + transparency + "-" + itoa(maxDimension) + ext
}

// datePath 返回 <YYYY>/<MM>/<DD>。一律用 UTC，避免部署时区变了以后
// 同一张图算出两个不同的 key。
func datePath(t time.Time) string {
	return t.UTC().Format("2006/01/02")
}

// itoa 是 strconv.Itoa 的本地别名，纯粹为了让上面的拼接读起来短一点。
func itoa(v int) string {
	return strconv.Itoa(v)
}
