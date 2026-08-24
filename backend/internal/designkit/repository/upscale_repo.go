package repository

// 高清放大任务（designkit_upscale_tasks，9004 迁移）的读写。
//
// 【一次重试一行】失败后重新放大 = 插一行新任务，旧的 failed 行留作历史。
// 所以「这张图现在的放大状态」永远是最新的一行（LatestByAsset），
// (user_id, asset_uid) 上是普通索引不是唯一索引。
//
// 【状态流转全部带守卫】MarkRunning 只认 queued、MarkDone/MarkFailed 只认
// running，影响行数 0 一律 ErrConflict——重启恢复和用户新入队之间有一条
// 很窄的竞态（详见 service/upscale.go 的恢复注释），守卫在这里把
// 「同一个任务被处理两遍」压成「后到的一方空转返回」。
//
// 本包铁律的落点：归属过滤在 SQL 里做（LatestByAsset 带 user_id），
// 查别人的图和查没排过的图**同样**返回 ErrNotFound；
// 列表查询显式 ORDER BY（恢复顺序 = 入队顺序）。

import (
	"context"
	"database/sql"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// upscaleTaskColumns 列名和顺序必须跟 backend/migrations/9004_designkit_p1.sql
// 的建表一模一样（upscale_repo_test.go 解析那份迁移逐列比对）。
const upscaleTaskColumns = `uid, asset_uid, user_id, origin, status, ` +
	`result_asset_uid, error_code, error_message, created_at, updated_at`

const insertUpscaleTaskSQL = `
INSERT INTO designkit_upscale_tasks (uid, asset_uid, user_id, origin, status)
VALUES ($1, $2, $3, $4, 'queued')
RETURNING ` + upscaleTaskColumns

// latestUpscaleTaskSQL 「这张图最新的一行」。
// uid 是 ULID（按时间字典序递增），DESC 并列裁决同一毫秒的两行。
const latestUpscaleTaskSQL = `SELECT ` + upscaleTaskColumns + `
FROM designkit_upscale_tasks
WHERE user_id = $1 AND asset_uid = $2
ORDER BY created_at DESC, uid DESC
LIMIT 1`

const getUpscaleTaskSQL = `SELECT ` + upscaleTaskColumns + `
FROM designkit_upscale_tasks
WHERE uid = $1`

const markUpscaleRunningSQL = `
UPDATE designkit_upscale_tasks
SET status = 'running', updated_at = NOW()
WHERE uid = $1 AND status = 'queued'`

const markUpscaleDoneSQL = `
UPDATE designkit_upscale_tasks
SET status = 'done', result_asset_uid = $2, error_code = '', error_message = '', updated_at = NOW()
WHERE uid = $1 AND status = 'running'`

const markUpscaleFailedSQL = `
UPDATE designkit_upscale_tasks
SET status = 'failed', error_code = $2, error_message = $3, updated_at = NOW()
WHERE uid = $1 AND status = 'running'`

// requeueInterruptedUpscaleSQL 重启恢复第一步：running 的重置回 queued——
// 上次进程死在半路，那张图没放完也没判失败。
const requeueInterruptedUpscaleSQL = `
UPDATE designkit_upscale_tasks
SET status = 'queued', updated_at = NOW()
WHERE status = 'running'`

// listQueuedUpscaleSQL 恢复第二步：把还排着队的按入队顺序捞出来重新入队。
const listQueuedUpscaleSQL = `SELECT ` + upscaleTaskColumns + `
FROM designkit_upscale_tasks
WHERE status = 'queued'
ORDER BY created_at ASC, uid ASC
LIMIT $1`

// UpscaleRepo 高清放大任务的持久化层。跟 ChatRepo 一样只依赖 *sql.DB，
// 不碰 ent（CLAUDE.md 第二·五节 A2）。
type UpscaleRepo struct {
	sql dkExecutor
}

// NewUpscaleRepo 建持久化层。db 来自 designkit 自己的连接池（internal/designkit/db）。
func NewUpscaleRepo(db *sql.DB) *UpscaleRepo {
	return &UpscaleRepo{sql: db}
}

// Insert 插一行新任务（status=queued），返回带时间戳的完整行。
// uid 由调用方生成（26 位 ULID）。
func (r *UpscaleRepo) Insert(ctx context.Context, rec *dkdomain.UpscaleTaskRecord) (*dkdomain.UpscaleTaskRecord, error) {
	if rec == nil || strings.TrimSpace(rec.UID) == "" {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("放大任务缺少编号（uid），无法创建。")
	}
	created, err := scanUpscaleTask(r.sql.QueryRowContext(ctx, insertUpscaleTaskSQL,
		rec.UID, rec.AssetUID, rec.UserID, rec.Origin.String()))
	if err != nil {
		return nil, translate(err, "放大任务")
	}
	return created, nil
}

// LatestByAsset 这张图最新的放大任务，同时校验归属。
// 没排过 / 别人的图一律 ErrNotFound（不泄露存在性）。
func (r *UpscaleRepo) LatestByAsset(ctx context.Context, userID int64, assetUID string) (*dkdomain.UpscaleTaskRecord, error) {
	rec, err := scanUpscaleTask(r.sql.QueryRowContext(ctx, latestUpscaleTaskSQL, userID, assetUID))
	if err != nil {
		return nil, translate(err, "放大任务")
	}
	return rec, nil
}

// GetByUID 按任务编号取一行（worker 从队列信道拿到 uid 之后回读用）。
func (r *UpscaleRepo) GetByUID(ctx context.Context, uid string) (*dkdomain.UpscaleTaskRecord, error) {
	rec, err := scanUpscaleTask(r.sql.QueryRowContext(ctx, getUpscaleTaskSQL, uid))
	if err != nil {
		return nil, translate(err, "放大任务")
	}
	return rec, nil
}

// MarkRunning queued → running。守卫没命中（已被别人处理/状态不对）返回 ErrConflict。
func (r *UpscaleRepo) MarkRunning(ctx context.Context, uid string) error {
	return r.guardedExec(ctx, markUpscaleRunningSQL, uid)
}

// MarkDone running → done，记下产物商品图。
func (r *UpscaleRepo) MarkDone(ctx context.Context, uid, resultAssetUID string) error {
	return r.guardedExec(ctx, markUpscaleDoneSQL, uid, resultAssetUID)
}

// MarkFailed running → failed，记下错误码和给运营看的中文。
func (r *UpscaleRepo) MarkFailed(ctx context.Context, uid, errorCode, errorMessage string) error {
	return r.guardedExec(ctx, markUpscaleFailedSQL, uid, errorCode, errorMessage)
}

// RequeueInterrupted 把 running 的任务重置回 queued（重启恢复第一步），
// 返回重置了几行。
func (r *UpscaleRepo) RequeueInterrupted(ctx context.Context) (int64, error) {
	res, err := r.sql.ExecContext(ctx, requeueInterruptedUpscaleSQL)
	if err != nil {
		return 0, err
	}
	return affectedRows(res)
}

// ListQueued 还排着队的任务，按入队顺序（恢复第二步）。
func (r *UpscaleRepo) ListQueued(ctx context.Context, limit int) ([]*dkdomain.UpscaleTaskRecord, error) {
	rows, err := r.sql.QueryContext(ctx, listQueuedUpscaleSQL,
		clampLimit(limit, DefaultPageLimit, MaxPageLimit))
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	var out []*dkdomain.UpscaleTaskRecord
	for rows.Next() {
		rec, err := scanUpscaleTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// guardedExec 跑一条带状态守卫的 UPDATE，影响行数 0 返回 ErrConflict。
func (r *UpscaleRepo) guardedExec(ctx context.Context, query string, args ...any) error {
	res, err := r.sql.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	affected, err := affectedRows(res)
	if err != nil {
		return err
	}
	if affected == 0 {
		return conflictErr("放大任务的状态已经被别人改过了")
	}
	return nil
}

// ---- 扫描 ----

func scanUpscaleTask(row rowScanner) (*dkdomain.UpscaleTaskRecord, error) {
	var rec dkdomain.UpscaleTaskRecord
	var origin string
	var resultAssetUID sql.NullString
	if err := row.Scan(
		&rec.UID, &rec.AssetUID, &rec.UserID, &origin, &rec.Status,
		&resultAssetUID, &rec.ErrorCode, &rec.ErrorMessage, &rec.CreatedAt, &rec.UpdatedAt,
	); err != nil {
		return nil, err
	}
	// uid 列是 CHAR(26)，值恒为 26 位 ULID，不会出现填充空格；
	// TrimSpace 是防御——手工改库塞了短值时别把空格带进对外 JSON。
	rec.UID = strings.TrimSpace(rec.UID)
	rec.Origin = dkdomain.Origin(origin)
	if resultAssetUID.Valid {
		v := resultAssetUID.String
		rec.ResultAssetUID = &v
	}
	return &rec, nil
}
