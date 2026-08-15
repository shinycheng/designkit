// Package designkit 是 designkit 模块的**组合根**（composition root）：
// 唯一一处把 repository / 对象存储 / 预处理客户端 / 内部 Key / 出图网关 /
// 后台队列 new 出来、串起来、启动、关停的地方。
//
// 为什么要单独有这么一层：
//
//	在这之前，装配散在 handler/routes.go 里，而它只建了一个数据库连接池，
//	然后往 RegisterBusinessRoutes 传了一个空的 Services{} ——
//	RegisterBusinessRoutes 看见 Services 不全就原地返回，于是
//	repository.NewRepository / storage.NewLocalStoreFromEnv /
//	service.NewGatewayInvoker / service.NewInternalKeyService / worker.New
//	在生产代码里**一次都没有被调用过**：整条出图链路是死代码。
//	本文件就是那条缺失的装配线。
//
// 依赖方向（不许反过来，反了就是循环依赖，编译不过）：
//
//	designkit（本包） → designkit/handler → designkit/service → designkit/domain
//	                  → designkit/{db,repository,storage,worker}
//
// 所以本包可以 import handler，**handler 永远不许 import 本包**。
//
// # 环境变量（全模块只在这里读，改一处就够）
//
//	DESIGNKIT_STORAGE_DIR             图片落盘根目录，默认 /app/data/designkit。
//	                                  群晖上没有 S3，这就是唯一的存图位置；
//	                                  建不出来 → 上传和出图整个不可用。
//	DESIGNKIT_IMGSVC_URL              Python 预处理服务地址，默认
//	                                  http://designkit-imgsvc:8000（compose 里的服务名）。
//	DESIGNKIT_IMGSVC_TOKEN            预处理服务的 Bearer Token；服务端没开鉴权就留空。
//	DESIGNKIT_IMGSVC_TIMEOUT_SECONDS  单次预处理超时秒数，默认 60。填了非正整数直接报错，
//	                                  不静默退化 —— 免得运维以为自己改生效了。
//
// 常量本身定义在 storage/local.go 和 service/preprocess_client.go 里，
// 这里只负责「读」和「读不到时给一句运营看得懂的中文提示」。
package designkit

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	dkdb "github.com/Wei-Shaw/sub2api/internal/designkit/db"
	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	dkhandler "github.com/Wei-Shaw/sub2api/internal/designkit/handler"
	dkrepository "github.com/Wei-Shaw/sub2api/internal/designkit/repository"
	dkservice "github.com/Wei-Shaw/sub2api/internal/designkit/service"
	dkstorage "github.com/Wei-Shaw/sub2api/internal/designkit/storage"
	dkworker "github.com/Wei-Shaw/sub2api/internal/designkit/worker"
	upstreamhandler "github.com/Wei-Shaw/sub2api/internal/handler"
	upstreamrepo "github.com/Wei-Shaw/sub2api/internal/repository"
	upstreammw "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// workerStopBudget 是 Close() 留给「停队列 + 交还租约」的时间。
//
// 这个数被上游卡死了：backend/cmd/server/main.go 收到信号后是
//
//	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
//	app.Server.Shutdown(ctx)
//
// 5 秒之后 main 返回、进程直接消失，谁也拦不住。而一张图要 20 秒。
//
// 所以 Close() **不等在途的图跑完**，只做一件事：让 worker 停止领新任务、
// 把手上那几张的租约交还（worker.Stop 内部走
// CompleteItemFailure{Terminal:false, RetryAfter:0}，item 退回 pending、
// worker_id / lease_expires_at 置空），重启后的实例立刻能接着跑。
// 3 秒是「够跑完那一条 UPDATE」和「留 2 秒给连接池收尾」的折中。
//
// 等不完也不是灾难：租约 180 秒后过期，别的 worker 照样能接手，
// 最坏就是这几张多等 3 分钟。
const workerStopBudget = 3 * time.Second

