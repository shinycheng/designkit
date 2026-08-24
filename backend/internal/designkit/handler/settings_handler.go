package handler

// ============================================================================
// designkit 设置（**仅管理员**）
// ============================================================================
//
// 两个端点，只挂浏览器组（/api/v1/designkit）：
//
//	GET  /admin/settings   读全部配置 + 每一项的允许范围 + 按当前配置算出来的价格预览
//	PUT  /admin/settings   改配置（只改传上来的那几项，没传的保持不变）
//
// 为什么不挂 ERP 组（/v1/designkit）：改并发数、改单价是运维动作。
// 给外部系统那把 Key 一个「把出图并发调到 8」的入口没有任何好处，
// 只会多一个误触面 —— 跟灵感库同步那组的理由完全一样。
//
// # 三件必须守住的事
//
//  1. **护栏在服务端，不在界面上。**
//     界面上的 min/max 只是提示，改一改网页就能绕过去。真正拦下来的是本文件。
//     并发数、单次批量、单张超时这三项填过头的后果都不是「报错」，
//     而是「看起来正常、实际很难用」——必须在保存这一刻就拦住并说明理由。
//
//  2. **空的单价绝不能变成 0。**
//     价目表还没实测出来（设计定型 第十节 1）。没填的档一律**不写进配置**，
//     让报价那边照常走「价格待确认」。写成 0 会让运营读成「免费」，
//     然后一口气排几十张。
//
//  3. **灵感库同步用的代理在这里是只读的。**
//     它已经在灵感库页面能改了。两个地方都能改、改了不同步，
//     结果就是「界面上显示走代理、实际在裸连」这种最难查的状态。
//     所以这里只显示现在选的是谁，改要去灵感库那一页。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/gin-gonic/gin"
)

// ----------------------------------------------------------------------------
// 允许范围（护栏）
// ----------------------------------------------------------------------------
//
// ⚠ 这几个数字**镜像**别处已经生效的收敛区间，改这里要三处一起改，
// 否则会出现「界面说保存成功、跑起来是另一个值」：
//
//	单次批量   service/job.go   jobMinBatchItems / jobMaxBatchItemsLimit  = 1 / 500
//	单张超时   worker/config.go LoadSettings 里 readIntSetting 的 10 / 3600
//	最长边     worker/config.go 256 / 8192，service/asset.go 同名区间也是 256 / 8192
//	并发数     worker/config.go MaxConcurrency = 16、SafeConcurrency = 4
//
// 本文件的区间**只能比它们更严**，不能更松：更松的话，管理员填的值会在跑的时候
// 被静默收敛回去，而设置页上显示的还是他填的那个数。
const (
	// SettingsMinBatchItems / SettingsMaxBatchItems 单次批量张数的允许范围。
	// 下限 1：填 0 等于谁也提交不了。
	// 上限 500：一次提交就是一次事务写 500 行，再多会把提交这一步拖到几十秒。
	SettingsMinBatchItems = 1
	SettingsMaxBatchItems = 500

	// SettingsMinItemTimeoutSeconds / SettingsMaxItemTimeoutSeconds 单张出图超时的允许范围。
	// 下限 10 秒：比一张图正常耗时还短，填了等于「每张都超时」。
	// 上限 3600 秒：一张图卡一个小时不算超时的话，排在后面的人一整天都轮不上。
	SettingsMinItemTimeoutSeconds = 10
	SettingsMaxItemTimeoutSeconds = 3600

	// SettingsMinDimension / SettingsMaxDimension 发给网关的图片最长边的允许范围。
	SettingsMinDimension = 256
	SettingsMaxDimension = 8192

	// SettingsMinConcurrency / SettingsMaxConcurrency 同时出几张的允许范围。
	//
	// 上限刻意定成 8 而不是队列本身的硬顶 16：上游给每个用户默认只放 5 个并发，
	// 填到 8 已经是明显超卖了，再高只会让所有人一起排队。
	SettingsMinConcurrency = 1
	SettingsMaxConcurrency = 8

	// SettingsSafeConcurrency 超过这个数就会开始跟运营在界面上的单张操作抢并发格子。
	// 不拦（4→5 只是变慢，不是坏），但保存之后要把这句话说给管理员听。
	SettingsSafeConcurrency = 4

	// SettingsMaxRatios 出图比例最多配几个。界面上是一排按钮，十几个已经挑花眼了。
	SettingsMaxRatios = 12

	// SettingsMaxModelLen 出图模型名的长度上限。
	SettingsMaxModelLen = 64

	// SettingsMaxAdminContactLen 管理员联系方式的长度上限。
	// 它会被拼进「余额不足」的提示里，太长会把提示撑爆。
	SettingsMaxAdminContactLen = 200
)

