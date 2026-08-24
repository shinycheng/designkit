package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ============================================================================
// 高清放大（Real-ESRGAN ×4，跑在 Python imgsvc 里）
// ============================================================================
//
// 两块东西：
//
//	1. ImgsvcUpscaleClient —— POST imgsvc /v1/upscale 的 HTTP 客户端。
//	2. UpscaleService      —— 任务表（designkit_upscale_tasks，9004）+ 单 worker：
//	   运营点「高清放大」立刻返回 queued，后台一张一张放；前端每 5 秒查一次状态。
//
// 【任务表为准，内存只剩队列信道】（9004 起；原来是纯内存队列，重启丢任务）
// 状态的唯一真相在数据库：入队 = 插行，Status 查询 = 查最新一行，
// 状态流转 = 带守卫的 UPDATE。内存里只有一条 chan 给 worker 递任务编号，
// 丢了也无所谓——重启时把 status IN (queued, running) 的任务重新入队接着放
//（running 的重置回 queued：上次进程死在半路）。运营点过就不用再点第二次。
//
// 【为什么单 worker、队列封顶 10】
// imgsvc 那边单块 tile 推理峰值按 1~2GB 估，串行是内存的硬要求；
// 队列封顶是为了别让 100 个排队任务把「等 1~2 分钟」变成「等一下午」——
// 满了直接告诉运营「排队满了，等会儿再试」。
//
// 【去重】
// 同一张商品图已经在排队/在放/放完了，再点一次不入队，直接返回现有任务
//（按「这张图最新的一行」判）；failed 的重新入队 = 插一行新任务，旧行留作历史。
// 放完的结果本身也走 sha256 去重入库（UploadAsset），磁盘上永远只有一份。

const (
	// EnvUpscaleTimeoutSeconds 单张放大的超时秒数，默认 300（5 分钟）。
	// 值要盖过「NAS arm64 上最慢那张」——超时判失败但 Python 侧还在算，
	// 白算一张的代价远小于把能成的放大判死。
	EnvUpscaleTimeoutSeconds = "DESIGNKIT_UPSCALE_TIMEOUT_SECONDS"

	// DefaultUpscaleTimeout 单张放大默认超时。
	DefaultUpscaleTimeout = 5 * time.Minute

	// upscalePath imgsvc 的放大端点。
	upscalePath = "/v1/upscale"

	// defaultUpscaleQueueCap 队列封顶（不含正在放的那一张）。设计冻结值 10。
	defaultUpscaleQueueCap = 10

	// maxUpscaleResponseBytes 放大结果的响应体上限。
	// ⚠ 不能沿用 preprocess 的 64MB：×4 之后一张 5016×5016 的照片级 PNG
	// 就可能 30~60MB，输入顶格（2048×2048 → 8192×8192）时更大。
	maxUpscaleResponseBytes = 256 << 20

	// maxUpscaleAssetBytes 放大结果入库时的大小上限（绕过 designkit_settings
	// 里给「运营上传」定的 20MB——那是给手机原图定的，放大产物天然更大）。
	maxUpscaleAssetBytes = 256 << 20
)

// ----------------------------------------------------------------------------
// HTTP 客户端
// ----------------------------------------------------------------------------

// UpscaleBackend 是「把一张图放大」这一个能力，UpscaleService 依赖它。
// 单测塞假实现，生产走 ImgsvcUpscaleClient。
type UpscaleBackend interface {
	// Upscale 输入原图字节，返回放大后的 PNG 字节。失败返回 *DesignkitError。
	Upscale(ctx context.Context, data []byte, filename, contentType string) ([]byte, error)
}

// ImgsvcUpscaleClient 是 UpscaleBackend 的唯一生产实现。
type ImgsvcUpscaleClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

var _ UpscaleBackend = (*ImgsvcUpscaleClient)(nil)

