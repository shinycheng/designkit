package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ============================================================================
// Python 预处理服务（designkit-imgsvc）的客户端
// ============================================================================
//
// 契约见 designkit/docs/设计定型.md 第七节，服务端代码在 designkit/imgsvc/。
//
// 【为什么这一步不能省】
// 自建网关**忽略 size 参数，输出比例完全跟随输入图比例**。想让模型出 3:4，
// 唯一手段就是先把输入图补白边补成 3:4。所以预处理在出图主链路上，
// 它失败这一张就不能出。
//
// 【fail-closed，绝不回吐原图】
// 老代码（legacy-python 分支的 prepare_input_image）是 fail-open：
// 大 try 兜住，异常就 return 原始字节。搬过来就是——补边失败 →
// 原图什么比例出什么比例 → **运营选 3:4 拿回 4:3，钱照扣**，只有一行 warning，
// 没人会发现。这里任何一步失败都返回 error，调用方把该 item 置 failed。
//
// 【max_dimension 每次都必须显式带上】
// 唯一真相是 designkit_settings.max_dimension。Python 侧的
// DESIGNKIT_IMGSVC_DEFAULT_MAX_DIMENSION 只是「Go 忘了传」的兜底。
// 两边漂了的后果是「界面说 4K、实际按 2K 出图」，**而且不报错**
// （最长边 ≤1024→1K、≤2048→2K、更大→4K，这个数直接决定计费档）。
// 所以下面对 MaxDimension <= 0 是**直接拒绝**，不是取个默认值糊过去。

const (
	// EnvImgsvcURL 预处理服务地址，例如 http://designkit-imgsvc:8000。
	EnvImgsvcURL = "DESIGNKIT_IMGSVC_URL"
	// EnvImgsvcToken 预处理服务的 Bearer Token，留空表示服务端没开鉴权。
	EnvImgsvcToken = "DESIGNKIT_IMGSVC_TOKEN"
	// EnvImgsvcTimeoutSeconds 单次调用超时秒数。
	EnvImgsvcTimeoutSeconds = "DESIGNKIT_IMGSVC_TIMEOUT_SECONDS"

	// DefaultImgsvcURL compose 里的服务名（容器内 8000，不映射宿主端口）。
	DefaultImgsvcURL = "http://designkit-imgsvc:8000"
	// DefaultImgsvcTimeout 单次调用默认超时。
	// 一张 20MB 的手机原图在群晖 arm64 上的耗时还没实测（设计定型 第十节），
	// 先给宽一点，宁可等也不要把能成的图判成失败。
	DefaultImgsvcTimeout = 60 * time.Second

	// preprocessPath / healthPath 服务端路径。
	preprocessPath = "/v1/preprocess"
	healthPath     = "/healthz"

	// defaultPreprocessAttempts 总尝试次数 = 1 次 + 2 次重试。
	// **只对连接失败和 502/503/504 重试，4xx 不重试**（4xx 重试一万次也还是 4xx）。
	defaultPreprocessAttempts = 3
	// defaultPreprocessBackoff 第一次重试前等多久，之后翻倍。
	defaultPreprocessBackoff = 300 * time.Millisecond

	// maxPreprocessResponseBytes 响应体上限，防止服务端异常时把内存撑爆。
	// 一张 4K PNG 大约 20~30MB，64MB 足够。
	maxPreprocessResponseBytes = 64 << 20

	// maxUpstreamMessageLen 记进 last_error_message 的上游原文长度上限。
	maxUpstreamMessageLen = 512
)

