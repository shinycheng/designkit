//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/require"
)

// ============================================================================
// 高清放大：队列状态机单测
// ============================================================================
//
// 假上游是一个 httptest 版的 imgsvc（真 HTTP、真 multipart 解析、返回一张
// 真实小 PNG），客户端用生产那份 ImgsvcUpscaleClient —— 错误格式解析、
// 响应体读取这些真问题在这一层就能暴露。商品图服务用 asset_test.go 的
// fixture（真本地磁盘存储 + 假仓储），不碰数据库、不联网、不花钱。

// fakeImgsvc 一个可控的假 imgsvc /v1/upscale。
type fakeImgsvc struct {
	t *testing.T
	// calls 收到过几次放大请求。
	calls atomic.Int64
	// gate 非 nil 时，每个请求都先等它放行 —— 用来把任务钉在 running 状态。
	gate chan struct{}
	// done 测试收尾时关掉：卡在 gate 上的请求立刻放行，
	// 否则「断言失败、gate 没来得及 close」时 server.Close 会等 handler 等到天荒地老。
	done chan struct{}
	// respond 决定这次怎么答。默认返回一张 4 倍大的 PNG。
	respond atomic.Pointer[func(w http.ResponseWriter)]
	server  *httptest.Server
}

func newFakeImgsvc(t *testing.T, gate chan struct{}) *fakeImgsvc {
	t.Helper()
	f := &fakeImgsvc{t: t, gate: gate, done: make(chan struct{})}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/upscale", r.URL.Path)
		// 真解析 multipart：字段名必须是 file（imgsvc 那边写死的）。
		file, _, err := r.FormFile("file")
		require.NoError(t, err)
		defer func() { _ = file.Close() }()

		f.calls.Add(1)
		if f.gate != nil {
			select {
			case <-f.gate:
			case <-f.done:
			}
		}
		if respond := f.respond.Load(); respond != nil {
			(*respond)(w)
			return
		}
		out := testPNG(t, 32, 24) // 假装是放大结果，尺寸无所谓，字节是真 PNG
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(out)
	}))
	// Cleanup 按 LIFO 跑：先 close(done) 放行卡着的 handler，再 server.Close。
	t.Cleanup(f.server.Close)
	t.Cleanup(func() { close(f.done) })
	return f
}

// failWith 让下一批请求返回 imgsvc 格式的错误 JSON。
func (f *fakeImgsvc) failWith(status int, code, message string) {
	respond := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": code, "message": message},
		})
	}
	f.respond.Store(&respond)
}

// succeed 恢复成功响应。
func (f *fakeImgsvc) succeed() {
	f.respond.Store(nil)
}

// upscaleFixture 队列测试的全套零件。
type upscaleFixture struct {
	*assetTestFixture
	imgsvc *fakeImgsvc
	up     *UpscaleService
}

func newUpscaleFixture(t *testing.T, gate chan struct{}, queueCap int) *upscaleFixture {
	t.Helper()
	base := newAssetTestFixture(t)
	imgsvc := newFakeImgsvc(t, gate)

	client, err := NewUpscaleClient(imgsvc.server.URL, "", 10*time.Second)
	require.NoError(t, err)

	up, err := NewUpscaleService(UpscaleServiceDeps{
		Assets:   base.svc,
		Backend:  client,
		QueueCap: queueCap,
	})
	require.NoError(t, err)
	t.Cleanup(up.Close)

	return &upscaleFixture{assetTestFixture: base, imgsvc: imgsvc, up: up}
}

// uploadSource 传一张原图进 fixture，返回它的 uid。
// 每次内容都不一样（sha256 不撞），免得两个「不同」的源图被去重成一条。
func (f *upscaleFixture) uploadSource(t *testing.T, userID int64, seed int) string {
	t.Helper()
	payload := append(testPNG(t, 8+seed, 6), []byte(fmt.Sprintf("tail-%d", seed))...)
	result, err := f.svc.UploadAsset(context.Background(), UploadAssetInput{
		UserID:   userID,
		Origin:   dkdomain.OriginWeb,
		Filename: fmt.Sprintf("source-%d.png", seed),
		Data:     bytes.NewReader(payload),
	})
	require.NoError(t, err)
	return result.Asset.UID
}

// waitStatus 等任务变成期望状态。放大 worker 是真 goroutine，只能等。
func (f *upscaleFixture) waitStatus(t *testing.T, userID int64, uid string, want UpscaleStatus) *UpscaleTask {
	t.Helper()
	var got *UpscaleTask
	require.Eventually(t, func() bool {
		task, err := f.up.Status(context.Background(), userID, uid)
		if err != nil {
			return false
		}
		got = task
		return task.Status == want
	}, 5*time.Second, 10*time.Millisecond,
		"任务 %s 没有在期限内变成 %s（最后见到 %+v）", uid, want, got)
	return got
}

