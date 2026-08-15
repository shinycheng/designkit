package handler

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/gin-gonic/gin"
)

// maxUploadBytes 单张商品图的上限。
//
// 20MB 是**界面提示的口径**（CLAUDE.md 第二·五节 E）：上游网关的
// gateway.max_body_size 默认 256MB，但整个请求体是一次性读进内存的，
// 而且我们后面还要把字节交给 Python 预处理服务补白边。
// 手机原图普遍 3~8MB，20MB 够用，也给内存留了余量。
const maxUploadBytes int64 = 20 << 20

// uploadFormField 上传表单里的文件字段名。
//
// ⚠ 别跟出图时发给上游网关的 multipart 混了：那边的文件字段名**必须叫 image**
// （上游写死的），是另一段代码的事。这里是我们自己的接口，用 file。
const uploadFormField = "file"

// AssetHandler 商品图上传与读取。
type AssetHandler struct {
	assets AssetService
	// images 「继续生成」要用：把一张出好的结果图变成新的商品图。
	// **允许为 nil**（结果图服务缺席时），那条路由不挂。
	images ImageService
}

// NewAssetHandler 建 handler。images 可以为 nil。
func NewAssetHandler(assets AssetService, images ImageService) *AssetHandler {
	return &AssetHandler{assets: assets, images: images}
}

// hasContinue 「继续生成」这条路由能不能挂。
func (h *AssetHandler) hasContinue() bool {
	return h != nil && h.assets != nil && h.images != nil
}

// ContinueFromImage 处理 POST /assets/from-image/:uid。
//
// 「继续生成」：把一张**已经出好的结果图**变成一张新的商品图，
// 运营可以拿它接着出下一轮（换个提示词、换个比例、在上一版基础上再加工）。
//
// # 为什么在服务端复制，不让前端下载完再传上来
//
// 前端本来也能做（取图字节 → 构造 File → 调 POST /assets），零后端改动。
// 但一张出好的图 ~2MB，那样是**下行 2MB + 上行 2MB**。
// 局域网里无所谓，等她把系统放到公网远程用的时候，点一次要等好几秒、
// 还吃她的家宽上行。服务端复制是本地磁盘到本地磁盘。
//
// # 会不会存重复的图
//
// 不会。AssetService.UploadAsset 按 sha256 去重：同一个人重复点「继续生成」，
// 拿到的是同一条 asset 记录，磁盘上只有一份。
//
// # 归属
//
// 两边都校验：OpenImageContent 确认这张结果图是他的，
// CreateAsset 把新素材记在他名下。ERP 走这条路时同理（Origin 由挂载前缀决定）。
func (h *AssetHandler) ContinueFromImage(c *gin.Context) {
	// 纯防御：服务缺席时这条路由不会挂（register_business.go）。
	if !h.hasContinue() {
		failCode(c, dkdomain.ErrCodeInternal)
		return
	}
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid := strings.TrimSpace(c.Param("uid"))
	if uid == "" {
		failCode(c, dkdomain.ErrCodeInvalidRequest)
		return
	}

	blob, err := h.images.OpenImageContent(c.Request.Context(), userID, uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeImageNotFound)
		return
	}
	if blob == nil || len(blob.Data) == 0 {
		failCode(c, dkdomain.ErrCodeImageNotFound)
		return
	}

	contentType := strings.TrimSpace(blob.ContentType)
	if contentType == "" {
		// 结果图一律是 PNG（storage/key.go 的 object_key 就是 .png 结尾）。
		// 兜底给一个，别让 service 那边按空串去猜。
		contentType = "image/png"
	}

	asset, err := h.assets.CreateAsset(c.Request.Context(), CreateAssetInput{
		UserID:   userID,
		Origin:   originOf(c),
		Filename: "continue-" + uid + extensionForContentType(contentType),
		// ⚠ 这里给的 ContentType 只是**参考**：service 一律按字节头判真实格式。
		ContentType: contentType,
		Data:        blob.Data,
	})
	if err != nil {
		failService(c, err, dkdomain.ErrCodeImageNotFound)
		return
	}
	if asset == nil {
		failCode(c, dkdomain.ErrCodeInternal)
		return
	}
	// 201：确实新建了一条（去重命中时也返回 201，对调用方没有区别 ——
	// 它只关心「拿到一个能用的商品图编号」）。
	c.JSON(http.StatusCreated, newAssetDTO(mountPrefixOf(c), asset))
}

