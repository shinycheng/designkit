//go:build unit

package designkit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	dkhandler "github.com/Wei-Shaw/sub2api/internal/designkit/handler"
	dkrepository "github.com/Wei-Shaw/sub2api/internal/designkit/repository"
	dkservice "github.com/Wei-Shaw/sub2api/internal/designkit/service"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// 灵感库的装配（组合根这一侧）
// ============================================================================
//
// 这里守的是三条**只在装配层才看得见**的底线：
//  1. 翻页判「还有没有下一页」不能被 repository 的每页上限吃掉；
//  2. 同步记录挂死在 running 时不能让界面永远转圈；
//  3. 分类被删 / 改名时，灵感库还能照常翻。

// ---- promptProbeLimit ----

// 「还有没有下一页」是靠多要一条判出来的。
// repository 会把每页条数压到 MaxPageLimit（100），直接要 101 会被压回 100，
// 于是**永远判不出还有下一页** —— 第 101 条之后的词谁也翻不到，而且不报错。
func TestPromptProbeLimitNeverExceedsRepositoryCap(t *testing.T) {
	cases := []struct {
		in        int
		wantPage  int
		wantProbe int
	}{
		{in: 0, wantPage: dkrepository.DefaultPageLimit, wantProbe: dkrepository.DefaultPageLimit + 1},
		{in: -5, wantPage: dkrepository.DefaultPageLimit, wantProbe: dkrepository.DefaultPageLimit + 1},
		{in: 1, wantPage: 1, wantProbe: 2},
		{in: 20, wantPage: 20, wantProbe: 21},
		{in: dkrepository.MaxPageLimit, wantPage: dkrepository.MaxPageLimit - 1, wantProbe: dkrepository.MaxPageLimit},
		{in: 1000, wantPage: dkrepository.MaxPageLimit - 1, wantProbe: dkrepository.MaxPageLimit},
	}
	for _, tc := range cases {
		page, probe := promptProbeLimit(tc.in)
		assert.Equal(t, tc.wantPage, page, "limit=%d", tc.in)
		assert.Equal(t, tc.wantProbe, probe, "limit=%d", tc.in)
		assert.LessOrEqual(t, probe, dkrepository.MaxPageLimit,
			"要的条数一旦超过上限就会被静默压回来，has_more 从此永远是 false")
		assert.Greater(t, probe, page, "必须多要一条，否则判不出还有没有下一页")
	}
}

// ---- isSyncRunning ----

// 同步跑到一半进程被重启，那条记录会永远挂着 running。
// 光看 status 的话，管理员界面就一直转圈等一个不会结束的同步，
// 而「立即同步」按钮被禁用着 —— 死局，只能改数据库才解得开。
func TestIsSyncRunningTreatsStaleRunAsFinished(t *testing.T) {
	now := time.Now()

	fresh := &dkdomain.SyncRun{Status: dkdomain.SyncStatusRunning, StartedAt: now.Add(-5 * time.Second)}
	assert.True(t, isSyncRunning(fresh, now), "刚开始几秒的同步就是在跑")

	stale := &dkdomain.SyncRun{Status: dkdomain.SyncStatusRunning, StartedAt: now.Add(-promptSyncStaleAfter - time.Second)}
	assert.False(t, isSyncRunning(stale, now), "挂太久的 running 记录要当成没有正常结束")

	done := &dkdomain.SyncRun{Status: dkdomain.SyncStatusSucceeded, StartedAt: now}
	assert.False(t, isSyncRunning(done, now))

	assert.False(t, isSyncRunning(nil, now), "一次都没跑过时不能算「正在跑」")
}

// 兜底窗口必须比一次同步实际要的时间宽裕得多，不然会把跑着的同步误判成挂了。
func TestPromptSyncStaleAfterIsGenerous(t *testing.T) {
	assert.GreaterOrEqual(t, promptSyncStaleAfter, 5*time.Minute,
		"一次同步实测十几秒，窗口太窄会把跑着的同步误判成挂了")
}

// ---- newPromptView ----

