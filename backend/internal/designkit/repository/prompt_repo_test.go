//go:build unit

package repository

// 灵感库查询的守卫。跟本包其他测试一样：只测拼出来的 SQL，不连数据库。

import (
	"fmt"
	"strings"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

func TestCountPromptsByCategorySQL(t *testing.T) {
	// 分类页一进去就要显示「电商主图 416」这样的条数。
	// 一次 GROUP BY 拿全部，不要按分类查 15 次。
	all := fmt.Sprintf(countPromptsByCategorySQL, "")
	enabled := fmt.Sprintf(countPromptsByCategorySQL, " AND is_enabled")

	for _, sqlText := range []string{all, enabled} {
		for _, want := range []string{
			"SELECT category_id, COUNT(*)",
			"FROM designkit_prompts",
			// 软删的不能算进条数：界面显示 416，点进去只有 400 条，没人说得清哪错了。
			"WHERE deleted_at IS NULL",
			"GROUP BY category_id",
			// 本包铁律：所有返回多行的查询都要显式 ORDER BY。
			"ORDER BY category_id ASC NULLS LAST",
		} {
			if !strings.Contains(sqlText, want) {
				t.Fatalf("这条统计 SQL 里少了 %q:\n%s", want, sqlText)
			}
		}
	}

	if strings.Contains(all, "is_enabled") {
		t.Fatalf("不过滤下架时不该出现 is_enabled:\n%s", all)
	}
	if !strings.Contains(enabled, "AND is_enabled") {
		t.Fatalf("要能只数没下架的:\n%s", enabled)
	}
	// 拼进去的只有上面那两个常量，没有任何外部输入 —— 一旦出现 $ 参数
	// 就说明有人把用户输入拼进了 SQL。
	if strings.Contains(all, "$") {
		t.Fatalf("这条 SQL 不该有参数占位符:\n%s", all)
	}
}

func TestPromptKeywordSearchHitsTitleAndBody(t *testing.T) {
	// 9001 迁移给 title 和 body 各建了一个 pg_trgm 索引。
	// 两列都用同一个参数 ILIKE，PostgreSQL 才能把两个索引做 bitmap OR；
	// 拼成 title || body 那种写法索引直接用不上，1.4 万行每次全表扫。
	sqlText, args := buildListPromptsQuery(dkdomain.ListPromptsQuery{Keyword: "白底"})
	if !strings.Contains(sqlText, "(title ILIKE $1 OR body ILIKE $1)") {
		t.Fatalf("关键词要同时搜标题和正文，而且共用一个参数:\n%s", sqlText)
	}
	if strings.Contains(sqlText, "||") {
		t.Fatalf("不要把 title 和 body 拼起来搜，那样用不上 trigram 索引:\n%s", sqlText)
	}
	if len(args) != 2 || args[0] != "%白底%" {
		t.Fatalf("关键词没拼成包含匹配: %v", args)
	}
}

func TestPromptKeywordEscapesWildcards(t *testing.T) {
	// 运营在搜索框里打一个 % 就等于「全表匹配」，搜出来的结果莫名其妙。
	sqlText, args := buildListPromptsQuery(dkdomain.ListPromptsQuery{Keyword: "50%_off"})
	if !strings.Contains(sqlText, "ILIKE") {
		t.Fatalf("必须用 ILIKE:\n%s", sqlText)
	}
	if args[0] != `%50\%\_off%` {
		t.Fatalf("通配符没转义: %v", args[0])
	}
}

func TestListPromptsAlwaysOrdersByIDAscending(t *testing.T) {
	// 游标就是按 id 往前推的（`AND id > $n`）。少了 ORDER BY 或者换成倒序，
	// 翻页会漏行或者无限循环。
	sqlText, _ := buildListPromptsQuery(dkdomain.ListPromptsQuery{CursorID: 10})
	if !strings.Contains(sqlText, "AND id > $1") {
		t.Fatalf("游标条件不对:\n%s", sqlText)
	}
	if !strings.Contains(sqlText, "ORDER BY id ASC") {
		t.Fatalf("必须按 id 升序，游标才推得动:\n%s", sqlText)
	}
	if strings.Contains(sqlText, "OFFSET") {
		t.Fatalf("不许用 OFFSET（持续写入时会漏数据）:\n%s", sqlText)
	}
}

func TestListPromptsFiltersBySource(t *testing.T) {
	// 「我的提示词」和共享目录靠 source 分开。少了这个条件，
	// 运营 A 存的词会混进运营 B 的灵感库列表 —— 自建词只有本人可见。
	src := dkdomain.PromptSourceUser
	owner := int64(9)
	sqlText, args := buildListPromptsQuery(dkdomain.ListPromptsQuery{
		Source:      &src,
		OwnerUserID: &owner,
	})
	if !strings.Contains(sqlText, "AND source = $1") {
		t.Fatalf("要能按来源过滤:\n%s", sqlText)
	}
	if !strings.Contains(sqlText, "AND owner_user_id = $2") {
		t.Fatalf("我的提示词必须带归属过滤:\n%s", sqlText)
	}
	if args[0] != "user" {
		t.Fatalf("来源参数不对: %v", args[0])
	}

	// 不传 Source 时不能凭空多出过滤条件（共享目录的过滤由 service 侧显式给）。
	// ⚠ 判据必须是「过滤子句」而不是裸字符串 "source"——SELECT 列表里本来就有
	// source / source_ref 两列，拿裸字符串判会永远失败。
	plain, _ := buildListPromptsQuery(dkdomain.ListPromptsQuery{})
	if strings.Contains(plain, "AND source = ") {
		t.Fatalf("没传 Source 不该出现 source 过滤条件:\n%s", plain)
	}
}

func TestSyncedPromptUpsertNeverTouchesUserPrompts(t *testing.T) {
	// 同步写入的 source 恒为 'youmind'，绝不会覆盖运营自己存的那些
	// （它们的 source_ref 是空的，部分唯一索引也不会命中）。
	if !strings.Contains(upsertSyncedPromptSQL, "'youmind'") {
		t.Fatalf("同步写入的来源必须钉死成 youmind:\n%s", upsertSyncedPromptSQL)
	}
	if strings.Contains(upsertSyncedPromptSQL, "owner_user_id") {
		t.Fatalf("同步进来的提示词没有归属人，不该碰这一列:\n%s", upsertSyncedPromptSQL)
	}
}
