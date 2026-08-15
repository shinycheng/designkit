package service

// gateway_invoke.go —— 把一张图发给上游网关。
//
// 做法（设计定型 第一节）：**在进程内构造一个合成 gin.Context，重入
// 上游的 OpenAIGatewayHandler.Images()**。这样账号调度、失败切换、限流、
// 计费、用量记录全部复用上游那一套，我们一行都不重写。
//
// 上游自己也是这么干的：internal/handler/image_task_handler.go 的
// newAsyncImageContext(261-278) + run(223-253)。本文件逐步照抄，只有一处
// 不同：AsyncImageHandler 是把客户端送来的 body 原样透传（Clone 入站请求），
// 而我们的图片字节来自对象存储、提示词来自数据库，**没有入站 multipart 可抄**，
// 所以 multipart 要自己拼（见 buildImagesEditsMultipart）。
//
// ⚠ 三处漏了就出随机故障 / 出钱的事故，改这个文件之前先读完：
//
//  1. context.WithoutCancel —— 漏了：运营一关页面，整批请求跟着 canceled，
//     而上游是**故意**把出图和浏览器连接脱钩的（CLAUDE.md 决策 15），
//     图照出、钱照扣，我们这边却拿不到结果 → 图丢了钱花了。
//  2. 每个 item 一份独立的 Request + recorder + gin.Context —— 共用会让并发
//     goroutine 互相覆盖账号选择，且往别人的 Writer 上写会串包或 panic。
//  3. 无条件覆写 ctxkey.ClientRequestID / ctxkey.RequestID —— 漏了：一单 N 张
//     共用入站请求的 id，幂等表 (request_id, api_key_id) 唯一键让第 2 张起
//     **静默不扣钱**（详见 domain/billing_request_id.go 文件头）。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"time"

	// 只为注册解码器：结果图的**实际像素**决定计费落 1K/2K/4K 哪一档
	// （CLAUDE.md B3），所以必须能读出宽高。
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"

	"github.com/gin-gonic/gin"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"
)

// ---------------------------------------------------------------------------
// 依赖（都收成最小接口，单测才塞得进假实现）
// ---------------------------------------------------------------------------

// ImagesHandler 是上游 *handler.OpenAIGatewayHandler 的最小切面。
//
// 故意只声明这一个方法、而不是 import internal/handler：
// ① 单测可以塞假实现，不需要起整套网关；
// ② 少一条对上游包的依赖，跟版时少一处可能的签名冲突。
// 真实实现是 handler.Handlers.OpenAIGateway（在 designkit/handler 里传进来）。
type ImagesHandler interface {
	Images(c *gin.Context)
}

// APIKeyLoader 按主键取一把完整的 API Key（**必须带 User 和 Group**）。
//
// 上游 *service.APIKeyService.GetByID 满足它：它内部走 WithUser().WithGroup()，
// 并且排除了软删除的行。Images() 第一件事就是从 gin 上下文里取 *service.APIKey，
// 取不到直接 401；后面还要用 apiKey.Group 判断分组有没有开生图、
// 用 apiKey.User 做计费准入，所以关联对象缺一不可。
//
// ⚠ 这里取的是 **job 行上记的那把 Key**：
//   - 浏览器提交的批次 → 建 job 时写的是运营的内部专用 Key（见 internal_key.go）
//   - ERP 提交的批次   → 写的是 ERP 自己那把 Key（账要记在 ERP 名下，决策 4）
//
// 所以出图时**不要**再去「找这个用户的内部 Key」，那会把 ERP 的账记错人。
type APIKeyLoader interface {
	GetByID(ctx context.Context, id int64) (*upstreamservice.APIKey, error)
}

// ImageFetcher 下载上游只给了链接、没给 base64 的结果图。
//
// 为什么必须有：钱在上游已经扣掉了，这时候拿不到字节 = 钱花了图丢了。
// 默认实现见 defaultGatewayImageFetcher；单测塞假的，不联网。
type ImageFetcher func(ctx context.Context, url string) (data []byte, contentType string, err error)

// ---------------------------------------------------------------------------
// 常量
// ---------------------------------------------------------------------------

