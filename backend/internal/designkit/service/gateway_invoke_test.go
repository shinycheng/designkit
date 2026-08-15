//go:build unit

package service

// 这些测试全部指向**假上游**：不联网、不碰数据库、不花钱（CLAUDE.md 第三节）。
// 假上游就是下面的 gwFakeImages —— 它冒充 handler.OpenAIGatewayHandler.Images，
// 把收到的合成 gin.Context 原样记下来，再吐一张真实的小 PNG。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"
)

const gwTestJobUID = "01J8ZK7Q9X2M4N6P8R0T2V4W6Y"

func init() {
	// 免得每次 gin.CreateTestContext 都打一遍 debug 横幅。
	gin.SetMode(gin.TestMode)
}

// ---------------------------------------------------------------------------
// 假上游
// ---------------------------------------------------------------------------

// gwCapturedCall 是假上游从合成 gin.Context 里抄下来的一切。
type gwCapturedCall struct {
	clientRequestID string
	requestID       string
	ctxUserID       int64
	method          string
	path            string
	contentType     string
	header          http.Header
	body            []byte
	writer          gin.ResponseWriter
	apiKey          *upstreamservice.APIKey
	subject         middleware.AuthSubject
	role            string
	requestCtxErr   error
}

type gwFakeImages struct {
	mu    sync.Mutex
	calls []gwCapturedCall

	// respond 决定这一次假上游怎么回。nil = 回一张 4x2 的 PNG。
	respond func(c *gin.Context, index int)
}

func (f *gwFakeImages) Images(c *gin.Context) {
	call := gwCapturedCall{
		method:        c.Request.Method,
		path:          c.Request.URL.Path,
		contentType:   c.GetHeader("Content-Type"),
		header:        c.Request.Header.Clone(),
		writer:        c.Writer,
		requestCtxErr: c.Request.Context().Err(),
	}
	call.clientRequestID, _ = c.Request.Context().Value(ctxkey.ClientRequestID).(string)
	call.requestID, _ = c.Request.Context().Value(ctxkey.RequestID).(string)
	call.ctxUserID, _ = c.Request.Context().Value(ctxkey.UserID).(int64)
	call.apiKey, _ = middleware.GetAPIKeyFromContext(c)
	call.subject, _ = middleware.GetAuthSubjectFromContext(c)
	call.role, _ = middleware.GetUserRoleFromContext(c)
	if c.Request.Body != nil {
		call.body, _ = io.ReadAll(c.Request.Body)
	}

	f.mu.Lock()
	index := len(f.calls)
	f.calls = append(f.calls, call)
	respond := f.respond
	f.mu.Unlock()

	if respond == nil {
		gwWriteImagesSuccess(c, gwTinyPNG(4, 2))
		return
	}
	respond(c, index)
}

func (f *gwFakeImages) snapshot() []gwCapturedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]gwCapturedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// gwWriteImagesSuccess 模仿上游成功时的响应体（OpenAI images 格式）。
func gwWriteImagesSuccess(c *gin.Context, imageBytes []byte) {
	payload := map[string]any{
		"created": 1,
		"data": []map[string]any{
			{"b64_json": base64.StdEncoding.EncodeToString(imageBytes)},
		},
	}
	body, _ := json.Marshal(payload)
	c.Data(http.StatusOK, "application/json", body)
}

// gwTinyPNG 造一张真实的小 PNG（不是随便几个字节 —— 我们要靠它验证
// 「从图片字节读出实际像素」这条路，那是计费落哪档的依据）。
func gwTinyPNG(width, height int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 10), G: uint8(y * 10), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// 假 API Key 仓库
// ---------------------------------------------------------------------------

type gwFakeKeyLoader struct {
	key *upstreamservice.APIKey
	err error

	mu    sync.Mutex
	calls int
}

func (f *gwFakeKeyLoader) GetByID(_ context.Context, id int64) (*upstreamservice.APIKey, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.key == nil || f.key.ID != id {
		return nil, upstreamservice.ErrAPIKeyNotFound
	}
	return f.key, nil
}

