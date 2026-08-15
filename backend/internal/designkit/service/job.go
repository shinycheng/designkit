package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"
)

// ============================================================================
// 批次（报价 / 提交 / 查询 / 停止排队 / 重试）
// ============================================================================
//
// 这是**唯一**一处「让一张图开始出」的入口。在它落地之前，
// handler.Services.Ready() 永远是 false，业务端点整组不注册，
// 线上只有两个健康检查 —— 运营界面上没有任何按钮能让一张图开始出。
//
// 三件「错一次就是钱」的事，改这个文件之前先读一遍：
//
//  1. **展开顺序固定「商品图外层、提示词内层」，seq 从 1 连续递增。**
//     图1×词1 → seq 1，图1×词2 → seq 2，图2×词1 → seq 3 …
//     这是发给 ERP 的对外契约（「按序号取第几张图」），破了不会报错，
//     只会让 ERP 把图配错商品。有专门单测守着。
//
//  2. **顺序不能反**：先校验、再展开、再在**一个事务**里
//     (FOR UPDATE 查可用额 → 建 job → 建 items → 建 hold)。
//     反了就会造出「冻结额建了但任务没建」的孤儿冻结 ——
//     运营看到的是「明明没任务却提示余额不足」，而且没有任何报错。
//     那个事务在 repository.CreateJobWithItemsAndHold 里，本文件只负责把
//     参数准备对、把错误翻译成中文。
//
//  3. **价目表还没实测出来**（设计定型 第十节 1 是上线阻塞项）。
//     没有单价时预估金额一律返回 nil（界面显示「价格待确认」），
//     **绝不返回 0，也绝不返回一个猜的数字** —— 0 会被运营读成「免费」。
//
// 幂等在 handler 那一层（上游协调器 + scope 里揉进 user id），
// 本层只负责把 IdempotencyKey 透传给 repository：那条事务在拿到用户行锁**之后**
// 再查一次同 key 的老任务，命中就原样返回，什么都不建、也不再冻结一次。

const (
	// SettingKeyUnitPrices 出图单价表。
	//
	// ⚠ 9001 迁移**刻意没有预置这一项**（那个文件跑过就永不可改）：
	// 每个比例实际出多大像素、落 1K/2K/4K 哪一档、单价填多少，
	// 必须真出图实测才知道（设计定型 第十节 1）。读不到 = 价格待确认。
	//
	// 值是 JSON 对象：计费档 → **十进制字符串**金额（美元）。写字符串不写数字，
	// 理由跟对外 JSON 一样：JSON 数字是 float64，0.1+0.2 会变成 0.30000000000000004。
	//
	//	{"1K":"0.011","2K":"0.042","4K":"0.167"}
	//
	// 实测完之后由管理员写进 designkit_settings（PutSetting），不用改代码、不用发版。
	SettingKeyUnitPrices = "unit_prices"

	// SettingKeyRateMultiplier 报价倍率（分组/用户专属折扣或加价）。
	// 读不到、读坏了一律按 1 算。值可以是 JSON 数字也可以是字符串："1.2" / 1.2。
	SettingKeyRateMultiplier = "rate_multiplier"

	// jobPricingSnapshotVersion 定价快照的结构版本号，以后改快照结构靠它区分。
	jobPricingSnapshotVersion = 1

	// jobPriceSourceSettings 单价来自 designkit_settings（不是上游的 channel/group/default）。
	// 列是 VARCHAR(32)，这个字面量 18 个字符，放得下。
	jobPriceSourceSettings = "designkit_settings"

	// jobDefaultListLimit / jobMaxListLimit 列表分页。跟 handler 的 parseLimit 一致。
	jobDefaultListLimit = 20
	jobMaxListLimit     = 100

	// jobMinBatchItems / jobMaxBatchItemsLimit 是 max_batch_items 的合理区间。
	// 管理员把它填成 0（谁也提交不了）或者 100000（一次提交把数据库打满）都不接受，
	// 一律收敛回区间内。
	jobMinBatchItems      = 1
	jobMaxBatchItemsLimit = 500
)

// ----------------------------------------------------------------------------
// 依赖
// ----------------------------------------------------------------------------

