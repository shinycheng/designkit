package repository

// 本文件是 designkit 持久化层的公共骨架：执行器接口、事务封装、错误翻译。
//
// 形态照抄上游 backend/internal/repository/batch_image_repo.go：
// **只依赖 *sql.DB，完全不碰 ent**（CLAUDE.md 第二·五节 A2）。
//
// 三条铁律，改这个包之前先看一遍：
//  1. 所有返回列表的查询**必须显式 ORDER BY**（ERP 契约靠它），
//     job_items 一律 ORDER BY seq ASC；
//  2. 计数列一律 `SET x = x + 1` 原子自增，绝不「读回来再写」；
//  3. 带租约的写回一律带 `WHERE id=? AND worker_id=? AND status='running'`，
//     影响行数不等于 1 就返回 ErrLeaseLost 并**丢弃结果**。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// dkExecutor 是 *sql.DB 和 *sql.Tx 的公共子集。
// 同一段 SQL 既要在池上跑、也要在事务里跑时，函数参数就写这个类型。
//
// 注意跟本包已有的 designkitSQLExecutor（health_repo.go，只有 QueryRowContext）
// 是两个东西，那个不要动。
type dkExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// rowScanner 让 *sql.Row 和 *sql.Rows 共用同一个 scanXxx 函数。
type rowScanner interface {
	Scan(dest ...any) error
}

// designkitRepository 实现 domain.Repository 的全部六个子接口。
type designkitRepository struct {
	db  *sql.DB
	sql dkExecutor
}

// NewRepository 建持久化层。db 来自 designkit 自己的连接池（internal/designkit/db）。
func NewRepository(db *sql.DB) dkdomain.Repository {
	return &designkitRepository{db: db, sql: db}
}

var _ dkdomain.Repository = (*designkitRepository)(nil)

// ErrNoDB 连接池没给进来。属于装配错误，不是业务错误。
var ErrNoDB = errors.New("designkit repository: 数据库连接池未初始化")

// withTx 跑一个事务：fn 返回错误就回滚，返回 nil 就提交。
//
// 事务边界一律在 repository 内部，service 不感知（port.go 的实现约束）。
func (r *designkitRepository) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if r.db == nil {
		return ErrNoDB
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		// 已经 Commit 过再 Rollback 会返回 sql.ErrTxDone，忽略即可。
		_ = tx.Rollback()
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ----------------------------------------------------------------------------
// 错误翻译
// ----------------------------------------------------------------------------
//
// repository 必须把底层错误翻译成 domain 里那几个哨兵错误，
// service 靠 errors.Is 判断，不去解析 SQL 驱动的错误串（port.go 开头的约定）。

// notFoundErr 包一个带中文说明的 ErrNotFound。
func notFoundErr(what string) error {
	return fmt.Errorf("designkit: 找不到%s: %w", what, dkdomain.ErrNotFound)
}

// conflictErr 包一个带中文说明的 ErrConflict。
// 用在「守卫没命中（影响行数 0）」和「唯一约束冲突」两种场景。
func conflictErr(what string) error {
	return fmt.Errorf("designkit: %s: %w", what, dkdomain.ErrConflict)
}

// leaseLostErr 包一个带中文说明的 ErrLeaseLost。**拿到它必须丢弃结果。**
func leaseLostErr(itemID int64, workerID string) error {
	return fmt.Errorf("designkit: item %d 的租约已不在 worker %q 手里: %w",
		itemID, workerID, dkdomain.ErrLeaseLost)
}

// illegalTransitionErr 包一个带中文说明的 ErrIllegalTransition。
func illegalTransitionErr(what string, from, to string) error {
	return fmt.Errorf("designkit: %s 不允许从 %s 迁到 %s: %w",
		what, from, to, dkdomain.ErrIllegalTransition)
}

// translate 把 database/sql 的错误翻译成 domain 哨兵错误。
// what 是给人看的中文名词，例如「任务」「提示词」。
func translate(err error, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return notFoundErr(what)
	}
	if isUniqueViolation(err) {
		return fmt.Errorf("designkit: %s 唯一约束冲突（%v）: %w", what, err, dkdomain.ErrConflict)
	}
	return err
}

// isUniqueViolation 判断是不是唯一约束冲突。
// 判定方式照抄上游 internal/repository/error_translate.go（那份是 unexported，跨包用不到）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		// 23505 = unique_violation
		return pgErr.Code == "23505"
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "unique constraint")
}

// affectedRows 取影响行数，顺手把 driver 不支持的情况变成错误而不是静默的 0。
func affectedRows(res sql.Result) (int64, error) {
	if res == nil {
		return 0, errors.New("designkit: 数据库没有返回执行结果")
	}
	return res.RowsAffected()
}

// closeRows 统一收敛 rows.Close 的错误处理（errcheck 要求不能裸调）。
func closeRows(rows *sql.Rows) {
	if rows != nil {
		_ = rows.Close()
	}
}
