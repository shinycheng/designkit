// Package handler 是 designkit 模块的 HTTP 层。
//
// 合并纪律（CLAUDE.md 第一节）：上游文件里只允许在
// backend/internal/server/router.go 的 registerRoutes 里留一行注册调用，
// 其余全部代码都在 internal/designkit 下。
// 特别地：**不要**往 handler.Handlers 结构体加字段——
// ① 本包 import internal/handler，反过来 handler.go 再 import 本包就是循环依赖；
// ② 真正赋值的是 handler/wire.go 的 ProvideHandlers，wire.go 不在白名单里，
// 加了字段也永远是 nil。
//
// # 本文件只做「挂路由」，不做装配
//
// 建连接池、建 repository、建对象存储、起后台队列这些事全在
// internal/designkit/module.go（组合根）里。
//
// 原来它们都写在这个文件里，结果是：这里只建了一个连接池，然后往
// RegisterBusinessRoutes 传了一个空的 Services{}，整条出图链路从来没被构造过。
// 装配和挂路由分开之后，本文件的职责只剩一句话：**把已经建好的东西挂上去**。
//
// 依赖方向：designkit（组合根）→ designkit/handler → designkit/service。
// **本包永远不许 import 组合根包**，反了就是循环依赖，编译不过。
package handler

import (
	"log/slog"

	"github.com/Wei-Shaw/sub2api/internal/config"
	dkservice "github.com/Wei-Shaw/sub2api/internal/designkit/service"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// MountOptions 是挂载 designkit 路由要的全部东西。
// 全部由组合根（internal/designkit）建好之后传进来，本包不自己 new 任何依赖。
type MountOptions struct {
	// Engine 根路由。机器（ERP）那条 /v1/designkit 从它派生。
	Engine *gin.Engine
	// V1 上游的 /api/v1 组。浏览器那条 /api/v1/designkit 从它派生。
	V1 *gin.RouterGroup
	// JWTAuth 浏览器侧鉴权。为 nil 时浏览器组不挂鉴权（只应出现在测试里）。
	JWTAuth middleware.JWTAuthMiddleware
	// APIKeyAuth 上游的 API Key 鉴权，只给 POST /jobs 用。
	APIKeyAuth middleware.APIKeyAuthMiddleware
	// APIKeyService ERP 读接口的轻量 Key 鉴权要用它查 Key。
	APIKeyService *upstreamservice.APIKeyService
	// SettingService 面板限流器要读系统设置里的阈值。
	SettingService *upstreamservice.SettingService
	// Config KeyAuth 要读 IP 白黑名单等配置。
	Config *config.Config
	// Redis 面板限流器的计数落在它上面。
	Redis *redis.Client
	// Health 健康检查服务。为 nil 时健康检查会如实报 db=error。
	Health *dkservice.HealthService
	// Services 业务实现。任何一个为 nil，业务端点整组不注册（只留健康检查）。
	Services Services
}

// Mount 把 designkit 挂进上游路由树。
//
// 两个挂载点，同一批 handler，service 完全共用（CLAUDE.md 第二节）：
//
//	浏览器（运营）→ /api/v1/designkit/*，挂上游 jwtAuth
//	机器（ERP）  → /v1/designkit/*，跟 /v1/images/edits 同一个命名空间
//
// 为什么不能合成一条：两个鉴权中间件**都只认 Bearer**，靠「JWT 有两个点、
// API Key 没有」分流不可靠（Key 前缀是可配置项）。分成两条路径零猜测，
// 还顺带避开 API Key 认证失败的 IP 连坐。
func Mount(opts MountOptions) {
	if opts.Engine == nil || opts.V1 == nil {
		// 挂载点都没有就别硬挂，否则是一个 nil 解引用把整个进程带走。
		slog.Error("designkit 路由没有挂载点，本次不注册任何端点")
		return
	}

	healthHandler := NewHealthHandler(opts.Health)

	// ---- 浏览器（运营）：/api/v1/designkit/* ----
	//
	// 从 v1 派生而不是 Engine.Group("/api/v1/designkit")，是为了继承上游
	// 以后可能加在 /api/v1 组上的中间件。上游当前的 v1 组是裸的。
	browser := opts.V1.Group("/designkit")
	if opts.JWTAuth != nil {
		browser.Use(gin.HandlerFunc(opts.JWTAuth))
	}
	// 面板限流（panelRateLimiter）**不会**自动继承，业务端点在
	// RegisterBusinessRoutes 里自己挂 Global()（240 次/分/用户）。
	// 健康检查刻意不挂：它就是给监控高频探活用的，挂了反而会被 429。
	// 也永远不要给任何 designkit 端点挂 Heavy()——那档只有 60 次/分，
	// 还跟运营在别处的重查询共用一个桶（CLAUDE.md B8）。
	browser.GET("/health", healthHandler.Health)

	// ---- 机器（ERP）：/v1/designkit/* ----
	//
	// 从 Engine 派生而不是复用上游的 gateway 组，这样**不会**继承它的
	// apiKeyAuth / 计费准入链——那正是设计定型 3.1 要避开的：
	// Key 额度耗尽时连「查任务」「取已经付过钱的图」都会被拦掉。
	machine := opts.Engine.Group("/v1/designkit")
	// 错误格式改写中间件（ErrorEnvelope）挂在业务端点那几个子组的最前面，
	// 见 register_business.go —— 它把上游三种互不兼容的错误 JSON 统一成
	// {"error":{"type","error_code","message","request_id"}}，并回 X-Designkit-Request-Id。
	// 健康检查**免鉴权**：探活不该依赖任何一把 Key 是否有效。
	// 它也刻意不挂错误格式改写中间件——固定返 200，没有错误体要改。
	machine.GET("/health", healthHandler.Health)

	// ---- 业务端点（设计定型 第三节）----
	//
	// 错误格式改写、轻量 Key 鉴权、限流、两个前缀怎么分组，全在
	// register_business.go 里，见那个文件的表格。
	var keyAuth gin.HandlerFunc
	if opts.APIKeyService != nil {
		keyAuth = KeyAuth(opts.APIKeyService, opts.Config)
	}

	// 面板限流器在这里 New 一个：上游那一个建在 registerRoutes 里、没有传给我们。
	// 它本身是无状态包装，计数落在 Redis 的同一个 key 上（panel:global:user:<id>），
	// 所以 New 一个**不会**另开一套额度，运营在 designkit 和在上游面板共用同一个桶。
	RegisterBusinessRoutes(BusinessRouteOptions{
		Browser:          browser,
		Machine:          machine,
		Services:         opts.Services,
		PanelRateLimiter: middleware.NewPanelRateLimiter(opts.Redis, opts.SettingService),
		KeyAuth:          keyAuth,
		APIKeyAuth:       opts.APIKeyAuth,
	})
}
