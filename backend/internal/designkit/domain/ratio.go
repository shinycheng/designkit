package domain

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ============================================================================
// 出图比例
// ============================================================================
//
// 控制出图比例的**唯一手段**是「给输入图补白边到目标比例」（不裁产品），
// 网关会跟着输入图的比例出图。所以 Ratio 会一路传到 Python 预处理服务
// （POST /v1/preprocess 的 ratio 表单字段，形如 "3:4"）。
//
// ⚠ 比例白名单**在 Go 侧校验**，读 designkit_settings.ratios。
// Python 侧的 parse_aspect 接受任意 W:H（"7:13" 也照收），
// 它里面的 SUPPORTED_RATIOS 常量只用于错误提示文案，**不是护栏**。

// Ratio 是出图比例，形如 "3:4"。底层是 string，
// 所以能直接扫进/写出 VARCHAR(16) 列，也能直接序列化成 JSON 字符串。
type Ratio string

// 五个出图比例（CLAUDE.md B3-1，monica 拍板）。
// designkit_settings.ratios 的预置值就是这五个，顺序也是界面显示顺序。
// 以后加比例是改配置，不用改代码 —— 因为我们不受官方三种尺寸限制。
const (
	Ratio1x1  Ratio = "1:1" // 主图
	Ratio3x4  Ratio = "3:4" // 详情竖版
	Ratio4x3  Ratio = "4:3"
	Ratio16x9 Ratio = "16:9" // banner
	Ratio9x16 Ratio = "9:16"
)

// DefaultRatios 是 designkit_settings.ratios 的预置值，顺序即界面显示顺序。
// **运行时一律以数据库里的值为准**，这里只作为读不到配置时的兜底和文档。
func DefaultRatios() []Ratio {
	return []Ratio{Ratio1x1, Ratio3x4, Ratio4x3, Ratio16x9, Ratio9x16}
}

// ErrInvalidRatio 比例串解析失败。
var ErrInvalidRatio = errors.New("designkit: invalid ratio")

// ErrRatioNotAllowed 比例合法但不在白名单里。
var ErrRatioNotAllowed = errors.New("designkit: ratio not allowed")

// String 让 Ratio 能直接拼进日志、SQL 参数和 multipart 表单字段。
func (r Ratio) String() string { return string(r) }

// Parse 把 "3:4" 解析成 (3, 4)。
//
// 只接受 "W:H" 这一种写法，W、H 都必须是正整数。
// **不接受 "1536x1024" 这种像素写法** —— 老 Python 代码的 parse_ratio 只认像素写法，
// 传 "3:4" 进去会静默不补边，运营选 3:4 拿回原图比例、钱照扣。
// 两边格式必须一致，所以这里也只认一种。
func (r Ratio) Parse() (width int, height int, err error) {
	return ParseRatioParts(string(r))
}

// ParseRatioParts 解析比例串。独立成函数是为了给接口入参校验用。
func ParseRatioParts(s string) (width int, height int, err error) {
	trimmed := strings.TrimSpace(s)
	parts := strings.Split(trimmed, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%w: 期望 \"宽:高\" 形式，实际 %q", ErrInvalidRatio, s)
	}
	width, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || width <= 0 {
		return 0, 0, fmt.Errorf("%w: 宽必须是正整数，实际 %q", ErrInvalidRatio, s)
	}
	height, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || height <= 0 {
		return 0, 0, fmt.Errorf("%w: 高必须是正整数，实际 %q", ErrInvalidRatio, s)
	}
	return width, height, nil
}

// ParseRatio 校验并归一化一个比例串（去空格），返回 Ratio。
func ParseRatio(s string) (Ratio, error) {
	w, h, err := ParseRatioParts(s)
	if err != nil {
		return "", err
	}
	return Ratio(fmt.Sprintf("%d:%d", w, h)), nil
}

// IsWellFormed 判断格式对不对（不判断在不在白名单里）。
func (r Ratio) IsWellFormed() bool {
	_, _, err := r.Parse()
	return err == nil
}

// IsAllowed 判断比例在不在白名单里。allowed 一律来自
// designkit_settings.ratios，**不要写死在代码里**。
func (r Ratio) IsAllowed(allowed []Ratio) bool {
	for _, a := range allowed {
		if a == r {
			return true
		}
	}
	return false
}

// ValidateRatio 一次做完两件事：格式对不对、在不在白名单里。
// 提交任务、预处理素材、报价三个入口都该调它。
func ValidateRatio(r Ratio, allowed []Ratio) error {
	if _, _, err := r.Parse(); err != nil {
		return err
	}
	if !r.IsAllowed(allowed) {
		return fmt.Errorf("%w: %q 不在允许的比例列表里", ErrRatioNotAllowed, r)
	}
	return nil
}

