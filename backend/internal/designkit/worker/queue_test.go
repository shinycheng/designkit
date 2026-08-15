//go:build unit

package worker

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/require"
)

// TestClaimNextItem_OnlyOneWorkerWins 并发领取同一张图，只能有一个拿到。
//
// 拿到两次的后果是**同一张图出两遍、上游收两次钱**，而运营那边只看到一张图。
// 真实的保护是 `FOR UPDATE SKIP LOCKED`，这里守的是这条语义本身。
func TestClaimNextItem_OnlyOneWorkerWins(t *testing.T) {
	f := newFixture(t, 1, nil)

	const workers = 20
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			workerID := "w-" + string(rune('a'+idx%26))
			<-start
			item, err := f.repo.ClaimNextItem(context.Background(), dkdomain.ClaimItemParams{
				WorkerID: workerID,
				LeaseFor: 3 * time.Second,
			})
			if err == nil && item != nil {
				mu.Lock()
				winners = append(winners, workerID)
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	require.Len(t, winners, 1, "同一张图只能被一个 worker 领走")
	require.Equal(t, 1, f.repo.item(1).AttemptCount, "attempt_count 只能 +1 一次")
}

// TestClaimNextItem_ExpiredLeaseIsTakenOver 租约过期后别人能接手，
// 而原来那个 worker 的写回会被丢弃。
func TestClaimNextItem_ExpiredLeaseIsTakenOver(t *testing.T) {
	f := newFixture(t, 1, nil)
	ctx := context.Background()

	first, err := f.repo.ClaimNextItem(ctx, dkdomain.ClaimItemParams{WorkerID: "worker-a", LeaseFor: 3 * time.Second})
	require.NoError(t, err)
	require.Equal(t, 1, first.AttemptCount)

	// 租约没过期时别人领不走。
	_, err = f.repo.ClaimNextItem(ctx, dkdomain.ClaimItemParams{WorkerID: "worker-b", LeaseFor: 3 * time.Second})
	require.ErrorIs(t, err, dkdomain.ErrNotFound)

	// 进程被杀，租约到期。
	f.clock.Advance(4 * time.Second)

	second, err := f.repo.ClaimNextItem(ctx, dkdomain.ClaimItemParams{WorkerID: "worker-b", LeaseFor: 3 * time.Second})
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID, "接手的应该是同一张图")
	require.Equal(t, 2, second.AttemptCount, "重新领取要再 +1，否则崩一次就会被永远重试")

	// worker-a 醒过来写回：守卫不命中，必须被拒绝。
	err = f.repo.CompleteItemSuccess(ctx, dkdomain.CompleteItemSuccessParams{
		ItemID:   first.ID,
		WorkerID: "worker-a",
		Attempt:  1,
	})
	require.ErrorIs(t, err, dkdomain.ErrLeaseLost)
	require.Equal(t, 0, f.repo.imageCount(first.ID), "租约丢了就不能写进任何图")
}

// TestClaimNextItem_PendingWithoutBudgetIsNeverClaimed
// pending 支的次数守卫：已经试满的 item 绝不能被领出来。
//
// 这是「万一哪天有人写出『把试满的 item 退回 pending』的代码」的第二道防线
// （第一道在 worker 领到手那一刻）。守卫在这里 = 那份代码花不掉钱。
//
// ⚠ 代价是造出一个死结：这行 item 从此永远领不出来、永远到不了终态，
// 而心跳按 status IN ('pending','running') 算，会让整批永远躲过僵尸巡检。
// 拆结的是 repository 的 reapDeadPendingItems（每分钟把这类 item 直接判死）。
// 改这条断言之前先去看那一段。
func TestClaimNextItem_PendingWithoutBudgetIsNeverClaimed(t *testing.T) {
	f := newFixture(t, 1, nil)

	f.repo.mu.Lock()
	f.repo.items[1].Status = dkdomain.ItemStatusPending
	f.repo.items[1].AttemptCount = dkdomain.DefaultMaxAttempts
	f.repo.items[1].MaxAttempts = dkdomain.DefaultMaxAttempts
	f.repo.mu.Unlock()

	_, err := f.repo.ClaimNextItem(context.Background(),
		dkdomain.ClaimItemParams{WorkerID: "w1", LeaseFor: 3 * time.Second})
	require.ErrorIs(t, err, dkdomain.ErrNotFound,
		"试满次数的 pending item 领出来就是再出一张图、再扣一次钱")
	require.Equal(t, 0, f.gw.callCount())
	require.Equal(t, dkdomain.DefaultMaxAttempts, f.repo.item(1).AttemptCount,
		"没领走就不该 +1")
}

