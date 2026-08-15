//go:build unit

package repository

// AI 对话查询的守卫。跟本包其他测试一样：只测拼出来的 SQL，不连数据库。
// mustContain / mustNotContain 复用 query_build_test.go 的，
// parseTableColumns 复用 columns_parity_test.go 的，不要再定义一遍。

import (
	"os"
	"path/filepath"
	"testing"
)

// ---- 列清单与迁移逐列比对 ----

const chatMigrationRelPath = "../../../migrations/9002_designkit_chat.sql"

func loadChatMigration(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(chatMigrationRelPath))
	if err != nil {
		t.Fatalf("读不到对话建表 SQL: %v", err)
	}
	return string(raw)
}

func TestChatColumnListsMatchMigration(t *testing.T) {
	migration := loadChatMigration(t)

	// 会话：chatSessionColumns 刻意不含 deleted_at（实体里没有这个字段，
	// 查询一律只取未删的）。所以比对时先确认迁移里最后一列确实是 deleted_at，
	// 再拿前面的列跟代码逐个对。
	sessions := parseTableColumns(t, migration, "designkit_chat_sessions")
	if sessions[len(sessions)-1] != "deleted_at" {
		t.Fatalf("会话表最后一列应该是 deleted_at，实际 %q", sessions[len(sessions)-1])
	}
	want := sessions[:len(sessions)-1]
	got := splitColumns(chatSessionColumns)
	if len(got) != len(want) {
		t.Fatalf("会话列数对不上: 代码里 %d 列 %v，迁移里 %d 列 %v",
			len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("会话第 %d 列对不上: 代码里是 %q，迁移里是 %q", i+1, got[i], want[i])
		}
	}

	// 消息：全量一致。
	wantMsg := parseTableColumns(t, migration, "designkit_chat_messages")
	gotMsg := splitColumns(chatMessageColumns)
	if len(gotMsg) != len(wantMsg) {
		t.Fatalf("消息列数对不上: 代码里 %d 列 %v，迁移里 %d 列 %v",
			len(gotMsg), gotMsg, len(wantMsg), wantMsg)
	}
	for i := range wantMsg {
		if gotMsg[i] != wantMsg[i] {
			t.Fatalf("消息第 %d 列对不上: 代码里是 %q，迁移里是 %q", i+1, gotMsg[i], wantMsg[i])
		}
	}
}

// ---- 消息顺序 ----

func TestListChatMessagesOrdersByIDAscending(t *testing.T) {
	// 对话顺序就是 id 顺序。少了显式 ORDER BY，PostgreSQL 不保证返回顺序 ——
	// 界面上「问在答后面」这种错查起来极难（CLAUDE.md：顺序必须显式）。
	mustContain(t, listChatMessagesSQL,
		"WHERE session_id = $1",
		"ORDER BY id ASC",
	)
	// 拉的是一个会话的全量，不分页；出现 OFFSET 说明有人改成了分页却没改契约。
	mustNotContain(t, listChatMessagesSQL, "OFFSET")
}

// ---- 会话归属与软删过滤 ----

func TestGetChatSessionByUIDFiltersOwnerAndDeleted(t *testing.T) {
	// 归属过滤必须在 SQL 里做：查别人的会话和查不存在的会话要**同样**返回
	// ErrNotFound。少了 user_id 条件，任何登录用户拿到 uid 就能翻别人的对话。
	// 少了 deleted_at 条件，删掉的会话还能打开。
	mustContain(t, getChatSessionByUIDSQL,
		"uid = $1",
		"AND user_id = $2",
		"AND deleted_at IS NULL",
	)
}

func TestListChatSessionsOrdersByUpdatedAtDesc(t *testing.T) {
	mustContain(t, listChatSessionsSQL,
		"WHERE user_id = $1",
		"AND deleted_at IS NULL",
		// 最近聊的排最前；id DESC 是并列裁决，同一毫秒建的两个会话顺序才稳定。
		"ORDER BY updated_at DESC, id DESC",
	)
}

func TestSoftDeleteChatSessionGuards(t *testing.T) {
	// 三个条件缺一不可：uid 定位、本人校验、只删未删的
	// （重复删要报「找不到」，靠 deleted_at IS NULL 让影响行数归 0）。
	mustContain(t, softDeleteChatSessionSQL,
		"SET deleted_at = NOW()",
		"WHERE uid = $1 AND user_id = $2 AND deleted_at IS NULL",
	)
}

// ---- TouchSession 的两个分支 ----

func TestTouchChatSessionEmptyTitleOnlyBumpsUpdatedAt(t *testing.T) {
	// title 空串 = 「没起名」不是「改名成空」。这一支要是碰了 title，
	// 每插一条消息都会把已经起好的标题抹成空。
	sqlText, args := buildTouchChatSessionQuery(7, "")
	mustContain(t, sqlText,
		"SET updated_at = NOW()",
		"WHERE id = $1 AND deleted_at IS NULL",
	)
	mustNotContain(t, sqlText, "title")
	if len(args) != 1 || args[0] != int64(7) {
		t.Fatalf("空 title 分支只该带一个参数（sessionID）: %v", args)
	}
}

func TestTouchChatSessionWithTitleUpdatesBoth(t *testing.T) {
	sqlText, args := buildTouchChatSessionQuery(7, "夏季主图")
	mustContain(t, sqlText,
		"SET title = $2, updated_at = NOW()",
		"WHERE id = $1 AND deleted_at IS NULL",
	)
	if len(args) != 2 || args[0] != int64(7) || args[1] != "夏季主图" {
		t.Fatalf("带 title 分支的参数不对: %v", args)
	}
}

// ---- JSONB 编解码 ----

func TestChatAssetUIDsEncodeNilAsEmptyArray(t *testing.T) {
	// 列是 NOT NULL：nil 必须写成 []，写 null 会当场被约束拒掉。
	raw, err := encodeChatAssetUIDs(nil)
	if err != nil {
		t.Fatalf("编码 nil 不该报错: %v", err)
	}
	if string(raw) != "[]" {
		t.Fatalf("nil 应该编码成 []，实际 %q", raw)
	}
}

func TestChatAssetUIDsRoundTrip(t *testing.T) {
	in := []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV", "01BX5ZZKBKACTAV9WEVGEMMVS0"}
	raw, err := encodeChatAssetUIDs(in)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	out, err := decodeChatAssetUIDs(raw)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if len(out) != len(in) || out[0] != in[0] || out[1] != in[1] {
		t.Fatalf("编解码不对称: 进 %v 出 %v", in, out)
	}

	// 历史数据/手工改库可能给空，兜住不炸。
	if got, err := decodeChatAssetUIDs(nil); err != nil || got != nil {
		t.Fatalf("空列应该解成 nil 且不报错: %v, %v", got, err)
	}
}