// PreprocessClientConfig 预处理客户端的配置。
type PreprocessClientConfig struct {
	// BaseURL 服务地址，可带路径前缀；末尾的 / 会被去掉。
	BaseURL string
	// Token 留空表示不带 Authorization 头。
	Token string
	// Timeout 单次 HTTP 调用超时。<=0 用 DefaultImgsvcTimeout。
	Timeout time.Duration
	// HTTPClient 传了就用它（测试用）。为 nil 时自己建一个。
	HTTPClient *http.Client
	// MaxAttempts 总尝试次数。<=0 用 defaultPreprocessAttempts。
	MaxAttempts int
	// RetryBackoff 第一次重试前的等待时长，之后翻倍。<=0 用 defaultPreprocessBackoff。
	RetryBackoff time.Duration
}

// PreprocessClient 是 domain.ImagePreprocessor 的唯一实现。
type PreprocessClient struct {
	baseURL      string
	token        string
	httpClient   *http.Client
	maxAttempts  int
	retryBackoff time.Duration
}

// 编译期断言：接口对齐。
var _ dkdomain.ImagePreprocessor = (*PreprocessClient)(nil)

// NewPreprocessClientFromEnv 从环境变量建客户端。
//
// **刻意只走 os.Getenv，不碰 config.Config** —— 那个结构体在上游文件里，
// 不在允许触碰的 5 个文件白名单内（CLAUDE.md 第一节）。
func NewPreprocessClientFromEnv() (*PreprocessClient, error) {
	base := strings.TrimSpace(os.Getenv(EnvImgsvcURL))
	if base == "" {
		base = DefaultImgsvcURL
	}
	timeout := DefaultImgsvcTimeout
	if raw := strings.TrimSpace(os.Getenv(EnvImgsvcTimeoutSeconds)); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds <= 0 {
			// 配错了不让它静默退化：这个值决定「多久算超时」，
			// 悄悄用默认值会让运维以为自己改生效了。
			return nil, fmt.Errorf("designkit: %s 必须是正整数秒，实际 %q", EnvImgsvcTimeoutSeconds, raw)
		}
		timeout = time.Duration(seconds) * time.Second
	}
	return NewPreprocessClient(PreprocessClientConfig{
		BaseURL: base,
		Token:   strings.TrimSpace(os.Getenv(EnvImgsvcToken)),
		Timeout: timeout,
	})
}

// NewPreprocessClient 建客户端。BaseURL 不合法直接报错，不延迟到第一次调用。
func NewPreprocessClient(cfg PreprocessClientConfig) (*PreprocessClient, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("designkit: 预处理服务地址为空（检查 %s）", EnvImgsvcURL)
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("designkit: 预处理服务地址不合法：%q（应形如 %s）", cfg.BaseURL, DefaultImgsvcURL)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultImgsvcTimeout
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: timeout,
			// Proxy 显式设成 nil。
			// 设计定型 第七节：dev compose 的 NO_PROXY 名单里只有
			// postgres,redis,sub2api，网段规则对不上 designkit-imgsvc 这个主机名。
			// 一旦环境里设了 HTTP_PROXY，默认的 ProxyFromEnvironment 会把
			// 容器间的内部调用发到代理上去，表现是「预处理莫名其妙全超时」。
			// 自己带一个 Proxy=nil 的 Transport，就不依赖任何人去改 NO_PROXY。
			Transport: &http.Transport{
				Proxy: nil,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:          16,
				MaxIdleConnsPerHost:   8,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		}
	}

	attempts := cfg.MaxAttempts
	if attempts <= 0 {
		attempts = defaultPreprocessAttempts
	}
	backoff := cfg.RetryBackoff
	if backoff <= 0 {
		backoff = defaultPreprocessBackoff
	}

	return &PreprocessClient{
		baseURL:      base,
		token:        strings.TrimSpace(cfg.Token),
		httpClient:   client,
		maxAttempts:  attempts,
		retryBackoff: backoff,
	}, nil
}