// TestHandleItem_Success 走一遍完整链路：预处理 → 出图 → 存图 → 写回 → 收尾。
func TestHandleItem_Success(t *testing.T) {
	f := newFixture(t, 1, nil)
	ctx := context.Background()

	item, err := f.repo.ClaimNextItem(ctx, dkdomain.ClaimItemParams{WorkerID: "w1", LeaseFor: 3 * time.Second})
	require.NoError(t, err)

	f.pool.handleItem(ctx, "w1", item)

	stored := f.repo.item(item.ID)
	require.Equal(t, dkdomain.ItemStatusSucceeded, stored.Status)
	require.NotNil(t, stored.BillingRequestID)
	require.Equal(t,
		dkdomain.StoredBillingRequestID(testJobUID, 1, 1), *stored.BillingRequestID,
		"落库的必须是带 client: 前缀的完整值，否则结算时一条账单也查不到")
	require.LessOrEqual(t, len(*stored.BillingRequestID), dkdomain.UsageLogRequestIDMaxLen)

	// 发给上游的那个值**不带** client: 前缀，而且每张图各不相同。
	require.Equal(t, []string{dkdomain.BillingRequestID(testJobUID, 1, 1)}, f.gw.billingRequestIDs())

	require.Equal(t, 1, f.repo.imageCount(item.ID))
	require.Equal(t, 1, f.pre.callCount(), "第一次出图要真的调预处理")

	// 图片按定死的格式落进对象存储。
	wantKey := dkdomain.ImageObjectKey(f.clock.Now(), testJobUID, 1, 1, 1)
	require.True(t, f.store.has(wantKey), "结果图必须落在 %s", wantKey)

	// 发给网关的是**补过白边的**字节，不是原图。
	require.Len(t, f.gw.requests, 1)
	require.True(t, strings.HasPrefix(string(f.gw.requests[0].ImageData), "padded:"),
		"必须发预处理产物；发原图 = 运营选 3:4 拿回 4:3，钱照扣")
	require.Equal(t, dkdomain.Ratio3x4, f.gw.requests[0].Ratio)
	require.NotEmpty(t, f.gw.requests[0].Size)

	// 批次：holding → running，最后一张落地后转 settling（不是终态）。
	job := f.repo.job(1)
	require.Equal(t, dkdomain.JobStatusSettling, job.Status,
		"终态要等结算 worker 对完账才写；立刻汇总会因为 usage_logs 是异步写的而少算")
	require.Equal(t, 1, job.SuccessCount)
	require.NotNil(t, job.FinishedAt)
	require.Equal(t, 1, f.repo.finalizeWon, "只能有一个 worker 抢到收尾")

	require.Contains(t, f.repo.statusUpdates,
		statusUpdate{jobID: 1, from: dkdomain.JobStatusHolding, to: dkdomain.JobStatusRunning})
}

// TestHandleItem_ReusesExistingVariant 已经有预处理产物时不再调 Python 服务。
func TestHandleItem_ReusesExistingVariant(t *testing.T) {
	f := newFixture(t, 1, nil)
	ctx := context.Background()

	// 预先放一个产物 + 对应的文件。
	key := "designkit/variants/2026/08/13/asset-1-3x4-o-2048.png"
	f.store.objects[key] = storedObject{data: []byte("padded:CACHED"), contentType: "image/png"}
	f.repo.variants[variantKey(101, dkdomain.Ratio3x4, false, dkdomain.DefaultMaxDimension)] = &dkdomain.AssetVariant{
		ID:               42,
		AssetID:          101,
		Ratio:            dkdomain.Ratio3x4,
		KeepTransparency: false,
		MaxDimension:     dkdomain.DefaultMaxDimension,
		ObjectKey:        key,
		ContentType:      "image/png",
	}

	item, err := f.repo.ClaimNextItem(ctx, dkdomain.ClaimItemParams{WorkerID: "w1", LeaseFor: 3 * time.Second})
	require.NoError(t, err)
	f.pool.handleItem(ctx, "w1", item)

	require.Equal(t, 0, f.pre.callCount(), "同参数的产物必须复用，不该再调一次预处理")
	require.Equal(t, dkdomain.ItemStatusSucceeded, f.repo.item(item.ID).Status)
	require.Equal(t, []byte("padded:CACHED"), f.gw.requests[0].ImageData)
}