func TestNewPromptViewFillsCategory(t *testing.T) {
	idx := &categoryIndex{byID: map[int64]*dkdomain.PromptCategory{
		1: {ID: 1, Slug: "product-shot", NameZH: "商品实拍"},
		2: {ID: 2, Slug: "lifestyle", NameEN: "Lifestyle"},
	}}

	categoryID := int64(1)
	view := newPromptView(&dkdomain.Prompt{UID: "u1", CategoryID: &categoryID}, idx)
	assert.Equal(t, "product-shot", view.CategorySlug)
	assert.Equal(t, "商品实拍", view.CategoryName)

	// 中文名空着时退到英文名，再空退到 slug：界面上绝不留一行空白。
	categoryID = 2
	view = newPromptView(&dkdomain.Prompt{UID: "u2", CategoryID: &categoryID}, idx)
	assert.Equal(t, "Lifestyle", view.CategoryName)

	// 分类被删过（ON DELETE SET NULL）时不报错，就是「未分类」。
	view = newPromptView(&dkdomain.Prompt{UID: "u3"}, idx)
	assert.Empty(t, view.CategorySlug)
	assert.Empty(t, view.CategoryName)

	missing := int64(99)
	view = newPromptView(&dkdomain.Prompt{UID: "u4", CategoryID: &missing}, idx)
	assert.Empty(t, view.CategoryName, "对不上号的分类只当未分类，不要报错")
}

// ---- promptSyncAdapter ----

// 假 repository：只实现灵感库同步这几条用得上的方法，其余靠内嵌接口占位。
// 真调到没实现的方法会 panic，正好逼我们把用例写准。
type fakePromptRepo struct {
	dkdomain.Repository

	settings map[string]json.RawMessage
	putKey   string
	putValue json.RawMessage
	putErr   error

	latest    *dkdomain.SyncRun
	latestErr error

	promptCount int
	categories  []*dkdomain.PromptCategory
}

func (f *fakePromptRepo) GetSetting(_ context.Context, key string) (*dkdomain.Setting, error) {
	value, ok := f.settings[key]
	if !ok {
		return nil, dkdomain.ErrNotFound
	}
	return &dkdomain.Setting{Key: key, Value: value}, nil
}

func (f *fakePromptRepo) PutSetting(_ context.Context, key string, value []byte) error {
	f.putKey = key
	f.putValue = append(json.RawMessage(nil), value...)
	if f.putErr != nil {
		return f.putErr
	}
	if f.settings == nil {
		f.settings = map[string]json.RawMessage{}
	}
	f.settings[key] = f.putValue
	return nil
}

func (f *fakePromptRepo) LatestSyncRun(_ context.Context) (*dkdomain.SyncRun, error) {
	if f.latestErr != nil {
		return nil, f.latestErr
	}
	return f.latest, nil
}

func (f *fakePromptRepo) CountPrompts(_ context.Context, _ dkdomain.ListPromptsQuery) (int, error) {
	return f.promptCount, nil
}

func (f *fakePromptRepo) ListCategories(_ context.Context) ([]*dkdomain.PromptCategory, error) {
	return f.categories, nil
}

// 一次都没同步过时不能报错：全新部署本来就是这样。
func TestSyncStatusWithoutAnyRun(t *testing.T) {
	repo := &fakePromptRepo{
		latestErr:   dkdomain.ErrNotFound,
		promptCount: 0,
	}
	adapter := promptSyncAdapter{repo: repo}

	status, err := adapter.SyncStatus(context.Background())
	require.NoError(t, err, "没同步过不是错误，全新部署本来就是这样")
	assert.Nil(t, status.Latest)
	assert.False(t, status.Running)
}

func TestSyncStatusCountsLibrary(t *testing.T) {
	repo := &fakePromptRepo{
		latest:      &dkdomain.SyncRun{Status: dkdomain.SyncStatusSucceeded, StartedAt: time.Now()},
		promptCount: 14700,
		categories:  []*dkdomain.PromptCategory{{ID: 1}, {ID: 2}},
	}
	adapter := promptSyncAdapter{repo: repo}

	status, err := adapter.SyncStatus(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 14700, status.PromptCount)
	assert.Equal(t, 2, status.CategoryCount)
	assert.False(t, status.Running)
}

// 没配过代理 = 直连，不能报错。
func TestGetSyncSettingsDefaultsToDirect(t *testing.T) {
	adapter := promptSyncAdapter{repo: &fakePromptRepo{}}

	settings, err := adapter.GetSyncSettings(context.Background())
	require.NoError(t, err)
	assert.Nil(t, settings.ProxyID, "没配过就是直连")
	assert.NotNil(t, settings.Proxies, "代理列表要给空数组，前端要直接 v-for")
	assert.Empty(t, settings.Proxies)
}

