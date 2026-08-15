//go:build unit

package domain

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// testJobUID 是一个合法的 26 位 ULID（Crockford Base32，最长的那种：全 Z）。
const testJobUID = "01J8ZK7Q9X2M4N6P8R0T2V4W6Y"

// maxJobUID 用字符集里最"宽"的字符占满 26 位。长度跟内容无关，
// 但写出来是为了强调：ULID 恒定 26 位，没有变长的情况。
const maxJobUID = "ZZZZZZZZZZZZZZZZZZZZZZZZZZ"

func TestBillingRequestID_Format(t *testing.T) {
	got := BillingRequestID(testJobUID, 3, 1)
	require.Equal(t, "dki:01J8ZK7Q9X2M4N6P8R0T2V4W6Y:0003:01", got)

	stored := StoredBillingRequestID(testJobUID, 3, 1)
	require.Equal(t, "client:dki:01J8ZK7Q9X2M4N6P8R0T2V4W6Y:0003:01", stored)
	require.Equal(t, UpstreamClientRequestIDPrefix+got, stored,
		"落库值必须正好是 client: + 发给上游的值，否则拿它反查 usage_logs 一条也查不到")
}

func TestBillingRequestID_FixedLength(t *testing.T) {
	require.Len(t, BillingRequestID(testJobUID, 1, 1), BillingRequestIDLen)
	require.Len(t, StoredBillingRequestID(testJobUID, 1, 1), StoredBillingRequestIDLen)
}

// TestStoredBillingRequestID_FitsUsageLogColumn 这是**本包最重要的一条断言**。
//
// usage_logs.request_id 是 VARCHAR(64)。超长的后果不是截断，是 PostgreSQL 抛 22001
// 让整条 INSERT 失败 —— 而扣费在写账单**之前**已经成功、且是独立事务。
// 结果就是：钱扣了、账单没有，兜底重插会因为同样的原因再失败一次，
// 最后只在日志里留一行，**没有告警、没有重试、没有补偿队列**。
// 遍历 seq / attempt 的边界值，一个都不许超。
func TestStoredBillingRequestID_FitsUsageLogColumn(t *testing.T) {
	seqs := []int{1, 2, 9, 10, 49, 50, 99, 100, 999, 1000, BillingRequestIDMaxSeq}
	attempts := []int{1, 2, 3, 9, 10, BillingRequestIDMaxAttempt}

	for _, uid := range []string{testJobUID, maxJobUID} {
		for _, seq := range seqs {
			for _, attempt := range attempts {
				stored := StoredBillingRequestID(uid, seq, attempt)
				require.LessOrEqualf(t, len(stored), UsageLogRequestIDMaxLen,
					"seq=%d attempt=%d 生成的 %q 长度 %d 超过 usage_logs.request_id 的 VARCHAR(64)",
					seq, attempt, stored, len(stored))
			}
		}
	}
}

// TestStoredBillingRequestID_FitsEvenWhenOutOfRange 越界参数也不许把列撑爆。
// 越界本身由 ValidateBillingRequestIDParts 拦，但万一漏拦了，
// 后果必须是「id 难看」而不是「钱扣了账单没写」。
func TestStoredBillingRequestID_FitsEvenWhenOutOfRange(t *testing.T) {
	for _, c := range []struct{ seq, attempt int }{
		{1000000, 999},
		{99999999, 99999},
	} {
		stored := StoredBillingRequestID(maxJobUID, c.seq, c.attempt)
		require.LessOrEqualf(t, len(stored), UsageLogRequestIDMaxLen,
			"越界参数 seq=%d attempt=%d 生成的 %q 也不许超过 64", c.seq, c.attempt, stored)
	}
}