// TestHandleItem_LeaseLostOnWriteBackDiscardsResult 写回时 worker_id 对不上就丢弃结果。
//
// 这是「同一张图被两个 worker 各出一遍」时唯一的止损点：
// 硬写会把两次结果混在一行上，而且 success_count 会被加两次。
func TestHandleItem_LeaseLostOnWriteBackDiscardsResult(t *testing.T) {
	f := newFixture(t, 1, nil)
	ctx := context.Background()

	item, err := f.repo.ClaimNextItem(ctx, dkdomain.ClaimItemParams{WorkerID: "w1", LeaseFor: 3 * time.Second})
	require.NoError(t, err)

	// 出图期间租约被别人抢走：写回时守卫不命中。
	f.repo.mu.Lock()
	f.repo.successErr = dkdomain.ErrLeaseLost
	f.repo.mu.Unlock()

	f.pool.handleItem(ctx, "w1", item)

	require.Equal(t, 0, f.repo.imageCount(item.ID), "结果必须被丢弃")
	require.Equal(t, 0, f.repo.job(1).SuccessCount, "success_count 不能被加")
	require.Equal(t, 0, f.repo.finalizeCalls, "丢弃结果之后不该去判收尾")
}

// TestHandleItem_RetryableFailureThenTerminal 可重试的错误先退避重排，
// 次数用完才判死。
func TestHandleItem_RetryableFailureThenTerminal(t *testing.T) {
	f := newFixture(t, 1, func(cfg *Config, _ *Deps) {
		cfg.RetryBackoffBase = 30 * time.Second
	})
	ctx := context.Background()

	// 上游并发排队：等一会儿就好，而且这种情况下图根本没出、不花钱。
	f.gw.handler = func(context.Context, dkdomain.GenerateRequest) (dkdomain.GenerateResult, error) {
		return dkdomain.GenerateResult{}, dkdomain.MapUpstreamError(429,
			"Image generation concurrency limit exceeded, please retry later")
	}
	f.repo.mu.Lock()
	f.repo.items[1].MaxAttempts = 2
	f.repo.mu.Unlock()

	item, err := f.repo.ClaimNextItem(ctx, dkdomain.ClaimItemParams{WorkerID: "w1", LeaseFor: 3 * time.Second})
	require.NoError(t, err)
	f.pool.handleItem(ctx, "w1", item)

	first := f.repo.item(1)
	require.Equal(t, dkdomain.ItemStatusPending, first.Status, "还能再试的要退回 pending，不是 failed")
	require.NotNil(t, first.LastErrorCode)
	require.Equal(t, dkdomain.ErrCodeUpstreamBusy, *first.LastErrorCode)
	require.True(t, first.AvailableAt.After(f.clock.Now()), "必须往后推 available_at 做退避")
	require.Equal(t, 0, f.repo.job(1).FailCount)
	require.Nil(t, first.WorkerID, "租约要还回去，别人才能接手")

	// 退避到期，再试一次 —— 这次用完了 max_attempts。
	f.clock.Advance(time.Minute)
	item2, err := f.repo.ClaimNextItem(ctx, dkdomain.ClaimItemParams{WorkerID: "w2", LeaseFor: 3 * time.Second})
	require.NoError(t, err)
	require.Equal(t, 2, item2.AttemptCount)
	f.pool.handleItem(ctx, "w2", item2)

	second := f.repo.item(1)
	require.Equal(t, dkdomain.ItemStatusFailed, second.Status)
	require.Equal(t, dkdomain.ErrCodeMaxAttemptsExceeded, *second.LastErrorCode,
		"次数用完要换成「重试到上限」，不能还写「稍后自动重试」")
	require.Equal(t, 1, f.repo.job(1).FailCount)
	require.Equal(t, dkdomain.JobStatusSettling, f.repo.job(1).Status,
		"一张都没成功也要走结算路径，否则冻结额永远挂着")
}