// settingsMaxUnitPrice 单张单价的上限（美元）。
//
// 不是技术限制，是**手滑护栏**：实测下来一张图是几分钱的量级，
// 多打两个零就是每张 $4.2，一批 50 张 = $210。
const settingsMaxUnitPrice = "10"

// settingsMinRateMultiplier / settingsMaxRateMultiplier 报价倍率的允许范围。
// 0 和负数都拒绝：0 没有任何合理用途（真要免费就把单价留空走「价格待确认」，
// 或者干脆填一个很小的数），负数更无意义；10 倍以上多半是手滑多打了个零。
//
// 注意别把这条护栏的理由说成「填 0 会让工作台显示免费」——不会。
// 后端读倍率时读到 0 会退回 1（service/job.go 的 rateMultiplier），
// 所以真填进去的后果是「界面接受了，实际按 1 算」，一样是不该发生的不一致。
const (
	settingsMinRateMultiplier = "0.01"
	settingsMaxRateMultiplier = "10"
)

// ----------------------------------------------------------------------------
// service 接口
// ----------------------------------------------------------------------------

// SettingsView 是「现在配的是什么」。**只读**，由组合根从 designkit_settings 拼出来。
type SettingsView struct {
	// Ratios 可选出图比例，顺序即界面显示顺序。
	Ratios []dkdomain.Ratio
	// Model 出图模型名。
	Model string
	// MaxBatchItems 单次批量张数上限。
	MaxBatchItems int
	// ItemTimeoutSeconds 单张出图超时秒数。
	ItemTimeoutSeconds int
	// MaxDimension 发给网关的图片最长边像素。
	MaxDimension int
	// WorkerConcurrency 同时出几张。
	WorkerConcurrency int
	// AdminContact 管理员联系方式；空串表示没配。
	AdminContact string

	// CleanupEnabled 图片自动清理开关（决策 17）。默认 false = 图片永久保留。
	CleanupEnabled bool
	// CleanupRetentionDays 图片保留天数。清理任务每一轮现读，改完不用重启。
	CleanupRetentionDays int

	// UnitPrices 计费档 → 十进制字符串金额（美元）。
	//
	// **没测出来的档不要出现在这个 map 里**，也不要放空串或 "0" ——
	// 缺档的语义是「价格待确认」，报价那边就是这么读的。
	UnitPrices map[string]string

	// RateMultiplier 报价倍率，十进制字符串。空串表示没配（按 1 算）。
	RateMultiplier string

	// PromptSyncProxyID 灵感库同步走哪个代理；nil = 直连。**本页只读。**
	PromptSyncProxyID *int64
	// PromptSyncProxyName 那个代理的名字；直连或代理已被删掉时是空串。
	PromptSyncProxyName string
}

// SettingsInput 是一次保存。**每个字段都是指针：nil 表示这一项不动。**
//
// 为什么不做「整份覆盖」：设置页以后会拆成几张卡片分别保存，
// 整份覆盖的话，任何一次前端漏传字段都会把那一项悄悄清空 ——
// 表现是「昨天还好好的，今天出图比例少了一个」，而页面上看不出任何异常。
type SettingsInput struct {
	Ratios               []dkdomain.Ratio
	Model                *string
	MaxBatchItems        *int
	ItemTimeoutSeconds   *int
	MaxDimension         *int
	WorkerConcurrency    *int
	AdminContact         *string
	CleanupEnabled       *bool
	CleanupRetentionDays *int
	// UnitPrices 非 nil 表示整份替换单价表。map 里只放**填了值**的档。
	UnitPrices map[string]string
	// RateMultiplier 十进制字符串。
	RateMultiplier *string
}

// SettingsService 读写 designkit 自己的配置（designkit_settings 表）。
//
// **不碰上游的 settings 表**：撞键的话，跟版之后上游后台页面会把我们的值改掉。
type SettingsService interface {
	// GetSettings 读全部配置。任何一项读不到都用兜底默认值，不报错。
	GetSettings(ctx context.Context) (*SettingsView, error)
	// PutSettings 保存 in 里非 nil 的那几项，返回保存之后的完整配置。
	PutSettings(ctx context.Context, in SettingsInput) (*SettingsView, error)
}

// ----------------------------------------------------------------------------
// 对外 JSON
// ----------------------------------------------------------------------------

