//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/require"
)

// testPNG 生成一张真实的小 PNG（w×h 全白）。
// 测试一律用真实图片字节，不用 []byte("fake")——
// 我们的代码会去解它的文件头，假字节测不出问题。
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.White)
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func newTestPreprocessClient(t *testing.T, serverURL string) *PreprocessClient {
	t.Helper()
	client, err := NewPreprocessClient(PreprocessClientConfig{
		BaseURL:      serverURL,
		Token:        "test-token",
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
		MaxAttempts:  3,
		RetryBackoff: time.Millisecond,
	})
	require.NoError(t, err)
	return client
}

func samplePreprocessRequest(t *testing.T) dkdomain.PreprocessRequest {
	t.Helper()
	return dkdomain.PreprocessRequest{
		Data:             testPNG(t, 4, 4),
		Filename:         `我的"商品图".png`,
		ContentType:      "image/png",
		Ratio:            dkdomain.Ratio3x4,
		KeepTransparency: false,
		MaxDimension:     2048,
	}
}

func TestPreprocessClient_Success(t *testing.T) {
	out := testPNG(t, 6, 8)

	// ⚠ 服务端 handler 跑在另一个 goroutine 上，**不要在里面调 require**
	// （require 失败会在那条 goroutine 上 Goexit，测试拿到的是莫名其妙的超时/空响应）。
	// 一律把值捞出来，回到测试 goroutine 再断言。
	var gotMethod, gotPath, gotRatio, gotKeep, gotMaxDim, gotAuth, gotFilename string
	var gotFileBytes []byte
	var gotFileErr error

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotRatio = r.FormValue("ratio")
		gotKeep = r.FormValue("keep_transparency")
		gotMaxDim = r.FormValue("max_dimension")

		file, header, err := r.FormFile("file")
		if err != nil {
			gotFileErr = err
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer func() { _ = file.Close() }()
		gotFilename = header.Filename
		gotFileBytes, gotFileErr = io.ReadAll(file)

		notes, _ := json.Marshal([]string{"补白边到 3:4"})
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set(dkdomain.PreprocessHeaderChanged, "true")
		w.Header().Set(dkdomain.PreprocessHeaderWidth, "6")
		w.Header().Set(dkdomain.PreprocessHeaderHeight, "8")
		w.Header().Set(dkdomain.PreprocessHeaderActions, base64.StdEncoding.EncodeToString(notes))
		w.Header().Set("X-Dk-Action-Codes", "exif_rotated,padded")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}))
	defer server.Close()

	client := newTestPreprocessClient(t, server.URL)
	req := samplePreprocessRequest(t)
	result, err := client.Preprocess(context.Background(), req)
	require.NoError(t, err)
	require.NoError(t, gotFileErr)
	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "/v1/preprocess", gotPath)

	require.Equal(t, out, result.Data)
	require.Equal(t, "image/png", result.ContentType)
	require.Equal(t, 6, result.Width)
	require.Equal(t, 8, result.Height)
	require.True(t, result.Changed)
	require.Equal(t, []string{"exif_rotated", "padded"}, result.Actions)

	// 四个表单字段一个都不能少，尤其是 max_dimension——
	// 它决定落 2K 还是 4K 计费档，漏了就是「界面说 4K、实际按 2K」且不报错。
	require.Equal(t, "3:4", gotRatio)
	require.Equal(t, "false", gotKeep)
	require.Equal(t, "2048", gotMaxDim)
	require.Equal(t, req.Data, gotFileBytes)
	require.Equal(t, "Bearer test-token", gotAuth)
	require.NotContains(t, gotFilename, `"`, "文件名里的引号必须被清掉，否则 multipart 结构会被撑坏")
}

// TestPreprocessClient_MaxDimensionAlwaysSent 单独钉一条：
// 无论调用方传什么，max_dimension 都必须原样出现在表单里。
func TestPreprocessClient_MaxDimensionAlwaysSent(t *testing.T) {
	// PNG 在测试 goroutine 里先做好：handler 里不能调 require（见上）。
	out := testPNG(t, 4, 4)

	for _, want := range []int{1024, 2048, 4096} {
		got := ""
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.FormValue("max_dimension")
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set(dkdomain.PreprocessHeaderWidth, "4")
			w.Header().Set(dkdomain.PreprocessHeaderHeight, "4")
			_, _ = w.Write(out)
		}))

		client := newTestPreprocessClient(t, server.URL)
		req := samplePreprocessRequest(t)
		req.MaxDimension = want
		_, err := client.Preprocess(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, strconv.Itoa(want), got)
		server.Close()
	}
}

