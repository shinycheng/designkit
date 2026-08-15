//go:build unit

package repository

// 纯逻辑单测：不连数据库，`make test-unit` 直接跑。

import (
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

func TestClampLimit(t *testing.T) {
	cases := []struct {
		name string
		in   int
		def  int
		max  int
		want int
	}{
		{name: "零用默认值", in: 0, def: 20, max: 100, want: 20},
		{name: "负数用默认值", in: -5, def: 20, max: 100, want: 20},
		{name: "正常值原样返回", in: 37, def: 20, max: 100, want: 37},
		{name: "超上限压到上限", in: 5000, def: 20, max: 100, want: 100},
		{name: "默认值本身超上限也要压住", in: 0, def: 500, max: 100, want: 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clampLimit(c.in, c.def, c.max); got != c.want {
				t.Fatalf("clampLimit(%d, %d, %d) = %d，期望 %d", c.in, c.def, c.max, got, c.want)
			}
		})
	}
}

func TestClampLimitNeverZero(t *testing.T) {
	// 返回 0 的后果是查询一条都不返，界面上表现为「任务凭空消失」。
	for _, limit := range []int{-1, 0, 1, 99999} {
		if got := clampLimit(limit, 0, 0); got < 1 {
			t.Fatalf("clampLimit(%d, 0, 0) = %d，绝不能小于 1", limit, got)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	createdAt := time.Date(2026, 8, 13, 1, 2, 3, 456789000, time.UTC)
	cursor := EncodeCursor(createdAt, 4242)
	if cursor == "" {
		t.Fatal("编码出来是空串")
	}

	gotTime, gotID, err := DecodeCursor(cursor)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if !gotTime.Equal(createdAt) {
		t.Fatalf("时间对不上: %v != %v", gotTime, createdAt)
	}
	if gotID != 4242 {
		t.Fatalf("id 对不上: %d != 4242", gotID)
	}
}

func TestCursorKeepsNanosecondPrecision(t *testing.T) {
	// created_at 是 TIMESTAMPTZ（微秒精度）。游标丢精度会让翻页漏行或重复行。
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 123456000, time.UTC)
	gotTime, _, err := DecodeCursor(EncodeCursor(createdAt, 1))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if gotTime.Nanosecond() != createdAt.Nanosecond() {
		t.Fatalf("亚秒精度丢了: %d != %d", gotTime.Nanosecond(), createdAt.Nanosecond())
	}
}

func TestEncodeCursorEmptyWhenNoNextPage(t *testing.T) {
	if got := EncodeCursor(time.Now(), 0); got != "" {
		t.Fatalf("id=0 时应该返回空串，实际 %q", got)
	}
}

func TestDecodeCursorEmptyMeansFirstPage(t *testing.T) {
	at, id, err := DecodeCursor("  ")
	if err != nil {
		t.Fatalf("空游标不该报错: %v", err)
	}
	if !at.IsZero() || id != 0 {
		t.Fatalf("空游标应该返回零值，实际 %v / %d", at, id)
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"!!!not base64!!!", "YWJj", "MjAyNi0wMS0wMXww"} {
		if _, _, err := DecodeCursor(bad); err == nil {
			t.Fatalf("%q 应该被拒绝", bad)
		}
	}
}

func TestEscapeLikePattern(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "连衣裙", want: "连衣裙"},
		{in: "100%", want: `100\%`},
		{in: "a_b", want: `a\_b`},
		{in: `a\b`, want: `a\\b`},
		{in: `%_\`, want: `\%\_\\`},
	}
	for _, c := range cases {
		if got := escapeLikePattern(c.in); got != c.want {
			t.Fatalf("escapeLikePattern(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestLikeContains(t *testing.T) {
	// 只有首尾两个 % 是通配符，用户输入里的 % 必须被转义。
	if got := likeContains("50%"); got != `%50\%%` {
		t.Fatalf("likeContains(\"50%%\") = %q", got)
	}
}

func TestAllowedSourceStatusesMatchesWhitelist(t *testing.T) {
	// SQL 里的状态清单必须由 domain.CanTransition 派生，
	// 手写第二份清单迟早会跟白名单不一致，而状态写错的代价是钱对不上账。
	for _, to := range dkdomain.AllJobStatuses() {
		sources := allowedSourceStatuses(to)
		for _, from := range dkdomain.AllJobStatuses() {
			want := dkdomain.CanTransition(from, to)
			got := containsString(sources, from.String())
			if want != got {
				t.Fatalf("%s → %s: 白名单说 %v，allowedSourceStatuses 给的是 %v", from, to, want, got)
			}
		}
	}
}

func TestAllowedSourceStatusesForSettling(t *testing.T) {
	got := allowedSourceStatuses(dkdomain.JobStatusSettling)
	if len(got) != 1 || got[0] != "running" {
		t.Fatalf("只有 running 能进 settling，实际 %v", got)
	}
}

func TestSecondsFallback(t *testing.T) {
	if got := seconds(0, 180*time.Second); got != 180 {
		t.Fatalf("零值应该用兜底值，实际 %v", got)
	}
	if got := seconds(-3*time.Second, 180*time.Second); got != 180 {
		t.Fatalf("负数应该用兜底值（否则租约立刻过期），实际 %v", got)
	}
	if got := seconds(45*time.Second, 180*time.Second); got != 45 {
		t.Fatalf("正常值应该原样返回，实际 %v", got)
	}
}
