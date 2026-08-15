package repository

// 灵感库同步的锁与记录（designkit_sync_runs），以及 designkit 自己的配置
// （designkit_settings，**不碰上游 settings 表**）。

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// syncAdvisoryLockSQL 手动同步和自动同步**共用同一把** PostgreSQL advisory lock。
//
// 不要用应用内 mutex —— 多副本部署时应用内锁根本不起作用，
// 老系统就是各用一把锁，并发把 1.4 万条提示词写重复了。
//
// 键用 hashtext('...') 而不是魔数，跟上游 scheduler_outbox_repo.go 同一个写法，
// 字符串不同就不会撞上游的锁。
const (
	syncAdvisoryLockSQL   = `SELECT pg_try_advisory_lock(hashtext('designkit_prompt_sync'))`
	syncAdvisoryUnlockSQL = `SELECT pg_advisory_unlock(hashtext('designkit_prompt_sync'))`
)

// syncUnlockTimeout 释放锁时给的超时。进程要退出了也得把锁还掉。
const syncUnlockTimeout = 2 * time.Second

// TryLockSync 抢同步锁。抢不到返回 (false, nil, nil)，调用方记一条 status='skipped' 的 SyncRun。
//
// ⚠ advisory lock 是**会话级**的：这里从池里单独取一条连接（*sql.Conn）并一直握着，
// 用完在**同一条连接上** pg_advisory_unlock 再归还。
// 直接用 *sql.DB 跑这两条 SQL 会落在不同连接上，锁会跟着连接回池而失效 ——
// 表现就是「锁了个寂寞」，两个同步照样并发跑。
func (r *designkitRepository) TryLockSync(ctx context.Context) (bool, func(), error) {
	if r.db == nil {
		return false, nil, ErrNoDB
	}
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return false, nil, err
	}

	var locked bool
	if err := conn.QueryRowContext(ctx, syncAdvisoryLockSQL).Scan(&locked); err != nil {
		_ = conn.Close()
		return false, nil, err
	}
	if !locked {
		_ = conn.Close()
		return false, nil, nil
	}

	var once bool
	unlock := func() {
		if once || conn == nil {
			return
		}
		once = true
		// 用独立的 context：调用方那个 ctx 很可能已经被取消了（比如进程要退出），
		// 拿它去解锁会直接失败，锁就得等连接断开才释放。
		releaseCtx, cancel := context.WithTimeout(context.Background(), syncUnlockTimeout)
		defer cancel()
		if _, err := conn.ExecContext(releaseCtx, syncAdvisoryUnlockSQL); err != nil {
			slog.Warn("designkit 释放灵感库同步锁失败", slog.String("error", err.Error()))
		}
		_ = conn.Close()
	}
	return true, unlock, nil
}

func (r *designkitRepository) StartSyncRun(ctx context.Context, kind dkdomain.SyncKind) (*dkdomain.SyncRun, error) {
	if kind == "" {
		kind = dkdomain.SyncKindAuto
	}
	run, err := scanSyncRun(r.sql.QueryRowContext(ctx, `
INSERT INTO designkit_sync_runs (kind, status) VALUES ($1, $2)
RETURNING `+syncRunColumns, string(kind), string(dkdomain.SyncStatusRunning)))
	if err != nil {
		return nil, translate(err, "同步记录")
	}
	return run, nil
}

func (r *designkitRepository) FinishSyncRun(ctx context.Context, run *dkdomain.SyncRun) error {
	if run == nil || run.ID <= 0 {
		return notFoundErr("同步记录")
	}
	finishedAt := run.FinishedAt
	if finishedAt == nil {
		now := time.Now()
		finishedAt = &now
	}
	status := run.Status
	if status == "" {
		status = dkdomain.SyncStatusSucceeded
	}

	res, err := r.sql.ExecContext(ctx, `
UPDATE designkit_sync_runs SET
  status = $2, finished_at = $3,
  fetched = $4, inserted = $5, updated = $6, skipped = $7,
  error = $8
WHERE id = $1`,
		run.ID, string(status), *finishedAt,
		run.Fetched, run.Inserted, run.Updated, run.Skipped,
		nullableStringPtr(run.Error))
	if err != nil {
		return err
	}
	affected, err := affectedRows(res)
	if err != nil {
		return err
	}
	if affected == 0 {
		return notFoundErr("同步记录")
	}
	run.Status = status
	run.FinishedAt = finishedAt
	return nil
}

// latestSyncRunSQL 取「最近一次同步」，**跳过 skipped**。
//
// ⚠ 为什么必须排除 skipped（这是实际会发生的一类事故）：
// 抢不到锁时我们会记一条 status='skipped'，而它的 started_at 比正在跑的那条更晚。
// 不排除的话，运营连点两次「立即同步」之后：
//
//	第 2 次返回 409 → 面板拿到那条 skipped → running=false → 前端停止轮询、
//	定格在「已经有一次在跑，这次跳过了」；
//	真正那一轮 15 秒后成功了，但它的 started_at 更早，**永远不会再成为「最近一次」**，
//	进度、条数、成功提示全都看不到。
//
// 连带伤害：启动时的「补同步」判断看到 latest 不是 succeeded 就会重跑一轮全量，
//
//	于是每次重启都多拉一次 1.4 万条。
//
// 「12 小时定时同步撞上手动同步」和多副本部署都会踩到同一条。
//
// skipped 记录本身保留（排查「为什么我点了没反应」时有用），只是不参与「最近一次」。
const latestSyncRunSQL = `SELECT ` + syncRunColumns +
	` FROM designkit_sync_runs WHERE status <> 'skipped'` +
	` ORDER BY started_at DESC, id DESC LIMIT 1`

func (r *designkitRepository) LatestSyncRun(ctx context.Context) (*dkdomain.SyncRun, error) {
	run, err := scanSyncRun(r.sql.QueryRowContext(ctx, latestSyncRunSQL))
	if err != nil {
		return nil, translate(err, "同步记录")
	}
	return run, nil
}

// ---- 配置 ----

func (r *designkitRepository) GetSetting(ctx context.Context, key string) (*dkdomain.Setting, error) {
	s, err := scanSetting(r.sql.QueryRowContext(ctx,
		`SELECT `+settingColumns+` FROM designkit_settings WHERE key = $1`, key))
	if err != nil {
		return nil, translate(err, "配置项")
	}
	return s, nil
}

const listSettingsSQL = `SELECT ` + settingColumns + ` FROM designkit_settings ORDER BY key ASC`

func (r *designkitRepository) ListSettings(ctx context.Context) ([]*dkdomain.Setting, error) {
	rows, err := r.sql.QueryContext(ctx, listSettingsSQL)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	return scanSettings(rows)
}

// PutSetting 写一个配置项。value 必须是合法 JSON（列是 JSONB）——
// 标量也用 JSON 写：数字直接 4，字符串要带引号 "abc"。
// 这里先在 Go 侧校验一次，不然 PostgreSQL 会抛一个运营看不懂的 22P02。
func (r *designkitRepository) PutSetting(ctx context.Context, key string, value []byte) error {
	if !json.Valid(value) {
		return dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessagef("配置项 %s 的值不是合法的 JSON。", key)
	}
	_, err := r.sql.ExecContext(ctx, `
INSERT INTO designkit_settings (key, value, updated_at) VALUES ($1, $2, NOW())
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`,
		key, string(value))
	if err != nil {
		return translate(err, "配置项")
	}
	return nil
}
