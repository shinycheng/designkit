package domain

// ============================================================================
// 用户记录（管理端视图）
// ============================================================================
//
// 管理员跨用户查看「AI 对话」和「出图批次」用的只读视图。
// 形态照 QuotaRequestDetail（entity.go 第 10 节）：实体本体 + JOIN 上游 users
// 补出来的邮箱（只读，绝不写 users）。
//
// 两条口径（跟 handler / repository 三层一致，别在任何一层放松）：
//   - **软删过的一律不出现**：会话按 deleted_at IS NULL、批次按
//     user_deleted_at IS NULL 过滤 —— 跟用户自己看到的一致，
//     别把人家已经删掉的记录翻出来。
//   - 账号已删时邮箱是空串（界面显示「已删除的账号」），记录本身照样列出来。

// RecordUser 是「有记录的账户」下拉框里的一项。
//
// **这里暴露上游 users 的数字 id**：管理端要拿它当筛选参数
// （?user_id=），跟额度申请管理页暴露自增 id 是同一个惯例。
// 运营侧接口仍然不暴露任何数字主键。
type RecordUser struct {
	// ID 上游 users.id。
	ID int64
	// Email 账号邮箱；账号已删时是空串。
	Email string
	// SessionCount 这个账户未删的对话会话数。
	SessionCount int
	// JobCount 这个账户未删的出图批次数。
	JobCount int
}

// ChatSessionAdminView 是管理端看到的一段对话：本体 + 归属人邮箱 + 消息条数。
type ChatSessionAdminView struct {
	ChatSession
	// UserEmail 归属人邮箱；账号已删时是空串。
	UserEmail string
	// MessageCount 会话里的消息条数（user + assistant 都算）。
	MessageCount int
}

// JobAdminView 是管理端看到的一个批次：本体 + 归属人邮箱。
type JobAdminView struct {
	Job
	// UserEmail 归属人邮箱；账号已删时是空串。
	UserEmail string
}

// JobItemAdminView 是管理端批次详情里的一张：本体 + 有没有出好的图。
type JobItemAdminView struct {
	JobItem
	// HasImage 这一张当前版本有没有结果图（软删的图不算）。
	// 前端靠它决定要不要去取缩略图 —— 没图就别发那个必然 404 的请求。
	HasImage bool
}
