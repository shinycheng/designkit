//go:build integration

package repository

// 首批真库集成测试（冒烟级）。四件事，每件都是假 repo 单测天生测不到的：
//
//  1. 9001~9003 迁移在真 PostgreSQL 全量跑通（表、列、部分唯一索引都真的在，
//     且再跑一遍 ApplyMigrations 是无害的——这就是进程每次启动的日常路径）；
//  2. chat_repo 真库往返：交错插消息后 ListMessages 的顺序只能来自
//     ORDER BY id ASC，不可能「碰巧对」；会话列表按最近活跃倒序；
//  3. prompt_repo 的 source 过滤真查：youmind / user+归属人 互不串；
//  4. 额度申请的 WHERE status='pending' 原子认领：16 个并发只放一个过。
//
// 数据管理约定（没有事务回滚，库是全包共享的）：
//   - 每个测试用 testSeq 造自己独享的 user_id / uid / 关键词，测试间互不可见；
//   - 自己插的行自己在 t.Cleanup 里删掉。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	upstreamrepo "github.com/Wei-Shaw/sub2api/internal/repository"
)

// testSeq 造进程内唯一的数字：user_id、uid、关键词后缀全从它出。
// 用 UnixNano 打底，跨一次测试进程也不会撞（容器每次都是全新库，本不必跨进程，
// 但多一层保险不花钱）。
var testSeq atomic.Int64

func init() { testSeq.Store(time.Now().UnixNano()) }

// testUID 造一个 26 字符的唯一编号，填 CHAR(26)/VARCHAR(32) 的 uid 列。
// 真 ULID 的字母表数据库不校验，唯一就够。
func testUID() string {
	return fmt.Sprintf("%026d", testSeq.Add(1))
}

// testUserID 每个测试要一个独享的 user_id（designkit 表对上游 users 无外键，
// 见 9001 文件头，所以不需要真的建用户）。
func testUserID() int64 {
	return testSeq.Add(1)
}

// ----------------------------------------------------------------------------
// ① 9001~9003 迁移
// ----------------------------------------------------------------------------

func TestDesignkitMigrationsOnRealPostgres(t *testing.T) {
	ctx := context.Background()

	// 三个迁移都以「迁移」的身份被记录过——不是表恰好在，
	// 而是 schema_migrations 里有它们的行（文件名即主键）。
	for _, filename := range []string{
		"9001_designkit_init.sql",
		"9002_designkit_chat.sql",
		"9003_designkit_quota_admin.sql",
	} {
		var applied bool
		require.NoError(t, integrationDB.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)`,
			filename).Scan(&applied))
		require.True(t, applied, "迁移 %s 没有被应用", filename)
	}

	// 14 张表全部真的建出来了（9001 的 12 张 + 9002 的 2 张）。
	for _, table := range []string{
		"designkit_assets",
		"designkit_asset_variants",
		"designkit_prompt_categories",
		"designkit_prompts",
		"designkit_jobs",
		"designkit_job_items",
		"designkit_images",
		"designkit_holds",
		"designkit_billing_alerts",
		"designkit_quota_requests",
		"designkit_sync_runs",
		"designkit_settings",
		"designkit_chat_sessions",
		"designkit_chat_messages",
	} {
		var exists bool
		require.NoError(t, integrationDB.QueryRowContext(ctx,
			`SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists))
		require.True(t, exists, "表 %s 不存在", table)
	}

	// 9003 的两列真的加上了（ALTER TABLE ADD COLUMN 跑过）。
	for _, column := range []string{"handle_note", "approved_amount"} {
		var exists bool
		require.NoError(t, integrationDB.QueryRowContext(ctx, `
SELECT EXISTS(
  SELECT 1 FROM information_schema.columns
  WHERE table_schema = 'public'
    AND table_name = 'designkit_quota_requests'
    AND column_name = $1
)`, column).Scan(&exists))
		require.True(t, exists, "designkit_quota_requests.%s 列不存在", column)
	}

	// 两个部分唯一索引在。ON CONFLICT（同步幂等）和「同人只能有一条 pending」
	// 全靠它们，缺了功能不会立刻报错，只会在并发时写重复。
	for _, index := range []string{
		"idx_designkit_prompts_source_ref_uq",
		"idx_designkit_quota_requests_pending_uq",
	} {
		var exists bool
		require.NoError(t, integrationDB.QueryRowContext(ctx, `
SELECT EXISTS(
  SELECT 1 FROM pg_indexes
  WHERE schemaname = 'public' AND indexname = $1
)`, index).Scan(&exists))
		require.True(t, exists, "索引 %s 不存在", index)
	}

	// 再跑一遍全量迁移必须无害通过——这就是「进程每次启动都跑迁移」的日常路径，
	// checksum 校验（CLAUDE.md B6）在这里被真的走到。
	require.NoError(t, upstreamrepo.ApplyMigrations(ctx, integrationDB),
		"重复应用迁移必须是无害的 no-op")
}

