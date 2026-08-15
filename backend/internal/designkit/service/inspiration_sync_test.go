//go:build unit

package service

// 灵感库同步的单元测试。全部指向本地假上游，不联网、不花钱（CLAUDE.md 第三节）。
//
// 重点守住四件事（前三件都是老系统真出过事的地方）：
//  1. 手动和自动共用同一把锁，抢不到就跳过，不排队也不重试；
//  2. 代理只作用于同步这一个客户端，绝不污染全局（生图请求是局域网地址，
//     被代理带走必然连不上）；
//  3. 代理不通就停下，绝不静默回退直连；
//  4. 代理配置的键名跟管理界面那边（module.go）是同一个 —— 不一致的话
//     界面显示「已选代理」而同步在裸连，最难查。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ----------------------------------------------------------------------------
// 假仓储
// ----------------------------------------------------------------------------

type fakePromptStore struct {
	mu         sync.Mutex
	categories []string
	batches    [][]dkdomain.SyncPromptRow
	upsertErr  error
}

func (f *fakePromptStore) UpsertCategory(_ context.Context, category *dkdomain.PromptCategory) (*dkdomain.PromptCategory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.categories = append(f.categories, category.Slug)
	category.ID = int64(len(f.categories))
	return category, nil
}

func (f *fakePromptStore) UpsertSyncedPrompts(_ context.Context, rows []dkdomain.SyncPromptRow) (*dkdomain.SyncPromptsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return nil, f.upsertErr
	}
	batch := make([]dkdomain.SyncPromptRow, len(rows))
	copy(batch, rows)
	f.batches = append(f.batches, batch)
	return &dkdomain.SyncPromptsResult{Inserted: len(rows)}, nil
}

func (f *fakePromptStore) batchSizes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, 0, len(f.batches))
	for _, b := range f.batches {
		out = append(out, len(b))
	}
	return out
}

func (f *fakePromptStore) categorySlugs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.categories))
	copy(out, f.categories)
	return out
}

type fakeSyncStore struct {
	mu        sync.Mutex
	grantLock bool
	lockErr   error
	unlocks   int
	nextID    int64
	latest    *dkdomain.SyncRun
	latestErr error
	finished  chan dkdomain.SyncRun
}

func newFakeSyncStore(grantLock bool) *fakeSyncStore {
	return &fakeSyncStore{
		grantLock: grantLock,
		latestErr: dkdomain.ErrNotFound,
		finished:  make(chan dkdomain.SyncRun, 8),
	}
}

func (f *fakeSyncStore) TryLockSync(_ context.Context) (bool, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lockErr != nil {
		return false, nil, f.lockErr
	}
	if !f.grantLock {
		return false, nil, nil
	}
	return true, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.unlocks++
	}, nil
}

func (f *fakeSyncStore) StartSyncRun(_ context.Context, kind dkdomain.SyncKind) (*dkdomain.SyncRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	return &dkdomain.SyncRun{
		ID:        f.nextID,
		Kind:      kind,
		Status:    dkdomain.SyncStatusRunning,
		StartedAt: time.Now(),
	}, nil
}

func (f *fakeSyncStore) FinishSyncRun(_ context.Context, run *dkdomain.SyncRun) error {
	select {
	case f.finished <- *run:
	default:
	}
	return nil
}

func (f *fakeSyncStore) LatestSyncRun(_ context.Context) (*dkdomain.SyncRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.latestErr != nil {
		return nil, f.latestErr
	}
	return f.latest, nil
}

func (f *fakeSyncStore) unlockCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unlocks
}

// waitFinished 等后台那一轮把结果写进同步记录。
func (f *fakeSyncStore) waitFinished(t *testing.T) dkdomain.SyncRun {
	t.Helper()
	select {
	case run := <-f.finished:
		return run
	case <-time.After(3 * time.Second):
		t.Fatal("等了 3 秒也没等到同步结束，后台那一轮多半卡住了")
		return dkdomain.SyncRun{}
	}
}

// waitUnlocked 等咨询锁被还回去。锁是会话级的，不还的话下一轮永远抢不到，
// 灵感库从此再也不更新。
func (f *fakeSyncStore) waitUnlocked(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.unlockCount() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("同步结束后没有把咨询锁还回去，下一轮会永远抢不到锁")
}