// Module 持有 designkit 的全部进程级资源。
//
// **一个进程只该有一个**（RegisterRoutes 会保证这一点）。
type Module struct {
	// pool 我们自己的数据库连接池。
	// 为什么不复用上游那一个：registerRoutes 拿不到 *sql.DB 也拿不到 *ent.Client，
	// 想拿到就得改 wire_gen.go（已入库的上游文件）。见 db/pool.go 的包注释。
	pool *dkdb.Pool

	// repo 持久化层（只依赖 *sql.DB，完全不碰 ent）。
	repo dkdomain.Repository

	// healthRepo 健康检查专用的 SELECT 1。
	healthRepo dkservice.HealthRepository

	// store 对象存储。群晖上是本地磁盘。
	store dkdomain.ObjectStore

	// preprocessor Python 预处理服务客户端（补白边到目标比例）。
	preprocessor dkdomain.ImagePreprocessor

	// keys 每个运营用户一把「内部专用 API Key」。
	// 出图必须有 Key：上游 Images() 第一件事就是从上下文取 API Key，取不到直接 401。
	keys *dkservice.InternalKeyService

	// gateway 出图调用器（进程内重入上游 h.OpenAIGateway.Images()）。
	gateway dkdomain.GatewayInvoker

	// chat 对话调用器（进程内重入 h.OpenAIGateway.ChatCompletions()）。
	// 只给「AI 挑提示词」用；出图**不走它**。
	chat dkservice.ChatInvoker

	// suggest 「AI 挑提示词」：读商品图 → 分类里挑 5 条 → 合成 1 条。
	//
	// 允许为 nil：它比灵感库浏览多要对象存储（读商品图）和出图网关（调对话模型），
	// 缺任意一个就建不起来。缺席的唯一后果是那一条端点不挂、
	// 前端显示「AI 推荐还没准备好」并提示去灵感库自己挑 —— 出图照常。
	suggest *dkservice.PromptSuggestService

	// assets 商品图上传 / 预处理 / 比例白名单。
	assets *dkservice.AssetService

	// jobs 批次的报价 / 提交 / 查询 / 停止排队 / 重试。
	// 它是整条链路的入口：没有它，运营界面上没有任何按钮能让一张图开始出。
	jobs *dkservice.JobService

	// worker 后台跑批：出图 + 僵尸巡检 + 结算。
	worker *dkworker.Pool

	// proxies 上游的代理管理服务。
	//
	// 只给**灵感库同步**用两件事：列出可选代理（管理员那个下拉框）、
	// 把选中的代理 ID 换成一个可用的代理地址（GetURL，同步器要）。
	//
	// ⚠ 出图**绝不走它**。老代码的血泪教训（原话）：
	// 「不能用全局 urllib 代理或环境变量——那会把生图请求也带进代理」。
	proxies *upstreamservice.ProxyService

	// inspiration 灵感库同步服务（手动同步 + 每 12 小时一次的自动同步）。
	//
	// 单独留一个**具体类型**的字段，是因为它有生命周期：Start 起定时器、
	// Close 停定时器并还掉数据库咨询锁。只留下面那个接口字段的话，
	// 定时器永远起不来，自动同步就成了摆设。
	inspiration *dkservice.InspirationSyncService

	// promptSync 真正干活的灵感库同步器（去开源库拉一万四千条、写库、记同步记录）。
	//
	// **允许为 nil**：缺席时管理员仍然看得到同步状态、也能选代理，
	// 只有「立即同步」按钮会返回一句「还没准备好」。
	// 见 PromptSyncRunner 的注释。
	promptSync PromptSyncRunner

	// services 交给 handler 的业务实现。
	services dkhandler.Services

	// health 健康检查服务，装的是本模块自己的探测逻辑（见 moduleHealthRepo）。
	health *dkservice.HealthService

	// startupErr 致命错误：连数据库连接池都没建起来。
	startupErr error

	// degraded 非致命的降级原因（中文），健康检查和启动日志都会用到。
	degraded []string

	mu        sync.Mutex
	started   bool
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

// NewModule 按依赖顺序把 designkit 的部件建起来。
//
// 参数只取 registerRoutes（backend/internal/server/router.go）**实际拿得到**的东西：
//   - cfg            数据库连接信息；本模块自己 sql.Open 一个池
//   - h              出图要重入 h.OpenAIGateway.Images()
//   - apiKeyService  建 / 取内部专用 API Key
//
// 返回 error 的情况**只有两种**：cfg 不可用、数据库连接池建不出来。
// 其余部件（对象存储、预处理服务、出图网关）缺失一律记成「降级」并继续 ——
// 我们是附加模块，绝不能因为图像服务没配就把 monica 的整个网关拖死。
// 降级的后果写在日志里，健康检查也会返回 status=degraded。
//
// 任何情况下都**不 panic**。
func NewModule(
	cfg *config.Config,
	h *upstreamhandler.Handlers,
	apiKeyService *upstreamservice.APIKeyService,
) (*Module, error) {
	if cfg == nil {
		return nil, errors.New("designkit: 缺少配置（config 为空），模块无法启动")
	}
	if strings.TrimSpace(cfg.Database.DBName) == "" {
		// 早点炸、炸得清楚。留到第一次查询才报错的话，运维看到的是一行
		// 「pq: database \"\" does not exist」，跟配置项对不上号。
		return nil, errors.New("designkit: 配置里的 database.dbname 是空的，无法连接数据库")
	}

	m := &Module{}

	// ---- 1. 连接池 ----
	pool, err := dkdb.Open(cfg)
	if err != nil {
		return nil, fmt.Errorf("designkit: 数据库连接池创建失败：%w", err)
	}
	m.pool = pool

	// ---- 2. repository ----
	// sql.Open 只解析 DSN 不建连接，所以这里拿到的 db 一定非 nil。
	db := pool.DB()
	m.repo = dkrepository.NewRepository(db)
	m.healthRepo = dkrepository.NewHealthRepository(db)
	m.health = dkservice.NewHealthService(moduleHealthRepo{m: m}).
		WithModuleStatus(m.healthModuleStatus)

	// ---- 3. 对象存储 ----
	if store, storeErr := dkstorage.NewLocalStoreFromEnv(); storeErr != nil {
		m.degrade("图片存储目录不可用（检查环境变量 %s，默认 %s）：%v",
			dkstorage.EnvStorageDir, dkstorage.DefaultStorageDir, storeErr)
	} else {
		// 只在成功时赋值：把一个 nil 的 *LocalStore 装进接口，接口本身就不是 nil 了，
		// 后面所有 `if store == nil` 都会失效，直接变成 nil 指针解引用。
		m.store = store
	}

	// ---- 4. 预处理服务（Python）----
	if pre, preErr := dkservice.NewPreprocessClientFromEnv(); preErr != nil {
		m.degrade("图像处理服务地址不可用（检查环境变量 %s / %s）：%v",
			dkservice.EnvImgsvcURL, dkservice.EnvImgsvcTimeoutSeconds, preErr)
	} else {
		m.preprocessor = pre
	}

	// ---- 5. 内部专用 API Key ----
	if apiKeyService == nil {
		m.degrade("拿不到上游的 API Key 服务，运营在界面上提交的批次没有 Key 可用，出图会被上游判 401")
	} else {
		// 第二个参数是「内部 Key 建出来时绑哪个分组」。
		//
		// ⚠ 这一项**必须有**：账号调度是按分组选账号的，Key 不绑分组就只能靠
		// 「允许未分组 Key 调度」那条系统设置（`allow_ungrouped_key_scheduling`，
		// 默认 false），否则出图报「没有可用的出图账号」。
		//
		// 2026-08-14 改：原来这里传的是 nil，理由是「分组是运营在后台建的，
		// 数字 ID 事先不知道，写进部署脚本必然填错」——这个理由只否掉了
		// 「把 ID 写死」，不该顺带否掉「运行时查一次」。传 nil 的代价是
		// 每开一个运营账号，管理员都要手动去后台把那把 Key 绑一次分组，
		// 而且漏了不会有任何提示，表现是出图报「没有可用的出图账号」——
		// monica 2026-08-14 指出这一步不该由人来做，她是对的。
		//
		// 现在改成运行时解析，规则见 imageGroupIDResolver。
		m.keys = dkservice.NewInternalKeyService(apiKeyService, m.imageGroupIDResolver())
	}

	// ---- 6. 出图网关 ----
	switch {
	case h == nil || h.OpenAIGateway == nil:
		m.degrade("拿不到上游的出图网关（h.OpenAIGateway 为空），出图功能不可用")
	case m.keys == nil:
		m.degrade("内部专用 API Key 服务没建起来，出图功能不可用")
	default:
		// h.OpenAIGateway 已确认非 nil，装进 ImagesHandler 接口是安全的。
		m.gateway = dkservice.NewGatewayInvoker(h.OpenAIGateway, m.keys)
		// 同一个网关、同一把内部 Key，换一条端点：出图走 Images()，
		// 「AI 挑提示词」要读图出文字，走 ChatCompletions()。
		m.chat = dkservice.NewChatInvoker(h.OpenAIGateway, m.keys)
	}

	// ---- 7. 商品图服务 ----
	if m.store == nil {
		m.degrade("没有对象存储，商品图上传和预处理不可用")
	} else {
		assets, assetErr := dkservice.NewAssetService(dkservice.AssetServiceDeps{
			Assets:   m.repo,
			Settings: m.repo,
			Store:    m.store,
			// 允许为 nil：没有预处理服务时 EnsureVariant 直接失败（fail-closed），
			// 绝不把没补边的原图发出去 —— 那会让运营选 3:4 拿回 4:3、钱照扣。
			Preprocessor: m.preprocessor,
		})
		if assetErr != nil {
			m.degrade("商品图服务没建起来：%v", assetErr)
		} else {
			m.assets = assets
		}
	}

	// ---- 7·b. AI 挑提示词 ----
	//
	// 三个前提缺一不可：灵感库（数据库）、商品图读取（对象存储）、对话调用器（出图网关）。
	// 缺了**只记降级不报错**：出图和翻灵感库都不受影响，只是那个按钮点不了。
	switch {
	case m.repo == nil:
		m.degrade("拿不到数据库，AI 挑提示词不可用")
	case m.assets == nil:
		m.degrade("没有商品图读取能力，AI 挑提示词不可用（它要把商品图发给模型看）")
	case m.chat == nil:
		m.degrade("没有对话调用器，AI 挑提示词不可用")
	default:
		suggest, suggestErr := dkservice.NewPromptSuggestService(dkservice.PromptSuggestDeps{
			Prompts: m.repo,
			Assets:  m.assets,
			Chat:    m.chat,
			// ⚠ 不能写成 Keys: m.keys 之外再判一次 —— m.keys 的声明类型是
			// *InternalKeyService，为 nil 时装进接口会变 typed nil。这里字段本身
			// 就是具体类型，service 内部按 `s.keys == nil` 判得准。
			Keys: m.keys,
		})
		if suggestErr != nil {
			m.degrade("AI 挑提示词没建起来：%v", suggestErr)
		} else {
			m.suggest = suggest
		}
	}

	// ---- 8. 后台队列 ----
	m.worker = m.newWorker()

	// ---- 9. 灵感库同步用的代理 ----
	//
	// 建不出来只影响「管理员能不能在下拉框里选代理」，同步直连照跑，
	// 所以**不记降级**（记了会让健康检查因为一个可选项常年发黄）。
	m.proxies = newProxyService(db)

	// ---- 10. 灵感库同步 ----
	m.newInspirationSync()

	// ---- 11. 交给 handler 的业务实现 ----
	m.services = m.buildServices()

	m.logStartupSummary()
	return m, nil
}

// newWorker 建后台队列。缺依赖时返回 nil 并记降级原因（不报错）。
func (m *Module) newWorker() *dkworker.Pool {
	// worker.New 自己也会校验，但那样只能拿到一句英文的 ErrMissingDependency，
	// 说不清「该去改哪个环境变量」。所以这里先自己判一次，给出中文原因。
	missing := make([]string, 0, 3)
	if m.store == nil {
		missing = append(missing, "对象存储")
	}
	if m.preprocessor == nil {
		missing = append(missing, "图像处理服务")
	}
	if m.gateway == nil {
		missing = append(missing, "出图网关")
	}
	if len(missing) > 0 {
		m.degrade("出图队列没有启动，缺少：%s。已提交的批次会一直停在排队中", strings.Join(missing, "、"))
		return nil
	}

	queue, err := dkworker.New(dkworker.Config{}, dkworker.Deps{
		// 并发数、单张超时、最长边三项以 designkit_settings 为准（Start 时读一次），
		// 所以 Config 传零值即可，别在代码里写死。
		Repo:         m.repo,
		Store:        m.store,
		Preprocessor: m.preprocessor,
		Gateway:      m.gateway,
		// Locker 留空：群晖上只跑一个副本。
		// **哪天要跑第二个副本，必须先补上 advisory lock** —— 结算路径没有
		// 数据库层的守卫，两个副本会同时结算同一个批次，把冻结额多减一遍。
		Locker: nil,
		// 心跳必须覆盖「排在后面、一张还没开始跑」的批次，否则排队超过 10 分钟
		// 会被僵尸巡检当成崩溃残留判死，而它其实好好的。
		ActiveJobIDs: activeJobIDsFunc(m.pool.DB()),
	})
	if err != nil {
		m.degrade("出图队列没建起来：%v", err)
		return nil
	}

	// ⚠ 上面 Locker 传的是 nil（没有分布式锁）。这不是降级项 —— 群晖上就是单副本，
	// 单副本下一切正常，所以不能记成 degraded 把健康检查染红。
	// 但它是一条**部署约束**，必须每次启动都喊一嗓子：
	// 多副本一上，两个实例会同时结算同一个批次，而结算会按张递减冻结额，
	// 跑两遍就多减一次 —— 用户的钱当场少一截，而且没有任何报错。
	slog.Warn("designkit 当前是单副本模式：出图队列没有分布式锁（advisory lock），" +
		"整套服务只能起一个实例。要跑第二个实例（多副本、滚动更新期间新旧实例并存）之前，" +
		"必须先给僵尸巡检和结算补上 PostgreSQL 咨询锁，否则同一个批次会被结算两遍、" +
		"冻结额按张多减一次，用户的余额会凭空少一截，且不会报错。" +
		"详见 designkit/docs/设计定型.md 第十节第 9 条。")
	return queue
}

// newInspirationSync 建灵感库同步服务，并把它接到 promptSync 上。
//
// 建不出来时**只降级不报错**：灵感库同步挂了，出图一点都不受影响
// （运营还可以手打提示词），为它把整个模块拖成 down 完全不成比例。
func (m *Module) newInspirationSync() {
	if m.repo == nil {
		m.degrade("拿不到数据库，灵感库同步不可用：管理员点「立即同步」会被告知功能未就绪")
		return
	}

	deps := dkservice.InspirationSyncDeps{
		Prompts:  m.repo,
		Sync:     m.repo,
		Settings: m.repo,
	}
	// ⚠ 不能直接写 Proxies: m.proxies。m.proxies 的声明类型是
	// *upstreamservice.ProxyService，为 nil 时装进接口字段会变成「typed nil」——
	// 接口本身非 nil，同步服务里那句 `if s.proxies == nil` 永远为假，
	// 于是管理员选了代理时不会走到「拿不到代理服务」那条中文提示，
	// 而是一个 nil 指针解引用把出图进程一起带走。
	// 上面第 6 步给 m.store、buildServices 给 m.keys 都写过同样的注意事项。
	if m.proxies != nil {
		deps.Proxies = proxyResolver{svc: m.proxies}
	}

	sync, err := dkservice.NewInspirationSyncService(deps)
	if err != nil {
		m.degrade("灵感库同步没建起来：%v", err)
		return
	}
	m.inspiration = sync
	m.promptSync = sync
}

// buildServices 拼出 handler 需要的四个业务实现。
//
// **四个里缺一个，RegisterBusinessRoutes 就整组不注册**（只留健康检查）——
// 所以这里每一个赋值失败都必须记一条降级原因，否则线上表现是
// 「接口全 404、日志里什么都没有」。
func (m *Module) buildServices() dkhandler.Services {
	svcs := dkhandler.Services{}
	if m.assets != nil {
		svcs.Assets = assetServiceAdapter{svc: m.assets}
	}
	if m.repo != nil && m.store != nil {
		svcs.Images = imageServiceAdapter{repo: m.repo, store: m.store}
	}
	if m.repo != nil {
		deps := dkservice.JobServiceDeps{
			Repo: m.repo,
			// 允许为 nil：没有对象存储时只有「取结果图字节」这一个动作会失败，
			// 报价 / 提交 / 查询照常。
			Store: m.store,
		}
		// ⚠ 不能直接写 Keys: m.keys。m.keys 的声明类型是 *InternalKeyService，
		// 为 nil 时装进接口字段会变成「typed nil」——接口本身非 nil，
		// service 里那句 `if s.keys == nil` 永远为假，于是走不到
		// 「账号还没开通出图权限，请联系管理员」，运营看到的是「系统内部错误」。
		// 上面第 6 步给 m.store 专门写过同样的注意事项，这里照做。
		if m.keys != nil {
			deps.Keys = m.keys
		}
		jobs, jobErr := dkservice.NewJobService(deps)
		if jobErr != nil {
			m.degrade("批次服务（提交 / 报价 / 查询）没建起来：%v", jobErr)
		} else {
			m.jobs = jobs
			svcs.Jobs = jobServiceAdapter{svc: jobs}
			// 比例列表挂在只依赖数据库的 JobService 上。
			// 这样 DESIGNKIT_STORAGE_DIR 建不出来时，坏掉的只是「传图」，
			// 「看比例 / 查历史 / 看报价」照常可用，而不是整组接口 404。
			svcs.Catalog = catalogServiceAdapter{svc: jobs}
		}
	}
	if svcs.Catalog == nil && m.assets != nil {
		// JobService 没建起来时的兜底（AssetService 也实现了同样两个方法）。
		svcs.Catalog = catalogServiceAdapter{svc: m.assets}
	}
	if m.repo != nil {
		// 「我的消费」**只吃 repository**：不要把对象存储或出图网关吊上来。
		// 存储目录建不出来时，工作台角落的「本月出图 / 花费 / 余额」照样该显示，
		// 「申请额度」这条出路更是一刻都不能断（决策 19）。
		svcs.Me = meServiceAdapter{repo: m.repo}

		// 灵感库也**只吃 repository**（同步用的代理列表除外）。
		// 跟「我的消费」同一个道理：存储/网关坏了的时候，
		// 「翻灵感库找一条词」照样该能用。
		svcs.Prompts = promptServiceAdapter{repo: m.repo}
		// ⚠ typed-nil：m.suggest 的声明类型是 *PromptSuggestService，
		// 为 nil 时直接赋给接口字段会让 register_business.go 那句
		// `if opts.Services.Suggest != nil` 恒为真，于是路由挂上去、
		// 运营点了拿到 500，而不是我们设计好的「裸 404 → 显示还没准备好」。
		if m.suggest != nil {
			svcs.Suggest = suggestServiceAdapter{svc: m.suggest}
		}
		svcs.PromptSync = promptSyncAdapter{
			repo:    m.repo,
			proxies: m.proxies,
			// 为 nil 时「立即同步」返回一句中文的「还没准备好」，
			// 同步状态和代理设置照常可用。
			runner: m.promptSync,
		}

		// 「商品图设置」也**只吃 repository**（代理服务只用来显示同步代理的名字）。
		// 理由同上：存储 / 网关坏掉的时候，管理员更需要能打开设置页去改东西。
		svcs.Settings = settingsServiceAdapter{repo: m.repo, proxies: m.proxies}
	}
	if !svcs.Ready() {
		m.degrade("业务服务没配齐（素材 / 比例 / 批次 / 结果图 四缺一），业务接口整组未注册，只有健康检查可用")
	}
	return svcs
}

// degrade 记一条降级原因：日志里立刻能看到，健康检查也会因此报 degraded。
func (m *Module) degrade(format string, args ...any) {
	reason := fmt.Sprintf(format, args...)
	m.degraded = append(m.degraded, reason)
	slog.Error("designkit 模块降级：" + reason)
}

// logStartupSummary 用一行日志说清楚「哪些能用、哪些不能用」。
// 运维排障时第一眼看的就是它，不要删。
func (m *Module) logStartupSummary() {
	slog.Info("designkit 模块装配完成（true=可用，false=不可用）",
		slog.Bool("database", m.pool != nil),
		slog.Bool("object_store", m.store != nil),
		slog.Bool("imgsvc", m.preprocessor != nil),
		slog.Bool("gateway", m.gateway != nil),
		slog.Bool("worker", m.worker != nil),
		slog.Bool("jobs", m.jobs != nil),
		// 灵感库两项：浏览只要数据库在就有；同步器还没装配时「立即同步」不可用，
		// 但同步状态和代理设置照常给得出来（见 promptSyncAdapter）。
		slog.Bool("prompt_library", m.services.Prompts != nil),
		slog.Bool("prompt_sync_runner", m.promptSync != nil),
		slog.Bool("proxy_options", m.proxies != nil),
		slog.Bool("business_api", m.services.Ready()),
		slog.String("module_status", m.ModuleStatus()),
		slog.Int("degraded_count", len(m.degraded)),
	)
}

// 模块整体状态的三个取值（健康检查的 module_status 字段）。
const (
	// ModuleStatusOK 一切正常。
	ModuleStatusOK = "ok"
	// ModuleStatusDegraded 起来了但有功能不可用（原因在 DegradedReasons 里）。
	ModuleStatusDegraded = "degraded"
	// ModuleStatusDown 模块根本没装配起来（连数据库连接池都没建出来）。
	ModuleStatusDown = "down"
)

// ModuleStatus 返回模块整体状态：ok / degraded / down。
//
// ⚠ 它跟「数据库通不通」是**两件事**，健康检查里必须分成两个字段：
//   - db           只回答「数据库通不通」（moduleHealthRepo.Ping）
//   - module_status 回答「出图这套东西全不全」
//
// 之前这两件事被塞进同一个 db 字段：只要有任何降级项（比如图像服务没配），
// db 就恒为 error，而且**根本不去 ping 数据库**。后果是运维再也分不出
// 数据库到底挂没挂 —— 而 db 字段的对外契约本来就是「数据库通不通」。
func (m *Module) ModuleStatus() string {
	if m == nil || m.startupErr != nil {
		return ModuleStatusDown
	}
	if len(m.degraded) > 0 {
		return ModuleStatusDegraded
	}
	return ModuleStatusOK
}

// DegradedReasons 返回降级原因（中文，可以直接显示给管理员看）的副本。
// 模块根本没起来时，第一条就是那个致命错误。
// healthModuleStatus 喂给 HealthService，让 /health 能报出模块整体状态。
//
// 为什么必须有：出图网关没配、图像服务地址填错、对象存储目录建不出来这几种情况下，
// 数据库是好的（db=ok），但**一张图也出不来**。只看 db 的话，
// 运营和管理员会看到绿色的「一切正常」，而实际上整个产品是废的。
func (m *Module) healthModuleStatus() (string, []string) {
	status := m.ModuleStatus()
	if status == ModuleStatusOK {
		return status, nil
	}
	return status, m.DegradedReasons()
}

func (m *Module) DegradedReasons() []string {
	if m == nil {
		return []string{"designkit 模块没有装配起来"}
	}
	reasons := make([]string, 0, len(m.degraded)+1)
	if m.startupErr != nil {
		reasons = append(reasons, m.startupErr.Error())
	}
	reasons = append(reasons, m.degraded...)
	return reasons
}

// StartupError 返回「模块没起来」的原因；一切正常时返回 nil。
//
// 致命错误和降级都算「没起来」。**注意它不再决定健康检查的 db 字段**
// （那一条只反映数据库），模块状态请用 ModuleStatus / DegradedReasons。
func (m *Module) StartupError() error {
	if m == nil {
		return errors.New("designkit: 模块没有装配起来")
	}
	if m.startupErr != nil {
		return m.startupErr
	}
	if len(m.degraded) > 0 {
		return fmt.Errorf("designkit: 模块以降级状态运行：%s", strings.Join(m.degraded, "；"))
	}
	return nil
}

// Start 启动后台任务：出图队列（worker + 僵尸巡检 + 结算）和灵感库自动同步。
//
// 传进来的 ctx 只用于「启动阶段读 designkit_settings」，**不决定后台任务的生命周期** ——
// 它们活到 Close 被调用为止。注册路由时手上那个 ctx 往往是请求级的，
// 拿它当后台任务的生命周期，队列会在第一次请求结束时就停掉。
//
// 队列没建起来时返回 nil（原因在启动日志里），不阻塞进程。
func (m *Module) Start(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.started || m.closed {
		m.mu.Unlock()
		return nil
	}
	m.started = true
	w := m.worker
	inspiration := m.inspiration
	m.mu.Unlock()

	// 灵感库自动同步先起：它跟出图队列**没有任何依赖关系**，
	// 出图那边缺依赖（没配图像服务之类）时，灵感库照样该定时更新 ——
	// 运营翻词、复制词不需要出图链路是好的。
	if inspiration != nil {
		if err := inspiration.Start(ctx); err != nil {
			// 只记不返：自动同步起不来不该拦住出图队列。
			// 管理员仍然可以手动点「立即同步」。
			slog.Error("designkit 灵感库自动同步启动失败：灵感库不会自己更新，"+
				"管理员可以在界面上手动点「立即同步」", slog.Any("error", err))
		}
	} else {
		slog.Warn("designkit 灵感库自动同步未启动（原因见上面的降级日志）")
	}

	if w == nil {
		slog.Warn("designkit 出图队列未启动，提交的批次不会开始出图（原因见上面的降级日志）")
		return nil
	}
	if err := w.Start(ctx); err != nil {
		return fmt.Errorf("designkit: 出图队列启动失败：%w", err)
	}
	return nil
}

// Close 停队列、关连接池。可以重复调用（第二次起返回第一次的结果）。
//
// **顺序不能反**：worker.Stop 要把租约交还给数据库（一条 UPDATE），
// 先关连接池的话那条 UPDATE 必然报 "sql: database is closed"，
// 在途的那几张就会一直挂着 running + 已过期的租约，等 180 秒租约到期
// 才被巡检回收 —— 运营看到的是「重启完还卡在生成中」。
func (m *Module) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		w := m.worker
		inspiration := m.inspiration
		m.mu.Unlock()

		// 灵感库同步先停：它握着一把 PostgreSQL 咨询锁，
		// 连接池一关那把锁只能等连接断开才释放，期间新起来的实例
		// 点「立即同步」会一直被告知「已经有一次在跑」。
		// 它自己有收尾预算（不等在途那一轮下载跑完），不会拖住关停。
		if inspiration != nil {
			if err := inspiration.Close(); err != nil {
				slog.Warn("designkit 灵感库同步关停时出错", slog.Any("error", err))
			}
		}

		if w != nil {
			// 不等图跑完，只等「交还租约」这一步，见 workerStopBudget。
			ctx, cancel := context.WithTimeout(context.Background(), workerStopBudget)
			if err := w.Stop(ctx); err != nil {
				slog.Warn("designkit 出图队列没能在窗口内交还全部租约，剩下的等租约过期后由别的实例接手",
					slog.Any("error", err))
			}
			cancel()
		}
		if m.pool != nil {
			m.closeErr = m.pool.Close()
		}
	})
	return m.closeErr
}

