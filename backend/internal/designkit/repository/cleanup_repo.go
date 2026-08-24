package repository

// 图片自动清理（决策 17 的落地）的持久化层。
//
// 【判据】9001 里 designkit_images 有预留的 expires_at（带部分索引），但从来没有
// 任何代码往里写值；designkit_assets 连这一列都没有。所以清理的判据统一是
// 「created_at 早于 now − 保留天数」；designkit_images.expires_at **若被显式设过也认**
// （字段和索引都在，认了它这条列才算真的启用），两个条件满足其一即过期。
//
// 【只挑 deleted_at IS NULL 的行】deleted_at 同时充当「本轮清过了」的标记：
// 软删掉的行下一轮不再出现，文件删除是幂等的（ObjectStore.Delete 对不存在的
// key 返回 nil），所以整个流程重跑无害。运营自己软删过的素材（SoftDeleteAsset）
// 刻意**不在**清理范围内 —— 那条流程的约定是「历史任务仍能引用」，文件要留。
//
// 【被未结束批次引用的素材不删】批次还在跑（created/holding/running/settling）时，
// worker 随时会按 asset 的 object_key 去读原图做预处理。筛选时用 NOT EXISTS
// 把这些素材整个排除，而不是删了文件让 worker 读空。结果图同理按 job 状态排除
// （正常不会出现「图比保留天数老、批次还没结束」，但僵尸批次可能凑出这种行，
// 多一道守卫零成本）。
//
// 【一次最多 LIMIT 条】调用方按批处理（默认 500），一批一个短事务，
// 不把上万行的 UPDATE 压在一条语句里锁表。

import (
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// cleanupBusyJobStatuses 「还没结束」的批次状态（非终态）。
// 从 domain 的状态表推出来而不是手抄字符串：以后加状态时这里自动跟上。
func cleanupBusyJobStatuses() []string {
	all := dkdomain.AllJobStatuses()
	busy := make([]string, 0, len(all))
	for _, s := range all {
		if !s.IsTerminal() {
			busy = append(busy, s.String())
		}
	}
	return busy
}

// listExpiredImagesSQL 过期结果图。expires_at 显式设过也认（见文件头）。
const listExpiredImagesSQL = `
SELECT i.id, i.object_key, i.byte_size
FROM designkit_images i
WHERE i.deleted_at IS NULL
  AND (i.created_at < $1 OR (i.expires_at IS NOT NULL AND i.expires_at < NOW()))
  AND NOT EXISTS (
        SELECT 1 FROM designkit_jobs j
        WHERE j.id = i.job_id AND j.status = ANY($2)
  )
ORDER BY i.id ASC
LIMIT $3`

// listExpiredAssetsSQL 过期素材。被未结束批次引用的整个排除。
const listExpiredAssetsSQL = `
SELECT a.id, a.object_key, a.byte_size
FROM designkit_assets a
WHERE a.deleted_at IS NULL
  AND a.created_at < $1
  AND NOT EXISTS (
        SELECT 1
        FROM designkit_job_items ji
        JOIN designkit_jobs j ON j.id = ji.job_id
        WHERE ji.asset_id = a.id AND j.status = ANY($2)
  )
ORDER BY a.id ASC
LIMIT $3`

// listVariantKeysSQL 素材名下全部预处理产物的文件路径。
// 产物表没有 deleted_at 也没有 byte_size：文件跟着原图一起删，行保留 ——
// 原图软删之后 GetAssetByUID 就查不到它了，EnsureVariant 不可能再走到这些行，
// 留着的行只是历史记录，不会有人拿它去读一个已删除的文件。
const listVariantKeysSQL = `
SELECT asset_id, object_key
FROM designkit_asset_variants
WHERE asset_id = ANY($1)
ORDER BY asset_id ASC, id ASC`

const softDeleteImagesSQL = `
UPDATE designkit_images SET deleted_at = NOW()
WHERE id = ANY($1) AND deleted_at IS NULL`

const softDeleteAssetsSQL = `
UPDATE designkit_assets SET deleted_at = NOW()
WHERE id = ANY($1) AND deleted_at IS NULL`

// CleanupRepo 图片自动清理的持久化层。跟 ChatRepo / UpscaleRepo 一样
// 只依赖 *sql.DB，不碰 ent（CLAUDE.md 第二·五节 A2）。
type CleanupRepo struct {
	sql dkExecutor
}

// NewCleanupRepo 建清理持久化层。db 来自 designkit 自己的连接池。
func NewCleanupRepo(db *sql.DB) *CleanupRepo {
	return &CleanupRepo{sql: db}
}

// ListExpiredImages 取一批过期的结果图（按 id 升序，最多 limit 条）。
func (r *CleanupRepo) ListExpiredImages(ctx context.Context, cutoff time.Time, limit int) ([]dkdomain.CleanupCandidate, error) {
	return r.listCandidates(ctx, listExpiredImagesSQL, cutoff, limit)
}

// ListExpiredAssets 取一批过期的素材（按 id 升序，最多 limit 条）。
func (r *CleanupRepo) ListExpiredAssets(ctx context.Context, cutoff time.Time, limit int) ([]dkdomain.CleanupCandidate, error) {
	return r.listCandidates(ctx, listExpiredAssetsSQL, cutoff, limit)
}

func (r *CleanupRepo) listCandidates(ctx context.Context, query string, cutoff time.Time, limit int) ([]dkdomain.CleanupCandidate, error) {
	if r == nil || r.sql == nil {
		return nil, ErrNoDB
	}
	rows, err := r.sql.QueryContext(ctx, query, cutoff, pq.Array(cleanupBusyJobStatuses()), limit)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	var out []dkdomain.CleanupCandidate
	for rows.Next() {
		var c dkdomain.CleanupCandidate
		if err := rows.Scan(&c.ID, &c.ObjectKey, &c.ByteSize); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListVariantKeysByAsset 素材 id → 名下全部预处理产物的文件路径。
func (r *CleanupRepo) ListVariantKeysByAsset(ctx context.Context, assetIDs []int64) (map[int64][]string, error) {
	if r == nil || r.sql == nil {
		return nil, ErrNoDB
	}
	if len(assetIDs) == 0 {
		return map[int64][]string{}, nil
	}
	rows, err := r.sql.QueryContext(ctx, listVariantKeysSQL, pq.Array(assetIDs))
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	out := make(map[int64][]string)
	for rows.Next() {
		var assetID int64
		var key string
		if err := rows.Scan(&assetID, &key); err != nil {
			return nil, err
		}
		out[assetID] = append(out[assetID], key)
	}
	return out, rows.Err()
}

// SoftDeleteImages 把这批结果图标记删除，返回真正改到的行数。
func (r *CleanupRepo) SoftDeleteImages(ctx context.Context, ids []int64) (int64, error) {
	return r.softDelete(ctx, softDeleteImagesSQL, ids)
}

// SoftDeleteAssets 把这批素材标记删除，返回真正改到的行数。
func (r *CleanupRepo) SoftDeleteAssets(ctx context.Context, ids []int64) (int64, error) {
	return r.softDelete(ctx, softDeleteAssetsSQL, ids)
}

func (r *CleanupRepo) softDelete(ctx context.Context, query string, ids []int64) (int64, error) {
	if r == nil || r.sql == nil {
		return 0, ErrNoDB
	}
	if len(ids) == 0 {
		return 0, nil
	}
	res, err := r.sql.ExecContext(ctx, query, pq.Array(ids))
	if err != nil {
		return 0, err
	}
	return affectedRows(res)
}
