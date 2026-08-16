package admin

// designkit 加的文件（不是上游自带的）。
//
// designkit 的「额度申请」管理页在管理员点「通过」时要给用户加余额，
// 而加余额只允许走上游 AdminService.UpdateUserBalance 这条原子通道
// （直接 UPDATE users.balance 会盖掉并发扣费，而且不失效缓存、不留调整记录）。
//
// wire 装配好的 AdminService 只存在于 UserHandler 的未导出字段里，
// designkit 模块（NewModule 拿得到 *handler.Handlers）从这里取。
// 只读取、不替换 —— 这是唯一的用途，别拿它做别的。

import "github.com/Wei-Shaw/sub2api/internal/service"

// AdminService 返回 wire 装配好的管理服务（designkit 额度申请「通过」时加余额用）。
func (h *UserHandler) AdminService() service.AdminService {
	if h == nil {
		return nil
	}
	return h.adminService
}