// ----------------------------------------------------------------------------
// 挂路由
// ----------------------------------------------------------------------------

// RegisterRoutes 是 backend/internal/server/router.go 里 registerRoutes 调的那一行。
//
// 参数逐个对应上游 registerRoutes 手上的东西，**顺序和类型不要改** ——
// 改了就要动上游文件，而那是每次跟版的冲突高发区（CLAUDE.md 第一节）。
//
// 这个函数负责：建模块 → 启动后台队列 → 装退出信号 → 挂路由。
// 任何一步失败都只让 designkit 降级，绝不 panic、绝不让整个 Sub2API 起不来。
func RegisterRoutes(
	r *gin.Engine,
	v1 *gin.RouterGroup,
	h *upstreamhandler.Handlers,
	jwtAuth upstreammw.JWTAuthMiddleware,
	apiKeyAuth upstreammw.APIKeyAuthMiddleware,
	apiKeyService *upstreamservice.APIKeyService,
	settingService *upstreamservice.SettingService,
	cfg *config.Config,
	redisClient *redis.Client,
) {
	m, err := NewModule(cfg, h, apiKeyService)
	if err != nil {
		slog.Error("designkit 模块启动失败：出图功能不可用，网关其余功能不受影响。"+
			"健康检查 /api/v1/designkit/health 会返回 status=degraded",
			slog.Any("error", err))
		// 仍然挂上健康检查：不挂的话运维只能看到 404，分不清是「没装这个模块」
		// 还是「装了但挂了」。
		m = &Module{startupErr: err}
		m.health = dkservice.NewHealthService(moduleHealthRepo{m: m}).
			WithModuleStatus(m.healthModuleStatus)
	}

	setCurrentModule(m)

	if startErr := m.Start(context.Background()); startErr != nil {
		slog.Error("designkit 出图队列启动失败：已提交的批次会一直停在排队中", slog.Any("error", startErr))
	}

	m.RegisterRoutes(RouteDeps{
		Engine:         r,
		V1:             v1,
		JWTAuth:        jwtAuth,
		APIKeyAuth:     apiKeyAuth,
		APIKeyService:  apiKeyService,
		SettingService: settingService,
		Config:         cfg,
		Redis:          redisClient,
	})
}

// RouteDeps 是挂路由要的上游零件。
type RouteDeps struct {
	// Engine 根路由，机器（ERP）那条 /v1/designkit 从它派生。
	Engine *gin.Engine
	// V1 上游的 /api/v1 组，浏览器那条 /api/v1/designkit 从它派生。
	V1 *gin.RouterGroup
	// JWTAuth 浏览器侧鉴权。
	JWTAuth upstreammw.JWTAuthMiddleware
	// APIKeyAuth 上游的 API Key 鉴权，只给 POST /jobs 用。
	APIKeyAuth upstreammw.APIKeyAuthMiddleware
	// APIKeyService ERP 读接口的轻量 Key 鉴权要用它查 Key。
	APIKeyService *upstreamservice.APIKeyService
	// SettingService 面板限流器要读系统设置里的阈值。
	SettingService *upstreamservice.SettingService
	// Config KeyAuth 要读 IP 白黑名单等配置。
	Config *config.Config
	// Redis 面板限流器的计数落在它上面。
	Redis *redis.Client
}

// RegisterRoutes 把 designkit 挂进上游路由树，并把**真正的** Services 传进去。
func (m *Module) RegisterRoutes(deps RouteDeps) {
	if m == nil {
		return
	}
	dkhandler.Mount(dkhandler.MountOptions{
		Engine:         deps.Engine,
		V1:             deps.V1,
		JWTAuth:        deps.JWTAuth,
		APIKeyAuth:     deps.APIKeyAuth,
		APIKeyService:  deps.APIKeyService,
		SettingService: deps.SettingService,
		Config:         deps.Config,
		Redis:          deps.Redis,
		Health:         m.health,
		Services:       m.services,
	})
}

// ----------------------------------------------------------------------------
// 进程级单例 + 退出信号
// ----------------------------------------------------------------------------
//
// 为什么要自己听信号：上游 main.go 的关闭流程是 wire 生成的
// （backend/cmd/server/wire_gen.go 是已入库的上游文件，不在可改白名单里），
// 我们挂不进它的 Cleanup。
// signal.Notify 是按 channel 广播的，**不会**抢走 main.go 自己那一份信号。

var (
	moduleMu    sync.Mutex
	currentMod  *Module
	watcherOnce sync.Once
)

// setCurrentModule 记录当前模块；之前已经有一个（例如测试里反复建路由）就把旧的关掉。
func setCurrentModule(m *Module) {
	moduleMu.Lock()
	old := currentMod
	currentMod = m
	moduleMu.Unlock()

	if old != nil && old != m {
		if err := old.Close(); err != nil {
			slog.Warn("designkit 关闭上一个模块实例失败", slog.Any("error", err))
		}
	}
	startShutdownWatcher()
}