// extensionForContentType 给个扩展名，只为让文件名好看，语义不重要。
func extensionForContentType(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

// Create 处理 POST /assets（multipart/form-data，文件字段名 file）。
//
//	201 -> assetDTO
func (h *AssetHandler) Create(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}

	// Content-Length 是客户端说的，可能撒谎；下面还会按真实读到的字节再判一次。
	if c.Request.ContentLength > maxUploadBytes {
		failCode(c, dkdomain.ErrCodeImageTooLarge)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadBytes+1)

	fileHeader, err := c.FormFile(uploadFormField)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "too large") {
			failCode(c, dkdomain.ErrCodeImageTooLarge)
			return
		}
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("没有收到图片文件，请用 file 字段上传一张商品图。").
			WithCause(err))
		return
	}
	if fileHeader.Size > maxUploadBytes {
		failCode(c, dkdomain.ErrCodeImageTooLarge)
		return
	}

	opened, err := fileHeader.Open()
	if err != nil {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeStorageError).WithCause(err))
		return
	}
	defer func() { _ = opened.Close() }()

	// 多读 1 字节：读满说明客户端的 Content-Length / Size 不可信。
	data, err := io.ReadAll(io.LimitReader(opened, maxUploadBytes+1))
	if err != nil {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeStorageError).WithCause(err))
		return
	}
	if int64(len(data)) > maxUploadBytes {
		failCode(c, dkdomain.ErrCodeImageTooLarge)
		return
	}
	if len(data) == 0 {
		abortWithDesignkitError(c, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("上传的文件是空的，请重新选一张商品图。"))
		return
	}

	contentType, ok := resolveImageContentType(fileHeader.Header.Get("Content-Type"), fileHeader.Filename, data)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnsupportedImageFormat)
		return
	}

	asset, err := h.assets.CreateAsset(c.Request.Context(), CreateAssetInput{
		UserID:      userID,
		Origin:      originOf(c),
		Filename:    filepath.Base(fileHeader.Filename),
		ContentType: contentType,
		Data:        data,
	})
	if err != nil {
		failService(c, err, dkdomain.ErrCodeAssetNotFound)
		return
	}

	c.JSON(http.StatusCreated, newAssetDTO(mountPrefixOf(c), asset))
}

// Get 处理 GET /assets/:uid。
func (h *AssetHandler) Get(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid, ok := requireUID(c, "uid", dkdomain.ErrCodeAssetNotFound)
	if !ok {
		return
	}

	asset, err := h.assets.GetAsset(c.Request.Context(), userID, uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeAssetNotFound)
		return
	}
	c.JSON(http.StatusOK, newAssetDTO(mountPrefixOf(c), asset))
}

// Content 处理 GET /assets/:uid/content，直接回图片字节。
func (h *AssetHandler) Content(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid, ok := requireUID(c, "uid", dkdomain.ErrCodeAssetNotFound)
	if !ok {
		return
	}

	blob, err := h.assets.OpenAssetContent(c.Request.Context(), userID, uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeAssetNotFound)
		return
	}
	writeContent(c, blob, dkdomain.ErrCodeAssetNotFound)
}

// ----------------------------------------------------------------------------
// 图片格式白名单
// ----------------------------------------------------------------------------

// 允许上传的 MIME 类型。HEIC/HEIF 收下来是因为 iPhone 默认就存这个格式，
// 转码交给 Python 预处理服务（pillow-heif）。
var allowedImageContentTypes = map[string]string{
	"image/jpeg":  "image/jpeg",
	"image/jpg":   "image/jpeg", // 有些客户端会发这个非标准值
	"image/pjpeg": "image/jpeg",
	"image/png":   "image/png",
	"image/webp":  "image/webp",
	"image/heic":  "image/heic",
	"image/heif":  "image/heif",
}

// 扩展名兜底：HEIC 这类格式 http.DetectContentType 认不出来
// （它只认 JPEG/PNG/GIF/WEBP 等少数几种），而部分客户端上传时又不填 Content-Type。
var extensionContentTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".heic": "image/heic",
	".heif": "image/heif",
}

// resolveImageContentType 定下这张图的 MIME 类型。
//
// 顺序：客户端声明的 → 文件扩展名 → 嗅探字节。三条都不在白名单里就拒绝。
// **不做「反正存下来再说」的兜底**：格式不认识就意味着预处理多半会失败，
// 而预处理是 fail-closed 的，早点在上传口拒绝比让运营等到出图那一刻才失败友好得多。
func resolveImageContentType(declared, filename string, data []byte) (string, bool) {
	if normalized, ok := allowedImageContentTypes[normalizeMIME(declared)]; ok {
		return normalized, true
	}
	if ct, ok := extensionContentTypes[strings.ToLower(filepath.Ext(filename))]; ok {
		return ct, true
	}
	if normalized, ok := allowedImageContentTypes[normalizeMIME(http.DetectContentType(data))]; ok {
		return normalized, true
	}
	return "", false
}

// normalizeMIME 去掉 "; charset=..." 之类的参数并转小写。
func normalizeMIME(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if idx := strings.IndexByte(value, ';'); idx >= 0 {
		value = strings.TrimSpace(value[:idx])
	}
	return value
}