const (
	// imagesEditsURL 合成请求的 URL。
	//
	// ⚠ **路径里必须含 /images/edits 字面量**：上游
	// service/openai_images.go 的 normalizeOpenAIImagesEndpointPath 是拿
	// c.Request.URL.Path 做 strings.Contains 判断端点的，判不出来直接报
	// "unsupported images endpoint"。host 随便写，这个请求不会真的走网络，
	// 只是喂给上游 handler 的一个数据结构。
	imagesEditsURL = "http://designkit.internal/v1/images/edits"

	// multipartImageField multipart 里放图片的字段名。
	//
	// ⚠ **必须是 image**：上游 parseOpenAIImagesMultipartRequest 只认
	// name == "image" 或 "image[" 前缀的文件字段，别的名字会让
	// len(req.Uploads) == 0，然后报 "image file is required"
	// （service/openai_images.go:434）。
	multipartImageField = "image"

	// defaultGatewayImageFilename 上游要求文件部分带非空 filename，否则那一段
	// 会被当成普通表单字段（part.FileName() == "" 走 WriteField 分支）。
	defaultGatewayImageFilename = "image.png"

	// syntheticRemoteAddr 合成请求的来源地址。
	// 上游会取 ClientIP 落进 usage_logs.ip_address；给个环回地址即可，
	// 留空会让日志里出现空 IP，排障时分不清是「没记」还是「记了空」。
	syntheticRemoteAddr = "127.0.0.1:0"

	// maxUpstreamErrorSnippet 上游原文落 last_error_message 的截断长度。
	// 列宽有限，且这一列只给管理员排障看。
	maxUpstreamErrorSnippet = 1024

	// maxFetchedImageBytes 默认下载器单张图的上限，跟上游
	// openAIImageMaxDownloadBytes 对齐（20MB）。
	maxFetchedImageBytes = 20 << 20

	// imageFetchTimeout 下载「只给了链接」的结果图的总超时。
	imageFetchTimeout = 60 * time.Second
)

// stickySessionHeaders 是所有会让上游把请求**粘到同一个账号**的请求头。
//
// 名字来自上游 service/openai_gateway_scheduling.go:24-37 与
// openai_gateway_grok_cache.go:17-18，一个都不能少：
// 只要其中任意一个有值，GenerateExplicitSessionHash 就会算出 sticky hash，
// 一批 50 张全被钉到同一个上游账号上，撞上 accounts.concurrency 默认 3，
// 批量退化成三张三张串行。
//
// 我们的合成请求头是自己造的，本来就不会带这些；这里显式删一遍是**防守**：
// 万一以后有人往 baseHeaders 里加东西、或者从入站请求拷头，这一步能兜住。
var stickySessionHeaders = []string{
	"session_id",
	"conversation_id",
	"X-Session-Affinity",
	"X-Session-Id",
	"X-OpenCode-Session",
	"X-Conversation-ID",
	"X-Grok-Conv-Id",
	"X-Claude-Code-Session-Id",
}

// ---------------------------------------------------------------------------
// GatewayInvoker
// ---------------------------------------------------------------------------

// GatewayInvoker 实现 domain.GatewayInvoker。
type GatewayInvoker struct {
	images ImagesHandler
	keys   APIKeyLoader
	fetch  ImageFetcher
	log    *slog.Logger
}

// GatewayInvokerOption 可选配置。
type GatewayInvokerOption func(*GatewayInvoker)

// WithImageFetcher 换掉默认的图片下载器（单测用）。
func WithImageFetcher(f ImageFetcher) GatewayInvokerOption {
	return func(g *GatewayInvoker) {
		if f != nil {
			g.fetch = f
		}
	}
}

// WithGatewayInvokerLogger 换掉默认日志器。
func WithGatewayInvokerLogger(l *slog.Logger) GatewayInvokerOption {
	return func(g *GatewayInvoker) {
		if l != nil {
			g.log = l
		}
	}
}