func gwTestAPIKey(userID, keyID int64) *upstreamservice.APIKey {
	groupID := int64(7)
	return &upstreamservice.APIKey{
		ID:      keyID,
		UserID:  userID,
		Name:    InternalAPIKeyName,
		GroupID: &groupID,
		Status:  upstreamservice.StatusActive,
		User: &upstreamservice.User{
			ID:          userID,
			Role:        "user",
			Concurrency: 5,
			Status:      upstreamservice.StatusActive,
		},
		Group: &upstreamservice.Group{
			ID:                   groupID,
			Name:                 "designkit",
			Platform:             "openai",
			Status:               upstreamservice.StatusActive,
			Hydrated:             true,
			AllowImageGeneration: true,
		},
	}
}

func gwTestRequest(seq, attempt int, prompt string) dkdomain.GenerateRequest {
	return dkdomain.GenerateRequest{
		UserID:           42,
		APIKeyID:         9,
		JobUID:           gwTestJobUID,
		Seq:              seq,
		Attempt:          attempt,
		Model:            dkdomain.DefaultModel,
		Prompt:           prompt,
		Size:             "1024x1024",
		Ratio:            dkdomain.Ratio1x1,
		ImageData:        gwTinyPNG(8, 8),
		ImageFilename:    "product.png",
		ImageContentType: "image/png",
		BillingRequestID: dkdomain.BillingRequestID(gwTestJobUID, seq, attempt),
		Timeout:          5 * time.Second,
	}
}

func gwNewInvoker(t *testing.T, images *gwFakeImages) *GatewayInvoker {
	t.Helper()
	return NewGatewayInvoker(images, &gwFakeKeyLoader{key: gwTestAPIKey(42, 9)})
}

// ---------------------------------------------------------------------------
// ⚠ 钱的测试：一单 N 张必须产生 N 个互不相同的 request_id
// ---------------------------------------------------------------------------

// 一单 3 张（含「2 图 × 同 1 词」）必须产生 3 个互不相同的 request_id。
//
// 为什么专门测「2 图 × 同 1 词」：multipart 的计费指纹只取 StickySessionSeed
// （endpoint|model|size|prompt），**seed 里不含图片字节**，
// 所以这两张的指纹完全一样，靠指纹兜不住 —— 只能靠 request_id。
func TestGenerate_ThreeItemsProduceThreeDistinctRequestIDs(t *testing.T) {
	images := &gwFakeImages{}
	invoker := gwNewInvoker(t, images)

	reqs := []dkdomain.GenerateRequest{
		gwTestRequest(1, 1, "白底商品图"), // 图1 × 词1
		gwTestRequest(2, 1, "白底商品图"), // 图2 × 同一个词 ← 指纹相同的那一对
		gwTestRequest(3, 1, "场景图"),   // 图1 × 词2
	}
	for _, req := range reqs {
		if _, err := invoker.Generate(context.Background(), req); err != nil {
			t.Fatalf("Generate 失败: %v", err)
		}
	}

	calls := images.snapshot()
	if len(calls) != 3 {
		t.Fatalf("上游应该被调用 3 次，实际 %d 次", len(calls))
	}

	seen := map[string]bool{}
	for i, call := range calls {
		want := dkdomain.BillingRequestID(gwTestJobUID, i+1, 1)
		if call.clientRequestID != want {
			t.Errorf("第 %d 张的 ClientRequestID = %q，应为 %q", i+1, call.clientRequestID, want)
		}
		if call.requestID != want {
			t.Errorf("第 %d 张的 RequestID = %q，应为 %q", i+1, call.requestID, want)
		}
		if seen[call.clientRequestID] {
			t.Fatalf("第 %d 张的 request_id 与前面重复了：%q —— 这会让上游幂等表判重、静默不扣钱",
				i+1, call.clientRequestID)
		}
		seen[call.clientRequestID] = true
	}
}