// TestHandleItem_ContentBlockedIsTerminalImmediately 内容审核拦截**一次都不重试**。
//
// 重试一次 = 重新出一张图 = 重新收一次钱，而三次的结果一模一样。
func TestHandleItem_ContentBlockedIsTerminalImmediately(t *testing.T) {
	f := newFixture(t, 1, nil)
	ctx := context.Background()

	f.gw.handler = func(context.Context, dkdomain.GenerateRequest) (dkdomain.GenerateResult, error) {
		return dkdomain.GenerateResult{}, dkdomain.MapUpstreamError(400,
			"Your request was rejected as a result of our safety system")
	}

	item, err := f.repo.ClaimNextItem(ctx, dkdomain.ClaimItemParams{WorkerID: "w1", LeaseFor: 3 * time.Second})
	require.NoError(t, err)
	f.pool.handleItem(ctx, "w1", item)

	got := f.repo.item(1)
	require.Equal(t, dkdomain.ItemStatusFailed, got.Status)
	require.Equal(t, dkdomain.ErrCodeContentBlocked, *got.LastErrorCode)
	require.Equal(t, 1, f.gw.callCount(), "只能调一次网关")
	require.Contains(t, *got.LastErrorMessage, "safety system",
		"上游英文原文只落 last_error_message 给管理员看")
}

// TestHandleItem_PreprocessFailureIsFailClosed 预处理失败绝不拿原图顶上去。
func TestHandleItem_PreprocessFailureIsFailClosed(t *testing.T) {
	f := newFixture(t, 1, nil)
	ctx := context.Background()
	f.pre.err = context.DeadlineExceeded

	item, err := f.repo.ClaimNextItem(ctx, dkdomain.ClaimItemParams{WorkerID: "w1", LeaseFor: 3 * time.Second})
	require.NoError(t, err)
	f.pool.handleItem(ctx, "w1", item)

	require.Equal(t, 0, f.gw.callCount(), "预处理失败就不许发给网关（fail-closed）")
	got := f.repo.item(1)
	require.Equal(t, dkdomain.ItemStatusFailed, got.Status)
	require.Equal(t, dkdomain.ErrCodePreprocessFailed, *got.LastErrorCode)
}

// TestHandleItem_StopRequestedDoesNotSpendMoney 已经点了「停止排队」的批次，
// 抢在竞态里被领出来的那一张也不许出图。
func TestHandleItem_StopRequestedDoesNotSpendMoney(t *testing.T) {
	f := newFixture(t, 1, nil)
	ctx := context.Background()

	requested := f.clock.Now()
	f.repo.mu.Lock()
	f.repo.jobs[1].CancelRequestedAt = &requested
	f.repo.mu.Unlock()

	item, err := f.repo.ClaimNextItem(ctx, dkdomain.ClaimItemParams{WorkerID: "w1", LeaseFor: 3 * time.Second})
	require.NoError(t, err)
	f.pool.handleItem(ctx, "w1", item)

	require.Equal(t, 0, f.gw.callCount(), "运营点停止的动机 100% 是「别再花钱」")
	require.Equal(t, dkdomain.ItemStatusFailed, f.repo.item(1).Status)
	require.Equal(t, dkdomain.ErrCodeCanceled, *f.repo.item(1).LastErrorCode)
}