// TestBillingRequestID_StableForSameSeqAndAttempt 同一 (seq, attempt) 无论算几次
// 都是同一个 id —— 幂等。网络抖动导致我们不确定上游到底扣没扣时，
// 重投同一个 id 是安全的（上游的 usage_billing_dedup 会认出来，applied=false）。
func TestBillingRequestID_StableForSameSeqAndAttempt(t *testing.T) {
	first := BillingRequestID(testJobUID, 7, 2)
	for i := 0; i < 5; i++ {
		require.Equal(t, first, BillingRequestID(testJobUID, 7, 2),
			"同一 (seq, attempt) 第 %d 次生成的 id 必须一样", i+1)
	}
}

// TestBillingRequestID_ChangesWithAttempt attempt + 1 必须换新 id。
//
// 沿用旧 id 的后果：图重出了、成本在上游产生了，上游把它判成重复请求，
// 我们**一分钱不扣**。重试一张 = 重新出一张图 = 必须重新收一次钱。
func TestBillingRequestID_ChangesWithAttempt(t *testing.T) {
	a1 := BillingRequestID(testJobUID, 7, 1)
	a2 := BillingRequestID(testJobUID, 7, 2)
	a3 := BillingRequestID(testJobUID, 7, 3)
	require.NotEqual(t, a1, a2)
	require.NotEqual(t, a2, a3)
	require.NotEqual(t, a1, a3)
}

// TestBillingRequestID_UniquePerItem 一个批次里每一张图的 id 必须互不相同。
// 这正是「一单 3 张扣 3 份钱」的前提：id 相同 → 幂等表唯一键
// (request_id, api_key_id) 命中 → 第 2 张起静默不扣。
func TestBillingRequestID_UniquePerItem(t *testing.T) {
	seen := map[string]string{}
	for seq := 1; seq <= 50; seq++ {
		for attempt := 1; attempt <= 3; attempt++ {
			id := BillingRequestID(testJobUID, seq, attempt)
			if prev, dup := seen[id]; dup {
				t.Fatalf("id 撞车：%q 同时属于 %s 和 seq=%d attempt=%d", id, prev, seq, attempt)
			}
			seen[id] = fmt.Sprintf("seq=%d attempt=%d", seq, attempt)
		}
	}
	require.Len(t, seen, 150)
}

// TestBillingRequestID_DifferentJobsNeverCollide 两个批次之间也不许撞。
func TestBillingRequestID_DifferentJobsNeverCollide(t *testing.T) {
	require.NotEqual(t,
		BillingRequestID(testJobUID, 1, 1),
		BillingRequestID(maxJobUID, 1, 1))
}

func TestValidateBillingRequestIDParts(t *testing.T) {
	require.NoError(t, ValidateBillingRequestIDParts(testJobUID, 1, 1))
	require.NoError(t, ValidateBillingRequestIDParts(testJobUID, BillingRequestIDMaxSeq, BillingRequestIDMaxAttempt))

	bad := []struct {
		name    string
		uid     string
		seq     int
		attempt int
	}{
		{"uid 是空的", "", 1, 1},
		{"uid 短了一位", testJobUID[:25], 1, 1},
		{"uid 长了一位", testJobUID + "Z", 1, 1},
		{"uid 含 ULID 字符集之外的 I", "01J8ZK7Q9X2M4N6P8R0T2V4W6I", 1, 1},
		{"uid 是小写", "01j8zk7q9x2m4n6p8r0t2v4w6y", 1, 1},
		{"seq 从 0 开始（对外文档说的第 1 张就是 seq=1）", testJobUID, 0, 1},
		{"seq 是负数", testJobUID, -1, 1},
		{"seq 越界", testJobUID, BillingRequestIDMaxSeq + 1, 1},
		{"attempt 从 0 开始（attempt_count 在领取那刻就 +1，第一次就是 1）", testJobUID, 1, 0},
		{"attempt 越界", testJobUID, 1, BillingRequestIDMaxAttempt + 1},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateBillingRequestIDParts(c.uid, c.seq, c.attempt)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrInvalidBillingRequestID)
		})
	}
}

