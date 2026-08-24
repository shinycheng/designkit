//go:build unit

package handler

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// 打包下载 GET /jobs/:uid/images.zip
// ============================================================================
//
// 这一组守三条底线：
//  1. 包内文件名「第{seq}张.png」跟接口里的 seq 严格对应，且只有**出成功**的进包；
//  2. 一张成功的图都没有 → DK_ 中文 404，绝不回空 zip；
//  3. 不是他的批次 → 404（「找不到」，不泄露任务号存在）。

const testZipJobUID = "01J8ZK7Q9X2M4N6P8R0T2V4W6Y"

// zipItemView 造一张指定状态的 item；withImage 时挂一张当前版本的结果图。
func zipItemView(seq int, status dkdomain.ItemStatus, withImage bool) *JobItemView {
	v := testItemView(seq)
	v.Item.Status = status
	if withImage {
		v.Images = []*dkdomain.Image{{
			ID:          int64(seq * 10),
			UID:         testZipJobUID,
			ImageIndex:  1,
			Attempt:     1,
			IsCurrent:   true,
			ContentType: "image/png",
			CreatedAt:   testTime(),
		}}
	}
	return v
}

// readZip 把响应体当 zip 解开，按包内顺序返回「文件名 → 内容」。
func readZip(t *testing.T, body []byte) (names []string, contents map[string][]byte) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err, "响应体必须是一个能打开的 zip")
	contents = make(map[string][]byte, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
		rc, err := f.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		contents[f.Name] = data
	}
	return names, contents
}

// 包内文件名按 seq 命名、按 seq 升序排列；失败/没图的不进包；
// Content-Disposition 带 RFC 5987 编码的批次名。
func TestJobImagesZipFileNamesFollowSeq(t *testing.T) {
	jobs := &fakeJobService{
		job: testJob(), // Name = 夏季连衣裙
		items: []*JobItemView{
			// 刻意乱序给：handler 必须自己排回 seq 升序。
			zipItemView(3, dkdomain.ItemStatusSucceeded, true),
			zipItemView(1, dkdomain.ItemStatusSucceeded, true),
			zipItemView(2, dkdomain.ItemStatusFailed, false),
			zipItemView(4, dkdomain.ItemStatusSucceeded, false), // 状态成功但没图：不进包
		},
		blobBySeq: map[int]*ContentBlob{
			1: {Data: []byte("img-1"), ContentType: "image/png"},
			3: {Data: []byte("img-3"), ContentType: "image/png"},
		},
	}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/jobs/"+testZipJobUID+"/images.zip", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/zip", rec.Header().Get("Content-Type"))

	// 下载文件名：ASCII 兜底 + RFC 5987 的中文（夏季连衣裙.zip 的 UTF-8 百分号编码）。
	disposition := rec.Header().Get("Content-Disposition")
	assert.Contains(t, disposition, "attachment;")
	assert.Contains(t, disposition,
		`filename*=UTF-8''%E5%A4%8F%E5%AD%A3%E8%BF%9E%E8%A1%A3%E8%A3%99.zip`,
		"中文批次名必须按 RFC 5987 百分号编码进 filename*")
	assert.Contains(t, disposition, `filename="images.zip"`,
		"中文名编码后还要留一个纯 ASCII 的 filename 兜底")

	names, contents := readZip(t, rec.Body.Bytes())
	assert.Equal(t, []string{"第1张.png", "第3张.png"}, names,
		"只有出成功且有图的进包，且必须按 seq 升序")
	assert.Equal(t, []byte("img-1"), contents["第1张.png"])
	assert.Equal(t, []byte("img-3"), contents["第3张.png"])

	// 只取了成功的那两张的字节，失败/没图的一次都不取。
	assert.Equal(t, []int{1, 3}, jobs.contentSeqs)
}

