package handler

import (
	"fmt"
	"strconv"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ============================================================================
// 对外响应体
// ============================================================================
//
// 三条铁律：
//  1. **任何层级都不许出现名为 code 的字段**（上游幂等协调器落库前会把它脱敏成 ***，
//     重放时 ERP 会拿到 ***）。要给错误码就叫 error_code / last_error_code。
//  2. **金额是字符串**，配一个固定 "USD" 的 currency 字段。
//     decimal 默认就序列化成带引号的字符串，ERP 用 decimal/BigDecimal 接不会丢精度，
//     也不会出现 0.1+0.2=0.30000000000000004 这种给运营看的怪数。
//     金额一律美元，不做汇率换算，文案里不许出现「元」。
//  3. **上游返回的英文原文绝不进响应体**。item 上只给我方错误码 + 中文文案；
//     英文原文留在 designkit_job_items.last_error_message 里给管理员看。

// timeString 把时间格式化成 RFC3339（UTC）。
func timeString(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// timeStringPtr 可空时间 → 可空字符串。
func timeStringPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := timeString(*t)
	return &s
}

// moneyStringPtr 可空金额 → 可空字符串。
func moneyStringPtr(m *dkdomain.Money) *string {
	if m == nil {
		return nil
	}
	s := dkdomain.MoneyString(*m)
	return &s
}

// errorMessageOf 把我方错误码翻成给运营看的中文。
// 传进来的是 designkit_job_items.last_error_code。
func errorMessageOf(code *string) *string {
	if code == nil || *code == "" {
		return nil
	}
	msg := dkdomain.NewError(*code).Message
	return &msg
}

// ---- 素材 ----

type assetDTO struct {
	UID         string `json:"uid"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	Width       *int   `json:"width"`
	Height      *int   `json:"height"`
	Origin      string `json:"origin"`
	CreatedAt   string `json:"created_at"`
	// ContentURL 取原图字节的地址。**运行时拼，绝不入库**——
	// 对象存储默认签发 24 小时失效的链接，存进库过期后拼不回来。
	ContentURL string `json:"content_url"`
}

func newAssetDTO(prefix string, a *dkdomain.Asset) assetDTO {
	if a == nil {
		return assetDTO{}
	}
	return assetDTO{
		UID:         a.UID,
		ContentType: a.ContentType,
		ByteSize:    a.ByteSize,
		Width:       a.Width,
		Height:      a.Height,
		Origin:      a.Origin.String(),
		CreatedAt:   timeString(a.CreatedAt),
		ContentURL:  prefix + "/assets/" + a.UID + "/content",
	}
}

// ---- 比例 ----

type ratioDTO struct {
	Ratio        string `json:"ratio"`
	TargetWidth  int    `json:"target_width"`
	TargetHeight int    `json:"target_height"`
	PricingTier  string `json:"pricing_tier"`
	// UnitPrice 单张预估价；**null = 价格待确认**。
	//
	// 每个比例实际出多大像素、落哪档、单价多少，必须真出图实测才知道
	// （设计定型 第十节 1 是上线阻塞项）。测出来之前一律 null，
	// 界面显示「价格待确认」，不许显示任何猜的单价。
	UnitPrice *string `json:"unit_price"`
	// PriceConfirmed 单价是不是已经实测确认过。false 时界面必须显示「价格待确认」。
	PriceConfirmed bool `json:"price_confirmed"`
	IsDefault      bool `json:"is_default"`
}

type ratioListDTO struct {
	Ratios   []ratioDTO `json:"ratios"`
	Currency string     `json:"currency"`
}

func newRatioDTO(o RatioOption) ratioDTO {
	return ratioDTO{
		Ratio:          o.Ratio.String(),
		TargetWidth:    o.TargetWidth,
		TargetHeight:   o.TargetHeight,
		PricingTier:    o.PricingTier,
		UnitPrice:      moneyStringPtr(o.UnitPrice),
		PriceConfirmed: o.UnitPrice != nil,
		IsDefault:      o.IsDefault,
	}
}

// ---- 报价 ----

type estimateDTO struct {
	ItemCount     int     `json:"item_count"`
	AssetCount    int     `json:"asset_count"`
	PromptCount   int     `json:"prompt_count"`
	MaxBatchItems int     `json:"max_batch_items"`
	PricingTier   string  `json:"pricing_tier"`
	UnitPrice     *string `json:"unit_price"`
	EstimatedCost *string `json:"estimated_cost"`
	// PriceConfirmed false 时 unit_price / estimated_cost 都是 null，界面显示「价格待确认」。
	PriceConfirmed bool `json:"price_confirmed"`
	// PriceNote 价格待确认时的**中文说明**（单价确认后是空串）。
	//
	// 必须显示出来。少了这句话，运营在确认弹窗上看到的是一个不带任何解释的空价格，
	// 分不清是「这一批不要钱」还是「系统坏了」，只能不敢点。
	PriceNote string `json:"price_note"`
	Currency  string `json:"currency"`
	Balance   string `json:"balance"`
	// Available 可用额 = 余额 - 已冻结。提交时真正拦透支的是数据库里那条
	// SELECT ... FOR UPDATE，这里只是给界面看的参考值。
	Available string `json:"available"`
	// Sufficient 够不够这一单。
	Sufficient bool `json:"sufficient"`
	// Shortfall 还差多少（够的时候是 "0"）。余额不足时界面要显示它 + 「申请额度」按钮。
	Shortfall string `json:"shortfall"`
}

func newEstimateDTO(r *EstimateResult) estimateDTO {
	if r == nil {
		return estimateDTO{Currency: dkdomain.CurrencyUSD}
	}
	return estimateDTO{
		ItemCount:      r.ItemCount,
		AssetCount:     r.AssetCount,
		PromptCount:    r.PromptCount,
		MaxBatchItems:  r.MaxBatchItems,
		PricingTier:    r.PricingTier,
		UnitPrice:      moneyStringPtr(r.UnitPrice),
		EstimatedCost:  moneyStringPtr(r.EstimatedCost),
		PriceConfirmed: r.UnitPrice != nil,
		PriceNote:      r.PriceNote,
		Currency:       dkdomain.CurrencyUSD,
		Balance:        dkdomain.MoneyString(r.Balance),
		Available:      dkdomain.MoneyString(r.Available),
		Sufficient:     r.Sufficient,
		Shortfall:      dkdomain.MoneyString(r.Shortfall),
	}
}

// ---- 批次 ----

// jobCreatedDTO 是 POST /jobs 的成功响应。
//
// **只返小对象，绝不内联 items 数组**（设计定型 3.2，单测断言 < 8KB）：
// 这个响应体会被上游幂等协调器整个存进数据库，重放时原样吐回来；
// 内联 50 条 item 会让它膨胀，还会在重放时给出一份过时的进度。
// 要看进度去 GET /jobs/:uid/items。
type jobCreatedDTO struct {
	UID           string  `json:"uid"`
	Status        string  `json:"status"`
	ItemCount     int     `json:"item_count"`
	Currency      string  `json:"currency"`
	EstimatedCost string  `json:"estimated_cost"`
	Revision      int     `json:"revision"`
	Name          string  `json:"name"`
	Ratio         string  `json:"ratio"`
	Model         string  `json:"model"`
	CreatedAt     string  `json:"created_at"`
	CallbackURL   *string `json:"callback_url"`
}

func newJobCreatedDTO(j *dkdomain.Job) jobCreatedDTO {
	if j == nil {
		return jobCreatedDTO{Currency: dkdomain.CurrencyUSD}
	}
	currency := j.Currency
	if currency == "" {
		currency = dkdomain.CurrencyUSD
	}
	return jobCreatedDTO{
		UID:           j.UID,
		Status:        j.Status.String(),
		ItemCount:     j.ItemCount,
		Currency:      currency,
		EstimatedCost: dkdomain.MoneyString(j.EstimatedCost),
		Revision:      j.Revision,
		Name:          j.Name,
		Ratio:         j.Ratio.String(),
		Model:         j.Model,
		CreatedAt:     timeString(j.CreatedAt),
		CallbackURL:   j.CallbackURL,
	}
}

// jobDTO 是批次详情 / 列表里的一行。
type jobDTO struct {
	UID    string `json:"uid"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Origin string `json:"origin"`
	Ratio  string `json:"ratio"`
	Model  string `json:"model"`

	// 四个计数**任何终态都会一起返回**。
	// ERP 文档写死：判断有没有图一律看 success_count，**不看 status** ——
	// cancelled 不代表没花钱，已经在跑的那几张照样出图照样扣费。
	ItemCount      int `json:"item_count"`
	SuccessCount   int `json:"success_count"`
	FailCount      int `json:"fail_count"`
	CancelledCount int `json:"cancelled_count"`

	Currency      string  `json:"currency"`
	EstimatedCost string  `json:"estimated_cost"`
	ActualCost    *string `json:"actual_cost"`

	Revision int `json:"revision"`
	// LastErrorCode 整批失败的原因码，例如 DK_STALE。
	LastErrorCode *string `json:"last_error_code"`
	// LastErrorMessage 上面那个码对应的**中文**。
	// 上游返回的英文原文只留在数据库里给管理员看，不进这里。
	LastErrorMessage *string `json:"last_error_message"`

	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
	StartedAt         *string `json:"started_at"`
	FinishedAt        *string `json:"finished_at"`
	SettledAt         *string `json:"settled_at"`
	CancelRequestedAt *string `json:"cancel_requested_at"`

	ItemsURL string `json:"items_url"`
}

func newJobDTO(prefix string, j *dkdomain.Job) jobDTO {
	if j == nil {
		return jobDTO{Currency: dkdomain.CurrencyUSD}
	}
	currency := j.Currency
	if currency == "" {
		currency = dkdomain.CurrencyUSD
	}
	return jobDTO{
		UID:               j.UID,
		Name:              j.Name,
		Status:            j.Status.String(),
		Origin:            j.Origin.String(),
		Ratio:             j.Ratio.String(),
		Model:             j.Model,
		ItemCount:         j.ItemCount,
		SuccessCount:      j.SuccessCount,
		FailCount:         j.FailCount,
		CancelledCount:    j.CancelledCount,
		Currency:          currency,
		EstimatedCost:     dkdomain.MoneyString(j.EstimatedCost),
		ActualCost:        moneyStringPtr(j.ActualCost),
		Revision:          j.Revision,
		LastErrorCode:     j.LastErrorCode,
		LastErrorMessage:  errorMessageOf(j.LastErrorCode),
		CreatedAt:         timeString(j.CreatedAt),
		UpdatedAt:         timeString(j.UpdatedAt),
		StartedAt:         timeStringPtr(j.StartedAt),
		FinishedAt:        timeStringPtr(j.FinishedAt),
		SettledAt:         timeStringPtr(j.SettledAt),
		CancelRequestedAt: timeStringPtr(j.CancelRequestedAt),
		ItemsURL:          prefix + "/jobs/" + j.UID + "/items",
	}
}

// jobListDTO 是游标分页的一页。
//
// **不用 offset**：offset 在持续写入时会漏数据（新任务插到前面，
// 第二页会把第一页已经看过的行再给一遍、并漏掉真正的下一批）。
type jobListDTO struct {
	Items      []jobDTO `json:"items"`
	HasMore    bool     `json:"has_more"`
	NextCursor *string  `json:"next_cursor"`
}

// ---- 单张图 ----

type jobItemDTO struct {
	Seq        int    `json:"seq"`
	Status     string `json:"status"`
	PromptText string `json:"prompt_text"`
	// AssetUID / PromptUID 用哪张原图、哪条提示词；手打提示词时 prompt_uid 为 null。
	// 本轮 service 还没把 uid 带出来时为 null，不影响 ERP 按 seq 取图。
	AssetUID  *string `json:"asset_uid"`
	PromptUID *string `json:"prompt_uid"`

	AttemptCount int `json:"attempt_count"`
	MaxAttempts  int `json:"max_attempts"`

	// ErrorCode 我方错误码，界面显示中文时带上它方便运营截图报障。
	ErrorCode *string `json:"error_code"`
	// ErrorMessage 中文文案。**上游英文原文不进这里。**
	ErrorMessage *string `json:"error_message"`

	Currency    string  `json:"currency"`
	BilledCost  *string `json:"billed_cost"`
	BillingTier *string `json:"billing_tier"`

	CreatedAt  string  `json:"created_at"`
	StartedAt  *string `json:"started_at"`
	FinishedAt *string `json:"finished_at"`

	// Images 当前版本的结果图，按 image_index 升序。
	Images []imageDTO `json:"images"`
	// ContentURL 直接取这一张当前版本第一张图的地址（图还没出来时也能先拼好）。
	ContentURL string `json:"content_url"`
}

func newJobItemDTO(prefix, jobUID string, v *JobItemView) jobItemDTO {
	if v == nil || v.Item == nil {
		return jobItemDTO{Currency: dkdomain.CurrencyUSD}
	}
	it := v.Item

	images := make([]imageDTO, 0, len(v.Images))
	for _, img := range v.Images {
		images = append(images, newImageDTO(prefix, jobUID, it.Seq, img))
	}

	return jobItemDTO{
		Seq:          it.Seq,
		Status:       it.Status.String(),
		PromptText:   it.PromptText,
		AssetUID:     nil,
		PromptUID:    nil,
		AttemptCount: it.AttemptCount,
		MaxAttempts:  it.MaxAttempts,
		ErrorCode:    it.LastErrorCode,
		ErrorMessage: errorMessageOf(it.LastErrorCode),
		Currency:     dkdomain.CurrencyUSD,
		BilledCost:   moneyStringPtr(it.BilledCost),
		BillingTier:  it.BillingTier,
		CreatedAt:    timeString(it.CreatedAt),
		StartedAt:    timeStringPtr(it.StartedAt),
		FinishedAt:   timeStringPtr(it.FinishedAt),
		Images:       images,
		ContentURL:   prefix + "/jobs/" + jobUID + "/items/" + strconv.Itoa(it.Seq) + "/content",
	}
}

type jobItemListDTO struct {
	// Items **按 seq 升序**。这是发给 ERP 的对外契约：
	// 「按序号取第几张图」必须跟返回顺序严格对应，有专门测试守着。
	Items []jobItemDTO `json:"items"`
}

// ---- 结果图 ----

type imageDTO struct {
	UID         string  `json:"uid"`
	JobUID      string  `json:"job_uid"`
	Seq         int     `json:"seq"`
	Attempt     int     `json:"attempt"`
	ImageIndex  int     `json:"image_index"`
	IsCurrent   bool    `json:"is_current"`
	ContentType string  `json:"content_type"`
	ByteSize    int64   `json:"byte_size"`
	Width       *int    `json:"width"`
	Height      *int    `json:"height"`
	BillingTier *string `json:"billing_tier"`
	CreatedAt   string  `json:"created_at"`
	ContentURL  string  `json:"content_url"`
}

func newImageDTO(prefix, jobUID string, seq int, img *dkdomain.Image) imageDTO {
	if img == nil {
		return imageDTO{}
	}
	return imageDTO{
		UID:         img.UID,
		JobUID:      jobUID,
		Seq:         seq,
		Attempt:     img.Attempt,
		ImageIndex:  img.ImageIndex,
		IsCurrent:   img.IsCurrent,
		ContentType: img.ContentType,
		ByteSize:    img.ByteSize,
		Width:       img.Width,
		Height:      img.Height,
		BillingTier: img.BillingTier,
		CreatedAt:   timeString(img.CreatedAt),
		ContentURL:  prefix + "/images/" + img.UID + "/content",
	}
}

func newImageViewDTO(prefix string, v *ImageView) imageDTO {
	if v == nil {
		return imageDTO{}
	}
	return newImageDTO(prefix, v.JobUID, v.Seq, v.Image)
}

// ---- 停止排队 ----

// stopJobDTO 是 POST /jobs/:uid/stop 的响应（决策 21）。
//
// 两个计数就是弹窗上那两个数。**message 是已经拼好的整句中文**，
// 界面直接显示即可 —— 这句话必须说清「哪些不扣费、哪些照扣费」，
// 否则运营会以为点了「停止」就一分钱不花。
type stopJobDTO struct {
	// StoppedCount 这次停掉了几张（还没开始的，**不生成也不扣费**）。
	StoppedCount int `json:"stopped_count"`
	// StillRunningCount 还有几张正在生成，**会跑完并正常扣费**，图会存进作品库。
	StillRunningCount int `json:"still_running_count"`
	// Message 给运营看的整句中文。
	Message string `json:"message"`
	// Job 停完之后的批次，界面拿它直接刷新那一行（四个计数都是最新的）。
	Job jobDTO `json:"job"`
}

func newStopJobDTO(prefix string, r *StopJobResult) stopJobDTO {
	if r == nil {
		return stopJobDTO{Message: stopJobMessage(0, 0)}
	}
	return stopJobDTO{
		StoppedCount:      r.Cancelled,
		StillRunningCount: r.StillRunning,
		Message:           stopJobMessage(r.Cancelled, r.StillRunning),
		Job:               newJobDTO(prefix, r.Job),
	}
}

// stopJobMessage 拼「停止排队」之后给运营看的那句话。
//
// 三种情况分开说，因为运营关心的只有一件事：**这次到底还要不要花钱。**
func stopJobMessage(stopped, stillRunning int) string {
	switch {
	case stillRunning > 0 && stopped > 0:
		return fmt.Sprintf(
			"已经停止排队。还没开始的 %d 张不会再生成、不扣费；正在生成的 %d 张会跑完并正常扣费，图会保存在作品库。",
			stopped, stillRunning)
	case stillRunning > 0:
		return fmt.Sprintf(
			"已经停止排队。没有还在排队的图了；正在生成的 %d 张会跑完并正常扣费，图会保存在作品库。",
			stillRunning)
	case stopped > 0:
		return fmt.Sprintf("已经停止排队。还没开始的 %d 张不会再生成、不扣费。", stopped)
	default:
		return "已经停止排队。这批任务没有还在排队或正在生成的图了。"
	}
}

// ---- 重试一张 ----

// retryAcceptedDTO 是 POST /jobs/:uid/items/:seq/retry 的响应（决策 20）。
type retryAcceptedDTO struct {
	// Item 重试之后这一张的最新状态（已经退回排队中，尝试次数已经 +1）。
	// images 是空数组：新图还没出来，旧图属于上一个版本。
	Item jobItemDTO `json:"item"`
	// Message 给运营看的中文说明，含「还能再试几次」。
	Message string `json:"message"`
	// RemainingAttempts 这一张还能再重试几次（累计上限默认 3 次，决策 20）。
	RemainingAttempts int `json:"remaining_attempts"`
}

func newRetryAcceptedDTO(prefix, jobUID string, item *dkdomain.JobItem) retryAcceptedDTO {
	if item == nil {
		return retryAcceptedDTO{Item: jobItemDTO{Currency: dkdomain.CurrencyUSD}}
	}
	remaining := item.MaxAttempts - item.AttemptCount
	if remaining < 0 {
		remaining = 0
	}
	return retryAcceptedDTO{
		Item:              newJobItemDTO(prefix, jobUID, &JobItemView{Item: item}),
		Message:           retryAcceptedMessage(remaining),
		RemainingAttempts: remaining,
	}
}

// retryAcceptedMessage 重试成功之后那句话。
//
// **必须写明「重新收一次费用」**：重试一张就是重新出一张图、重新扣一次钱，
// 技术上改不了（决策 20）。运营默认「失败 = 没扣钱」，不说清楚就会一顿猛点。
func retryAcceptedMessage(remaining int) string {
	if remaining <= 0 {
		return "已经重新排队，出好之后会自动出现在这一批里。这一张会重新收一次费用；重试次数已经用完，之后不能再重试了。"
	}
	return fmt.Sprintf(
		"已经重新排队，出好之后会自动出现在这一批里。这一张会重新收一次费用；之后还能再重试 %d 次。",
		remaining)
}

// ---- 我的消费 ----

// usageSummaryDTO 是 GET /me/usage/summary 的响应（决策 16）。
//
// 工作台角落显示「本月出图 N 张 / 花费 $X / 余额 $Y」。
// **金额一律美元**（决策 18），界面渲染 `$`，文案里不许出现「元」。
type usageSummaryDTO struct {
	// PeriodStart / PeriodEnd 统计窗口（本月），RFC3339 的 UTC 时间。
	// PeriodEnd 是下个月 1 号 0 点，**不含**这一刻。
	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`
	// ImageCount 窗口内出图张数。
	ImageCount int `json:"image_count"`
	// Currency 固定 "USD"。
	Currency string `json:"currency"`
	// Cost 窗口内的花费。
	Cost string `json:"cost"`
	// Balance 当前余额。
	Balance string `json:"balance"`
	// Available 当前可用额 = 余额 - 已冻结（还没出完的批次占着的那部分）。
	Available string `json:"available"`
	// AdminContact 管理员联系方式；没配置时是空串，界面就不显示那一行。
	AdminContact string `json:"admin_contact"`
}

func newUsageSummaryDTO(s *UsageSummary) usageSummaryDTO {
	if s == nil {
		return usageSummaryDTO{Currency: dkdomain.CurrencyUSD}
	}
	return usageSummaryDTO{
		PeriodStart:  timeString(s.PeriodStart),
		PeriodEnd:    timeString(s.PeriodEnd),
		ImageCount:   s.ImageCount,
		Currency:     dkdomain.CurrencyUSD,
		Cost:         dkdomain.MoneyString(s.Cost),
		Balance:      dkdomain.MoneyString(s.Balance),
		Available:    dkdomain.MoneyString(s.Available),
		AdminContact: s.AdminContact,
	}
}

// quotaRequestDTO 是 POST /me/quota-requests 的响应（决策 19）。
//
// **不返内部主键**：那是数据库的自增 id，对外一律不暴露。
// 运营需要知道的只有「已经通知到了没有」和「找谁」。
type quotaRequestDTO struct {
	// Status 处理状态，刚提交时是 pending。
	Status string `json:"status"`
	// Note 运营自己填的说明，没填就是 null。
	Note *string `json:"note"`
	// CreatedAt 提交时间。
	CreatedAt string `json:"created_at"`
	// AdminContact 管理员联系方式；没配置时是空串。
	AdminContact string `json:"admin_contact"`
	// Message 给运营看的整句中文，界面直接显示。
	Message string `json:"message"`
}

func newQuotaRequestDTO(r *QuotaRequestResult) quotaRequestDTO {
	if r == nil || r.Request == nil {
		return quotaRequestDTO{Message: quotaRequestMessage("")}
	}
	return quotaRequestDTO{
		Status:       string(r.Request.Status),
		Note:         r.Request.Note,
		CreatedAt:    timeString(r.Request.CreatedAt),
		AdminContact: r.AdminContact,
		Message:      quotaRequestMessage(r.AdminContact),
	}
}

// ---- 灵感库：分类 ----

// promptCategoryDTO 是灵感库左侧那一列分类里的一项。
//
// **没有 id 字段**：分类的自增主键对外一律不暴露，对外标识是 slug
// （GET /prompts?category=<slug> 用的也是它）。
type promptCategoryDTO struct {
	// Slug 稳定的英文标识，前端拿它当 key、也拿它做筛选参数。
	Slug string `json:"slug"`
	// Name 界面显示的名字。**默认中文**；中文名空着时退到英文名，再空就退到 slug ——
	// 宁可显示一个英文标识，也不要在界面上留一行空白让运营点不中。
	Name string `json:"name"`
	// NameEN 英文名，可能是空串。
	NameEN string `json:"name_en"`
	// SortOrder 界面排序，越小越靠前（items 本身已经是排好的，这个值只是给前端备用）。
	SortOrder int `json:"sort_order"`
	// PromptCount 这个分类下现在有多少条可用的提示词。
	PromptCount int `json:"prompt_count"`
}

// promptCategoryListDTO 是 GET /prompt-categories 的响应。
type promptCategoryListDTO struct {
	// Categories 分类列表，顺序即界面显示顺序。
	Categories []promptCategoryDTO `json:"categories"`
	// TotalPromptCount 全部分类加起来一共多少条，界面上「全部（14700）」那个数。
	TotalPromptCount int `json:"total_prompt_count"`
}

// categoryDisplayName 挑一个**一定不为空**的分类名。
func categoryDisplayName(c *dkdomain.PromptCategory) string {
	if c == nil {
		return ""
	}
	if c.NameZH != "" {
		return c.NameZH
	}
	if c.NameEN != "" {
		return c.NameEN
	}
	return c.Slug
}

func newPromptCategoryDTO(v *PromptCategoryView) promptCategoryDTO {
	if v == nil || v.Category == nil {
		return promptCategoryDTO{}
	}
	return promptCategoryDTO{
		Slug:        v.Category.Slug,
		Name:        categoryDisplayName(v.Category),
		NameEN:      v.Category.NameEN,
		SortOrder:   v.Category.SortOrder,
		PromptCount: v.PromptCount,
	}
}

func newPromptCategoryListDTO(views []*PromptCategoryView) promptCategoryListDTO {
	items := make([]promptCategoryDTO, 0, len(views))
	total := 0
	for _, v := range views {
		if v == nil || v.Category == nil {
			continue
		}
		items = append(items, newPromptCategoryDTO(v))
		total += v.PromptCount
	}
	return promptCategoryListDTO{Categories: items, TotalPromptCount: total}
}

// ---- 灵感库：提示词 ----

// promptVariableDTO 是提示词正文里的一个占位变量。
//
// 界面把它渲染成一个输入框：name 当标签，example 当 placeholder。
type promptVariableDTO struct {
	Name    string `json:"name"`
	Example string `json:"example"`
}

// promptDTO 是一条提示词。
//
// **body 是完整正文**，不做截断：运营点「用这条」就是把它整段带进工作台的输入框，
// 少一个字出来的图就不是他看到的那条。
type promptDTO struct {
	UID string `json:"uid"`
	// Title 标题，可能是空串（上游库里有不少条只有正文）。
	Title string `json:"title"`
	// Body 提示词正文，界面「用这条」按钮带走的就是它。
	Body string `json:"body"`
	// Variables 正文里的占位变量；没有变量时是**空数组**不是 null
	// （前端可以直接 v-for，不用先判空）。
	Variables []promptVariableDTO `json:"variables"`
	// PreviewURL 上游库给的效果预览图地址（外链，仅展示）；没有时是 null。
	PreviewURL *string `json:"preview_url"`
	// CategorySlug / CategoryName 所属分类；没有分类时都是空串。
	CategorySlug string `json:"category_slug"`
	CategoryName string `json:"category_name"`
	// Source youmind=从开源库同步来的；user=运营自己存的。
	Source string `json:"source"`
	// IsEnabled 是不是还在架上。列表里只会出现 true，
	// 详情页可能是 false（历史收藏点进来的）。
	IsEnabled bool   `json:"is_enabled"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func newPromptDTO(v *PromptView) promptDTO {
	if v == nil || v.Prompt == nil {
		return promptDTO{Variables: []promptVariableDTO{}}
	}
	p := v.Prompt

	vars := make([]promptVariableDTO, 0, len(p.Variables))
	for _, one := range p.Variables {
		vars = append(vars, promptVariableDTO{Name: one.Name, Example: one.Example})
	}

	return promptDTO{
		UID:          p.UID,
		Title:        p.Title,
		Body:         p.Body,
		Variables:    vars,
		PreviewURL:   p.PreviewURL,
		CategorySlug: v.CategorySlug,
		CategoryName: v.CategoryName,
		Source:       string(p.Source),
		IsEnabled:    p.IsEnabled,
		CreatedAt:    timeString(p.CreatedAt),
		UpdatedAt:    timeString(p.UpdatedAt),
	}
}

// promptListDTO 是 GET /prompts 的一页。
//
// 字段形状跟 jobListDTO 一致（items / has_more / next_cursor），
// 多一个 total 给界面显示「共 N 条」——灵感库有一万四千条，
// 不告诉运营总数的话，他不知道该搜还是该翻。
type promptListDTO struct {
	Items      []promptDTO `json:"items"`
	Total      int         `json:"total"`
	HasMore    bool        `json:"has_more"`
	NextCursor *string     `json:"next_cursor"`
}

func newPromptListDTO(page *PromptPage) promptListDTO {
	if page == nil {
		return promptListDTO{Items: []promptDTO{}}
	}
	items := make([]promptDTO, 0, len(page.Items))
	for _, v := range page.Items {
		items = append(items, newPromptDTO(v))
	}
	out := promptListDTO{Items: items, Total: page.Total, HasMore: page.HasMore}
	if page.HasMore && page.NextCursorID > 0 {
		cursor := encodePromptCursor(page.NextCursorID)
		out.NextCursor = &cursor
	}
	return out
}

// ---- 灵感库：同步 ----

// syncRunDTO 是一次同步的记录。
//
// ⚠ 失败原因这一列在数据库里叫 error，**对外不能直接叫 error** ——
// 那会跟错误响应体的顶层 error 对象撞名，ERP/前端按 `resp.error` 判断
// 「这次请求有没有出错」时会把一次「查询成功、但上次同步失败了」当成请求失败。
// 所以叫 error_message。（顺带满足「任何层级不许出现名为 code 的字段」。）
type syncRunDTO struct {
	// Kind manual=管理员点的；auto=定时跑的。
	Kind string `json:"kind"`
	// Status running / succeeded / failed / skipped。
	Status string `json:"status"`
	// StartedAt / FinishedAt 开始和结束时间；还在跑时 finished_at 是 null。
	StartedAt  string  `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
	// Fetched 从开源库拉回来多少条。
	Fetched int `json:"fetched"`
	// Inserted 新增、Updated 更新、Skipped 内容没变跳过。
	Inserted int `json:"inserted"`
	Updated  int `json:"updated"`
	Skipped  int `json:"skipped"`
	// DurationSeconds 这次跑了多少秒；还没跑完时是 null。
	DurationSeconds *int `json:"duration_seconds"`
	// ErrorMessage 失败原因；成功时是 null。
	ErrorMessage *string `json:"error_message"`
}

func newSyncRunDTO(run *dkdomain.SyncRun) *syncRunDTO {
	if run == nil {
		return nil
	}
	out := &syncRunDTO{
		Kind:         string(run.Kind),
		Status:       string(run.Status),
		StartedAt:    timeString(run.StartedAt),
		FinishedAt:   timeStringPtr(run.FinishedAt),
		Fetched:      run.Fetched,
		Inserted:     run.Inserted,
		Updated:      run.Updated,
		Skipped:      run.Skipped,
		ErrorMessage: run.Error,
	}
	if run.FinishedAt != nil {
		seconds := int(run.FinishedAt.Sub(run.StartedAt).Seconds())
		if seconds < 0 {
			seconds = 0
		}
		out.DurationSeconds = &seconds
	}
	return out
}

// syncStatusDTO 是 GET /prompts/sync/latest 的响应。
type syncStatusDTO struct {
	// Sync 最近一次同步；一次都没跑过时是 null。
	Sync *syncRunDTO `json:"sync"`
	// Running 现在是不是正有一次在跑。**前端靠它决定要不要继续轮询**。
	Running bool `json:"running"`
	// PromptCount / CategoryCount 库里现在有多少条、多少个分类。
	PromptCount   int `json:"prompt_count"`
	CategoryCount int `json:"category_count"`
	// Message 已经拼好的整句中文，管理员界面直接显示。
	Message string `json:"message"`
}

func newSyncStatusDTO(s *SyncStatusView) syncStatusDTO {
	if s == nil {
		return syncStatusDTO{Message: syncStatusMessage(nil, false)}
	}
	return syncStatusDTO{
		Sync:          newSyncRunDTO(s.Latest),
		Running:       s.Running,
		PromptCount:   s.PromptCount,
		CategoryCount: s.CategoryCount,
		Message:       syncStatusMessage(s.Latest, s.Running),
	}
}

// syncStatusMessage 拼给管理员看的那句话。
//
// 用中文人话，不出现「API」「token」「HTTP」这类词（CLAUDE.md 铁律）。
func syncStatusMessage(run *dkdomain.SyncRun, running bool) string {
	if running {
		return "正在同步灵感库，大概需要十几秒，这个页面会自动刷新。"
	}
	if run == nil {
		return "还没有同步过灵感库。点「立即同步」把提示词拉进来。"
	}
	switch run.Status {
	case dkdomain.SyncStatusSucceeded:
		return fmt.Sprintf(
			"上次同步完成于 %s：拉到 %d 条，新增 %d 条，更新 %d 条，没变化 %d 条。",
			localTimeText(run.FinishedAt), run.Fetched, run.Inserted, run.Updated, run.Skipped)
	case dkdomain.SyncStatusFailed:
		// 失败原因单独放在 error_message 里，这句话只负责告诉管理员「该干什么」。
		return fmt.Sprintf("上次同步（%s）没成功，灵感库还是原来的内容。可以点「立即同步」再试一次；"+
			"一直失败多半是拉不到外网，检查一下「同步走的代理」这一项。", localTimeText(run.FinishedAt))
	case dkdomain.SyncStatusSkipped:
		return "上次点同步的时候已经有一次在跑，这次就跳过了。灵感库内容不受影响。"
	case dkdomain.SyncStatusRunning:
		// 走到这里说明记录还挂着「进行中」，但已经超时太久了（多半是服务重启过）。
		// **必须说清「可以再点」**：不说的话管理员看着一个转不完的圈，
		// 而按钮又是禁用的 —— 那就是个死局，只能改数据库才解得开。
		return "上次同步没有正常结束（多半是服务重启了），灵感库还是原来的内容。可以点「立即同步」重新跑一次。"
	default:
		return "灵感库同步状态未知，可以点「立即同步」跑一次看看。"
	}
}

// localTimeText 把时间写成运营看得懂的样子。
//
// 刻意**不用** RFC3339：给管理员看的整句话里出现 2026-08-13T01:02:03Z
// 只会让人以为是乱码。机器要读的时间在 started_at / finished_at 字段里，
// 那两个仍然是标准格式。
func localTimeText(t *time.Time) string {
	if t == nil {
		return "刚刚"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// syncStartedDTO 是 POST /prompts/sync 的响应（HTTP 202）。
//
// 同步是长任务（一万四千条，约 15 秒），所以这里**立刻返回**，
// 不让请求挂着等 —— 中间任何一个反代超时都会让管理员看到「失败」，
// 而同步其实跑得好好的。真正的进度靠 poll_url 轮询。
type syncStartedDTO struct {
	// Accepted 固定 true（没接受的情况会走错误响应，不会走到这里）。
	Accepted bool `json:"accepted"`
	// Sync 刚建好的那条 running 记录。
	Sync *syncRunDTO `json:"sync"`
	// PollURL 去哪儿看进度。运行时拼，跟着挂载前缀走。
	PollURL string `json:"poll_url"`
	// Message 给管理员看的整句中文。
	Message string `json:"message"`
}

func newSyncStartedDTO(prefix string, run *dkdomain.SyncRun) syncStartedDTO {
	return syncStartedDTO{
		Accepted: true,
		Sync:     newSyncRunDTO(run),
		PollURL:  prefix + "/prompts/sync/latest",
		Message:  "已经开始同步灵感库，大概需要十几秒。可以离开这个页面，同步会在后台继续。",
	}
}

// ---- 灵感库：同步用的代理 ----

// proxyOptionDTO 是「同步走哪个代理」下拉框里的一项。
//
// ⚠ 字段名跟上游 GET /api/v1/admin/proxies/all 的响应**逐字对齐**
// （internal/handler/dto/types.go 的 Proxy）：前端那个下拉框直接复用上游的
// frontend/src/components/common/ProxySelector.vue，
// 名字对不上就是一个空白下拉框，而且不会报任何错。
//
// **没有 password 字段**：上游 dto.Proxy 把它标成 `json:"-"`，我们照做。
type proxyOptionDTO struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Status   string `json:"status"`

	ExpiresAt      *string `json:"expires_at"`
	FallbackMode   string  `json:"fallback_mode"`
	BackupProxyID  *int64  `json:"backup_proxy_id"`
	ExpiryWarnDays int     `json:"expiry_warn_days"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

func newProxyOptionDTO(p ProxyOption) proxyOptionDTO {
	return proxyOptionDTO{
		ID:             p.ID,
		Name:           p.Name,
		Protocol:       p.Protocol,
		Host:           p.Host,
		Port:           p.Port,
		Username:       p.Username,
		Status:         p.Status,
		ExpiresAt:      timeStringPtr(p.ExpiresAt),
		FallbackMode:   p.FallbackMode,
		BackupProxyID:  p.BackupProxyID,
		ExpiryWarnDays: p.ExpiryWarnDays,
		CreatedAt:      timeString(p.CreatedAt),
		UpdatedAt:      timeString(p.UpdatedAt),
	}
}

// syncSettingsDTO 是 GET / PUT /prompts/sync/settings 的响应。
//
// **两样东西必须一起给**：选中的是谁（proxy_id）和能选谁（proxies）。
// 分成两个接口的话，运营会在两次请求之间看到一个短暂的「无代理」。
type syncSettingsDTO struct {
	// ProxyID 现在选的代理；null = 不走代理（直连）。
	// 前端 `<ProxySelector v-model="form.proxy_id" :proxies="proxies" />` 绑的就是它。
	ProxyID *int64 `json:"proxy_id"`
	// ProxyName 选中那个代理的名字，方便界面在别处显示；没选时是空串。
	ProxyName string `json:"proxy_name"`
	// Proxies 可选的代理，顺序即下拉框顺序。**没有代理时是空数组不是 null。**
	Proxies []proxyOptionDTO `json:"proxies"`
	// Message 给管理员看的整句中文，说清「这个代理只给同步用」。
	Message string `json:"message"`
}

func newSyncSettingsDTO(s *SyncSettings) syncSettingsDTO {
	if s == nil {
		return syncSettingsDTO{Proxies: []proxyOptionDTO{}, Message: syncProxyMessage("")}
	}
	items := make([]proxyOptionDTO, 0, len(s.Proxies))
	name := ""
	for _, p := range s.Proxies {
		items = append(items, newProxyOptionDTO(p))
		if s.ProxyID != nil && p.ID == *s.ProxyID {
			name = p.Name
		}
	}
	return syncSettingsDTO{
		ProxyID:   s.ProxyID,
		ProxyName: name,
		Proxies:   items,
		Message:   syncProxyMessage(name),
	}
}

// syncProxyMessage 说清楚这个设置管的是什么。
//
// **必须写明「只影响同步」**：不写的话，管理员会以为选了代理之后出图也走代理，
// 于是在出图变慢时去改这里 —— 改了没用，还找不到原因。
// （老代码的注释原话：「不能用全局代理或环境变量——那会把生图请求也带进代理」。）
func syncProxyMessage(proxyName string) string {
	if proxyName == "" {
		return "现在不走代理，直接去开源提示词库拉取。这一项只影响灵感库同步，不影响出图。"
	}
	return "同步会通过「" + proxyName + "」去开源提示词库拉取。这一项只影响灵感库同步，不影响出图。"
}

// quotaRequestMessage 「申请额度」成功之后那句话。
//
// 有联系方式就带上：充值和兑换码菜单对运营是隐藏的（决策 10），
// 「余额不足」对他来说本来是死路一条，这条路必须指到具体的人（决策 19）。
func quotaRequestMessage(adminContact string) string {
	if adminContact == "" {
		return "已经把申请提交给管理员了，请等管理员加额度，不用重复提交。"
	}
	return "已经把申请提交给管理员了，请等管理员加额度，不用重复提交。着急的话可以直接找：" + adminContact
}
