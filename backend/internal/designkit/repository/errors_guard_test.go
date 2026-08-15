//go:build unit

package repository

// 错误翻译 + 「绝对不能出现在 SQL 里的东西」的守卫。

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/lib/pq"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// allStaticSQL 收集本包里成型的 SQL，供下面几条「不许出现」的断言扫描。
func allStaticSQL() []string {
	jobsSQL, _ := buildListJobsQuery(dkdomain.ListJobsQuery{UserID: 1})
	assetsSQL, _ := buildListAssetsQuery(dkdomain.ListAssetsQuery{UserID: 1})
	promptsSQL, _ := buildListPromptsQuery(dkdomain.ListPromptsQuery{})
	countSQL, _ := buildCountPromptsQuery(dkdomain.ListPromptsQuery{})

	return []string{
		jobsSQL, assetsSQL, promptsSQL, countSQL,
		listJobItemsSQL,
		listJobsPendingSettlementSQL,
		listCategoriesSQL,
		listSettingsSQL,
		latestSyncRunSQL,
		upsertSyncedPromptSQL,
		buildClaimItemSQL(true),
		buildClaimItemSQL(false),
		buildInsertJobItemsSQL(3),
		buildListImagesByItemSQL(true),
		buildListImagesByItemSQL(false),
		buildListQuotaRequestsSQL(true),
		buildListQuotaRequestsSQL(false),
	}
}

func TestSyncLockUsesSameKeyForManualAndAuto(t *testing.T) {
	// 手动同步和自动同步**共用同一把锁**。两把锁的后果是并发把 1.4 万条提示词写重复
	// （老系统就是这么坏的）。加锁和解锁必须是同一个键。
	const key = "hashtext('designkit_prompt_sync')"
	if !strings.Contains(syncAdvisoryLockSQL, key) || !strings.Contains(syncAdvisoryUnlockSQL, key) {
		t.Fatalf("加锁 %q 和解锁 %q 的键不一致", syncAdvisoryLockSQL, syncAdvisoryUnlockSQL)
	}
	if !strings.Contains(syncAdvisoryLockSQL, "pg_try_advisory_lock") {
		t.Fatal("必须用 try 版本，抢不到就跳过，不能阻塞等待")
	}
}

func TestNoForbiddenIdempotencyConflictTarget(t *testing.T) {
	// designkit_jobs 上**没有** (user_id, idempotency_key) 唯一索引
	// （9001 迁移刻意只建了普通部分索引，理由见那份 SQL 的注释）。
	// 写 ON CONFLICT (user_id, idempotency_key) 会在运行时报 42P10。
	for _, s := range allStaticSQL() {
		if strings.Contains(s, "ON CONFLICT (user_id, idempotency_key)") {
			t.Fatalf("这条 SQL 用了不存在的唯一索引做冲突目标:\n%s", s)
		}
	}
}

func TestEverySelectListHasOrderBy(t *testing.T) {
	// 所有返回列表的查询必须显式 ORDER BY —— ERP 契约靠它。
	for _, s := range allStaticSQL() {
		if !strings.HasPrefix(strings.TrimSpace(s), "SELECT") {
			continue
		}
		if strings.Contains(s, "COUNT(*)") {
			continue
		}
		if !strings.Contains(s, "ORDER BY") {
			t.Fatalf("这条查询没有显式 ORDER BY:\n%s", s)
		}
	}
}

func TestIdempotencyLookupIsTimeBounded(t *testing.T) {
	// 幂等复用必须带时间窗，跟上游协调器的 24 小时对齐。
	// 不带窗口就等于把被否掉的唯一索引又装了回来：24 小时后 ERP 复用同一个 key
	// 下新单，会被错认成老单，**直接不出图**。
	if idempotencyLookupWindow != "24 hours" {
		t.Fatalf("幂等窗口被改成了 %q，改之前先跟上游协调器的窗口对齐", idempotencyLookupWindow)
	}
}

func TestNotFoundErrIsSentinel(t *testing.T) {
	err := notFoundErr("任务")
	if !errors.Is(err, dkdomain.ErrNotFound) {
		t.Fatal("service 层靠 errors.Is(err, ErrNotFound) 判断，不能断链")
	}
	if !strings.Contains(err.Error(), "任务") {
		t.Fatalf("错误信息里应该带上是什么找不到: %v", err)
	}
}

