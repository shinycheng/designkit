package repository

// 「用户记录」（管理端）：跨用户查询对话会话和出图批次。
//
// 跟 ChatRepo 一样是独立的小仓储（只依赖 *sql.DB，不进 domain.Repository ——
// 那个接口是冻结的，这里是纯新增的管理端能力）。
//
// 三条口径，三层（repository / service / handler）一致：
//  1. **不做归属过滤**（这是管理员通道，RequireAdmin 在路由层挡普通用户）；
//  2. **软删过的一律不给**：会话 deleted_at IS NULL、批次 user_deleted_at IS NULL
//     —— 跟用户自己看到的一致，不把人家删掉的记录翻出来；
//  3. 所有列表显式 ORDER BY（本包铁律）：会话按 updated_at DESC、
//     批次按 created_at DESC、item 按 seq ASC，并列一律拿 id 裁决。
//
// JOIN 上游 users 只读邮箱（照 billing_repo 的 quotaRequestDetailSQL 手法）：
// LEFT JOIN + COALESCE，账号已删时行照样列出来、邮箱给空串，**绝不写 users**。

import (
	"context"
	"database/sql"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// AdminRecordsRepo 是「用户记录」的持久化层。
type AdminRecordsRepo struct {
	sql dkExecutor
}

// NewAdminRecordsRepo 建仓储。db 来自 designkit 自己的连接池（internal/designkit/db）。
func NewAdminRecordsRepo(db *sql.DB) *AdminRecordsRepo {
	return &AdminRecordsRepo{sql: db}
}

// 列表的默认页/封顶（对外契约：limit 默认 50、封顶 200）。
// handler 已经按这两个数收敛过，这里再夹一次是防「有人绕过 handler 直接调仓储」。
const (
	// AdminRecordsDefaultLimit 列表默认条数。
	AdminRecordsDefaultLimit = 50
	// AdminRecordsMaxLimit 列表条数封顶。
	AdminRecordsMaxLimit = 200
)

// extraScanner 在既有 scanXxx 扫描目标的**后面**追加几个扫描目标。
//
// JOIN 出来的行 = 实体列 + 追加列（邮箱、计数……）。有了它，
// scanJob / scanChatSession / scanJobItem 这些函数可以原样复用，
// 不用为了多一列邮箱把 37 列的扫描清单重抄一遍（抄错一列就是错位读数）。
// 前提：SQL 里追加列必须放在实体列**之后**，顺序跟 extra 一致。
type extraScanner struct {
	row   rowScanner
	extra []any
}

func (s extraScanner) Scan(dest ...any) error {
	return s.row.Scan(append(dest, s.extra...)...)
}

// ---- 有记录的账户 ----

// adminRecordUsersSQL 「有记录的账户」：会话或批次至少有一条未删记录的 user_id 全集。
//
// 从两张 designkit 表 UNION 出人，再 LEFT JOIN users 补邮箱 ——
// 账号已删时行保留、邮箱空串（记录还在，不能因为删号就从筛选框里消失）。
// ORDER BY r.user_id ASC：下拉框要一个稳定顺序，id 序即注册序。
const adminRecordUsersSQL = `
SELECT r.user_id,
       COALESCE(u.email, ''),
       COALESCE(s.cnt, 0),
       COALESCE(j.cnt, 0)
FROM (
  SELECT user_id FROM designkit_chat_sessions WHERE deleted_at IS NULL
  UNION
  SELECT user_id FROM designkit_jobs WHERE user_deleted_at IS NULL
) r
LEFT JOIN users u ON u.id = r.user_id AND u.deleted_at IS NULL
LEFT JOIN (
  SELECT user_id, COUNT(*) AS cnt FROM designkit_chat_sessions
  WHERE deleted_at IS NULL GROUP BY user_id
) s ON s.user_id = r.user_id
LEFT JOIN (
  SELECT user_id, COUNT(*) AS cnt FROM designkit_jobs
  WHERE user_deleted_at IS NULL GROUP BY user_id
) j ON j.user_id = r.user_id
ORDER BY r.user_id ASC`

// ListRecordUsers 列出有记录的账户（给筛选下拉用）。
func (r *AdminRecordsRepo) ListRecordUsers(ctx context.Context) ([]*dkdomain.RecordUser, error) {
	rows, err := r.sql.QueryContext(ctx, adminRecordUsersSQL)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	var out []*dkdomain.RecordUser
	for rows.Next() {
		var u dkdomain.RecordUser
		if err := rows.Scan(&u.ID, &u.Email, &u.SessionCount, &u.JobCount); err != nil {
			return nil, err
		}
		out = append(out, &u)
	}
	return out, rows.Err()
}

// ---- 对话会话 ----

// adminChatSessionSelect 会话行的公共 SELECT：实体列在前、追加列（邮箱、消息数）在后
// （extraScanner 的前提）。消息数走相关子查询，落在 (session_id, id) 索引上。
const adminChatSessionSelect = `
SELECT s.id, s.uid, s.user_id, s.title, s.created_at, s.updated_at,
       COALESCE(u.email, ''),
       (SELECT COUNT(*) FROM designkit_chat_messages m WHERE m.session_id = s.id)
FROM designkit_chat_sessions s
LEFT JOIN users u ON u.id = s.user_id AND u.deleted_at IS NULL`

// buildAdminListChatSessionsSQL 会话列表。filterByUser=false 时看全部账户。
//
// ORDER BY s.updated_at DESC：最近聊的排最前（跟用户自己的列表同一个序）；
// s.id DESC 是并列裁决，同一毫秒摸过的两个会话没有它翻页会跳。
func buildAdminListChatSessionsSQL(filterByUser bool) string {
	where := `
WHERE s.deleted_at IS NULL`
	if filterByUser {
		where += ` AND s.user_id = $3`
	}
	return adminChatSessionSelect + where + `
ORDER BY s.updated_at DESC, s.id DESC
LIMIT $1 OFFSET $2`
}

// getAdminChatSessionSQL 单条会话。**没有 user_id 条件**（管理员通道），
// 但软删过的照样不给。
const getAdminChatSessionSQL = adminChatSessionSelect + `
WHERE s.uid = $1 AND s.deleted_at IS NULL`

func scanAdminChatSession(row rowScanner) (*dkdomain.ChatSessionAdminView, error) {
	var v dkdomain.ChatSessionAdminView
	s, err := scanChatSession(extraScanner{row: row, extra: []any{&v.UserEmail, &v.MessageCount}})
	if err != nil {
		return nil, err
	}
	v.ChatSession = *s
	return &v, nil
}

// ListChatSessions 会话列表。userID=0 看全部账户；否则只看这一个人的。
func (r *AdminRecordsRepo) ListChatSessions(ctx context.Context, userID int64, limit, offset int) ([]*dkdomain.ChatSessionAdminView, error) {
	if offset < 0 {
		offset = 0
	}
	args := []any{clampLimit(limit, AdminRecordsDefaultLimit, AdminRecordsMaxLimit), offset}
	filterByUser := userID > 0
	if filterByUser {
		args = append(args, userID)
	}
	rows, err := r.sql.QueryContext(ctx, buildAdminListChatSessionsSQL(filterByUser), args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	var out []*dkdomain.ChatSessionAdminView
	for rows.Next() {
		v, err := scanAdminChatSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetChatSessionByUID 单条会话。不存在（或已被用户删掉）返回 ErrNotFound。
func (r *AdminRecordsRepo) GetChatSessionByUID(ctx context.Context, uid string) (*dkdomain.ChatSessionAdminView, error) {
	v, err := scanAdminChatSession(r.sql.QueryRowContext(ctx, getAdminChatSessionSQL, uid))
	if err != nil {
		return nil, translate(err, "对话")
	}
	return v, nil
}

// ListChatMessages 一个会话的全部消息，按 id 升序（就是对话顺序）。
// 复用用户侧那条 SQL（listChatMessagesSQL）：消息层本来就不带归属条件。
func (r *AdminRecordsRepo) ListChatMessages(ctx context.Context, sessionID int64) ([]*dkdomain.ChatMessage, error) {
	rows, err := r.sql.QueryContext(ctx, listChatMessagesSQL, sessionID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	return scanChatMessages(rows)
}

// ---- 出图批次 ----

// adminJobSelect 批次行的公共 SELECT：实体列在前、邮箱在后。
var adminJobSelect = `
SELECT ` + prefixColumns("j", jobColumns) + `,
       COALESCE(u.email, '')
FROM designkit_jobs j
LEFT JOIN users u ON u.id = j.user_id AND u.deleted_at IS NULL`

// buildAdminListJobsSQL 批次列表。filterByUser=false 时看全部账户。
// ORDER BY j.created_at DESC：最新提交的排最前；j.id DESC 并列裁决。
func buildAdminListJobsSQL(filterByUser bool) string {
	where := `
WHERE j.user_deleted_at IS NULL`
	if filterByUser {
		where += ` AND j.user_id = $3`
	}
	return adminJobSelect + where + `
ORDER BY j.created_at DESC, j.id DESC
LIMIT $1 OFFSET $2`
}

// getAdminJobSQL 单个批次。没有 user_id 条件；用户软删的照样不给。
var getAdminJobSQL = adminJobSelect + `
WHERE j.uid = $1 AND j.user_deleted_at IS NULL`

func scanAdminJob(row rowScanner) (*dkdomain.JobAdminView, error) {
	var v dkdomain.JobAdminView
	job, err := scanJob(extraScanner{row: row, extra: []any{&v.UserEmail}})
	if err != nil {
		return nil, err
	}
	v.Job = *job
	return &v, nil
}

// ListJobs 批次列表。userID=0 看全部账户；否则只看这一个人的。
func (r *AdminRecordsRepo) ListJobs(ctx context.Context, userID int64, limit, offset int) ([]*dkdomain.JobAdminView, error) {
	if offset < 0 {
		offset = 0
	}
	args := []any{clampLimit(limit, AdminRecordsDefaultLimit, AdminRecordsMaxLimit), offset}
	filterByUser := userID > 0
	if filterByUser {
		args = append(args, userID)
	}
	rows, err := r.sql.QueryContext(ctx, buildAdminListJobsSQL(filterByUser), args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	var out []*dkdomain.JobAdminView
	for rows.Next() {
		v, err := scanAdminJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetJobByUID 单个批次。不存在（或已被用户删掉）返回 ErrNotFound。
func (r *AdminRecordsRepo) GetJobByUID(ctx context.Context, uid string) (*dkdomain.JobAdminView, error) {
	v, err := scanAdminJob(r.sql.QueryRowContext(ctx, getAdminJobSQL, uid))
	if err != nil {
		return nil, translate(err, "任务")
	}
	return v, nil
}

// adminJobItemsSQL 批次里的每一张 + 有没有出好的图。
//
// ORDER BY it.seq ASC 跟用户侧同一条契约（本包铁律 1）。
// has_image 用 EXISTS 一趟查完：只认**当前版本、未软删**的图 ——
// 跟取图字节那条路（ListCurrentImagesByItem）的判据一致，
// 否则会出现「列表说有图、点开取不到」。
var adminJobItemsSQL = `
SELECT ` + prefixColumns("it", jobItemColumns) + `,
       EXISTS(SELECT 1 FROM designkit_images i
              WHERE i.item_id = it.id AND i.is_current AND i.deleted_at IS NULL)
FROM designkit_job_items it
WHERE it.job_id = $1
ORDER BY it.seq ASC`

// ListJobItemsWithImageFlag 一个批次的全部 item，按 seq 升序，带 has_image。
func (r *AdminRecordsRepo) ListJobItemsWithImageFlag(ctx context.Context, jobID int64) ([]*dkdomain.JobItemAdminView, error) {
	rows, err := r.sql.QueryContext(ctx, adminJobItemsSQL, jobID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	var out []*dkdomain.JobItemAdminView
	for rows.Next() {
		var v dkdomain.JobItemAdminView
		item, err := scanJobItem(extraScanner{row: rows, extra: []any{&v.HasImage}})
		if err != nil {
			return nil, err
		}
		v.JobItem = *item
		out = append(out, &v)
	}
	return out, rows.Err()
}

// GetJobItemBySeq 按序号取某一张（取图字节那条路用）。seq 从 1 开始。
func (r *AdminRecordsRepo) GetJobItemBySeq(ctx context.Context, jobID int64, seq int) (*dkdomain.JobItem, error) {
	item, err := scanJobItem(r.sql.QueryRowContext(ctx,
		`SELECT `+jobItemColumns+` FROM designkit_job_items WHERE job_id = $1 AND seq = $2`,
		jobID, seq))
	if err != nil {
		return nil, translate(err, "任务里的这一张")
	}
	return item, nil
}

// ListCurrentImagesByItem 某一张当前版本的结果图（按 image_index 升序）。
// 复用用户侧的 SQL（buildListImagesByItemSQL），判据跟 has_image 一致。
func (r *AdminRecordsRepo) ListCurrentImagesByItem(ctx context.Context, itemID int64) ([]*dkdomain.Image, error) {
	rows, err := r.sql.QueryContext(ctx, buildListImagesByItemSQL(true), itemID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	return scanImages(rows)
}

// ---- 对话附图（商品图）----

// getAdminAssetSQL 跨用户按对外编号取一张商品图（对话附图的缩略图那条路用）。
// **没有 user_id 条件**（管理员通道，口径 1），但软删过的照样不给（口径 2）。
const getAdminAssetSQL = `SELECT ` + assetColumns + ` FROM designkit_assets
WHERE uid = $1 AND deleted_at IS NULL`

// GetAssetByUID 单张商品图。不存在（或已被主人删掉）返回 ErrNotFound。
func (r *AdminRecordsRepo) GetAssetByUID(ctx context.Context, uid string) (*dkdomain.Asset, error) {
	asset, err := scanAsset(r.sql.QueryRowContext(ctx, getAdminAssetSQL, uid))
	if err != nil {
		return nil, translate(err, "商品图")
	}
	return asset, nil
}
