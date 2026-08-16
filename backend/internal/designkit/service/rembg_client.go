package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ============================================================================
// 抠图服务（rembg）的客户端 ——「生成白底图」的第一步
// ============================================================================
//
// 服务端是现成镜像 danielgatis/rembg（compose 里的 designkit-rembg），
// 端点 POST /api/remove：multipart 传一张图，返回抠掉背景的**透明 PNG**。
// 合成白底不在这里做，在 Go 侧（asset_whitebg.go 的 composeOnWhite）。
//
// 写法照 preprocess_client.go：从环境变量构造、错误一律翻译成中文的
// *domain.DesignkitError。跟预处理不同的两点：
//   - **不重试**。抠图是运营在界面上等着的同步动作，失败让它立刻失败，
//     界面提示「没抠出来，重试一次」，比在后台默默重试三轮更省运营的时间；
//   - 模型有**许可证白名单**（见 allowedRembgModels）。
//
// ⚠ 模型权重不在镜像里：第一次用到某个模型时 rembg 才从 GitHub 下载。
// 那一次请求可能超过 60 秒超时而失败，等模型下完（volume 里缓存住）
// 再点一次就是正常速度。这是部署注意事项，不是这里能修的。

const (
	// EnvRembgURL 抠图服务地址，例如 http://designkit-rembg:7000。
	EnvRembgURL = "DESIGNKIT_REMBG_URL"

	// DefaultRembgURL compose 里的服务名（容器内 7000，不映射宿主端口）。
	DefaultRembgURL = "http://designkit-rembg:7000"

	// DefaultRembgTimeout 单次调用超时。isnet 在 CPU 上抠一张几 MB 的图
	// 一般十几秒，60 秒给足余量。
	DefaultRembgTimeout = 60 * time.Second

	// DefaultRembgModel 默认模型。isnet-general-use 是通用场景里效果和
	// 速度折中最好的，而且许可证干净（见 allowedRembgModels）。
	DefaultRembgModel = "isnet-general-use"

	// rembgRemovePath 服务端抠图端点。
	rembgRemovePath = "/api/remove"

	// maxRembgResponseBytes 响应体上限，防止服务端异常时把内存撑爆。
	// 输入最大 20MB，透明 PNG 输出比输入大一截也到不了 64MB。
	maxRembgResponseBytes = 64 << 20
)

// allowedRembgModels 允许使用的 rembg 模型白名单。
//
// ⛔ **bria-rmbg 一系（RMBG-1.4 / RMBG-2.0）刻意不在名单里，别加。**
// 那是 BRIA AI 的**非商用许可**权重（Creative Commons 非商业条款，
// 商用要单独找 BRIA 买授权）。designkit 是卖给别的团队自部署的商业系统
// （CLAUDE.md 决策 33），带上它就是许可证违规——跟登录页那四个
// 「刻意用文字标不用图形标」的平台标识同一个道理。
//
// 名单里这四个都是许可证干净的（u2net 系 Apache-2.0，
// isnet / birefnet-general-lite 是 MIT/Apache 系开源权重）：
var allowedRembgModels = map[string]bool{
	"isnet-general-use":     true,
	"u2net":                 true,
	"u2netp":                true,
	"birefnet-general-lite": true,
}

// allowedRembgModelList 白名单的稳定顺序版本，只用来拼报错文案。
func allowedRembgModelList() string {
	names := make([]string, 0, len(allowedRembgModels))
	for name := range allowedRembgModels {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, " / ")
}

// BackgroundRemover 是「抠掉背景、返回透明 PNG」这一个能力。
//
// AssetService 依赖这个接口而不是具体类型：单测塞假实现，不联网、不起容器。
type BackgroundRemover interface {
	// RemoveBackground 抠掉 data 这张图的背景，返回透明 PNG 字节。
	// filename 只用于 multipart 表单，语义不重要。
	RemoveBackground(ctx context.Context, data []byte, filename string) ([]byte, error)
}

// RembgClientConfig 抠图客户端的配置。
type RembgClientConfig struct {
	// BaseURL 服务地址；末尾的 / 会被去掉。
	BaseURL string
	// Model 用哪个模型。空串 = DefaultRembgModel。
	// **必须在 allowedRembgModels 白名单里**，否则构造直接报错。
	Model string
	// Timeout 单次 HTTP 调用超时。<=0 用 DefaultRembgTimeout。
	Timeout time.Duration
	// HTTPClient 传了就用它（测试用）。为 nil 时自己建一个。
	HTTPClient *http.Client
}

// RembgClient 是 BackgroundRemover 的唯一生产实现。
type RembgClient struct {
	baseURL    string
	model      string
	httpClient *http.Client
}