// TestHandleItem_ExhaustedAttemptNeverCallsGateway 已经试满次数的僵尸 item
// 被重新领走之后，**一次网关都不许调**。
//
// 这是真实会烧钱的路径，不是假想：
// 领取 SQL 的僵尸分支（status='running' AND lease_expires_at < NOW()）
// **故意不看 attempt**（不看才能把崩溃残留的 running 行捞回来判死），
// 所以拦截点只能在 worker 这一侧。原来 worker 也没判 ——
// 于是租约每过期一次，这张图就被重新领走、真出一次图、真扣一次钱，
// 然后才在写回时被判 DK_MAX_ATTEMPTS_EXCEEDED。
func TestHandleItem_ExhaustedAttemptNeverCallsGateway(t *testing.T) {
	f := newFixture(t, 1, nil)
	ctx := context.Background()

	// 造一个「跑到最后一次尝试时进程被 SIGKILL」的残骸：
	// 停在 running、attempt 已经等于 max_attempts、租约早就过期。
	dead := "dead-worker"
	expired := f.clock.Now().Add(-time.Second)
	f.repo.mu.Lock()
	f.repo.items[1].Status = dkdomain.ItemStatusRunning
	f.repo.items[1].WorkerID = &dead
	f.repo.items[1].LeaseExpiresAt = &expired
	f.repo.items[1].AttemptCount = dkdomain.DefaultMaxAttempts
	f.repo.items[1].MaxAttempts = dkdomain.DefaultMaxAttempts
	f.repo.mu.Unlock()

	// 僵尸回收把它领了出来 —— 这一步是对的，不领就永远判不了死。
	item, err := f.repo.ClaimNextItem(ctx, dkdomain.ClaimItemParams{WorkerID: "w2", LeaseFor: 3 * time.Second})
	require.NoError(t, err)
	require.Equal(t, dkdomain.DefaultMaxAttempts+1, item.AttemptCount,
		"领取时照样 +1，所以拿到手的 attempt 已经越界")

	f.pool.handleItem(ctx, "w2", item)

	require.Equal(t, 0, f.gw.callCount(), "越界的尝试绝不能调网关 —— 调一次就是一张图的钱")
	require.Equal(t, 0, f.pre.callCount(), "连预处理都不该做")

	got := f.repo.item(1)
	require.Equal(t, dkdomain.ItemStatusFailed, got.Status, "必须落终态，不能再退回 pending")
	require.NotNil(t, got.LastErrorCode)
	require.Equal(t, dkdomain.ErrCodeMaxAttemptsExceeded, *got.LastErrorCode)

	require.Equal(t, 1, f.repo.job(1).FailCount)
	require.Equal(t, dkdomain.JobStatusSettling, f.repo.job(1).Status,
		"判死之后要接着判收尾，否则批次永远停在 running，冻结额一直占着")
	require.Equal(t, 1, f.repo.finalizeWon)
}

// TestReleaseLease_ExhaustedAttemptIsNotRequeued 停机交还租约时，
// 已经试满次数的那一张**不能**被退回队列。
//
// 原来 releaseLease 无条件写 Terminal=false / RetryAfter=0，谁都不看 attempt。
// 后果：每重启一次进程，已经试满 3 次的图就被立刻重领、再真出一次图、再扣一次钱。
// 滚动更新、OOM 重启都会踩到。
func TestReleaseLease_ExhaustedAttemptIsNotRequeued(t *testing.T) {
	f := newFixture(t, 1, nil)

	holder := "w1"
	expires := f.clock.Now().Add(3 * time.Second)
	f.repo.mu.Lock()
	f.repo.items[1].Status = dkdomain.ItemStatusRunning
	f.repo.items[1].WorkerID = &holder
	f.repo.items[1].LeaseExpiresAt = &expires
	f.repo.items[1].AttemptCount = dkdomain.DefaultMaxAttempts // 正在跑的就是最后一次
	f.repo.items[1].MaxAttempts = dkdomain.DefaultMaxAttempts
	f.repo.mu.Unlock()

	item := f.repo.item(1)
	f.pool.releaseLease(&item, testJobUID, holder, discardLogger())

	got := f.repo.item(1)
	require.Equal(t, dkdomain.ItemStatusFailed, got.Status,
		"次数用完的不能退回 pending —— 退回去就是重启一次多扣一次钱")
	require.Equal(t, dkdomain.ErrCodeMaxAttemptsExceeded, *got.LastErrorCode)
	require.Equal(t, 1, f.repo.job(1).FailCount)
	require.Equal(t, dkdomain.JobStatusSettling, f.repo.job(1).Status)
	require.Equal(t, 0, f.gw.callCount())
}