func TestParseBillingRequestID(t *testing.T) {
	// 带 client: 前缀（从 designkit_job_items.billing_request_id 读出来的形状）
	uid, seq, attempt, err := ParseBillingRequestID(StoredBillingRequestID(testJobUID, 42, 3))
	require.NoError(t, err)
	require.Equal(t, testJobUID, uid)
	require.Equal(t, 42, seq)
	require.Equal(t, 3, attempt)

	// 不带前缀（我们刚生成、还没发出去的形状）
	uid, seq, attempt, err = ParseBillingRequestID(BillingRequestID(testJobUID, 1, 1))
	require.NoError(t, err)
	require.Equal(t, testJobUID, uid)
	require.Equal(t, 1, seq)
	require.Equal(t, 1, attempt)
}

func TestParseBillingRequestID_RoundTrip(t *testing.T) {
	for _, seq := range []int{1, 7, 50, 9999} {
		for _, attempt := range []int{1, 3, 99} {
			uid, gotSeq, gotAttempt, err := ParseBillingRequestID(StoredBillingRequestID(testJobUID, seq, attempt))
			require.NoError(t, err)
			require.Equal(t, testJobUID, uid)
			require.Equal(t, seq, gotSeq)
			require.Equal(t, attempt, gotAttempt)
		}
	}
}

func TestParseBillingRequestID_Rejects(t *testing.T) {
	bad := []struct {
		id  string
		why string
	}{
		{"", "空串"},
		{"dki", "只有前缀"},
		{"dki:01J8ZK7Q9X2M4N6P8R0T2V4W6Y:0003", "段数不够"},
		{"dki:01J8ZK7Q9X2M4N6P8R0T2V4W6Y:0003:01:extra", "段数多了"},
		{"dkh:01J8ZK7Q9X2M4N6P8R0T2V4W6Y:0003:01", "前缀不对"},
		{"dki:not-a-ulid:0003:01", "uid 段不是 ULID"},
		{"dki:01J8ZK7Q9X2M4N6P8R0T2V4W6Y:abcd:01", "seq 不是数字"},
		{"dki:01J8ZK7Q9X2M4N6P8R0T2V4W6Y:0000:01", "seq 从 1 开始"},
		{"dki:01J8ZK7Q9X2M4N6P8R0T2V4W6Y:0003:00", "attempt 从 1 开始"},
		{"req_abc123", "上游 RequestLogger 注入的那种 id"},
		{"client:req_abc123", "同上，带 client: 前缀"},
	}
	for _, c := range bad {
		t.Run(c.why, func(t *testing.T) {
			_, _, _, err := ParseBillingRequestID(c.id)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrInvalidBillingRequestID)
		})
	}
}

func TestIsValidULID(t *testing.T) {
	require.True(t, IsValidULID(testJobUID))
	require.True(t, IsValidULID(maxJobUID))
	require.True(t, IsValidULID("00000000000000000000000000"))

	require.False(t, IsValidULID(""))
	require.False(t, IsValidULID(testJobUID[:25]), "25 位")
	require.False(t, IsValidULID(testJobUID+"0"), "27 位")
	require.False(t, IsValidULID("01J8ZK7Q9X2M4N6P8R0T2V4W6I"), "I 不在 Crockford Base32 里")
	require.False(t, IsValidULID("01J8ZK7Q9X2M4N6P8R0T2V4W6L"), "L 不在 Crockford Base32 里")
	require.False(t, IsValidULID("01J8ZK7Q9X2M4N6P8R0T2V4W6O"), "O 不在 Crockford Base32 里")
	require.False(t, IsValidULID("01J8ZK7Q9X2M4N6P8R0T2V4W6U"), "U 不在 Crockford Base32 里")
	require.False(t, IsValidULID("01j8zk7q9x2m4n6p8r0t2v4w6y"), "小写")
	require.False(t, IsValidULID("01J8ZK7Q9X2M4N6P8R0T2V4W6-"), "连字符")
}
