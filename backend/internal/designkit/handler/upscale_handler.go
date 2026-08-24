package handler

import (
	"net/http"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 高清放大（Real-ESRGAN ×4）
// ============================================================================
//
//	POST /assets/:uid/upscale   排一张进放大队列（重复点 = 拿到现有任务）
//	GET  /assets/:uid/upscale   查这张的放大状态（前端每 5 秒问一次）
//
// 两条都**不花钱**（放大跑在本地 imgsvc，不经过出图网关），所以跟查询同一组
// 鉴权：浏览器组 + ERP 读组，**不挂 Heavy 限流**（轮询 5 秒一次 = 12 次/分，
// Global() 的 240 次/分绰绰有余；Heavy 只有 60 次/分还跟重查询共用桶）。
//
// 放大是**异步**的：POST 立刻返回 queued，一张要一两分钟。
// 任务落库（designkit_upscale_tasks，9004），重启后没放完的自动续跑；
// GET 返回 DK_UPSCALE_NOT_FOUND 只剩一种情况：这张图从来没排过（或不是他的）。

// UpscaleHandler 高清放大的两个端点。
type UpscaleHandler struct {
	svc UpscaleService
}

// NewUpscaleHandler 建 handler。
func NewUpscaleHandler(svc UpscaleService) *UpscaleHandler {
	return &UpscaleHandler{svc: svc}
}

// upscaleTaskDTO 任务状态的对外形状。字段定了别改（ERP 会按它解析）。
type upscaleTaskDTO struct {
	// AssetUID 被放大的那张商品图。
	AssetUID string `json:"asset_uid"`
	// Status queued / running / done / failed。
	Status string `json:"status"`
	// ErrorMessage failed 时的中文原因。
	ErrorMessage string `json:"error_message,omitempty"`
	// ErrorCode failed 时的我方错误码（DK_ 前缀），方便截图报障。
	// ⚠ 字段名不能叫 code：上游幂等协调器会把名为 code 的字段脱敏成 ***。
	ErrorCode string `json:"error_code,omitempty"`
	// Result done 时的产物：一条新的商品图，直接能塞进下一批的 asset_uids。
	Result    *assetDTO `json:"result,omitempty"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at"`
}

// upscaleTaskEnvelope 统一包一层 task，跟设计冻结的 {task:{status,...}} 对齐。
type upscaleTaskEnvelope struct {
	Task upscaleTaskDTO `json:"task"`
}

func newUpscaleTaskEnvelope(prefix string, view *UpscaleTaskView) upscaleTaskEnvelope {
	dto := upscaleTaskDTO{
		AssetUID:     view.AssetUID,
		Status:       view.Status,
		ErrorMessage: view.ErrorMessage,
		ErrorCode:    view.ErrorCode,
		CreatedAt:    timeString(view.CreatedAt),
		UpdatedAt:    timeString(view.UpdatedAt),
	}
	if view.ResultAsset != nil {
		result := newAssetDTO(prefix, view.ResultAsset)
		dto.Result = &result
	}
	return upscaleTaskEnvelope{Task: dto}
}

// Start 处理 POST /assets/:uid/upscale。
//
//	202 -> {task:{status:"queued",...}}   刚排进去（或这张已在排队/在放）
//	202 -> {task:{status:"done",...}}     这张之前就放完了，结果直接给
//	404 DK_ASSET_NOT_FOUND                图不存在或不是他的
//	429 DK_UPSCALE_QUEUE_FULL             队列满（「排队满了，等会儿再试。」）
//
// 统一 202：调用方只看 task.status 分支，不用记「哪个状态对应哪个 HTTP 码」。
func (h *UpscaleHandler) Start(c *gin.Context) {
	if h == nil || h.svc == nil {
		failCode(c, dkdomain.ErrCodeUpscaleUnavailable)
		return
	}
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid := strings.TrimSpace(c.Param("uid"))
	if uid == "" {
		failCode(c, dkdomain.ErrCodeAssetNotFound)
		return
	}

	view, err := h.svc.StartUpscale(c.Request.Context(), userID, originOf(c), uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeAssetNotFound)
		return
	}
	if view == nil {
		failCode(c, dkdomain.ErrCodeInternal)
		return
	}
	c.JSON(http.StatusAccepted, newUpscaleTaskEnvelope(mountPrefixOf(c), view))
}

// Status 处理 GET /assets/:uid/upscale。
//
//	200 -> {task:{...}}
//	404 DK_UPSCALE_NOT_FOUND   没有这张的任务（多半是服务重启过，重新点一次）
func (h *UpscaleHandler) Status(c *gin.Context) {
	if h == nil || h.svc == nil {
		failCode(c, dkdomain.ErrCodeUpscaleUnavailable)
		return
	}
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid := strings.TrimSpace(c.Param("uid"))
	if uid == "" {
		failCode(c, dkdomain.ErrCodeUpscaleNotFound)
		return
	}

	view, err := h.svc.UpscaleStatus(c.Request.Context(), userID, uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeUpscaleNotFound)
		return
	}
	if view == nil {
		failCode(c, dkdomain.ErrCodeUpscaleNotFound)
		return
	}
	c.JSON(http.StatusOK, newUpscaleTaskEnvelope(mountPrefixOf(c), view))
}