type fakeSettingStore struct {
	mu     sync.Mutex
	values map[string]string
}

func newFakeSettingStore() *fakeSettingStore {
	return &fakeSettingStore{values: map[string]string{}}
}

func (f *fakeSettingStore) GetSetting(_ context.Context, key string) (*dkdomain.Setting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.values[key]
	if !ok {
		return nil, dkdomain.ErrNotFound
	}
	return &dkdomain.Setting{Key: key, Value: json.RawMessage(raw)}, nil
}

func (f *fakeSettingStore) set(key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] = value
}

type fakeProxies struct {
	mu        sync.Mutex
	url       string
	err       error
	inactive  bool // 模拟「代理被停用 / 过期 / 删除」
	activeErr error
}

func (f *fakeProxies) GetURL(_ context.Context, _ int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.url, f.err
}

// IsActive 照真实实现的语义：停用的代理 GetURL 照样能返回地址，
// 靠这一层把它拦住。
func (f *fakeProxies) IsActive(_ context.Context, _ int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.inactive, f.activeErr
}

func (f *fakeProxies) set(url string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.url, f.err = url, err
}

func (f *fakeProxies) setInactive(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inactive = v
}

type fakeFetcher struct {
	mu       sync.Mutex
	snapshot *InspirationSnapshot
	err      error
	calls    int
}

func (f *fakeFetcher) Fetch(_ context.Context, _ *http.Client) (*InspirationSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.snapshot, nil
}

func (f *fakeFetcher) Probe(_ context.Context, _ *http.Client) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return "2026-08-13T00:00:00Z", nil
}

func (f *fakeFetcher) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeFetcher) setSnapshot(s *InspirationSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshot = s
}