// 入站请求固定带 X-Request-Id（ERP 图省事的典型写法）时，
// 合成 context 仍然必须覆写成每张图独有的 id。
//
// 不覆写的后果：那把 Key 下**所有任务**全部塌缩成一条 usage_logs，
// 从第 2 张起一分钱不扣。
func TestGenerate_OverridesInheritedRequestIDs(t *testing.T) {
	images := &gwFakeImages{}
	invoker := gwNewInvoker(t, images)

	// 模拟 RequestLogger 中间件注进来的固定 id。
	parent := context.WithValue(context.Background(), ctxkey.RequestID, "erp-fixed-request-id")
	parent = context.WithValue(parent, ctxkey.ClientRequestID, "erp-fixed-request-id")

	for seq := 1; seq <= 3; seq++ {
		if _, err := invoker.Generate(parent, gwTestRequest(seq, 1, "白底商品图")); err != nil {
			t.Fatalf("Generate 失败: %v", err)
		}
	}

	seen := map[string]bool{}
	for i, call := range images.snapshot() {
		if call.clientRequestID == "erp-fixed-request-id" || call.requestID == "erp-fixed-request-id" {
			t.Fatalf("第 %d 张继承了入站的固定 request_id，没有被覆写", i+1)
		}
		if seen[call.clientRequestID] {
			t.Fatalf("第 %d 张的 request_id 重复：%q", i+1, call.clientRequestID)
		}
		seen[call.clientRequestID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("应该有 3 个互不相同的 request_id，实际 %d 个", len(seen))
	}
}

// attempt +1 必须换新 id（重试 = 重新出一张 = 该扣一次钱）；
// 同一个 (seq, attempt) 重投多少次都是同一个 id（幂等，不重复扣）。
func TestGenerate_RequestIDIsStablePerAttempt(t *testing.T) {
	images := &gwFakeImages{}
	invoker := gwNewInvoker(t, images)

	for _, req := range []dkdomain.GenerateRequest{
		gwTestRequest(1, 1, "白底商品图"),
		gwTestRequest(1, 1, "白底商品图"), // 同一张重投
		gwTestRequest(1, 2, "白底商品图"), // 重试一次
	} {
		if _, err := invoker.Generate(context.Background(), req); err != nil {
			t.Fatalf("Generate 失败: %v", err)
		}
	}

	calls := images.snapshot()
	if calls[0].clientRequestID != calls[1].clientRequestID {
		t.Errorf("同一 (seq,attempt) 重投必须复用同一个 id：%q vs %q",
			calls[0].clientRequestID, calls[1].clientRequestID)
	}
	if calls[2].clientRequestID == calls[0].clientRequestID {
		t.Errorf("attempt 变了必须换新 id，否则重试出的图一分钱不扣：%q", calls[2].clientRequestID)
	}
}

// 落 usage_logs 的完整值（带 client: 前缀）绝不能超过 64 —— 超了是
// PostgreSQL 22001，钱扣了账单写不进去，而且没有任何告警。
func TestResolveBillingRequestID_NeverExceedsUsageLogColumn(t *testing.T) {
	for _, tc := range []struct{ seq, attempt int }{
		{1, 1},
		{9999, 99},
		{dkdomain.BillingRequestIDMaxSeq, dkdomain.BillingRequestIDMaxAttempt},
	} {
		req := gwTestRequest(tc.seq, tc.attempt, "词")
		id, err := resolveGatewayBillingRequestID(req)
		if err != nil {
			t.Fatalf("seq=%d attempt=%d: %v", tc.seq, tc.attempt, err)
		}
		stored := dkdomain.UpstreamClientRequestIDPrefix + id
		if len(stored) > dkdomain.UsageLogRequestIDMaxLen {
			t.Fatalf("seq=%d attempt=%d 的落库值超长（%d）: %q", tc.seq, tc.attempt, len(stored), stored)
		}
	}
}

// 调用方给的 request_id 与 (job,seq,attempt) 推导值不一致时必须失败，
// 不许糊里糊涂发出去 —— 那要么重复扣钱、要么白出一张图。
func TestGenerate_RejectsMismatchedBillingRequestID(t *testing.T) {
	images := &gwFakeImages{}
	invoker := gwNewInvoker(t, images)

	req := gwTestRequest(1, 1, "白底商品图")
	req.BillingRequestID = dkdomain.BillingRequestID(gwTestJobUID, 2, 1) // 张冠李戴

	_, err := invoker.Generate(context.Background(), req)
	gwRequireCode(t, err, dkdomain.ErrCodeInternal)
	if len(images.snapshot()) != 0 {
		t.Fatal("参数不一致时绝不能真的发给上游")
	}
}

// 调用方没给 id 时，用 (job, seq, attempt) 现算。
func TestGenerate_DerivesBillingRequestIDWhenMissing(t *testing.T) {
	images := &gwFakeImages{}
	invoker := gwNewInvoker(t, images)

	req := gwTestRequest(5, 2, "白底商品图")
	req.BillingRequestID = ""

	result, err := invoker.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	want := dkdomain.StoredBillingRequestID(gwTestJobUID, 5, 2)
	if result.StoredBillingRequestID != want {
		t.Fatalf("StoredBillingRequestID = %q，应为 %q", result.StoredBillingRequestID, want)
	}
	if got := images.snapshot()[0].clientRequestID; got != dkdomain.BillingRequestID(gwTestJobUID, 5, 2) {
		t.Fatalf("发给上游的 id = %q，不是推导出来的那个", got)
	}
}

// ---------------------------------------------------------------------------
// 合成 context 的形状
// ---------------------------------------------------------------------------

// 合成 context 的 Writer 必须是这一张图自己的，绝不能是别人的。
//
// 共用 Writer 的后果：往已经归还对象池 / 已经写完的 Writer 上写，
// 高并发下串包或直接 panic。
func TestGenerate_EachCallGetsItsOwnWriter(t *testing.T) {
	images := &gwFakeImages{}
	invoker := gwNewInvoker(t, images)

	// 冒充「原始」上下文（比如一个入站 HTTP 请求的 gin.Context）。
	originalRecorder := gwNewRecorder()
	originalCtx, _ := gin.CreateTestContext(originalRecorder)

	for seq := 1; seq <= 2; seq++ {
		if _, err := invoker.Generate(context.Background(), gwTestRequest(seq, 1, "词")); err != nil {
			t.Fatalf("Generate 失败: %v", err)
		}
	}

	calls := images.snapshot()
	if calls[0].writer == originalCtx.Writer {
		t.Fatal("合成 context 用了原始 Writer")
	}
	if calls[0].writer == calls[1].writer {
		t.Fatal("两张图共用了同一个 Writer")
	}
	if originalRecorder.Body.Len() != 0 {
		t.Fatal("上游的响应写进了原始 Writer")
	}
}

// multipart 的形状：文件字段名必须是 image，URL 必须含 /images/edits。
//
// 字段名不对 → 上游报 "image file is required"（service/openai_images.go:434）；
// 路径不含 /images/edits → 上游 normalizeOpenAIImagesEndpointPath 判不出端点，
// 直接报 "unsupported images endpoint"。
func TestGenerate_MultipartShape(t *testing.T) {
	images := &gwFakeImages{}
	invoker := gwNewInvoker(t, images)

	req := gwTestRequest(1, 1, "白底商品图，正面平铺")
	if _, err := invoker.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	call := images.snapshot()[0]

	if call.method != http.MethodPost {
		t.Errorf("method = %s，应为 POST", call.method)
	}
	if !strings.Contains(call.path, "/images/edits") {
		t.Fatalf("URL.Path = %q，必须含 /images/edits 字面量", call.path)
	}

	mediaType, params, err := mime.ParseMediaType(call.contentType)
	if err != nil {
		t.Fatalf("Content-Type 解析失败: %v", err)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("Content-Type = %q，应为 multipart/form-data", call.contentType)
	}

	reader := multipart.NewReader(bytes.NewReader(call.body), params["boundary"])
	fields := map[string]string{}
	fileFields := map[string]string{} // 字段名 → 文件名
	var fileBytes []byte
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("读 multipart 失败: %v", err)
		}
		data, _ := io.ReadAll(part)
		if part.FileName() != "" {
			fileFields[part.FormName()] = part.FileName()
			fileBytes = data
			if ct := part.Header.Get("Content-Type"); ct != "image/png" {
				t.Errorf("文件段 Content-Type = %q，应为 image/png", ct)
			}
		} else {
			fields[part.FormName()] = string(data)
		}
	}

	if _, ok := fileFields["image"]; !ok {
		t.Fatalf("文件字段名必须是 image，实际拿到 %v", fileFields)
	}
	if fileFields["image"] == "" {
		t.Fatal("文件名不能为空，否则上游会把它当成普通表单字段")
	}
	if !bytes.Equal(fileBytes, req.ImageData) {
		t.Fatal("发出去的图片字节跟传进来的不一致")
	}
	if fields["prompt"] != req.Prompt {
		t.Errorf("prompt = %q，应为 %q", fields["prompt"], req.Prompt)
	}
	if fields["model"] != req.Model {
		t.Errorf("model = %q，应为 %q", fields["model"], req.Model)
	}
	if fields["size"] != req.Size {
		t.Errorf("size = %q，应为 %q", fields["size"], req.Size)
	}
}