// settingsDTO 是 GET / PUT 的响应体。字段名一律 snake_case，跟别的端点一致。
type settingsDTO struct {
	Ratios             []string `json:"ratios"`
	Model              string   `json:"model"`
	MaxBatchItems      int      `json:"max_batch_items"`
	ItemTimeoutSeconds int      `json:"item_timeout_seconds"`
	MaxDimension       int      `json:"max_dimension"`
	WorkerConcurrency  int      `json:"worker_concurrency"`
	AdminContact       string   `json:"admin_contact"`

	// CleanupEnabled / CleanupRetentionDays 图片自动清理（决策 17）。
	// 开关默认关；两项改完都**立刻生效**（清理任务每一轮现读），不进 RestartNote。
	CleanupEnabled       bool `json:"cleanup_enabled"`
	CleanupRetentionDays int  `json:"cleanup_retention_days"`

	// UnitPrices 只包含**已经填了**的档。没填的档不出现在这里，
	// 前端据此显示「价格待确认」。**永远不要在这里放 0。**
	UnitPrices map[string]string `json:"unit_prices"`
	// RateMultiplier 空串表示没配（按 1 算）。
	RateMultiplier string `json:"rate_multiplier"`

	// PromptSync 灵感库同步用的代理，**只读**，改要去灵感库页面。
	PromptSync settingsPromptSyncDTO `json:"prompt_sync"`

	// Limits 每一项的允许范围，前端拿它渲染输入框的 min/max 和提示语。
	// 写死在前端会跟服务端的护栏对不上，所以由服务端下发。
	Limits settingsLimitsDTO `json:"limits"`

	// Warnings 保存成功、但有话要说（例如并发数超过安全值）。空数组表示没有。
	Warnings []string `json:"warnings"`

	// RestartNote 哪些项改完要重启才生效，整句中文。没有要重启的项时是空串。
	RestartNote string `json:"restart_note"`
}

type settingsPromptSyncDTO struct {
	ProxyID   *int64 `json:"proxy_id"`
	ProxyName string `json:"proxy_name"`
	// Message 整句中文，例如「现在不走代理，直接去开源提示词库拉取。」
	Message string `json:"message"`
	// Editable 恒为 false：这一项在这一页只能看，改要去灵感库那一页。
	Editable bool `json:"editable"`
	// EditPath 去哪儿改。前端把它渲染成一个链接。
	EditPath string `json:"edit_path"`
}

type settingsLimitsDTO struct {
	MaxBatchItemsMin      int `json:"max_batch_items_min"`
	MaxBatchItemsMax      int `json:"max_batch_items_max"`
	ItemTimeoutSecondsMin int `json:"item_timeout_seconds_min"`
	ItemTimeoutSecondsMax int `json:"item_timeout_seconds_max"`
	MaxDimensionMin       int `json:"max_dimension_min"`
	MaxDimensionMax       int `json:"max_dimension_max"`
	WorkerConcurrencyMin  int `json:"worker_concurrency_min"`
	WorkerConcurrencyMax  int `json:"worker_concurrency_max"`
	// WorkerConcurrencySafe 超过它就会跟运营的单张操作抢并发格子（不拦，只提醒）。
	WorkerConcurrencySafe int `json:"worker_concurrency_safe"`
	RatiosMax             int `json:"ratios_max"`
	ModelMaxLength        int `json:"model_max_length"`
	AdminContactMaxLength int `json:"admin_contact_max_length"`
	// CleanupRetentionDaysMin / Max 图片保留天数的允许范围。
	CleanupRetentionDaysMin int `json:"cleanup_retention_days_min"`
	CleanupRetentionDaysMax int `json:"cleanup_retention_days_max"`
	// Tiers 单价表有哪几档，从便宜到贵。
	Tiers []string `json:"tiers"`
	// UnitPriceMax 单张单价的上限（十进制字符串）。
	UnitPriceMax string `json:"unit_price_max"`
	// RateMultiplierMin / RateMultiplierMax 报价倍率的允许范围。
	RateMultiplierMin string `json:"rate_multiplier_min"`
	RateMultiplierMax string `json:"rate_multiplier_max"`
}