func (f *fakeFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ----------------------------------------------------------------------------
// 装配
// ----------------------------------------------------------------------------

type syncFixture struct {
	svc      *InspirationSyncService
	prompts  *fakePromptStore
	runs     *fakeSyncStore
	settings *fakeSettingStore
	proxies  *fakeProxies
	fetcher  *fakeFetcher
}

func newSyncFixture(t *testing.T, grantLock bool) *syncFixture {
	t.Helper()
	f := &syncFixture{
		prompts:  &fakePromptStore{},
		runs:     newFakeSyncStore(grantLock),
		settings: newFakeSettingStore(),
		proxies:  &fakeProxies{url: "http://127.0.0.1:19999"},
		fetcher:  &fakeFetcher{snapshot: fakeSnapshot(3)},
	}
	svc, err := NewInspirationSyncService(InspirationSyncDeps{
		Prompts:      f.prompts,
		Sync:         f.runs,
		Settings:     f.settings,
		Proxies:      f.proxies,
		Fetcher:      f.fetcher,
		Interval:     time.Hour,
		StartupDelay: time.Millisecond,
		NewUID:       fixedUID(),
	})
	if err != nil {
		t.Fatalf("建同步服务失败: %v", err)
	}
	f.svc = svc
	return f
}

func fakeSnapshot(n int) *InspirationSnapshot {
	prompts := make([]UpstreamPrompt, 0, n)
	for i := 1; i <= n; i++ {
		prompts = append(prompts, UpstreamPrompt{
			ID:      int64(i),
			Title:   fmt.Sprintf("标题 %d", i),
			Content: fmt.Sprintf("正文 %d", i),
			Slugs:   []string{"ecommerce-main-image"},
		})
	}
	return &InspirationSnapshot{UpdatedAt: "2026-08-13", Prompts: prompts}
}

func startManual(t *testing.T, f *syncFixture) *dkdomain.SyncRun {
	t.Helper()
	run, err := f.svc.StartSync(context.Background(), dkdomain.SyncKindManual)
	if err != nil {
		t.Fatalf("同步没能开始: %v", err)
	}
	return run
}

// ----------------------------------------------------------------------------
// 锁
// ----------------------------------------------------------------------------

func TestStartSyncSkipsWhenAnotherSyncHoldsTheLock(t *testing.T) {
	// 老系统手动一把锁、自动一把锁，两路同时导入把 1.4 万条写重复了，
	// 是代码审查抓出来的 critical。现在共用一把 PostgreSQL 咨询锁，
	// 抢不到就跳过 —— 不排队、不重试。
	f := newSyncFixture(t, false)

	_, err := f.svc.StartSync(context.Background(), dkdomain.SyncKindManual)
	if err == nil {
		t.Fatal("抢不到锁时要给出「已经有一次在跑」的提示")
	}
	dkErr, ok := dkdomain.AsDesignkitError(err)
	if !ok || dkErr.Code != dkdomain.ErrCodeSyncInProgress {
		t.Fatalf("要用 DK_SYNC_IN_PROGRESS（界面按它显示「等这一次跑完再点」）：%v", err)
	}
	if strings.Contains(dkErr.Message, "API") || strings.Contains(dkErr.Message, "token") {
		t.Fatalf("给运营看的文案里不许出现技术词：%q", dkErr.Message)
	}
	if f.fetcher.callCount() != 0 {
		t.Fatal("没抢到锁却去下载了，等于两路同时导入")
	}

	// 还要记一条 skipped 的记录，否则「灵感库怎么两天没更新」根本查不出来。
	run := f.runs.waitFinished(t)
	if run.Status != dkdomain.SyncStatusSkipped {
		t.Fatalf("跳过的那一次要记成 skipped，得到 %q", run.Status)
	}
}

func TestSyncReleasesLockAfterFinishing(t *testing.T) {
	f := newSyncFixture(t, true)
	startManual(t, f)
	f.runs.waitFinished(t)
	f.runs.waitUnlocked(t)
	if got := f.runs.unlockCount(); got != 1 {
		t.Fatalf("同步结束后应该正好还一次锁，实际 %d 次", got)
	}
}

func TestSyncFailureAlsoReleasesLock(t *testing.T) {
	f := newSyncFixture(t, true)
	f.fetcher.fail(errors.New("dial tcp: i/o timeout"))

	startManual(t, f)
	run := f.runs.waitFinished(t)
	if run.Status != dkdomain.SyncStatusFailed {
		t.Fatalf("下载失败应该记 failed，得到 %q", run.Status)
	}
	f.runs.waitUnlocked(t)
}

// ----------------------------------------------------------------------------
// 一轮同步
// ----------------------------------------------------------------------------

func TestSyncReturnsImmediatelyAndWritesInBatches(t *testing.T) {
	f := newSyncFixture(t, true)
	f.fetcher.setSnapshot(fakeSnapshot(1200)) // 跨 3 批（500 + 500 + 200）

	run := startManual(t, f)
	if run.Status != dkdomain.SyncStatusRunning {
		t.Fatalf("要立刻返回 running（下载在后台跑，不能让接口挂十几秒），得到 %q", run.Status)
	}

	done := f.runs.waitFinished(t)
	if done.Status != dkdomain.SyncStatusSucceeded {
		t.Fatalf("应该成功，得到 %q（%v）", done.Status, done.Error)
	}
	if done.Fetched != 1200 || done.Inserted != 1200 {
		t.Fatalf("计数不对：拉回 %d、新增 %d", done.Fetched, done.Inserted)
	}
	if got := f.prompts.batchSizes(); len(got) != 3 || got[0] != 500 || got[2] != 200 {
		t.Fatalf("应该分 3 批写（一个事务扛 1.4 万条是个十几秒的长事务），实际 %v", got)
	}
	// 分类必须先建齐：repository 按 slug 找分类 id，找不到就把 category_id 写空，
	// 运营按分类什么都翻不到。
	if got := f.prompts.categorySlugs(); len(got) != len(inspirationCategories) {
		t.Fatalf("应该建 %d 个分类，实际 %d 个", len(inspirationCategories), len(got))
	}
}

func TestSyncFailureMessageIsChineseAndActionable(t *testing.T) {
	f := newSyncFixture(t, true)
	f.fetcher.fail(errors.New("dial tcp 140.82.0.1:443: connect: connection refused"))

	startManual(t, f)
	run := f.runs.waitFinished(t)
	if run.Error == nil {
		t.Fatal("失败必须记原因，否则界面上只有一个红点，管理员无从下手")
	}
	msg := *run.Error
	if !strings.Contains(msg, "连不上") {
		t.Fatalf("错误文案要说人话：%q", msg)
	}
	for _, jargon := range []string{"API", "token", "dial tcp", "error"} {
		if strings.Contains(msg, jargon) {
			t.Fatalf("给运营看的文案里出现了技术词 %q：%q", jargon, msg)
		}
	}
}

// ----------------------------------------------------------------------------
// 代理
// ----------------------------------------------------------------------------

func TestProxySettingKeyMatchesAdminUI(t *testing.T) {
	// 管理界面把管理员选的代理写进 designkit_settings 的某个键，同步器读的必须是同一个。
	// 两边不一致的后果是：界面显示「已选代理 3」，同步却读不到、老老实实走直连，
	// 而且没有任何报错 —— 正是最难查的那种。
	//
	// 这条测试原来是 grep module.go 里有没有那个**字面量**。
	// 后来 module.go 改成 `const SettingKeyPromptSyncProxyID = dkservice.SettingKeyPromptSyncProxyID`
	// 直接引用本包的常量，字面量自然就没有了，测试就红了 ——
	// 而那恰恰是更好的写法（两边从「靠测试钉住」升级成「结构上不可能不一致」）。
	//
	// 所以判据改成：module.go 必须**引用本包的常量**，不许自己另写一个字面量。
	raw, err := os.ReadFile(filepath.Clean("../module.go"))
	if err != nil {
		t.Fatalf("读不到 module.go: %v", err)
	}
	src := string(raw)

	if !strings.Contains(src, "dkservice.SettingKeyPromptSyncProxyID") {
		t.Fatalf("module.go 没有引用 dkservice.SettingKeyPromptSyncProxyID —— " +
			"管理界面写的键必须跟同步器读的是同一个常量，不要各写一份字面量")
	}

	// 反向守卫：如果哪天有人在 module.go 里手写了一个**不一样**的字面量，
	// 引用还在、但实际用的是手写那个，一样会不一致。
	//
	// 判据要精确到「长得像这个设置键」：同时以 prompt_sync 开头、以 proxy_id 结尾。
	// 只判其中一头会误伤日志字段名（module.go 里有 "prompt_sync_runner" 和 "proxy_id"，
	// 那两个是 slog 的键，跟设置键没关系）。
	for _, lit := range stringLiteralRe.FindAllStringSubmatch(src, -1) {
		v := lit[1]
		if !strings.HasPrefix(v, "prompt_sync") || !strings.HasSuffix(v, "proxy_id") {
			continue
		}
		if v != SettingKeyPromptSyncProxyID {
			t.Fatalf("module.go 里手写了一个设置键 %q，跟同步器读的 %q 不一致 —— "+
				"界面会显示「已选代理」而同步在裸连", v, SettingKeyPromptSyncProxyID)
		}
	}
}

// stringLiteralRe 抓 Go 源码里的双引号字面量（够用即可：这些键里不会有转义）。
var stringLiteralRe = regexp.MustCompile(`"([^"\\]*)"`)

// 代理被停用/过期/删掉之后，同步**必须停下并报错**，绝不能静默走它、
// 也绝不能静默改成直连。
//
// 为什么单独守这一条：上游 ProxyService.GetURL 走的是 GetByID，**不过滤 status**，
// 停用的代理照样返回一个能用的地址。少了 IsActive 这道判断就会变成
// 「设置页显示直连、同步实际还在走那个停用的代理」——
// 通了没人发现，不通时报的错还指向一个界面上根本没选中的代理。
func TestSyncRefusesInactiveProxy(t *testing.T) {
	f := newSyncFixture(t, true)
	ctx := context.Background()

	f.settings.set(SettingKeyPromptSyncProxyID, "3")
	f.proxies.set("http://127.0.0.1:1/", nil) // GetURL 照样给得出地址
	f.proxies.setInactive(true)               // 但它已经停用了

	_, err := f.svc.proxyURL(ctx, 3)
	if err == nil {
		t.Fatal("代理已停用时必须报错，不能拿 GetURL 返回的地址接着用")
	}
	msg := err.Error()
	for _, want := range []string{"停用", "没有改成直连"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误文案里应该说清「已停用」和「没有改成直连」，实际是：%s", msg)
		}
	}
	// 错误里不许出现代理地址（可能带账号密码）
	if strings.Contains(msg, "127.0.0.1") {
		t.Fatalf("错误文案里不许带代理地址：%s", msg)
	}
}