// 绝不能带 sticky session 头：带了会让一批 50 张全粘到同一个上游账号，
// 撞上 accounts.concurrency 默认 3，批量退化成三张三张串行。
func TestGenerate_NoStickySessionHeaders(t *testing.T) {
	images := &gwFakeImages{}
	invoker := gwNewInvoker(t, images)

	if _, err := invoker.Generate(context.Background(), gwTestRequest(1, 1, "词")); err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	header := images.snapshot()[0].header
	for _, name := range stickySessionHeaders {
		if v := header.Get(name); v != "" {
			t.Errorf("合成请求带了 sticky 头 %s = %q", name, v)
		}
	}
}

// 鉴权上下文：API Key / 用户主体 / 角色三件都要塞进 gin.Context，
// 否则 Images() 第一步 GetAPIKeyFromContext 取不到就 401。
func TestGenerate_PopulatesAuthContext(t *testing.T) {
	images := &gwFakeImages{}
	invoker := gwNewInvoker(t, images)

	if _, err := invoker.Generate(context.Background(), gwTestRequest(1, 1, "词")); err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	call := images.snapshot()[0]

	if call.apiKey == nil || call.apiKey.ID != 9 {
		t.Fatalf("gin.Context 里没有拿到 API Key: %+v", call.apiKey)
	}
	if call.apiKey.User == nil || call.apiKey.Group == nil {
		t.Fatal("API Key 必须带 User 和 Group，否则上游计费准入会取不到用户")
	}
	if call.subject.UserID != 42 || call.subject.Concurrency != 5 {
		t.Fatalf("AuthSubject = %+v，应为 {42 5}", call.subject)
	}
	if call.role != "user" {
		t.Fatalf("role = %q，应为 user", call.role)
	}
	if call.ctxUserID != 42 {
		t.Fatalf("request context 里的 UserID = %d，应为 42", call.ctxUserID)
	}
}