// 编译期断言：接口对齐。
var _ BackgroundRemover = (*RembgClient)(nil)

// NewRembgClientFromEnv 从环境变量建客户端。
//
// 跟 preprocess_client.go 一样**刻意只走 os.Getenv，不碰 config.Config**。
// 模型不开环境变量：换模型是代码决策（要过许可证白名单），不是运维配置。
func NewRembgClientFromEnv() (*RembgClient, error) {
	base := strings.TrimSpace(os.Getenv(EnvRembgURL))
	if base == "" {
		base = DefaultRembgURL
	}
	return NewRembgClient(RembgClientConfig{BaseURL: base})
}

// NewRembgClient 建客户端。地址不合法、模型不在白名单里都**当场报错**，
// 不延迟到运营点按钮那一刻。
func NewRembgClient(cfg RembgClientConfig) (*RembgClient, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("designkit: 抠图服务地址为空（检查 %s）", EnvRembgURL)
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("designkit: 抠图服务地址不合法：%q（应形如 %s）", cfg.BaseURL, DefaultRembgURL)
	}

	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = DefaultRembgModel
	}
	if !allowedRembgModels[model] {
		// bria 系是最可能被「顺手换上」的，报错里单独点名说清楚为什么不行。
		return nil, fmt.Errorf(
			"designkit: 抠图模型 %q 不在许可证白名单里（可用：%s）。"+
				"bria-rmbg 一系是 BRIA 的非商用权重，商业系统不能用",
			model, allowedRembgModelList())
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultRembgTimeout
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: timeout,
			// Proxy 显式设成 nil，理由同 preprocess_client.go：
			// dev 编排给 sub2api 配了走宿主机的 HTTP_PROXY，容器间的内部调用
			// 一旦被代理接管，表现是「抠图莫名其妙全超时」。
			// （rembg **容器自己**下载模型时要走代理，那是 compose 里
			// designkit-rembg 那段的环境变量，跟这里的 Go→rembg 调用无关。）
			Transport: &http.Transport{
				Proxy: nil,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:          8,
				MaxIdleConnsPerHost:   4,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		}
	}

	return &RembgClient{
		baseURL:    base,
		model:      model,
		httpClient: client,
	}, nil
}

// RemoveBackground 调 POST /api/remove?model=<model>，multipart 字段名 file。
// 成功时响应体**就是透明 PNG 的字节**。任何失败都返回 *domain.DesignkitError。
func (c *RembgClient) RemoveBackground(ctx context.Context, data []byte, filename string) ([]byte, error) {
	if c == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("白底图功能还没准备好，请联系管理员。")
	}
	if len(data) == 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("要抠图的图片是空的，请重新上传。")
	}

	body, contentType, err := buildRembgBody(data, filename, c.model)
	if err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).WithCause(err)
	}

	endpoint := c.baseURL + rembgRemovePath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).WithCause(err)
	}
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.ContentLength = int64(len(body))

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// 连接失败 / 超时。第一次用某个模型时 rembg 还在从 GitHub 下载权重，
		// 超时是常见情况，文案里要给「稍等再试」这条出路。
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("连不上抠图服务或处理超时，稍等一两分钟再点一次；一直这样请联系管理员。").
			WithUpstream(err.Error()).
			WithCause(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamMessageLen*4))
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("没抠出来，重试一次；一直这样请联系管理员。").
			WithUpstream(fmt.Sprintf("rembg %d: %s", resp.StatusCode, truncateForLog(string(raw))))
	}

	out, err := io.ReadAll(io.LimitReader(resp.Body, maxRembgResponseBytes+1))
	if err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).WithCause(err)
	}
	if len(out) == 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("抠图服务返回了空图片，请再试一次。")
	}
	if len(out) > maxRembgResponseBytes {
		return nil, dkdomain.NewError(dkdomain.ErrCodeImageTooLarge).
			WithMessage("抠出来的图片太大了，请换一张小一点的商品图。")
	}
	return out, nil
}

// buildRembgBody 拼 multipart 表单：file（图片字节）+ model（模型名）。
//
// ⚠ model 必须走**表单字段**，不能走 query（2026-08-16 在 NAS 上实测：
// POST /api/remove?model=… 会被忽略、回落到默认的 u2net 并报 500；
// 表单字段 model 才生效）。文件名复用 preprocess_client.go 的清洗。
func buildRembgBody(data []byte, filename, model string) ([]byte, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.WriteField("model", model); err != nil {
		return nil, "", err
	}

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename=%q`, sanitizeMultipartFilename(filename)))
	header.Set("Content-Type", "application/octet-stream")
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