// TestPreprocessClient_RefusesWithoutMaxDimension max_dimension 没配就**不发请求**。
// 发过去的话 Python 会用自己的环境变量兜底，两边一漂就是静默的计费档错位。
func TestPreprocessClient_RefusesWithoutMaxDimension(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestPreprocessClient(t, server.URL)
	req := samplePreprocessRequest(t)
	req.MaxDimension = 0

	result, err := client.Preprocess(context.Background(), req)
	require.Nil(t, result)
	require.Error(t, err)
	dkErr, ok := dkdomain.AsDesignkitError(err)
	require.True(t, ok)
	require.Equal(t, dkdomain.ErrCodePreprocessFailed, dkErr.Code)
	require.Zero(t, atomic.LoadInt32(&calls), "参数不全时一次请求都不该发出去")
}

// TestPreprocessClient_RefusesBadRatio 比例护栏的最后一道：
// Python 侧把空串和 auto 当成「明确要求不补边」，发过去会静默不补边。
func TestPreprocessClient_RefusesBadRatio(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer server.Close()

	client := newTestPreprocessClient(t, server.URL)
	for _, ratio := range []dkdomain.Ratio{"", "auto", "1536x1024", "3/4", "abc"} {
		req := samplePreprocessRequest(t)
		req.Ratio = ratio
		_, err := client.Preprocess(context.Background(), req)
		require.Error(t, err, "比例 %q 必须被拒绝", ratio)
		dkErr, ok := dkdomain.AsDesignkitError(err)
		require.True(t, ok)
		require.Equal(t, dkdomain.ErrCodeRatioNotAllowed, dkErr.Code)
	}
	require.Zero(t, atomic.LoadInt32(&calls))
}

// TestPreprocessClient_RetriesOn5xx 只对连接失败和 502/503/504 重试 2 次。
func TestPreprocessClient_RetriesOn5xx(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantCalls  int32
		wantCode   string
		bodyIsJSON bool
	}{
		{name: "500 不重试", status: http.StatusInternalServerError, wantCalls: 1, wantCode: dkdomain.ErrCodePreprocessFailed},
		{name: "502 重试到上限", status: http.StatusBadGateway, wantCalls: 3, wantCode: dkdomain.ErrCodePreprocessFailed},
		{name: "503 重试到上限", status: http.StatusServiceUnavailable, wantCalls: 3, wantCode: dkdomain.ErrCodePreprocessFailed},
		{name: "504 重试到上限", status: http.StatusGatewayTimeout, wantCalls: 3, wantCode: dkdomain.ErrCodePreprocessFailed},
		{name: "400 不重试", status: http.StatusBadRequest, wantCalls: 1, wantCode: dkdomain.ErrCodePreprocessFailed, bodyIsJSON: true},
		{name: "413 不重试", status: http.StatusRequestEntityTooLarge, wantCalls: 1, wantCode: dkdomain.ErrCodeImageTooLarge, bodyIsJSON: true},
		{name: "415 不重试", status: http.StatusUnsupportedMediaType, wantCalls: 1, wantCode: dkdomain.ErrCodeUnsupportedImageFormat, bodyIsJSON: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.bodyIsJSON {
					_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"参数不对"}}`))
				} else {
					_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"服务端处理图片时出错"}}`))
				}
			}))
			defer server.Close()

			client := newTestPreprocessClient(t, server.URL)
			result, err := client.Preprocess(context.Background(), samplePreprocessRequest(t))

			// fail-closed：失败时**绝不能**返回任何图片字节。
			require.Nil(t, result)
			require.Error(t, err)
			dkErr, ok := dkdomain.AsDesignkitError(err)
			require.True(t, ok)
			require.Equal(t, tc.wantCode, dkErr.Code)
			require.NotEmpty(t, dkErr.Upstream, "上游原文要留给管理员看")
			require.NotContains(t, dkErr.Message, "元", "金额和文案里不许出现「元」")
			require.Equal(t, tc.wantCalls, atomic.LoadInt32(&calls))
		})
	}
}