// 配置被手工改坏时**按直连处理并放行**，不能让整个设置页面打不开 ——
// 打不开的话管理员连「改回去」的入口都没有。
func TestGetSyncSettingsSurvivesBrokenValue(t *testing.T) {
	repo := &fakePromptRepo{settings: map[string]json.RawMessage{
		SettingKeyPromptSyncProxyID: json.RawMessage(`"不是数字"`),
	}}
	adapter := promptSyncAdapter{repo: repo}

	settings, err := adapter.GetSyncSettings(context.Background())
	require.NoError(t, err, "读不懂的配置不该让设置页面打不开")
	assert.Nil(t, settings.ProxyID)
}

// 选中的代理后来被删了 / 停用了：如实回 null。
// 留着一个列表里没有的 ID，下拉框会显示成空白，管理员以为还走着代理。
func TestGetSyncSettingsDropsVanishedProxy(t *testing.T) {
	repo := &fakePromptRepo{settings: map[string]json.RawMessage{
		SettingKeyPromptSyncProxyID: json.RawMessage(`7`),
	}}
	adapter := promptSyncAdapter{repo: repo}

	settings, err := adapter.GetSyncSettings(context.Background())
	require.NoError(t, err)
	assert.Nil(t, settings.ProxyID, "代理已经不在列表里了，要如实回「直连」")
}

// 直连写进库里的是 JSON 的 null（那一列是 JSONB，标量也得是合法 JSON）。
func TestPutSyncSettingsWritesJSONNullForDirect(t *testing.T) {
	repo := &fakePromptRepo{}
	adapter := promptSyncAdapter{repo: repo}

	settings, err := adapter.PutSyncSettings(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, settings.ProxyID)
	assert.Equal(t, SettingKeyPromptSyncProxyID, repo.putKey)
	assert.Equal(t, "null", string(repo.putValue))
	assert.True(t, json.Valid(repo.putValue), "designkit_settings.value 是 JSONB，写进去的必须是合法 JSON")
}

// 选了一个不在列表里的代理时**明确拒绝**，不静默改成直连：
// 静默的后果是管理员以为走了代理、实际在裸连，拉不动时查不出原因。
func TestPutSyncSettingsRejectsUnknownProxy(t *testing.T) {
	repo := &fakePromptRepo{}
	adapter := promptSyncAdapter{repo: repo}

	unknown := int64(7)
	_, err := adapter.PutSyncSettings(context.Background(), &unknown)
	require.Error(t, err)

	dkErr, ok := dkdomain.AsDesignkitError(err)
	require.True(t, ok, "要给一个我方错误码，不能是裸 error")
	assert.Equal(t, dkdomain.ErrCodeInvalidRequest, dkErr.Code)
	assert.Empty(t, repo.putKey, "拒绝的时候不该已经写过库")
}

// 同步器还没装配时，「立即同步」给一句中文，而不是 500 或者静默成功。
func TestStartSyncWithoutRunnerFailsLoudlyInChinese(t *testing.T) {
	adapter := promptSyncAdapter{repo: &fakePromptRepo{}}

	_, err := adapter.StartSync(context.Background())
	require.Error(t, err)

	dkErr, ok := dkdomain.AsDesignkitError(err)
	require.True(t, ok)
	assert.NotEmpty(t, dkErr.Message)
	assert.Contains(t, dkErr.Message, "管理员", "运营/管理员要看得懂下一步找谁")
}

// 装了同步器就必须真的调它，而且**必须标成 manual**
// （手动和自动在 designkit_sync_runs 里要分得开，排障时靠它认人）。
type fakeSyncRunner struct {
	calls []dkdomain.SyncKind
	run   *dkdomain.SyncRun
	err   error
}

func (f *fakeSyncRunner) StartSync(_ context.Context, kind dkdomain.SyncKind) (*dkdomain.SyncRun, error) {
	f.calls = append(f.calls, kind)
	return f.run, f.err
}

func TestStartSyncDelegatesAsManual(t *testing.T) {
	runner := &fakeSyncRunner{run: &dkdomain.SyncRun{ID: 1, Kind: dkdomain.SyncKindManual}}
	adapter := promptSyncAdapter{repo: &fakePromptRepo{}, runner: runner}

	run, err := adapter.StartSync(context.Background())
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, []dkdomain.SyncKind{dkdomain.SyncKindManual}, runner.calls)
}

