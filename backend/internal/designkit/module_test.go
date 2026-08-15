//go:build unit

package designkit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	dkservice "github.com/Wei-Shaw/sub2api/internal/designkit/service"
	dkstorage "github.com/Wei-Shaw/sub2api/internal/designkit/storage"
)

// 本文件守的是同一条底线：**designkit 装配失败时返回错误，绝不 panic。**
//
// 我们是附加模块。这里一旦 panic，倒下的不是「出图功能」，是 monica 的整个网关。
// 所以每个用例都在「依赖是假的 / 缺的 / 填错的」前提下跑，断言函数正常返回。
//
// 注意：这些用例**不连数据库**。database/sql 的 Open 只解析 DSN 不建连接，
// 所以只要 DSN 拼得出来，NewModule 就能走完，真正的连通性由健康检查负责。

// newTestConfig 造一份「DSN 拼得出来、但连不上任何真实数据库」的配置。
func newTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Timezone = "Asia/Shanghai"
	cfg.Database.Host = "127.0.0.1"
	cfg.Database.Port = 65432
	cfg.Database.User = "designkit_test"
	cfg.Database.Password = "designkit_test"
	cfg.Database.DBName = "designkit_test"
	cfg.Database.SSLMode = "disable"
	return cfg
}

// isolateEnv 把本模块读的四个环境变量固定住，避免被开发机 / CI 上已有的值干扰。
func isolateEnv(t *testing.T) {
	t.Helper()
	t.Setenv(dkstorage.EnvStorageDir, t.TempDir())
	t.Setenv(dkservice.EnvImgsvcURL, "http://127.0.0.1:65000")
	t.Setenv(dkservice.EnvImgsvcToken, "")
	t.Setenv(dkservice.EnvImgsvcTimeoutSeconds, "")
}

func TestNewModuleNilConfigReturnsError(t *testing.T) {
	isolateEnv(t)

	m, err := NewModule(nil, nil, nil)
	if err == nil {
		t.Fatal("配置为空时必须返回错误，而不是往下走到 nil 解引用")
	}
	if m != nil {
		t.Fatalf("构造失败时不该返回模块实例，实际 %#v", m)
	}
}

func TestNewModuleEmptyDBNameReturnsError(t *testing.T) {
	isolateEnv(t)

	cfg := newTestConfig()
	cfg.Database.DBName = ""

	m, err := NewModule(cfg, nil, nil)
	if err == nil {
		t.Fatal("database.dbname 为空时必须早点报错")
	}
	if m != nil {
		t.Fatal("构造失败时不该返回模块实例")
	}
	// 错误文案要指名道姓地说是哪个配置项，不然运维只能猜。
	if !strings.Contains(err.Error(), "database.dbname") {
		t.Fatalf("错误信息里应该点名 database.dbname，实际：%v", err)
	}
}

// 缺上游依赖（拿不到出图网关、拿不到 API Key 服务）时，模块必须**降级**而不是报错——
// designkit 挂不上不该把整个 Sub2API 拖死。
func TestNewModuleMissingUpstreamDepsDegrades(t *testing.T) {
	isolateEnv(t)

	m, err := NewModule(newTestConfig(), nil, nil)
	if err != nil {
		t.Fatalf("缺上游依赖只该降级，不该返回错误：%v", err)
	}
	if m == nil {
		t.Fatal("降级时仍然要返回模块实例，否则健康检查连「模块挂了」都报不出来")
	}
	t.Cleanup(func() { _ = m.Close() })

	if m.worker != nil {
		t.Fatal("没有出图网关时不该把后台队列起起来")
	}
	// 出图队列起不来，但**业务端点必须照常注册**：
	// 报价、查历史、取已经出好的图都跟网关无关，把整组端点关掉等于
	// 「图像服务没配 → 运营连昨天出的图都看不了」。
	if !m.services.Ready() {
		t.Fatal("缺上游网关不该让业务接口整组不注册")
	}
	if m.jobs == nil {
		t.Fatal("批次服务只依赖数据库，缺上游依赖时也该建起来")
	}
	// 「我的消费」只依赖数据库。任何降级都不该把它关掉 ——
	// 那正是运营看「还剩多少钱」和点「申请额度」的时候（决策 16 / 19）。
	if m.services.Me == nil {
		t.Fatal("「我的消费」只依赖数据库，缺上游依赖时也该可用")
	}
	if err := m.StartupError(); err == nil {
		t.Fatal("降级状态必须记下来，StartupError 不该是 nil")
	}
	if m.ModuleStatus() != ModuleStatusDegraded {
		t.Fatalf("模块状态应该是 degraded，实际 %s", m.ModuleStatus())
	}
	if len(m.DegradedReasons()) == 0 {
		t.Fatal("降级原因要能透给运维看")
	}

	// 队列没建起来时 Start 是空转，不能报错、更不能 panic。
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("队列没建起来时 Start 应该安静返回，实际：%v", err)
	}
}

