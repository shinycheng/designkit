package wordcheck

import (
	"strings"
	"unicode/utf8"
)

// Platform 一个电商平台的标题规则。
type Platform struct {
	// Key 稳定标识，接口参数用它（对外契约，定了别改）。
	Key string
	// Name 显示名（品牌名，中英文界面都用它）。
	Name string
	// TitleMaxRunes 标题字数上限，按「字（rune）」计。
	TitleMaxRunes int
}

// platforms 平台标题规则表。顺序就是前端下拉框的顺序。
//
// ⚠ 规则会变——平台随时调整标题上限，改这里就行，全系统只有这一处
// （前端 api/contentcheck.ts 里有一份镜像给「实时字数条」用，改这里要同步改它）。
//
// 上限按「字（rune）」计，取的是各平台文档口径的**汉字数**：
//   - 淘宝 / 天猫：60 字符（1 汉字 = 2 字符）→ 30 字；
//   - 京东：商品名称 45 字（90 字符）；
//   - 拼多多：60 字符 → 30 字（2026-08 查证的常用值）；
//   - 抖音（抖店）：60 字符 → 30 字（2026-08 查证的常用值）;
//   - Amazon：多数类目 200 字符（英文按字母计，rune 数 = 字符数）。
//
// 纯中文标题下「字数」和「字符 ÷ 2」两种算法一致；标题夹带英文数字时
// 这里会偏严（英文字母也按 1 个字计）——宁严勿松，超没超以平台后台为准。
var platforms = []Platform{
	{Key: "taobao", Name: "淘宝", TitleMaxRunes: 30},
	{Key: "tmall", Name: "天猫", TitleMaxRunes: 30},
	{Key: "jd", Name: "京东", TitleMaxRunes: 45},
	{Key: "pdd", Name: "拼多多", TitleMaxRunes: 30},
	{Key: "douyin", Name: "抖音", TitleMaxRunes: 30},
	{Key: "amazon", Name: "Amazon", TitleMaxRunes: 200},
}

// Platforms 全部平台，按下拉框顺序。返回副本，调用方改不坏规则表。
func Platforms() []Platform {
	out := make([]Platform, len(platforms))
	copy(out, platforms)
	return out
}

// PlatformByKey 按标识取平台。大小写不敏感，找不到时 ok=false。
func PlatformByKey(key string) (Platform, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, p := range platforms {
		if p.Key == key {
			return p, true
		}
	}
	return Platform{}, false
}

// PlatformKeys 全部平台标识，拼错误提示用（「可选：taobao/tmall/…」）。
func PlatformKeys() []string {
	keys := make([]string, len(platforms))
	for i, p := range platforms {
		keys[i] = p.Key
	}
	return keys
}

// TitleLen 标题字数：按字（rune）计，首尾空白不算。
// 「按 rune」是对外契约（第 1 个字就是第一个字），别改成字节或 UTF-16。
func TitleLen(text string) int {
	return utf8.RuneCountInString(strings.TrimSpace(text))
}