func TestProxyIDReadsSetting(t *testing.T) {
	f := newSyncFixture(t, true)
	ctx := context.Background()

	// 没配过 → 直连
	id, err := f.svc.ProxyID(ctx)
	if err != nil || id != nil {
		t.Fatalf("没配过应该是直连：%v %v", id, err)
	}

	// null → 直连
	f.settings.set(SettingKeyPromptSyncProxyID, "null")
	if id, err = f.svc.ProxyID(ctx); err != nil || id != nil {
		t.Fatalf("null 应该是直连：%v %v", id, err)
	}

	// 数字 → 用那个代理
	f.settings.set(SettingKeyPromptSyncProxyID, "7")
	if id, err = f.svc.ProxyID(ctx); err != nil || id == nil || *id != 7 {
		t.Fatalf("应该读出 7：%v %v", id, err)
	}

	// 值坏了 → **报错**，不能当成直连悄悄跑（那正是要避免的「以为走代理其实没走」）
	f.settings.set(SettingKeyPromptSyncProxyID, `"abc"`)
	if _, err = f.svc.ProxyID(ctx); err == nil {
		t.Fatal("值不合法必须报错，不能静默当直连")
	}
}

func TestProxyURLRejectsUnusableProxy(t *testing.T) {
	f := newSyncFixture(t, true)
	ctx := context.Background()

	// 协议不在白名单里（proxyurl.Parse 只认 http/https/socks5/socks5h）
	f.proxies.set("ftp://nope", nil)
	if _, err := f.svc.proxyURL(ctx, 2); err == nil {
		t.Fatal("协议不支持的代理必须当场拒绝，不能拿去建客户端")
	}

	// 代理被管理员删了
	f.proxies.set("", errors.New("proxy not found"))
	_, err := f.svc.proxyURL(ctx, 5)
	if err == nil {
		t.Fatal("代理取不到时必须报错")
	}
	dkErr, ok := dkdomain.AsDesignkitError(err)
	if !ok || !strings.Contains(dkErr.Message, "IP管理") {
		t.Fatalf("错误文案要指路到「IP管理」：%v", err)
	}
}

