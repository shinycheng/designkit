package repository

// 商品图（designkit_assets）和预处理产物（designkit_asset_variants）。
//
// 为什么产物要单独一张表：预处理产物是 (ratio, keep_transparency, max_dimension)
// 三个参数的函数，同一张原图在 3:4 任务和 16:9 任务里是**两个不同文件**。
// 早期设计想在 assets 上放一列 processed_key，那会互相覆盖或被错误复用，
// 导致出图比例静默失控。

import (
	"context"
	"fmt"
	"strings"

	"github.com/lib/pq"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

func (r *designkitRepository) CreateAsset(ctx context.Context, asset *dkdomain.Asset) error {
	if asset == nil {
		return fmt.Errorf("designkit: 商品图为空: %w", dkdomain.ErrNotFound)
	}
	origin := asset.Origin
	if origin == "" {
		origin = dkdomain.OriginWeb
	}
	created, err := scanAsset(r.sql.QueryRowContext(ctx, `
INSERT INTO designkit_assets (
  uid, user_id, object_key, content_type, byte_size, width, height, sha256, origin
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING `+assetColumns,
		asset.UID, asset.UserID, asset.ObjectKey, asset.ContentType, asset.ByteSize,
		asset.Width, asset.Height, asset.SHA256, origin.String()))
	if err != nil {
		return translate(err, "商品图")
	}
	*asset = *created
	return nil
}

func (r *designkitRepository) GetAssetByUID(ctx context.Context, userID int64, uid string) (*dkdomain.Asset, error) {
	asset, err := scanAsset(r.sql.QueryRowContext(ctx,
		`SELECT `+assetColumns+` FROM designkit_assets
		 WHERE uid = $1 AND user_id = $2 AND deleted_at IS NULL`, uid, userID))
	if err != nil {
		return nil, translate(err, "商品图")
	}
	return asset, nil
}

// GetAssetsByIDs 批量取原图，**返回顺序跟传进来的 ids 严格一致**。
//
// 展开批次时的顺序是「商品图外层、提示词内层」，那是对外契约的一部分；
// 数据库不保证还你原顺序，所以这里在内存里重排。
// 任何一个 id 取不到就整体 ErrNotFound —— 少一张会让后面的序号整体错位，
// 「第 3 张」就不是运营看到的第 3 张了。
func (r *designkitRepository) GetAssetsByIDs(ctx context.Context, userID int64, ids []int64) ([]*dkdomain.Asset, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.sql.QueryContext(ctx,
		`SELECT `+assetColumns+` FROM designkit_assets
		 WHERE user_id = $1 AND id = ANY($2) AND deleted_at IS NULL
		 ORDER BY id ASC`, userID, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	assets, err := scanAssets(rows)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*dkdomain.Asset, len(assets))
	for _, a := range assets {
		byID[a.ID] = a
	}

	out := make([]*dkdomain.Asset, 0, len(ids))
	for _, id := range ids {
		a, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("designkit: 商品图 %d 不存在或已删除: %w", id, dkdomain.ErrNotFound)
		}
		out = append(out, a)
	}
	return out, nil
}

// FindAssetBySHA256 认出「同一个人又传了同一张图」，直接复用不重复存。
// 查不到返回 ErrNotFound（调用方据此决定要不要真的上传）。
func (r *designkitRepository) FindAssetBySHA256(ctx context.Context, userID int64, sha256 string) (*dkdomain.Asset, error) {
	if strings.TrimSpace(sha256) == "" {
		return nil, notFoundErr("商品图")
	}
	asset, err := scanAsset(r.sql.QueryRowContext(ctx,
		`SELECT `+assetColumns+` FROM designkit_assets
		 WHERE user_id = $1 AND sha256 = $2 AND deleted_at IS NULL
		 ORDER BY id DESC LIMIT 1`, userID, sha256))
	if err != nil {
		return nil, translate(err, "商品图")
	}
	return asset, nil
}

// buildListAssetsQuery 商品图列表，游标分页，按 (created_at DESC, id DESC)。
func buildListAssetsQuery(query dkdomain.ListAssetsQuery) (string, []any) {
	args := []any{query.UserID}
	var b strings.Builder
	_, _ = b.WriteString("SELECT " + assetColumns + " FROM designkit_assets " +
		"WHERE user_id = $1 AND deleted_at IS NULL")
	if !query.CursorCreatedAt.IsZero() && query.CursorID > 0 {
		args = append(args, query.CursorCreatedAt, query.CursorID)
		fmt.Fprintf(&b, " AND (created_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, clampLimit(query.Limit, DefaultPageLimit, MaxPageLimit))
	fmt.Fprintf(&b, " ORDER BY created_at DESC, id DESC LIMIT $%d", len(args))
	return b.String(), args
}

func (r *designkitRepository) ListAssets(ctx context.Context, query dkdomain.ListAssetsQuery) ([]*dkdomain.Asset, error) {
	sqlText, args := buildListAssetsQuery(query)
	rows, err := r.sql.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	return scanAssets(rows)
}

// SoftDeleteAsset 软删除：用户删了就不再列出，但历史任务仍能引用
// （job_items.asset_id 上刻意没建外键，就是为了这个）。
func (r *designkitRepository) SoftDeleteAsset(ctx context.Context, userID int64, uid string) error {
	res, err := r.sql.ExecContext(ctx, `
UPDATE designkit_assets SET deleted_at = NOW()
WHERE uid = $1 AND user_id = $2 AND deleted_at IS NULL`, uid, userID)
	if err != nil {
		return err
	}
	affected, err := affectedRows(res)
	if err != nil {
		return err
	}
	if affected == 0 {
		return notFoundErr("商品图")
	}
	return nil
}

func (r *designkitRepository) GetVariant(ctx context.Context, assetID int64, ratio dkdomain.Ratio, keepTransparency bool, maxDimension int) (*dkdomain.AssetVariant, error) {
	v, err := scanAssetVariant(r.sql.QueryRowContext(ctx,
		`SELECT `+assetVariantColumns+` FROM designkit_asset_variants
		 WHERE asset_id = $1 AND ratio = $2 AND keep_transparency = $3 AND max_dimension = $4`,
		assetID, ratio.String(), keepTransparency, maxDimension))
	if err != nil {
		return nil, translate(err, "预处理产物")
	}
	return v, nil
}

// UpsertVariant 写入产物。四个参数唯一确定一个产物，并发重复预处理时靠
// ON CONFLICT 收敛，不炸也不产生两行。
func (r *designkitRepository) UpsertVariant(ctx context.Context, params dkdomain.UpsertVariantParams) (*dkdomain.AssetVariant, error) {
	v, err := scanAssetVariant(r.sql.QueryRowContext(ctx, `
INSERT INTO designkit_asset_variants (
  asset_id, ratio, keep_transparency, max_dimension, object_key, width, height, content_type
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (asset_id, ratio, keep_transparency, max_dimension) DO UPDATE SET
  object_key = EXCLUDED.object_key,
  width = EXCLUDED.width,
  height = EXCLUDED.height,
  content_type = EXCLUDED.content_type
RETURNING `+assetVariantColumns,
		params.AssetID, params.Ratio.String(), params.KeepTransparency, params.MaxDimension,
		params.ObjectKey, params.Width, params.Height, params.ContentType))
	if err != nil {
		return nil, translate(err, "预处理产物")
	}
	return v, nil
}