// NewGatewayInvoker 造一个出图调用器。
//
// images 传上游的 h.OpenAIGateway，keys 传上游的 apiKeyService。
// 两个都不能为 nil —— 为 nil 时 Generate 会直接返回 DK_INTERNAL 而不是 panic。
func NewGatewayInvoker(images ImagesHandler, keys APIKeyLoader, opts ...GatewayInvokerOption) *GatewayInvoker {
	g := &GatewayInvoker{
		images: images,
		keys:   keys,
		fetch:  defaultGatewayImageFetcher,
		log:    slog.Default(),
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// 编译期断言：接口对得上。
var _ dkdomain.GatewayInvoker = (*GatewayInvoker)(nil)

// Generate 出一张图。
//
// 返回的 error 一律是 *domain.DesignkitError（Message 是中文，Upstream 是英文原文）。
func (g *GatewayInvoker) Generate(ctx context.Context, req dkdomain.GenerateRequest) (result dkdomain.GenerateResult, err error) {
	// 每个出图 goroutine 第一行 defer recover（设计定型 1.4）。
	// 上游 Images() 内部自己也有一层 recover，但它只覆盖它自己的调用栈；
	// 我们这边的 multipart 拼装、结果解析同样可能 panic，一张图 panic 掉
	// 整个 worker 进程 = 整批任务卡死。
	defer func() {
		if recovered := recover(); recovered != nil {
			g.logger().Error("designkit 出图调用 panic",
				slog.String("billing_request_id", req.BillingRequestID),
				slog.Any("panic", recovered))
			result = dkdomain.GenerateResult{}
			err = dkdomain.NewError(dkdomain.ErrCodeInternal).
				WithCause(fmt.Errorf("designkit: gateway invoke panicked: %v", recovered))
		}
	}()

	if g == nil || g.images == nil || g.keys == nil {
		return dkdomain.GenerateResult{}, dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithCause(errors.New("designkit: gateway invoker 未正确装配"))
	}

	billingRequestID, err := resolveGatewayBillingRequestID(req)
	if err != nil {
		return dkdomain.GenerateResult{}, err
	}
	req.BillingRequestID = billingRequestID

	if err := validateGatewayGenerateRequest(req); err != nil {
		return dkdomain.GenerateResult{}, err
	}

	apiKey, err := g.loadAPIKey(ctx, req)
	if err != nil {
		return dkdomain.GenerateResult{}, err
	}

	body, contentType, err := buildImagesEditsMultipart(req)
	if err != nil {
		return dkdomain.GenerateResult{}, err
	}

	taskCtx, recorder, cancel, err := newSyntheticImagesContext(ctx, apiKey, req, body, contentType)
	if err != nil {
		return dkdomain.GenerateResult{}, err
	}
	defer cancel()

	// 重入上游网关。同步返回，返回时上游已经写完 recorder。
	g.images.Images(taskCtx)

	return g.readResult(ctx, taskCtx, recorder, req)
}

func (g *GatewayInvoker) logger() *slog.Logger {
	if g == nil || g.log == nil {
		return slog.Default()
	}
	return g.log
}

// loadAPIKey 取出这一张图要记账的那把 Key，并做归属校验。
func (g *GatewayInvoker) loadAPIKey(ctx context.Context, req dkdomain.GenerateRequest) (*upstreamservice.APIKey, error) {
	apiKey, err := g.keys.GetByID(ctx, req.APIKeyID)
	if err != nil || apiKey == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).WithCause(err)
	}
	// 归属校验：Key 必须属于这个 job 的用户。
	// 不匹配说明数据被改坏了或调用方传错了 —— 这时候继续跑就是**把钱记到别人头上**，
	// 宁可这一张失败。
	if apiKey.UserID != req.UserID {
		return nil, dkdomain.NewError(dkdomain.ErrCodeForbidden).
			WithCause(fmt.Errorf("designkit: api key %d 属于用户 %d，与任务用户 %d 不符", apiKey.ID, apiKey.UserID, req.UserID))
	}
	if !apiKey.IsActive() {
		return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
			WithCause(fmt.Errorf("designkit: api key %d 状态为 %s", apiKey.ID, apiKey.Status))
	}
	// Images() 会用 apiKey.User 做计费准入（billingCacheService.CheckBillingEligibility），
	// User 为 nil 会直接 panic 在上游那边，这里提前拦下并给中文提示。
	if apiKey.User == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
			WithCause(fmt.Errorf("designkit: api key %d 没有加载到用户信息", apiKey.ID))
	}
	return apiKey, nil
}

