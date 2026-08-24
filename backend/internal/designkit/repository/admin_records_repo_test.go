//go:build unit

package repository

// 「用户记录」（管理端）SQL 的守卫。手法照 errors_guard_test.go：
// 本机没有 Go 编译器也没有数据库，能不连库就验的东西必须验掉 ——
// 这些约束破了不会报错，只会让管理员看到错序 / 看到用户已删的记录。

import (
	"strings"
	"testing"
)

// adminRecordsAllSQL 收集本文件守护的全部 SQL。
func adminRecordsAllSQL() []string {
	return []string{
		adminRecordUsersSQL,
		buildAdminListChatSessionsSQL(true),
		buildAdminListChatSessionsSQL(false),
		getAdminChatSessionSQL,
		buildAdminListJobsSQL(true),
		buildAdminListJobsSQL(false),
		getAdminJobSQL,
		adminJobItemsSQL,
		getAdminAssetSQL,
	}
}

// 所有返回列表的查询必须显式 ORDER BY（本包铁律 1）。
func TestAdminRecordsEveryListHasOrderBy(t *testing.T) {
	for _, s := range []string{
		adminRecordUsersSQL,
		buildAdminListChatSessionsSQL(true),
		buildAdminListChatSessionsSQL(false),
		buildAdminListJobsSQL(true),
		buildAdminListJobsSQL(false),
		adminJobItemsSQL,
	} {
		if !strings.Contains(s, "ORDER BY") {
			t.Fatalf("这条列表查询没有显式 ORDER BY:\n%s", s)
		}
	}
}

// 会话列表按 updated_at 降序（最近聊的排最前），id 降序做并列裁决。
func TestAdminChatSessionsOrderIsUpdatedAtDesc(t *testing.T) {
	for _, filter := range []bool{true, false} {
		s := buildAdminListChatSessionsSQL(filter)
		if !strings.Contains(s, "ORDER BY s.updated_at DESC, s.id DESC") {
			t.Fatalf("会话列表必须按 updated_at DESC（并列拿 id 裁决）:\n%s", s)
		}
	}
}

// 批次列表按 created_at 降序（最新提交的排最前），id 降序做并列裁决。
func TestAdminJobsOrderIsCreatedAtDesc(t *testing.T) {
	for _, filter := range []bool{true, false} {
		s := buildAdminListJobsSQL(filter)
		if !strings.Contains(s, "ORDER BY j.created_at DESC, j.id DESC") {
			t.Fatalf("批次列表必须按 created_at DESC（并列拿 id 裁决）:\n%s", s)
		}
	}
}

// item 一律 ORDER BY seq ASC —— 跟用户侧同一条契约。
func TestAdminJobItemsOrderIsSeqAsc(t *testing.T) {
	if !strings.Contains(adminJobItemsSQL, "ORDER BY it.seq ASC") {
		t.Fatalf("批次详情的 item 必须按 seq 升序:\n%s", adminJobItemsSQL)
	}
}

// 软删过滤：管理员看到的跟用户自己看到的一致，**不把删掉的翻出来**。
// 会话按 deleted_at IS NULL、批次按 user_deleted_at IS NULL，列表和详情都要有。
func TestAdminRecordsFilterSoftDeleted(t *testing.T) {
	for _, s := range []string{
		buildAdminListChatSessionsSQL(true),
		buildAdminListChatSessionsSQL(false),
		getAdminChatSessionSQL,
	} {
		if !strings.Contains(s, "s.deleted_at IS NULL") {
			t.Fatalf("会话查询必须过滤软删（deleted_at IS NULL）:\n%s", s)
		}
	}
	for _, s := range []string{
		buildAdminListJobsSQL(true),
		buildAdminListJobsSQL(false),
		getAdminJobSQL,
	} {
		if !strings.Contains(s, "j.user_deleted_at IS NULL") {
			t.Fatalf("批次查询必须过滤用户软删（user_deleted_at IS NULL）:\n%s", s)
		}
	}
	// 「有记录的账户」的两个来源子查询同样只数未删的。
	if !strings.Contains(adminRecordUsersSQL, "deleted_at IS NULL") ||
		!strings.Contains(adminRecordUsersSQL, "user_deleted_at IS NULL") {
		t.Fatalf("账户清单的两个来源子查询必须只数未删记录:\n%s", adminRecordUsersSQL)
	}
	// 对话附图同理：素材被主人删了，管理员这边也不给。
	if !strings.Contains(getAdminAssetSQL, "deleted_at IS NULL") {
		t.Fatalf("对话附图查询必须过滤软删（deleted_at IS NULL）:\n%s", getAdminAssetSQL)
	}
}