// JobStore 是本服务用到的持久化能力，是 domain.Repository 的**子集**，
// 方法签名逐字照抄（文件末尾有编译期断言钉死这件事）。
//
// 为什么不直接用 domain.Repository：单测要塞假实现，
// 假实现没必要为了测一次提交去实现灵感库同步和素材上传那几十个方法。
type JobStore interface {
	// —— 提交（一个事务里查可用额 + 建 job/items/hold）——
	CreateJobWithItemsAndHold(ctx context.Context, params dkdomain.CreateJobParams) (*dkdomain.CreateJobResult, error)

	// —— 读 ——
	GetJobByUID(ctx context.Context, userID int64, uid string) (*dkdomain.Job, error)
	ListJobs(ctx context.Context, query dkdomain.ListJobsQuery) (*dkdomain.JobPage, error)
	ListJobItems(ctx context.Context, jobID int64) ([]*dkdomain.JobItem, error)
	GetJobItemBySeq(ctx context.Context, jobID int64, seq int) (*dkdomain.JobItem, error)
	ListImagesByItem(ctx context.Context, itemID int64, currentOnly bool) ([]*dkdomain.Image, error)

	// —— 状态 ——
	UpdateJobStatus(ctx context.Context, jobID int64, from, to dkdomain.JobStatus) error
	ClearJobIdempotencyKey(ctx context.Context, jobID int64) error
	RequestJobStop(ctx context.Context, jobID int64) (cancelled int, err error)
	RetryItem(ctx context.Context, params dkdomain.RetryItemParams) (*dkdomain.JobItem, error)
	FinalizeJob(ctx context.Context, params dkdomain.FinalizeJobParams) (won bool, err error)

	// —— 素材 / 灵感库（展开批次要把 uid 换成内部主键）——
	GetAssetByUID(ctx context.Context, userID int64, uid string) (*dkdomain.Asset, error)
	GetPromptByUID(ctx context.Context, uid string) (*dkdomain.Prompt, error)

	// —— 钱 ——
	GetUsageSummary(ctx context.Context, userID int64, from, to time.Time) (*dkdomain.UsageSummary, error)
	ReleaseHold(ctx context.Context, jobID int64) error

	// —— 配置 ——
	GetSetting(ctx context.Context, key string) (*dkdomain.Setting, error)
}

// 编译期断言：JobStore 必须是 domain.Repository 的子集。
// 谁改了 domain 里的签名而没同步这里，编译当场就炸 —— 本机没有 Go 编译器，
// 能在 CI 第一步炸掉的错误是最便宜的错误。
var _ JobStore = (dkdomain.Repository)(nil)

// InternalKeyProvider 给「浏览器端提交的批次」兜底一把内部专用 API Key。
//
// designkit_jobs.api_key_id 是 NOT NULL，出图时上游 Images() 第一件事就是从上下文
// 取 API Key，取不到直接 401。ERP 走上游 apiKeyAuth，上下文里有真实的 Key；
// 浏览器端走 jwtAuth，没有 Key，只能由这里兜底。
//
// *InternalKeyService 天然满足它。允许为 nil：此时浏览器端提交会拿到
// DK_API_KEY_MISSING + 中文提示，而 ERP（自带 Key）照常能提交。
type InternalKeyProvider interface {
	EnsureInternalKey(ctx context.Context, userID int64) (*upstreamservice.APIKey, error)
}

// JobServiceDeps 建 JobService 要的依赖。
type JobServiceDeps struct {
	// Repo 持久化层，必填。
	Repo JobStore
	// Store 对象存储。允许为 nil：此时只有「取图片字节」这一个动作会失败，
	// 报价、提交、查询照常 —— 别为了一个降级项把整组端点又关掉。
	Store dkdomain.ObjectStore
	// Keys 内部专用 API Key 服务，允许为 nil（后果见 InternalKeyProvider）。
	Keys InternalKeyProvider
	// Now 取当前时间，测试可以塞固定值。为 nil 用 time.Now。
	Now func() time.Time
	// NewUID 生成 26 位 ULID。为 nil 用本包内置实现。
	NewUID func() string
	// Logger 日志。为 nil 用 slog.Default()。
	Logger *slog.Logger
}

// JobService 批次的报价、提交与查询。
type JobService struct {
	repo   JobStore
	store  dkdomain.ObjectStore
	keys   InternalKeyProvider
	now    func() time.Time
	newUID func() string
	log    *slog.Logger
}