// ---------------------------------------------------------------------------
// 入参校验
// ---------------------------------------------------------------------------

// resolveGatewayBillingRequestID 定下这一张图的计费 id，并守住长度。
//
// 两件事：
//   - 调用方没给（空串）时，用 (JobUID, Seq, Attempt) 现算一个；
//   - 调用方给了，就跟现算的比对，**不一致直接失败**。
//     不一致 = 这一张要么重复扣钱、要么静默不扣钱，是钱的事故，不能放过去。
//
// 最后强制断言 len("client:" + id) <= 64：超了会让上游写 usage_logs 时
// 抛 PostgreSQL 22001，而扣费和写账单是两个事务、扣费在前 ——
// 结果是**钱扣了、账单没有，且没有任何告警**（CLAUDE.md B1）。
func resolveGatewayBillingRequestID(req dkdomain.GenerateRequest) (string, error) {
	given := strings.TrimSpace(req.BillingRequestID)

	derivable := dkdomain.ValidateBillingRequestIDParts(req.JobUID, req.Seq, req.Attempt) == nil
	if derivable {
		derived := dkdomain.BillingRequestID(req.JobUID, req.Seq, req.Attempt)
		if given == "" {
			given = derived
		} else if given != derived {
			return "", dkdomain.NewError(dkdomain.ErrCodeInternal).
				WithCause(fmt.Errorf("designkit: billing_request_id 与 (job=%s, seq=%d, attempt=%d) 推导值不一致：给的是 %q，应该是 %q",
					req.JobUID, req.Seq, req.Attempt, given, derived))
		}
	}

	if given == "" {
		return "", dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithCause(errors.New("designkit: billing_request_id 为空，且 (job_uid, seq, attempt) 也算不出来"))
	}

	if stored := dkdomain.UpstreamClientRequestIDPrefix + given; len(stored) > dkdomain.UsageLogRequestIDMaxLen {
		return "", dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithCause(fmt.Errorf("designkit: billing_request_id 落 usage_logs 会超长（%d > %d）：%q",
				len(stored), dkdomain.UsageLogRequestIDMaxLen, stored))
	}
	return given, nil
}

func validateGatewayGenerateRequest(req dkdomain.GenerateRequest) error {
	switch {
	case req.UserID <= 0:
		return dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithCause(errors.New("designkit: GenerateRequest.UserID 必须大于 0"))
	case req.APIKeyID <= 0:
		// 上游 Images() 第一件事就是 GetAPIKeyFromContext，取不到直接 401。
		return dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
			WithCause(errors.New("designkit: GenerateRequest.APIKeyID 必须大于 0"))
	case len(req.ImageData) == 0:
		return dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithCause(errors.New("designkit: GenerateRequest.ImageData 为空"))
	case strings.TrimSpace(req.Prompt) == "":
		return dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithCause(errors.New("designkit: GenerateRequest.Prompt 为空"))
	}
	return nil
}

// ---------------------------------------------------------------------------
// multipart（上游没得抄，全靠自己拼）
// ---------------------------------------------------------------------------