// 调用方（浏览器/运营）取消了，出图请求也不能跟着取消：
// 上游是**故意**把出图和浏览器连接脱钩的，图照出、钱照扣，
// 我们这边掐掉只会拿不到结果 —— 钱花了图丢了。
func TestGenerate_SurvivesCallerCancellation(t *testing.T) {
	images := &gwFakeImages{}
	invoker := gwNewInvoker(t, images)

	parent, cancel := context.WithCancel(context.Background())
	cancel() // 运营关掉了网页

	result, err := invoker.Generate(parent, gwTestRequest(1, 1, "词"))
	if err != nil {
		t.Fatalf("父 context 取消不应该影响出图: %v", err)
	}
	if len(result.Images) != 1 {
		t.Fatalf("应该拿回 1 张图，实际 %d 张", len(result.Images))
	}
	if got := images.snapshot()[0].requestCtxErr; got != nil {
		t.Fatalf("合成请求的 context 已经被取消了: %v", got)
	}
}

// ---------------------------------------------------------------------------
// 结果读取
// ---------------------------------------------------------------------------

func TestGenerate_ParsesImageAndPixels(t *testing.T) {
	images := &gwFakeImages{
		respond: func(c *gin.Context, _ int) {
			gwWriteImagesSuccess(c, gwTinyPNG(1500, 1000))
		},
	}
	invoker := gwNewInvoker(t, images)

	result, err := invoker.Generate(context.Background(), gwTestRequest(2, 1, "词"))
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	if len(result.Images) != 1 {
		t.Fatalf("应该 1 张，实际 %d 张", len(result.Images))
	}
	img := result.Images[0]
	// 像素必须读得出来 —— 计费落 1K/2K/4K 哪一档全靠它（CLAUDE.md B3）。
	if img.Width != 1500 || img.Height != 1000 {
		t.Fatalf("像素 = %dx%d，应为 1500x1000", img.Width, img.Height)
	}
	if tier := dkdomain.ClassifyBillingTier(img.Width, img.Height); tier != dkdomain.BillingTier2K {
		t.Fatalf("1500x1000 应该落 2K 档，实际 %q", tier)
	}
	if img.ContentType != "image/png" {
		t.Fatalf("ContentType = %q", img.ContentType)
	}
	if result.UpstreamStatus != http.StatusOK {
		t.Fatalf("UpstreamStatus = %d", result.UpstreamStatus)
	}
	want := dkdomain.StoredBillingRequestID(gwTestJobUID, 2, 1)
	if result.StoredBillingRequestID != want {
		t.Fatalf("StoredBillingRequestID = %q，应为 %q", result.StoredBillingRequestID, want)
	}
	if len(result.StoredBillingRequestID) != dkdomain.StoredBillingRequestIDLen {
		t.Fatalf("落库值长度 = %d，应为 %d", len(result.StoredBillingRequestID), dkdomain.StoredBillingRequestIDLen)
	}
}