// settingsRestartNote 说清楚哪几项要重启。
//
// 为什么必须有这句话：并发数 / 单张超时 / 最长边这三项，出图队列是在**启动那一刻**
// 读一次就不再读了（worker/pool.go 的 Start 里调 LoadSettings）。
// 不写这句话，管理员把并发从 4 改成 2 之后会盯着日志等半天，
// 然后得出「这个设置页是假的」这个结论。
const settingsRestartNote = "「同时出几张」「单张出图超时」「图片最长边」这三项，" +
	"已经在排队和正在生成的图还是按旧值跑；要让它们完全生效，请重启一次服务。" +
	"（其余各项保存后立刻生效，不用重启。）" +
	"⚠ 特别是「图片最长边」：不重启的话，工作台上显示的出图尺寸已经是新值，" +
	"真正发给中转平台的还是旧值——两边对不上，而且不会有任何报错。"

// settingsRestartKeys 改了哪些项要提示重启。
var settingsRestartKeys = map[string]bool{
	"worker_concurrency":   true,
	"item_timeout_seconds": true,
	"max_dimension":        true,
}

// promptSyncEditPath 灵感库页面的路径（同步代理在那儿改）。
// 跟 frontend/src/features/designkit/nav.ts 的 DESIGNKIT_INSPIRATION_PATH 一致。
const promptSyncEditPath = "/inspiration"

func newSettingsDTO(v *SettingsView, warnings []string, restartNote string) settingsDTO {
	if v == nil {
		v = &SettingsView{}
	}

	ratios := make([]string, 0, len(v.Ratios))
	for _, r := range v.Ratios {
		ratios = append(ratios, r.String())
	}

	// map 一律给出去一个非 nil 的，前端不用判空。
	prices := make(map[string]string, len(v.UnitPrices))
	for tier, amount := range v.UnitPrices {
		amount = strings.TrimSpace(amount)
		if amount == "" {
			// 空 = 待确认，不下发这一档。见文件头第 2 条。
			continue
		}
		prices[tier] = amount
	}

	if warnings == nil {
		warnings = []string{}
	}

	return settingsDTO{
		Ratios:               ratios,
		Model:                v.Model,
		MaxBatchItems:        v.MaxBatchItems,
		ItemTimeoutSeconds:   v.ItemTimeoutSeconds,
		MaxDimension:         v.MaxDimension,
		WorkerConcurrency:    v.WorkerConcurrency,
		AdminContact:         v.AdminContact,
		CleanupEnabled:       v.CleanupEnabled,
		CleanupRetentionDays: v.CleanupRetentionDays,
		UnitPrices:           prices,
		RateMultiplier:       strings.TrimSpace(v.RateMultiplier),
		PromptSync: settingsPromptSyncDTO{
			ProxyID:   v.PromptSyncProxyID,
			ProxyName: v.PromptSyncProxyName,
			Message:   syncProxyMessage(v.PromptSyncProxyName),
			Editable:  false,
			EditPath:  promptSyncEditPath,
		},
		Limits: settingsLimitsDTO{
			MaxBatchItemsMin:        SettingsMinBatchItems,
			MaxBatchItemsMax:        SettingsMaxBatchItems,
			ItemTimeoutSecondsMin:   SettingsMinItemTimeoutSeconds,
			ItemTimeoutSecondsMax:   SettingsMaxItemTimeoutSeconds,
			MaxDimensionMin:         SettingsMinDimension,
			MaxDimensionMax:         SettingsMaxDimension,
			WorkerConcurrencyMin:    SettingsMinConcurrency,
			WorkerConcurrencyMax:    SettingsMaxConcurrency,
			WorkerConcurrencySafe:   SettingsSafeConcurrency,
			RatiosMax:               SettingsMaxRatios,
			ModelMaxLength:          SettingsMaxModelLen,
			AdminContactMaxLength:   SettingsMaxAdminContactLen,
			CleanupRetentionDaysMin: dkdomain.MinCleanupRetentionDays,
			CleanupRetentionDaysMax: dkdomain.MaxCleanupRetentionDays,
			Tiers:                   dkdomain.AllBillingTiers(),
			UnitPriceMax:            settingsMaxUnitPrice,
			RateMultiplierMin:       settingsMinRateMultiplier,
			RateMultiplierMax:       settingsMaxRateMultiplier,
		},
		Warnings:    warnings,
		RestartNote: restartNote,
	}
}

// ----------------------------------------------------------------------------
// handler
// ----------------------------------------------------------------------------