// buildImagesEditsMultipart 拼出 /v1/images/edits 的 multipart body。
//
// 字段：
//
//	image  （文件，字段名写死，见 multipartImageField）
//	prompt （文本）
//	model  （文本）
//	size   （文本，空就不写 —— 写空串会让上游把 ExplicitSize 判成 false 之外
//	        还多一个空字段，不如不写）
//
// 故意**不写** n / response_format / stream 等字段：
// 上游 applyOpenAIImagesDefaults 会把 n 补成 1，多写反而会把请求从
// images-basic 档推到 images-native 档（classifyOpenAIImagesCapability），
// 那会改变账号能力筛选的结果。
func buildImagesEditsMultipart(req dkdomain.GenerateRequest) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	filename := sanitizeGatewayImageFilename(req.ImageFilename)
	contentType := strings.TrimSpace(req.ImageContentType)
	if contentType == "" {
		contentType = http.DetectContentType(req.ImageData)
	}

	// 用 CreatePart 而不是 CreateFormFile：后者把 Content-Type 写死成
	// application/octet-stream，上游会把它原样带给网关，有的网关据此拒收。
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name=%q; filename=%q`, multipartImageField, filename))
	header.Set("Content-Type", contentType)

	part, err := w.CreatePart(header)
	if err != nil {
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
	if _, err := part.Write(req.ImageData); err != nil {
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}

	if err := w.WriteField("prompt", req.Prompt); err != nil {
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = dkdomain.DefaultModel
	}
	if err := w.WriteField("model", model); err != nil {
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}

	if size := strings.TrimSpace(req.Size); size != "" {
		if err := w.WriteField("size", size); err != nil {
			return nil, "", dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
		}
	}

	if err := w.Close(); err != nil {
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// sanitizeGatewayImageFilename 去掉会破坏 Content-Disposition 头的字符。
// filename 只是给上游认扩展名用的，语义不重要，但不能带引号、反斜杠和换行。
func sanitizeGatewayImageFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		switch r {
		case '"', '\\', '\r', '\n':
			return -1
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "" {
		return defaultGatewayImageFilename
	}
	return name
}

// ---------------------------------------------------------------------------
// 合成 gin.Context（六步）
// ---------------------------------------------------------------------------

// newSyntheticImagesContext 造一个能直接喂给上游 Images() 的 gin.Context。
//
// 六步（设计定型 1.1，照抄 image_task_handler.go:261-278）：
//
//  1. context.WithoutCancel(原始 ctx)
//  2. context.WithTimeout(单张超时)
//  3. 新建 http.Request（不是 Clone —— 我们没有入站 multipart 可 Clone）
//  4. 换 Body / GetBody / ContentLength
//  5. 每个 item 一份独立的 gin.Context
//  6. Writer 指向这一张图自己的 recorder
//
// 关于第 5、6 步为什么这里用 gin.CreateTestContext 而不是 c.Copy()：
// 上游 AsyncImageHandler 手里有一个入站请求的 *gin.Context，所以它 Copy 一份；
// 我们是队列 worker，**根本没有入站请求**，只能凭空造一个。
// gin.CreateTestContext 造出来的 Context 不来自 gin 的对象池、也永远不会被归还，
// 天然满足「每个 item 一份、Writer 是自己的」这两个要求 ——
// 而 c.Copy() 造出来的副本 Writer 底层是 nil，必须再手动接一个 recorder 才能写，
// 那一步正是上游 taskCtx.Writer = recorderCtx.Writer 在干的事。
// gin.CreateTestContext 虽然名字带 Test，但它是 gin 正式包里的导出函数，
// 上游自己的生产代码（image_task_handler.go:274）也在用。
func newSyntheticImagesContext(
	parent context.Context,
	apiKey *upstreamservice.APIKey,
	req dkdomain.GenerateRequest,
	body []byte,
	contentType string,
) (*gin.Context, *httptest.ResponseRecorder, context.CancelFunc, error) {
	if parent == nil {
		parent = context.Background()
	}
	// 防御：Images() 取不到 API Key 直接 401，取不到 User 会在计费准入那里炸。
	// 正常路径上 loadAPIKey 已经保证了这两个，这里再兜一次，免得以后有人
	// 直接调用这个函数。
	if apiKey == nil || apiKey.User == nil {
		return nil, nil, nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
			WithCause(errors.New("designkit: 合成请求缺少 API Key 或用户信息"))
	}

	// 1. 跟调用方的取消信号脱钩。
	//    运营点「停止排队」/ 关网页 → 我们这边的 ctx 会被 cancel，
	//    但上游那张图**已经在出了、钱已经在扣了**（决策 15），
	//    这时候把请求掐掉只会让我们拿不到结果：钱花了、图丢了。
	base := context.WithoutCancel(parent)

	// ⚠ 无条件覆写（不是「没有才设」）。
	//    上游 resolveUsageBillingRequestID（service/gateway_usage_billing.go:206-226）
	//    优先取 ctx 里的 ClientRequestID，其次 RequestID；而 RequestLogger 是全局
	//    中间件，入站请求注进来的 id 会被 WithoutCancel 完整继承。
	//    不覆写 → 一单 N 张共用同一个 id → 幂等表 (request_id, api_key_id) 唯一键
	//    → 第 2 张起 applied=false，**静默不扣钱**。
	//    ERP 那边固定填一个 X-Request-Id 更狠：那把 Key 下所有任务全塌缩成一条。
	base = context.WithValue(base, ctxkey.ClientRequestID, req.BillingRequestID)
	base = context.WithValue(base, ctxkey.RequestID, req.BillingRequestID)

	// 补上 apiKeyAuth 中间件平时会写进 request context 的两个值
	// （middleware/api_key_auth.go:166 与 setGroupContext:383-392）。
	// 少了它们，上游的用户级策略和分组级限流会拿不到主体。
	if apiKey != nil && apiKey.User != nil {
		base = context.WithValue(base, ctxkey.UserID, apiKey.User.ID)
	}
	if apiKey != nil && upstreamservice.IsGroupContextValid(apiKey.Group) {
		base = context.WithValue(base, ctxkey.Group, apiKey.Group)
	}

	// 2. 单张超时。值由调用方从 designkit_settings.item_timeout_seconds 读出来传进来，
	//    这里只兜一个底，不硬编码业务值。
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = time.Duration(dkdomain.DefaultItemTimeoutSeconds) * time.Second
	}
	execCtx, cancel := context.WithTimeout(base, timeout)

	// 3. 新建请求。
	httpReq, err := http.NewRequestWithContext(execCtx, http.MethodPost, imagesEditsURL, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, nil, nil, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}

	// 4. Body / GetBody / ContentLength 三个都要设。
	//    上游读 body 用 ReadRequestBodyWithPrealloc(c.Request)，它按 ContentLength
	//    预分配；失败切换账号时可能要重放 body，那时候取的是 GetBody。
	httpReq.Body = io.NopCloser(bytes.NewReader(body))
	httpReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	httpReq.ContentLength = int64(len(body))
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.RemoteAddr = syntheticRemoteAddr

	// 显式剔除所有 sticky session 头，理由见 stickySessionHeaders 注释。
	stripStickySessionHeaders(httpReq.Header)

	// 5 + 6. 这一张图自己的 gin.Context 和 recorder。
	recorder := httptest.NewRecorder()
	taskCtx, _ := gin.CreateTestContext(recorder)
	taskCtx.Request = httpReq

	// 补上 apiKeyAuth 平时会写进 gin.Context 的三个键
	// （middleware/api_key_auth.go:275-280）。Images() 取不到 API Key 直接 401。
	taskCtx.Set(string(middleware.ContextKeyAPIKey), apiKey)
	taskCtx.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{
		UserID:      apiKey.User.ID,
		Concurrency: apiKey.User.Concurrency,
	})
	taskCtx.Set(string(middleware.ContextKeyUserRole), apiKey.User.Role)

	return taskCtx, recorder, cancel, nil
}

func stripStickySessionHeaders(h http.Header) {
	for _, name := range stickySessionHeaders {
		h.Del(name)
	}
}

// ---------------------------------------------------------------------------
// 结果读取（照抄 image_task_handler.go 的 run()）
// ---------------------------------------------------------------------------

func (g *GatewayInvoker) readResult(
	parent context.Context,
	taskCtx *gin.Context,
	recorder *httptest.ResponseRecorder,
	req dkdomain.GenerateRequest,
) (dkdomain.GenerateResult, error) {
	// 非流式生图会周期性往 body 里写 " \n" 保活
	// （service/openai_images_json_keepalive.go），所以先 TrimSpace，
	// 否则 json.Valid 直接判假。
	body := bytes.TrimSpace(recorder.Body.Bytes())

	status := recorder.Code
	if status == 0 {
		// httptest.NewRecorder 默认就是 200，但上游这么兜过底，我们保持一致。
		status = http.StatusOK
	}

	stored := dkdomain.UpstreamClientRequestIDPrefix + req.BillingRequestID

	// 超时：上游一个字都没写回来。
	if ctxErr := taskCtx.Request.Context().Err(); ctxErr != nil && len(body) == 0 {
		return dkdomain.GenerateResult{UpstreamStatus: status, StoredBillingRequestID: stored},
			dkdomain.NewError(dkdomain.ErrCodeTimeout).WithCause(ctxErr)
	}

	result := dkdomain.GenerateResult{
		UpstreamStatus:         status,
		RawResponse:            append([]byte(nil), body...),
		StoredBillingRequestID: stored,
	}

	if len(body) == 0 || !json.Valid(body) {
		return result, dkdomain.NewError(dkdomain.ErrCodeUpstreamError).
			WithUpstream(gatewaySnippet(body)).
			WithCause(errors.New("designkit: 上游返回的不是合法 JSON"))
	}

	payload := parseGatewayImagesResponse(body)

	// 取图用的 ctx 也要跟调用方的取消脱钩：图已经出了、钱已经扣了，
	// 这时候因为「运营关了网页」而放弃下载，就是白花钱。
	fetchCtx, fetchCancel := context.WithTimeout(context.WithoutCancel(parent), imageFetchTimeout)
	defer fetchCancel()

	// 先取图：**只要有图就算成功**。
	// 上游在「已经开始写响应之后才失败」的场景里可能既给图又给 error 字段
	// （openai_images.go:264 的 forward_partial_error_with_image_result），
	// 这时候钱已经花了，把图丢掉是最亏的做法。
	images, fetchErr := g.collectImages(fetchCtx, payload)
	if len(images) > 0 {
		if fetchErr != nil {
			g.logger().Warn("designkit 部分结果图取不到字节，已保留取到的部分",
				slog.String("billing_request_id", req.BillingRequestID),
				slog.Any("error", fetchErr))
		}
		result.Images = images
		return result, nil
	}

	// 没有图 → 翻译错误。关键词优先于状态码（MapUpstreamError 内部就是这么做的）。
	if msg := payload.errorMessage(); msg != "" {
		return result, dkdomain.MapUpstreamError(status, msg)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return result, dkdomain.MapUpstreamError(status, gatewaySnippet(body))
	}
	if fetchErr != nil {
		return result, fetchErr
	}
	return result, dkdomain.NewError(dkdomain.ErrCodeUpstreamError).
		WithUpstream(gatewaySnippet(body)).
		WithCause(errors.New("designkit: 上游返回 2xx 但没有任何图片"))
}

// gatewayImagesResponse 是 /v1/images/* 的返回体里我们关心的那几个字段。
//
// 兼容三种 base64 字段名：b64_json（官方）、base64、image_base64。
// 这跟上游 collectOpenAIImageInlineAssets（service/openai_images.go:1377）
// 的 firstNonEmptyString 取值顺序一致。
type gatewayImagesResponse struct {
	Data []struct {
		B64JSON     string `json:"b64_json"`
		Base64      string `json:"base64"`
		ImageBase64 string `json:"image_base64"`
		URL         string `json:"url"`
		MimeType    string `json:"mime_type"`
		ContentType string `json:"content_type"`
	} `json:"data"`
	Error json.RawMessage `json:"error"`
}

func parseGatewayImagesResponse(body []byte) *gatewayImagesResponse {
	out := &gatewayImagesResponse{}
	// 解析失败不当致命错误：可能是我们没见过的返回形状，
	// 后面还有 errorMessage / 状态码两层兜底。
	_ = json.Unmarshal(body, out)
	return out
}

// errorMessage 从 error 字段里挖出人能读的英文原文。
// 三种形状都见过：{"error":{"message":".."}}、{"error":"文本"}、{"error":{...}}。
func (r *gatewayImagesResponse) errorMessage() string {
	if r == nil || len(r.Error) == 0 {
		return ""
	}
	// ⚠ 很多实现成功时也会带一个 "error": null / {} / []。
	// 不把它们当错误，否则会把「出图成功」误判成「出图失败」，
	// 图丢了、钱照扣。
	switch strings.TrimSpace(string(r.Error)) {
	case "", "null", "{}", "[]", `""`:
		return ""
	}

	var asObject struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if json.Unmarshal(r.Error, &asObject) == nil {
		if msg := strings.TrimSpace(asObject.Message); msg != "" {
			return msg
		}
		if t := strings.TrimSpace(asObject.Type); t != "" {
			return t
		}
	}
	var asString string
	if json.Unmarshal(r.Error, &asString) == nil {
		if msg := strings.TrimSpace(asString); msg != "" {
			return msg
		}
	}
	return gatewaySnippet(r.Error)
}

// collectImages 把返回体里的每一张图变成字节。
//
// 返回的 error 只在「一张都没取到」时才该被当成失败 —— 取到几张算几张，
// 因为钱是按上游实际出图算的，我们丢一张就是白花一张的钱。
func (g *GatewayInvoker) collectImages(ctx context.Context, payload *gatewayImagesResponse) ([]dkdomain.GeneratedImage, error) {
	if payload == nil || len(payload.Data) == 0 {
		return nil, nil
	}

	images := make([]dkdomain.GeneratedImage, 0, len(payload.Data))
	var firstErr error

	for i := range payload.Data {
		entry := payload.Data[i]
		declaredType := gatewayFirstNonEmpty(entry.MimeType, entry.ContentType)

		var (
			data        []byte
			contentType string
			err         error
		)

		if encoded := gatewayFirstNonEmpty(entry.B64JSON, entry.Base64, entry.ImageBase64); encoded != "" {
			data, contentType, err = gatewayDecodeBase64Image(encoded)
			if contentType == "" {
				contentType = declaredType
			}
		} else if url := strings.TrimSpace(entry.URL); url != "" {
			if g.fetch == nil {
				err = dkdomain.NewError(dkdomain.ErrCodeUpstreamError).
					WithMessage("图片出好了但只给了下载链接，系统没有配置下载通道，请联系管理员。").
					WithUpstream(url)
			} else {
				data, contentType, err = g.fetch(ctx, url)
				if err != nil {
					err = dkdomain.NewError(dkdomain.ErrCodeStorageError).WithCause(err).WithUpstream(url)
				}
				if contentType == "" {
					contentType = declaredType
				}
			}
		} else {
			continue // data 里既没有 base64 也没有 url，不是一张图（可能是 revised_prompt 之类）
		}

		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if len(data) == 0 {
			continue
		}

		if strings.TrimSpace(contentType) == "" {
			contentType = http.DetectContentType(data)
		}
		width, height := gatewayDecodeImageSize(data)
		images = append(images, dkdomain.GeneratedImage{
			Data:        data,
			ContentType: contentType,
			Width:       width,
			Height:      height,
		})
	}

	return images, firstErr
}

// gatewayDecodeBase64Image 解 base64，顺带认 data: URL 前缀。
func gatewayDecodeBase64Image(encoded string) ([]byte, string, error) {
	encoded = strings.TrimSpace(encoded)
	contentType := ""

	if strings.HasPrefix(encoded, "data:") {
		if comma := strings.IndexByte(encoded, ','); comma > 0 {
			meta := encoded[len("data:"):comma]
			meta = strings.TrimSuffix(meta, ";base64")
			contentType = strings.TrimSpace(meta)
			encoded = encoded[comma+1:]
		}
	}
	// 有的实现会在 base64 里塞换行。
	encoded = strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(encoded)

	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// 少数实现用不带 padding 的 raw 编码。
		if raw, rawErr := base64.RawStdEncoding.DecodeString(encoded); rawErr == nil {
			return raw, contentType, nil
		}
		return nil, contentType, dkdomain.NewError(dkdomain.ErrCodeUpstreamError).
			WithCause(fmt.Errorf("designkit: 结果图 base64 解码失败: %w", err))
	}
	return data, contentType, nil
}

// gatewayDecodeImageSize 读出实际像素。读不出来返回 0,0 —— 调用方用
// domain.ClassifyBillingTierOrDefault 兜底到 2K 档，不要因为读不出宽高就丢图。
func gatewayDecodeImageSize(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// defaultGatewayImageFetcher 默认下载器：只做一次 GET，限流 20MB，超时跟随 ctx。
func defaultGatewayImageFetcher(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, "", fmt.Errorf("designkit: 下载结果图失败，HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchedImageBytes))
	if err != nil {
		return nil, "", err
	}
	return data, strings.TrimSpace(resp.Header.Get("Content-Type")), nil
}

func gatewayFirstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// gatewaySnippet 截断上游原文，只落 last_error_message 给管理员看。
func gatewaySnippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) <= maxUpstreamErrorSnippet {
		return text
	}
	return text[:maxUpstreamErrorSnippet] + "…"
}