// 非流式生图会周期性写 " \n" 保活，结果读取必须先 TrimSpace。
func TestGenerate_ToleratesKeepalivePadding(t *testing.T) {
	images := &gwFakeImages{
		respond: func(c *gin.Context, _ int) {
			_, _ = c.Writer.Write([]byte(" \n \n"))
			gwWriteImagesSuccess(c, gwTinyPNG(4, 4))
		},
	}
	invoker := gwNewInvoker(t, images)

	result, err := invoker.Generate(context.Background(), gwTestRequest(1, 1, "词"))
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	if len(result.Images) != 1 {
		t.Fatalf("保活空白应该被 TrimSpace 掉，实际拿到 %d 张图", len(result.Images))
	}
}

// 上游英文错误要翻译成中文错误码；英文原文只落 Upstream 给管理员看。
func TestGenerate_MapsUpstreamError(t *testing.T) {
	images := &gwFakeImages{
		respond: func(c *gin.Context, _ int) {
			c.Data(http.StatusForbidden, "application/json",
				[]byte(`{"error":{"type":"permission_error","message":"Image generation is not enabled for this group"}}`))
		},
	}
	invoker := gwNewInvoker(t, images)

	_, err := invoker.Generate(context.Background(), gwTestRequest(1, 1, "词"))
	dkErr := gwRequireCode(t, err, dkdomain.ErrCodeImageNotEnabled)
	if !strings.Contains(dkErr.Message, "出图") {
		t.Fatalf("给运营看的必须是中文: %q", dkErr.Message)
	}
	if !strings.Contains(dkErr.Upstream, "Image generation is not enabled") {
		t.Fatalf("英文原文应该原样落在 Upstream: %q", dkErr.Upstream)
	}
}

// 保活已经把 200 写出去之后才失败的场景：状态码是 200，但 body 里是 error。
// 这时候仍然必须判成失败，不能当成「出了 0 张图的成功」。
func TestGenerate_MapsErrorEnvelopeEvenOn200(t *testing.T) {
	images := &gwFakeImages{
		respond: func(c *gin.Context, _ int) {
			c.Data(http.StatusOK, "application/json",
				[]byte(`{"error":{"message":"Image generation concurrency limit exceeded, please retry later"}}`))
		},
	}
	invoker := gwNewInvoker(t, images)

	_, err := invoker.Generate(context.Background(), gwTestRequest(1, 1, "词"))
	gwRequireCode(t, err, dkdomain.ErrCodeUpstreamBusy)
}