// NewJobService 建服务。缺必填依赖直接报错，不留到运行时才 nil panic。
func NewJobService(deps JobServiceDeps) (*JobService, error) {
	if deps.Repo == nil {
		return nil, errors.New("designkit: JobService 缺少持久化层（Repo）")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	newUID := deps.NewUID
	if newUID == nil {
		// 跟素材用同一个 ULID 实现（26 位 Crockford Base32，时间戳在前）。
		newUID = newAssetULID
	}
	return &JobService{
		repo:   deps.Repo,
		store:  deps.Store,
		keys:   deps.Keys,
		now:    now,
		newUID: newUID,
		log:    deps.Logger,
	}, nil
}

func (s *JobService) logger() *slog.Logger {
	if s == nil || s.log == nil {
		return slog.Default()
	}
	return s.log
}

// ----------------------------------------------------------------------------
// 输入输出（本层自己的类型）
// ----------------------------------------------------------------------------
//
// 为什么不直接用 handler/ports.go 里那几个结构体：handler 已经 import 了本包，
// 本包再反过来 import handler 就是循环依赖，编译不过。
// 两边的翻译在组合根 internal/designkit/module.go 的 jobServiceAdapter 里。

// JobSpec 是「这一批要出哪些图」的描述，报价和提交共用。
//
// 展开顺序固定为**商品图外层、提示词内层**：
// 图1×词1 → seq 1，图1×词2 → seq 2，图2×词1 → seq 3 …
type JobSpec struct {
	// UserID 提交人。
	UserID int64
	// Origin web / erp，由挂载前缀决定。
	Origin dkdomain.Origin
	// Ratio 整批统一的出图比例。白名单在本层比对 designkit_settings.ratios。
	Ratio dkdomain.Ratio
	// AssetUIDs 选中的商品图，顺序即展开时的外层顺序。
	AssetUIDs []string
	// PromptUIDs 选中的灵感库提示词，顺序即展开时的内层顺序。
	PromptUIDs []string
	// PromptTexts 运营手打的提示词，接在 PromptUIDs 后面。
	PromptTexts []string
	// Model 出图模型；空串表示用 designkit_settings.model。
	Model string
	// KeepTransparency 预处理时是否保留透明底。
	//
	// ⚠ **当前不落库**：9001 的 designkit_jobs 没有这一列（那个文件跑过、永不可改），
	// 出图 worker 用的是它自己的全局配置 worker.Config.KeepTransparency。
	// 想做到「每批可选」得先加一条 9xxx 迁移加列。见 contract 说明。
	KeepTransparency bool
}

// CreateJobInput 提交一个批次。
type CreateJobInput struct {
	JobSpec

	// Name 运营给这批起的名字，可为空串。
	Name string
	// CallbackURL 完成回调地址，空串 = 不回调。
	CallbackURL string
	// IdempotencyKey 客户端送来的防重复标识。**浏览器端也必须带**
	// （否则运营手滑双击 = 两个 job 两份钱）。handler 已保证它非空。
	IdempotencyKey string
	// APIKeyID 记在哪把 Key 上；0 表示让本层用该用户的内部专用 Key。
	APIKeyID int64
}

// JobEstimate 是 POST /jobs/estimate 的结果。
type JobEstimate struct {
	// ItemCount 展开后的总张数 = 商品图数 × 提示词数。
	ItemCount int
	// AssetCount 商品图数。
	AssetCount int
	// PromptCount 提示词数。
	PromptCount int
	// MaxBatchItems 单次批量上限（designkit_settings.max_batch_items）。
	MaxBatchItems int
	// PricingTier 预估档位 1K/2K/4K。
	//
	// ⚠ 真正计费的档由上游按**实际输出像素**定（实测请求 1:1 拿回 1254×1254，
	// 落的是 2K 不是 1K），所以这里只是预估。
	PricingTier string
	// UnitPrice 单张预估价；**nil = 价格待确认**，不是 0。
	UnitPrice *dkdomain.Money
	// EstimatedCost 这一批的预估花费；UnitPrice 为 nil 时也为 nil。
	EstimatedCost *dkdomain.Money
	// PriceNote 价格待确认时给运营看的中文说明；单价确认后是空串。
	//
	// 对外落在 POST /jobs/estimate 响应的 price_note 字段上
	// （handler/dto.go 的 estimateDTO），适配器在 module.go 里搬过去。
	// **不许再丢掉它**：price_confirmed=false 时 unit_price / estimated_cost
	// 都是 null，界面上没有这句话就是一个不带任何解释的空价格。
	PriceNote string
	// Balance 当前余额。
	Balance dkdomain.Money
	// Available 当前可用额 = 余额 - 已冻结。
	Available dkdomain.Money
	// Sufficient 可用额够不够这一单。**价格待确认时一律 true**
	// —— 拦不住也不该假装拦得住。
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

// ----------------------------------------------------------------------------
// 报价
// ----------------------------------------------------------------------------

// EstimateJob 只算账，**不建任何东西、不冻结任何额度**。
//
// 刻意**不**逐个校验商品图和提示词存不存在：报价是运营每改一次勾选就调一次的
// 高频接口，为它发几十条 SELECT 不划算；真正的存在性校验在提交那一步做，
// 那一步本来就要把 uid 换成内部主键。
//
// 张数超过 max_batch_items 时**也不报错**：如实返回张数和上限，让界面自己
// 提示「超了，请少选几张」。报价接口报错会让运营连「差多少」都看不到。
func (s *JobService) EstimateJob(ctx context.Context, spec JobSpec) (*JobEstimate, error) {
	if spec.UserID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUnauthorized)
	}
	if err := s.checkRatio(ctx, spec.Ratio); err != nil {
		return nil, err
	}

	assets := trimNonEmpty(spec.AssetUIDs)
	prompts := len(trimNonEmpty(spec.PromptUIDs)) + len(trimNonEmpty(spec.PromptTexts))
	itemCount := len(assets) * prompts

	pricing := s.resolvePricing(ctx, spec.Ratio)
	balance, available, err := s.balances(ctx, spec.UserID)
	if err != nil {
		return nil, err
	}

	out := &JobEstimate{
		ItemCount:     itemCount,
		AssetCount:    len(assets),
		PromptCount:   prompts,
		MaxBatchItems: s.maxBatchItems(ctx),
		PricingTier:   pricing.Tier,
		Balance:       balance,
		Available:     available,
		// 价格待确认时一律「够」：我们根本算不出这一单要多少钱，
		// 假装拦一下只会让运营以为余额有问题。
		Sufficient: true,
		Shortfall:  dkdomain.ZeroMoney,
	}

	if pricing.UnitPrice == nil {
		out.PriceNote = jobPricePendingNote
		return out, nil
	}

	unit := *pricing.UnitPrice
	cost := pricing.costOf(itemCount)
	out.UnitPrice = &unit
	out.EstimatedCost = &cost
	if available.LessThan(cost) {
		out.Sufficient = false
		shortfall := cost.Sub(available)
		if shortfall.IsNegative() {
			shortfall = dkdomain.ZeroMoney
		}
		out.Shortfall = dkdomain.QuantizeMoney(shortfall)
	}
	return out, nil
}

