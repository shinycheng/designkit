package domain

import (
	"math"

	"github.com/shopspring/decimal"
)

// ============================================================================
// 金额类型
// ============================================================================
//
// 【数据库侧：跟上游完全一致】
// 我们的金额列全是 NUMERIC(20,10)，跟上游 usage_logs 的 input_cost / output_cost /
// total_cost / actual_cost 一模一样；倍率列 NUMERIC(10,4)，也照抄上游。
// 量化刻度也跟上游一致：上游 internal/service/usage_billing.go 里
// `const UsageBillingMonetaryScale = 8`，写库前统一 Round 到 8 位小数，
// 采用 PostgreSQL NUMERIC 的 half-away-from-zero 规则。我们用同一个 8。
//
// 【Go 侧：刻意跟上游不同，这是唯一一处】
// 上游在 Go 里用 float64 存这些列（internal/service/usage_log.go 的
// UsageLog.ActualCost float64），并且专门写了 QuantizeUsageBillingAmount，
// 用 shopspring/decimal 把 float64 舍到 8 位——注释里明说，
// 不这么做「余额少扣、Key 配额多记，随请求量线性累积，无法精确对账」。
//
// 也就是说：**上游自己已经承认 float64 直接做金额运算会错，只是它每笔独立记账，
// 单笔量化一次就够了。我们不行。** designkit 要做三件上游不做的事：
//
//	① 可用额 = users.balance - SUM(designkit_holds.amount)   ← 减法
//	② actual_cost = SUM(job_items.billed_cost)（一批最多 50 张，含重试）← 连加
//	③ 每张图计费落地后按张递减 hold 金额                      ← 连减
//
// 50 次 float64 累加，误差会攒到肉眼可见，而这是钱：hold 减不到 0 就永久占着用户
// 的可用额（表现为「明明没任务却提示余额不足」），或者减成负数放出比实际更多的额度。
// 所以我们在内存里用十进制定点数，只在与上游 API 交界处转 float64。
//
// shopspring/decimal 是上游 go.mod 里的**直接依赖**（v1.4.0），不引新包。
// 它实现了 sql.Scanner / driver.Valuer，能直接扫 PostgreSQL 的 NUMERIC，不丢精度。

// Money 是 designkit 里所有金额的类型。单位固定美元（见 CurrencyUSD）。
//
// 这是类型别名不是新类型，为的是白拿 decimal 的全套方法和 Scanner/Valuer：
//
//	a.Add(b) / a.Sub(b) / a.Mul(b) / a.Cmp(b) / a.IsZero() / a.IsNegative()
//	a.GreaterThan(b) / a.LessThan(b) / a.Round(8) / a.String()
//
// 零值可用（等于 0），不需要初始化。
//
// ⚠ 扫描 NULL：Money 本身扫不了 NULL（decimal.Decimal.Scan(nil) 会报错）。
// 可空的金额列（jobs.actual_cost、job_items.billed_cost）在结构体里是 *Money，
// repository 先扫进 decimal.NullDecimal 再转指针。
//
// ⚠ JSON：decimal 默认序列化成**带引号的字符串**（"0.042"），不是数字。
// 这正是我们要的——ERP 那边用强类型语言反序列化成 decimal/BigDecimal 不会丢精度，
// 也不会出现 0.1+0.2=0.30000000000000004 这种给运营看的怪数。
// 接口文档里要写明：所有金额字段是字符串，配套的 currency 字段固定 "USD"。
type Money = decimal.Decimal

// Multiplier 是倍率类型（分组倍率、账号倍率、超支比例），对应 NUMERIC(10,4)。
// 跟 Money 分开只是为了读代码时一眼看出「这不是钱」，底层是同一个类型。
type Multiplier = decimal.Decimal

const (
	// MoneyScale 金额量化刻度：8 位小数。
	// 跟上游 service.UsageBillingMonetaryScale 严格相同，不要改成 10。
	// 列是 NUMERIC(20,10) 能存 10 位，但上游写 users.balance / usage_logs 时
	// 已经把值量化到 8 位了；我们跟着用 8，两边的数才落在同一个刻度上，能精确对账。
	MoneyScale int32 = 8

	// MultiplierScale 倍率量化刻度：4 位小数，对应 NUMERIC(10,4)。
	MultiplierScale int32 = 4

	// CurrencyUSD 是唯一的货币取值（CLAUDE.md 决策 18：不做汇率换算）。
	CurrencyUSD = "USD"
)