func TestStartSyncPropagatesInProgress(t *testing.T) {
	runner := &fakeSyncRunner{err: dkdomain.NewError(dkdomain.ErrCodeSyncInProgress)}
	adapter := promptSyncAdapter{repo: &fakePromptRepo{}, runner: runner}

	_, err := adapter.StartSync(context.Background())
	require.Error(t, err)

	dkErr, ok := dkdomain.AsDesignkitError(err)
	require.True(t, ok)
	assert.Equal(t, dkdomain.ErrCodeSyncInProgress, dkErr.Code,
		"抢不到锁要原样透到界面上：「等这一次跑完再点」")
}

// 代理服务没建起来时给空列表，不要报错 ——
// 那样管理员至少还能看到「现在是直连」并且照常点同步。
func TestListProxyOptionsWithoutProxyService(t *testing.T) {
	adapter := promptSyncAdapter{repo: &fakePromptRepo{}}

	options, err := adapter.listProxyOptions(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, options)
	assert.Empty(t, options)
}

// 设置键名是**跨包契约**：管理界面（本包）写它、同步器（dkservice）读它。
//
// 两边不一致的后果最难查：界面显示「已选代理 3」，同步却读不到、老老实实走直连 ——
// 正是「以为走了代理其实没走」这个最糟的状态。所以本包的常量直接引用同步器那个，
// 这里再钉一次字面量，免得哪天有人把两边一起改了。
func TestPromptSyncProxySettingKeyIsStable(t *testing.T) {
	assert.Equal(t, "prompt_sync_proxy_id", SettingKeyPromptSyncProxyID)
	assert.Equal(t, dkservice.SettingKeyPromptSyncProxyID, SettingKeyPromptSyncProxyID,
		"管理界面写的键和同步器读的键必须是同一个")
}

// 装配：同步器接上了，「立即同步」才真的能点。
func TestNewInspirationSyncWiresRunner(t *testing.T) {
	m := &Module{repo: &fakePromptRepo{}}
	m.newInspirationSync()

	require.NotNil(t, m.inspiration, "自动同步的定时器要靠这个具体类型 Start / Close")
	require.NotNil(t, m.promptSync, "接不上同步器的话，「立即同步」永远只会回一句「还没准备好」")
	assert.Empty(t, m.degraded, "一切正常时不该记降级")
}

// 数据库都没有时只降级，**不 panic 也不返回错误** —— 我们是附加模块，
// 灵感库同步挂了不该把 monica 的整个网关拖下水。
func TestNewInspirationSyncWithoutRepoDegrades(t *testing.T) {
	m := &Module{}
	m.newInspirationSync()

	assert.Nil(t, m.promptSync)
	assert.Nil(t, m.inspiration)
	assert.NotEmpty(t, m.degraded, "降级原因要写出来，否则线上表现是「按钮点了没反应、日志里什么都没有」")
}

// 出图队列没建起来（缺对象存储/图像服务/网关）时，灵感库自动同步**照样要起** ——
// 运营翻词、复制词不需要出图链路是好的。
func TestStartRunsInspirationSyncWithoutWorker(t *testing.T) {
	m := &Module{repo: &fakePromptRepo{}}
	m.newInspirationSync()
	require.NotNil(t, m.inspiration)

	// Start 之后立刻 Close：自动同步的第一次「补跑」要等 60 秒才发生，
	// 所以这条用例只验证「起得来、关得掉」，不会真的去碰数据库。
	require.NoError(t, m.Start(context.Background()), "没有出图队列不该让 Start 报错")
	require.NoError(t, m.Close())
}

// 装配出来的东西确实满足 handler 那两个接口（编译期就该挡住，这里再钉一次）。
func TestPromptAdaptersSatisfyHandlerPorts(t *testing.T) {
	var _ dkhandler.PromptService = promptServiceAdapter{}
	var _ dkhandler.PromptSyncService = promptSyncAdapter{}

	// 同步器缺席不是「装配失败」，只是「立即同步」按钮不可用 ——
	// 同步状态和代理设置照常给得出来。
	adapter := promptSyncAdapter{repo: &fakePromptRepo{}}
	_, err := adapter.StartSync(context.Background())
	assert.Error(t, err, "没有同步器时要明说，不能假装同步成功了")

	settings, err := adapter.GetSyncSettings(context.Background())
	require.NoError(t, err, "同步器缺席不该连设置都读不出来")
	assert.Nil(t, settings.ProxyID)
}