// 价格预览**只在前端算**（components/settings/PricingPreviewTable.vue）。
//
// 后端原来也算了一份下发，但页面从来没用过它——那张表必须跟着输入框实时变，
// 服务端下发的只能是「保存后的样子」。留着就是同一条计费规则的两份实现，
// 迟早对不上，而且对不上时没人会发现（两边看起来都合理）。
//
// ⚠ 前端那份的分档判据必须跟 domain.ClassifyBillingTier 保持一致
// （≤1024 → 1K，≤2048 → 2K，更大 → 4K）。改其中一边时记得改另一边。
// 真正扣钱用的档由上游按**实际输出像素**定，这张表只是预估。

// SettingsHandler designkit 设置的读写。**仅管理员**（路由上挂 RequireAdmin）。
type SettingsHandler struct {
	settings SettingsService
}

// NewSettingsHandler 建 handler。
func NewSettingsHandler(settings SettingsService) *SettingsHandler {
	return &SettingsHandler{settings: settings}
}

// Get 处理 GET /admin/settings。
func (h *SettingsHandler) Get(c *gin.Context) {
	view, err := h.settings.GetSettings(c.Request.Context())
	if err != nil {
		failService(c, err, dkdomain.ErrCodeInternal)
		return
	}
	c.JSON(http.StatusOK, newSettingsDTO(view, nil, ""))
}

// Update 处理 PUT /admin/settings。
//
// 只改传上来的那几项。校验全部在这里做完再落库 —— 一半合法一半不合法时
// **一项都不写**，不然管理员看到「保存失败」，而其实前三项已经改了。
func (h *SettingsHandler) Update(c *gin.Context) {
	body, ok := decodeSettingsBody(c)
	if !ok {
		return
	}

	in, warnings, restart, ok := body.validate(c)
	if !ok {
		return
	}

	view, err := h.settings.PutSettings(c.Request.Context(), in)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeInternal)
		return
	}

	note := ""
	if restart {
		note = settingsRestartNote
	}
	c.JSON(http.StatusOK, newSettingsDTO(view, warnings, note))
}

// settingsBody 是 PUT 的请求体。
//
// 全部用指针 / map：**分得清「没传这一项」和「传了一个空值」**。
// 普通结构体做不到 —— 没传 max_batch_items 和传了 0 都是 0，
// 于是「前端漏传一个字段」会被当成「管理员要把批量上限改成 0」，谁也提交不了图。
type settingsBody struct {
	Ratios               *[]string          `json:"ratios"`
	Model                *string            `json:"model"`
	MaxBatchItems        *int               `json:"max_batch_items"`
	ItemTimeoutSeconds   *int               `json:"item_timeout_seconds"`
	MaxDimension         *int               `json:"max_dimension"`
	WorkerConcurrency    *int               `json:"worker_concurrency"`
	AdminContact         *string            `json:"admin_contact"`
	CleanupEnabled       *bool              `json:"cleanup_enabled"`
	CleanupRetentionDays *int               `json:"cleanup_retention_days"`
	UnitPrices           *map[string]string `json:"unit_prices"`
	RateMultiplier       *string            `json:"rate_multiplier"`
}

// decodeSettingsBody 解请求体。返回 false 时已经写好错误响应。
func decodeSettingsBody(c *gin.Context) (*settingsBody, bool) {
	var body settingsBody
	if err := c.ShouldBindJSON(&body); err != nil {
		msg := "提交的内容格式不对，请刷新页面重试。"
		if errors.Is(err, io.EOF) {
			msg = "没有提交任何内容。请先改一项设置再保存。"
		}
		// 数字格式错误（例如输入框里打了汉字）单独给一句能听懂的话。
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			msg = "「" + settingsFieldLabel(typeErr.Field) + "」填的内容格式不对，请检查一下。"
		}
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage(msg).WithCause(err))
		return nil, false
	}
	return &body, true
}

// settingsFieldLabel 把 JSON 字段名换成运营看得懂的中文。
// 认不出来时原样返回，总比显示一个空的书名号强。
func settingsFieldLabel(field string) string {
	switch field {
	case "ratios":
		return "出图比例"
	case "model":
		return "出图模型"
	case "max_batch_items":
		return "单次最多出几张"
	case "item_timeout_seconds":
		return "单张出图超时"
	case "max_dimension":
		return "图片最长边"
	case "worker_concurrency":
		return "同时出几张"
	case "admin_contact":
		return "管理员联系方式"
	case "cleanup_enabled":
		return "图片自动清理"
	case "cleanup_retention_days":
		return "图片保留天数"
	case "unit_prices":
		return "出图单价"
	case "rate_multiplier":
		return "价格倍率"
	}
	return field
}