// Shutdown 关掉当前模块。导出一份是给测试和以后可能出现的显式关闭点用的，可重复调用。
func Shutdown() error {
	moduleMu.Lock()
	m := currentMod
	currentMod = nil
	moduleMu.Unlock()
	return m.Close()
}

// startShutdownWatcher 进程内只装一次。
//
// 收到信号后**立刻**关，不睡任何 grace ——
// 上游 main.go 只给 5 秒就让进程消失，睡够 grace 再动手等于什么都没做：
// 队列不会交还租约，在途的那几张会一直挂着 running，
// 等 180 秒租约过期才被巡检回收。
func startShutdownWatcher() {
	watcherOnce.Do(func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-ch
			if err := Shutdown(); err != nil {
				slog.Warn("designkit 关停时出错", slog.Any("error", err))
			}
		}()
	})
}

// ----------------------------------------------------------------------------
// 健康检查
// ----------------------------------------------------------------------------

// moduleHealthRepo 是健康检查的 db 字段的**唯一**来源。
//
// ⚠ 它只回答一件事：**数据库通不通**。
//
// 曾经这里写成「有任何降级项就直接返错、根本不 ping 数据库」，
// 于是 /health 永远是 {"status":"degraded","db":"error"}：
// 图像服务没配 → db=error，数据库其实好好的。
// 运维照着 db 字段去查数据库，查半天什么也查不出来 —— 那个字段的对外契约
// （handler/health.go 里写死的）就是「数据库通不通」，不能被借去表达别的意思。
//
// 「模块整体全不全」由 Module.ModuleStatus / DegradedReasons 表达，
// 健康检查另开字段承接（见交付说明里的响应体形状）。
type moduleHealthRepo struct {
	m *Module
}

// 编译期断言：接口对得上。
var _ dkservice.HealthRepository = moduleHealthRepo{}

// Ping 真的往数据库发一条 SELECT 1。
func (r moduleHealthRepo) Ping(ctx context.Context) error {
	if r.m == nil {
		return dkservice.ErrDatabaseUnavailable
	}
	// startupErr 只有两种来源：配置不可用、连接池建不出来。两种都等于「没有数据库」，
	// 所以它确实属于 db 这一栏。**降级原因（degraded）不在这里判**。
	if r.m.startupErr != nil {
		return r.m.startupErr
	}
	if r.m.healthRepo == nil {
		return dkservice.ErrDatabaseUnavailable
	}
	return r.m.healthRepo.Ping(ctx)
}

// suggestServiceAdapter 把 service.PromptSuggestService 适配成 handler.SuggestService。
//
// 两边的结构体字段一一对应，翻译只是搬字段 —— 之所以还要搬一次，是因为
// ports.go 的类型定义在 handler 包，而 handler 已经 import 了 service，
// service 再反过来 import handler 就是循环依赖。跟别的适配器同一个道理。
type suggestServiceAdapter struct {
	svc *dkservice.PromptSuggestService
}

var _ dkhandler.SuggestService = suggestServiceAdapter{}

func (a suggestServiceAdapter) SuggestPrompt(ctx context.Context, in dkhandler.SuggestPromptInput) (*dkhandler.SuggestPromptResult, error) {
	result, err := a.svc.Suggest(ctx, dkservice.SuggestInput{
		UserID:         in.UserID,
		AssetUID:       in.AssetUID,
		ExtraAssetUIDs: in.ExtraAssetUIDs,
		CategorySlug:   in.CategorySlug,
		Features:       in.Features,
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal)
	}
	candidates := make([]dkhandler.SuggestedCandidate, 0, len(result.Candidates))
	for _, one := range result.Candidates {
		candidates = append(candidates, dkhandler.SuggestedCandidate{UID: one.UID, Title: one.Title})
	}
	return &dkhandler.SuggestPromptResult{
		Prompt: result.Prompt,
		Category: dkhandler.SuggestedCategory{
			Slug: result.Category.Slug,
			Name: result.Category.Name,
		},
		Candidates: candidates,
		Note:       result.Note,
	}, nil
}

// ----------------------------------------------------------------------------
// 内部 Key 绑哪个分组
// ----------------------------------------------------------------------------

// SettingKeyImageGroupID 管理员**显式**指定内部专用 Key 绑哪个分组（分组的数字 ID）。
//
// 平时不用填：只有一个能出图的分组时会自动认出来（见 imageGroupIDResolver）。
// 有多个候选时必须填，否则我们不猜——猜错的后果是图出在错的账号上、
// 记在错的分组账里，而且一切正常没有任何报错。
//
// 值可以是 JSON 数字也可以是字符串：2 或 "2"。
const SettingKeyImageGroupID = "image_group_id"

// imageGroupIDResolver 决定 designkit-internal 这把 Key 建出来时绑哪个分组。
//
// 顺序：
//
//  1. designkit_settings.image_group_id 有值 → 用它（管理员显式指定，最高优先）
//  2. 否则自动挑：**活跃 + 允许出图 + 名下至少有一个可调度账号**的分组，
//     正好只有一个时用它
//  3. 有多个 / 一个都没有 → 返回 nil 并打一条中文日志说清楚该干什么
//
// 返回 nil 不是错误，Key 照建（只是没绑分组，出图会报「没有可用的出图账号」）。
// **刻意不在这里返回 error**：建 Key 是运营点「提交」时顺带发生的，
// 为了一个可以事后补的配置问题把提交整个打回去，运营看到的是一句
// 看不懂的系统错误，而不是「管理员还没配好分组」。
//
// ⚠ 只在**创建**那一刻生效。已经建出来的 Key 不会被追认——上游的
// APIKeyProvisioner 切面里没有 Update，我们也不打算加：改别人已经建好的 Key
// 属于管理员动作，不该由一次出图请求顺手做掉。
func (m *Module) imageGroupIDResolver() dkservice.GroupIDResolver {
	return func(ctx context.Context) (*int64, error) {
		if m == nil || m.pool == nil {
			return nil, nil
		}

		// ---- 1. 管理员显式指定 ----
		if id, ok := m.settingImageGroupID(ctx); ok {
			slog.Info("designkit 内部专用 Key 按 designkit_settings 指定的分组绑定",
				slog.String("setting", SettingKeyImageGroupID),
				slog.Int64("group_id", id))
			return &id, nil
		}

		// ---- 2. 自动挑 ----
		groups, err := listImageCapableGroups(ctx, m.pool.DB())
		if err != nil {
			// 查不动数据库时不要拦住提交：Key 照建，出图那一步自己会报错。
			slog.Warn("designkit 查不到可出图的分组，内部专用 Key 这次不绑分组",
				slog.Any("error", err))
			return nil, nil
		}

		switch len(groups) {
		case 1:
			slog.Info("designkit 内部专用 Key 自动绑定到唯一一个可出图的分组",
				slog.Int64("group_id", groups[0].id),
				slog.String("group_name", groups[0].name))
			id := groups[0].id
			return &id, nil
		case 0:
			slog.Error("designkit 找不到任何「活跃 + 允许出图 + 名下有可调度账号」的分组，" +
				"内部专用 Key 不绑分组，出图会报「没有可用的出图账号」。" +
				"请到后台确认：分组是否启用、是否勾了「允许生成图片」、上游账号是否已加进该分组且可调度。")
			return nil, nil
		default:
			names := make([]string, 0, len(groups))
			for _, g := range groups {
				names = append(names, fmt.Sprintf("%s(id=%d)", g.name, g.id))
			}
			slog.Error("designkit 有多个分组都能出图，无法自动判断内部专用 Key 该绑哪个，本次不绑分组。"+
				"请在 designkit_settings 里把 "+SettingKeyImageGroupID+" 设成想用的那个分组 ID。",
				slog.String("candidates", strings.Join(names, "、")))
			return nil, nil
		}
	}
}

// settingImageGroupID 读 designkit_settings.image_group_id。
// 没配、配成 0 或负数、格式不对，一律当作「没指定」（第二个返回值 false）。
func (m *Module) settingImageGroupID(ctx context.Context) (int64, bool) {
	if m.repo == nil {
		return 0, false
	}
	setting, err := m.repo.GetSetting(ctx, SettingKeyImageGroupID)
	if err != nil || setting == nil || len(setting.Value) == 0 {
		return 0, false
	}

	// 数字和字符串两种写法都认（2 和 "2"），跟 rate_multiplier 一个待遇：
	// 管理员在设置页填的是文本框，存进 JSONB 很容易变成带引号的那种。
	var asNumber int64
	if err := json.Unmarshal(setting.Value, &asNumber); err == nil {
		if asNumber > 0 {
			return asNumber, true
		}
		return 0, false
	}
	var asString string
	if err := json.Unmarshal(setting.Value, &asString); err == nil {
		parsed, convErr := strconv.ParseInt(strings.TrimSpace(asString), 10, 64)
		if convErr == nil && parsed > 0 {
			return parsed, true
		}
	}
	slog.Warn("designkit 配置里的出图分组 ID 不是合法的正整数，按「没指定」处理",
		slog.String("setting", SettingKeyImageGroupID),
		slog.String("value", string(setting.Value)))
	return 0, false
}

// imageCapableGroup 是自动挑分组时用到的最小信息。
type imageCapableGroup struct {
	id   int64
	name string
}

// listImageCapableGroups 找出「真的能出图」的分组。
//
// 三个条件缺一不可，都是踩过的：
//   - `status='active'`：停用的分组调度器根本不会选
//   - `allow_image_generation`：这一格没勾，出图会被上游拦下来
//   - **名下至少有一个可调度账号**：只有前两条的话，一个空分组也会被选中，
//     然后出图报「没有可用的出图账号」，而管理员看分组配置一切正常
//
// 刻意不过滤 `is_exclusive`：专属分组也可以是正当选择（管理员把人加进
// user_allowed_groups 即可）。绑不上时上游会返回 ErrGroupNotAllowed，
// internal_key.go 已经把它翻成中文的「账号还没开通出图权限，请联系管理员」。
func listImageCapableGroups(ctx context.Context, db *sql.DB) ([]imageCapableGroup, error) {
	const query = `
SELECT g.id, g.name
FROM groups g
WHERE g.deleted_at IS NULL
  AND g.status = 'active'
  AND g.allow_image_generation = TRUE
  AND EXISTS (
        SELECT 1
        FROM account_groups ag
        JOIN accounts a ON a.id = ag.account_id
        WHERE ag.group_id = g.id
          AND a.deleted_at IS NULL
          AND a.status = 'active'
          AND a.schedulable = TRUE
  )
ORDER BY g.id`

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]imageCapableGroup, 0, 4)
	for rows.Next() {
		var g imageCapableGroup
		if err := rows.Scan(&g.id, &g.name); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ----------------------------------------------------------------------------
// 在途批次（给队列续心跳用）
// ----------------------------------------------------------------------------

// activeJobIDsFunc 返回「还有活没干完」的批次 id。
//
// 为什么这条 SQL 写在装配层而不是 repository：它只服务于 worker.Deps.ActiveJobIDs
// 这一个回调，不属于 domain.Repository 那份对外契约。哪天有第二个调用方，
// 再挪进 repository 并加进接口。
//
// 心跳只覆盖「本进程正在出图」的批次是不够的：4 个 worker 面对 3 个批次时，
// 排在后面的批次一张也没开始跑，心跳就不会被续，排队超过 10 分钟会被僵尸巡检
// 当成崩溃残留判死 —— 而它其实好好的。
func activeJobIDsFunc(db *sql.DB) func(ctx context.Context) ([]int64, error) {
	if db == nil {
		return nil
	}
	// 状态值用 domain 常量传参，不写字面量：哪天状态改名了，编译不过好过静默查不到。
	const query = `SELECT DISTINCT job_id FROM designkit_job_items WHERE status IN ($1, $2) ORDER BY job_id`
	return func(ctx context.Context) ([]int64, error) {
		rows, err := db.QueryContext(ctx, query,
			string(dkdomain.ItemStatusPending), string(dkdomain.ItemStatusRunning))
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()

		ids := make([]int64, 0, 8)
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, rows.Err()
	}
}

// ----------------------------------------------------------------------------
// handler 端口的适配器
// ----------------------------------------------------------------------------
//
// 这几个类型只干一件事：把 service / repository 的形状翻译成
// handler/ports.go 里那几个接口的形状。
//
// 为什么适配器在这里而不在 service 包里：ports.go 的类型（Services、
// ContentBlob、RatioOption、ImageView…）定义在 handler 包，
// 而 handler 已经 import 了 service —— service 再反过来 import handler 就是循环依赖。
// 本包在依赖图的最外层，是唯一能同时看见两边的地方。

// assetServiceAdapter 把 service.AssetService 适配成 handler.AssetService。
type assetServiceAdapter struct {
	svc *dkservice.AssetService
}

var _ dkhandler.AssetService = assetServiceAdapter{}

// CreateAsset 存一张原图。
func (a assetServiceAdapter) CreateAsset(ctx context.Context, in dkhandler.CreateAssetInput) (*dkdomain.Asset, error) {
	result, err := a.svc.UploadAsset(ctx, dkservice.UploadAssetInput{
		UserID: in.UserID,
		Origin: in.Origin,
		// ⚠ ContentType 只当参考：service 一律按字节头判真实格式。
		// 浏览器对 HEIC 常常报 image/jpeg，Mac 导出的 HEIC 还经常顶着 .jpg 扩展名。
		ClientContentType: in.ContentType,
		Filename:          in.Filename,
		DeclaredSize:      int64(len(in.Data)),
		Data:              bytes.NewReader(in.Data),
	})
	if err != nil {
		return nil, err
	}
	if result == nil || result.Asset == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal)
	}
	return result.Asset, nil
}

