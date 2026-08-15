//go:build unit

package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/require"
)

// TestLoadSettings_ReadsFromDatabase 并发数、单张超时、最长边**以数据库为准**。
func TestLoadSettings_ReadsFromDatabase(t *testing.T) {
	clock := newFakeClock(time.Now())
	repo := newFakeRepo(clock.Now)
	repo.settings[dkdomain.SettingKeyWorkerConcurrency] = json.RawMessage(`3`)
	repo.settings[dkdomain.SettingKeyItemTimeoutSeconds] = json.RawMessage(`240`)
	repo.settings[dkdomain.SettingKeyMaxDimension] = json.RawMessage(`4096`)

	got := LoadSettings(context.Background(), repo, discardLogger())

	require.Equal(t, 3, got.Concurrency)
	require.Equal(t, 240*time.Second, got.ItemTimeout)
	require.Equal(t, 4096, got.MaxDimension)
}

// TestLoadSettings_Defaults 读不到就用默认值。
//
// 默认并发是 **4 不是 5**：上游每用户默认放 5 个并发，
// 占满会让运营在界面上的单张操作排队（超出的排队 25 个、等 30 秒，再多直接 429）。
func TestLoadSettings_Defaults(t *testing.T) {
	clock := newFakeClock(time.Now())
	repo := newFakeRepo(clock.Now)

	got := LoadSettings(context.Background(), repo, discardLogger())

	require.Equal(t, dkdomain.DefaultWorkerConcurrency, got.Concurrency)
	require.Equal(t, 4, got.Concurrency)
	require.Equal(t, time.Duration(dkdomain.DefaultItemTimeoutSeconds)*time.Second, got.ItemTimeout)
	require.Equal(t, dkdomain.DefaultMaxDimension, got.MaxDimension)
}

// TestLoadSettings_BadValuesFallBack 配置值坏掉不能让整个出图队列起不来。
func TestLoadSettings_BadValuesFallBack(t *testing.T) {
	clock := newFakeClock(time.Now())
	repo := newFakeRepo(clock.Now)
	repo.settings[dkdomain.SettingKeyWorkerConcurrency] = json.RawMessage(`"很多"`) // 不是整数
	repo.settings[dkdomain.SettingKeyItemTimeoutSeconds] = json.RawMessage(`-5`)  // 负数
	repo.settings[dkdomain.SettingKeyMaxDimension] = json.RawMessage(`999999`)    // 离谱的大

	got := LoadSettings(context.Background(), repo, discardLogger())

	require.Equal(t, dkdomain.DefaultWorkerConcurrency, got.Concurrency)
	require.Equal(t, 10*time.Second, got.ItemTimeout, "越界要收敛到下限，不是直接用负数")
	require.Equal(t, 8192, got.MaxDimension, "越界要收敛到上限")
}

// TestLoadSettings_ConcurrencyClampedToPoolSize 并发不能超过我们自己连接池的规模。
func TestLoadSettings_ConcurrencyClampedToPoolSize(t *testing.T) {
	clock := newFakeClock(time.Now())
	repo := newFakeRepo(clock.Now)
	repo.settings[dkdomain.SettingKeyWorkerConcurrency] = json.RawMessage(`100`)

	got := LoadSettings(context.Background(), repo, discardLogger())
	require.Equal(t, MaxConcurrency, got.Concurrency)
}

// TestConfigDefaults_LeaseRenewMustBeShorterThanLease 续租间隔必须真的小于租约。
//
// 反过来的话每次都是「租约刚过期才去续」，等于没有租约保护 ——
// 同一张图会被两个 worker 各出一遍，也就是收两次钱。
func TestConfigDefaults_LeaseRenewMustBeShorterThanLease(t *testing.T) {
	got := Config{LeaseFor: 30 * time.Second, LeaseRenewInterval: 60 * time.Second}.withDefaults()
	require.Less(t, got.LeaseRenewInterval, got.LeaseFor)
	require.Equal(t, 10*time.Second, got.LeaseRenewInterval)

	zero := Config{}.withDefaults()
	require.Equal(t, time.Duration(dkdomain.DefaultLeaseSeconds)*time.Second, zero.LeaseFor)
	require.Equal(t, time.Duration(dkdomain.DefaultLeaseRenewSeconds)*time.Second, zero.LeaseRenewInterval)
	require.Equal(t, time.Duration(dkdomain.DefaultHeartbeatSeconds)*time.Second, zero.HeartbeatInterval)
	require.Equal(t, time.Duration(dkdomain.DefaultStaleJobMinutes)*time.Minute, zero.StaleAfter)
	require.Less(t, zero.ShutdownWait, 5*time.Second,
		"上游给整个进程的优雅关闭窗口只有 5 秒，等超了等于白等")
}

