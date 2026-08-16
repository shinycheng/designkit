package handler

import (
	"log/slog"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	upstreammw "github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 业务端点的挂载（设计定型 第三节）
// ============================================================================
//
// 同一批 handler 挂两个前缀，service 完全共用：
//
//	| 用途             | 前缀                 | 鉴权                     |
//	|------------------|----------------------|--------------------------|
//	| 浏览器（运营）   | /api/v1/designkit    | 上游 jwtAuth             |
//	| 机器（ERP）读    | /v1/designkit        | **自写轻量 Key 鉴权**     |
//	| 机器（ERP）写    | /v1/designkit        | 上游 apiKeyAuth（提交、重试）|
//
// 为什么读接口不能挂上游 apiKeyAuth：它的计费准入只对三条路径跳过，
// /v1/designkit/* 不在里面 → Key 额度耗尽或余额 ≤0 时，
// 连「查任务」「取已经付过钱的图」都会被拦掉，而我们恰恰建议给 ERP Key
// 设日消费封顶。**额度用尽只影响提交新任务，不影响查询和取图** —— 写进 ERP 文档。
//
// 为什么两个前缀不能合成一条：两个鉴权中间件**都只认 Bearer**，
// 靠「JWT 有两个点、API Key 没有」分流不可靠（Key 前缀是可配置项）。
// 分成两条路径零猜测，还顺带避开 API Key 认证失败的 IP 连坐。

// BusinessRouteOptions 是挂载业务端点需要的东西。
type BusinessRouteOptions struct {
	// Browser 浏览器组，调用方已经挂好 jwtAuth（/api/v1/designkit）。
	Browser *gin.RouterGroup
	// Machine 机器组，**没有挂任何鉴权**（/v1/designkit），鉴权在这里按端点分别挂。
	Machine *gin.RouterGroup
	// Services 业务实现。出图那四个（素材/比例/批次/结果图）缺一个就整组不注册；
	// Me 缺席只关掉它自己那两个端点。
	Services Services
	// PanelRateLimiter 面板限流器；nil 表示不限流。
	PanelRateLimiter *upstreammw.PanelRateLimiter
	// KeyAuth ERP 读接口用的轻量 Key 鉴权；nil 表示不挂（只应出现在测试里）。
	KeyAuth gin.HandlerFunc
	// APIKeyAuth 上游的 API Key 鉴权，只给花钱的那两个端点用（提交、重试）。
	APIKeyAuth upstreammw.APIKeyAuthMiddleware
}

// RegisterBusinessRoutes 把本轮实现的端点挂上去。
//
// **每一块都是可缺席的**：缺哪块就只关掉哪块的端点，其余照常挂。
//
// ⚠ 这里原来是「Services.Ready() 不成立就整组不挂」，那样有个很现实的坏后果：
// 出图三件套（素材 / 比例 / 结果图）都依赖对象存储，而
// DESIGNKIT_STORAGE_DIR 建不出来在群晖上是常见情况（权限没配对）。
// 一旦如此，连「看看灵感库里有哪些提示词」「查昨天的任务」都会 404，
// 运营看到的是整个功能消失，而真正坏掉的只是「传不了图」。
// 灵感库和「我的消费」只依赖数据库，没有任何理由被出图故障连坐。
func RegisterBusinessRoutes(opts BusinessRouteOptions) {
	var assets *AssetHandler
	if opts.Services.Assets != nil {
		assets = NewAssetHandler(opts.Services.Assets, opts.Services.Images)
	}
	var catalog *CatalogHandler
	if opts.Services.Catalog != nil {
		catalog = NewCatalogHandler(opts.Services.Catalog)
	}
	var jobs *JobHandler
	if opts.Services.Jobs != nil {
		jobs = NewJobHandler(opts.Services.Jobs)
	}
	var images *ImageHandler
	if opts.Services.Images != nil {
		images = NewImageHandler(opts.Services.Images)
	}
	if assets == nil || catalog == nil || jobs == nil || images == nil {
		slog.Warn("designkit 出图相关服务未就绪：上传 / 报价 / 提交 / 结果图这几组端点不可用，" +
			"灵感库和消费查询照常。多半是图片存储目录没建出来，看启动日志里的降级原因")
	}

	// 灵感库跟「我的消费」一样是**可缺席**的：它只依赖数据库，
	// 缺席的唯一后果是「灵感库」那一页空着 —— 运营还可以手打提示词照常出图。
	// 为它把出图端点整组下线完全不成比例。
	var prompts *PromptHandler
	if opts.Services.Prompts != nil {
		prompts = NewPromptHandler(opts.Services.Prompts, opts.Services.PromptSync, opts.Services.Suggest)
	} else {
		slog.Warn("designkit 灵感库服务未就绪：分类、检索、同步这几个端点不可用，" +
			"运营仍可手打提示词出图")
	}

	// 「我的消费」缺席时**只关掉它自己那两个端点**，其余照常挂 ——
	// 它只依赖数据库，为它把整组出图端点下线完全不成比例。
	var me *MeHandler
	if opts.Services.Me != nil {
		me = NewMeHandler(opts.Services.Me)
	} else {
		slog.Warn("designkit 消费服务未就绪：工作台角落的「本月出图 / 花费 / 余额」" +
			"和「申请额度」按钮不可用，出图和查询照常")
	}

	// 「AI 对话」同样可缺席：只关它自己那四个端点（决策 38）。
	var chat *ChatHandler
	if opts.Services.Chat != nil {
		chat = NewChatHandler(opts.Services.Chat)
	} else {
		slog.Warn("designkit 对话服务未就绪：「AI 对话」页不可用，出图和灵感库照常")
	}

	// 文案检查：纯计算（词库编译期 embed），没有任何服务依赖，永远可用。
	contentCheck := NewContentCheckHandler()

	// 「高清放大」可缺席：缺席时那两条路由**整个不挂**（裸 404）。
	// 前端跟「AI 推荐」同一套约定：裸 404 = 功能没上线，显示「还没准备好」。
	var upscale *UpscaleHandler
	if opts.Services.Upscale != nil {
		upscale = NewUpscaleHandler(opts.Services.Upscale)
	} else {
		slog.Warn("designkit 高清放大服务未就绪：「高清放大」按钮不可用，上传和出图照常")
	}

	// ---- 浏览器（运营）----
	//
	// 面板限流**不会**自动继承（上游的 /api/v1 组是裸的，各模块自己 Use 才生效），
	// 所以这里显式挂 Global()：240 次/分/用户，够运营点。
	// **绝不挂 Heavy()**（只有 60 次/分，而且跟运营在别处的重查询共用一个桶）。
	if opts.Browser != nil {
		web := opts.Browser.Group("")
		web.Use(ErrorEnvelope(), MountContext(dkdomain.OriginWeb, BrowserMountPrefix))
		if opts.PanelRateLimiter != nil {
			web.Use(opts.PanelRateLimiter.Global())
		}
		web.Use(RequireAuthenticated())
		mountCommonRoutes(web, assets, catalog, jobs, images, prompts)
		mountMeRoutes(web, me)
		mountBillableRoutes(web, jobs)
		// 对话：读写都挂浏览器组。发送那条也花钱，但浏览器侧的余额判定
		// 在上游计费链里做，这里不再单独分组。
		mountChatReadRoutes(web, chat)
		mountChatSendRoute(web, chat)
		// 文案检查：不花钱、无依赖，跟查询同一组鉴权。
		mountContentCheckRoutes(web, contentCheck)
		// 高清放大：不花钱（本地推理），跟查询同一组鉴权。别挂 Heavy 限流。
		mountUpscaleRoutes(web, upscale)
		// 灵感库同步**只挂浏览器组**，见 mountPromptAdminRoutes 的注释。
		mountPromptAdminRoutes(web, prompts, opts.Services.PromptSync)
		// designkit 设置同理：只挂浏览器组、只放管理员。
		mountSettingsAdminRoutes(web, opts.Services.Settings)
	}

	// ---- 机器（ERP）----
	if opts.Machine != nil {
		// 读 + 不花钱的写（上传素材、报价）：轻量 Key 鉴权，不做计费准入。
		erpRead := opts.Machine.Group("")
		erpRead.Use(ErrorEnvelope(), MountContext(dkdomain.OriginERP, MachineMountPrefix))
		if opts.KeyAuth != nil {
			erpRead.Use(opts.KeyAuth)
		}
		erpRead.Use(RequireAuthenticated())
		mountCommonRoutes(erpRead, assets, catalog, jobs, images, prompts)
		mountMeRoutes(erpRead, me)
		mountChatReadRoutes(erpRead, chat)
		// 文案检查对 ERP 也开放（决策 4：界面上每一步都有接口）。
		// 挂读组：它不花钱，额度耗尽时照样能查。
		mountContentCheckRoutes(erpRead, contentCheck)
		// 高清放大对 ERP 也开放。挂读组：不花钱，额度耗尽时照样能放大和轮询。
		mountUpscaleRoutes(erpRead, upscale)

		// 真正花钱的那两个：走上游 apiKeyAuth，该拦的额度就得拦。
		//
		// ⚠ ERP 轮询进度走的是上面那组，**这里不要挂任何面板限流**：
		// Global() 按 userID 计数、跟面板共用一个桶，ERP 高频轮询会把运营挤掉。
		erpWrite := opts.Machine.Group("")
		erpWrite.Use(ErrorEnvelope(), MountContext(dkdomain.OriginERP, MachineMountPrefix))
		if opts.APIKeyAuth != nil {
			erpWrite.Use(gin.HandlerFunc(opts.APIKeyAuth))
		}
		erpWrite.Use(RequireAuthenticated())
		mountBillableRoutes(erpWrite, jobs)
		// 对话发送对 ERP 也开放（决策 4：界面上每一步都有接口）。
		// 跟提交批次同组：走上游 apiKeyAuth，该拦的额度就得拦。
		mountChatSendRoute(erpWrite, chat)
	}
}

// mountChatReadRoutes 对话的三个只读端点。
func mountChatReadRoutes(g *gin.RouterGroup, chat *ChatHandler) {
	if g == nil || chat == nil {
		return
	}
	g.GET("/chat/sessions", chat.ListSessions)
	g.GET("/chat/sessions/:uid", chat.GetSession)
	g.DELETE("/chat/sessions/:uid", chat.DeleteSession)
}

// mountChatSendRoute 对话的发送端点（花钱的那一个）。
func mountChatSendRoute(g *gin.RouterGroup, chat *ChatHandler) {
	if g == nil || chat == nil {
		return
	}
	g.POST("/chat/messages", chat.Send)
}

// mountUpscaleRoutes 高清放大的两个端点（排队 + 查状态）。
//
// 挂在 /assets/:uid 下：放大的输入和产出都是**商品图**（asset），
// 结果编号直接能塞进 POST /jobs 的 asset_uids。
// ⚠ 轮询端点绝不能挂 Heavy 限流（60 次/分且跟重查询共用桶，见 CLAUDE.md B8）。
func mountUpscaleRoutes(g *gin.RouterGroup, h *UpscaleHandler) {
	if g == nil || h == nil {
		return
	}
	g.POST("/assets/:uid/upscale", h.Start)
	g.GET("/assets/:uid/upscale", h.Status)
}

// mountContentCheckRoutes 文案检查（违禁词 + 标题字数）。
//
// 用 POST 而不是 GET：待检文案可能几千字，塞 query string 会撞 URL 长度限制，
// 而且会整段进访问日志。POST body 两个问题都没有。
func mountContentCheckRoutes(g *gin.RouterGroup, h *ContentCheckHandler) {
	if g == nil || h == nil {
		return
	}
	g.POST("/content/check", h.Check)
}

// mountCommonRoutes 挂**不花钱**的那些端点（见 mountBillableRoutes 是哪两个花钱）。
//
// 为什么 POST /assets、POST /jobs/estimate、POST /jobs/:uid/stop 也算在这里：
// 它们**不花钱**。上传素材、看报价、停止排队在额度耗尽时仍然该能用 ——
// 不然运营/ERP 连「还差多少钱」都看不到，更别说把还在排队的图停下来止损。
func mountCommonRoutes(g *gin.RouterGroup, assets *AssetHandler, catalog *CatalogHandler, jobs *JobHandler, images *ImageHandler, prompts *PromptHandler) {
	// 每一块都判空：缺哪块只少哪几条路由，不影响别的。

	// 素材
	if assets != nil {
		g.POST("/assets", assets.Create)
		g.GET("/assets/:uid", assets.Get)
		g.GET("/assets/:uid/content", assets.Content)

		// 「继续生成」：把一张出好的结果图变成新的商品图，接着出下一轮。
		//
		// 路径放在 /assets 下而不是 /images 下，是因为**产出物是一条 asset**——
		// 调用方拿到的是商品图编号，接着就能塞进 POST /jobs 的 asset_uids。
		// 挂在 /images/:uid/continue 会让人以为返回的是图片。
		//
		// 结果图服务缺席时整条不挂（跟别处一个规矩：宁可 404，
		// 也不要挂上去再返回一个运营看不懂的错误）。
		if assets.hasContinue() {
			g.POST("/assets/from-image/:uid", assets.ContinueFromImage)
		}

		// 「生成白底图」：抠掉背景、合成白底，存成一条**新的**商品图。
		// **不花钱**（走本地 rembg 容器，不经过出图网关），所以挂在这一组。
		// 抠图服务没配置时路由照挂：service 会返回中文的「还没准备好」，
		// 比裸 404 说得清楚（这条不像 AI 推荐那样有「按 404 判功能未上线」的前端约定）。
		g.POST("/assets/:uid/remove-background", assets.RemoveBackground)
		// TODO(designkit): DELETE /assets/:uid、POST /assets/:uid/preprocess
	}

	// 报价
	if catalog != nil {
		g.GET("/ratios", catalog.Ratios)
	}

	// 生成（查询侧）
	if jobs != nil {
		g.POST("/jobs/estimate", jobs.Estimate)
		g.GET("/jobs", jobs.List)
		g.GET("/jobs/:uid", jobs.Get)
		g.GET("/jobs/:uid/items", jobs.Items)
		g.GET("/jobs/:uid/items/:seq", jobs.Item)
		g.GET("/jobs/:uid/items/:seq/content", jobs.ItemContent)

		// 「停止排队」算在这一组：它**不花钱**，正相反 —— 它是运营用来**别再花钱**的。
		// 挂在花钱那一组会被上游的计费准入拦掉：额度耗尽时最需要能停的人反而停不了。
		g.POST("/jobs/:uid/stop", jobs.Stop)
		// TODO(designkit): DELETE /jobs/:uid
	}

	// 结果图
	if images != nil {
		g.GET("/images/:uid", images.Get)
		g.GET("/images/:uid/content", images.Content)
	}

	// 灵感库（浏览）。prompts 为 nil 时整块不挂，其余端点照常。
	//
	// ERP 组也挂：外部系统提交任务时要按 prompt 的 uid 选词，
	// 不给它一个「列出有哪些词」的地方，对接方只能靠人肉抄编号。
	if prompts != nil {
		g.GET("/prompt-categories", prompts.Categories)
		g.GET("/prompts", prompts.List)
		g.GET("/prompts/:uid", prompts.Get)

		// 「AI 挑提示词」。**服务缺席时刻意整条不挂**，让它自然 404。
		//
		// 前端 isSuggestUnavailableError 正是靠「裸 404（响应体里没有 DK_ 前缀的错误码）」
		// 来区分两件事：功能整个没上线 → 显示「AI 推荐还没准备好，请联系管理员，
		// 这期间去灵感库自己挑一条」；商品图不存在之类的业务错误 → 显示对应的中文。
		// 挂上去再返回一个 DK_ 错误码的话，第一种情况会被误显示成「这次没推荐出来，
		// 再点一次试试」—— 运营会一直点，而它永远不会成功。
		//
		// ERP 组也挂：外部系统同样可能想让 AI 配一条词再提交。
		if prompts.hasSuggest() {
			g.POST("/prompts/suggest", prompts.SuggestPrompt)
		}
	}
	// TODO(designkit): POST /prompts、PUT|DELETE /prompts/:uid（运营自己存的提示词）
}

// mountPromptAdminRoutes 挂灵感库同步那四个端点。**仅管理员**。
//
// 为什么只挂浏览器组、不挂 ERP 组：同步是运维动作，要用到代理配置
// （那是管理员在后台才看得到的东西）。ERP 那把 Key 是给外部系统读写任务的，
// 给它一个「把全站灵感库刷一遍」的按钮没有任何好处，只会多一个误触面。
//
// syncSvc 为 nil 时整块不挂（同步服务缺席），浏览照常 —— 运营那一页不受影响。
//
// ⚠ RequireAdmin 必须挂在 RequireAuthenticated 后面：它从上下文取角色，
// 而角色是鉴权中间件塞进去的。
func mountPromptAdminRoutes(g *gin.RouterGroup, prompts *PromptHandler, syncSvc PromptSyncService) {
	if prompts == nil || syncSvc == nil {
		if prompts != nil {
			slog.Warn("designkit 灵感库同步服务未就绪：管理员看不到「立即同步」和「同步走的代理」，" +
				"灵感库的浏览和搜索照常")
		}
		return
	}

	admin := g.Group("")
	admin.Use(RequireAdmin())
	admin.POST("/prompts/sync", prompts.StartSync)
	admin.GET("/prompts/sync/latest", prompts.SyncLatest)
	admin.GET("/prompts/sync/settings", prompts.SyncSettings)
	admin.PUT("/prompts/sync/settings", prompts.UpdateSyncSettings)
}

// mountSettingsAdminRoutes 挂 designkit 设置那两个端点。**仅管理员**。
//
// 为什么只挂浏览器组、不挂 ERP 组：改并发数、改单价是运维动作。
// 给外部系统那把 Key 一个「把出图并发调到 8」的入口没有任何好处，只多一个误触面。
//
// svc 为 nil 时整块不挂（配置服务缺席，通常意味着数据库没连上），
// 出图仍按数据库里已有的配置跑 —— 设置页在前端会显示成「这个功能还没准备好」。
//
// ⚠ RequireAdmin 必须挂在 RequireAuthenticated 后面：它从上下文取角色，
// 而角色是鉴权中间件塞进去的。
func mountSettingsAdminRoutes(g *gin.RouterGroup, svc SettingsService) {
	if svc == nil {
		slog.Warn("designkit 配置服务未就绪：管理员打不开「商品图设置」那一页，" +
			"出图会继续按数据库里已有的配置跑")
		return
	}

	handler := NewSettingsHandler(svc)
	admin := g.Group("")
	admin.Use(RequireAdmin("商品图设置只有管理员能改，需要调整的话请联系管理员。"))
	admin.GET("/admin/settings", handler.Get)
	admin.PUT("/admin/settings", handler.Update)
}

// mountMeRoutes 挂「我的消费」。me 为 nil 时整块不挂，其余端点照常。
//
// 这两个端点都**不花钱**，所以跟查询走同一组鉴权：额度耗尽时正是最需要
// 「看清自己欠多少」和「申请额度」的时候，把它们挂到计费准入后面等于把出路堵死。
func mountMeRoutes(g *gin.RouterGroup, me *MeHandler) {
	if me == nil {
		return
	}
	g.GET("/me/usage/summary", me.UsageSummary)
	g.POST("/me/quota-requests", me.CreateQuotaRequest)
}

// mountBillableRoutes 挂**真正花钱**的两个端点，单独一组鉴权。
//
//	POST /jobs                          提交一批（一次可能是几十张图的钱）
//	POST /jobs/:uid/items/:seq/retry    重试一张 = 重新出一张图 = 重新收一次钱
//
// 两个都要求带 Idempotency-Key（handler 自己判，缺了直接 400）。
func mountBillableRoutes(g *gin.RouterGroup, jobs *JobHandler) {
	// jobs 缺席时整块不挂（跟 mountCommonRoutes 同一个道理）：
	// 提交和重试都要 JobService，没有它挂上去就是空指针。
	if jobs == nil {
		return
	}
	g.POST("/jobs", jobs.Create)
	g.POST("/jobs/:uid/items/:seq/retry", jobs.RetryItem)
}