// ----------------------------------------------------------------------------
// 状态机：queued → running → done
// ----------------------------------------------------------------------------

func TestUpscale_Lifecycle_QueuedRunningDone(t *testing.T) {
	gate := make(chan struct{})
	f := newUpscaleFixture(t, gate, 0)
	uid := f.uploadSource(t, 7, 1)

	task, err := f.up.Enqueue(context.Background(), 7, dkdomain.OriginWeb, uid)
	require.NoError(t, err)
	// Enqueue 返回那一刻 worker 可能已经把它捡走了，两个状态都算对。
	require.Contains(t, []UpscaleStatus{UpscaleStatusQueued, UpscaleStatusRunning}, task.Status)

	// 假 imgsvc 卡在闸门上 → 任务必须停在 running，且查得到。
	f.waitStatus(t, 7, uid, UpscaleStatusRunning)

	close(gate)
	done := f.waitStatus(t, 7, uid, UpscaleStatusDone)

	// 产物是一条**新的**商品图，归属同一个人，能按正常路径取回。
	require.NotNil(t, done.Result)
	require.NotEqual(t, uid, done.Result.UID)
	got, err := f.svc.GetAsset(context.Background(), 7, done.Result.UID)
	require.NoError(t, err)
	require.Equal(t, "image/png", got.ContentType)
	require.Empty(t, done.ErrorMessage)
	require.Equal(t, int64(1), f.imgsvc.calls.Load())
}

// ----------------------------------------------------------------------------
// 去重：排队中 / 已完成都不重复入队
// ----------------------------------------------------------------------------

func TestUpscale_Dedup_NoDoubleProcessing(t *testing.T) {
	gate := make(chan struct{})
	f := newUpscaleFixture(t, gate, 0)
	uid := f.uploadSource(t, 7, 1)

	first, err := f.up.Enqueue(context.Background(), 7, dkdomain.OriginWeb, uid)
	require.NoError(t, err)
	second, err := f.up.Enqueue(context.Background(), 7, dkdomain.OriginWeb, uid)
	require.NoError(t, err)
	require.Equal(t, first.AssetUID, second.AssetUID)

	close(gate)
	f.waitStatus(t, 7, uid, UpscaleStatusDone)
	// 点了两次，只放了一次。
	require.Equal(t, int64(1), f.imgsvc.calls.Load())

	// 放完再点：直接拿到 done 的现有任务，不再进队列。
	third, err := f.up.Enqueue(context.Background(), 7, dkdomain.OriginWeb, uid)
	require.NoError(t, err)
	require.Equal(t, UpscaleStatusDone, third.Status)
	require.NotNil(t, third.Result)
	require.Equal(t, int64(1), f.imgsvc.calls.Load())
}

// ----------------------------------------------------------------------------
// 队列封顶
// ----------------------------------------------------------------------------

func TestUpscale_QueueFull(t *testing.T) {
	gate := make(chan struct{})
	f := newUpscaleFixture(t, gate, 1) // 队列只放得下 1 个（不含在放的那张）
	uidA := f.uploadSource(t, 7, 1)
	uidB := f.uploadSource(t, 7, 2)
	uidC := f.uploadSource(t, 7, 3)

	_, err := f.up.Enqueue(context.Background(), 7, dkdomain.OriginWeb, uidA)
	require.NoError(t, err)
	// 等 A 被 worker 捡走（卡在闸门上），确保队列真的空出来了再排 B。
	f.waitStatus(t, 7, uidA, UpscaleStatusRunning)

	_, err = f.up.Enqueue(context.Background(), 7, dkdomain.OriginWeb, uidB)
	require.NoError(t, err)

	// 第三张：队列满，必须报「排队满了」，而且**不留任务残骸**。
	_, err = f.up.Enqueue(context.Background(), 7, dkdomain.OriginWeb, uidC)
	dkErr, ok := dkdomain.AsDesignkitError(err)
	require.True(t, ok, "队列满必须返回我方错误，实际 %v", err)
	require.Equal(t, dkdomain.ErrCodeUpscaleQueueFull, dkErr.Code)
	_, err = f.up.Status(context.Background(), 7, uidC)
	dkErr, ok = dkdomain.AsDesignkitError(err)
	require.True(t, ok)
	require.Equal(t, dkdomain.ErrCodeUpscaleNotFound, dkErr.Code)

	// 放行后 A、B 都能正常跑完。
	close(gate)
	f.waitStatus(t, 7, uidA, UpscaleStatusDone)
	f.waitStatus(t, 7, uidB, UpscaleStatusDone)
}

// ----------------------------------------------------------------------------
// 失败 → 中文原因 → 重新入队
// ----------------------------------------------------------------------------

