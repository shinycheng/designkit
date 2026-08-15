package handler

import (
	"net/http"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/gin-gonic/gin"
)

// CatalogHandler 出图比例等配置的读取。
type CatalogHandler struct {
	catalog CatalogService
}

// NewCatalogHandler 建 handler。
func NewCatalogHandler(catalog CatalogService) *CatalogHandler {
	return &CatalogHandler{catalog: catalog}
}

// Ratios 处理 GET /ratios。
//
// 返回的是 designkit_settings.ratios 里配的比例，顺序即界面显示顺序。
// 以后加比例是改配置，不用改代码 —— 我们靠「给输入图补白边」控制比例，
// 不受官方那三种尺寸限制。
//
// ⚠ unit_price 为 null（price_confirmed=false）时，界面必须显示「价格待确认」，
// **不许显示任何猜的单价**：每个比例实际出多大像素、落 1K/2K/4K 哪一档，
// 必须真出图实测才知道（设计定型 第十节 1，上线阻塞项）。
func (h *CatalogHandler) Ratios(c *gin.Context) {
	options, err := h.catalog.ListRatios(c.Request.Context())
	if err != nil {
		failService(c, err, dkdomain.ErrCodeInternal)
		return
	}

	items := make([]ratioDTO, 0, len(options))
	for _, o := range options {
		items = append(items, newRatioDTO(o))
	}

	c.JSON(http.StatusOK, ratioListDTO{
		Ratios:   items,
		Currency: dkdomain.CurrencyUSD,
	})
}
