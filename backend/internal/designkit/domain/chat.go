package domain

import "time"

// ============================================================================
// AI 对话
// ============================================================================
//
// 字段跟 backend/migrations/9002_designkit_chat.sql 的列一一对应，顺序也跟 SQL
// 一致，方便逐行核对。那个迁移文件跑过之后永不可改，列名以它为准。
//
// 约定同 entity.go：ID 是自增主键只在内部用；UID 是 26 位 ULID，接口里只出现它。
// ChatSession 的 deleted_at 列**刻意不进结构体**——查询一律只取未删的，
// 软删标记留在数据库里就够了，带出来只会诱导上层拿它做分支。

// 消息角色。对应 designkit_chat_messages.role 的 CHECK 约束，取值定了别改。
const (
	// ChatRoleUser 运营发的。
	ChatRoleUser = "user"
	// ChatRoleAssistant AI 回的。
	ChatRoleAssistant = "assistant"
)

// ChatSession 是一段对话。
type ChatSession struct {
	// ID 自增主键，只在内部用。
	ID int64
	// UID 对外暴露的 ULID，接口里只出现它。
	UID string
	// UserID 上游 users.id（无外键，理由见迁移文件头）。
	UserID int64
	// Title 会话标题；空串 = 还没起名。
	Title string
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 最近活跃时间：每插一条消息就摸一下，列表按它倒序。
	UpdatedAt time.Time
}

// ChatMessage 是对话里的一条消息。
type ChatMessage struct {
	// ID 自增主键。消息顺序就是 id 顺序，读取显式 ORDER BY id ASC。
	ID int64
	// SessionID 指向 designkit_chat_sessions.id。
	SessionID int64
	// Role 谁说的：ChatRoleUser / ChatRoleAssistant。
	Role string
	// Content 消息正文。
	Content string
	// AssetUIDs 消息引用的商品图 uid 数组（designkit_assets.uid）。
	// SQL 里是 JSONB，repository 负责编解码；没有引用就是空切片。
	AssetUIDs []string
	// RequestID 关联的计费编号；没有就空串。
	RequestID string
	// CreatedAt 发出时间。
	CreatedAt time.Time
}
