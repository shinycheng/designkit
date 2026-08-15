package repository

// AI 对话：会话（designkit_chat_sessions）和消息（designkit_chat_messages）。
//
// 方法签名是跟上层服务约定死的（冻结），改任何一个都要连着服务层一起改。
//
// 两条本包铁律在这里的落点：
//  1. 所有返回列表的查询显式 ORDER BY —— 消息按 id ASC（对话顺序就是插入顺序），
//     会话按 updated_at DESC（最近聊的排最前）；
//  2. 归属过滤在 SQL 里做（WHERE user_id = ?），不是查出来再判 ——
//     查别人的会话和查不存在的会话必须**同样**返回 ErrNotFound，
//     不能让调用方从错误形态里区分「有没有这个会话」。

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ---- 列清单 ----
//
// 列名和顺序必须跟 backend/migrations/9002_designkit_chat.sql 一模一样
// （chat_repo_test.go 会去解析那份迁移逐列比对，改错了单测当场红）。
// 会话的 deleted_at 刻意不在清单里：查询一律只取未删的，实体里也没有这个字段。

const chatSessionColumns = `id, uid, user_id, title, created_at, updated_at`

const chatMessageColumns = `id, session_id, role, content, asset_uids, request_id, created_at`

// ---- SQL ----

const createChatSessionSQL = `
INSERT INTO designkit_chat_sessions (uid, user_id, title)
VALUES ($1, $2, $3)
RETURNING ` + chatSessionColumns

// getChatSessionByUIDSQL 只取**本人未删的**。
// uid 撞上别人的会话时也返回「找不到」，绝不能返回「没权限」——
// 后者等于告诉对方「这个编号存在」。
const getChatSessionByUIDSQL = `SELECT ` + chatSessionColumns + `
FROM designkit_chat_sessions
WHERE uid = $1 AND user_id = $2 AND deleted_at IS NULL`

// listChatSessionsSQL 会话列表：最近聊的排最前。
// id DESC 是并列裁决：同一毫秒建的两个会话没有它顺序不稳定，翻页会跳。
const listChatSessionsSQL = `SELECT ` + chatSessionColumns + `
FROM designkit_chat_sessions
WHERE user_id = $1 AND deleted_at IS NULL
ORDER BY updated_at DESC, id DESC
LIMIT $2 OFFSET $3`

const softDeleteChatSessionSQL = `
UPDATE designkit_chat_sessions
SET deleted_at = NOW(), updated_at = NOW()
WHERE uid = $1 AND user_id = $2 AND deleted_at IS NULL`

const insertChatMessageSQL = `
INSERT INTO designkit_chat_messages (session_id, role, content, asset_uids, request_id)
VALUES ($1, $2, $3, $4, $5)
RETURNING ` + chatMessageColumns

// listChatMessagesSQL 一次拉一个会话的全部消息。
// ORDER BY id ASC 是对话顺序的唯一保证（CLAUDE.md：顺序必须显式），
// 正好落在 (session_id, id) 索引上。
const listChatMessagesSQL = `SELECT ` + chatMessageColumns + `
FROM designkit_chat_messages
WHERE session_id = $1
ORDER BY id ASC`

// buildTouchChatSessionQuery 摸一下会话（每插一条消息调一次）。
// title 空串 = 只更新 updated_at，**不碰 title**——空串是「没起名」不是「改名成空」，
// 拿它覆盖会把已经起好的标题抹掉。
func buildTouchChatSessionQuery(sessionID int64, title string) (string, []any) {
	if title == "" {
		return `UPDATE designkit_chat_sessions SET updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL`, []any{sessionID}
	}
	return `UPDATE designkit_chat_sessions SET title = $2, updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL`, []any{sessionID, title}
}

// ---- ChatRepo ----

// ChatRepo 是 AI 对话的持久化层。跟 designkitRepository 一样只依赖 *sql.DB，
// 不碰 ent（CLAUDE.md 第二·五节 A2）。
type ChatRepo struct {
	sql dkExecutor
}

// NewChatRepo 建对话持久化层。db 来自 designkit 自己的连接池（internal/designkit/db）。
func NewChatRepo(db *sql.DB) *ChatRepo {
	return &ChatRepo{sql: db}
}

// CreateSession 建一段新对话。uid 由调用方生成（26 位 ULID）。
func (r *ChatRepo) CreateSession(ctx context.Context, userID int64, uid string, title string) (*dkdomain.ChatSession, error) {
	if strings.TrimSpace(uid) == "" {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("会话缺少编号（uid），无法创建。")
	}
	s, err := scanChatSession(r.sql.QueryRowContext(ctx, createChatSessionSQL, uid, userID, title))
	if err != nil {
		return nil, translate(err, "对话")
	}
	return s, nil
}

// GetSessionByUID 按对外编号取会话，同时校验归属。
// 只取本人未删的；查不到（不存在 / 别人的 / 已删）一律 ErrNotFound。
func (r *ChatRepo) GetSessionByUID(ctx context.Context, userID int64, uid string) (*dkdomain.ChatSession, error) {
	s, err := scanChatSession(r.sql.QueryRowContext(ctx, getChatSessionByUIDSQL, uid, userID))
	if err != nil {
		return nil, translate(err, "对话")
	}
	return s, nil
}

