// Package service 承载 designkit 的业务逻辑。
//
// 分层约定照抄上游：**接口定义在 service，实现放 repository**
// （例：上游 repository.NewBatchImageRepository 返回的是 service.BatchImageRepository）。
// 所以依赖方向永远是 repository → service，service 里不许 import repository。
package service

import (
	"context"
	"errors"
)

// ErrDatabaseUnavailable 表示 designkit 的数据库连接池没建起来。
// 出现它说明启动阶段 db.Open 就失败了，健康检查会如实报 db=error。
var ErrDatabaseUnavailable = errors.New("designkit: database is not available")

// HealthRepository 由 designkit 的 repository 层实现。
type HealthRepository interface {
	// Ping 真的往数据库发一条 SELECT 1，不是只看连接池对象在不在。
	Ping(ctx context.Context) error
}

// 模块整体状态的三个取值。跟 db 是两回事：
// db 只回答「数据库通不通」，这个回答「这个模块的功能齐不齐」。
const (
	ModuleStatusOK       = "ok"
	ModuleStatusDegraded = "degraded"
	ModuleStatusDown     = "down"
)

// ModuleStatusFunc 由组合根注入，返回模块整体状态和中文降级原因。
//
// 为什么要单独有它：出图网关没配、图像处理服务地址填错、对象存储目录建不出来
// 这几种情况下，数据库是好的（db=ok），但**一张图也出不来**。
// 如果健康检查只看 db，运营和管理员会看到绿色的「一切正常」，
// 而实际上整个产品是废的——这正是这个端点存在的意义所在。
type ModuleStatusFunc func() (status string, reasons []string)

// HealthService 健康检查。
type HealthService struct {
	repo         HealthRepository
	moduleStatus ModuleStatusFunc
}

// NewHealthService 允许传 nil：连接池没建起来时就是 nil，
// 此时 CheckDB 直接返回 ErrDatabaseUnavailable，而不是 panic。
func NewHealthService(repo HealthRepository) *HealthService {
	return &HealthService{repo: repo}
}

// WithModuleStatus 注入模块状态回调。
//
// 用回调而不是让 handler 直接问组合根，是为了避免循环依赖：
// 依赖方向是 designkit（组合根）→ handler → service，
// handler 反过来 import 组合根就成环了。
func (s *HealthService) WithModuleStatus(fn ModuleStatusFunc) *HealthService {
	if s != nil {
		s.moduleStatus = fn
	}
	return s
}

// CheckDB 探测数据库是否可用。ctx 的超时由调用方控制。
func (s *HealthService) CheckDB(ctx context.Context) error {
	if s == nil || s.repo == nil {
		return ErrDatabaseUnavailable
	}
	return s.repo.Ping(ctx)
}

// ModuleStatus 返回模块整体状态和中文降级原因。
// 没注入回调时一律当作 ok（老行为，不制造假警报）。
func (s *HealthService) ModuleStatus() (string, []string) {
	if s == nil || s.moduleStatus == nil {
		return ModuleStatusOK, nil
	}
	status, reasons := s.moduleStatus()
	if status == "" {
		status = ModuleStatusOK
	}
	return status, reasons
}