func TestUpscale_FailureThenRetry(t *testing.T) {
	f := newUpscaleFixture(t, nil, 0)
	uid := f.uploadSource(t, 7, 1)

	f.imgsvc.failWith(http.StatusInternalServerError, "internal_error", "服务端处理图片时出错")
	_, err := f.up.Enqueue(context.Background(), 7, dkdomain.OriginWeb, uid)
	require.NoError(t, err)

	failed := f.waitStatus(t, 7, uid, UpscaleStatusFailed)
	require.Equal(t, dkdomain.ErrCodeUpscaleFailed, failed.ErrorCode)
	// 给运营看的必须是中文目录文案，不是上游英文原文。
	require.Equal(t, "放大失败，重试一次。", failed.ErrorMessage)
	require.Nil(t, failed.Result)

	// failed 的任务允许重新入队（「放大失败，重试一次」点的就是这条路）。
	f.imgsvc.succeed()
	retried, err := f.up.Enqueue(context.Background(), 7, dkdomain.OriginWeb, uid)
	require.NoError(t, err)
	require.Contains(t, []UpscaleStatus{UpscaleStatusQueued, UpscaleStatusRunning}, retried.Status)

	done := f.waitStatus(t, 7, uid, UpscaleStatusDone)
	require.NotNil(t, done.Result)
	// 重试之后错误信息必须清干净，不能 done 了还挂着上一次的失败原因。
	require.Empty(t, done.ErrorMessage)
	require.Empty(t, done.ErrorCode)
	require.Equal(t, int64(2), f.imgsvc.calls.Load())
}

func TestUpscale_BackendUnavailable(t *testing.T) {
	f := newUpscaleFixture(t, nil, 0)
	uid := f.uploadSource(t, 7, 1)

	// 模型没进镜像 / onnxruntime 没装：imgsvc 返 503 upscale_unavailable。
	f.imgsvc.failWith(http.StatusServiceUnavailable, "upscale_unavailable", "onnxruntime 没装上")
	_, err := f.up.Enqueue(context.Background(), 7, dkdomain.OriginWeb, uid)
	require.NoError(t, err)

	failed := f.waitStatus(t, 7, uid, UpscaleStatusFailed)
	require.Equal(t, dkdomain.ErrCodeUpscaleUnavailable, failed.ErrorCode)
	require.Equal(t, "放大功能还没准备好，请联系管理员。", failed.ErrorMessage)
}

// ----------------------------------------------------------------------------
// 归属与找不到
// ----------------------------------------------------------------------------

func TestUpscale_OwnershipAndNotFound(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	f := newUpscaleFixture(t, gate, 0)
	uid := f.uploadSource(t, 7, 1)

	// 别人的图排不进去：报「找不到」（不泄露存在性），也不进队列。
	_, err := f.up.Enqueue(context.Background(), 8, dkdomain.OriginWeb, uid)
	dkErr, ok := dkdomain.AsDesignkitError(err)
	require.True(t, ok)
	require.Equal(t, dkdomain.ErrCodeAssetNotFound, dkErr.Code)
	require.Equal(t, int64(0), f.imgsvc.calls.Load())

	// 没排过的图查状态 → DK_UPSCALE_NOT_FOUND（重启后前端轮询走的就是这条）。
	_, err = f.up.Status(context.Background(), 7, uid)
	dkErr, ok = dkdomain.AsDesignkitError(err)
	require.True(t, ok)
	require.Equal(t, dkdomain.ErrCodeUpscaleNotFound, dkErr.Code)

	// 排进去之后，本人查得到，别人查不到。
	_, err = f.up.Enqueue(context.Background(), 7, dkdomain.OriginWeb, uid)
	require.NoError(t, err)
	_, err = f.up.Status(context.Background(), 7, uid)
	require.NoError(t, err)
	_, err = f.up.Status(context.Background(), 8, uid)
	dkErr, ok = dkdomain.AsDesignkitError(err)
	require.True(t, ok)
	require.Equal(t, dkdomain.ErrCodeUpscaleNotFound, dkErr.Code)
}

// ----------------------------------------------------------------------------
// 结果去重：同样的放大产物只占一份磁盘
// ----------------------------------------------------------------------------

func TestUpscale_ResultDedupBySHA256(t *testing.T) {
	f := newUpscaleFixture(t, nil, 0)
	// 两张**不同**的源图，但假 imgsvc 对谁都返回同一张 PNG——
	// 入库走 UploadAsset 的 sha256 去重，两次放大应指向同一条产物。
	uidA := f.uploadSource(t, 7, 1)
	uidB := f.uploadSource(t, 7, 2)

	_, err := f.up.Enqueue(context.Background(), 7, dkdomain.OriginWeb, uidA)
	require.NoError(t, err)
	doneA := f.waitStatus(t, 7, uidA, UpscaleStatusDone)

	_, err = f.up.Enqueue(context.Background(), 7, dkdomain.OriginWeb, uidB)
	require.NoError(t, err)
	doneB := f.waitStatus(t, 7, uidB, UpscaleStatusDone)

	require.Equal(t, doneA.Result.UID, doneB.Result.UID, "同样的字节必须去重成同一条商品图")
}
