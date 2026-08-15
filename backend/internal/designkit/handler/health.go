package handler

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	dkservice "github.com/Wei-Shaw/sub2api/internal/designkit/service"

	"github.com/gin-gonic/gin"
)

// healthPingTimeout 健康检查探数据库的超时。
// 故意设得很短：健康检查要能**快速**告诉运维「数据库连不上」，
// 而不是跟着数据库一起卡住。
const healthPingTimeout = 2 * time.Second

// HealthHandler 健康检查。
type HealthHandler struct {
	health *dkservice.HealthService
}

func NewHealthHandler(health *dkservice.HealthService) *HealthHandler {
	return &HealthHandler{health: health}
}

// Health 同时挂在两条路径上：
//
//	GET /api/v1/designkit/health  （浏览器，jwtAuth）
//	GET /v1/designkit/health      （机器，本轮免鉴权）
//
// 固定返回 HTTP 200，用 body 里的 db 字段表达数据库状态：
// 监控脚本判「模块有没有挂进去」看状态码，判「数据库通不通」看 db 字段。
//
// 响应体（对外契约，别改字段名）：
//
//	{"status":"ok","module":"designkit","db":"ok","module_status":"ok"}
//	{"status":"degraded","module":"designkit","db":"error","module_status":"ok"}
//	{"status":"degraded","module":"designkit","db":"ok","module_status":"degraded",
//	 "degraded_reasons":["出图队列没有启动，缺少：图像处理服务。已提交的批次会一直停在排队中"]}
//
// db 和 module_status 是两件事，别混：
//   - db           数据库通不通
//   - module_status 这个模块的功能齐不齐（出图网关没配、图像服务地址填错、
//     对象存储目录建不出来时，数据库是好的，但一张图也出不来）
//
// 只看 db 会让「整个产品是废的」显示成绿色。
//
// 具体的数据库错误只写进服务端日志，不回给调用方——
// /v1/designkit/health 是免鉴权端点，错误原文里可能带主机名、用户名。
func (h *HealthHandler) Health(c *gin.Context) {
	dbStatus := "ok"

	ctx, cancel := context.WithTimeout(c.Request.Context(), healthPingTimeout)
	defer cancel()

	if err := h.health.CheckDB(ctx); err != nil {
		dbStatus = "error"
		slog.Warn("designkit 健康检查：数据库不可用", slog.Any("error", err))
	}

	// status 必须跟着 db 走。原来这里恒为 "ok"，导致数据库挂了接口照样自称健康——
	// 而这个端点存在的唯一理由就是让人一眼看出地基有没有问题。
	//
	// HTTP 仍然返 200（不返 503）：前端 axios 拦截器会把非 2xx 当异常抛掉，
	// 那样就拿不到 db 字段，只能显示笼统的「连不上」，反而更难判断是哪一段断了。
	moduleStatus, reasons := h.health.ModuleStatus()

	status := "ok"
	if dbStatus != "ok" || moduleStatus != dkservice.ModuleStatusOK {
		status = "degraded"
	}

	body := gin.H{
		"status":        status,
		"module":        "designkit",
		"db":            dbStatus,
		"module_status": moduleStatus,
	}
	// 只在真降级时才带这个字段：正常情况下响应体保持最小。
	// 原因是中文的，直接给运营截图给管理员用。
	if len(reasons) > 0 {
		body["degraded_reasons"] = reasons
	}

	c.JSON(http.StatusOK, body)
}