// Preprocess 调 POST /v1/preprocess。
//
// 成功时响应体**就是图片字节**，元数据在响应头
// X-Dk-Changed / X-Dk-Width / X-Dk-Height / X-Dk-Actions。
// 任何失败都返回 *domain.DesignkitError，**绝不返回原图**。
func (c *PreprocessClient) Preprocess(ctx context.Context, req dkdomain.PreprocessRequest) (*dkdomain.PreprocessResult, error) {
	if c == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("图像处理服务没有配置，请联系管理员。")
	}
	if err := validatePreprocessRequest(req); err != nil {
		return nil, err
	}

	body, contentType, err := buildPreprocessBody(req)
	if err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).WithCause(err)
	}

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if attempt > 1 {
			// 退避期间也要响应取消，否则运营点了停止还要干等。
			wait := c.retryBackoff << (attempt - 2)
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).WithCause(ctx.Err())
			case <-timer.C:
			}
		}

		result, retryable, err := c.doPreprocess(ctx, body, contentType)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !retryable || ctx.Err() != nil {
			return nil, err
		}
		slog.Warn("designkit 预处理失败，准备重试",
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", c.maxAttempts),
			slog.String("ratio", req.Ratio.String()),
			slog.Int("max_dimension", req.MaxDimension),
			slog.Any("error", err),
		)
	}
	return nil, lastErr
}

// doPreprocess 发一次请求。第二个返回值表示「这次失败能不能重试」。
func (c *PreprocessClient) doPreprocess(ctx context.Context, body []byte, contentType string) (*dkdomain.PreprocessResult, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+preprocessPath, bytes.NewReader(body))
	if err != nil {
		return nil, false, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).WithCause(err)
	}
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.ContentLength = int64(len(body))
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// 连接失败 / 超时：可以重试（服务刚好在重启是最常见的原因）。
		return nil, true, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("连不上图像处理服务，稍后会自动重试；一直这样请联系管理员。").
			WithUpstream(err.Error()).
			WithCause(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamMessageLen*4))
		dkErr := preprocessErrorFromResponse(resp.StatusCode, raw)
		// 只有 502/503/504 值得重试；4xx 是我们自己或图片本身的问题，重试没意义。
		retryable := resp.StatusCode == http.StatusBadGateway ||
			resp.StatusCode == http.StatusServiceUnavailable ||
			resp.StatusCode == http.StatusGatewayTimeout
		return nil, retryable, dkErr
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPreprocessResponseBytes+1))
	if err != nil {
		return nil, true, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).WithCause(err)
	}
	if len(data) == 0 {
		return nil, true, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("图像处理服务返回了空图片，这张图没有被发出去。")
	}
	if len(data) > maxPreprocessResponseBytes {
		return nil, false, dkdomain.NewError(dkdomain.ErrCodeImageTooLarge).
			WithMessage("处理后的图片太大了，请换一张小一点的商品图。")
	}

	result, err := parsePreprocessResponse(resp.Header, data)
	if err != nil {
		return nil, false, err
	}
	return result, false, nil
}