// ERP 读组（/v1/designkit）也挂了同一条：额度耗尽照样能把付过钱的图打包取走。
func TestJobImagesZipMountedOnMachineReadGroup(t *testing.T) {
	jobs := &fakeJobService{
		job:       testJob(),
		items:     []*JobItemView{zipItemView(1, dkdomain.ItemStatusSucceeded, true)},
		blobBySeq: map[int]*ContentBlob{1: {Data: []byte("img-1"), ContentType: "image/png"}},
	}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodGet,
		"/v1/designkit/jobs/"+testZipJobUID+"/images.zip", "", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	names, _ := readZip(t, rec.Body.Bytes())
	assert.Equal(t, []string{"第1张.png"}, names)
}

// 一张成功的图都没有 → DK_IMAGE_NOT_FOUND 的中文 404，绝不回空 zip。
func TestJobImagesZipEmptyBatch404(t *testing.T) {
	jobs := &fakeJobService{
		job: testJob(),
		items: []*JobItemView{
			zipItemView(1, dkdomain.ItemStatusFailed, false),
			zipItemView(2, dkdomain.ItemStatusPending, false),
		},
	}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/jobs/"+testZipJobUID+"/images.zip", "", nil)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeImageNotFound)
	message, _ := errorObject(t, rec.Body.Bytes())["message"].(string)
	assert.Contains(t, message, "没有出好的图", "空批次要说清是还没有图，不是系统坏了")
	assert.Empty(t, jobs.contentSeqs, "没图可打时一次图字节都不该取")
}

// 越权（不是他的批次）→ 404「找不到」，不泄露任务号存在，也不取任何图。
func TestJobImagesZipNotOwner404(t *testing.T) {
	jobs := &fakeJobService{getErr: dkdomain.ErrNotFound}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/jobs/"+testZipJobUID+"/images.zip", "", nil)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeJobNotFound)
	assert.Empty(t, jobs.contentSeqs)
}

// uid 不是合法 ULID → 404，service 一次都不被调到。
func TestJobImagesZipRejectsBadUID(t *testing.T) {
	jobs := &fakeJobService{}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/jobs/not-a-ulid/images.zip", "", nil)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeJobNotFound)
	assert.Empty(t, jobs.contentSeqs)
}

// 第一张就取不到（多半是存储坏了）：还没开始写 zip，必须回 JSON 的存储错误，
// 而不是一个损坏的 zip 或误报 404。
func TestJobImagesZipFirstImageStorageErrorIsJSON(t *testing.T) {
	jobs := &fakeJobService{
		job:        testJob(),
		items:      []*JobItemView{zipItemView(1, dkdomain.ItemStatusSucceeded, true)},
		contentErr: dkdomain.NewError(dkdomain.ErrCodeStorageError),
	}
	engine := newTestEngine(t, testServices(jobs), testUserID)

	rec := doRequest(t, engine, http.MethodGet,
		"/api/v1/designkit/jobs/"+testZipJobUID+"/images.zip", "", nil)

	require.NotEqual(t, http.StatusOK, rec.Code)
	require.NotEqual(t, http.StatusNotFound, rec.Code,
		"存储坏了不能报 404 —— 运营会以为图被删了，实际要查的是磁盘")
	assertErrorEnvelope(t, rec.Body.Bytes(), dkdomain.ErrCodeStorageError)
}

// RFC 5987 编码本体：attr-char 原样，引号/分号/空格/中文全部 %XX ——
// 编码结果放进响应头必须是安全的。
func TestRFC5987Encode(t *testing.T) {
	assert.Equal(t, "abc-XYZ_0.9", rfc5987Encode("abc-XYZ_0.9"), "attr-char 原样保留")
	assert.Equal(t, "%E5%9B%BE.zip", rfc5987Encode("图.zip"))
	assert.Equal(t, "a%20b", rfc5987Encode("a b"))
	assert.Equal(t, "%22%3B%0D%0A", rfc5987Encode("\";\r\n"),
		"能拆坏响应头的字符必须全部被编码")
}