// 既有图又有 error 时保留图 —— 钱已经花了，丢图最亏。
func TestGenerate_KeepsImagesWhenErrorAlsoPresent(t *testing.T) {
	images := &gwFakeImages{
		respond: func(c *gin.Context, _ int) {
			body, _ := json.Marshal(map[string]any{
				"data":  []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(gwTinyPNG(4, 4))}},
				"error": map[string]any{"message": "partial failure"},
			})
			c.Data(http.StatusOK, "application/json", body)
		},
	}
	invoker := gwNewInvoker(t, images)

	result, err := invoker.Generate(context.Background(), gwTestRequest(1, 1, "词"))
	if err != nil {
		t.Fatalf("有图就该算成功: %v", err)
	}
	if len(result.Images) != 1 {
		t.Fatalf("图丢了")
	}
}

// 上游一个字都没写回来 + 超时 → DK_TIMEOUT。
func TestGenerate_TimesOut(t *testing.T) {
	images := &gwFakeImages{
		respond: func(c *gin.Context, _ int) {
			<-c.Request.Context().Done() // 假装上游卡住
		},
	}
	invoker := gwNewInvoker(t, images)

	req := gwTestRequest(1, 1, "词")
	req.Timeout = 50 * time.Millisecond

	_, err := invoker.Generate(context.Background(), req)
	gwRequireCode(t, err, dkdomain.ErrCodeTimeout)
}

// 返回体不是合法 JSON → DK_UPSTREAM_ERROR，原文留给管理员。
func TestGenerate_RejectsInvalidJSON(t *testing.T) {
	images := &gwFakeImages{
		respond: func(c *gin.Context, _ int) {
			c.Data(http.StatusOK, "text/plain", []byte("<html>502 Bad Gateway</html>"))
		},
	}
	invoker := gwNewInvoker(t, images)

	_, err := invoker.Generate(context.Background(), gwTestRequest(1, 1, "词"))
	gwRequireCode(t, err, dkdomain.ErrCodeUpstreamError)
}

// 上游只给链接不给 base64 时，要把图下回来（不然钱花了图丢了）。
func TestGenerate_FetchesImageByURL(t *testing.T) {
	wantBytes := gwTinyPNG(600, 600)
	images := &gwFakeImages{
		respond: func(c *gin.Context, _ int) {
			c.Data(http.StatusOK, "application/json",
				[]byte(`{"data":[{"url":"https://example.invalid/a.png"}]}`))
		},
	}
	fetched := ""
	invoker := NewGatewayInvoker(images, &gwFakeKeyLoader{key: gwTestAPIKey(42, 9)},
		WithImageFetcher(func(_ context.Context, url string) ([]byte, string, error) {
			fetched = url
			return wantBytes, "image/png", nil
		}))

	result, err := invoker.Generate(context.Background(), gwTestRequest(1, 1, "词"))
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	if fetched != "https://example.invalid/a.png" {
		t.Fatalf("下载器没拿到链接: %q", fetched)
	}
	if len(result.Images) != 1 || !bytes.Equal(result.Images[0].Data, wantBytes) {
		t.Fatal("下回来的图片字节不对")
	}
	if result.Images[0].Width != 600 {
		t.Fatalf("像素读错了: %d", result.Images[0].Width)
	}
}

// ---------------------------------------------------------------------------
// 鉴权 / 参数守卫
// ---------------------------------------------------------------------------