func TestProxyErrorNeverLeaksCredentials(t *testing.T) {
	// 代理地址里带账号密码，绝不能出现在给运营看的文案里。
	f := newSyncFixture(t, true)
	f.proxies.set("http://user:sup3rsecret@10.0.0.9:8080", errors.New("boom"))

	_, err := f.svc.proxyURL(context.Background(), 3)
	if err == nil {
		t.Fatal("应该报错")
	}
	dkErr, ok := dkdomain.AsDesignkitError(err)
	if !ok {
		t.Fatalf("应该是我们自己的错误类型：%v", err)
	}
	if strings.Contains(dkErr.Message, "sup3rsecret") || strings.Contains(dkErr.Message, "10.0.0.9") {
		t.Fatalf("代理凭据泄漏进了界面文案：%q", dkErr.Message)
	}
}

func TestProxyIsScopedToSyncClientOnly(t *testing.T) {
	// **本文件最要紧的一条断言。**
	// 老代码的注释原话：「不能用全局 urllib 代理或环境变量——那会把生图请求也带进代理」。
	// 生图网关是局域网地址，一旦被代理带走必然连不上，而且报错完全看不出原因。
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	beforeTransport := http.DefaultTransport

	f := newSyncFixture(t, true)
	f.settings.set(SettingKeyPromptSyncProxyID, "3")
	f.proxies.set("http://127.0.0.1:19999", nil)

	client, via, err := f.svc.syncHTTPClient(context.Background())
	if err != nil {
		t.Fatalf("建同步客户端失败：%v", err)
	}
	if client == http.DefaultClient {
		t.Fatal("同步不能用全局默认客户端")
	}
	if client.Transport == http.DefaultTransport {
		t.Fatal("同步的代理配置跑到全局 Transport 上去了，生图请求会被一起带进代理")
	}
	if http.DefaultTransport != beforeTransport {
		t.Fatal("http.DefaultTransport 被换掉了 —— 全进程的出站请求都会受影响")
	}
	if http.DefaultClient.Transport != nil {
		t.Fatal("全局默认客户端被装上了自定义 Transport，生图请求会跟着受影响")
	}
	if v := stdEnvProxy(); v != "" {
		t.Fatalf("同步不许设 HTTP_PROXY 环境变量，实际 %q", v)
	}
	if !strings.Contains(via, "代理") {
		t.Fatalf("说明文案应该写明走了代理：%q", via)
	}
	// 说明文案只带编号，不带地址（地址里有账号密码，日志里也不能出现）。
	if strings.Contains(via, "127.0.0.1") {
		t.Fatalf("说明文案里不该出现代理地址：%q", via)
	}
}

