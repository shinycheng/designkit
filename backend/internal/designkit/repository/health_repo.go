// Package repository 是 designkit 的持久化层。
//
// 只依赖 *sql.DB，**完全不碰 ent**（形态照抄上游
// backend/internal/repository/batch_image_repo.go）。
// 原因见 CLAUDE.md 第二·五节 A2：ent 的 ExecContext/QueryContext 定义在未导出的
// config 结构体上、且没有 QueryRowContext，跨包用不上；而新增 ent schema 会重写
// backend/ent/ 下 200+ 个上游生成文件。
package repository

import (
	"context"
	"database/sql"
	"fmt"

	dkservice "github.com/Wei-Shaw/sub2api/internal/designkit/service"
)

// designkitSQLExecutor 与上游 batchImageSQLExecutor 同形：
// *sql.DB 和 *sql.Tx 都满足它，后续要在事务里跑同一段 SQL 时直接换实现即可。
// 上游那份还列了 ExecContext / QueryContext，这里只声明当前真正用到的一个方法，
// 写到再加——多声明的方法会被 unused 检查盯上。
type designkitSQLExecutor interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type healthRepository struct {
	sql designkitSQLExecutor
}

// NewHealthRepository 返回 service 层定义的接口，调用方拿不到具体类型。
func NewHealthRepository(db *sql.DB) dkservice.HealthRepository {
	return &healthRepository{sql: db}
}

func (r *healthRepository) Ping(ctx context.Context) error {
	var probe int
	if err := r.sql.QueryRowContext(ctx, "SELECT 1").Scan(&probe); err != nil {
		return fmt.Errorf("designkit: 数据库探测失败: %w", err)
	}
	if probe != 1 {
		return fmt.Errorf("designkit: 数据库探测返回了意外的值 %d", probe)
	}
	return nil
}