// 环境变量填错（超时秒数不是正整数）时同样只降级。
func TestNewModuleBadImgsvcTimeoutDegrades(t *testing.T) {
	isolateEnv(t)
	t.Setenv(dkservice.EnvImgsvcTimeoutSeconds, "不是数字")

	m, err := NewModule(newTestConfig(), nil, nil)
	if err != nil {
		t.Fatalf("环境变量填错只该降级：%v", err)
	}
	if m == nil {
		t.Fatal("降级时仍然要返回模块实例")
	}
	t.Cleanup(func() { _ = m.Close() })

	if m.preprocessor != nil {
		t.Fatal("超时秒数填错时不该建出预处理客户端")
	}
	if len(m.degraded) == 0 {
		t.Fatal("降级原因必须记下来，否则运维在日志里看不到")
	}
}

// stubHealthRepo 是一个「数据库好好的」的假实现。
type stubHealthRepo struct {
	err error
}

func (s stubHealthRepo) Ping(context.Context) error { return s.err }

// 健康检查的 db 字段**只反映数据库**，不许被模块降级污染。
//
// 曾经这里写成「有任何降级项就直接返错、根本不 ping 数据库」，
// 于是 /health 永远是 db=error：图像服务没配 → db=error，而数据库其实好好的。
// 运维照着 db 字段去查数据库，查半天什么也查不出来。
func TestModuleHealthDBFieldOnlyReflectsDatabase(t *testing.T) {
	isolateEnv(t)

	m, err := NewModule(newTestConfig(), nil, nil)
	if err != nil {
		t.Fatalf("构造不该失败：%v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if len(m.degraded) == 0 {
		t.Fatal("前提没成立：这个用例需要一个降级中的模块")
	}

	// 数据库是通的 → db 必须是 ok，哪怕模块整体是降级的。
	m.healthRepo = stubHealthRepo{}
	if err := m.health.CheckDB(context.Background()); err != nil {
		t.Fatalf("数据库通的时候 db 必须报 ok，实际：%v", err)
	}
	// 模块降级从另一个字段透出去。
	if m.ModuleStatus() != ModuleStatusDegraded {
		t.Fatalf("模块状态应该是 degraded，实际 %s", m.ModuleStatus())
	}

	// 数据库真的不通时才报错。
	m.healthRepo = stubHealthRepo{err: errors.New("connection refused")}
	if err := m.health.CheckDB(context.Background()); err == nil {
		t.Fatal("数据库不通时健康检查不能自称健康")
	}

	// 连模块都没有时也不能 panic。
	var missing moduleHealthRepo
	if err := missing.Ping(context.Background()); !errors.Is(err, dkservice.ErrDatabaseUnavailable) {
		t.Fatalf("没有模块时应返回 ErrDatabaseUnavailable，实际：%v", err)
	}
}

// 模块整体状态的三个取值都要说得清。
func TestModuleStatusValues(t *testing.T) {
	var missing *Module
	if missing.ModuleStatus() != ModuleStatusDown {
		t.Fatalf("没装配起来的模块应该是 down，实际 %s", missing.ModuleStatus())
	}
	if len(missing.DegradedReasons()) == 0 {
		t.Fatal("没装配起来时也要给一条原因")
	}

	healthy := &Module{}
	if healthy.ModuleStatus() != ModuleStatusOK {
		t.Fatalf("没有降级项时应该是 ok，实际 %s", healthy.ModuleStatus())
	}

	broken := &Module{startupErr: errors.New("数据库连接池建不出来")}
	if broken.ModuleStatus() != ModuleStatusDown {
		t.Fatalf("致命错误应该是 down，实际 %s", broken.ModuleStatus())
	}
	if got := broken.DegradedReasons(); len(got) == 0 || !strings.Contains(got[0], "连接池") {
		t.Fatalf("致命错误要排在原因列表第一条，实际 %v", got)
	}
}

// nil 模块上的每个方法都要能安全调用：RegisterRoutes 失败分支里传出去的就可能是它。
func TestNilModuleIsSafe(t *testing.T) {
	var m *Module

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("nil 模块的 Start 应返回 nil，实际：%v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("nil 模块的 Close 应返回 nil，实际：%v", err)
	}
	if err := m.StartupError(); err == nil {
		t.Fatal("nil 模块必须报「没有装配起来」")
	}
	m.RegisterRoutes(RouteDeps{}) // 不 panic 即可
}

// Close 可以重复调用（信号处理和显式关闭可能同时发生）。
func TestModuleCloseIsIdempotent(t *testing.T) {
	isolateEnv(t)

	m, err := NewModule(newTestConfig(), nil, nil)
	if err != nil {
		t.Fatalf("构造不该失败：%v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("第一次 Close 出错：%v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("重复 Close 出错：%v", err)
	}
}

// 关停预算必须留在上游那 5 秒窗口以内，否则等于没关。
//
// backend/cmd/server/main.go 收到信号后只给 5 秒，之后进程直接消失。
// 这个断言就是防止有人把它调大 —— 调大之后队列不会交还租约，
// 在途的图会一直挂着 running，等 180 秒租约过期才被巡检回收。
func TestWorkerStopBudgetFitsUpstreamWindow(t *testing.T) {
	const upstreamShutdownWindow = 5 * time.Second
	if workerStopBudget >= upstreamShutdownWindow {
		t.Fatalf("关停预算 %v 必须小于上游的 %v", workerStopBudget, upstreamShutdownWindow)
	}
}

// 没有连接池时不该返回一个会 panic 的回调。
func TestActiveJobIDsFuncWithoutDB(t *testing.T) {
	if fn := activeJobIDsFunc(nil); fn != nil {
		t.Fatal("没有连接池时应返回 nil 回调，交给 worker 自己判空")
	}
}

// 「本月」的窗口必须是左闭右开的整月，且落在 UTC 上。
//
// 右端点写成「下月 1 号 0 点」而不是「本月最后一天 23:59:59」：
// 后者会漏掉最后一秒里的账单，一年漏 12 次，对不上账的时候没人查得出来。
func TestCurrentMonthRange(t *testing.T) {
	from, to := currentMonthRange(time.Date(2026, 8, 13, 15, 4, 5, 0, time.UTC))

	if got := from.Format(time.RFC3339); got != "2026-08-01T00:00:00Z" {
		t.Fatalf("窗口起点应该是本月 1 号 0 点（UTC），实际 %s", got)
	}
	if got := to.Format(time.RFC3339); got != "2026-09-01T00:00:00Z" {
		t.Fatalf("窗口终点应该是下月 1 号 0 点（UTC），实际 %s", got)
	}
	if !to.After(from) {
		t.Fatal("窗口必须是正区间")
	}
}

// 12 月要能正确跨到下一年，别算成 13 月。
func TestCurrentMonthRangeCrossesYear(t *testing.T) {
	_, to := currentMonthRange(time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC))
	if got := to.Format(time.RFC3339); got != "2027-01-01T00:00:00Z" {
		t.Fatalf("12 月的下一个月是次年 1 月，实际 %s", got)
	}
}

// 没写说明时存 NULL 而不是空串：空串和 NULL 混着存会让
// 管理员后台「有没有写说明」这类判断失准。
func TestOptionalNote(t *testing.T) {
	if optionalNote("   ") != nil {
		t.Fatal("空白说明应该存 NULL")
	}
	got := optionalNote("  还差 30 张  ")
	if got == nil || *got != "还差 30 张" {
		t.Fatalf("说明要去掉首尾空白后原样保留，实际 %v", got)
	}
}