// GetAsset 按对外编号取一张原图（service 内部已校验归属）。
func (a assetServiceAdapter) GetAsset(ctx context.Context, userID int64, uid string) (*dkdomain.Asset, error) {
	return a.svc.GetAsset(ctx, userID, uid)
}

// OpenAssetContent 取原图字节。
//
// Filename 留空 = 不下发 Content-Disposition：这个端点是给界面预览用的，
// 浏览器直接内联显示即可；真要下载再单独做。
func (a assetServiceAdapter) OpenAssetContent(ctx context.Context, userID int64, uid string) (*dkhandler.ContentBlob, error) {
	data, contentType, err := a.svc.AssetContent(ctx, userID, uid)
	if err != nil {
		return nil, err
	}
	return &dkhandler.ContentBlob{Data: data, ContentType: contentType}, nil
}

// ratioCatalog 是「比例列表」需要的最小能力。
//
// 刻意用接口而不是绑死某个 service：比例只读 designkit_settings，
// 不该依赖对象存储。JobService（只要数据库）和 AssetService（要对象存储）
// 都满足它，优先用前者，见 buildServices。
type ratioCatalog interface {
	AllowedRatios(ctx context.Context) []dkdomain.Ratio
	MaxDimension(ctx context.Context) int
}

// catalogServiceAdapter 把「出图比例」这块配置适配成 handler.CatalogService。
type catalogServiceAdapter struct {
	svc ratioCatalog
}

var _ dkhandler.CatalogService = catalogServiceAdapter{}

// ListRatios 返回允许的出图比例，顺序即界面显示顺序（第一个默认选中）。
//
// UnitPrice 一律给 nil = 「价格待确认」。
// **不许在这里编一个猜出来的单价**：真正计费的档由上游按「实际输出像素」定，
// 实测请求 1:1 拿回 1254×1254 落的是 2K 不是 1K。每个比例实际落哪档、
// 单价填多少，必须真出一张图实测才知道（CLAUDE.md 第二·五节 B3）。
func (c catalogServiceAdapter) ListRatios(ctx context.Context) ([]dkhandler.RatioOption, error) {
	allowed := c.svc.AllowedRatios(ctx)
	maxDimension := c.svc.MaxDimension(ctx)

	options := make([]dkhandler.RatioOption, 0, len(allowed))
	for _, ratio := range allowed {
		width, height, err := ratio.TargetSize(maxDimension)
		if err != nil {
			// AllowedRatios 已经把不合法的比例过滤掉了，走到这里说明配置又坏了一层，
			// 跳过这一个而不是让整个列表失败 —— 少一个比例好过界面全空。
			slog.Warn("designkit 出图比例算不出目标尺寸，已跳过",
				slog.String("ratio", ratio.String()),
				slog.Int("max_dimension", maxDimension),
				slog.Any("error", err))
			continue
		}
		options = append(options, dkhandler.RatioOption{
			Ratio:        ratio,
			TargetWidth:  width,
			TargetHeight: height,
			PricingTier:  dkdomain.ClassifyBillingTierOrDefault(width, height),
			UnitPrice:    nil,
			IsDefault:    len(options) == 0,
		})
	}
	return options, nil
}

// jobServiceAdapter 把 service.JobService 适配成 handler.JobService。
//
// 两边的结构体字段是**一一对应**的，翻译只是搬字段 —— 之所以还要搬一次，
// 是因为 ports.go 的类型定义在 handler 包，而 handler 已经 import 了 service，
// service 再反过来 import handler 就是循环依赖，编译不过。
type jobServiceAdapter struct {
	svc *dkservice.JobService
}

var _ dkhandler.JobService = jobServiceAdapter{}

// toServiceJobSpec 把 handler 的批次描述翻成 service 的。
func toServiceJobSpec(spec dkhandler.JobSpec) dkservice.JobSpec {
	return dkservice.JobSpec{
		UserID:      spec.UserID,
		Origin:      spec.Origin,
		Ratio:       spec.Ratio,
		AssetUIDs:   spec.AssetUIDs,
		PromptUIDs:  spec.PromptUIDs,
		PromptTexts: spec.PromptTexts,
		Model:       spec.Model,
		// ⚠ 传下去也不会落库：designkit_jobs 没有这一列（9001 跑过、永不可改），
		// 出图 worker 用的是它自己的全局配置。要做到「每批可选」得先加一条 9xxx 迁移。
		KeepTransparency: spec.KeepTransparency,
	}
}

// EstimateJob 只算账，不建任何东西。
func (a jobServiceAdapter) EstimateJob(ctx context.Context, spec dkhandler.JobSpec) (*dkhandler.EstimateResult, error) {
	result, err := a.svc.EstimateJob(ctx, toServiceJobSpec(spec))
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal)
	}
	// PriceNote 必须带过去（对应响应里的 price_note）：价格待确认时
	// unit_price / estimated_cost 都是 null，界面上如果连一句解释都没有，
	// 运营看到的就是一个空价格 —— 分不清「这一批不要钱」还是「系统坏了」。
	return &dkhandler.EstimateResult{
		ItemCount:     result.ItemCount,
		AssetCount:    result.AssetCount,
		PromptCount:   result.PromptCount,
		MaxBatchItems: result.MaxBatchItems,
		PricingTier:   result.PricingTier,
		UnitPrice:     result.UnitPrice,
		EstimatedCost: result.EstimatedCost,
		PriceNote:     result.PriceNote,
		Balance:       result.Balance,
		Available:     result.Available,
		Sufficient:    result.Sufficient,
		Shortfall:     result.Shortfall,
	}, nil
}

// CreateJob 提交一个批次。
func (a jobServiceAdapter) CreateJob(ctx context.Context, in dkhandler.CreateJobInput) (*dkdomain.Job, error) {
	return a.svc.CreateJob(ctx, dkservice.CreateJobInput{
		JobSpec:        toServiceJobSpec(in.JobSpec),
		Name:           in.Name,
		CallbackURL:    in.CallbackURL,
		IdempotencyKey: in.IdempotencyKey,
		APIKeyID:       in.APIKeyID,
	})
}

// GetJob 按对外任务号取批次（service 内部已校验归属）。
func (a jobServiceAdapter) GetJob(ctx context.Context, userID int64, jobUID string) (*dkdomain.Job, error) {
	return a.svc.GetJob(ctx, userID, jobUID)
}

// ListJobs 批次列表，游标分页。
func (a jobServiceAdapter) ListJobs(ctx context.Context, query dkdomain.ListJobsQuery) (*dkdomain.JobPage, error) {
	return a.svc.ListJobs(ctx, query)
}

// ListJobItems 取一个批次的全部 item，已按 seq 升序。
func (a jobServiceAdapter) ListJobItems(ctx context.Context, userID int64, jobUID string) ([]*dkhandler.JobItemView, error) {
	views, err := a.svc.ListJobItems(ctx, userID, jobUID)
	if err != nil {
		return nil, err
	}
	out := make([]*dkhandler.JobItemView, 0, len(views))
	for _, v := range views {
		if v == nil {
			continue
		}
		out = append(out, &dkhandler.JobItemView{Item: v.Item, Images: v.Images})
	}
	return out, nil
}

// GetJobItem 按序号取某一张。
func (a jobServiceAdapter) GetJobItem(ctx context.Context, userID int64, jobUID string, seq int) (*dkhandler.JobItemView, error) {
	view, err := a.svc.GetJobItem(ctx, userID, jobUID, seq)
	if err != nil {
		return nil, err
	}
	if view == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeItemNotFound)
	}
	return &dkhandler.JobItemView{Item: view.Item, Images: view.Images}, nil
}

// OpenJobItemContent 取某一张当前版本的结果图字节。
func (a jobServiceAdapter) OpenJobItemContent(ctx context.Context, userID int64, jobUID string, seq, imageIndex int) (*dkhandler.ContentBlob, error) {
	data, contentType, err := a.svc.OpenJobItemContent(ctx, userID, jobUID, seq, imageIndex)
	if err != nil {
		return nil, err
	}
	return &dkhandler.ContentBlob{
		Data:        data,
		ContentType: contentType,
		// 下载时的建议文件名：任务号 + 序号，运营一眼能对上是哪一批的第几张。
		Filename: fmt.Sprintf("%s-%d.png", jobUID, seq),
	}, nil
}

// StopJob 「停止排队」（决策 21）。
//
// service 侧只做两件事：把**还没开始**的那几张转 cancelled、给批次打
// cancel_requested_at。已经在跑的必须等它自然落地并照常扣费 ——
// 上游出图跟浏览器连接是故意脱钩的，停不下来。
func (a jobServiceAdapter) StopJob(ctx context.Context, userID int64, jobUID string) (*dkhandler.StopJobResult, error) {
	result, err := a.svc.StopJob(ctx, userID, jobUID)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Job == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeJobNotFound)
	}
	return &dkhandler.StopJobResult{
		Job:          result.Job,
		Cancelled:    result.Cancelled,
		StillRunning: result.StillRunning,
	}, nil
}

// RetryJobItem 重试一张（决策 20）。
//
// 走跟提交完全同一套准入（单张预估 → FOR UPDATE 查可用额 → 不够就拒绝），
// 因为**重试一张 = 重新出一张图 = 重新收一次钱**，技术上改不了。
func (a jobServiceAdapter) RetryJobItem(ctx context.Context, in dkhandler.RetryItemInput) (*dkdomain.JobItem, error) {
	item, err := a.svc.RetryJobItem(ctx, dkservice.RetryItemInput{
		UserID:         in.UserID,
		JobUID:         in.JobUID,
		Seq:            in.Seq,
		IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeItemNotFound)
	}
	return item, nil
}

// ----------------------------------------------------------------------------
// 我的消费（决策 16 / 19）
// ----------------------------------------------------------------------------

// meServiceAdapter 把 repository 直接适配成 handler.MeService。
//
// **只有 repo 一个字段，这是刻意的。** 这两个端点不读图不写图；
// 把对象存储或出图网关吊上来，只会让「存储目录建不出来」这种降级顺手
// 把工作台角落那三个数和「申请额度」按钮一起干掉 —— 而那正是运营
// 最需要看到出路的时候。
type meServiceAdapter struct {
	repo dkdomain.Repository
}

var _ dkhandler.MeService = meServiceAdapter{}