// TestReleaseLease_UnderBudgetStillRequeues 次数还没用完的照旧退回队列，
// 别把上一条修过头 —— 重启不该白白吃掉运营剩下的重试机会。
func TestReleaseLease_UnderBudgetStillRequeues(t *testing.T) {
	f := newFixture(t, 1, nil)
	ctx := context.Background()

	item, err := f.repo.ClaimNextItem(ctx, dkdomain.ClaimItemParams{WorkerID: "w1", LeaseFor: 3 * time.Second})
	require.NoError(t, err)
	require.Equal(t, 1, item.AttemptCount)

	f.pool.releaseLease(item, testJobUID, "w1", discardLogger())

	got := f.repo.item(1)
	require.Equal(t, dkdomain.ItemStatusPending, got.Status)
	require.Equal(t, dkdomain.ErrCodeStale, *got.LastErrorCode)
	require.Nil(t, got.WorkerID)
	require.False(t, got.AvailableAt.After(f.clock.Now()), "交还的租约不加退避，立刻可被领取")
	require.Equal(t, 0, f.repo.job(1).FailCount, "这不算一次失败")
}

// TestHandleItem_TimeoutIsTerminalAndDoesNotRequeue 超时**不进重试队列**。
//
// 超时那张多半上游已经出图、已经扣过钱，只是结果没回到我们手上
// （上游调用跟调用方连接是故意脱钩的）。自动重试 = 换一个新的 request_id =
// 上游再收一次钱，最坏一张图扣 3 份。要不要再花这笔钱，交给人决定。
func TestHandleItem_TimeoutIsTerminalAndDoesNotRequeue(t *testing.T) {
	f := newFixture(t, 1, nil)
	ctx := context.Background()

	f.gw.handler = func(context.Context, dkdomain.GenerateRequest) (dkdomain.GenerateResult, error) {
		return dkdomain.GenerateResult{}, context.DeadlineExceeded
	}

	item, err := f.repo.ClaimNextItem(ctx, dkdomain.ClaimItemParams{WorkerID: "w1", LeaseFor: 3 * time.Second})
	require.NoError(t, err)
	f.pool.handleItem(ctx, "w1", item)

	got := f.repo.item(1)
	require.Equal(t, dkdomain.ItemStatusFailed, got.Status,
		"超时必须落 failed；退回 pending 就会自动再出一次图、再扣一次钱")
	require.Equal(t, dkdomain.ErrCodeTimeout, *got.LastErrorCode,
		"错误码保持 DK_TIMEOUT，运营要能一眼看出「钱可能已经花了」")
	require.Equal(t, 1, f.gw.callCount(), "只能调一次网关")
	require.Equal(t, 1, f.repo.job(1).FailCount)
	require.Equal(t, dkdomain.JobStatusSettling, f.repo.job(1).Status)

	// 退避重排那条路一步都不能走：available_at 不许被往后推。
	require.False(t, got.AvailableAt.After(f.clock.Now()),
		"落终态就不该再排退避，available_at 被推说明走错分支了")
}

// TestPool_ProcessesEachItemExactlyOnce 起真正的 Pool 跑一批，
// 断言每张图只出一次、计费 id 互不相同、只有一个 worker 抢到收尾。
//
// 「一单 N 张扣 N 份钱」的前提就是 N 个互不相同的 request_id。
func TestPool_ProcessesEachItemExactlyOnce(t *testing.T) {
	const items = 6
	f := newFixture(t, items, func(cfg *Config, _ *Deps) {
		cfg.Concurrency = 3
	})

	require.NoError(t, f.pool.Start(context.Background()))
	t.Cleanup(func() { _ = f.pool.Stop(context.Background()) })

	waitFor(t, 5*time.Second, f.repo.allItemsTerminal)

	require.Equal(t, items, f.gw.callCount(), "每张图只能调一次网关")

	ids := f.gw.billingRequestIDs()
	unique := map[string]bool{}
	for _, id := range ids {
		require.False(t, unique[id], "计费 request_id 不能重复：重复的第 2 条起会被上游判成幂等，静默不扣钱")
		unique[id] = true
		require.LessOrEqual(t, len(dkdomain.UpstreamClientRequestIDPrefix+id), dkdomain.UsageLogRequestIDMaxLen)
	}
	require.Len(t, unique, items)

	job := f.repo.job(1)
	require.Equal(t, items, job.SuccessCount)
	require.Equal(t, dkdomain.JobStatusSettling, job.Status)
	require.Equal(t, 1, f.repo.finalizeWon, "回调只能发一次")

	require.NoError(t, f.pool.Stop(context.Background()))
}