// TargetSize 算出「补白边到这个比例、最长边不超过 maxDimension」之后的目标像素。
//
// maxDimension 来自 designkit_settings.max_dimension（默认 2048），
// **每次调 Python 预处理都必须显式带上它**。它直接决定出图落 2K 还是 4K 计费档，
// 不能埋在常量里 —— 管理员把它从 2048 改成 4096 而环境变量没跟着改，
// 就会「界面说 4K、实际按 2K 出图」，而且不报错。
//
// 返回值只用于**预估和展示**，不是计费依据（理由见 ClassifyBillingTier 的注释）。
func (r Ratio) TargetSize(maxDimension int) (width int, height int, err error) {
	w, h, err := r.Parse()
	if err != nil {
		return 0, 0, err
	}
	if maxDimension <= 0 {
		return 0, 0, fmt.Errorf("%w: max_dimension 必须为正，实际 %d", ErrInvalidRatio, maxDimension)
	}
	if w >= h {
		width = maxDimension
		height = maxDimension * h / w
	} else {
		height = maxDimension
		width = maxDimension * w / h
	}
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	return width, height, nil
}

// ============================================================================
// 计费档
// ============================================================================

const (
	// BillingTier1K 最长边 <= 1024。
	BillingTier1K = "1K"
	// BillingTier2K 最长边 <= 2048。
	BillingTier2K = "2K"
	// BillingTier4K 最长边 > 2048。
	BillingTier4K = "4K"
)

// AllBillingTiers 返回全部计费档，从便宜到贵。
func AllBillingTiers() []string {
	return []string{BillingTier1K, BillingTier2K, BillingTier4K}
}

// ClassifyBillingTier 按像素分计费档：取最长边，<=1024 → 1K，<=2048 → 2K，更大 → 4K。
//
// 宽或高不是正数时返回空串（分不出档），跟上游 ClassifyImageBillingTier
// 返回 (\"\", false) 的行为一致。要兜底成 2K 用 ClassifyBillingTierOrDefault。
//
// **规则必须跟上游 internal/service/image_billing_size.go 的
// ClassifyImageBillingTier 完全一致**（那个函数吃的是 "1024x1536" 这种字符串，
// 内部同样取最长边分档）。ratio_upstream_parity_test.go 会拿上游函数逐点比对，
// 上游哪天改了规则，那个测试会先炸。
//
// ⚠ **这个函数算出来的档不是计费依据，只用于预估和展示。**
// 真正计费用的是上游按「实际输出像素」算出来的档：
// ResolveImageBillingSize 先看输出图 —— 把所有输出分档取**最高**那一档；
// 一张都分不出档时才回落到请求里写的 size，再不行才兜底 2K。
// 实测记录过：同一张图请求 1:1，网关返回的是 1254×1254，最长边 1254，
// 落的是 **2K 档不是 1K 档**。网关按自己的规格出图，我们请求什么尺寸它不一定听。
// 所以「比例护栏 ≠ 价目表」，两者之间隔着一个我们控制不了的变量，
// 预估档和实际档**可能不一致**，界面上必须说明这是预估。
func ClassifyBillingTier(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	maxEdge := width
	if height > maxEdge {
		maxEdge = height
	}
	switch {
	case maxEdge <= 1024:
		return BillingTier1K
	case maxEdge <= 2048:
		return BillingTier2K
	default:
		return BillingTier4K
	}
}

// ClassifyBillingTierOrDefault 分不出档时兜底 2K。
// 跟上游 NormalizeImageBillingTierOrDefault 的兜底值一致。
func ClassifyBillingTierOrDefault(width, height int) string {
	if tier := ClassifyBillingTier(width, height); tier != "" {
		return tier
	}
	return BillingTier2K
}

// HighestBillingTier 在一组档里取最贵的那个（未知值忽略）。
// 上游对一次返回多张图的批次就是这么定档的（取最高档），我们对账时按同一口径。
func HighestBillingTier(tiers []string) string {
	best := ""
	for _, t := range tiers {
		if billingTierRank(t) > billingTierRank(best) {
			best = t
		}
	}
	return best
}

func billingTierRank(tier string) int {
	switch tier {
	case BillingTier1K:
		return 1
	case BillingTier2K:
		return 2
	case BillingTier4K:
		return 3
	default:
		return 0
	}
}
