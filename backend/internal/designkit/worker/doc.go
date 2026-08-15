// Package worker 是 designkit 的出图队列、崩溃恢复和结算。
//
// 它是整个模块里**唯一会自己起 goroutine**的包，也是唯一会长时间持有一张图的
// 「钱」的地方。三个后台角色跑在同一个 Pool 里：
//
//	① 出图 worker（并发数取 designkit_settings.worker_concurrency，默认 4）
//	   领取 item → 预处理 → 调网关出图 → 存图 → 写回结果
//	② 僵尸巡检（reaper）
//	   扫 heartbeat 超过 10 分钟没动静的 holding/running 任务，原子转 failed 并释放冻结
//	③ 结算 worker（settlement）
//	   把 settling 的任务按 billing_request_id 回 usage_logs 对账，全部命中才落终态
//
// # 为什么并发默认是 4 而不是 5
//
// 上游对**每个用户**默认放 5 个并发（CLAUDE.md B5），超出的会排队最多 25 个、
// 等 30 秒，再多直接 429。我们把 4 个格子占满，第 5 格留给运营在界面上的单张操作
// （重试一张、预览），否则运营点一下要干等半分钟。
//
// # 为什么租约 180 秒、每 60 秒续一次
//
// 一张图大约 20 秒，180 秒是「网关卡住」和「进程被杀」都能覆盖的量级。
// 续期间隔取租约的三分之一：漏掉一次续期还有两次机会，不会因为一次网络抖动
// 就把自己的租约丢了、让别人把同一张图重出一遍（= 重新收一次钱）。
//
// **写回结果必须带 worker_id 守卫**（repository 那条
// `WHERE id=? AND worker_id=? AND status='running'`）。影响行数不等于 1 时
// repository 返回 domain.ErrLeaseLost，本包**丢弃结果、不重试写入** ——
// 那说明租约已经被别人抢走，对方正在（或已经）出同一张图，硬写会把两次结果
// 混在一行上。
//
// # 关于 SIGTERM：不能指望做完
//
// 上游 main.go 给优雅关闭的窗口只有 **5 秒**（`context.WithTimeout(..., 5*time.Second)`，
// backend/cmd/server/main.go:182），而且那 5 秒只等 HTTP server，
// **后台 goroutine 根本不在等待范围内**。一张图要 20 秒，所以：
//
//	收到停止信号 → 立刻不再领新 item → 取消在途的出图 →
//	把在途 item 的租约还回去（CompleteItemFailure，Terminal=false，立刻可被别人领取）
//
// 「还回去」用的是失败写回的非终态分支：item 退回 pending，另一个副本
// （或重启后的自己）会重新领走。**注意这一张的钱可能已经花了** ——
// 上游出图跟我们的 context 是故意脱钩的，取消只是我们不等了，
// 图照出、钱照扣，只是结果我们没接住。这是重启期不可避免的代价，
// 日志里会带上 billing_request_id 方便管理员对账。
//
// # 三个「不许」
//
//   - **不许用应用内 mutex 做多副本互斥。** 巡检和结算都可能有多个副本同时跑，
//     进程内的锁在另一台机器上根本不存在。用 PostgreSQL advisory lock（AdvisoryLocker）。
//   - **不许用「成功张数 × 单价」算 actual_cost。** 同一批 10 张可能落不同计费档、
//     单价不同（CLAUDE.md B3），必须 SUM(job_items.billed_cost)。
//   - **不许因为「实际 > 预估」就报错。** 钱已经被上游扣走了，报错只会让任务卡死。
//     超 1.2 倍记一条 warn 告警，超 2 倍记 critical，然后照样结算。
package worker
