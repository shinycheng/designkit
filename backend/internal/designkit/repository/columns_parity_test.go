//go:build unit

package repository

// 列清单守卫。
//
// scan.go 里每个 xxxColumns 常量的**列名和顺序**必须跟
// backend/migrations/9001_designkit_init.sql 一模一样，否则 Scan 会把
// width 扫进 height、把 estimated_cost 扫进 actual_cost —— 这类错不会报错，
// 只会让界面上的数字全是错的，而且很难查。
//
// 那份迁移已经跑过、**永不可改**，所以拿它当唯一真相去比对是安全的。
// 本机没有数据库也没有 Go 编译器，这是唯一能在提交前挡住列错位的手段。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const initMigrationRelPath = "../../../migrations/9001_designkit_init.sql"

// p1MigrationRelPath 9004 给 designkit_jobs ALTER 追加了 keep_transparency，
// 物理列序排在 9001 建表列之后。比对 jobColumns 时两份都要看。
//
// ⚠ 不是所有 ALTER 加的列都进 xxxColumns：9003 给 designkit_quota_requests
// 加的两列**刻意不在** quotaRequestColumns 里（管理端查询单独列列名，
// 见那份迁移的文件头）。所以「追加哪个文件的列」按表逐个声明，不做全量扫描。
const p1MigrationRelPath = "../../../migrations/9004_designkit_p1.sql"

func loadMigrationFile(t *testing.T, relPath string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(relPath))
	if err != nil {
		t.Fatalf("读不到迁移 SQL %s: %v", relPath, err)
	}
	return string(raw)
}

func loadInitMigration(t *testing.T) string {
	t.Helper()
	return loadMigrationFile(t, initMigrationRelPath)
}

// parseTableColumns 从建表 SQL 里按顺序抠出列名。
//
// 解析规则刻意写得很笨：一行一列、`--` 之后是注释、遇到行首的 `)` 就结束。
// 9001 那份 SQL 就是这么写的，而且它永不可改，所以不需要一个真正的 SQL 解析器。
func parseTableColumns(t *testing.T, migration, table string) []string {
	t.Helper()
	marker := "CREATE TABLE IF NOT EXISTS " + table + " ("
	idx := strings.Index(migration, marker)
	if idx < 0 {
		t.Fatalf("建表 SQL 里找不到表 %s", table)
	}

	var columns []string
	for _, line := range strings.Split(migration[idx+len(marker):], "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ")") {
			break
		}
		fields := strings.Fields(line)
		name := strings.Trim(fields[0], ",")
		switch strings.ToUpper(name) {
		case "CONSTRAINT", "PRIMARY", "UNIQUE", "FOREIGN", "CHECK":
			continue
		}
		columns = append(columns, name)
	}
	if len(columns) == 0 {
		t.Fatalf("表 %s 一列都没解析出来，解析器可能坏了", table)
	}
	return columns
}

// parseAlterAddColumns 按出现顺序抠出「ALTER TABLE <table> ADD COLUMN
// IF NOT EXISTS <name> …」加的列名。9004 就是这么写的，且跑过后永不可改，
// 所以跟 parseTableColumns 一样不需要真正的 SQL 解析器。
func parseAlterAddColumns(t *testing.T, migration, table string) []string {
	t.Helper()
	var columns []string
	rest := migration
	marker := "ALTER TABLE " + table
	for {
		idx := strings.Index(rest, marker)
		if idx < 0 {
			break
		}
		rest = rest[idx+len(marker):]
		const add = "ADD COLUMN IF NOT EXISTS "
		addIdx := strings.Index(rest, add)
		if addIdx < 0 {
			break
		}
		rest = rest[addIdx+len(add):]
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			t.Fatalf("表 %s 的 ALTER ADD COLUMN 后面没有列名，解析器可能坏了", table)
		}
		columns = append(columns, strings.Trim(fields[0], ",;"))
	}
	return columns
}

// splitColumns 把 scan.go 里的列清单常量拆成列名切片。
func splitColumns(list string) []string {
	parts := strings.Split(list, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func TestColumnListsMatchMigration(t *testing.T) {
	migration := loadInitMigration(t)
	p1 := loadMigrationFile(t, p1MigrationRelPath)

	cases := []struct {
		table   string
		columns string
		// alterFrom 非空时，把这份迁移里 ALTER 追加的列接在 9001 建表列之后
		//（物理列序就是这样）。只有 jobs 这么干，理由见 p1MigrationRelPath 注释。
		alterFrom string
	}{
		{table: "designkit_assets", columns: assetColumns},
		{table: "designkit_asset_variants", columns: assetVariantColumns},
		{table: "designkit_prompt_categories", columns: promptCategoryColumns},
		{table: "designkit_prompts", columns: promptColumns},
		{table: "designkit_jobs", columns: jobColumns, alterFrom: p1},
		{table: "designkit_job_items", columns: jobItemColumns},
		{table: "designkit_images", columns: imageColumns},
		{table: "designkit_quota_requests", columns: quotaRequestColumns},
		{table: "designkit_sync_runs", columns: syncRunColumns},
		{table: "designkit_settings", columns: settingColumns},
	}

	for _, c := range cases {
		t.Run(c.table, func(t *testing.T) {
			want := parseTableColumns(t, migration, c.table)
			if c.alterFrom != "" {
				extra := parseAlterAddColumns(t, c.alterFrom, c.table)
				if len(extra) == 0 {
					t.Fatalf("表 %s 声明了 ALTER 来源却一列都没解析出来，解析器可能坏了", c.table)
				}
				want = append(want, extra...)
			}
			got := splitColumns(c.columns)
			if len(got) != len(want) {
				t.Fatalf("列数对不上: 代码里 %d 列 %v，迁移里 %d 列 %v",
					len(got), got, len(want), want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("第 %d 列对不上: 代码里是 %q，迁移里是 %q\n代码: %v\n迁移: %v",
						i+1, got[i], want[i], got, want)
				}
			}
		})
	}
}

func TestParserFindsKnownColumns(t *testing.T) {
	// 解析器自己也要有个反向断言，否则它哪天静默返回空清单，
	// 上面那个测试会变成「两边都空、一致通过」。
	migration := loadInitMigration(t)
	jobs := parseTableColumns(t, migration, "designkit_jobs")

	for _, want := range []string{"id", "uid", "api_key_id", "estimated_cost", "user_deleted_at"} {
		found := false
		for _, c := range jobs {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("解析器没抠出 %s，解析规则可能跟迁移文件的写法脱节了", want)
		}
	}
	if jobs[0] != "id" {
		t.Fatalf("第一列应该是 id，实际 %q", jobs[0])
	}
	if jobs[len(jobs)-1] != "user_deleted_at" {
		t.Fatalf("最后一列应该是 user_deleted_at，实际 %q", jobs[len(jobs)-1])
	}
}