// jobPricePendingNote 价格待确认时的中文说明。
//
// 每个比例实际出多大像素、落哪档、单价填多少，必须真出图实测才知道
// （设计定型 第十节 1 是上线阻塞项）。测出来之前**一个数字都不许给** ——
// 给 0 会被读成「免费」，给一个猜的数会让运营按错的预算下单。
const jobPricePendingNote = "出图单价还没实测确认，这一批的花费暂时算不出来。可以照常提交，实际花费以任务完成后的结算为准。"

// ----------------------------------------------------------------------------
// 定价
// ----------------------------------------------------------------------------

// jobPricing 是一次报价用到的全部价格参数。
type jobPricing struct {
	// Tier 预估计费档 1K/2K/4K。
	Tier string
	// UnitPrice 单张单价；nil = 价格待确认。
	UnitPrice *dkdomain.Money
	// Multiplier 报价倍率，读不到按 1 算。
	Multiplier dkdomain.Multiplier
	// Source 价格来自哪一层，落定价快照。
	Source string
	// BillingMode 计价方式，落定价快照。价格待确认时留空串（= 未知），
	// **绝不写 "token"** —— 那是「后台配错了」的标志值（9001 的列注释）。
	BillingMode string
}

// costOf 算 count 张的预估花费。单价未知时返回 0（调用方必须先判 UnitPrice）。
func (p jobPricing) costOf(count int) dkdomain.Money {
	if p.UnitPrice == nil || count <= 0 {
		return dkdomain.ZeroMoney
	}
	total := p.UnitPrice.Mul(dkdomain.MoneyFromInt(int64(count))).Mul(p.Multiplier)
	return dkdomain.QuantizeMoney(total)
}