// ZeroMoney 是金额 0。等价于 Money{}，写出来只是为了可读。
var ZeroMoney = decimal.Zero

// MoneyFromFloat 把 float64 转成 Money，并按 MoneyScale 量化。
//
// **这个函数只用在与上游交界处**：上游的定价解析、余额查询给出来的都是 float64。
// 一旦转进 Money，后面的加减乘除全在十进制域里做，不再回 float64。
//
// 量化规则跟上游 service.QuantizeUsageBillingAmount 一致：
// decimal.NewFromFloat 取 float64 的最短十进制表示（正是 PostgreSQL 把 float8
// 参数转成 numeric 时用的表示），再 Round(8)（half-away-from-zero）。
//
// NaN / ±Inf 一律当 0——这是钱，宁可算成 0 被上层的「预估为 0」校验拦下，
// 也不能让一个 NaN 悄悄流进 SQL 参数里。
func MoneyFromFloat(f float64) Money {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return ZeroMoney
	}
	return decimal.NewFromFloat(f).Round(MoneyScale)
}

// MoneyFromString 解析 "0.042" 这种十进制字符串，不做量化（保留原始精度）。
// 用于解析接口入参和 designkit_settings 里的价目表。
func MoneyFromString(s string) (Money, error) {
	return decimal.NewFromString(s)
}

// MoneyFromInt 由整数构造金额，主要给测试和「张数 × 单价」里的张数用。
func MoneyFromInt(i int64) Money {
	return decimal.NewFromInt(i)
}

// QuantizeMoney 把金额舍入到 MoneyScale 位小数。
//
// **写库之前必须调一次。** 理由跟上游 quantizeMonetaryFields 一样：
// 让进 SQL 的参数已经落在 8 位刻度上，存储阶段不再发生任何舍入，
// 两条 UPDATE 拿到的 delta 才精确相等。
func QuantizeMoney(m Money) Money {
	return m.Round(MoneyScale)
}

// MoneyToFloat 把 Money 转回 float64。
//
// **只在调用上游 API 时用**（上游的计费入参、余额比较全是 float64）。
// 转换会丢精度，所以绝不要「转出去算一下再转回来」。
func MoneyToFloat(m Money) float64 {
	f, _ := m.Float64()
	return f
}

// MoneyString 返回金额的最短十进制表示，例如 "0.042"。
// 这是写进接口 JSON 的形状（配 currency:"USD"）。
func MoneyString(m Money) string {
	return m.String()
}

// FormatMoneyUSD 返回给人看的金额，例如 "$0.0420"。
//
// 固定 4 位小数：单张图的价格量级在 $0.01~$0.20，4 位既看得出差别又不会刷屏。
// 文案里只出现 $，**不许出现「元」**（CLAUDE.md 决策 18）。
func FormatMoneyUSD(m Money) string {
	return "$" + m.StringFixed(4)
}

// MultiplierFromFloat 把上游给的倍率转成 Multiplier，并量化到 4 位。
func MultiplierFromFloat(f float64) Multiplier {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return decimal.NewFromInt(1)
	}
	return decimal.NewFromFloat(f).Round(MultiplierScale)
}

// SumMoney 把一串金额加起来。
// 结算时算 actual_cost = SUM(job_items.billed_cost) 就走这个，
// **禁止用「成功张数 × 单价」**——同一批 10 张可能落不同计费档、单价不同
// （CLAUDE.md B3）。
func SumMoney(values []Money) Money {
	total := ZeroMoney
	for _, v := range values {
		total = total.Add(v)
	}
	return total
}

// MoneyPtr 返回 m 的指针，给可空金额列赋值时用。
func MoneyPtr(m Money) *Money {
	return &m
}

// MoneyOrZero 解引用可空金额，nil 当 0。
func MoneyOrZero(m *Money) Money {
	if m == nil {
		return ZeroMoney
	}
	return *m
}
