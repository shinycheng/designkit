//go:build unit

package handler

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tinyPNG 是一段真实的 PNG 文件头 + 最小 IHDR，够 http.DetectContentType 认出来。
var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89,
}

func uploadRequest(t *testing.T, path, field, filename, contentType string, data []byte) *http.Request {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	header := make(map[string][]string)
	header["Content-Disposition"] = []string{
		`form-data; name="` + field + `"; filename="` + filename + `"`,
	}
	if contentType != "" {
		header["Content-Type"] = []string{contentType}
	}
	part, err := writer.CreatePart(header)
	require.NoError(t, err)
	_, err = part.Write(data)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func TestCreateAssetAcceptsPNG(t *testing.T) {
	width, height := 1200, 1600
	assets := &fakeAssetService{asset: &dkdomain.Asset{
		ID:          1,
		UID:         "01J8ZK7Q9X2M4N6P8R0T2V4W6Y",
		UserID:      testUserID,
		ObjectKey:   "designkit/assets/2026/08/13/01J8ZK7Q9X2M4N6P8R0T2V4W6Y.png",
		ContentType: "image/png",
		ByteSize:    int64(len(tinyPNG)),
		Width:       &width,
		Height:      &height,
		Origin:      dkdomain.OriginWeb,
		CreatedAt:   testTime(),
	}}
	services := testServices(&fakeJobService{})
	services.Assets = assets
	engine := newTestEngine(t, services, testUserID)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, uploadRequest(t, "/api/v1/designkit/assets", uploadFormField, "裙子.png", "image/png", tinyPNG))

	require.Equal(t, http.StatusCreated, rec.Code, "响应体：%s", rec.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", body["uid"])
	assert.Equal(t, "image/png", body["content_type"])
	assert.Equal(t,
		BrowserMountPrefix+"/assets/01J8ZK7Q9X2M4N6P8R0T2V4W6Y/content",
		body["content_url"],
		"content_url 必须跟着挂载前缀走，而且是运行时拼的，绝不入库")

	_, hasObjectKey := body["object_key"]
	assert.False(t, hasObjectKey, "对象存储路径是内部信息，不对外暴露")

	require.Equal(t, 1, assets.createCall)
	assert.Equal(t, testUserID, assets.lastInput.UserID)
	assert.Equal(t, dkdomain.OriginWeb, assets.lastInput.Origin)
	assert.Equal(t, "image/png", assets.lastInput.ContentType)
	assert.Equal(t, tinyPNG, assets.lastInput.Data)
}

// ERP 前缀上传的素材要记成 erp。
func TestCreateAssetFromMachinePrefixIsERP(t *testing.T) {
	assets := &fakeAssetService{asset: &dkdomain.Asset{
		UID:         "01J8ZK7Q9X2M4N6P8R0T2V4W6Y",
		ContentType: "image/png",
		Origin:      dkdomain.OriginERP,
		CreatedAt:   testTime(),
	}}
	services := testServices(&fakeJobService{})
	services.Assets = assets
	engine := newTestEngine(t, services, testUserID)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, uploadRequest(t, "/v1/designkit/assets", uploadFormField, "a.png", "image/png", tinyPNG))

	require.Equal(t, http.StatusCreated, rec.Code, "响应体：%s", rec.Body.String())
	require.Equal(t, 1, assets.createCall)
	assert.Equal(t, dkdomain.OriginERP, assets.lastInput.Origin)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, MachineMountPrefix+"/assets/01J8ZK7Q9X2M4N6P8R0T2V4W6Y/content", body["content_url"])
}

// 不认识的格式在上传口就拒掉：预处理是 fail-closed 的，
// 让运营等到出图那一刻才失败要难受得多。
func TestCreateAssetRejectsUnsupportedFormat(t *testing.T) {
	assets := &fakeAssetService{}
	services := testServices(&fakeJobService{})
	services.Assets = assets
	engine := newTestEngine(t, services, testUserID)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, uploadRequest(t, "/api/v1/designkit/assets", uploadFormField,
		"报价单.pdf", "application/pdf", []byte("%PDF-1.7 not an image")))

	require.Equal(t, http.StatusUnsupportedMediaType, rec.Code)

	assert.Equal(t, dkdomain.ErrCodeUnsupportedImageFormat, errorObject(t, rec.Body.Bytes())["error_code"])
	assert.Zero(t, assets.createCall)
}

// 字段名写错（比如照着出图那边写成 image）要给一句能照着改的中文。
func TestCreateAssetRejectsWrongFieldName(t *testing.T) {
	services := testServices(&fakeJobService{})
	engine := newTestEngine(t, services, testUserID)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, uploadRequest(t, "/api/v1/designkit/assets", "image", "a.png", "image/png", tinyPNG))

	require.Equal(t, http.StatusBadRequest, rec.Code)

	errObj := errorObject(t, rec.Body.Bytes())
	assert.Equal(t, dkdomain.ErrCodeInvalidRequest, errObj["error_code"])
	assert.Contains(t, errObj["message"], uploadFormField)
}

func TestResolveImageContentType(t *testing.T) {
	// 客户端声明优先。
	ct, ok := resolveImageContentType("image/jpeg", "a.bin", nil)
	assert.True(t, ok)
	assert.Equal(t, "image/jpeg", ct)

	// 非标准写法归一化。
	ct, ok = resolveImageContentType("IMAGE/JPG; charset=binary", "a.bin", nil)
	assert.True(t, ok)
	assert.Equal(t, "image/jpeg", ct)

	// 客户端没填 Content-Type（HEIC 常见），靠扩展名认。
	ct, ok = resolveImageContentType("", "IMG_0421.HEIC", nil)
	assert.True(t, ok)
	assert.Equal(t, "image/heic", ct)

	// 声明和扩展名都不靠谱时嗅探字节。
	ct, ok = resolveImageContentType("application/octet-stream", "noext", tinyPNG)
	assert.True(t, ok)
	assert.Equal(t, "image/png", ct)

	// 三条都不在白名单里就拒。
	_, ok = resolveImageContentType("application/pdf", "a.pdf", []byte("%PDF-1.7"))
	assert.False(t, ok)
}