// GetUsageSummary 工作台角落的「本月出图 N 张 / 花费 $X / 余额 $Y」（决策 16）。
//
// 时间窗在这里算：本月 1 号 00:00（UTC）到下月 1 号 00:00（UTC，不含）。
// 用 UTC 而不是本地时区，是为了跟上游 usage_logs.created_at 落在同一把尺子上；
// 换算成「北京时间的本月」得先确定服务器时区，那是个会静默算错的坑，
// 哪天真要按本地月份统计，改这一处即可。
func (a meServiceAdapter) GetUsageSummary(ctx context.Context, userID int64) (*dkhandler.UsageSummary, error) {
	from, to := currentMonthRange(time.Now())

	summary, err := a.repo.GetUsageSummary(ctx, userID, from, to)
	if err != nil {
		return nil, err
	}
	if summary == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal)
	}
	return &dkhandler.UsageSummary{
		PeriodStart:  from,
		PeriodEnd:    to,
		ImageCount:   summary.ImageCount,
		Cost:         summary.Cost,
		Balance:      summary.Balance,
		Available:    summary.Available,
		AdminContact: a.adminContact(ctx),
	}, nil
}

// CreateQuotaRequest 运营点「申请额度」（决策 19）。
//
// 同一个人已经有一条待处理时，数据库的部分唯一索引会挡下来并返回
// domain.ErrConflict —— handler 把它翻成「不用重复提交」。
func (a meServiceAdapter) CreateQuotaRequest(ctx context.Context, userID int64, note string) (*dkhandler.QuotaRequestResult, error) {
	request, err := a.repo.CreateQuotaRequest(ctx, userID, optionalNote(note))
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal)
	}
	return &dkhandler.QuotaRequestResult{
		Request:      request,
		AdminContact: a.adminContact(ctx),
	}, nil
}

// adminContact 读 designkit_settings.admin_contact（部署时填）。
//
// 读不到就返回空串、界面不显示那一行 —— **绝不能因为没配联系方式就报错**：
// 那样运营连申请都提交不了，等于把决策 19 留的这条出路自己堵死。
func (a meServiceAdapter) adminContact(ctx context.Context) string {
	setting, err := a.repo.GetSetting(ctx, dkdomain.SettingKeyAdminContact)
	if err != nil || setting == nil || len(setting.Value) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(setting.Value, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// optionalNote 空说明存 NULL 而不是空串。
// 空串和 NULL 混着存会让管理员后台「有没有写说明」这类判断失准。
func optionalNote(note string) *string {
	note = strings.TrimSpace(note)
	if note == "" {
		return nil
	}
	return &note
}

// currentMonthRange 返回「本月」的左闭右开区间（UTC）。
func currentMonthRange(now time.Time) (from, to time.Time) {
	utc := now.UTC()
	from = time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
	to = from.AddDate(0, 1, 0)
	return from, to
}

// imageServiceAdapter 把 repository + 对象存储适配成 handler.ImageService。
type imageServiceAdapter struct {
	repo  dkdomain.Repository
	store dkdomain.ObjectStore
}

var _ dkhandler.ImageService = imageServiceAdapter{}

// GetImage 按图片编号取一张结果图，并补上「第几批的第几张」。
//
// 数据库里的 job_id / item_id 是内部主键，对外一律不暴露，
// 所以这里换算成任务号 + 序号 —— ERP 就是靠这两个值把图对回自己的订单。
func (s imageServiceAdapter) GetImage(ctx context.Context, userID int64, uid string) (*dkhandler.ImageView, error) {
	// 归属校验在 repository 里：不是这个人的一律返回 ErrNotFound，
	// 不返回 403 —— 403 等于告诉对方「这个编号是存在的，只是不是你的」。
	image, err := s.repo.GetImageByUID(ctx, userID, uid)
	if err != nil {
		return nil, err
	}
	if image == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeImageNotFound)
	}

	job, err := s.repo.GetJobByID(ctx, image.JobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeJobNotFound)
	}

	// item_id → seq。designkit_job_items 有外键指向 job，所以一定找得到；
	// 找不到说明数据被手工改坏了，宁可报错也不要把 seq=0 发给 ERP ——
	// 序号从 1 开始是对外契约的一部分，0 会让对方按「第 0 张」去取图。
	items, err := s.repo.ListJobItems(ctx, image.JobID)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item != nil && item.ID == image.ItemID {
			return &dkhandler.ImageView{Image: image, JobUID: job.UID, Seq: item.Seq}, nil
		}
	}
	return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).
		WithCause(fmt.Errorf("designkit: 图片 %s 找不到对应的 item（item_id=%d）", uid, image.ItemID))
}

// OpenImageContent 取结果图字节。
func (s imageServiceAdapter) OpenImageContent(ctx context.Context, userID int64, uid string) (*dkhandler.ContentBlob, error) {
	view, err := s.GetImage(ctx, userID, uid)
	if err != nil {
		return nil, err
	}
	data, contentType, err := s.store.Get(ctx, view.Image.ObjectKey)
	if err != nil {
		// 库里有记录、存储里没文件不是「找不到」，是存储出了问题。
		// 报成 404 会让运营以为图被自己删了，实际要查的是磁盘。
		return nil, dkdomain.NewError(dkdomain.ErrCodeStorageError).WithCause(err)
	}
	if contentType == "" {
		contentType = view.Image.ContentType
	}
	return &dkhandler.ContentBlob{
		Data:        data,
		ContentType: contentType,
		// 下载时的建议文件名：任务号 + 序号，运营一眼能对上是哪一批的第几张。
		Filename: fmt.Sprintf("%s-%d.png", view.JobUID, view.Seq),
	}, nil
}

// ----------------------------------------------------------------------------
// 灵感库（浏览）
// ----------------------------------------------------------------------------

// promptServiceAdapter 把 repository 适配成 handler 要的 PromptService。
//
// **没有中间的 service 层**：灵感库的浏览就是三条查询，没有任何业务规则
// （没有归属校验 —— 灵感库是全站共享的目录；没有钱的事）。
// 为它单独造一个 service 只是多一层转发。
//
// 唯一在这里做的事是「把 category_id 换成 slug / 名字」：
// 自增主键对外一律不暴露（9001 迁移的文件头写死了这条），
// 而界面上要显示分类名，两边的转换总得有个地方做。
type promptServiceAdapter struct {
	repo dkdomain.Repository
}

var _ dkhandler.PromptService = promptServiceAdapter{}

// categoryIndex 是一次请求里用得上的分类索引。
//
// 分类只有十几个、几乎不变，每次请求查一遍完全不心疼；
// 但**不要缓存到进程里** —— 同步刚把分类改完，界面上还显示旧名字，
// 管理员会以为同步没生效又点一次。
type categoryIndex struct {
	ordered []*dkdomain.PromptCategory
	bySlug  map[string]*dkdomain.PromptCategory
	byID    map[int64]*dkdomain.PromptCategory
}

func (a promptServiceAdapter) loadCategories(ctx context.Context) (*categoryIndex, error) {
	cats, err := a.repo.ListCategories(ctx)
	if err != nil {
		return nil, err
	}
	idx := &categoryIndex{
		ordered: make([]*dkdomain.PromptCategory, 0, len(cats)),
		bySlug:  make(map[string]*dkdomain.PromptCategory, len(cats)),
		byID:    make(map[int64]*dkdomain.PromptCategory, len(cats)),
	}
	for _, c := range cats {
		if c == nil {
			continue
		}
		idx.ordered = append(idx.ordered, c)
		idx.bySlug[c.Slug] = c
		idx.byID[c.ID] = c
	}
	return idx, nil
}

// ListCategories 分类列表 + 每个分类下有多少条可用的词。
//
// 条数走 CountPromptsByCategory（一句 GROUP BY），**不要按分类查 15 次** ——
// 这个端点是灵感库页面一进去就调的，一次请求打 15 条 COUNT 完全没必要。
func (a promptServiceAdapter) ListCategories(ctx context.Context) ([]*dkhandler.PromptCategoryView, error) {
	idx, err := a.loadCategories(ctx)
	if err != nil {
		return nil, err
	}

	// 只数**没下架也没删**的：界面上写着「电商主图 416」，
	// 点进去却因为过滤掉下架的只剩 400 条，运营会以为漏了词。
	counts, err := a.repo.CountPromptsByCategory(ctx, true)
	if err != nil {
		return nil, err
	}

	out := make([]*dkhandler.PromptCategoryView, 0, len(idx.ordered))
	for _, c := range idx.ordered {
		out = append(out, &dkhandler.PromptCategoryView{Category: c, PromptCount: counts[c.ID]})
	}
	return out, nil
}

// promptProbeLimit 算「多取一条用来判断还有没有下一页」时该向 repository 要多少条。
//
// ⚠ repository 会把每页条数压到 MaxPageLimit（100）。直接要 limit+1 的话，
// 运营把每页设成 100 时要到的 101 会被压回 100，于是**永远判不出还有下一页**，
// 第 101 条之后的词谁也翻不到，而且不报错。
// 所以到顶时反过来把这一页缩一条。返回值是 (这一页真正给几条, 向库里要几条)。
func promptProbeLimit(limit int) (pageSize, probe int) {
	if limit <= 0 {
		limit = dkrepository.DefaultPageLimit
	}
	if limit >= dkrepository.MaxPageLimit {
		limit = dkrepository.MaxPageLimit - 1
	}
	return limit, limit + 1
}

// ListPrompts 检索提示词。
func (a promptServiceAdapter) ListPrompts(ctx context.Context, in dkhandler.ListPromptsInput) (*dkhandler.PromptPage, error) {
	idx, err := a.loadCategories(ctx)
	if err != nil {
		return nil, err
	}

	query := dkdomain.ListPromptsQuery{
		Keyword:  in.Keyword,
		CursorID: in.CursorID,
		// 灵感库只列**没下架也没删**的。
		// 下架过的词仍然能按 uid 单独打开（历史收藏点进来），见 GetPrompt。
		OnlyEnabled: true,
	}
	if slug := in.CategorySlug; slug != "" {
		category, ok := idx.bySlug[slug]
		if !ok {
			// 分类刚被同步改名 / 删掉时，界面上那个旧链接不该变成一个红色报错。
			// 给空页 + 真实总数 0，运营看到的是「这个分类下暂时没有内容」。
			return &dkhandler.PromptPage{Items: []*dkhandler.PromptView{}}, nil
		}
		categoryID := category.ID
		query.CategoryID = &categoryID
	}

	pageSize, probe := promptProbeLimit(in.Limit)
	query.Limit = probe

	rows, err := a.repo.ListPrompts(ctx, query)
	if err != nil {
		return nil, err
	}
	hasMore := len(rows) > pageSize
	if hasMore {
		rows = rows[:pageSize]
	}

	// 总数**不能受游标影响**，否则界面上的「共 N 条」会越翻越小。
	// repository 的 CountPrompts 本身就忽略游标，这里只是照抄同一份过滤条件。
	total, err := a.repo.CountPrompts(ctx, query)
	if err != nil {
		return nil, err
	}

	page := &dkhandler.PromptPage{
		Items:   make([]*dkhandler.PromptView, 0, len(rows)),
		Total:   total,
		HasMore: hasMore,
	}
	for _, p := range rows {
		page.Items = append(page.Items, newPromptView(p, idx))
	}
	if hasMore && len(rows) > 0 {
		page.NextCursorID = rows[len(rows)-1].ID
	}
	return page, nil
}

// GetPrompt 按对外编号取一条。**不过滤 is_enabled**（见 ports.go 的说明）。
func (a promptServiceAdapter) GetPrompt(ctx context.Context, uid string) (*dkhandler.PromptView, error) {
	prompt, err := a.repo.GetPromptByUID(ctx, uid)
	if err != nil {
		return nil, err
	}
	idx, err := a.loadCategories(ctx)
	if err != nil {
		return nil, err
	}
	return newPromptView(prompt, idx), nil
}

// newPromptView 给一条提示词补上它所属分类的 slug 和中文名。
// 分类被删过（ON DELETE SET NULL）或者对不上号时，两个字段都留空串 ——
// 界面上就是「未分类」，不要在这里报错：一条词没有分类不影响运营拿它出图。
func newPromptView(p *dkdomain.Prompt, idx *categoryIndex) *dkhandler.PromptView {
	view := &dkhandler.PromptView{Prompt: p}
	if p == nil || p.CategoryID == nil || idx == nil {
		return view
	}
	if category, ok := idx.byID[*p.CategoryID]; ok && category != nil {
		view.CategorySlug = category.Slug
		view.CategoryName = category.NameZH
		if view.CategoryName == "" {
			view.CategoryName = category.NameEN
		}
		if view.CategoryName == "" {
			view.CategoryName = category.Slug
		}
	}
	return view
}