func TestConflictAndLeaseErrorsAreSentinels(t *testing.T) {
	if !errors.Is(conflictErr("状态已被改过"), dkdomain.ErrConflict) {
		t.Fatal("conflictErr 必须能被 errors.Is(ErrConflict) 认出来")
	}
	if !errors.Is(leaseLostErr(7, "worker-a"), dkdomain.ErrLeaseLost) {
		t.Fatal("leaseLostErr 必须能被 errors.Is(ErrLeaseLost) 认出来")
	}
	if !errors.Is(illegalTransitionErr("任务", "running", "cancelled"), dkdomain.ErrIllegalTransition) {
		t.Fatal("illegalTransitionErr 必须能被 errors.Is(ErrIllegalTransition) 认出来")
	}
}

func TestTranslateMapsNoRowsAndUniqueViolation(t *testing.T) {
	if err := translate(nil, "任务"); err != nil {
		t.Fatalf("nil 应该原样返回 nil: %v", err)
	}
	if err := translate(sql.ErrNoRows, "任务"); !errors.Is(err, dkdomain.ErrNotFound) {
		t.Fatalf("ErrNoRows 应该翻译成 ErrNotFound: %v", err)
	}

	dup := &pq.Error{Code: "23505", Message: "duplicate key value violates unique constraint"}
	if err := translate(dup, "任务"); !errors.Is(err, dkdomain.ErrConflict) {
		t.Fatalf("23505 应该翻译成 ErrConflict: %v", err)
	}

	other := errors.New("connection refused")
	if err := translate(other, "任务"); !errors.Is(err, other) {
		t.Fatalf("其它错误必须原样往上抛，不能吞: %v", err)
	}
}

func TestIsUniqueViolation(t *testing.T) {
	if isUniqueViolation(nil) {
		t.Fatal("nil 不是唯一约束冲突")
	}
	if !isUniqueViolation(&pq.Error{Code: "23505"}) {
		t.Fatal("23505 就是唯一约束冲突")
	}
	if isUniqueViolation(&pq.Error{Code: "23503"}) {
		t.Fatal("23503 是外键冲突，不是唯一约束冲突")
	}
	if !isUniqueViolation(errors.New("pq: duplicate key value violates unique constraint")) {
		t.Fatal("字符串兜底也要认出来")
	}
}

func TestNewInsufficientBalance(t *testing.T) {
	required := dkdomain.MoneyFromFloat(1.5)
	available := dkdomain.MoneyFromFloat(0.25)

	err := newInsufficientBalance(required, available)
	if !errors.Is(err, dkdomain.ErrInsufficientBalance) {
		t.Fatal("必须能被 errors.Is(ErrInsufficientBalance) 认出来")
	}

	var ib *dkdomain.InsufficientBalanceError
	if !errors.As(err, &ib) {
		t.Fatal("界面要显示「还差多少」，必须能 errors.As 出结构体")
	}
	if !ib.Shortfall.Equal(dkdomain.MoneyFromFloat(1.25)) {
		t.Fatalf("差额算错了: %s", dkdomain.MoneyString(ib.Shortfall))
	}
}

func TestNewInsufficientBalanceNeverNegativeShortfall(t *testing.T) {
	// 差额是给运营看的数字，负数会显示成「还差 -3 美元」。
	err := newInsufficientBalance(dkdomain.MoneyFromFloat(1), dkdomain.MoneyFromFloat(4))
	var ib *dkdomain.InsufficientBalanceError
	if !errors.As(err, &ib) {
		t.Fatal("取不出结构体")
	}
	if ib.Shortfall.IsNegative() {
		t.Fatalf("差额不能是负数: %s", dkdomain.MoneyString(ib.Shortfall))
	}
}

func TestPromptSourceRefConflictTargetMatchesMigration(t *testing.T) {
	migration := loadInitMigration(t)
	// 迁移里的部分唯一索引谓词
	if !strings.Contains(migration, "WHERE source_ref IS NOT NULL AND source_ref <> ''") {
		t.Fatal("迁移里的部分唯一索引谓词变了？这不可能——那个文件永不可改")
	}
	if !strings.Contains(promptSourceRefConflictTarget,
		"WHERE source_ref IS NOT NULL AND source_ref <> ''") {
		t.Fatalf("ON CONFLICT 的谓词跟索引对不上: %s", promptSourceRefConflictTarget)
	}
}