func stdEnvProxy() string {
	for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	return ""
}

func TestSyncNeverSilentlyFallsBackToDirect(t *testing.T) {
	// 代理不通时**必须停下**。运营以为走了代理、实际走的直连，
	// 出口 IP 不是他以为的那个，而且没有任何提示 —— 比直接报错糟糕得多。
	f := newSyncFixture(t, true)
	f.settings.set(SettingKeyPromptSyncProxyID, "3")
	f.fetcher.fail(errors.New("proxyconnect tcp: connection refused"))

	startManual(t, f)
	run := f.runs.waitFinished(t)
	if run.Status != dkdomain.SyncStatusFailed {
		t.Fatalf("代理不通必须记失败，得到 %q", run.Status)
	}
	msg := ""
	if run.Error != nil {
		msg = *run.Error
	}
	for _, want := range []string{"代理连不上", "IP管理", "没有改成直连"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("失败文案里应该有 %q：%q", want, msg)
		}
	}
}

func TestConfiguredProxyWithoutResolverIsRefused(t *testing.T) {
	// 装配时没拿到上游的代理服务，但管理员选了代理 → 报错，
	// 不许「反正拿不到就直连」。
	f := newSyncFixture(t, true)
	f.svc.proxies = nil
	f.settings.set(SettingKeyPromptSyncProxyID, "3")

	if _, _, err := f.svc.syncHTTPClient(context.Background()); err == nil {
		t.Fatal("选了代理却拿不到代理服务时，必须报错而不是直连")
	}
}

func TestDirectConnectionNeedsNoProxyService(t *testing.T) {
	// 没选代理时，代理服务缺席完全不影响同步。
	f := newSyncFixture(t, true)
	f.svc.proxies = nil

	client, via, err := f.svc.syncHTTPClient(context.Background())
	if err != nil {
		t.Fatalf("直连不该失败：%v", err)
	}
	if client == nil {
		t.Fatal("直连也要给一个客户端")
	}
	if via != "直连" {
		t.Fatalf("说明文案应该是「直连」，得到 %q", via)
	}
}

// ----------------------------------------------------------------------------
// 定时器
// ----------------------------------------------------------------------------

func TestStartRunsCatchUpSyncThenStops(t *testing.T) {
	// 新装的机器灵感库是空的，等 12 小时太久；群晖半夜断电重启也一样。
	f := newSyncFixture(t, true)
	if err := f.svc.Start(context.Background()); err != nil {
		t.Fatalf("定时器没起来：%v", err)
	}
	defer func() {
		if err := f.svc.Close(); err != nil {
			t.Fatalf("关闭失败：%v", err)
		}
	}()

	run := f.runs.waitFinished(t)
	if run.Kind != dkdomain.SyncKindAuto {
		t.Fatalf("补同步应该记成自动触发，得到 %q", run.Kind)
	}
	if run.Status != dkdomain.SyncStatusSucceeded {
		t.Fatalf("补同步应该成功，得到 %q", run.Status)
	}
}