// resolvePricing 算出这个比例的预估档位和单价。
//
// 单价来自 designkit_settings.unit_prices，**读不到就是「价格待确认」**，
// 不猜、不兜底成任何数字。
func (s *JobService) resolvePricing(ctx context.Context, ratio dkdomain.Ratio) jobPricing {
	out := jobPricing{
		Tier:       dkdomain.BillingTier2K,
		Multiplier: dkdomain.MultiplierFromFloat(1),
	}

	// 预估档位：按「补白边到目标比例、最长边不超过 max_dimension」之后的像素分档。
	if width, height, err := ratio.TargetSize(s.maxDimension(ctx)); err == nil {
		out.Tier = dkdomain.ClassifyBillingTierOrDefault(width, height)
	}

	if m, ok := s.rateMultiplier(ctx); ok {
		out.Multiplier = m
	}

	table, ok := s.unitPriceTable(ctx)
	if !ok {
		return out
	}
	raw, ok := table[out.Tier]
	if !ok {
		return out
	}
	price, err := dkdomain.MoneyFromString(strings.TrimSpace(raw))
	if err != nil || price.IsNegative() {
		s.logger().Warn("designkit 出图单价表里的价格不是合法金额，按「价格待确认」处理",
			slog.String("setting", SettingKeyUnitPrices),
			slog.String("tier", out.Tier),
			slog.String("value", raw))
		return out
	}
	out.UnitPrice = &price
	out.Source = jobPriceSourceSettings
	// 有单价 = 管理员实测确认过「按张收费」，快照里如实记一笔。
	out.BillingMode = string(upstreamservice.BillingModeImage)
	return out
}

// unitPriceTable 读 designkit_settings.unit_prices。
func (s *JobService) unitPriceTable(ctx context.Context) (map[string]string, bool) {
	raw, ok := s.settingRaw(ctx, SettingKeyUnitPrices)
	if !ok {
		return nil, false
	}
	var table map[string]string
	if err := json.Unmarshal(raw, &table); err != nil {
		s.logger().Warn("designkit 出图单价表解析失败，按「价格待确认」处理",
			slog.String("setting", SettingKeyUnitPrices),
			slog.String("value", string(raw)),
			slog.Any("error", err))
		return nil, false
	}
	if len(table) == 0 {
		return nil, false
	}
	return table, true
}

// rateMultiplier 读 designkit_settings.rate_multiplier。数字和字符串两种写法都认。
func (s *JobService) rateMultiplier(ctx context.Context) (dkdomain.Multiplier, bool) {
	raw, ok := s.settingRaw(ctx, SettingKeyRateMultiplier)
	if !ok {
		return dkdomain.Multiplier{}, false
	}
	text := strings.TrimSpace(strings.Trim(strings.TrimSpace(string(raw)), `"`))
	value, err := dkdomain.MoneyFromString(text)
	if err != nil || value.IsNegative() || value.IsZero() {
		s.logger().Warn("designkit 报价倍率不可用，按 1 处理",
			slog.String("setting", SettingKeyRateMultiplier),
			slog.String("value", string(raw)))
		return dkdomain.Multiplier{}, false
	}
	return value, true
}

// ----------------------------------------------------------------------------
// 配置读取
// ----------------------------------------------------------------------------
//
// 都**不返回 error**：配置读不到时退回默认值并记一条日志。
// 唯一真相仍是数据库里的值，默认值只是兜底。
//
// ⚠ allowedRatios / maxDimension 跟 asset.go 里 *AssetService 的同名方法逻辑一致。
// 没抽成公共函数是因为两个服务各自持有自己的仓储句柄，而 asset.go 不在本轮可改文件里。
// 哪天要动比例白名单的解析规则，**两处一起改**。

// allowedRatios 读 designkit_settings.ratios。
// AllowedRatios 导出版本，供组合根把「出图比例」这块能力接给 handler.CatalogService。
//
// 为什么要有这个：比例列表只读 designkit_settings，本来不该依赖对象存储。
// 但原来 Catalog 是从 AssetService 上取的，而 AssetService 必须有对象存储才建得出来——
// 结果 DESIGNKIT_STORAGE_DIR 建不出来时（群晖上权限没配对是常见情况），
// Services.Ready() 不成立，业务接口**整组不注册**，连「看比例」「查昨天的历史」都 404。
// 挂在只依赖数据库的 JobService 上，故障面就只剩「传不了图」这一件事。
func (s *JobService) AllowedRatios(ctx context.Context) []dkdomain.Ratio {
	return s.allowedRatios(ctx)
}