// Key 不属于这个任务的用户时必须拒绝 —— 继续跑就是把钱记到别人头上。
func TestGenerate_RejectsAPIKeyOfAnotherUser(t *testing.T) {
	images := &gwFakeImages{}
	invoker := NewGatewayInvoker(images, &gwFakeKeyLoader{key: gwTestAPIKey(999, 9)})

	_, err := invoker.Generate(context.Background(), gwTestRequest(1, 1, "词"))
	gwRequireCode(t, err, dkdomain.ErrCodeForbidden)
	if len(images.snapshot()) != 0 {
		t.Fatal("归属校验没过时绝不能发给上游")
	}
}

func TestGenerate_RejectsMissingAPIKey(t *testing.T) {
	images := &gwFakeImages{}
	invoker := NewGatewayInvoker(images, &gwFakeKeyLoader{err: upstreamservice.ErrAPIKeyNotFound})

	_, err := invoker.Generate(context.Background(), gwTestRequest(1, 1, "词"))
	gwRequireCode(t, err, dkdomain.ErrCodeAPIKeyMissing)
}

func TestGenerate_RejectsEmptyImageData(t *testing.T) {
	images := &gwFakeImages{}
	invoker := gwNewInvoker(t, images)

	req := gwTestRequest(1, 1, "词")
	req.ImageData = nil

	_, err := invoker.Generate(context.Background(), req)
	gwRequireCode(t, err, dkdomain.ErrCodeInternal)
}

// 上游 handler panic 不能把整个 worker 带走。
func TestGenerate_RecoversUpstreamPanic(t *testing.T) {
	images := &gwFakeImages{
		respond: func(*gin.Context, int) {
			panic("上游炸了")
		},
	}
	invoker := gwNewInvoker(t, images)

	_, err := invoker.Generate(context.Background(), gwTestRequest(1, 1, "词"))
	gwRequireCode(t, err, dkdomain.ErrCodeInternal)
}

// 没装配好时返回错误而不是 panic。
func TestGenerate_NilDependencies(t *testing.T) {
	var invoker *GatewayInvoker
	if _, err := invoker.Generate(context.Background(), gwTestRequest(1, 1, "词")); err == nil {
		t.Fatal("nil invoker 应该返回错误")
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func gwRequireCode(t *testing.T, err error, wantCode string) *dkdomain.DesignkitError {
	t.Helper()
	if err == nil {
		t.Fatalf("应该失败，错误码 %s", wantCode)
	}
	dkErr, ok := dkdomain.AsDesignkitError(err)
	if !ok {
		t.Fatalf("错误类型不是 *DesignkitError: %T / %v", err, err)
	}
	if dkErr.Code != wantCode {
		t.Fatalf("错误码 = %s，应为 %s（消息：%s）", dkErr.Code, wantCode, dkErr.Message)
	}
	if strings.TrimSpace(dkErr.Message) == "" {
		t.Fatalf("错误码 %s 没有给运营看的中文文案", dkErr.Code)
	}
	return dkErr
}

// gwNewRecorder 造一个 httptest.ResponseRecorder 的等价物，
// 单独包一层只是为了让「原始 Writer」在测试里读起来更清楚。
func gwNewRecorder() *gwRecorderWriter {
	return &gwRecorderWriter{Body: &bytes.Buffer{}, header: http.Header{}, Code: http.StatusOK}
}

type gwRecorderWriter struct {
	Body   *bytes.Buffer
	header http.Header
	Code   int
}

func (r *gwRecorderWriter) Header() http.Header { return r.header }
func (r *gwRecorderWriter) Write(p []byte) (int, error) {
	return r.Body.Write(p)
}
func (r *gwRecorderWriter) WriteHeader(code int) { r.Code = code }

var _ http.ResponseWriter = (*gwRecorderWriter)(nil)

// gwTinyPNG 得真的是一张能被 image.DecodeConfig 读出宽高的 PNG，
// 不然「按实际像素定计费档」那条测试等于没测。
func TestTinyPNGIsDecodable(t *testing.T) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(gwTinyPNG(3, 7)))
	if err != nil {
		t.Fatalf("gwTinyPNG 不是合法图片: %v", err)
	}
	if format != "png" || cfg.Width != 3 || cfg.Height != 7 {
		t.Fatalf("解码结果不对: format=%s %dx%d", format, cfg.Width, cfg.Height)
	}
}
