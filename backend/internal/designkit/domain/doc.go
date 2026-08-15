// Package domain 是 designkit 模块的「共用词汇表」。
//
// 这里只放三样东西：
//  1. 类型（实体、状态、错误、请求/响应参数）
//  2. 常量（状态值、错误码、比例、计费档、对象存储路径格式）
//  3. 纯函数（状态迁移白名单、request_id 生成、比例解析、计费档分类、错误映射）
//     和接口定义（Repository / ObjectStore / ImagePreprocessor / GatewayInvoker）
//
// **本包不碰数据库、不碰 HTTP、不碰对象存储、不起 goroutine。**
// 除了标准库和 github.com/shopspring/decimal（上游已有的直接依赖），不 import 任何东西；
// 尤其**不 import 上游的 internal/service 或 internal/handler**——那会把跟版冲突引进来，
// 也会在 handler 侧造出循环依赖。
//
// # 全模块统一约定（后面每个包都照这份写）
//
// **可空列一律用指针**（*string / *int64 / *int / *time.Time / *Money），
// 不用 sql.NullString 那一套。理由：指针在 JSON 序列化时天然是 null，
// sql.NullXxx 序列化出来是 {"String":"","Valid":false} 这种发给 ERP 会出事的形状。
// repository 层扫描时可以先扫进 sql.NullXxx / decimal.NullDecimal，再转成指针填进结构体。
//
// **金额一律用 Money（= decimal.Decimal），禁止 float64 参与加减。** 见 money.go 的长注释。
//
// **时间列一律 time.Time**（可空的用 *time.Time）。数据库侧是 TIMESTAMPTZ。
//
// **对外只暴露 uid（26 位 ULID），不暴露自增主键 id。**
// 结构体里两个都有：ID 给内部用，UID 给接口用。handler 层禁止把 ID 写进响应。
//
// **金额一律美元**（CLAUDE.md 决策 18）。CurrencyUSD 是唯一取值，
// 接口里带 currency 字段固定 "USD"，界面渲染 $，文案里不许出现「元」。
//
// **面向运营的文案一律中文**（设计定型 第八节）。
// 上游返回的英文原文只写进 job_items.last_error_message 给管理员看，界面不显示。
//
// 列名以 backend/migrations/9001_designkit_init.sql 为准，那个文件已经跑过、永不可改。
package domain