// TestPool_StopReturnsLeaseToQueue 收到停止信号：不等图跑完，把租约还回队列。
//
// 上游给整个进程的优雅关闭窗口只有 5 秒，一张图要 20 秒 —— 等不完的。
// 还回去之后另一个副本（或重启后的自己）立刻就能接手。
func TestPool_StopReturnsLeaseToQueue(t *testing.T) {
	f := newFixture(t, 1, func(cfg *Config, _ *Deps) {
		cfg.Concurrency = 1
	})

	entered := make(chan struct{})
	var once sync.Once
	f.gw.handler = func(ctx context.Context, _ dkdomain.GenerateRequest) (dkdomain.GenerateResult, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done() // 出图卡住，直到 Stop 取消它
		return dkdomain.GenerateResult{}, ctx.Err()
	}

	require.NoError(t, f.pool.Start(context.Background()))

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("网关一直没被调用")
	}

	require.NoError(t, f.pool.Stop(context.Background()))

	got := f.repo.item(1)
	require.Equal(t, dkdomain.ItemStatusPending, got.Status, "在途的图要退回队列，不是判死")
	require.Nil(t, got.WorkerID, "worker_id 要清掉，别人才领得走")
	require.Nil(t, got.LeaseExpiresAt)
	require.NotNil(t, got.LastErrorCode)
	require.Equal(t, dkdomain.ErrCodeStale, *got.LastErrorCode)
	require.False(t, got.AvailableAt.After(f.clock.Now()), "交还的租约不该再加退避，立刻可被领取")
	require.Equal(t, 0, f.repo.job(1).FailCount, "这不算一次失败")
}

// TestPool_HeartbeatCoversInflightJobs 在途批次会被续心跳。
// 心跳断 10 分钟，那批任务会被巡检当成崩溃残骸判死。
func TestPool_HeartbeatCoversInflightJobs(t *testing.T) {
	f := newFixture(t, 1, func(cfg *Config, _ *Deps) {
		cfg.Concurrency = 1
		cfg.HeartbeatInterval = 10 * time.Millisecond
	})

	require.NoError(t, f.pool.Start(context.Background()))
	t.Cleanup(func() { _ = f.pool.Stop(context.Background()) })

	waitFor(t, 3*time.Second, f.repo.allItemsTerminal)

	f.repo.mu.Lock()
	touched := len(f.repo.heartbeats)
	f.repo.mu.Unlock()
	require.Positive(t, touched, "必须给在途批次续心跳")
	require.NotNil(t, f.repo.job(1).HeartbeatAt)
}

// TestHeartbeatTargets_MergesActiveJobs 排队中（还没轮到出图）的批次也要续心跳，
// 否则它会被误判成僵尸 —— 明明好好的，却被判死并退款。
func TestHeartbeatTargets_MergesActiveJobs(t *testing.T) {
	f := newFixture(t, 1, func(_ *Config, deps *Deps) {
		deps.ActiveJobIDs = func(context.Context) ([]int64, error) {
			return []int64{5, 3}, nil
		}
	})
	f.pool.inflight.add(9)
	f.pool.inflight.add(3)

	require.Equal(t, []int64{3, 5, 9}, f.pool.heartbeatTargets(context.Background()),
		"两边合并、去重、升序")
}

func TestMergeIDs(t *testing.T) {
	require.Equal(t, []int64{1, 2, 3}, mergeIDs([]int64{3, 1}, []int64{2, 1, 0, -4}))
	require.Empty(t, mergeIDs(nil, nil))
}

// TestTruncateWorkerID worker_id 列是 VARCHAR(64)，超长会让整条领取 UPDATE 报 22001，
// 表现为「一张图都领不出来」。
func TestTruncateWorkerID(t *testing.T) {
	long := strings.Repeat("x", 200) + "-7"
	got := truncateWorkerID(long)
	require.Len(t, got, 64)
	require.True(t, strings.HasSuffix(got, "-7"), "序号在尾部，要保住")
	require.Equal(t, "short-1", truncateWorkerID("short-1"))
}

// waitFor 轮询等条件成立。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时（%s）", timeout)
}
