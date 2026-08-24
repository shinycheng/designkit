//go:build unit

package repository

// 高清放大任务表（9004）的守卫。跟本包其他测试一样：只测拼出来的 SQL，
// 不连数据库。parseTableColumns / splitColumns 复用 columns_parity_test.go 的，
// mustContain 复用 query_build_test.go 的，不要再定义一遍。

import (
	"testing"
)

// ---- 列清单与迁移逐列比对 ----

func TestUpscaleColumnListMatchesMigration(t *testing.T) {
	migration := loadMigrationFile(t, p1MigrationRelPath)

	want := parseTableColumns(t, migration, "designkit_upscale_tasks")
	got := splitColumns(upscaleTaskColumns)
	if len(got) != len(want) {
		t.Fatalf("列数对不上: 代码里 %d 列 %v，迁移里 %d 列 %v",
			len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 列对不上: 代码里是 %q，迁移里是 %q", i+1, got[i], want[i])
		}
	}
}

// ---- SQL 语义守卫 ----

// LatestByAsset 是去重和状态轮询的判据，三件事一件都不能丢：
// 归属过滤在 SQL 里（不泄露存在性）、显式 ORDER BY（「最新一行」靠它）、LIMIT 1。
func TestUpscaleLatestByAssetQueryShape(t *testing.T) {
	mustContain(t, latestUpscaleTaskSQL,
		"user_id = $1",
		"asset_uid = $2",
		"ORDER BY created_at DESC, uid DESC",
		"LIMIT 1",
	)
}

// 恢复扫描按入队顺序，显式 ORDER BY（本包铁律）。
func TestUpscaleListQueuedQueryShape(t *testing.T) {
	mustContain(t, listQueuedUpscaleSQL,
		"status = 'queued'",
		"ORDER BY created_at ASC, uid ASC",
	)
}

// 状态流转必须带守卫：MarkRunning 只认 queued、MarkDone/MarkFailed 只认 running。
// 少一个守卫 = 「同一个任务被处理两遍」没有止损点。
func TestUpscaleTransitionsAreGuarded(t *testing.T) {
	mustContain(t, markUpscaleRunningSQL, "status = 'queued'")
	mustContain(t, markUpscaleDoneSQL, "status = 'running'")
	mustContain(t, markUpscaleFailedSQL, "status = 'running'")
	// 恢复只把 running 拉回 queued，绝不能碰 done/failed（那是历史）。
	mustContain(t, requeueInterruptedUpscaleSQL, "WHERE status = 'running'")
}