// TestPreprocessClient_RecoversAfter503 前两次 503、第三次成功。
func TestPreprocessClient_RecoversAfter503(t *testing.T) {
	out := testPNG(t, 4, 4)
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"busy","message":"忙"}}`))
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set(dkdomain.PreprocessHeaderWidth, "4")
		w.Header().Set(dkdomain.PreprocessHeaderHeight, "4")
		_, _ = w.Write(out)
	}))
	defer server.Close()

	client := newTestPreprocessClient(t, server.URL)
	result, err := client.Preprocess(context.Background(), samplePreprocessRequest(t))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, int32(3), atomic.LoadInt32(&calls))
}

// TestPreprocessClient_HeifUnsupported 422 heif_unsupported 是部署问题，
// 文案要告诉运营「导出成 JPG 再传」，而不是笼统地说失败。
func TestPreprocessClient_HeifUnsupported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"heif_unsupported","message":"没装 pillow-heif"}}`))
	}))
	defer server.Close()

	client := newTestPreprocessClient(t, server.URL)
	_, err := client.Preprocess(context.Background(), samplePreprocessRequest(t))
	require.Error(t, err)
	dkErr, ok := dkdomain.AsDesignkitError(err)
	require.True(t, ok)
	require.Equal(t, dkdomain.ErrCodeUnsupportedImageFormat, dkErr.Code)
	require.Contains(t, dkErr.Message, "JPG")
}

// TestPreprocessClient_EmptyBodyIsFailure 200 但响应体是空的，也必须当失败。
func TestPreprocessClient_EmptyBodyIsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestPreprocessClient(t, server.URL)
	result, err := client.Preprocess(context.Background(), samplePreprocessRequest(t))
	require.Nil(t, result)
	require.Error(t, err)
}

// TestPreprocessClient_FallsBackToDecodingSize 响应头被中间层吃掉时，
// 自己解一次图拿宽高，而不是直接判失败。
func TestPreprocessClient_FallsBackToDecodingSize(t *testing.T) {
	out := testPNG(t, 12, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(out)
	}))
	defer server.Close()

	client := newTestPreprocessClient(t, server.URL)
	result, err := client.Preprocess(context.Background(), samplePreprocessRequest(t))
	require.NoError(t, err)
	require.Equal(t, 12, result.Width)
	require.Equal(t, 16, result.Height)
}

func TestPreprocessClient_Health(t *testing.T) {
	gotPath := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","heif_supported":true}`))
	}))
	defer server.Close()

	client := newTestPreprocessClient(t, server.URL)
	require.NoError(t, client.Health(context.Background()))
	require.Equal(t, "/healthz", gotPath)
}

func TestNewPreprocessClient_RejectsBadURL(t *testing.T) {
	for _, base := range []string{"", "   ", "designkit-imgsvc:8000", "ftp://x", "/v1"} {
		_, err := NewPreprocessClient(PreprocessClientConfig{BaseURL: base})
		require.Error(t, err, "地址 %q 必须在构造时就被拒绝", base)
	}
}

func TestNewPreprocessClientFromEnv(t *testing.T) {
	t.Setenv(EnvImgsvcURL, "http://designkit-imgsvc:8000/")
	t.Setenv(EnvImgsvcToken, "abc")
	t.Setenv(EnvImgsvcTimeoutSeconds, "5")

	client, err := NewPreprocessClientFromEnv()
	require.NoError(t, err)
	require.Equal(t, "http://designkit-imgsvc:8000", client.baseURL)
	require.Equal(t, "abc", client.token)
	require.Equal(t, 5*time.Second, client.httpClient.Timeout)

	// 配错了不能静默退化成默认值，否则运维以为自己改生效了。
	t.Setenv(EnvImgsvcTimeoutSeconds, "abc")
	_, err = NewPreprocessClientFromEnv()
	require.Error(t, err)
}