// ListSessions 本人未删的会话，最近活跃的排最前（显式 ORDER BY updated_at DESC）。
func (r *ChatRepo) ListSessions(ctx context.Context, userID int64, limit, offset int) ([]*dkdomain.ChatSession, error) {
	if offset < 0 {
		offset = 0
	}
	rows, err := r.sql.QueryContext(ctx, listChatSessionsSQL,
		userID, clampLimit(limit, DefaultPageLimit, MaxPageLimit), offset)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	return scanChatSessions(rows)
}

// TouchSession 把会话的 updated_at 摸到现在；title 非空时顺带改标题。
// 会话不存在或已删返回 ErrNotFound。
func (r *ChatRepo) TouchSession(ctx context.Context, sessionID int64, title string) error {
	sqlText, args := buildTouchChatSessionQuery(sessionID, title)
	res, err := r.sql.ExecContext(ctx, sqlText, args...)
	if err != nil {
		return err
	}
	affected, err := affectedRows(res)
	if err != nil {
		return err
	}
	if affected == 0 {
		return notFoundErr("对话")
	}
	return nil
}

// SoftDeleteSession 软删一段本人的对话。
// 软删而不是真删：消息行保留，哪天要「恢复最近删除」不用补数据。
func (r *ChatRepo) SoftDeleteSession(ctx context.Context, userID int64, uid string) error {
	res, err := r.sql.ExecContext(ctx, softDeleteChatSessionSQL, uid, userID)
	if err != nil {
		return err
	}
	affected, err := affectedRows(res)
	if err != nil {
		return err
	}
	if affected == 0 {
		return notFoundErr("对话")
	}
	return nil
}

// InsertMessage 插一条消息，返回带 ID / CreatedAt 的完整行。
// **不负责摸会话**——调用方插完自己调 TouchSession（要不要改标题只有它知道）。
func (r *ChatRepo) InsertMessage(ctx context.Context, msg *dkdomain.ChatMessage) (*dkdomain.ChatMessage, error) {
	if msg == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("消息为空，无法保存。")
	}
	if msg.Role != dkdomain.ChatRoleUser && msg.Role != dkdomain.ChatRoleAssistant {
		// 数据库有同样的 CHECK 约束，但那个报出来是英文的 check_violation，
		// 翻译不了；在这里拦下才能给出说得清的错误。
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessagef("消息角色 %q 不合法。", msg.Role)
	}
	uids, err := encodeChatAssetUIDs(msg.AssetUIDs)
	if err != nil {
		return nil, err
	}
	created, err := scanChatMessage(r.sql.QueryRowContext(ctx, insertChatMessageSQL,
		msg.SessionID, msg.Role, msg.Content, uids, msg.RequestID))
	if err != nil {
		return nil, translate(err, "消息")
	}
	return created, nil
}

// ListMessages 一个会话的全部消息，按 id 升序（就是对话顺序）。
// 归属校验在会话层做（GetSessionByUID），这里只认 sessionID。
func (r *ChatRepo) ListMessages(ctx context.Context, sessionID int64) ([]*dkdomain.ChatMessage, error) {
	rows, err := r.sql.QueryContext(ctx, listChatMessagesSQL, sessionID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	return scanChatMessages(rows)
}

// ---- 扫描 ----

func scanChatSession(row rowScanner) (*dkdomain.ChatSession, error) {
	var s dkdomain.ChatSession
	if err := row.Scan(
		&s.ID, &s.UID, &s.UserID, &s.Title, &s.CreatedAt, &s.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &s, nil
}

func scanChatSessions(rows *sql.Rows) ([]*dkdomain.ChatSession, error) {
	var out []*dkdomain.ChatSession
	for rows.Next() {
		s, err := scanChatSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanChatMessage(row rowScanner) (*dkdomain.ChatMessage, error) {
	var m dkdomain.ChatMessage
	var assetUIDs []byte
	if err := row.Scan(
		&m.ID, &m.SessionID, &m.Role, &m.Content, &assetUIDs, &m.RequestID, &m.CreatedAt,
	); err != nil {
		return nil, err
	}
	uids, err := decodeChatAssetUIDs(assetUIDs)
	if err != nil {
		return nil, err
	}
	m.AssetUIDs = uids
	return &m, nil
}

func scanChatMessages(rows *sql.Rows) ([]*dkdomain.ChatMessage, error) {
	var out []*dkdomain.ChatMessage
	for rows.Next() {
		m, err := scanChatMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- JSONB ----

// encodeChatAssetUIDs 把商品图 uid 数组编码成 JSONB 参数。
// nil 一律写成 []，不写 null —— 列是 NOT NULL（同 encodePromptVariables 的约定）。
func encodeChatAssetUIDs(uids []string) ([]byte, error) {
	if len(uids) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal(uids)
}

// decodeChatAssetUIDs 把 asset_uids 这一列解出来。
// 列上有 NOT NULL DEFAULT '[]'，但历史数据/手工改库都可能给空，兜住不炸。
func decodeChatAssetUIDs(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var uids []string
	if err := json.Unmarshal(raw, &uids); err != nil {
		return nil, err
	}
	return uids, nil
}
