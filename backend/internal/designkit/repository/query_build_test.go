//go:build unit

package repository

// SQL 拼接的单测。本机没有 Go 编译器也没有 Docker，
// 所有能不连数据库就验的东西都必须在这里验掉。

import (
	"strings"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

func mustContain(t *testing.T, sqlText string, fragments ...string) {
	t.Helper()
	for _, f := range fragments {
		if !strings.Contains(sqlText, f) {
			t.Fatalf("SQL 里少了 %q:\n%s", f, sqlText)
		}
	}
}

func mustNotContain(t *testing.T, sqlText string, fragments ...string) {
	t.Helper()
	for _, f := range fragments {
		if strings.Contains(sqlText, f) {
			t.Fatalf("SQL 里不该出现 %q:\n%s", f, sqlText)
		}
	}
}

// ---- 领取待办 ----

func TestBuildClaimItemSQLKeepsTheThreeGuarantees(t *testing.T) {
	sqlText := buildClaimItemSQL(true)

	mustContain(t, sqlText,
		// 多个 worker 抢同一批时互相跳过，不打架
		"FOR UPDATE SKIP LOCKED",
		"LIMIT 1",
		// attempt_count 在**领取时**就 +1：否则进程崩一次这张图会被永远重试
		"attempt_count = attempt_count + 1",
		// 僵尸回收跟正常领取写在同一个 WHERE 里
		"status = 'pending' AND available_at <=",
		"status = 'running' AND lease_expires_at <",
		// 先来的批次先跑，批内按序号跑
		"ORDER BY job_id, seq",
		"status = 'running'",
	)
}

func TestBuildClaimItemSQLReturnsAllItemColumns(t *testing.T) {
	sqlText := buildClaimItemSQL(true)
	if !strings.Contains(sqlText, "RETURNING "+jobItemColumns) {
		t.Fatalf("领取之后必须原样返回整行，否则 worker 拿不到 attempt_count:\n%s", sqlText)
	}
}

func TestBuildClaimItemSQLInjectableClock(t *testing.T) {
	withDBNow := buildClaimItemSQL(true)
	mustContain(t, withDBNow, "NOW()")
	mustNotContain(t, withDBNow, "$3")

	withParamNow := buildClaimItemSQL(false)
	mustContain(t, withParamNow, "$3::timestamptz")
	mustNotContain(t, withParamNow, "NOW()")
}

// ---- 写回结果的守卫 ----

func TestCompleteItemGuard(t *testing.T) {
	// 少了任何一个条件，租约被抢走之后的迟到结果都会覆盖掉新 worker 的结果。
	mustContain(t, completeItemGuardSuffix,
		"WHERE id = $1",
		"worker_id = $2",
		"status = 'running'",
	)
}

// ---- 建 items ----

func TestBuildInsertJobItemsSQL(t *testing.T) {
	got := buildInsertJobItemsSQL(2)
	want := "INSERT INTO designkit_job_items " +
		"(job_id, seq, asset_id, variant_id, prompt_id, prompt_text, max_attempts) VALUES " +
		"($1, $2, $3, $4, $5, $6, $7), ($1, $8, $9, $10, $11, $12, $13)"
	if got != want {
		t.Fatalf("多行 VALUES 拼错了:\n得到 %s\n期望 %s", got, want)
	}
}

func TestBuildInsertJobItemsSQLParamCount(t *testing.T) {
	// 单次批量上限 50，参数数 = 1 + 6×50 = 301，离 PostgreSQL 的 65535 很远。
	got := buildInsertJobItemsSQL(dkdomain.DefaultMaxBatchItems)
	if n := strings.Count(got, "("); n != dkdomain.DefaultMaxBatchItems+1 {
		t.Fatalf("VALUES 组数不对: %d", n)
	}
	if !strings.HasSuffix(got, "$301)") {
		t.Fatalf("最后一个占位符应该是 $301:\n%s", got)
	}
}

// ---- 批次列表 ----

func TestBuildListJobsQueryDefault(t *testing.T) {
	sqlText, args := buildListJobsQuery(dkdomain.ListJobsQuery{UserID: 7})

	want := "SELECT " + jobColumns + " FROM designkit_jobs WHERE user_id = $1" +
		" AND user_deleted_at IS NULL ORDER BY created_at DESC, id DESC LIMIT $2"
	if sqlText != want {
		t.Fatalf("默认列表 SQL 不对:\n得到 %s\n期望 %s", sqlText, want)
	}
	if len(args) != 2 {
		t.Fatalf("参数个数不对: %v", args)
	}
	// 多取一条用来判断有没有下一页
	if args[1] != DefaultPageLimit+1 {
		t.Fatalf("应该多取一条: %v", args[1])
	}
}

func TestBuildListJobsQueryWithCursorAndStatuses(t *testing.T) {
	at := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	sqlText, args := buildListJobsQuery(dkdomain.ListJobsQuery{
		UserID:          7,
		Statuses:        []dkdomain.JobStatus{dkdomain.JobStatusRunning, dkdomain.JobStatusSettling},
		CursorCreatedAt: at,
		CursorID:        99,
		Limit:           5,
		IncludeDeleted:  true,
	})

	mustContain(t, sqlText,
		"status = ANY($2)",
		// (created_at, id) 行比较：只按时间翻页会在同一微秒建的批次上漏行
		"AND (created_at, id) < ($3, $4)",
		"ORDER BY created_at DESC, id DESC LIMIT $5",
	)
	mustNotContain(t, sqlText, "user_deleted_at IS NULL", "OFFSET")
	if len(args) != 5 || args[4] != 6 {
		t.Fatalf("参数不对: %v", args)
	}
}

func TestListQueriesNeverUseOffset(t *testing.T) {
	// offset 在持续写入时会漏数据（设计定型 3.3），ERP 拉历史一律游标分页。
	jobsSQL, _ := buildListJobsQuery(dkdomain.ListJobsQuery{UserID: 1})
	assetsSQL, _ := buildListAssetsQuery(dkdomain.ListAssetsQuery{UserID: 1})
	promptsSQL, _ := buildListPromptsQuery(dkdomain.ListPromptsQuery{})
	for _, s := range []string{jobsSQL, assetsSQL, promptsSQL} {
		mustNotContain(t, s, "OFFSET")
	}
}

// ---- 明细与结果图 ----

func TestListJobItemsAlwaysOrdersBySeq(t *testing.T) {
	// 「按序号取第几张图」是发给 ERP 的对外契约。这条断言别删。
	mustContain(t, listJobItemsSQL, "ORDER BY seq ASC")
}

func TestBuildListImagesByItemSQL(t *testing.T) {
	all := buildListImagesByItemSQL(false)
	mustContain(t, all, "WHERE item_id = $1 AND deleted_at IS NULL", "ORDER BY image_index ASC")
	mustNotContain(t, all, "AND is_current")

	current := buildListImagesByItemSQL(true)
	mustContain(t, current, "AND is_current", "ORDER BY image_index ASC")
}

// ---- 素材 ----

func TestBuildListAssetsQuery(t *testing.T) {
	sqlText, args := buildListAssetsQuery(dkdomain.ListAssetsQuery{UserID: 3, Limit: 10})
	mustContain(t, sqlText,
		"WHERE user_id = $1 AND deleted_at IS NULL",
		"ORDER BY created_at DESC, id DESC LIMIT $2",
	)
	if len(args) != 2 || args[1] != 10 {
		t.Fatalf("参数不对: %v", args)
	}

	withCursor, args := buildListAssetsQuery(dkdomain.ListAssetsQuery{
		UserID:          3,
		CursorCreatedAt: time.Now(),
		CursorID:        8,
	})
	mustContain(t, withCursor, "AND (created_at, id) < ($2, $3)", "LIMIT $4")
	if len(args) != 4 {
		t.Fatalf("参数不对: %v", args)
	}
}

// ---- 灵感库 ----

func TestBuildListPromptsQueryUsesILIKE(t *testing.T) {
	categoryID := int64(4)
	ownerID := int64(9)
	sqlText, args := buildListPromptsQuery(dkdomain.ListPromptsQuery{
		CategoryID:  &categoryID,
		Keyword:     "连衣裙",
		OwnerUserID: &ownerID,
		OnlyEnabled: true,
		CursorID:    100,
		Limit:       30,
	})

	mustContain(t, sqlText,
		"WHERE deleted_at IS NULL",
		"AND category_id = $1",
		// PostgreSQL 的 LIKE 区分大小写，搜索必须用 ILIKE
		"AND (title ILIKE $2 OR body ILIKE $2)",
		"AND owner_user_id = $3",
		"AND is_enabled",
		"AND id > $4",
		"ORDER BY id ASC LIMIT $5",
	)
	mustNotContain(t, sqlText, " LIKE $")
	if len(args) != 5 {
		t.Fatalf("参数不对: %v", args)
	}
	if args[1] != "%连衣裙%" {
		t.Fatalf("关键词没拼成包含匹配: %v", args[1])
	}
}

func TestBuildCountPromptsQueryIgnoresCursor(t *testing.T) {
	// 总数不能受游标影响，否则界面上的「共 N 条」会随翻页越翻越小。
	sqlText, args := buildCountPromptsQuery(dkdomain.ListPromptsQuery{CursorID: 100, Limit: 10})
	mustContain(t, sqlText, "SELECT COUNT(*) FROM designkit_prompts", "WHERE deleted_at IS NULL")
	mustNotContain(t, sqlText, "id > $", "LIMIT")
	if len(args) != 0 {
		t.Fatalf("不该有参数: %v", args)
	}
}

func TestUpsertSyncedPromptCarriesPartialIndexPredicate(t *testing.T) {
	// source_ref 上是**部分**唯一索引，ON CONFLICT 不带同样的 WHERE 谓词
	// PostgreSQL 推断不出索引，运行时直接报错。
	mustContain(t, upsertSyncedPromptSQL,
		`ON CONFLICT (source_ref) WHERE source_ref IS NOT NULL AND source_ref <> ''`,
		"xmax = 0",
		"IS DISTINCT FROM",
	)
	// is_enabled 不能被同步覆盖：管理员手动下架过的词，下次同步不能自己又打开。
	mustNotContain(t, upsertSyncedPromptSQL, "is_enabled = EXCLUDED.is_enabled")
}

func TestImageUpsertCarriesPartialIndexPredicate(t *testing.T) {
	// 唯一键里必须带 image_index：网关一次返回两张图时，
	// 少了这一列第二张会插不进去 —— 钱花了、图丢了。
	if imageCurrentConflictTarget !=
		`ON CONFLICT (item_id, image_index) WHERE is_current AND deleted_at IS NULL` {
		t.Fatalf("结果图的冲突目标被改了: %s", imageCurrentConflictTarget)
	}
}

// ---- 状态来源白名单 ----

func TestFinalizeSourceStatuses(t *testing.T) {
	got := finalizeSourceStatuses()
	for _, want := range []string{"running", "created", "holding"} {
		if !containsString(got, want) {
			t.Fatalf("终态判定应该放行 %s，实际 %v", want, got)
		}
	}
	// 已经结算过或已经是终态的批次不能再被判一次终态。
	for _, forbidden := range []string{"succeeded", "failed", "cancelled", "partially_failed", "settling"} {
		if containsString(got, forbidden) {
			t.Fatalf("终态判定不该放行 %s，实际 %v", forbidden, got)
		}
	}
}

func TestSettleSourceStatuses(t *testing.T) {
	got := settleSourceStatuses(dkdomain.JobStatusSucceeded)
	if !containsString(got, "settling") {
		t.Fatalf("结算必须能从 settling 落终态，实际 %v", got)
	}
	if !containsString(got, "succeeded") {
		t.Fatalf("结算 worker 重跑时不能卡死，实际 %v", got)
	}
}

func TestRetryCountAdjustSQL(t *testing.T) {
	cases := map[dkdomain.ItemStatus]string{
		dkdomain.ItemStatusSucceeded: "success_count = GREATEST(success_count - 1, 0)",
		dkdomain.ItemStatusFailed:    "fail_count = GREATEST(fail_count - 1, 0)",
		dkdomain.ItemStatusCancelled: "cancelled_count = GREATEST(cancelled_count - 1, 0)",
		// 还没落终态的那两个状态本来就没被计数，不能减
		dkdomain.ItemStatusPending: "",
		dkdomain.ItemStatusRunning: "",
	}
	for from, want := range cases {
		if got := retryCountAdjustSQL(from); got != want {
			t.Fatalf("retryCountAdjustSQL(%s) = %q，期望 %q", from, got, want)
		}
	}
}

// ---- 额度申请 ----

func TestBuildListQuotaRequestsSQL(t *testing.T) {
	all := buildListQuotaRequestsSQL(false)
	mustContain(t, all, "ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2")
	mustNotContain(t, all, "WHERE")

	filtered := buildListQuotaRequestsSQL(true)
	mustContain(t, filtered, "WHERE status = $3", "ORDER BY created_at DESC, id DESC")
}

// ---- 其它 ----

func TestPrefixColumns(t *testing.T) {
	if got := prefixColumns("i", "id, uid, job_id"); got != "i.id, i.uid, i.job_id" {
		t.Fatalf("prefixColumns 拼错了: %s", got)
	}
}

func TestTrimmedPtr(t *testing.T) {
	if got := trimmedPtr(nil); got != "" {
		t.Fatalf("nil 应该返回空串，实际 %q", got)
	}
	s := "  abc  "
	if got := trimmedPtr(&s); got != "abc" {
		t.Fatalf("应该去掉首尾空白，实际 %q", got)
	}
}

func TestNullableStringPtrTreatsBlankAsNull(t *testing.T) {
	// 空串和 NULL 混着存会让「WHERE idempotency_key IS NOT NULL」这类条件失准。
	blank := "   "
	if got := nullableStringPtr(&blank); got != nil {
		t.Fatalf("空白串应该写 NULL，实际 %v", got)
	}
	if got := nullableString(""); got != nil {
		t.Fatalf("空串应该写 NULL，实际 %v", got)
	}
}