// MaxDimension 导出版本，理由同 AllowedRatios。
func (s *JobService) MaxDimension(ctx context.Context) int {
	return s.maxDimension(ctx)
}

func (s *JobService) allowedRatios(ctx context.Context) []dkdomain.Ratio {
	raw, ok := s.settingRaw(ctx, dkdomain.SettingKeyRatios)
	if !ok {
		return dkdomain.DefaultRatios()
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		s.logger().Warn("designkit 配置 ratios 解析失败，退回默认比例",
			slog.String("value", string(raw)), slog.Any("error", err))
		return dkdomain.DefaultRatios()
	}
	out := make([]dkdomain.Ratio, 0, len(values))
	for _, v := range values {
		parsed, err := dkdomain.ParseRatio(v)
		if err != nil {
			s.logger().Warn("designkit 配置 ratios 里有不合法的比例，已跳过", slog.String("ratio", v))
			continue
		}
		out = append(out, parsed)
	}
	if len(out) == 0 {
		return dkdomain.DefaultRatios()
	}
	return out
}

// maxDimension 读 designkit_settings.max_dimension（只用于算预估档位）。
func (s *JobService) maxDimension(ctx context.Context) int {
	raw, ok := s.settingRaw(ctx, dkdomain.SettingKeyMaxDimension)
	if !ok {
		return dkdomain.DefaultMaxDimension
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil || value <= 0 {
		return dkdomain.DefaultMaxDimension
	}
	return value
}

// maxBatchItems 读 designkit_settings.max_batch_items（单次批量张数上限）。
func (s *JobService) maxBatchItems(ctx context.Context) int {
	raw, ok := s.settingRaw(ctx, dkdomain.SettingKeyMaxBatchItems)
	if !ok {
		return dkdomain.DefaultMaxBatchItems
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		s.logger().Warn("designkit 配置 max_batch_items 不是整数，退回默认值",
			slog.String("value", string(raw)), slog.Int("default", dkdomain.DefaultMaxBatchItems))
		return dkdomain.DefaultMaxBatchItems
	}
	if value < jobMinBatchItems || value > jobMaxBatchItemsLimit {
		s.logger().Warn("designkit 配置 max_batch_items 超出允许范围，已收敛",
			slog.Int("configured", value),
			slog.Int("min", jobMinBatchItems), slog.Int("max", jobMaxBatchItemsLimit))
		if value < jobMinBatchItems {
			return jobMinBatchItems
		}
		return jobMaxBatchItemsLimit
	}
	return value
}

// defaultModel 读 designkit_settings.model。
func (s *JobService) defaultModel(ctx context.Context) string {
	raw, ok := s.settingRaw(ctx, dkdomain.SettingKeyModel)
	if !ok {
		return dkdomain.DefaultModel
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return dkdomain.DefaultModel
	}
	return strings.TrimSpace(value)
}

// adminContact 读 designkit_settings.admin_contact，拼在「余额不足」的提示后面（决策 19）。
func (s *JobService) adminContact(ctx context.Context) string {
	raw, ok := s.settingRaw(ctx, dkdomain.SettingKeyAdminContact)
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// settingRaw 取一个配置项的原始 JSON；不存在或出错返回 ok=false。
func (s *JobService) settingRaw(ctx context.Context, key string) (json.RawMessage, bool) {
	setting, err := s.repo.GetSetting(ctx, key)
	if err != nil {
		if !errors.Is(err, dkdomain.ErrNotFound) {
			s.logger().Warn("designkit 读取配置失败，使用默认值",
				slog.String("key", key), slog.Any("error", err))
		}
		return nil, false
	}
	if setting == nil || len(setting.Value) == 0 {
		return nil, false
	}
	return setting.Value, true
}

// checkRatio 比例护栏：格式对不对 + 在不在 designkit_settings.ratios 白名单里。
//
// ⚠ **这里是唯一的关口。** Python 侧的 parse_aspect 接受任意 W:H（"7:13" 也照收），
// 它里面的 SUPPORTED_RATIOS 常量只是错误提示文案，不是护栏。
func (s *JobService) checkRatio(ctx context.Context, ratio dkdomain.Ratio) error {
	allowed := s.allowedRatios(ctx)
	if err := dkdomain.ValidateRatio(ratio, allowed); err != nil {
		return dkdomain.NewError(dkdomain.ErrCodeRatioNotAllowed).
			WithMessagef("不支持 %q 这个出图比例，请从 %s 里选一个。", ratio.String(), joinRatios(allowed)).
			WithCause(err)
	}
	return nil
}

// balances 取余额和可用额。
//
// 走 GetUsageSummary 而不是 GetAvailableBalance，是因为界面要同时显示「余额」和
// 「可用额」两个数，而 GetAvailableBalance 只给后者。时间窗给一个零宽区间
// （now, now）：那两个统计子查询会稳定命中空区间，只把 users 行上的余额取回来。
//
// ⚠ 这两个数**只是给界面看的参考值**。真正拦透支的是提交事务里那条
// `SELECT balance ... FOR UPDATE`，绝不能拿这里的返回值当准入判据 ——
// 读完到写之间的窗口就是上游「50 张并发全部放行」的那个窗口。
func (s *JobService) balances(ctx context.Context, userID int64) (balance, available dkdomain.Money, err error) {
	now := s.now().UTC()
	summary, err := s.repo.GetUsageSummary(ctx, userID, now, now)
	if err != nil {
		return dkdomain.ZeroMoney, dkdomain.ZeroMoney, mapJobRepoError(err, dkdomain.ErrCodeUnauthorized)
	}
	if summary == nil {
		return dkdomain.ZeroMoney, dkdomain.ZeroMoney, dkdomain.NewError(dkdomain.ErrCodeInternal)
	}
	return summary.Balance, summary.Available, nil
}

// ----------------------------------------------------------------------------
// 错误翻译
// ----------------------------------------------------------------------------

// mapJobRepoError 把仓储的哨兵错误翻译成带中文文案的我方错误。
//
// notFoundCode 是「查不到」时该用哪个错误码（任务 / 这一张 / 商品图…），
// 由调用点决定 —— 同一个 ErrNotFound 在不同入口对运营意味着不同的事。
func mapJobRepoError(err error, notFoundCode string) error {
	if err == nil {
		return nil
	}

	// 仓储自己造好的中文错误（DK_API_KEY_MISSING、DK_JOB_ALREADY_SETTLED、
	// DK_MAX_ATTEMPTS_EXCEEDED…）原样透传，别再包一层把文案盖掉。
	var dkErr *dkdomain.DesignkitError
	if errors.As(err, &dkErr) {
		return dkErr
	}

	var shortErr *dkdomain.InsufficientBalanceError
	if errors.As(err, &shortErr) {
		return insufficientBalanceError(shortErr, "")
	}

	switch {
	case errors.Is(err, dkdomain.ErrNotFound):
		return dkdomain.NewError(notFoundCode).WithCause(err)
	case errors.Is(err, dkdomain.ErrIllegalTransition), errors.Is(err, dkdomain.ErrConflict):
		return dkdomain.NewError(dkdomain.ErrCodeIllegalStateTransition).WithCause(err)
	case errors.Is(err, dkdomain.ErrInsufficientBalance):
		return dkdomain.NewError(dkdomain.ErrCodeInsufficientBalance).WithCause(err)
	default:
		return dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
}

// insufficientBalanceError 拼「还差多少」的中文提示（决策 19）。
//
// 差额必须显示出来：运营看到「余额不足」而不知道差多少，只能一张一张试。
// 金额一律美元，文案里不许出现「元」。
func insufficientBalanceError(short *dkdomain.InsufficientBalanceError, contact string) *dkdomain.DesignkitError {
	msg := fmt.Sprintf("余额不足，还差 %s。可以点「申请额度」通知管理员，或者少选几张再提交。",
		dkdomain.FormatMoneyUSD(short.Shortfall))
	if contact != "" {
		msg += "管理员联系方式：" + contact + "。"
	}
	return dkdomain.NewError(dkdomain.ErrCodeInsufficientBalance).
		WithMessage(msg).
		WithCause(short)
}

// trimNonEmpty 去掉首尾空白并丢掉空串，**保持原顺序**。
// 顺序就是展开顺序，既不能排序也不能去重（运营选了两次同一张图就该出两张）。
func trimNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
