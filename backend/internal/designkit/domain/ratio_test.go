//go:build unit

package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRatio_Parse(t *testing.T) {
	cases := []struct {
		in   Ratio
		w, h int
	}{
		{Ratio1x1, 1, 1},
		{Ratio3x4, 3, 4},
		{Ratio4x3, 4, 3},
		{Ratio16x9, 16, 9},
		{Ratio9x16, 9, 16},
		{" 3 : 4 ", 3, 4}, // 两侧空白要能容忍
	}
	for _, c := range cases {
		t.Run(c.in.String(), func(t *testing.T) {
			w, h, err := c.in.Parse()
			require.NoError(t, err)
			require.Equal(t, c.w, w)
			require.Equal(t, c.h, h)
		})
	}
}

func TestRatio_Parse_Rejects(t *testing.T) {
	bad := []struct {
		in  Ratio
		why string
	}{
		{"", "空串"},
		{"1", "缺冒号"},
		{"1:2:3", "段数多了"},
		{"0:1", "宽是 0"},
		{"1:0", "高是 0"},
		{"-3:4", "负数"},
		{"a:b", "不是数字"},
		{"1536x1024", "像素写法。老 Python 的 parse_ratio 只认这种，传 3:4 会静默不补边——两边必须只认一种格式"},
		{"3/4", "斜杠写法"},
	}
	for _, c := range bad {
		t.Run(c.why, func(t *testing.T) {
			_, _, err := c.in.Parse()
			require.Error(t, err)
			require.ErrorIs(t, err, ErrInvalidRatio)
		})
	}
}

func TestParseRatio_Normalizes(t *testing.T) {
	r, err := ParseRatio("  16 : 9 ")
	require.NoError(t, err)
	require.Equal(t, Ratio16x9, r)
}

func TestValidateRatio(t *testing.T) {
	allowed := DefaultRatios()

	require.NoError(t, ValidateRatio(Ratio3x4, allowed))

	err := ValidateRatio("7:13", allowed)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRatioNotAllowed,
		"格式合法但不在白名单里 —— Python 的 parse_aspect 会照收 7:13，护栏只能在 Go 侧")

	err = ValidateRatio("abc", allowed)
	require.ErrorIs(t, err, ErrInvalidRatio)
}

func TestRatio_TargetSize(t *testing.T) {
	cases := []struct {
		r            Ratio
		maxDim       int
		wantW, wantH int
	}{
		{Ratio1x1, 2048, 2048, 2048},
		{Ratio3x4, 2048, 1536, 2048},
		{Ratio4x3, 2048, 2048, 1536},
		{Ratio16x9, 2048, 2048, 1152},
		{Ratio9x16, 2048, 1152, 2048},
		{Ratio1x1, 1024, 1024, 1024},
	}
	for _, c := range cases {
		t.Run(c.r.String(), func(t *testing.T) {
			w, h, err := c.r.TargetSize(c.maxDim)
			require.NoError(t, err)
			require.Equal(t, c.wantW, w)
			require.Equal(t, c.wantH, h)
		})
	}

	_, _, err := Ratio3x4.TargetSize(0)
	require.Error(t, err, "max_dimension 必须显式传且为正")
}

// TestClassifyBillingTier 把关键像素钉死。
// 1254x1254 是实测记录：请求 1:1，网关返回 1254×1254，落 **2K 不是 1K**。
func TestClassifyBillingTier(t *testing.T) {
	cases := []struct {
		w, h int
		want string
		note string
	}{
		{1024, 1024, BillingTier1K, "边界：正好 1024 还是 1K"},
		{1024, 1536, BillingTier2K, "竖版 3:4，最长边 1536"},
		{1536, 1024, BillingTier2K, "横版 4:3，最长边 1536"},
		{1254, 1254, BillingTier2K, "实测：请求 1:1 网关却返回 1254×1254，超过 1024 就落 2K"},
		{3000, 2000, BillingTier4K, "最长边 3000 > 2048"},
		{1025, 100, BillingTier2K, "边界：1024 再加 1 就跳档"},
		{2048, 2048, BillingTier2K, "边界：正好 2048 还是 2K"},
		{2049, 1, BillingTier4K, "边界：2048 再加 1 就跳 4K"},
		{2048, 1152, BillingTier2K, "16:9 @2048"},
		{3840, 2160, BillingTier4K, "真 4K"},
	}
	for _, c := range cases {
		t.Run(c.note, func(t *testing.T) {
			require.Equal(t, c.want, ClassifyBillingTier(c.w, c.h))
		})
	}
}

func TestClassifyBillingTier_InvalidDimensions(t *testing.T) {
	require.Equal(t, "", ClassifyBillingTier(0, 0))
	require.Equal(t, "", ClassifyBillingTier(-1, 100))
	require.Equal(t, "", ClassifyBillingTier(100, 0))

	// 兜底跟上游 NormalizeImageBillingTierOrDefault 一致：分不出档就算 2K
	require.Equal(t, BillingTier2K, ClassifyBillingTierOrDefault(0, 0))
	require.Equal(t, BillingTier1K, ClassifyBillingTierOrDefault(512, 512))
}

// TestClassifyBillingTier_MatchesDefaultRatioTargets 五个比例在默认
// max_dimension=2048 下全部落 2K —— 这条断言的价值是提醒：
// 管理员把 max_dimension 调到 2049 以上，五个比例会**集体跳到 4K 档**，
// 出图成本按档位翻。改这个设置前必须先看价目表。
func TestClassifyBillingTier_MatchesDefaultRatioTargets(t *testing.T) {
	for _, r := range DefaultRatios() {
		w, h, err := r.TargetSize(2048)
		require.NoError(t, err)
		require.Equalf(t, BillingTier2K, ClassifyBillingTier(w, h),
			"%s 在 max_dimension=2048 下应落 2K（%dx%d）", r, w, h)
	}
	for _, r := range DefaultRatios() {
		w, h, err := r.TargetSize(1024)
		require.NoError(t, err)
		require.Equalf(t, BillingTier1K, ClassifyBillingTier(w, h),
			"%s 在 max_dimension=1024 下应落 1K（%dx%d）", r, w, h)
	}
}

func TestHighestBillingTier(t *testing.T) {
	require.Equal(t, BillingTier4K, HighestBillingTier([]string{BillingTier1K, BillingTier4K, BillingTier2K}))
	require.Equal(t, BillingTier2K, HighestBillingTier([]string{BillingTier1K, BillingTier2K}))
	require.Equal(t, BillingTier1K, HighestBillingTier([]string{BillingTier1K, "", "unknown"}))
	require.Equal(t, "", HighestBillingTier(nil))
}
