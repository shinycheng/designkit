package domain

// 高清放大任务的持久化形态（designkit_upscale_tasks，9004 迁移）。
//
// 状态字面量（queued/running/done/failed）定义在 service 层的 UpscaleStatus 上
//（它是对外契约），这里刻意用裸 string 存——domain 不反过来依赖 service，
// 而两边的字面量由数据库的 CHECK 约束和 service 的单测共同钉住。
//
// 【为什么一次重试一行】失败后重新放大 = 插一行新任务，旧的 failed 行留作
// 历史。所以「这张图现在的放大状态」永远是**最新的一行**
//（UpscaleTaskStore.LatestByAsset），(user_id, asset_uid) 上是普通索引不是唯一索引。

import "time"

// UpscaleTaskRecord 是 designkit_upscale_tasks 的一行。
type UpscaleTaskRecord struct {
	// UID 任务编号（26 位 ULID，主键）。对外不暴露——接口按 asset_uid 查任务。
	UID string
	// AssetUID 被放大的那张商品图（designkit_assets.uid）。
	AssetUID string
	// UserID 谁点的。归属过滤在 SQL 里做（查别人的一律「找不到」）。
	UserID int64
	// Origin web / erp，入库结果记在这个来路上。
	Origin Origin
	// Status queued / running / done / failed（数据库 CHECK 钉死这四个）。
	Status string
	// ResultAssetUID done 时的产物：一条新的商品图（sha256 去重）。其余状态为 nil。
	ResultAssetUID *string
	// ErrorCode failed 时的我方错误码（DK_ 前缀），空串 = 无。
	ErrorCode string
	// ErrorMessage failed 时给运营看的中文，空串 = 无。
	ErrorMessage string
	// CreatedAt 入队时间。
	CreatedAt time.Time
	// UpdatedAt 最近一次状态变化时间。
	UpdatedAt time.Time
}