// ----------------------------------------------------------------------------
// ② chat_repo 真库往返
// ----------------------------------------------------------------------------

func TestChatRepoRoundTripOnRealPostgres(t *testing.T) {
	ctx := context.Background()
	repo := NewChatRepo(integrationDB)
	userID := testUserID()
	t.Cleanup(func() {
		// sessions -> messages 有 ON DELETE CASCADE，删会话就把消息带走。
		_, _ = integrationDB.ExecContext(context.Background(),
			`DELETE FROM designkit_chat_sessions WHERE user_id = $1`, userID)
	})

	sessionA, err := repo.CreateSession(ctx, userID, testUID(), "会话A")
	require.NoError(t, err)
	require.NotZero(t, sessionA.ID)
	require.Equal(t, userID, sessionA.UserID)
	require.Equal(t, "会话A", sessionA.Title)

	sessionB, err := repo.CreateSession(ctx, userID, testUID(), "会话B")
	require.NoError(t, err)

	// 两个会话交错插消息：A 的消息 id 被 B 的隔开（不连续），
	// ListMessages(A) 还能按对话顺序回来，只可能是 WHERE + ORDER BY id ASC 干的，
	// 不可能是「插入顺序碰巧连续」。
	assetUID := testUID()
	inserts := []struct {
		sessionID int64
		role      string
		content   string
		assets    []string
	}{
		{sessionA.ID, dkdomain.ChatRoleUser, "A1", []string{assetUID}},
		{sessionB.ID, dkdomain.ChatRoleUser, "B1", nil},
		{sessionA.ID, dkdomain.ChatRoleAssistant, "A2", nil},
		{sessionB.ID, dkdomain.ChatRoleAssistant, "B2", nil},
		{sessionA.ID, dkdomain.ChatRoleUser, "A3", nil},
	}
	for _, in := range inserts {
		created, err := repo.InsertMessage(ctx, &dkdomain.ChatMessage{
			SessionID: in.sessionID,
			Role:      in.role,
			Content:   in.content,
			AssetUIDs: in.assets,
		})
		require.NoError(t, err, "插入消息 %s", in.content)
		require.NotZero(t, created.ID)
	}

	messages, err := repo.ListMessages(ctx, sessionA.ID)
	require.NoError(t, err)
	require.Len(t, messages, 3)
	for i, want := range []string{"A1", "A2", "A3"} {
		require.Equal(t, want, messages[i].Content, "第 %d 条消息内容", i+1)
		if i > 0 {
			require.Greater(t, messages[i].ID, messages[i-1].ID, "id 必须严格递增")
		}
	}
	// JSONB 往返：商品图 uid 数组原样回来。
	require.Equal(t, []string{assetUID}, messages[0].AssetUIDs)
	require.Empty(t, messages[1].AssetUIDs)

	// 会话列表按最近活跃倒序：B 建得晚本来排前面，摸一下 A 之后 A 必须到最前。
	// NOW() 的精度是微秒，睡 10ms 保证 updated_at 严格可比，不靠 id 并列裁决。
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, repo.TouchSession(ctx, sessionA.ID, ""))

	sessions, err := repo.ListSessions(ctx, userID, 10, 0)
	require.NoError(t, err)
	require.Len(t, sessions, 2)
	require.Equal(t, sessionA.UID, sessions[0].UID, "摸过的会话必须排最前（ORDER BY updated_at DESC）")
	require.Equal(t, "会话A", sessions[0].Title, "title 空串的 touch 不许把已有标题抹掉")

	// 归属过滤在 SQL 里：别人查这个 uid 必须是「找不到」，不能是「没权限」。
	stranger := testUserID()
	_, err = repo.GetSessionByUID(ctx, stranger, sessionA.UID)
	require.ErrorIs(t, err, dkdomain.ErrNotFound)

	// 软删：列表里消失、再删报「找不到」，但消息行保留。
	require.NoError(t, repo.SoftDeleteSession(ctx, userID, sessionB.UID))
	require.ErrorIs(t, repo.SoftDeleteSession(ctx, userID, sessionB.UID), dkdomain.ErrNotFound)

	sessions, err = repo.ListSessions(ctx, userID, 10, 0)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, sessionA.UID, sessions[0].UID)

	var remaining int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM designkit_chat_messages WHERE session_id = $1`,
		sessionB.ID).Scan(&remaining))
	require.Equal(t, 2, remaining, "软删会话必须保留消息行")
}

// ----------------------------------------------------------------------------
// ③ prompt_repo 的 source 过滤
// ----------------------------------------------------------------------------

func TestPromptRepoSourceFilterOnRealPostgres(t *testing.T) {
	ctx := context.Background()
	repo := NewRepository(integrationDB)

	// marker 是本测试独享的关键词：库是共享的，靠它把自己的三条捞出来。
	marker := fmt.Sprintf("dk-it-%d", testSeq.Add(1))
	owner := testUserID()
	other := testUserID()
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(),
			`DELETE FROM designkit_prompts WHERE title LIKE $1`, marker+"%")
	})

	create := func(source dkdomain.PromptSource, ownerID *int64, suffix string) *dkdomain.Prompt {
		t.Helper()
		p := &dkdomain.Prompt{
			UID:         testUID(),
			Title:       marker + suffix,
			Body:        "正文 " + suffix,
			Source:      source,
			OwnerUserID: ownerID,
			IsEnabled:   true,
		}
		require.NoError(t, repo.CreatePrompt(ctx, p))
		return p
	}
	shared := create(dkdomain.PromptSourceYouMind, nil, "-youmind")
	mine := create(dkdomain.PromptSourceUser, &owner, "-mine")
	others := create(dkdomain.PromptSourceUser, &other, "-others")

	// source=youmind：只有共享目录那条，运营自建的一条都不许混进来。
	srcYouMind := dkdomain.PromptSourceYouMind
	got, err := repo.ListPrompts(ctx, dkdomain.ListPromptsQuery{
		Keyword: marker, Source: &srcYouMind, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, shared.UID, got[0].UID)
	require.Equal(t, dkdomain.PromptSourceYouMind, got[0].Source)

	// source=user + 归属人：只看到自己那条——这是「运营 A 的自建词
	// 不许出现在运营 B 的列表里」的真库验证。
	srcUser := dkdomain.PromptSourceUser
	got, err = repo.ListPrompts(ctx, dkdomain.ListPromptsQuery{
		Keyword: marker, Source: &srcUser, OwnerUserID: &owner, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, mine.UID, got[0].UID)

	// 不带 source 过滤：三条全出，且按 id 升序（显式 ORDER BY 的真验）。
	got, err = repo.ListPrompts(ctx, dkdomain.ListPromptsQuery{Keyword: marker, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t,
		[]string{shared.UID, mine.UID, others.UID},
		[]string{got[0].UID, got[1].UID, got[2].UID},
		"必须按 id 升序（创建顺序）返回")

	// CountPrompts 跟列表同一套 WHERE：user 来源共 2 条（两个归属人各一条）。
	total, err := repo.CountPrompts(ctx, dkdomain.ListPromptsQuery{Keyword: marker, Source: &srcUser})
	require.NoError(t, err)
	require.Equal(t, 2, total)
}

// ----------------------------------------------------------------------------
// ④ 额度申请：WHERE status='pending' 的原子认领
// ----------------------------------------------------------------------------

func TestQuotaRequestClaimIsAtomicOnRealPostgres(t *testing.T) {
	ctx := context.Background()
	repo := NewRepository(integrationDB)
	userID := testUserID()
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(),
			`DELETE FROM designkit_quota_requests WHERE user_id = $1`, userID)
	})

	note := "余额不足，申请加额"
	request, err := repo.CreateQuotaRequest(ctx, userID, &note)
	require.NoError(t, err)
	require.Equal(t, dkdomain.QuotaRequestPending, request.Status)

	// 同一个人连点第二次：部分唯一索引挡下 → ErrConflict（防连点刷屏）。
	_, err = repo.CreateQuotaRequest(ctx, userID, nil)
	require.ErrorIs(t, err, dkdomain.ErrConflict)

	// 16 个「管理员」同时点通过。每个 HandleQuotaRequest 都是独立连接上的
	// 独立事务，输家的 UPDATE 会在行锁上等赢家提交、重评 WHERE status='pending'
	// 后打中 0 行——这正是「两个管理员同时点通过、钱只加一次」的唯一防线，
	// 假 repo 单测永远碰不到锁。
	const admins = 16
	approved := decimal.NewFromInt(10)

	start := make(chan struct{})
	var wg sync.WaitGroup
	winners := make(chan int64, admins)
	unexpected := make(chan error, admins)
	var conflicts atomic.Int64

	for i := 0; i < admins; i++ {
		adminID := int64(1000 + i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			amount := approved
			_, err := repo.HandleQuotaRequest(ctx, request.ID, adminID,
				dkdomain.QuotaRequestHandled, nil, &amount)
			switch {
			case err == nil:
				winners <- adminID
			case errors.Is(err, dkdomain.ErrConflict):
				conflicts.Add(1)
			default:
				unexpected <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(winners)
	close(unexpected)

	for err := range unexpected {
		require.NoError(t, err, "输家只允许拿到 ErrConflict")
	}
	var winnerIDs []int64
	for id := range winners {
		winnerIDs = append(winnerIDs, id)
	}
	require.Len(t, winnerIDs, 1, "必须恰好一个管理员认领成功")
	require.Equal(t, int64(admins-1), conflicts.Load(), "其余全部 ErrConflict")

	// 落库结果：状态 handled、处理人是赢家、金额只写了一次。
	var status string
	var handledBy int64
	var amount decimal.Decimal
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT status, handled_by, approved_amount FROM designkit_quota_requests WHERE id = $1`,
		request.ID).Scan(&status, &handledBy, &amount))
	require.Equal(t, "handled", status)
	require.Equal(t, winnerIDs[0], handledBy)
	require.True(t, amount.Equal(approved), "approved_amount 是 %s", amount)

	// 已处理的再处理 → ErrConflict（区别于「没这条」）。
	_, err = repo.HandleQuotaRequest(ctx, request.ID, 1,
		dkdomain.QuotaRequestHandled, nil, &approved)
	require.ErrorIs(t, err, dkdomain.ErrConflict)

	// 不存在的 → ErrNotFound。
	_, err = repo.HandleQuotaRequest(ctx, request.ID+1_000_000, 1,
		dkdomain.QuotaRequestHandled, nil, &approved)
	require.ErrorIs(t, err, dkdomain.ErrNotFound)
}
