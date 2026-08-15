package handler

import (
	"net/http"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/gin-gonic/gin"
)

// ImageHandler 结果图的读取。
type ImageHandler struct {
	images ImageService
}

// NewImageHandler 建 handler。
func NewImageHandler(images ImageService) *ImageHandler {
	return &ImageHandler{images: images}
}

// Get 处理 GET /images/:uid。
//
// 返回体里带 job_uid + seq，这样 ERP 能把图片对回「第几批的第几张」——
// 数据库里的 job_id / item_id 是内部主键，对外一律不暴露。
func (h *ImageHandler) Get(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid, ok := requireUID(c, "uid", dkdomain.ErrCodeImageNotFound)
	if !ok {
		return
	}

	view, err := h.images.GetImage(c.Request.Context(), userID, uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeImageNotFound)
		return
	}
	if view == nil || view.Image == nil {
		failCode(c, dkdomain.ErrCodeImageNotFound)
		return
	}
	c.JSON(http.StatusOK, newImageViewDTO(mountPrefixOf(c), view))
}

// Content 处理 GET /images/:uid/content，直接回图片字节。
func (h *ImageHandler) Content(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid, ok := requireUID(c, "uid", dkdomain.ErrCodeImageNotFound)
	if !ok {
		return
	}

	blob, err := h.images.OpenImageContent(c.Request.Context(), userID, uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeImageNotFound)
		return
	}
	writeContent(c, blob, dkdomain.ErrCodeImageNotFound)
}