// ----------------------------------------------------------------------------
// 灵感库（同步）
// ----------------------------------------------------------------------------

// SettingKeyPromptSyncProxyID 是「灵感库同步走哪个代理」这一项在
// designkit_settings 里的键名。值是 JSON：一个数字（代理 ID）或者 null（直连）。
//
// 管理界面（下面的 promptSyncAdapter）**写**它，同步器（dkservice）**读**它，
// 所以两边必须是同一个键名。这里刻意写成「引用同步器那个常量」而不是再抄一遍字面量：
// 抄一遍的话，两边不一致的后果最难查 —— 界面上显示「已选代理 3」，
// 同步却读不到、老老实实走直连，正是「以为走了代理其实没走」这个最糟的状态。
//
// 方向只能是这一边引用那一边：本包 import service，反过来是循环依赖
// （本包 → handler → service）。
const SettingKeyPromptSyncProxyID = dkservice.SettingKeyPromptSyncProxyID

// promptSyncStaleAfter 超过这么久还挂着 running 的同步记录，当成「没有正常结束」。
//
// 没有这条兜底的话：同步跑到一半进程被重启，那条 running 记录永远留在库里，
// 管理员界面就会一直转圈等一个永远不会结束的同步，而「立即同步」按钮
// 被界面禁用着 —— 死局，只能改数据库才能解开。
// 一次同步实测十几秒，10 分钟已经宽裕到不可能误判。
const promptSyncStaleAfter = 10 * time.Minute

// PromptSyncRunner 是真正干活的灵感库同步器。
//
// **由 internal/designkit/service 实现**（去开源提示词库拉一万四千条、
// 做 {argument} → {占位符} 的转换、按 source_ref 幂等入库、记同步记录）。
// 本包只负责把它接到 HTTP 上。
//
// 约定（实现方必须照做）：
//
//   - StartSync **立刻返回**那条已经建好的 running 记录，真正的活在后台跑。
//     一万四千条要十几秒，让 HTTP 请求挂着等，中间任何一个反代超时都会让
//     管理员看到「失败」，然后他会再点一次。
//   - 抢锁用 PostgreSQL 的 advisory lock（pg_try_advisory_lock），
//     **手动和自动共用同一把**。不要用应用内 mutex —— 多副本时它不起作用。
//     （老系统各用一把锁，并发把 1.4 万条写重复了，是审查抓出来的 critical。）
//   - 抢不到锁时返回 domain.NewError(domain.ErrCodeSyncInProgress)
//     （「灵感库正在同步，请等这一次跑完再点。」），并记一条 status='skipped' 的记录。
//   - 代理**只给同步那一个 HTTP 客户端配**：绝不动 http.DefaultTransport，
//     也绝不设 HTTP_PROXY / HTTPS_PROXY 环境变量 —— 那会把出图请求也带进代理。
type PromptSyncRunner interface {
	// StartSync 开一次同步，立刻返回那条 running 记录。
	StartSync(ctx context.Context, kind dkdomain.SyncKind) (*dkdomain.SyncRun, error)
}

// newProxyService 建上游的代理管理服务，套在**我们自己那一个连接池**上。
//
// ent 客户端只是查询构造器，套在已有的 *sql.DB 上不会另开连接池。
//
// ⚠ **永远不要调它的 Close()**：那会把底下的 *sql.DB 一起关掉，
// 而那个池是整个 designkit 在用的。连接池由 Module.Close 统一关。
//
// 为什么不自己写一句 SELECT 去查 proxies 表：那张表是上游的，
// 列名跟着上游走。用上游自己的 repository，跟版时它改列名我们不用管。
func newProxyService(db *sql.DB) *upstreamservice.ProxyService {
	if db == nil {
		return nil
	}
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	return upstreamservice.NewProxyService(upstreamrepo.NewProxyRepository(client, db))
}

// promptSyncAdapter 把「同步记录 + 配置 + 上游代理服务 + 同步器」拼成
// handler 要的 PromptSyncService。
type promptSyncAdapter struct {
	repo    dkdomain.Repository
	proxies *upstreamservice.ProxyService
	runner  PromptSyncRunner
}

var _ dkhandler.PromptSyncService = promptSyncAdapter{}

// StartSync 开一次手动同步。
func (a promptSyncAdapter) StartSync(ctx context.Context) (*dkdomain.SyncRun, error) {
	if a.runner == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithMessage("灵感库同步还没准备好，请联系管理员。").
			WithCause(errors.New("designkit: 灵感库同步器未装配（PromptSyncRunner 为空）"))
	}
	return a.runner.StartSync(ctx, dkdomain.SyncKindManual)
}

// SyncStatus 最近一次同步 + 库里现在有多少条。
func (a promptSyncAdapter) SyncStatus(ctx context.Context) (*dkhandler.SyncStatusView, error) {
	out := &dkhandler.SyncStatusView{}

	latest, err := a.repo.LatestSyncRun(ctx)
	switch {
	case err == nil:
		out.Latest = latest
		out.Running = isSyncRunning(latest, time.Now())
	case errors.Is(err, dkdomain.ErrNotFound):
		// 一次都没同步过。这不是错误 —— 全新部署本来就是这样。
	default:
		return nil, err
	}

	count, err := a.repo.CountPrompts(ctx, dkdomain.ListPromptsQuery{OnlyEnabled: true})
	if err != nil {
		return nil, err
	}
	out.PromptCount = count

	categories, err := a.repo.ListCategories(ctx)
	if err != nil {
		return nil, err
	}
	out.CategoryCount = len(categories)

	return out, nil
}

// isSyncRunning 判断「现在是不是真的有一次在跑」。
//
// 光看 status=='running' 不够：进程在同步中途被重启，那条记录会永远挂着
// running，管理员界面就一直转圈等一个不会结束的同步。见 promptSyncStaleAfter。
func isSyncRunning(run *dkdomain.SyncRun, now time.Time) bool {
	if run == nil || run.Status != dkdomain.SyncStatusRunning {
		return false
	}
	return now.Sub(run.StartedAt) < promptSyncStaleAfter
}

// proxyResolver 把上游的 ProxyService 适配成同步服务要的接口。
//
// 为什么不能直接把 *ProxyService 塞进去：同步服务需要一个
// 「这个代理现在还能用吗」的判断，而上游 ProxyService.GetURL 走的是 GetByID，
// **不过滤 status** —— 代理过期或被停用之后它照样返回一个能用的地址。
// 不加这道判断就会出现「设置页显示直连、同步实际还在走那个停用的代理」，
// 通了没人发现，不通时报的错还指向一个界面上根本没选中的代理。
type proxyResolver struct {
	svc *upstreamservice.ProxyService
}

func (r proxyResolver) GetURL(ctx context.Context, id int64) (string, error) {
	return r.svc.GetURL(ctx, id)
}

// IsActive 用「在不在可用列表里」判断，跟设置页那个下拉框是同一个来源，
// 两边永远不会各说各话。
func (r proxyResolver) IsActive(ctx context.Context, id int64) (bool, error) {
	proxies, err := r.svc.ListActive(ctx)
	if err != nil {
		return false, err
	}
	for i := range proxies {
		if proxies[i].ID == id {
			return true, nil
		}
	}
	return false, nil
}

// GetSyncSettings 现在选的代理 + 可选代理列表。
func (a promptSyncAdapter) GetSyncSettings(ctx context.Context) (*dkhandler.SyncSettings, error) {
	proxyID, err := a.readProxyID(ctx)
	if err != nil {
		return nil, err
	}
	options, err := a.listProxyOptions(ctx)
	if err != nil {
		return nil, err
	}

	// 选中的那个代理后来被管理员删了 / 停用了。
	//
	// ⚠ 原来这里只是「显示上回成 null」，但**不清数据库**，而同步侧是直接读表的，
	// 于是变成「设置页显示直连、同步实际还在走那个停用的代理」——
	// 通了没人发现；不通时报的错指向一个界面上根本没选中的代理，管理员彻底懵。
	//
	// 现在把设置真的清掉，让「页面显示的」和「同步会做的」始终是同一件事。
	// 清不掉也不要紧：同步侧还有一道 IsActive 校验会拦住并报中文错，
	// 绝不会静默走一个停用的代理。
	if proxyID != nil && !containsProxyID(options, *proxyID) {
		slog.Warn("designkit 灵感库同步配置的代理已经不可用，已清掉这项设置（改为直连）",
			slog.Int64("proxy_id", *proxyID))
		if err := a.repo.PutSetting(ctx, SettingKeyPromptSyncProxyID, json.RawMessage("null")); err != nil {
			slog.Warn("designkit 清理失效的同步代理设置失败，下次打开设置页会再试一次",
				slog.Any("error", err))
		}
		proxyID = nil
	}
	return &dkhandler.SyncSettings{ProxyID: proxyID, Proxies: options}, nil
}