// settingsValidationError 造一条「哪一项不合法 + 为什么」的错误。
//
// 文案规矩：先说是哪一项（带书名号，跟界面上的标题逐字一致，方便对上），
// 再说该填什么，最后说填错的后果。**不要只说「参数不合法」** ——
// 那句话对运营来说等于没说。
func settingsValidationError(field, reason string) *dkdomain.DesignkitError {
	return dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
		WithMessagef("「%s」%s", settingsFieldLabel(field), reason)
}

// validate 校验并转成 service 要的入参。
//
// 返回值：入参、保存成功之后要说的话、要不要提示重启、是否通过。
// 不通过时已经写好错误响应，调用方直接 return。
func (b *settingsBody) validate(c *gin.Context) (SettingsInput, []string, bool, bool) {
	var (
		in       SettingsInput
		warnings []string
		restart  bool
		changed  = map[string]bool{}
	)

	// ---- 出图比例 ----
	if b.Ratios != nil {
		values := *b.Ratios
		if len(values) == 0 {
			abortWithDesignkitError(c, settingsValidationError("ratios",
				"至少要留一个。一个都不留的话，运营在工作台上选不出比例，一张图也提交不了。"))
			return in, nil, false, false
		}
		if len(values) > SettingsMaxRatios {
			abortWithDesignkitError(c, settingsValidationError("ratios",
				"最多配 "+strconv.Itoa(SettingsMaxRatios)+" 个。再多工作台上就是一大排按钮，运营挑不过来。"))
			return in, nil, false, false
		}
		seen := make(map[dkdomain.Ratio]bool, len(values))
		ratios := make([]dkdomain.Ratio, 0, len(values))
		for _, raw := range values {
			parsed, err := dkdomain.ParseRatio(raw)
			if err != nil {
				abortWithDesignkitError(c, settingsValidationError("ratios",
					"里的「"+strings.TrimSpace(raw)+"」写法不对。要写成「宽:高」，例如 3:4，中间是英文冒号。"))
				return in, nil, false, false
			}
			if seen[parsed] {
				abortWithDesignkitError(c, settingsValidationError("ratios",
					"里的「"+parsed.String()+"」重复了，请删掉多余的那个。"))
				return in, nil, false, false
			}
			seen[parsed] = true
			ratios = append(ratios, parsed)
		}
		in.Ratios = ratios
		changed["ratios"] = true
	}

	// ---- 出图模型 ----
	if b.Model != nil {
		model := strings.TrimSpace(*b.Model)
		if model == "" {
			abortWithDesignkitError(c, settingsValidationError("model",
				"不能留空。留空的话，出图请求发出去会被网关拒绝，每一张都失败。"))
			return in, nil, false, false
		}
		if len([]rune(model)) > SettingsMaxModelLen {
			abortWithDesignkitError(c, settingsValidationError("model",
				"太长了，最多 "+strconv.Itoa(SettingsMaxModelLen)+" 个字符。"))
			return in, nil, false, false
		}
		if strings.ContainsAny(model, " \t\r\n") {
			abortWithDesignkitError(c, settingsValidationError("model",
				"中间不能有空格。请照着中转平台上写的模型名原样填，例如 gpt-image-2。"))
			return in, nil, false, false
		}
		in.Model = &model
		changed["model"] = true
	}

	// ---- 单次最多出几张 ----
	if b.MaxBatchItems != nil {
		value := *b.MaxBatchItems
		if value < SettingsMinBatchItems || value > SettingsMaxBatchItems {
			abortWithDesignkitError(c, settingsValidationError("max_batch_items",
				"要填 "+strconv.Itoa(SettingsMinBatchItems)+" 到 "+strconv.Itoa(SettingsMaxBatchItems)+" 之间的整数。"+
					"填 0 谁也提交不了；填得太大，一次提交要写几百条记录，运营会觉得点了没反应。"))
			return in, nil, false, false
		}
		in.MaxBatchItems = &value
		changed["max_batch_items"] = true
	}

	// ---- 单张出图超时 ----
	if b.ItemTimeoutSeconds != nil {
		value := *b.ItemTimeoutSeconds
		if value < SettingsMinItemTimeoutSeconds || value > SettingsMaxItemTimeoutSeconds {
			abortWithDesignkitError(c, settingsValidationError("item_timeout_seconds",
				"要填 "+strconv.Itoa(SettingsMinItemTimeoutSeconds)+" 到 "+strconv.Itoa(SettingsMaxItemTimeoutSeconds)+" 秒之间。"+
					"填太短会让本来能出的图被判成超时（钱可能已经花了）；填太长的话，"+
					"卡住的那一张会一直占着位置，后面排队的人一直等。"))
			return in, nil, false, false
		}
		in.ItemTimeoutSeconds = &value
		changed["item_timeout_seconds"] = true
	}

	// ---- 图片最长边 ----
	if b.MaxDimension != nil {
		value := *b.MaxDimension
		if value < SettingsMinDimension || value > SettingsMaxDimension {
			abortWithDesignkitError(c, settingsValidationError("max_dimension",
				"要填 "+strconv.Itoa(SettingsMinDimension)+" 到 "+strconv.Itoa(SettingsMaxDimension)+" 之间。"+
					"这个数直接决定每张图按哪一档计费（2048 及以下是 2K 档，超过就是 4K 档，4K 明显更贵），"+
					"改之前先看下面的价格预览。"))
			return in, nil, false, false
		}
		in.MaxDimension = &value
		changed["max_dimension"] = true
	}

	// ---- 同时出几张 ----
	if b.WorkerConcurrency != nil {
		value := *b.WorkerConcurrency
		if value < SettingsMinConcurrency || value > SettingsMaxConcurrency {
			abortWithDesignkitError(c, settingsValidationError("worker_concurrency",
				"要填 "+strconv.Itoa(SettingsMinConcurrency)+" 到 "+strconv.Itoa(SettingsMaxConcurrency)+" 之间。"+
					"建议就填 "+strconv.Itoa(SettingsSafeConcurrency)+"：中转平台给每个账号同时只放 5 个请求过去，"+
					"我们占满之后，你自己在界面上点一张图也得排队等。"))
			return in, nil, false, false
		}
		if value > SettingsSafeConcurrency {
			warnings = append(warnings,
				"「同时出几张」填的是 "+strconv.Itoa(value)+"，超过了建议的 "+strconv.Itoa(SettingsSafeConcurrency)+"。"+
					"中转平台给每个账号同时只放 5 个请求过去，占满之后你自己在界面上点一张图也要排队等。"+
					"觉得点什么都慢的话，先把这个数改回 "+strconv.Itoa(SettingsSafeConcurrency)+"。")
		}
		in.WorkerConcurrency = &value
		changed["worker_concurrency"] = true
	}

	// ---- 管理员联系方式 ----
	if b.AdminContact != nil {
		contact := strings.TrimSpace(*b.AdminContact)
		if len([]rune(contact)) > SettingsMaxAdminContactLen {
			abortWithDesignkitError(c, settingsValidationError("admin_contact",
				"太长了，最多 "+strconv.Itoa(SettingsMaxAdminContactLen)+" 个字。写一个微信号或者手机号就够了。"))
			return in, nil, false, false
		}
		if contact == "" {
			warnings = append(warnings,
				"「管理员联系方式」是空的。运营余额不足时，界面上就只能提示他「请联系管理员」，"+
					"却说不出该找谁 —— 充值和兑换码菜单对他是隐藏的，等于死路一条。建议填上。")
		}
		in.AdminContact = &contact
		changed["admin_contact"] = true
	}

	// ---- 图片自动清理（决策 17）----
	//
	// 开关本身没有非法值；真正要拦的是保留天数。两项都**立刻生效**
	// （清理任务每一轮从数据库现读），所以不进 settingsRestartKeys。
	if b.CleanupRetentionDays != nil {
		value := *b.CleanupRetentionDays
		if value < dkdomain.MinCleanupRetentionDays || value > dkdomain.MaxCleanupRetentionDays {
			abortWithDesignkitError(c, settingsValidationError("cleanup_retention_days",
				"要填 "+strconv.Itoa(dkdomain.MinCleanupRetentionDays)+" 到 "+strconv.Itoa(dkdomain.MaxCleanupRetentionDays)+" 之间的整数。"+
					"填得太小，上个月出的图会被删掉，而且删了找不回来。"))
			return in, nil, false, false
		}
		in.CleanupRetentionDays = &value
		changed["cleanup_retention_days"] = true
	}
	if b.CleanupEnabled != nil {
		value := *b.CleanupEnabled
		in.CleanupEnabled = &value
		changed["cleanup_enabled"] = true
		if value {
			// 开启是删文件的动作，保存成功后把后果再说一遍。
			warnings = append(warnings,
				"「图片自动清理」已开启：超过保留天数的结果图和商品图会被删除，删了找不回来。"+
					"账目和消费记录保留，已扣的费用不退也不多扣。开启前先确认备份正常（配置手册第 10 步）。")
		}
	}

	// ---- 三档单价 ----
	if b.UnitPrices != nil {
		prices, ok := validateUnitPrices(c, *b.UnitPrices)
		if !ok {
			return in, nil, false, false
		}
		in.UnitPrices = prices
		changed["unit_prices"] = true
		if len(prices) < len(dkdomain.AllBillingTiers()) {
			warnings = append(warnings,
				"还有档位没填单价。没填的档，工作台上一律显示「价格待确认」，"+
					"提交前看不到这一批要花多少钱 —— 这是有意的，绝不会显示成 $0。")
		}
	}

	// ---- 价格倍率 ----
	if b.RateMultiplier != nil {
		value := strings.TrimSpace(*b.RateMultiplier)
		if value == "" {
			// 空 = 不加价不打折。存 1 而不是清空：清空之后每次报价都要在日志里
			// 抱怨一句「倍率读不出来」，排障时非常吵。
			value = "1"
		}
		amount, err := dkdomain.MoneyFromString(value)
		if err != nil {
			abortWithDesignkitError(c, settingsValidationError("rate_multiplier",
				"要填一个数字，例如 1 表示按原价、1.2 表示加价两成。留空也可以，留空按原价算。"))
			return in, nil, false, false
		}
		lo, _ := dkdomain.MoneyFromString(settingsMinRateMultiplier)
		hi, _ := dkdomain.MoneyFromString(settingsMaxRateMultiplier)
		if amount.LessThan(lo) || amount.GreaterThan(hi) {
			abortWithDesignkitError(c, settingsValidationError("rate_multiplier",
				"要填 "+settingsMinRateMultiplier+" 到 "+settingsMaxRateMultiplier+" 之间。"+
					"填 0 没有任何合理用途：真要免费就把单价留空（工作台会显示「价格待确认」）。"))
			return in, nil, false, false
		}
		in.RateMultiplier = &value
		changed["rate_multiplier"] = true
	}

	if len(changed) == 0 {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("没有要保存的内容。请先改一项设置再保存。"))
		return in, nil, false, false
	}

	for key := range changed {
		if settingsRestartKeys[key] {
			restart = true
			break
		}
	}
	return in, warnings, restart, true
}

