//go:build unit

package worker

import (
	"strings"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/require"
)

// TestNewULID_IsAcceptedByDomain 我们生成的 uid 必须能过 domain 自己的校验。
//
// 过不了的后果不是编译错，是运行时：图存进去了，uid 却是个非法值，
// 对外接口拿它去查会一条也查不到。
func TestNewULID_IsAcceptedByDomain(t *testing.T) {
	for i := 0; i < 200; i++ {
		uid := NewULID()
		require.Len(t, uid, 26)
		require.True(t, dkdomain.IsValidULID(uid), "domain.IsValidULID 必须认这个值：%q", uid)
	}
}

// TestNewULID_Unique 同一毫秒内生成一批也不能重复 ——
// designkit_images.uid 上是 UNIQUE 索引，撞一次就是一张图入不了库。
func TestNewULID_Unique(t *testing.T) {
	const n = 5000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		uid := NewULID()
		_, dup := seen[uid]
		require.False(t, dup, "uid 重复了：%q", uid)
		seen[uid] = struct{}{}
	}
}

// TestNewULID_TimeOrdered 时间戳在前，所以按字典序排就是按创建时间排。
// 排障时「这批图是先出的还是后出的」一眼就能看出来。
func TestNewULID_TimeOrdered(t *testing.T) {
	base := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	earlier := newULIDAt(base)
	later := newULIDAt(base.Add(time.Second))
	require.Less(t, earlier, later)
}

// TestEncodeCrockford_KnownVector 编码规则本身：全 0 → 26 个 '0'，
// 全 1 → 第一个字符只有 3 bit 可用（最大 7 = 'Z' 之前的 '7'），其余全是 'Z'。
func TestEncodeCrockford_KnownVector(t *testing.T) {
	var zero [16]byte
	require.Equal(t, strings.Repeat("0", 26), encodeCrockford(zero))

	var ones [16]byte
	for i := range ones {
		ones[i] = 0xFF
	}
	got := encodeCrockford(ones)
	require.Len(t, got, 26)
	require.Equal(t, byte('7'), got[0], "最高位那个字符只有 3 bit，最大是 7")
	require.Equal(t, strings.Repeat("Z", 25), got[1:])
}