// PutSyncSettings 换一个代理。proxyID 为 nil 表示直连。
func (a promptSyncAdapter) PutSyncSettings(ctx context.Context, proxyID *int64) (*dkhandler.SyncSettings, error) {
	options, err := a.listProxyOptions(ctx)
	if err != nil {
		return nil, err
	}

	if proxyID != nil {
		// 选了一个不在列表里的代理时**明确拒绝**，不要静默改成直连：
		// 静默的后果是管理员以为走了代理，实际在裸连，而且拉不动时查不出原因。
		if !containsProxyID(options, *proxyID) {
			return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
				WithMessage("选中的代理不存在或者已经停用了，请刷新页面重新选一个。")
		}
	}

	value, err := json.Marshal(proxyID)
	if err != nil {
		// 走不到：*int64 一定序列化得出来。
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
	if err := a.repo.PutSetting(ctx, SettingKeyPromptSyncProxyID, value); err != nil {
		return nil, err
	}
	return &dkhandler.SyncSettings{ProxyID: proxyID, Proxies: options}, nil
}

// readProxyID 读「同步走哪个代理」。没配过、或者配的是 null，都返回 nil（直连）。
func (a promptSyncAdapter) readProxyID(ctx context.Context) (*int64, error) {
	setting, err := a.repo.GetSetting(ctx, SettingKeyPromptSyncProxyID)
	if err != nil {
		if errors.Is(err, dkdomain.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if setting == nil || len(setting.Value) == 0 {
		return nil, nil
	}

	var proxyID *int64
	if err := json.Unmarshal(setting.Value, &proxyID); err != nil {
		// 有人手工把这一行改坏了。**按直连处理并喊一嗓子**，不要让整个设置页面打不开 ——
		// 打不开的话管理员连「改回去」的入口都没有。
		slog.Warn("designkit 灵感库同步的代理配置读不懂，本次按直连处理",
			slog.String("key", SettingKeyPromptSyncProxyID),
			slog.String("error", err.Error()))
		return nil, nil
	}
	if proxyID != nil && *proxyID <= 0 {
		return nil, nil
	}
	return proxyID, nil
}

// listProxyOptions 列出可以选的代理。
//
// 用上游的 ListActive（只列 status=active 的那些）：停用和过期的代理留在下拉框里
// 只会被选中，然后同步失败，而管理员看着一个「已选好代理」的界面查不出原因。
//
// 代理服务缺席时返回**空列表**而不是报错：那样管理员至少还能看到
// 「现在是直连」并且照常点同步。
func (a promptSyncAdapter) listProxyOptions(ctx context.Context) ([]dkhandler.ProxyOption, error) {
	if a.proxies == nil {
		return []dkhandler.ProxyOption{}, nil
	}
	proxies, err := a.proxies.ListActive(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]dkhandler.ProxyOption, 0, len(proxies))
	for i := range proxies {
		p := proxies[i]
		// **密码绝不带出去。** 上游自己的 dto.Proxy 也是把 Password 标成 `json:"-"`。
		out = append(out, dkhandler.ProxyOption{
			ID:             p.ID,
			Name:           p.Name,
			Protocol:       p.Protocol,
			Host:           p.Host,
			Port:           p.Port,
			Username:       p.Username,
			Status:         p.Status,
			ExpiresAt:      p.ExpiresAt,
			FallbackMode:   p.FallbackMode,
			BackupProxyID:  p.BackupProxyID,
			ExpiryWarnDays: p.ExpiryWarnDays,
			CreatedAt:      p.CreatedAt,
			UpdatedAt:      p.UpdatedAt,
		})
	}
	return out, nil
}

func containsProxyID(options []dkhandler.ProxyOption, id int64) bool {
	for _, o := range options {
		if o.ID == id {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------------------
// designkit 设置（管理员那一页）
// ----------------------------------------------------------------------------
//
// 这一段把 designkit_settings 那张表接给 handler.SettingsService。
//
// 三条规矩，改之前先读一遍：
//
//  1. **读不到 = 用兜底默认值，绝不报错。**
//     设置页是「出了问题去改」的地方。为了一个读坏的配置项让整页打不开，
//     等于把唯一的出路堵死。读坏的项一律回落到 domain 里的 Default*，
//     并在日志里留一句。
//
//  2. **单价缺档就是缺档。**
//     没测出来的档**不写进 unit_prices**（而不是写 0 或空串）——
//     报价那边读不到就走「价格待确认」，这正是我们要的。
//     写 0 会让运营在工作台上看到 $0.00，读成免费。
//
//  3. **只写传上来的那几项。**
//     PutSetting 是一项一条 UPSERT，没传的项根本不会被碰到。

// settingsServiceAdapter 把 repository（+ 上游代理服务，只用来显示名字）
// 适配成 handler.SettingsService。
type settingsServiceAdapter struct {
	repo    dkdomain.Repository
	proxies *upstreamservice.ProxyService
}

var _ dkhandler.SettingsService = settingsServiceAdapter{}

// GetSettings 读全部配置。
func (a settingsServiceAdapter) GetSettings(ctx context.Context) (*dkhandler.SettingsView, error) {
	if a.repo == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithMessage("数据库还没连上，设置页暂时打不开。请稍后再试；一直这样请检查服务是不是正常启动了。")
	}

	view := &dkhandler.SettingsView{
		Ratios:              a.readRatios(ctx),
		Model:               a.readString(ctx, dkdomain.SettingKeyModel, dkdomain.DefaultModel),
		MaxBatchItems:       a.readInt(ctx, dkdomain.SettingKeyMaxBatchItems, dkdomain.DefaultMaxBatchItems),
		ItemTimeoutSeconds:  a.readInt(ctx, dkdomain.SettingKeyItemTimeoutSeconds, dkdomain.DefaultItemTimeoutSeconds),
		MaxDimension:        a.readInt(ctx, dkdomain.SettingKeyMaxDimension, dkdomain.DefaultMaxDimension),
		WorkerConcurrency:   a.readInt(ctx, dkdomain.SettingKeyWorkerConcurrency, dkdomain.DefaultWorkerConcurrency),
		AdminContact:        a.readString(ctx, dkdomain.SettingKeyAdminContact, ""),
		UnitPrices:          a.readUnitPrices(ctx),
		RateMultiplier:      a.readRateMultiplier(ctx),
		PromptSyncProxyID:   nil,
		PromptSyncProxyName: "",
	}

	// 灵感库同步的代理：这一页**只读**，改要去灵感库那一页。
	// 读不出来也不要紧，那一行按「直连」显示 —— 绝不能因此让整页打不开。
	if proxyID := a.readPromptSyncProxyID(ctx); proxyID != nil {
		view.PromptSyncProxyID = proxyID
		view.PromptSyncProxyName = a.proxyName(ctx, *proxyID)
	}

	return view, nil
}

// PutSettings 保存 in 里非 nil 的那几项。
//
// ⚠ 这里**不是一个事务**：PutSetting 一项一条 UPSERT。
// 中途数据库断了的话，会出现「前三项已经改了、后两项没改」。
// 这不做成事务是有意的：designkit_settings 的每一项都是独立生效的，
// 部分保存的后果只是「有几项没改上」，管理员再点一次保存即可；
// 而为它开一个事务要在 repository 上加一整套接口，代价远大于收益。
// 真出这种事时，handler 会把数据库那句错原样翻成中文回给管理员。
func (a settingsServiceAdapter) PutSettings(ctx context.Context, in dkhandler.SettingsInput) (*dkhandler.SettingsView, error) {
	if a.repo == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithMessage("数据库还没连上，这次没保存成功。请稍后再试。")
	}

	if in.Ratios != nil {
		values := make([]string, 0, len(in.Ratios))
		for _, r := range in.Ratios {
			values = append(values, r.String())
		}
		if err := a.putJSON(ctx, dkdomain.SettingKeyRatios, values); err != nil {
			return nil, err
		}
	}
	if in.Model != nil {
		if err := a.putJSON(ctx, dkdomain.SettingKeyModel, *in.Model); err != nil {
			return nil, err
		}
	}
	if in.MaxBatchItems != nil {
		if err := a.putJSON(ctx, dkdomain.SettingKeyMaxBatchItems, *in.MaxBatchItems); err != nil {
			return nil, err
		}
	}
	if in.ItemTimeoutSeconds != nil {
		if err := a.putJSON(ctx, dkdomain.SettingKeyItemTimeoutSeconds, *in.ItemTimeoutSeconds); err != nil {
			return nil, err
		}
	}
	if in.MaxDimension != nil {
		if err := a.putJSON(ctx, dkdomain.SettingKeyMaxDimension, *in.MaxDimension); err != nil {
			return nil, err
		}
	}
	if in.WorkerConcurrency != nil {
		if err := a.putJSON(ctx, dkdomain.SettingKeyWorkerConcurrency, *in.WorkerConcurrency); err != nil {
			return nil, err
		}
	}
	if in.AdminContact != nil {
		if err := a.putJSON(ctx, dkdomain.SettingKeyAdminContact, *in.AdminContact); err != nil {
			return nil, err
		}
	}
	if in.UnitPrices != nil {
		// handler 已经把「留空的档」剔掉了。这里原样写：
		// 空 map 写成 {}，报价那边读到 len==0 就走「价格待确认」。
		if err := a.putJSON(ctx, dkservice.SettingKeyUnitPrices, in.UnitPrices); err != nil {
			return nil, err
		}
	}
	if in.RateMultiplier != nil {
		// 存字符串不存数字：JSON 数字是 float64，1.2 存进去再读出来会带二进制尾巴。
		// 报价那边两种写法都认（service/job.go 的 rateMultiplier）。
		if err := a.putJSON(ctx, dkservice.SettingKeyRateMultiplier, *in.RateMultiplier); err != nil {
			return nil, err
		}
	}

	return a.GetSettings(ctx)
}

// putJSON 把一个值序列化成 JSON 存进 designkit_settings。
func (a settingsServiceAdapter) putJSON(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		// 走不到：存进去的都是字符串 / 整数 / 字符串数组 / map[string]string。
		return dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
	return a.repo.PutSetting(ctx, key, raw)
}

// settingRaw 取一项配置的原始 JSON；没有或读失败返回 ok=false。
func (a settingsServiceAdapter) settingRaw(ctx context.Context, key string) (json.RawMessage, bool) {
	setting, err := a.repo.GetSetting(ctx, key)
	if err != nil {
		if !errors.Is(err, dkdomain.ErrNotFound) {
			slog.Warn("designkit 设置页读配置失败，本次显示默认值",
				slog.String("key", key), slog.Any("error", err))
		}
		return nil, false
	}
	if setting == nil || len(setting.Value) == 0 {
		return nil, false
	}
	return setting.Value, true
}

func (a settingsServiceAdapter) readString(ctx context.Context, key, def string) string {
	raw, ok := a.settingRaw(ctx, key)
	if !ok {
		return def
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		slog.Warn("designkit 设置页：这一项存的不是文字，本次显示默认值",
			slog.String("key", key), slog.String("value", string(raw)))
		return def
	}
	return strings.TrimSpace(value)
}

func (a settingsServiceAdapter) readInt(ctx context.Context, key string, def int) int {
	raw, ok := a.settingRaw(ctx, key)
	if !ok {
		return def
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		slog.Warn("designkit 设置页：这一项存的不是整数，本次显示默认值",
			slog.String("key", key), slog.String("value", string(raw)))
		return def
	}
	return value
}

// readRatios 读出图比例。
//
// 读坏了退回 DefaultRatios()：设置页显示一个空的比例列表，管理员会以为
// 「比例被谁删光了」，然后照着空列表重填一遍 —— 而真相只是这一行存坏了。
func (a settingsServiceAdapter) readRatios(ctx context.Context) []dkdomain.Ratio {
	raw, ok := a.settingRaw(ctx, dkdomain.SettingKeyRatios)
	if !ok {
		return dkdomain.DefaultRatios()
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		slog.Warn("designkit 设置页：出图比例存坏了，本次显示默认比例",
			slog.String("value", string(raw)))
		return dkdomain.DefaultRatios()
	}
	out := make([]dkdomain.Ratio, 0, len(values))
	for _, v := range values {
		parsed, err := dkdomain.ParseRatio(v)
		if err != nil {
			slog.Warn("designkit 设置页：出图比例里有一条写法不对，已跳过",
				slog.String("ratio", v))
			continue
		}
		out = append(out, parsed)
	}
	if len(out) == 0 {
		return dkdomain.DefaultRatios()
	}
	return out
}

// readUnitPrices 读三档单价。
//
// **读不到就是读不到**，返回空 map（= 全部「价格待确认」），
// 绝不编一个数字出来 —— 编出来的数字会被运营当成真的报价。
func (a settingsServiceAdapter) readUnitPrices(ctx context.Context) map[string]string {
	raw, ok := a.settingRaw(ctx, dkservice.SettingKeyUnitPrices)
	if !ok {
		return map[string]string{}
	}
	var table map[string]string
	if err := json.Unmarshal(raw, &table); err != nil {
		slog.Warn("designkit 设置页：出图单价表存坏了，本次按「还没填」显示",
			slog.String("value", string(raw)))
		return map[string]string{}
	}
	out := make(map[string]string, len(table))
	for tier, amount := range table {
		amount = strings.TrimSpace(amount)
		if amount == "" {
			continue
		}
		out[strings.TrimSpace(tier)] = amount
	}
	return out
}

// readRateMultiplier 读价格倍率。没配过返回空串（界面显示占位的 1）。
func (a settingsServiceAdapter) readRateMultiplier(ctx context.Context) string {
	raw, ok := a.settingRaw(ctx, dkservice.SettingKeyRateMultiplier)
	if !ok {
		return ""
	}
	// 数字和字符串两种写法都要认：报价那边（service/job.go 的 rateMultiplier）
	// 就是两种都认的，设置页显示的必须跟它读到的是同一个值。
	text := strings.TrimSpace(strings.Trim(strings.TrimSpace(string(raw)), `"`))
	if text == "" || text == "null" {
		return ""
	}
	return text
}

// readPromptSyncProxyID 读「灵感库同步走哪个代理」。本页只显示，不改。
//
// 读不出来一律当直连：这一行显示错了只是少一句话，
// 而让整个设置页打不开会把「改回去」的入口也一起堵掉。
func (a settingsServiceAdapter) readPromptSyncProxyID(ctx context.Context) *int64 {
	raw, ok := a.settingRaw(ctx, SettingKeyPromptSyncProxyID)
	if !ok {
		return nil
	}
	var proxyID *int64
	if err := json.Unmarshal(raw, &proxyID); err != nil {
		return nil
	}
	if proxyID != nil && *proxyID <= 0 {
		return nil
	}
	return proxyID
}

// proxyName 查代理的名字，只为了在设置页上显示。
// 查不到（代理被删了 / 代理服务缺席）返回空串，那一行就显示成「直连」。
func (a settingsServiceAdapter) proxyName(ctx context.Context, id int64) string {
	if a.proxies == nil {
		return ""
	}
	proxies, err := a.proxies.ListActive(ctx)
	if err != nil {
		return ""
	}
	for i := range proxies {
		if proxies[i].ID == id {
			return proxies[i].Name
		}
	}
	return ""
}