// TestNew_RequiresDependencies 少依赖要当场报错，而不是跑起来之后空指针。
func TestNew_RequiresDependencies(t *testing.T) {
	_, err := New(Config{}, Deps{})
	require.ErrorIs(t, err, ErrMissingDependency)

	clock := newFakeClock(time.Now())
	_, err = New(Config{}, Deps{
		Repo:         newFakeRepo(clock.Now),
		Store:        newFakeStore(),
		Preprocessor: &fakePreprocessor{},
	})
	require.ErrorIs(t, err, ErrMissingDependency)

	pool, err := New(Config{}, Deps{
		Repo:         newFakeRepo(clock.Now),
		Store:        newFakeStore(),
		Preprocessor: &fakePreprocessor{},
		Gateway:      &fakeGateway{},
		Logger:       discardLogger(),
	})
	require.NoError(t, err)
	require.NotNil(t, pool)
}

// TestStart_Twice 起两次要报错（否则并发数会翻倍，静默超出上游的并发上限）。
func TestStart_Twice(t *testing.T) {
	f := newFixture(t, 1, nil)
	require.NoError(t, f.pool.Start(context.Background()))
	t.Cleanup(func() { _ = f.pool.Stop(context.Background()) })
	require.Error(t, f.pool.Start(context.Background()))
}

// TestWriteBackBudget_NotStopping 没在停机时就是配置的那个值，一点不缩。
func TestWriteBackBudget_NotStopping(t *testing.T) {
	f := newFixture(t, 1, func(cfg *Config, _ *Deps) {
		cfg.WriteBackTimeout = 10 * time.Second
	})
	require.Equal(t, 10*time.Second, f.pool.writeBackBudget())
}

// TestWriteBackBudget_ClampedToStopBudget 停机时写回预算必须缩进 Stop 的预算内。
//
// 原来错在哪：Module.Close 只给 worker.Stop 3 秒，而真正交还租约的那条 UPDATE
// 用的是 context.WithTimeout(context.Background(), WriteBackTimeout=10s) ——
// **既不继承 3 秒预算、也不会被 Stop 取消**。数据库稍慢一点就是：
// Stop 3 秒超时返回 → Close 立刻关连接池 → 那条 UPDATE 撞上
// "sql: database is closed" → **租约反而没交还成**，在途的那几张要挂满 180 秒
// 等租约自然过期，运营看到的是「重启完还卡在生成中」。
func TestWriteBackBudget_ClampedToStopBudget(t *testing.T) {
	f := newFixture(t, 1, func(cfg *Config, _ *Deps) {
		cfg.WriteBackTimeout = 10 * time.Second
		cfg.ShutdownWait = 10 * time.Second // 让调用方那个 ctx 成为更紧的一头
	})

	// 模拟 Module.Close：只给 3 秒。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = f.pool.Stop(ctx)

	budget := f.pool.writeBackBudget()
	require.Greater(t, budget, time.Duration(0))
	require.LessOrEqual(t, budget, 3*time.Second,
		"停机后写回预算必须 <= 调用方给的预算，否则那条 UPDATE 会撞上已关闭的连接池")

	// 交还租约走的也是同一条路，一起钉住。
	wbCtx, cancelWB := f.pool.writeBackContext()
	defer cancelWB()
	deadline, ok := wbCtx.Deadline()
	require.True(t, ok)
	require.LessOrEqual(t, time.Until(deadline), 3*time.Second)
}

// TestWriteBackBudget_NeverZero 预算花光了也要留一个很短的下限。
//
// 给 0 或负数 = context 当场过期 = 那条 UPDATE 根本发不出去，
// 租约只能等 180 秒自然过期。发出去至少还有一半机会赶在连接池关掉之前落库。
func TestWriteBackBudget_NeverZero(t *testing.T) {
	f := newFixture(t, 1, nil)

	f.pool.mu.Lock()
	f.pool.stopDeadline = time.Now().Add(-time.Minute) // 预算早就花光了
	f.pool.mu.Unlock()

	require.Equal(t, minWriteBackTimeout, f.pool.writeBackBudget())
}