// validateUnitPrices 校验三档单价。
//
// 规则：
//   - 档位只能是 1K / 2K / 4K，别的键直接拒绝（写错档名的话，那一档永远不会被读到，
//     而界面上看起来是填了的）；
//   - **空串 = 这一档还没测出来**，不写进配置（缺档的语义就是「价格待确认」）；
//   - 负数、0、非数字一律拒绝。0 会被运营读成免费。
func validateUnitPrices(c *gin.Context, raw map[string]string) (map[string]string, bool) {
	allowed := map[string]bool{}
	for _, tier := range dkdomain.AllBillingTiers() {
		allowed[tier] = true
	}

	maxPrice, _ := dkdomain.MoneyFromString(settingsMaxUnitPrice)
	out := make(map[string]string, len(raw))

	for tier, value := range raw {
		tier = strings.TrimSpace(tier)
		if !allowed[tier] {
			abortWithDesignkitError(c, settingsValidationError("unit_prices",
				"里出现了不认识的档位「"+tier+"」。只有 "+strings.Join(dkdomain.AllBillingTiers(), "、")+" 这三档。"))
			return nil, false
		}
		value = strings.TrimSpace(value)
		if value == "" {
			// 没测出来的档：不写。见函数注释。
			continue
		}
		amount, err := dkdomain.MoneyFromString(value)
		if err != nil {
			abortWithDesignkitError(c, settingsValidationError("unit_prices",
				"里「"+tier+"」档填的不是一个金额。请填美元数字，例如 0.042；还没测出来就留空。"))
			return nil, false
		}
		if amount.IsNegative() || amount.IsZero() {
			abortWithDesignkitError(c, settingsValidationError("unit_prices",
				"里「"+tier+"」档不能填 0 或负数。还没测出来请**留空**，留空会显示「价格待确认」；"+
					"填 0 运营会以为不要钱，一口气排几十张。"))
			return nil, false
		}
		if amount.GreaterThan(maxPrice) {
			abortWithDesignkitError(c, settingsValidationError("unit_prices",
				"里「"+tier+"」档填的是 $"+value+"，一张图不该这么贵，是不是多打了几个零？"+
					"上限是 $"+settingsMaxUnitPrice+"。"))
			return nil, false
		}
		out[tier] = value
	}
	return out, true
}