// Health 探活，对应 GET /healthz。
//
// 服务端**始终返 200**，能不能干活看 body 里的 status 字段——
// 没装 HEIC 解码器时它仍能处理 JPG/PNG，不该被判成不健康然后被反复重启。
// 所以这里只把「装没装 HEIC」记一条日志，不当失败。
func (c *PreprocessClient) Health(ctx context.Context) error {
	if c == nil {
		return errors.New("designkit: 预处理客户端未初始化")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+healthPath, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("designkit: 连不上图像处理服务：%w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("designkit: 图像处理服务健康检查返回 %d：%s", resp.StatusCode, truncateForLog(string(raw)))
	}

	var payload struct {
		Status        string `json:"status"`
		HeifSupported bool   `json:"heif_supported"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && !payload.HeifSupported {
		slog.Warn("designkit 图像处理服务没装 HEIC 解码器，iPhone 拍的 HEIC 照片会被拒绝",
			slog.String("status", payload.Status))
	}
	return nil
}

// validatePreprocessRequest 发请求之前的自检。
//
// 三条都是「宁可这张不出，也不能出错比例」：
//   - 没有图片字节 → 发过去也是 400；
//   - ratio 格式不对或为空 → Python 侧把空串和 auto 当成「明确要求不补边」，
//     发过去会**静默不补边**，运营选 3:4 拿回原比例；
//   - max_dimension <= 0 → 不传 Python 就用自己的环境变量兜底，
//     于是「界面说 4K、实际按 2K 出图」，而且不报错。
func validatePreprocessRequest(req dkdomain.PreprocessRequest) error {
	if len(req.Data) == 0 {
		return dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("要处理的图片是空的，请重新上传。")
	}
	if strings.TrimSpace(req.Ratio.String()) == "" || !req.Ratio.IsWellFormed() {
		return dkdomain.NewError(dkdomain.ErrCodeRatioNotAllowed).
			WithMessagef("出图比例 %q 不合法，请从列表里选一个。", req.Ratio.String())
	}
	if req.MaxDimension <= 0 {
		return dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("图片尺寸上限没有配置，无法处理，请联系管理员检查设置。")
	}
	return nil
}

// buildPreprocessBody 拼 multipart 表单。
//
// 字段名跟 Python 侧一一对应：file / ratio / keep_transparency / max_dimension。
// 整个 body 一次性拼好，重试时复用同一份字节（http.Request 的 Body 只能读一次，
// 每次重试都要新建一个 Reader）。
func buildPreprocessBody(req dkdomain.PreprocessRequest) ([]byte, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	filename := strings.TrimSpace(req.Filename)
	if filename == "" {
		filename = "input"
	}
	contentType := strings.TrimSpace(req.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename=%q`, sanitizeMultipartFilename(filename)))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(req.Data); err != nil {
		return nil, "", err
	}

	fields := [][2]string{
		{"ratio", req.Ratio.String()},
		{"keep_transparency", strconv.FormatBool(req.KeepTransparency)},
		// ⚠ 每次都显式带上，绝不省略。理由见文件头。
		{"max_dimension", strconv.Itoa(req.MaxDimension)},
	}
	for _, f := range fields {
		if err := writer.WriteField(f[0], f[1]); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}

// sanitizeMultipartFilename 去掉文件名里的引号、换行和路径分隔符。
// 运营上传的文件名可能是任何东西，直接拼进头里会把 multipart 结构撑坏。
func sanitizeMultipartFilename(name string) string {
	replacer := strings.NewReplacer("\"", "", "\\", "_", "/", "_", "\r", "", "\n", "")
	cleaned := strings.TrimSpace(replacer.Replace(name))
	if cleaned == "" {
		return "input"
	}
	if len(cleaned) > 100 {
		cleaned = cleaned[:100]
	}
	return cleaned
}

// parsePreprocessResponse 把响应头 + 图片字节拼成 PreprocessResult。
func parsePreprocessResponse(header http.Header, data []byte) (*dkdomain.PreprocessResult, error) {
	contentType := strings.TrimSpace(header.Get("Content-Type"))
	if parsed, _, err := mime.ParseMediaType(contentType); err == nil {
		contentType = parsed
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	width := parseHeaderInt(header.Get(dkdomain.PreprocessHeaderWidth))
	height := parseHeaderInt(header.Get(dkdomain.PreprocessHeaderHeight))
	if width <= 0 || height <= 0 {
		// 头缺了不代表图坏了（服务端版本旧、或中间有代理把 X- 头吃了），
		// 自己解一次头部拿尺寸。两条路都拿不到才算失败——
		// 宽高必须有：它决定 variant 落库的尺寸，也是排查计费档的第一手材料。
		if info, ok := dkDetectImageInfo(data); ok && info.Width > 0 && info.Height > 0 {
			width, height = info.Width, info.Height
		} else {
			return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
				WithMessage("图像处理服务返回的图片读不出尺寸，这张图没有被发出去。").
				WithUpstream("missing " + dkdomain.PreprocessHeaderWidth + "/" + dkdomain.PreprocessHeaderHeight)
		}
	}

	return &dkdomain.PreprocessResult{
		Data:        data,
		ContentType: contentType,
		Width:       width,
		Height:      height,
		Changed:     strings.EqualFold(strings.TrimSpace(header.Get(dkdomain.PreprocessHeaderChanged)), "true"),
		Actions:     parsePreprocessActions(header),
	}, nil
}

// parsePreprocessActions 取「做了哪些动作」，只用于排障。
//
// 服务端给了两个头：
//   - X-Dk-Action-Codes：ASCII 机器码，逗号分隔（exif_rotated、padded …）
//   - X-Dk-Actions：中文说明的 JSON 数组，**base64 之后**放进头
//     （HTTP 头按 RFC 只保证 latin-1，中文直接塞会在不同代理上乱码或报错）
//
// 优先用机器码；没有再去解 base64。两个都没有就返回 nil，不当失败。
func parsePreprocessActions(header http.Header) []string {
	if codes := strings.TrimSpace(header.Get("X-Dk-Action-Codes")); codes != "" {
		parts := strings.Split(codes, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		if len(out) > 0 {
			return out
		}
	}

	encoded := strings.TrimSpace(header.Get(dkdomain.PreprocessHeaderActions))
	if encoded == "" {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil
	}
	var notes []string
	if err := json.Unmarshal(decoded, &notes); err != nil {
		return nil
	}
	return notes
}

func parseHeaderInt(raw string) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

// preprocessErrorFromResponse 把 Python 侧的错误翻译成我方错误码 + 中文文案。
//
// 服务端只有一种错误格式：{"error":{"code":"...","message":"中文"}}。
// message 本身就是中文，但**不直接拿来当界面文案**——它面向的是调试，
// 措辞里有「字节」「Pillow」这类词。统一走我们的错误目录，
// 原文只落 last_error_message 给管理员看。
func preprocessErrorFromResponse(status int, body []byte) *dkdomain.DesignkitError {
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
	case code == "heif_unsupported":
		return dkdomain.NewError(dkdomain.ErrCodeUnsupportedImageFormat).
			WithMessage("这张是 iPhone 的 HEIC 照片，但服务器上没装 HEIC 解码器。请在手机上导出成 JPG 再上传，或联系管理员。").
			WithUpstream(upstream)
	case code == "file_too_large" || status == http.StatusRequestEntityTooLarge:
		return dkdomain.NewError(dkdomain.ErrCodeImageTooLarge).WithUpstream(upstream)
	case code == "unsupported_media_type" || status == http.StatusUnsupportedMediaType:
		return dkdomain.NewError(dkdomain.ErrCodeUnsupportedImageFormat).WithUpstream(upstream)
	case code == "invalid_ratio":
		return dkdomain.NewError(dkdomain.ErrCodeRatioNotAllowed).WithUpstream(upstream)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("图像处理服务鉴权失败，请联系管理员检查配置。").
			WithUpstream(upstream)
	case status == http.StatusServiceUnavailable || code == "busy":
		return dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("图像处理服务正忙，稍后会自动重试；一直这样请联系管理员。").
			WithUpstream(upstream)
	case status >= 400 && status < 500:
		// 400 invalid_request / invalid_max_dimension 之类：多半是我们自己传错了参数。
		return dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("商品图处理失败（参数不被图像服务接受），请把这一行的错误码发给管理员。").
			WithUpstream(upstream)
	default:
		return dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).WithUpstream(upstream)
	}
}

// truncateForLog 截断落库/落日志的上游原文。
// last_error_message 是 TEXT 不怕长，但没必要把一整个 HTML 错误页存进去。
func truncateForLog(s string) string {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) <= maxUpstreamMessageLen {
		return trimmed
	}
	return trimmed[:maxUpstreamMessageLen] + "…"
}