// NewUpscaleClientFromEnv 从环境变量建客户端。
// 地址和 token 跟预处理共用同一组（DESIGNKIT_IMGSVC_URL / _TOKEN）——
// 放大就跑在同一个 imgsvc 容器里，配两遍只会漂。
func NewUpscaleClientFromEnv() (*ImgsvcUpscaleClient, error) {
	base := strings.TrimSpace(os.Getenv(EnvImgsvcURL))
	if base == "" {
		base = DefaultImgsvcURL
	}
	timeout := DefaultUpscaleTimeout
	if raw := strings.TrimSpace(os.Getenv(EnvUpscaleTimeoutSeconds)); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds <= 0 {
			// 配错了不静默退化，跟 preprocess 客户端同一条纪律。
			return nil, fmt.Errorf("designkit: %s 必须是正整数秒，实际 %q", EnvUpscaleTimeoutSeconds, raw)
		}
		timeout = time.Duration(seconds) * time.Second
	}
	return NewUpscaleClient(base, strings.TrimSpace(os.Getenv(EnvImgsvcToken)), timeout)
}

// NewUpscaleClient 建客户端。BaseURL 不合法直接报错，不延迟到第一次调用。
func NewUpscaleClient(baseURL, token string, timeout time.Duration) (*ImgsvcUpscaleClient, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("designkit: 图像服务地址为空（检查 %s）", EnvImgsvcURL)
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("designkit: 图像服务地址不合法：%q（应形如 %s）", baseURL, DefaultImgsvcURL)
	}
	if timeout <= 0 {
		timeout = DefaultUpscaleTimeout
	}
	return &ImgsvcUpscaleClient{
		baseURL: base,
		token:   token,
		httpClient: &http.Client{
			Timeout: timeout,
			// Proxy 显式 nil，理由同 preprocess 客户端：容器间内部调用
			// 绝不能被环境里的 HTTP_PROXY 劫走（NO_PROXY 名单靠不住）。
			Transport: &http.Transport{
				Proxy: nil,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:          4,
				MaxIdleConnsPerHost:   2,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}, nil
}

// Upscale 调 POST /v1/upscale。**不重试**：一张要一两分钟，重试策略交给
// 运营的手指（失败 → 再点一次 = 重新入队），比机器盲试三遍省得多。
func (c *ImgsvcUpscaleClient) Upscale(ctx context.Context, data []byte, filename, contentType string) ([]byte, error) {
	if c == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpscaleUnavailable)
	}
	if len(data) == 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpscaleFailed).
			WithMessage("要放大的图片是空的。")
	}

	body, bodyContentType, err := buildUpscaleBody(data, filename, contentType)
	if err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpscaleFailed).WithCause(err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+upscalePath, bytes.NewReader(body))
	if err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpscaleFailed).WithCause(err)
	}
	httpReq.Header.Set("Content-Type", bodyContentType)
	httpReq.ContentLength = int64(len(body))
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpscaleFailed).
			WithMessage("连不上图像处理服务，重试一次；一直这样请联系管理员。").
			WithUpstream(err.Error()).
			WithCause(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamMessageLen*4))
		return nil, upscaleErrorFromResponse(resp.StatusCode, raw)
	}

	out, err := io.ReadAll(io.LimitReader(resp.Body, maxUpscaleResponseBytes+1))
	if err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpscaleFailed).WithCause(err)
	}
	if len(out) == 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpscaleFailed).
			WithMessage("图像处理服务返回了空图片。").
			WithUpstream("imgsvc returned empty body")
	}
	if len(out) > maxUpscaleResponseBytes {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpscaleFailed).
			WithMessage("放大结果太大，存不下来。换一张小一点的图。")
	}
	return out, nil
}