func TestNeedsCatchUp(t *testing.T) {
	f := newSyncFixture(t, true)
	now := time.Now()
	f.svc.now = func() time.Time { return now }
	ctx := context.Background()

	if !f.svc.needsCatchUp(ctx) {
		t.Fatal("从来没同步过就该补一次")
	}

	recent := now.Add(-30 * time.Minute)
	f.runs.latestErr = nil
	f.runs.latest = &dkdomain.SyncRun{
		Status: dkdomain.SyncStatusSucceeded, StartedAt: recent, FinishedAt: &recent,
	}
	if f.svc.needsCatchUp(ctx) {
		t.Fatal("半小时前刚成功过，间隔是一小时，不该再补")
	}

	old := now.Add(-2 * time.Hour)
	f.runs.latest = &dkdomain.SyncRun{
		Status: dkdomain.SyncStatusSucceeded, StartedAt: old, FinishedAt: &old,
	}
	if !f.svc.needsCatchUp(ctx) {
		t.Fatal("超过一个间隔就该补")
	}

	f.runs.latest = &dkdomain.SyncRun{Status: dkdomain.SyncStatusFailed, StartedAt: now}
	if !f.svc.needsCatchUp(ctx) {
		t.Fatal("上次失败了就该补")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	f := newSyncFixture(t, true)
	if err := f.svc.Start(context.Background()); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	if err := f.svc.Close(); err != nil {
		t.Fatalf("第一次关闭失败：%v", err)
	}
	if err := f.svc.Close(); err != nil {
		t.Fatalf("重复关闭应该是空操作：%v", err)
	}
	// 关掉之后再 Start 是空操作（进程正在退出，不该再起新的定时器）。
	if err := f.svc.Start(context.Background()); err != nil {
		t.Fatalf("关闭后再启动应该是空操作：%v", err)
	}
}

func TestProbeUpstreamSaysHowItConnected(t *testing.T) {
	f := newSyncFixture(t, true)
	msg, err := f.svc.ProbeUpstream(context.Background())
	if err != nil {
		t.Fatalf("测试连接失败：%v", err)
	}
	if !strings.Contains(msg, "直连") {
		t.Fatalf("要说清是直连还是走代理：%q", msg)
	}
}

// ----------------------------------------------------------------------------
// 真实数据源（打本地假上游，不联网）
// ----------------------------------------------------------------------------

// fakeUpstream 起一个本地 HTTP 服务，扮演 raw.githubusercontent.com。
func fakeUpstream(t *testing.T, files map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		body, ok := files[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("not found"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fullUpstreamFiles 造一份「11 个分类都在」的假上游。
func fullUpstreamFiles() map[string]string {
	files := map[string]string{
		"manifest.json": `{"updatedAt":"2026-08-13T06:00:00Z"}`,
	}
	for _, c := range inspirationCategories {
		files[c.Slug+".json"] = "[]"
	}
	return files
}

func TestFetcherMergesCategoriesAndSortsByID(t *testing.T) {
	files := fullUpstreamFiles()
	// 同一条（id=7）同时出现在两个分类文件里，应该合并成一条、带两个标签。
	files["social-media-post.json"] = `[
		{"id": 9, "title": "社交九号", "content": "c9", "sourceMedia": ["https://i/9.png"]},
		{"id": 7, "title": "两边都有", "content": "c7"}
	]`
	files["ecommerce-main-image.json"] = `[{"id": 7, "title": "两边都有", "content": "c7"}]`
	srv := fakeUpstream(t, files)

	snapshot, err := NewYouMindFetcher(srv.URL).Fetch(context.Background(), srv.Client())
	if err != nil {
		t.Fatalf("拉取失败：%v", err)
	}
	if snapshot.UpdatedAt != "2026-08-13T06:00:00Z" {
		t.Fatalf("快照时间没读到：%q", snapshot.UpdatedAt)
	}
	if len(snapshot.Prompts) != 2 {
		t.Fatalf("同一条出现在两个分类里应该合并成一条，得到 %d 条", len(snapshot.Prompts))
	}
	// 按 id 升序：map 遍历顺序是随机的，不排的话每轮同步写入顺序都不一样。
	if snapshot.Prompts[0].ID != 7 || snapshot.Prompts[1].ID != 9 {
		t.Fatalf("没有按 id 升序：%d, %d", snapshot.Prompts[0].ID, snapshot.Prompts[1].ID)
	}
	if len(snapshot.Prompts[0].Slugs) != 2 {
		t.Fatalf("两个分类的标签都要留着：%v", snapshot.Prompts[0].Slugs)
	}
	// 主分类按贴合度取「电商主图」，而不是先遇到的「社交媒体帖子」。
	if got := primaryCategorySlug(snapshot.Prompts[0].Slugs); got != "ecommerce-main-image" {
		t.Fatalf("主分类应该是电商主图，得到 %q", got)
	}
}

func TestFetcherToleratesWeirdIDsWithoutLosingTheWholeRound(t *testing.T) {
	// 上游偶尔把编号写成字符串。整份文件因此解析失败的话，
	// 1.4 万条一条都进不去 —— 为一条脏数据丢掉全部，不划算。
	files := fullUpstreamFiles()
	files["others.json"] = `[
		{"id": "42", "title": "编号是字符串", "content": "c1"},
		{"id": null, "title": "没有编号", "content": "c2"},
		{"id": 43, "title": "正常", "content": "c3"}
	]`
	srv := fakeUpstream(t, files)

	snapshot, err := NewYouMindFetcher(srv.URL).Fetch(context.Background(), srv.Client())
	if err != nil {
		t.Fatalf("一条脏数据不该让整轮失败：%v", err)
	}
	if len(snapshot.Prompts) != 2 {
		t.Fatalf("字符串编号要认出来、没编号的要丢掉，得到 %d 条", len(snapshot.Prompts))
	}
	if snapshot.Prompts[0].ID != 42 || snapshot.Prompts[1].ID != 43 {
		t.Fatalf("编号不对：%d, %d", snapshot.Prompts[0].ID, snapshot.Prompts[1].ID)
	}
}

func TestFetcherFailsWholeRoundWhenOneFileIsMissing(t *testing.T) {
	// 半截数据的后果是「某几个分类突然空了」，运营完全看不出发生了什么。
	files := fullUpstreamFiles()
	delete(files, "game-asset.json")
	srv := fakeUpstream(t, files)

	_, err := NewYouMindFetcher(srv.URL).Fetch(context.Background(), srv.Client())
	if err == nil {
		t.Fatal("少一个文件必须整轮失败，绝不能半截入库")
	}
	if !strings.Contains(err.Error(), "game-asset") {
		t.Fatalf("错误里要说清是哪个文件坏了：%v", err)
	}
}

func TestFetcherRejectsNonJSON(t *testing.T) {
	// 代理挡在前面时，返回的往往是一段 HTML 登录页而不是 JSON。
	files := fullUpstreamFiles()
	files["manifest.json"] = "<html>proxy login</html>"
	srv := fakeUpstream(t, files)

	_, err := NewYouMindFetcher(srv.URL).Fetch(context.Background(), srv.Client())
	if err == nil {
		t.Fatal("不是合法 JSON 必须报错")
	}
	if !strings.Contains(err.Error(), "代理") {
		t.Fatalf("要提示可能是代理返回了错误页：%v", err)
	}
}

func TestFetcherRefusesEmptyUpstream(t *testing.T) {
	// 上游一条都不给（多半是改了目录结构）时，宁可什么都不做，
	// 也不能让库里已有的 1.4 万条变成界面上的空白。
	srv := fakeUpstream(t, fullUpstreamFiles())
	_, err := NewYouMindFetcher(srv.URL).Fetch(context.Background(), srv.Client())
	if err == nil {
		t.Fatal("上游一条都没有时应该报错，不写库")
	}
}

func TestFetcherProbeOnlyReadsManifest(t *testing.T) {
	// 「测试连接」要几秒出结果：整轮同步十几秒，让人等那么久才发现代理不通太浪费。
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasSuffix(r.URL.Path, "manifest.json") {
			t.Errorf("测试连接不该去拉 %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"updatedAt":"2026-08-13"}`))
	}))
	defer srv.Close()

	updatedAt, err := NewYouMindFetcher(srv.URL).Probe(context.Background(), srv.Client())
	if err != nil {
		t.Fatalf("测试连接失败：%v", err)
	}
	if updatedAt != "2026-08-13" {
		t.Fatalf("快照时间不对：%q", updatedAt)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("只该请求一次，实际 %d 次", got)
	}
}
