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
//	2. UpscaleService      —— 内存队列 + 单 worker：运营点「高清放大」立刻返回
//	   queued，后台一张一张放；前端每 5 秒查一次状态。
//
// 【为什么是内存队列，不落表】
// 一张图要一两分钟，同步 HTTP 必超时；而为它建表又太重——放大不花钱、
// 可以随手重点。CLAUDE.md 已知约束「只能跑一个后端实例」，内存队列在单实例下
// 就是全局队列。**重启丢任务是接受过的代价**：运营看到失败/没结果就重点一次。
//
// 【为什么单 worker、队列封顶 10】
// imgsvc 那边单块 tile 推理峰值按 1~2GB 估，串行是内存的硬要求；
// 队列封顶是为了别让 100 个排队任务把「等 1~2 分钟」变成「等一下午」——
// 满了直接告诉运营「排队满了，等会儿再试」。
//
// 【去重】
// 同一张商品图已经在排队/在放/放完了，再点一次不入队，直接返回现有任务。
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

// UpscaleTask 一次放大任务的状态快照（Enqueue / Status 返回的都是副本）。
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

// UpscaleServiceDeps 建 UpscaleService 要的依赖。
type UpscaleServiceDeps struct {
	// Assets 商品图服务，必填：读原图（校验归属）、放大结果入库都靠它。
	Assets *AssetService
	// Backend 放大后端，必填。
	Backend UpscaleBackend
	// QueueCap 队列封顶。<=0 用默认 10。
	QueueCap int
	// Timeout 单张放大的处理超时（读图 + 推理 + 入库整段）。<=0 用 5 分钟。
	Timeout time.Duration
	// Now 取当前时间，测试塞固定值。nil 用 time.Now。
	Now func() time.Time
}

// UpscaleService 「高清放大」的排队与状态。
//
// 状态机（单向，除了 failed 可以重新入队）：
//
//	queued → running → done
//	                 ↘ failed →（运营再点一次）→ queued
type UpscaleService struct {
	assets  *AssetService
	backend UpscaleBackend
	timeout time.Duration
	now     func() time.Time

	mu    sync.Mutex
	tasks map[string]*UpscaleTask // key = 商品图 uid

	queue chan string

	// baseCtx 后台 worker 的生命周期；Close() 时 cancel，在放的那张立刻中断。
	baseCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewUpscaleService 建服务并启动后台 worker。缺依赖直接报错。
func NewUpscaleService(deps UpscaleServiceDeps) (*UpscaleService, error) {
	if deps.Assets == nil {
		return nil, errors.New("designkit: UpscaleService 缺少商品图服务")
	}
	if deps.Backend == nil {
		return nil, errors.New("designkit: UpscaleService 缺少放大后端")
	}
	queueCap := deps.QueueCap
	if queueCap <= 0 {
		queueCap = defaultUpscaleQueueCap
	}
	timeout := deps.Timeout
	if timeout <= 0 {
		timeout = DefaultUpscaleTimeout
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &UpscaleService{
		assets:  deps.Assets,
		backend: deps.Backend,
		timeout: timeout,
		now:     now,
		tasks:   make(map[string]*UpscaleTask),
		queue:   make(chan string, queueCap),
		baseCtx: ctx,
		cancel:  cancel,
	}
	s.wg.Add(1)
	go s.run()
	return s, nil
}

// Close 停掉后台 worker。**不等在放的那张放完**——上游 main.go 只给 5 秒
// 就让进程消失，而一张要一两分钟；cancel 之后推理请求立刻中断，
// 那张任务留在 failed/running 也无所谓：重启后任务表本来就是空的（内存队列）。
func (s *UpscaleService) Close() {
	if s == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
}

// Enqueue 把一张商品图排进放大队列。
//
// 去重规则（同一张图）：
//   - queued / running / done → 不重复入队，返回现有任务（done 直接就是结果）；
//   - failed → 重新入队（「放大失败，重试一次」点的就是这条路）。
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

	s.mu.Lock()
	defer s.mu.Unlock()

	// uid 是按人隔离的（GetAsset 刚校验过归属），同一个 uid 不可能属于两个人；
	// 这里再比一次 UserID 纯属防御。
	if existing, ok := s.tasks[assetUID]; ok && existing.UserID == userID {
		if existing.Status != UpscaleStatusFailed {
			return existing.snapshot(), nil
		}
	}

	task := &UpscaleTask{
		AssetUID:  assetUID,
		UserID:    userID,
		Origin:    origin,
		Status:    UpscaleStatusQueued,
		CreatedAt: s.now().UTC(),
		UpdatedAt: s.now().UTC(),
	}

	select {
	case s.queue <- assetUID:
		s.tasks[assetUID] = task
		return task.snapshot(), nil
	default:
		// 队列满。**不覆盖旧任务**：失败的那条留着，运营等会儿还能重试。
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpscaleQueueFull)
	}
}

// Status 查一张商品图的放大任务。没有（或不是他的）→ DK_UPSCALE_NOT_FOUND。
//
// ⚠ 重启后任务表是空的：正在轮询的前端会拿到 404，文案里写了
// 「重新点『高清放大』」，这就是内存队列约定好的代价。
func (s *UpscaleService) Status(_ context.Context, userID int64, assetUID string) (*UpscaleTask, error) {
	if userID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUnauthorized)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[strings.TrimSpace(assetUID)]
	if !ok || task.UserID != userID {
		// 归属不匹配也报「找不到」，不要 403 —— 403 等于告诉对方这个编号存在。
		return nil, dkdomain.NewError(dkdomain.ErrCodeUpscaleNotFound)
	}
	return task.snapshot(), nil
}

// snapshot 返回副本，调用方拿到的状态不会被 worker 改到一半。
// 调用方必须已持有 s.mu。
func (t *UpscaleTask) snapshot() *UpscaleTask {
	clone := *t
	return &clone
}

// run 后台 worker：一次一张，串行。
func (s *UpscaleService) run() {
	defer s.wg.Done()
	for {
		select {
		case <-s.baseCtx.Done():
			return
		case uid := <-s.queue:
			s.process(uid)
		}
	}
}

// process 放一张。任何失败都把任务置 failed + 中文原因，绝不静默。
func (s *UpscaleService) process(uid string) {
	s.mu.Lock()
	task, ok := s.tasks[uid]
	if !ok || task.Status != UpscaleStatusQueued {
		// 只处理还排着队的。（正常流程走不到别的状态；防御分支。）
		s.mu.Unlock()
		return
	}
	task.Status = UpscaleStatusRunning
	task.UpdatedAt = s.now().UTC()
	userID, origin := task.UserID, task.Origin
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(s.baseCtx, s.timeout)
	defer cancel()

	result, err := s.execute(ctx, userID, origin, uid)
	s.mu.Lock()
	defer s.mu.Unlock()
	// 任务行还是同一条（中间没有别的写方），直接改。
	task.UpdatedAt = s.now().UTC()
	if err != nil {
		task.Status = UpscaleStatusFailed
		task.ErrorMessage, task.ErrorCode = upscaleFailureText(err)
		slog.Warn("designkit 高清放大失败",
			slog.String("asset_uid", uid),
			slog.Int64("user_id", userID),
			slog.String("error_code", task.ErrorCode),
			slog.Any("error", err))
		return
	}
	task.Status = UpscaleStatusDone
	task.Result = result
	slog.Info("designkit 高清放大完成",
		slog.String("asset_uid", uid),
		slog.String("result_uid", result.UID),
		slog.Int64("user_id", userID),
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