// buildUpscaleBody 拼 multipart 表单（只有一个 file 字段）。
func buildUpscaleBody(data []byte, filename, contentType string) ([]byte, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	name := strings.TrimSpace(filename)
	if name == "" {
		name = "input"
	}
	if contentType = strings.TrimSpace(contentType); contentType == "" {
		contentType = "application/octet-stream"
	}

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename=%q`, sanitizeMultipartFilename(name)))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(data); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}

// upscaleErrorFromResponse 把 imgsvc 的错误翻译成我方错误码 + 中文文案。
// 服务端只有一种错误格式：{"error":{"code":"...","message":"中文"}}。
func upscaleErrorFromResponse(status int, body []byte) *dkdomain.DesignkitError {
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	code := ""
	message := strings.TrimSpace(string(body))
	if err := json.Unmarshal(body, &payload); err == nil {
		code = strings.TrimSpace(payload.Error.Code)
		if payload.Error.Message != "" {
			message = payload.Error.Message
		}
	}
	upstream := fmt.Sprintf("imgsvc %d %s: %s", status, code, truncateForLog(message))

	switch {
	case code == "upscale_unavailable":
		// 模型没进镜像 / onnxruntime 没装上：部署问题，图本身没毛病。
		return dkdomain.NewError(dkdomain.ErrCodeUpscaleUnavailable).WithUpstream(upstream)
	case code == "file_too_large" || code == "image_too_large" || status == http.StatusRequestEntityTooLarge:
		return dkdomain.NewError(dkdomain.ErrCodeImageTooLarge).
			WithMessage("这张图太大，放不了。放大只支持 2048×2048 像素以内的图。").
			WithUpstream(upstream)
	case code == "unsupported_media_type" || status == http.StatusUnsupportedMediaType:
		return dkdomain.NewError(dkdomain.ErrCodeUnsupportedImageFormat).WithUpstream(upstream)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return dkdomain.NewError(dkdomain.ErrCodeUpscaleFailed).
			WithMessage("图像处理服务鉴权失败，请联系管理员检查配置。").
			WithUpstream(upstream)
	default:
		return dkdomain.NewError(dkdomain.ErrCodeUpscaleFailed).WithUpstream(upstream)
	}
}

// ----------------------------------------------------------------------------
// 队列服务
// ----------------------------------------------------------------------------

// UpscaleStatus 任务的四个状态。字面量是对外契约（接口 JSON 原样返回），定了别改。
type UpscaleStatus string

const (
	// UpscaleStatusQueued 排队中。
	UpscaleStatusQueued UpscaleStatus = "queued"
	// UpscaleStatusRunning 正在放大。
	UpscaleStatusRunning UpscaleStatus = "running"
	// UpscaleStatusDone 放大完成，Result 里是新的商品图。
	UpscaleStatusDone UpscaleStatus = "done"
	// UpscaleStatusFailed 失败，ErrorMessage 里是中文原因。可以重新点一次。
	UpscaleStatusFailed UpscaleStatus = "failed"
)

// UpscaleTask 一次放大任务的状态快照（Enqueue / Status 返回的都是
// 从任务表现读出来的值，不共享内存，调用方随便改）。
type UpscaleTask struct {
	// AssetUID 被放大的那张商品图。任务按它去重。
	AssetUID string
	// UserID 谁点的。别人查不到这条任务（按「找不到」处理，不泄露存在性）。
	UserID int64
	// Origin web / erp，入库结果记在这个来路上。
	Origin dkdomain.Origin
	// Status 当前状态。
	Status UpscaleStatus
	// Result done 时的产物：一条新的商品图（sha256 去重，重复放大拿到同一条）。
	Result *dkdomain.Asset
	// ErrorMessage failed 时给运营看的中文。
	ErrorMessage string
	// ErrorCode failed 时的我方错误码，方便截图报障。
	ErrorCode string
	// CreatedAt / UpdatedAt 入队时间和最近一次状态变化时间。
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UpscaleStore 是本服务对任务表的全部要求。repository.UpscaleRepo 实现了它。
//
// 状态流转方法全部带守卫（MarkRunning 只认 queued、MarkDone/MarkFailed 只认
// running），守卫没命中返回 ErrConflict——这是「同一个任务被处理两遍」的止损点。
type UpscaleStore interface {
	// Insert 插一行新任务（status=queued），返回带时间戳的完整行。
	Insert(ctx context.Context, rec *dkdomain.UpscaleTaskRecord) (*dkdomain.UpscaleTaskRecord, error)
	// LatestByAsset 这张图最新的一行，归属过滤在 SQL 里做；没有一律 ErrNotFound。
	LatestByAsset(ctx context.Context, userID int64, assetUID string) (*dkdomain.UpscaleTaskRecord, error)
	// GetByUID 按任务编号取一行。
	GetByUID(ctx context.Context, uid string) (*dkdomain.UpscaleTaskRecord, error)
	// MarkRunning queued → running。
	MarkRunning(ctx context.Context, uid string) error
	// MarkDone running → done，记下产物商品图。
	MarkDone(ctx context.Context, uid, resultAssetUID string) error
	// MarkFailed running → failed，记下错误码和中文原因。
	MarkFailed(ctx context.Context, uid, errorCode, errorMessage string) error
	// RequeueInterrupted running → queued（重启恢复第一步），返回重置了几行。
	RequeueInterrupted(ctx context.Context) (int64, error)
	// ListQueued 还排着队的任务，按入队顺序（恢复第二步）。
	ListQueued(ctx context.Context, limit int) ([]*dkdomain.UpscaleTaskRecord, error)
}

// upscaleDBTimeout 单次任务表读写的超时。放大本体另有整段超时（timeout），
// 这里只管状态行的那几条小 SQL。
const upscaleDBTimeout = 10 * time.Second

// upscaleRecoverLimit 重启恢复一次最多捞多少条排队任务。
// 队列封顶 10 + 在放的 1，正常永远到不了这个数；到了说明有人手工改库。
const upscaleRecoverLimit = 100

// UpscaleServiceDeps 建 UpscaleService 要的依赖。
type UpscaleServiceDeps struct {
	// Assets 商品图服务，必填：读原图（校验归属）、放大结果入库都靠它。
	Assets *AssetService
	// Backend 放大后端，必填。
	Backend UpscaleBackend
	// Repo 任务表，必填（9004 起任务落库，重启不丢）。
	Repo UpscaleStore
	// QueueCap 队列封顶。<=0 用默认 10。
	QueueCap int
	// Timeout 单张放大的处理超时（读图 + 推理 + 入库整段）。<=0 用 5 分钟。
	Timeout time.Duration
	// NewUID 生成 26 位 ULID（任务编号）。nil 用内置实现。
	// （原来这里还有个 Now——任务时间戳改由数据库 NOW() 写，字段删了。）
	NewUID func() string
}

// UpscaleService 「高清放大」的排队与状态。
//
// 状态机（单向；failed 之后重新入队 = 插一行新任务）：
//
//	queued → running → done
//	                 ↘ failed →（运营再点一次）→ 新的一行 queued
type UpscaleService struct {
	assets  *AssetService
	backend UpscaleBackend
	repo    UpscaleStore
	timeout time.Duration
	newUID  func() string

	// mu 串行化入队：查最新一行 → 判满 → 插行 → 发信道 这一段必须原子，
	// 否则两次双击会给同一张图排两个任务。**所有往 queue 发的地方都持有 mu**，
	// 所以「len(queue) < cap 之后再发」不会阻塞。
	mu sync.Mutex

	// queue 队列信道，装的是任务编号（uid）。只是信道不是真相：
	// 信号丢了（比如插行成功后进程被杀）重启恢复会把任务捞回来。
	queue    chan string
	queueCap int

	// baseCtx 后台 worker 的生命周期；Close() 时 cancel，在放的那张立刻中断。
	baseCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewUpscaleService 建服务并启动后台 worker（worker 起手先做重启恢复）。
// 缺依赖直接报错。
func NewUpscaleService(deps UpscaleServiceDeps) (*UpscaleService, error) {
	if deps.Assets == nil {
		return nil, errors.New("designkit: UpscaleService 缺少商品图服务")
	}
	if deps.Backend == nil {
		return nil, errors.New("designkit: UpscaleService 缺少放大后端")
	}
	if deps.Repo == nil {
		return nil, errors.New("designkit: UpscaleService 缺少任务表（9004 起任务落库）")
	}
	queueCap := deps.QueueCap
	if queueCap <= 0 {
		queueCap = defaultUpscaleQueueCap
	}
	timeout := deps.Timeout
	if timeout <= 0 {
		timeout = DefaultUpscaleTimeout
	}
	newUID := deps.NewUID
	if newUID == nil {
		newUID = newAssetULID
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &UpscaleService{
		assets:   deps.Assets,
		backend:  deps.Backend,
		repo:     deps.Repo,
		timeout:  timeout,
		newUID:   newUID,
		queue:    make(chan string, queueCap),
		queueCap: queueCap,
		baseCtx:  ctx,
		cancel:   cancel,
	}
	s.wg.Add(1)
	go s.run()
	return s, nil
}

// Close 停掉后台 worker。**不等在放的那张放完**——上游 main.go 只给 5 秒
// 就让进程消失，而一张要一两分钟；cancel 之后推理请求立刻中断。
// 被打断的那张**刻意留在 running 不写失败**：重启恢复会把它重置回 queued
// 接着放，运营什么都不用做（见 process 里的关停分支）。
func (s *UpscaleService) Close() {
	if s == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
}

// Enqueue 把一张商品图排进放大队列（任务落库，重启不丢）。
//
// 去重规则（按「这张图最新的一行」判）：
//   - queued / running → 不重复入队，返回现有任务；
//   - done 且产物还在 → 直接返回结果；产物被删了 → 当没放过，重新排；
//   - failed / 没排过 → 插一行新任务入队（「放大失败，重试一次」点的就是这条路）。
//
// 队列满返回 DK_UPSCALE_QUEUE_FULL（「排队满了，等会儿再试。」）。
func (s *UpscaleService) Enqueue(ctx context.Context, userID int64, origin dkdomain.Origin, assetUID string) (*UpscaleTask, error) {
	if userID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUnauthorized)
	}
	assetUID = strings.TrimSpace(assetUID)
	if assetUID == "" {
		return nil, dkdomain.NewError(dkdomain.ErrCodeAssetNotFound)
	}
	if origin == "" {
		origin = dkdomain.OriginWeb
	}
	if !origin.IsValid() {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest)
	}

	// 先校验归属：不是他的图（或压根不存在）→「找不到」，不进队列。
	if _, err := s.assets.GetAsset(ctx, userID, assetUID); err != nil {
		return nil, err
	}

	// 从这里到发信道必须原子（见 mu 的注释），否则双击 = 同一张图两个任务。
	s.mu.Lock()
	defer s.mu.Unlock()

	latest, err := s.repo.LatestByAsset(ctx, userID, assetUID)
	if err != nil && !errors.Is(err, dkdomain.ErrNotFound) {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
	if latest != nil {
		switch UpscaleStatus(latest.Status) {
		case UpscaleStatusQueued, UpscaleStatusRunning:
			// 已经在排 / 在放，不重复入队。
			return s.taskFromRecord(ctx, latest), nil
		case UpscaleStatusDone:
			task := s.taskFromRecord(ctx, latest)
			if task.Result != nil {
				return task, nil
			}
			// done 但产物找不到了（被删/换过库）：当没放过，往下走重新排。
		case UpscaleStatusFailed:
			// 往下走：插一行新任务重试，failed 的旧行留作历史。
		}
	}

	// 判满在插行**之前**：满了就什么都不建、不留任务残骸，
	// 失败的旧行也原样留着，运营等会儿还能重试。
	// len 和发送都在 mu 里，这个判断是准的。
	if len(s.queue) >= s.queueCap {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpscaleQueueFull)
	}

	created, err := s.repo.Insert(ctx, &dkdomain.UpscaleTaskRecord{
		UID:      s.newUID(),
		AssetUID: assetUID,
		UserID:   userID,
		Origin:   origin,
	})
	if err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
	// 刚判过不满、发送方都持有 mu，这里必然发得进去。
	s.queue <- created.UID
	return s.taskFromRecord(ctx, created), nil
}

// Status 查一张商品图的放大任务（最新的一行，任务表为准，重启不丢）。
// 没有（或不是他的）→ DK_UPSCALE_NOT_FOUND，不要 403——403 等于告诉对方编号存在。
func (s *UpscaleService) Status(ctx context.Context, userID int64, assetUID string) (*UpscaleTask, error) {
	if userID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUnauthorized)
	}
	rec, err := s.repo.LatestByAsset(ctx, userID, strings.TrimSpace(assetUID))
	if err != nil {
		if errors.Is(err, dkdomain.ErrNotFound) {
			return nil, dkdomain.NewError(dkdomain.ErrCodeUpscaleNotFound)
		}
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
	return s.taskFromRecord(ctx, rec), nil
}

// taskFromRecord 把任务行翻成对外快照；done 的顺手把产物商品图读出来。
//
// 产物读不到（被删/换过库）**不报错**：任务本身的状态还是真的。
// Result 缺席时 Status 原样返回并留一行日志，Enqueue 会把它当「没放过」重新排。
func (s *UpscaleService) taskFromRecord(ctx context.Context, rec *dkdomain.UpscaleTaskRecord) *UpscaleTask {
	task := &UpscaleTask{
		AssetUID:     rec.AssetUID,
		UserID:       rec.UserID,
		Origin:       rec.Origin,
		Status:       UpscaleStatus(rec.Status),
		ErrorMessage: rec.ErrorMessage,
		ErrorCode:    rec.ErrorCode,
		CreatedAt:    rec.CreatedAt,
		UpdatedAt:    rec.UpdatedAt,
	}
	if task.Status == UpscaleStatusDone && rec.ResultAssetUID != nil {
		result, err := s.assets.GetAsset(ctx, rec.UserID, *rec.ResultAssetUID)
		if err != nil {
			slog.Warn("designkit 放大任务的产物商品图读不到了",
				slog.String("task_uid", rec.UID),
				slog.String("result_asset_uid", *rec.ResultAssetUID),
				slog.Any("error", err))
		} else {
			task.Result = result
		}
	}
	return task
}

// run 后台 worker：起手先做重启恢复，然后一次一张，串行。
func (s *UpscaleService) run() {
	defer s.wg.Done()
	s.recoverPending()
	for {
		select {
		case <-s.baseCtx.Done():
			return
		case uid := <-s.queue:
			s.process(uid)
		}
	}
}

// recoverPending 重启恢复：running 的重置回 queued（上次进程死在半路），
// 然后把还排着队的按入队顺序直接处理掉。
//
// **不经过队列信道**：恢复的任务在信道里没有名额也不占名额（Enqueue 的判满
// 只管新请求）。恢复期间用户新排的任务照常走信道，等恢复的处理完才轮到——
// 这正是「先来先放」。两边撞上同一个任务（恢复扫到的同时用户重试成功）也不怕：
// process 只认 queued + MarkRunning 带守卫，后到的一方空转返回。
func (s *UpscaleService) recoverPending() {
	ctx, cancel := context.WithTimeout(s.baseCtx, upscaleDBTimeout)
	requeued, err := s.repo.RequeueInterrupted(ctx)
	cancel()
	if err != nil {
		// 恢复失败不拦启动：新任务照常收，旧任务等下次重启再捞。
		slog.Error("designkit 高清放大重启恢复失败（running 重置 queued），旧任务暂时不会续跑",
			slog.Any("error", err))
		return
	}
	if requeued > 0 {
		slog.Info("designkit 高清放大：上次进程死在半路的任务已重新排队",
			slog.Int64("count", requeued))
	}

	ctx, cancel = context.WithTimeout(s.baseCtx, upscaleDBTimeout)
	pending, err := s.repo.ListQueued(ctx, upscaleRecoverLimit)
	cancel()
	if err != nil {
		slog.Error("designkit 高清放大重启恢复失败（读排队任务），旧任务暂时不会续跑",
			slog.Any("error", err))
		return
	}
	for _, rec := range pending {
		if s.baseCtx.Err() != nil {
			return
		}
		s.process(rec.UID)
	}
}

// process 放一张（入参是任务编号）。任何失败都把任务行置 failed + 中文原因，
// 绝不静默——唯一的例外是关停：被 Close 打断的那张**留在 running**，
// 下次启动 recoverPending 会把它重置回 queued 接着放，运营什么都不用做。
func (s *UpscaleService) process(uid string) {
	readCtx, cancelRead := context.WithTimeout(s.baseCtx, upscaleDBTimeout)
	task, err := s.repo.GetByUID(readCtx, uid)
	cancelRead()
	if err != nil {
		// 找不到 = 信道里的陈旧信号（行被手工清了），跳过即可；别的错误要留痕。
		if !errors.Is(err, dkdomain.ErrNotFound) {
			slog.Error("designkit 高清放大读任务失败，这一张先跳过（重启恢复会再捞）",
				slog.String("task_uid", uid), slog.Any("error", err))
		}
		return
	}
	if task.Status != string(UpscaleStatusQueued) {
		// 已经被处理过（恢复循环和信道信号撞上同一个任务）。不是错误。
		return
	}

	claimCtx, cancelClaim := context.WithTimeout(s.baseCtx, upscaleDBTimeout)
	err = s.repo.MarkRunning(claimCtx, uid)
	cancelClaim()
	if err != nil {
		if !errors.Is(err, dkdomain.ErrConflict) {
			slog.Error("designkit 高清放大领任务失败，这一张先跳过（重启恢复会再捞）",
				slog.String("task_uid", uid), slog.Any("error", err))
		}
		return
	}

	ctx, cancel := context.WithTimeout(s.baseCtx, s.timeout)
	defer cancel()
	result, execErr := s.execute(ctx, task.UserID, task.Origin, task.AssetUID)

	// 关停打断：不写失败，留在 running 让下次启动续跑（见函数头注释）。
	if s.baseCtx.Err() != nil {
		return
	}

	// 写回必须用干净的 context：execute 那个可能已经超时了，
	// 拿它写库会把「放大超时」变成「超时且状态没落库」。
	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(s.baseCtx), upscaleDBTimeout)
	defer cancelWrite()

	if execErr != nil {
		message, code := upscaleFailureText(execErr)
		if markErr := s.repo.MarkFailed(writeCtx, uid, code, message); markErr != nil {
			slog.Error("designkit 高清放大失败且状态写不进任务表（任务会留在 running，重启恢复会重放这一张）",
				slog.String("task_uid", uid), slog.Any("mark_error", markErr))
		}
		slog.Warn("designkit 高清放大失败",
			slog.String("task_uid", uid),
			slog.String("asset_uid", task.AssetUID),
			slog.Int64("user_id", task.UserID),
			slog.String("error_code", code),
			slog.Any("error", execErr))
		return
	}

	if markErr := s.repo.MarkDone(writeCtx, uid, result.UID); markErr != nil {
		// 图已入库但任务行没写上 done：留在 running，重启恢复会重放这一张，
		// 重放的产物走 sha256 去重（UploadAsset），不会重复占盘。
		slog.Error("designkit 高清放大完成但状态写不进任务表（重启恢复会重放，产物按 sha256 去重）",
			slog.String("task_uid", uid), slog.Any("mark_error", markErr))
		return
	}
	slog.Info("designkit 高清放大完成",
		slog.String("task_uid", uid),
		slog.String("asset_uid", task.AssetUID),
		slog.String("result_uid", result.UID),
		slog.Int64("user_id", task.UserID),
		slog.Int64("bytes", result.ByteSize))
}

// execute 读原图 → 调 imgsvc → 结果入库（sha256 去重）。
func (s *UpscaleService) execute(ctx context.Context, userID int64, origin dkdomain.Origin, uid string) (*dkdomain.Asset, error) {
	data, contentType, err := s.assets.AssetContent(ctx, userID, uid)
	if err != nil {
		return nil, err
	}

	out, err := s.backend.Upscale(ctx, data, uid+dkExtForContentType(contentType), contentType)
	if err != nil {
		return nil, err
	}

	// 入库走跟「用这张继续生成」同一条路（UploadAsset）：按 sha256 去重、
	// 探真实格式、落对象存储。文件名带 -x4 标记，日志里一眼认出是放大产物。
	//
	// MaxBytesOverride：放大产物天然大于运营上传的 20MB 上限
	//（×4 之后一张照片级 PNG 就是几十 MB），用放大自己的上限。
	result, err := s.assets.UploadAsset(ctx, UploadAssetInput{
		UserID:            userID,
		Origin:            origin,
		Filename:          "upscale-" + uid + "-x4.png",
		ClientContentType: "image/png",
		DeclaredSize:      int64(len(out)),
		Data:              bytes.NewReader(out),
		MaxBytesOverride:  maxUpscaleAssetBytes,
	})
	if err != nil {
		return nil, err
	}
	if result == nil || result.Asset == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal)
	}
	return result.Asset, nil
}

// upscaleFailureText 把失败翻成（中文文案, 错误码）。
// 超时单独说清楚——「过 5 分钟还没好」跟「模型坏了」是两回事。
func upscaleFailureText(err error) (message, code string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return "放大超时了，这张图可能太大。重试一次，一直这样请联系管理员。", dkdomain.ErrCodeTimeout
	}
	if dkErr, ok := dkdomain.AsDesignkitError(err); ok {
		return dkErr.Message, dkErr.Code
	}
	return dkdomain.NewError(dkdomain.ErrCodeUpscaleFailed).Message, dkdomain.ErrCodeUpscaleFailed
}