// user_id 筛选只在 filterByUser=true 时出现；单条详情**不带**归属条件
// （管理员通道，刻意不做归属校验 —— RequireAdmin 在路由层挡普通用户）。
func TestAdminRecordsUserFilterPlacement(t *testing.T) {
	if !strings.Contains(buildAdminListChatSessionsSQL(true), "s.user_id = $3") {
		t.Fatal("带筛选的会话列表少了 user_id 条件")
	}
	if strings.Contains(buildAdminListChatSessionsSQL(false), "s.user_id = $") {
		t.Fatal("不带筛选的会话列表不该有 user_id 条件（省略 = 全部账户）")
	}
	if !strings.Contains(buildAdminListJobsSQL(true), "j.user_id = $3") {
		t.Fatal("带筛选的批次列表少了 user_id 条件")
	}
	if strings.Contains(buildAdminListJobsSQL(false), "j.user_id = $") {
		t.Fatal("不带筛选的批次列表不该有 user_id 条件（省略 = 全部账户）")
	}
	// WHERE 里出现 user_id = 就是有人把归属校验加回来了（LEFT JOIN u.id = 那条不算）。
	for _, s := range []string{getAdminChatSessionSQL, getAdminJobSQL, getAdminAssetSQL} {
		if strings.Contains(s, "user_id = $2") {
			t.Fatalf("管理端详情不做归属过滤（这是管理员通道），别把 user_id 条件加回来:\n%s", s)
		}
	}
}

// has_image 的判据必须跟取图那条路一致：当前版本 + 未软删。
// 不一致会出现「列表说有图、点开取不到」。
func TestAdminJobItemsHasImagePredicateMatchesContentPath(t *testing.T) {
	if !strings.Contains(adminJobItemsSQL, "i.is_current") ||
		!strings.Contains(adminJobItemsSQL, "i.deleted_at IS NULL") {
		t.Fatalf("has_image 必须只认当前版本、未软删的图:\n%s", adminJobItemsSQL)
	}
	contentSQL := buildListImagesByItemSQL(true)
	if !strings.Contains(contentSQL, "is_current") || !strings.Contains(contentSQL, "deleted_at IS NULL") {
		t.Fatalf("取图那条路的判据变了，has_image 会跟它对不上:\n%s", contentSQL)
	}
}

// JOIN 上游 users 只读邮箱：LEFT JOIN + COALESCE（账号已删时行照给、邮箱空串），
// 且绝不写 users。
func TestAdminRecordsUsersJoinIsReadOnly(t *testing.T) {
	for _, s := range adminRecordsAllSQL() {
		upper := strings.ToUpper(s)
		for _, verb := range []string{"UPDATE USERS", "INSERT INTO USERS", "DELETE FROM USERS"} {
			if strings.Contains(upper, verb) {
				t.Fatalf("管理端记录查询绝不能写 users 表:\n%s", s)
			}
		}
	}
	for _, s := range []string{
		buildAdminListChatSessionsSQL(false),
		getAdminChatSessionSQL,
		buildAdminListJobsSQL(false),
		getAdminJobSQL,
		adminRecordUsersSQL,
	} {
		if !strings.Contains(s, "LEFT JOIN users") || !strings.Contains(s, "COALESCE(u.email, '')") {
			t.Fatalf("邮箱必须 LEFT JOIN + COALESCE：账号删了记录也要列出来（邮箱空串）:\n%s", s)
		}
	}
}

// 分页封顶跟对外契约对齐：limit 默认 50、封顶 200。
func TestAdminRecordsLimitContract(t *testing.T) {
	if AdminRecordsDefaultLimit != 50 || AdminRecordsMaxLimit != 200 {
		t.Fatalf("分页契约是「默认 50、封顶 200」，现在是 %d / %d —— 改之前先改对外文档和 handler",
			AdminRecordsDefaultLimit, AdminRecordsMaxLimit)
	}
}
