//go:build unit

package repository

// 图片自动清理 SQL 的单测（形态照 query_build_test.go：
// 能不连数据库就验的东西都在这里验掉）。
//
// 这几条守的都是「删错东西」级别的事故：
//   - 被未结束批次引用的素材绝不能删（worker 随时要按 object_key 读原图）；
//   - 软删必须带 deleted_at IS NULL 守卫（deleted_at 就是「清过了」的标记）；
//   - 一次最多 LIMIT 条（不锁表）。

import (
	"reflect"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// TestCleanupBusyJobStatusesAreExactlyTheNonTerminalOnes
// 「未结束」必须恰好等于 domain 状态表里的非终态。手抄字符串的话，
// 上游哪天加一个新状态，这里就会静默漏判，素材被删、批次读空。
func TestCleanupBusyJobStatusesAreExactlyTheNonTerminalOnes(t *testing.T) {
	want := []string{}
	for _, s := range dkdomain.AllJobStatuses() {
		if !s.IsTerminal() {
			want = append(want, s.String())
		}
	}
	got := cleanupBusyJobStatuses()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanupBusyJobStatuses() = %v, 应该是 %v", got, want)
	}
	// 四个非终态一个都不能少（漏了哪个，处于那个状态的批次引用的素材就会被删）。
	if len(got) != 4 {
		t.Fatalf("非终态应该是 4 个（created/holding/running/settling），实际 %v", got)
	}
}

// TestListExpiredAssetsSQLSkipsAssetsReferencedByBusyJobs 被引用素材跳过。
func TestListExpiredAssetsSQLSkipsAssetsReferencedByBusyJobs(t *testing.T) {
	mustContain(t, listExpiredAssetsSQL,
		// 只挑还没清过的
		"a.deleted_at IS NULL",
		// 判据：created_at + 保留天数（cutoff 由 service 算好传进来）
		"a.created_at < $1",
		// 被未结束批次引用的整个排除
		"NOT EXISTS",
		"ji.asset_id = a.id",
		"j.status = ANY($2)",
		// 分批 + 顺序稳定
		"ORDER BY a.id ASC",
		"LIMIT $3",
	)
}

// TestListExpiredImagesSQLHonorsExpiresAtAndSkipsBusyJobs
// 结果图：created_at 过期或 expires_at 显式过期都算；未结束批次的图不动。
func TestListExpiredImagesSQLHonorsExpiresAtAndSkipsBusyJobs(t *testing.T) {
	mustContain(t, listExpiredImagesSQL,
		"i.deleted_at IS NULL",
		"i.created_at < $1",
		// 9001 预留的 expires_at：显式设过就认（部分索引也在这一列上）
		"i.expires_at IS NOT NULL AND i.expires_at < NOW()",
		"NOT EXISTS",
		"j.id = i.job_id AND j.status = ANY($2)",
		"ORDER BY i.id ASC",
		"LIMIT $3",
	)
}

// TestCleanupSoftDeleteSQLIsGuardedAndSoft 软删是 UPDATE 不是 DELETE，
// 且带 deleted_at IS NULL 守卫（重复处理时影响行数为 0，不会把时间戳越写越新）。
func TestCleanupSoftDeleteSQLIsGuardedAndSoft(t *testing.T) {
	for _, sqlText := range []string{softDeleteImagesSQL, softDeleteAssetsSQL} {
		mustContain(t, sqlText,
			"SET deleted_at = NOW()",
			"id = ANY($1)",
			"deleted_at IS NULL",
		)
		mustNotContain(t, sqlText, "DELETE FROM")
	}
}

// TestListVariantKeysSQLHasStableOrder 产物路径查询按 (asset_id, id) 稳定排序。
func TestListVariantKeysSQLHasStableOrder(t *testing.T) {
	mustContain(t, listVariantKeysSQL,
		"asset_id = ANY($1)",
		"ORDER BY asset_id ASC, id ASC",
	)
}
