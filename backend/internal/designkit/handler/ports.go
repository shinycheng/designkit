package handler

import (
	"context"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ============================================================================
// handler 依赖的服务接口（消费方定义）
// ============================================================================
//
// 这些接口**由 handler 声明、由 service 包实现**，这样：
//   - handler 单测可以塞假实现，不需要数据库、对象存储和上游网关；
//   - service 包不需要 import handler（否则就是循环依赖）。
//
// 约定：
//   - 一律只在参数和返回值里出现 designkit/domain 的类型和本文件的输入输出结构体，
//     **不出现 *gin.Context、*sql.DB、上游 service 的类型**；
//   - 归属校验（这条记录是不是这个 userID 的）由 service/repository 负责，
//     查不到一律返回 domain.ErrNotFound 或对应的 *DesignkitError，
//     **不要返回 403** —— 那会泄露「这个任务号存在」；
//   - 面向运营的中文文案由 domain.NewError 生成，service 返回 *DesignkitError 即可，
//     handler 直接照着它的 Code / Message / HTTPStatus 回给客户端。

// Services 是本轮业务端点需要的全部服务实现。
// 任何一个为 nil，RegisterBusinessRoutes 就整组不注册（只留健康检查），
// 这样 service 还没落地时进程照样能起来。
type Services struct {
	// Assets 商品图上传与读取。
	Assets AssetService
	// Catalog 出图比例等配置读取。
	Catalog CatalogService
	// Jobs 批次的报价、提交、查询、停止排队、重试。
	Jobs JobService
	// Images 结果图读取。
	Images ImageService

	// Me 「我的消费」：本月用量汇总 + 申请额度。
	//
	// **刻意不算进 Ready()**：它只依赖数据库，缺席的唯一后果是工作台角落那三个数
	// 和「申请额度」按钮不可用。为这一块把出图端点整组下线，代价完全不成比例。
	Me MeService

	// Prompts 灵感库的浏览（分类、检索、看一条）。所有登录用户都能用。
	//
	// **同样刻意不算进 Ready()**：它只依赖数据库。缺席的唯一后果是
	// 「灵感库」那一页空着，运营还可以手打提示词照常出图 ——
	// 为它把出图端点整组下线完全不成比例。
	Prompts PromptService

	// MyPrompts 「我的提示词」（运营自建：新建 / 修改 / 删除）。所有登录用户都能用。
	// 读取走 Prompts（列表带 source=user 过滤）。
	//
	// **同样刻意不算进 Ready()**：只依赖数据库。缺席时那三条写端点整组不挂
	// （裸 404，前端据此显示「还没准备好」），浏览和出图照常。
	MyPrompts MyPromptService

	// PromptSync 灵感库同步（触发同步、看同步状态、选同步用的代理）。**只有管理员能用。**
	//
	// 缺席时那四个管理端点不注册，浏览照常 —— 运营那一页不受影响。
	PromptSync PromptSyncService

	// Suggest 「AI 挑提示词」（读商品图 → 分类里挑 5 条 → 合成 1 条）。所有登录用户都能用。
	//
	// **同样刻意不算进 Ready()**：它比浏览多要对象存储和出图网关，
	// 缺席时只关掉这一条端点，前端会显示「AI 推荐还没准备好，请联系管理员」，
	// 并提示运营去灵感库自己挑一条 —— 出图照常。
	Suggest SuggestService

	// Settings designkit 自己的配置读写（出图比例、模型、并发、单价…）。**只有管理员能用。**
	//
	// **同样刻意不算进 Ready()**：它只依赖数据库。缺席时「商品图设置」那一页不可用，
	// 出图照常按数据库里已有的配置跑 —— 为一个设置页把出图端点整组下线完全不成比例。
	Settings SettingsService

	// QuotaAdmin 额度申请的管理端（列表 + 通过/驳回）。**只有管理员能用。**
	//
	// **同样刻意不算进 Ready()**：缺席时只有「额度申请」那一页不可用（裸 404），
	// 运营提交申请（Me）和出图都照常。
	QuotaAdmin QuotaAdminService

	// AdminRecords 「用户记录」管理端（跨用户查看对话与出图批次）。**只有管理员能用。**
	//
	// **同样刻意不算进 Ready()**：缺席时只有那几条 /admin/records/* 不挂（裸 404），
	// 其余照常。
	AdminRecords AdminRecordsService

	// Chat 「AI 对话」页（决策 38：会话保存 / 能发图 / 所有运营可用）。
	//
	// **同样刻意不算进 Ready()**：缺席时只关掉对话那四个端点，
	// 前端对话页显示「还没准备好」，出图和灵感库照常。
	Chat ChatConversationService

	// Upscale 「高清放大」（Real-ESRGAN ×4，跑在本地 imgsvc，不花钱）。
	//
	// **同样刻意不算进 Ready()**：缺席时只关掉放大那两个端点（裸 404，
	// 前端据此显示「放大功能还没准备好，请联系管理员」），上传和出图照常。
	Upscale UpscaleService
}

// ---- 高清放大 ----

// UpscaleTaskView 一次放大任务的状态快照。
type UpscaleTaskView struct {
	// AssetUID 被放大的那张商品图（任务按它去重）。
	AssetUID string
	// Status queued / running / done / failed，字面量即对外契约。
	Status string
	// ResultAsset done 时的产物：一条新的商品图（sha256 去重）。
	ResultAsset *dkdomain.Asset
	// ErrorMessage failed 时给运营看的中文。
	ErrorMessage string
	// ErrorCode failed 时的我方错误码（DK_ 前缀）。
	ErrorCode string
	// CreatedAt / UpdatedAt 入队时间和最近一次状态变化时间。
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UpscaleService 「高清放大」的排队与状态查询。
//
// 任务落库（designkit_upscale_tasks，9004）：重启后没放完的自动续跑，
// 运营不用重点。队列信道仍在内存里（单实例约束不变），
// 结果入库按 sha256 去重，不会重复占盘。
type UpscaleService interface {
	// StartUpscale 排一张进队列。同一张图已在排队/在放/放完时**不重复入队**，
	// 直接返回现有任务；failed 的可以重新排。队列满返回 DK_UPSCALE_QUEUE_FULL。
	StartUpscale(ctx context.Context, userID int64, origin dkdomain.Origin, assetUID string) (*UpscaleTaskView, error)

	// UpscaleStatus 查一张图的放大任务。没有（或不是他的）返回 DK_UPSCALE_NOT_FOUND。
	UpscaleStatus(ctx context.Context, userID int64, assetUID string) (*UpscaleTaskView, error)
}

// ChatSendInput 发一条对话消息。
type ChatSendInput struct {
	// UserID 谁在聊。
	UserID int64
	// SessionUID 目标会话；空串 = 新建。
	SessionUID string
	// Text 正文。
	Text string
	// AssetUIDs 附带的商品图，最多 3 张（上限在 service 层守）。
	AssetUIDs []string
}

// ChatSendResult 一次发送的产出。
type ChatSendResult struct {
	Session          *dkdomain.ChatSession
	UserMessage      *dkdomain.ChatMessage
	AssistantMessage *dkdomain.ChatMessage
}

// ChatConversationService 是「AI 对话」这一组能力。
type ChatConversationService interface {
	// Send 发一条消息并等 AI 回复。这一步**花钱**（按 token 计费）。
	Send(ctx context.Context, in ChatSendInput) (*ChatSendResult, error)
	// SendStream 同 Send，但回复**边生成边**通过 onDelta 逐段回调
	//（同一 goroutine、按顺序）。返回值与 Send 相同：流完且拿到全文才落库，
	// 中途失败什么都不留。同样**花钱**。
	SendStream(ctx context.Context, in ChatSendInput, onDelta func(text string)) (*ChatSendResult, error)
	// ListSessions 会话列表（最近的在前）。
	ListSessions(ctx context.Context, userID int64) ([]*dkdomain.ChatSession, error)
	// GetSession 一个会话和它的全部消息（按发生顺序）。
	GetSession(ctx context.Context, userID int64, uid string) (*dkdomain.ChatSession, []*dkdomain.ChatMessage, error)
	// DeleteSession 删除会话。
	DeleteSession(ctx context.Context, userID int64, uid string) error
}

// Ready 判断四个**出图必需**的服务是不是都就绪了。
//
// Me 不在里面，理由见 Me 字段上的注释。
func (s Services) Ready() bool {
	return s.Assets != nil && s.Catalog != nil && s.Jobs != nil && s.Images != nil
}

// ContentBlob 是一段可以直接写进 HTTP 响应体的二进制内容。
//
// 为什么不返回 URL：对象存储默认签发 24 小时失效的链接，
// 而且「必须经我方鉴权才能看图」只能靠我们自己的下载端点实现（设计定型 第二·五节 C）。
type ContentBlob struct {
	// Data 文件字节。
	Data []byte
	// ContentType MIME 类型，空串时由 handler 兜底成 application/octet-stream。
	ContentType string
	// Filename 建议的下载文件名，空串表示不下发 Content-Disposition。
	Filename string
}

// ---- 素材 ----

// CreateAssetInput 上传一张商品图。
type CreateAssetInput struct {
	// UserID 上传人。
	UserID int64
	// Origin web=界面上传；erp=外部系统走接口上传。由挂载前缀决定，不看鉴权方式。
	Origin dkdomain.Origin
	// Filename 原始文件名，用来取扩展名。
	Filename string
	// ContentType 浏览器给的 MIME 类型（已由 handler 校验在白名单里）。
	ContentType string
	// Data 原图字节。
	Data []byte
}

// AssetService 商品图的上传与读取。
type AssetService interface {
	// CreateAsset 存原图（同一个人传过同样的字节时可以直接复用已有记录）。
	CreateAsset(ctx context.Context, in CreateAssetInput) (*dkdomain.Asset, error)

	// GetAsset 按对外编号取一张原图，校验归属。
	GetAsset(ctx context.Context, userID int64, uid string) (*dkdomain.Asset, error)

	// OpenAssetContent 取原图字节，校验归属。
	OpenAssetContent(ctx context.Context, userID int64, uid string) (*ContentBlob, error)

	// RemoveBackgroundAsset 一键白底图：抠掉背景、合成白底，存成一条**新的**商品图。
	// 不花钱（走本地 rembg，不经过出图网关）。抠图服务没配置时返回带中文文案的
	// *DesignkitError（「白底图功能还没准备好，请联系管理员。」）。
	RemoveBackgroundAsset(ctx context.Context, userID int64, uid string, origin dkdomain.Origin) (*dkdomain.Asset, error)

	// DeleteAsset 删除一张商品图（软删），校验归属。
	//
	// 只影响商品图列表：历史批次的记录和结果图**不受影响**
	// （item 存的是提示词快照和结果图，job_items.asset_id 刻意没建外键）。
	// 对象存储里的文件也不删（决策 17：图片永久保留）。
	DeleteAsset(ctx context.Context, userID int64, uid string) error
}

// ---- 配置 / 报价 ----

// RatioOption 是 GET /ratios 里的一个比例。
type RatioOption struct {
	// Ratio 比例串，例如 "3:4"。
	Ratio dkdomain.Ratio
	// TargetWidth 补白边并限制最长边之后的目标宽（只用于展示，不是计费依据）。
	TargetWidth int
	// TargetHeight 目标高。
	TargetHeight int
	// PricingTier 预估档位 1K/2K/4K。
	//
	// ⚠ 真正计费的档由上游按**实际输出像素**定：实测请求 1:1 拿回 1254×1254，
	// 落的是 2K 不是 1K。所以这里只是预估。
	PricingTier string
	// UnitPrice 单张预估价；**nil = 价格待确认**。
	//
	// 每个比例实际出多大像素、落哪档、单价填多少，必须真出图实测才知道
	// （设计定型 第十节 1 是上线阻塞项）。测出来之前一律给 nil，
	// 界面显示「价格待确认」，不许显示任何猜的单价。
	UnitPrice *dkdomain.Money
	// IsDefault 界面默认选中这一个。
	IsDefault bool
}

// CatalogService 出图比例等配置的读取（值来自 designkit_settings）。
type CatalogService interface {
	// ListRatios 返回允许的出图比例，顺序即界面显示顺序。
	ListRatios(ctx context.Context) ([]RatioOption, error)
}

// ---- 批次 ----

// JobSpec 是「这一批要出哪些图」的描述，报价和提交共用。
//
// 展开顺序固定为**商品图外层、提示词内层**：
// 图1×词1 → seq 1，图1×词2 → seq 2，图2×词1 → seq 3 …
// 运营是按商品思考的，而且「按序号取第几张图」是发给 ERP 的对外契约。
type JobSpec struct {
	// UserID 提交人。
	UserID int64
	// Origin web / erp，由挂载前缀决定。
	Origin dkdomain.Origin
	// Ratio 整批统一的出图比例（handler 只校验格式，白名单在 service 侧比对配置）。
	Ratio dkdomain.Ratio
	// AssetUIDs 选中的商品图，顺序即展开时的外层顺序。
	AssetUIDs []string
	// PromptUIDs 选中的灵感库提示词，顺序即展开时的内层顺序。
	PromptUIDs []string
	// PromptTexts 运营手打的提示词，接在 PromptUIDs 后面。
	PromptTexts []string
	// Model 出图模型；空串表示用 designkit_settings.model。
	Model string
	// KeepTransparency 预处理时是否保留透明底（false = 合成白底）。
	// 已是定值：「请求没传用默认」在 buildSpec 里 ResolveKeepTransparency 落定过。
	// 9004 起随批落库（designkit_jobs.keep_transparency），出图按批生效。
	KeepTransparency bool
}

// CreateJobInput 提交一个批次。
type CreateJobInput struct {
	JobSpec

	// Name 运营给这批起的名字，可为空串。
	Name string
	// CallbackURL 完成回调地址，空串 = 不回调。
	CallbackURL string
	// IdempotencyKey 客户端送来的防重复标识。
	// **浏览器端也必须带**（打开确认弹窗时生成一个 UUID 存住），
	// 否则运营手滑双击 = 两个 job 两份钱。handler 已保证它非空。
	IdempotencyKey string
	// APIKeyID 记在哪把 Key 上；0 表示让 service 用该用户的内部专用 Key。
	//
	// ERP 走上游 apiKeyAuth，上下文里有真实的 Key；浏览器端没有，
	// 由 service 兜底（designkit_jobs.api_key_id 是 NOT NULL，
	// 出图时上游 Images() 取不到 Key 直接 401）。
	APIKeyID int64
}

// EstimateResult 是 POST /jobs/estimate 的结果。
type EstimateResult struct {
	// ItemCount 展开后的总张数 = 商品图数 × 提示词数。
	ItemCount int
	// AssetCount 商品图数。
	AssetCount int
	// PromptCount 提示词数。
	PromptCount int
	// MaxBatchItems 单次批量上限（designkit_settings.max_batch_items）。
	MaxBatchItems int
	// PricingTier 预估档位。
	PricingTier string
	// UnitPrice 单张预估价；nil = 价格待确认（见 RatioOption.UnitPrice）。
	UnitPrice *dkdomain.Money
	// EstimatedCost 这一批的预估花费；UnitPrice 为 nil 时也为 nil。
	EstimatedCost *dkdomain.Money
	// PriceNote 价格待确认时给运营看的**中文说明**；单价确认之后是空串。
	//
	// 没有它，界面上就是一个不带任何解释的空价格 —— 运营不知道是系统坏了
	// 还是这一批不要钱。这句话必须原样透到界面上。
	PriceNote string
	// Balance 当前余额。
	Balance dkdomain.Money
	// Available 当前可用额 = 余额 - 已冻结。
	Available dkdomain.Money
	// Sufficient 可用额够不够这一单。价格待确认时一律给 true（拦不住也不该假装拦得住）。
	Sufficient bool
	// Shortfall 还差多少（Sufficient 为 true 时是 0）。界面要显示出来。
	Shortfall dkdomain.Money
}

// JobItemView 是一张图（图 × 词）连同它当前版本的结果图。
type JobItemView struct {
	// Item 这一张的记录。
	Item *dkdomain.JobItem
	// Images 当前版本的结果图，**按 image_index 升序**。
	Images []*dkdomain.Image
}

// ImageView 是一张结果图连同它的定位信息。
//
// Image 实体里只有内部主键 job_id / item_id，对外不能暴露，
// 所以这里补上任务号和序号，让 ERP 能把图片对回「第几批的第几张」。
type ImageView struct {
	// Image 图片记录。
	Image *dkdomain.Image
	// JobUID 所属批次的对外任务号。
	JobUID string
	// Seq 所属 item 的批次内序号，从 1 开始。
	Seq int
}

// JobService 批次的报价、提交与查询。
type JobService interface {
	// EstimateJob 只算账不建任何东西。
	EstimateJob(ctx context.Context, spec JobSpec) (*EstimateResult, error)

	// CreateJob 提交一个批次：在一个事务里查可用额、建 job / items / hold。
	// 可用额不够返回 *domain.InsufficientBalanceError。
	CreateJob(ctx context.Context, in CreateJobInput) (*dkdomain.Job, error)

	// GetJob 按对外任务号取批次，校验归属。
	GetJob(ctx context.Context, userID int64, jobUID string) (*dkdomain.Job, error)

	// ListJobs 批次列表，游标分页（created_at + id 复合游标，不用 offset）。
	ListJobs(ctx context.Context, query dkdomain.ListJobsQuery) (*dkdomain.JobPage, error)

	// ListJobItems 取一个批次的全部 item，**必须按 seq 升序**。
	// handler 还会再排一次序做兜底（ERP 契约靠它）。
	ListJobItems(ctx context.Context, userID int64, jobUID string) ([]*JobItemView, error)

	// GetJobItem 按序号取某一张。seq 从 1 开始。
	GetJobItem(ctx context.Context, userID int64, jobUID string, seq int) (*JobItemView, error)

	// OpenJobItemContent 取某一张当前版本的结果图字节。
	// imageIndex 从 1 开始；同一次尝试返回多张时才会用到大于 1 的值。
	OpenJobItemContent(ctx context.Context, userID int64, jobUID string, seq, imageIndex int) (*ContentBlob, error)

	// StopJob 「停止排队」（决策 21）：把**还没开始**的那几张砍掉，
	// **已经在跑的必须等它自然落地**并照常扣费 —— 上游出图跟浏览器连接是
	// 故意脱钩的，点了也停不下来。批次已经结束时返回
	// DK_ILLEGAL_STATE_TRANSITION（「这批任务已经结束了，不用再停。」）。
	StopJob(ctx context.Context, userID int64, jobUID string) (*StopJobResult, error)

	// RetryJobItem 重试一张（决策 20）。
	//
	// **重试 = 重新出一张图 = 重新收一次钱**，所以走跟提交完全同一套准入
	// （单张预估 → 查可用额 → 不够就拒绝）。可能的拒绝：
	// DK_MAX_ATTEMPTS_EXCEEDED（累计次数到顶）、DK_JOB_ALREADY_SETTLED（已结算）、
	// DK_INSUFFICIENT_BALANCE（余额不够）。
	RetryJobItem(ctx context.Context, in RetryItemInput) (*dkdomain.JobItem, error)

	// DeleteJob 删除一批记录（软删），校验归属。
	//
	// 删的只是可见性：这一批和它的结果图从「我的图片」消失，
	// 数据行、图片文件、账单都还在，**已扣的费用不退**。
	// 没结束的批次拒绝删除（DK_ILLEGAL_STATE_TRANSITION）——
	// 删掉一个在跑的批次等于把「停止排队」的入口藏起来（决策 21 的语义）。
	DeleteJob(ctx context.Context, userID int64, jobUID string) error
}

// StopJobResult 是一次「停止排队」的结果。
//
// 两个计数**都要显示给运营看**（决策 21 的弹窗文案就是拿它们填的）：
// 「还有 N 张没开始，停止后不再生成也不扣费；正在生成的 M 张会跑完并正常扣费，
// 图会保存在作品库。」
type StopJobResult struct {
	// Job 停完之后的批次（状态和四个计数都已经是最新的）。
	Job *dkdomain.Job
	// Cancelled 这次砍掉了几张 —— 只统计「还没开始就被停掉的」，**这部分不花钱**。
	Cancelled int
	// StillRunning 还有几张正在生成，必须等它们自然落地，**这部分照样扣费**。
	StillRunning int
}

// RetryItemInput 重试一张要的参数。
type RetryItemInput struct {
	// UserID 谁点的重试（校验归属）。
	UserID int64
	// JobUID 哪个批次。
	JobUID string
	// Seq 第几张，从 1 开始。
	Seq int
	// IdempotencyKey 客户端送来的防重复标识，**必填**（handler 已保证非空）。
	// 运营双击必然发生，而每点一次就是重新出一张图、重新收一次钱。
	IdempotencyKey string
}

// ImageService 结果图的读取。
type ImageService interface {
	// GetImage 按图片编号取一张结果图，校验归属。
	GetImage(ctx context.Context, userID int64, uid string) (*ImageView, error)

	// OpenImageContent 取结果图字节，校验归属。
	OpenImageContent(ctx context.Context, userID int64, uid string) (*ContentBlob, error)
}

// ---- 我的消费 ----

// UsageSummary 是工作台角落那三个数（决策 16）：
// 「本月出图 N 张 / 花费 $X / 余额 $Y」。金额一律美元（决策 18）。
type UsageSummary struct {
	// PeriodStart 统计窗口起点（本月 1 号 00:00，UTC）。
	PeriodStart time.Time
	// PeriodEnd 统计窗口终点（下月 1 号 00:00，UTC），**不含**这一刻。
	PeriodEnd time.Time
	// ImageCount 窗口内出成功的张数（重试出的每一张都真花过钱，所以也算在内）。
	ImageCount int
	// Cost 窗口内的花费。按上游账单自己的时间落窗口，跨月重试不会把钱挪月份。
	Cost dkdomain.Money
	// Balance 当前余额。
	Balance dkdomain.Money
	// Available 当前可用额 = 余额 - 已冻结（还没出完的批次占着的那部分）。
	Available dkdomain.Money
	// AdminContact 管理员联系方式（designkit_settings.admin_contact）。
	//
	// 余额不足时界面必须把它显示出来（决策 19）：充值和兑换码菜单对运营是隐藏的，
	// 没有联系方式就是死路一条。没配置时是空串，界面不显示那一行。
	AdminContact string
}

// QuotaRequestResult 是一次「申请额度」的结果。
type QuotaRequestResult struct {
	// Request 刚建好的那条待办。
	Request *dkdomain.QuotaRequest
	// AdminContact 管理员联系方式，拼进「已经通知管理员了」那句话里。
	AdminContact string
}

// MeService 「我的消费」：本月用量汇总 + 申请额度。
//
// **只依赖 repository。** 它不读图也不写图 —— 别把对象存储吊上来：
// 存储目录建不出来时，工作台角落这三个数照样该显示。
type MeService interface {
	// GetUsageSummary 本月出图 N 张 / 花费 $X / 余额 $Y。
	// 时间窗（本月）由实现算，handler 不关心。
	GetUsageSummary(ctx context.Context, userID int64) (*UsageSummary, error)

	// CreateQuotaRequest 运营点「申请额度」，在管理员后台生成一条待处理记录。
	// note 是运营自己填的说明，可以是空串。
	//
	// 同一个人已经有一条待处理时返回 domain.ErrConflict（部分唯一索引挡着，
	// 防连点刷屏）—— handler 会把它翻成「不用重复提交」。
	CreateQuotaRequest(ctx context.Context, userID int64, note string) (*QuotaRequestResult, error)
}

// ---- 额度申请（管理端，决策 19 的闭环）----

// QuotaRequestAdminList 管理端列表的一页。
type QuotaRequestAdminList struct {
	// Items 当前 tab 的行。
	Items []*dkdomain.QuotaRequestDetail
	// PendingCount 全部待处理条数（侧边栏红点 + 「待处理」tab 的角标）。
	// 跟 Items 无关：看「已处理」tab 时它也照给。
	PendingCount int
}

// HandleQuotaRequestInput 管理员处理一条申请的输入。
type HandleQuotaRequestInput struct {
	// ID 申请的自增主键（管理端接口暴露它，同上游用户管理的惯例；
	// 运营侧接口仍然不暴露，见 quotaRequestDTO 的注释）。
	ID int64
	// AdminUserID 处理人（当前登录的管理员）。
	AdminUserID int64
	// Approve true=通过并加额，false=驳回。
	Approve bool
	// Note 处理备注，可为空串。
	Note string
	// Amount 通过时要加的金额（美元）。Approve=true 时必须 > 0；驳回时忽略。
	Amount dkdomain.Money
}

// QuotaAdminService 额度申请的管理端。**只有管理员路由挂它。**
type QuotaAdminService interface {
	// ListQuotaRequests pendingOnly=true 看待处理，false 看处理过的（通过 + 驳回）。
	ListQuotaRequests(ctx context.Context, pendingOnly bool, limit, offset int) (*QuotaRequestAdminList, error)

	// HandleQuotaRequest 通过（标记 + 给申请人加余额）或驳回（只标记）。
	// 返回处理后的那一行。
	//   - 行不存在 → domain.ErrNotFound
	//   - 已被处理过（含两个管理员同时点）→ domain.ErrConflict
	//   - 标记成功但加余额失败 → 带中文 message 的 DesignkitError，
	//     实现内部已尽力把行退回 pending，文案里写清楚了还能不能直接重试
	HandleQuotaRequest(ctx context.Context, in HandleQuotaRequestInput) (*dkdomain.QuotaRequest, error)
}

// ---- 用户记录（管理端）----

// AdminRecordsService 「用户记录」：管理员跨用户查看对话会话与出图批次。
//
// **只有管理员路由挂它**（register_business.go 的 RequireAdmin），
// 所以实现侧**刻意不做归属校验** —— 这是本模块里唯一一组
// 「不带 userID 也能按 uid 取记录」的接口，绝不能挂到非管理员的路由上。
//
// 软删口径跟用户自己看到的一致：会话 deleted_at、批次 user_deleted_at
// 打了软删标记的**一律不出现**（列表和详情都是），不把人家删掉的翻出来。
type AdminRecordsService interface {
	// ListRecordUsers 有记录的账户（会话或批次至少一条未删记录），给筛选下拉用。
	ListRecordUsers(ctx context.Context) ([]*dkdomain.RecordUser, error)

	// ListChatSessionRecords 会话列表，**按 updated_at 降序**（仓储 SQL 定死）。
	// userID=0 看全部账户。
	ListChatSessionRecords(ctx context.Context, userID int64, limit, offset int) ([]*dkdomain.ChatSessionAdminView, error)

	// GetChatSessionRecord 一个会话 + 全部消息（**按 id 升序**，就是对话顺序）。
	// 不存在或已被用户删掉一律 domain.ErrNotFound 形态（DK_CHAT_SESSION_NOT_FOUND）。
	GetChatSessionRecord(ctx context.Context, uid string) (*dkdomain.ChatSessionAdminView, []*dkdomain.ChatMessage, error)

	// ListJobRecords 批次列表，**按 created_at 降序**（仓储 SQL 定死）。
	// userID=0 看全部账户。
	ListJobRecords(ctx context.Context, userID int64, limit, offset int) ([]*dkdomain.JobAdminView, error)

	// GetJobRecord 一个批次 + 每一张（**按 seq 升序**，带 has_image）。
	GetJobRecord(ctx context.Context, uid string) (*dkdomain.JobAdminView, []*dkdomain.JobItemAdminView, error)

	// OpenJobRecordItemContent 取某一张当前版本第一张结果图的字节（缩略图用）。
	// seq 从 1 开始。
	OpenJobRecordItemContent(ctx context.Context, jobUID string, seq int) (*ContentBlob, error)

	// OpenAssetRecordContent 跨用户取一张商品图的字节（对话附图的缩略图用）。
	// 素材被主人删掉时同「找不到」；记录在、文件不在报存储错误。
	OpenAssetRecordContent(ctx context.Context, uid string) (*ContentBlob, error)
}

// ---- 灵感库 ----

// PromptCategoryView 是灵感库左侧那一列分类里的一项。
//
// **对外不暴露 category_id**（那是自增主键，对外只暴露 uid / slug，
// 见 9001 迁移的文件头）。分类表没有 uid 列，所以对外的标识就是 slug ——
// GET /prompts?category=<slug> 用的也是它。
type PromptCategoryView struct {
	// Category 分类本体。
	Category *dkdomain.PromptCategory
	// PromptCount 这个分类下**没下架也没删**的提示词条数，界面显示成「（1234）」。
	PromptCount int
}

// ListPromptSourceMine 是 GET /prompts 里 source 参数的取值：「我的提示词」。
// 跟 promptDTO.Source 的字面量一致（user），别再发明第三个词。
const ListPromptSourceMine = "user"

// ListPromptsInput 是 GET /prompts 的检索条件。
//
// **一律游标分页**（cursor.go 的注释说了为什么不用 offset）。
// 灵感库是稳定的目录型数据，游标只需要一个 id，不需要复合游标。
type ListPromptsInput struct {
	// CategorySlug 按分类过滤；空串 = 全部分类。
	// 传了一个不存在的 slug 时，实现应当返回空页而不是报错 ——
	// 分类刚被同步改名时，界面上那个旧链接不该变成一个红色报错。
	CategorySlug string
	// Source 空串 = 共享目录（youmind，全站一样）；
	// "user" = 只看 ViewerUserID 自己存的（「我的提示词」）。
	//
	// ⚠ 实现必须保证：**任何取值下都带不出别人的自建词**。
	Source string
	// ViewerUserID 当前登录用户。Source="user" 时按它过滤归属。
	ViewerUserID int64
	// Keyword 关键词，标题和正文都搜。**实现必须用 ILIKE**（PostgreSQL 的 LIKE 区分大小写，
	// 用 LIKE 会出现「搜 dress 搜不到 Dress」这种运营根本想不到的结果）。
	Keyword string
	// CursorID 上一页最后一条的 id；0 表示第一页。**这个值不对外暴露**，
	// handler 会把它编码成不透明游标再给客户端。
	CursorID int64
	// Limit 每页条数，handler 已经夹到 [1, maxPageLimit]。
	Limit int
}

// PromptView 是一条提示词连同它所属分类的展示信息。
//
// 单独包一层是因为 Prompt 实体里只有 CategoryID（自增主键，对外不能暴露），
// 而界面上要显示分类名字。
type PromptView struct {
	// Prompt 提示词本体。
	Prompt *dkdomain.Prompt
	// CategorySlug 所属分类的稳定标识；没有分类时是空串。
	CategorySlug string
	// CategoryName 所属分类的中文名（界面默认中文）；没有分类时是空串。
	CategoryName string
}

// PromptPage 是检索结果的一页。
type PromptPage struct {
	// Items 这一页的提示词，**按 id 升序**（游标靠它推进）。
	Items []*PromptView
	// Total 命中总数（**不受游标影响**），界面显示「共 N 条」。
	Total int
	// HasMore 还有没有下一页。
	HasMore bool
	// NextCursorID 下一页的游标；HasMore 为 false 时是 0。
	NextCursorID int64
}

// PromptService 灵感库的浏览。**所有登录用户都能用**。
// 共享目录（youmind）不做归属过滤；运营自建的词（source=user）**只有本人可见**，
// 过滤责任在实现侧（列表按 Source/ViewerUserID，详情按归属）。
type PromptService interface {
	// ListCategories 分类列表，按 (sort_order, id) 排，顺序即界面显示顺序。
	ListCategories(ctx context.Context) ([]*PromptCategoryView, error)

	// ListPrompts 检索提示词，游标分页。只返回没下架、没删的。
	ListPrompts(ctx context.Context, in ListPromptsInput) (*PromptPage, error)

	// GetPrompt 按对外编号取一条。viewerUserID 是当前登录用户 ——
	// 别人的自建词一律「找不到」（不泄露编号存在）。
	//
	// **这里不过滤 is_enabled**：运营收藏夹里那条词后来被管理员下架了，
	// 点开详情仍要看得到，否则他只会看到一个莫名其妙的 404。
	GetPrompt(ctx context.Context, viewerUserID int64, uid string) (*PromptView, error)
}

// MyPromptService 「我的提示词」的写入。**所有登录用户都能用**，
// 但每条只有归属人自己能改能删；youmind 来源的一律拒绝（会被自动同步覆盖回去）。
//
// 读取刻意不放在这里：列表 / 详情走 PromptService（同一套分页、同一个 DTO），
// 前端「我的提示词」那个页签只是 GET /prompts?source=user。
type MyPromptService interface {
	// CreateMyPrompt 存一条（source=user，归属当前用户）。
	// 每人上限 200 条，超了返回带中文文案的 DK_INVALID_REQUEST。
	CreateMyPrompt(ctx context.Context, userID int64, title, body string) (*dkdomain.Prompt, error)

	// UpdateMyPrompt 改标题和正文。别人的词返回 domain.ErrNotFound；
	// youmind 来源返回 DK_INVALID_REQUEST（「灵感库的提示词不能修改」）。
	UpdateMyPrompt(ctx context.Context, userID int64, uid, title, body string) (*dkdomain.Prompt, error)

	// DeleteMyPrompt 软删一条自己的。历史任务里的提示词快照不受影响。
	DeleteMyPrompt(ctx context.Context, userID int64, uid string) error
}

// ----------------------------------------------------------------------------
// AI 挑提示词（2026-08-14）
// ----------------------------------------------------------------------------

// SuggestPromptInput 一次推荐请求。
type SuggestPromptInput struct {
	// UserID 谁在推荐。
	UserID int64
	// AssetUID 主商品图。必填 —— 整套流程的前提是「让模型看见这张图」。
	AssetUID string
	// ExtraAssetUIDs 这一批里的其余商品图，可空。整批一起看，写出来的词才适合整批。
	ExtraAssetUIDs []string
	// CategorySlug 分类的稳定标识；**空串 = 全部**（后端会让模型自己判该按哪一类挑）。
	//
	// ⚠ 是 slug 不是 uid。分类对外从来没有 uid，见 promptCategoryDTO 的注释。
	CategorySlug string
	// Features 运营填的「商品特点」，可空。
	Features string
	// Force 跳过缓存、强制重新推荐（「重新推荐」按钮）。新结果照样回写缓存。
	Force bool
}

// SuggestedCategory 这次实际按哪个分类挑的。
type SuggestedCategory struct {
	Slug string
	Name string
}

// SuggestedCandidate 模型参考过的一条。给运营看依据，别让推荐变成黑盒。
type SuggestedCandidate struct {
	UID   string
	Title string
}

// SuggestPromptResult 推荐的产出。
type SuggestPromptResult struct {
	// Prompt 合成出来的最终提示词，直接能拿去出图。**只有一条**（决策：5 条合成 1 条、出 1 张）。
	Prompt string
	// Category 实际用的分类。运营选「全部」时这里是模型判出来的那个。
	Category SuggestedCategory
	// Candidates 参考过的 5 条。
	Candidates []SuggestedCandidate
	// Note 给运营的一句中文说明，可空（比如「没能自动判断分类，按 XX 推荐的」）。
	Note string
	// CachedAt 非零 = 这条来自缓存（值是它最初生成的时间），没调模型、没计费。
	// 零值 = 本次现算的。
	CachedAt time.Time
}

// SuggestService 是「AI 挑提示词」这一个能力。
//
// 单独一个接口而不是塞进 PromptService：它跟浏览灵感库的依赖面完全不同 ——
// 浏览只要数据库，推荐还要对象存储（读商品图）和出图网关（调对话模型）。
// 合成一个接口的话，存储坏掉会顺手把「翻灵感库」也干掉，那完全不成比例。
type SuggestService interface {
	SuggestPrompt(ctx context.Context, in SuggestPromptInput) (*SuggestPromptResult, error)
}

// ProxyOption 是「同步走哪个代理」下拉框里的一项。
//
// ⚠ 字段名必须跟上游 GET /api/v1/admin/proxies/all 的响应**逐字对齐**：
// 前端那个下拉框是直接复用上游的通用组件
// frontend/src/components/common/ProxySelector.vue，
// 它按 proxy.id / name / protocol / host / port / account_count 渲染，
// 名字对不上就是一个空白的下拉框，而且不会报错。
//
// **绝不带密码**：上游 dto.Proxy 把 Password 标成 `json:"-"`，我们照做。
type ProxyOption struct {
	ID       int64
	Name     string
	Protocol string
	Host     string
	Port     int
	Username string
	Status   string

	ExpiresAt      *time.Time
	FallbackMode   string
	BackupProxyID  *int64
	ExpiryWarnDays int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SyncSettings 是「灵感库同步」的设置：现在选的代理 + 可以选哪些代理。
//
// 两样东西必须在**同一个响应**里给出来：前端那个下拉框既要知道选中了谁，
// 也要有一份候选列表才渲染得出来。分成两个接口的话，
// 运营会在两次请求之间看到一个短暂的「无代理」。
type SyncSettings struct {
	// ProxyID 现在选的代理；nil = 不走代理（直连）。
	ProxyID *int64
	// Proxies 可以选的代理（只列还在用的那些），顺序即下拉框顺序。
	Proxies []ProxyOption
}

// SyncStatusView 是「灵感库同步」的当前状态。
type SyncStatusView struct {
	// Latest 最近一次同步；一次都没跑过时为 nil。
	Latest *dkdomain.SyncRun
	// Running 现在是不是正有一次在跑（前端靠它决定要不要继续轮询）。
	Running bool
	// PromptCount 库里现在一共有多少条可用的提示词。
	PromptCount int
	// CategoryCount 一共有多少个分类。
	CategoryCount int
}

// PromptSyncService 灵感库同步。**只有管理员能用**（handler 侧由 RequireAdmin 挡着）。
//
// 同步是长任务（一万四千条，约 15 秒）：StartSync **必须立刻返回**，
// 真正的活在后台跑，前端靠 SyncStatus 轮询。让 HTTP 请求挂 15 秒等它，
// 中间任何一个反代超时都会让运营看到「失败」，而同步其实跑得好好的。
type PromptSyncService interface {
	// StartSync 开一次手动同步，**立刻返回**那条已经建好的 running 记录。
	//
	// 已经有一次在跑时返回 DK_SYNC_IN_PROGRESS
	// （「灵感库正在同步，请等这一次跑完再点。」）。
	//
	// ⚠ 抢锁必须用 PostgreSQL advisory lock（pg_try_advisory_lock），
	// **手动和自动共用同一把**，不要用应用内 mutex —— 多副本时它不起作用。
	// 老系统各用一把锁，并发把 1.4 万条写重复了。
	StartSync(ctx context.Context) (*dkdomain.SyncRun, error)

	// SyncStatus 最近一次同步的结果 + 库里现在有多少条。
	SyncStatus(ctx context.Context) (*SyncStatusView, error)

	// GetSyncSettings 现在选的代理 + 可选代理列表。
	GetSyncSettings(ctx context.Context) (*SyncSettings, error)

	// PutSyncSettings 换一个代理。proxyID 为 nil 表示不走代理（直连）。
	//
	// ⚠ 这个代理**只给同步用**。老代码的血泪教训（原话）：
	// 「不能用全局 urllib 代理或环境变量——那会把生图请求也带进代理」。
	// Go 侧同理：实现里绝不许动 http.DefaultTransport，也不许设
	// HTTP_PROXY / HTTPS_PROXY 环境变量，只给同步那一个 client 单独配。
	//
	// 传了一个不存在或已停用的代理时返回 DK_INVALID_REQUEST，不要静默改成直连 ——
	// 静默的后果是管理员以为走了代理，实际在裸连，而且拉不动时查不出原因。
	PutSyncSettings(ctx context.Context, proxyID *int64) (*SyncSettings, error)
}
