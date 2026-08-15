// Package storage 是 designkit 自己的对象存储实现（domain.ObjectStore）。
//
// # 为什么不用上游的 service.ImageStorage
//
// 上游那个只有一个 Save(ctx, key, contentType, data) (url, err)：没有 Get、
// 没有 Delete、没有 Exists，key 上传完就丢弃，唯一实现是 S3 兼容对象存储，
// 没配置的后果是异步生图功能整个关闭（handler 直接 404），
// 不是「兜底落本地目录」。而 monica 的群晖 NAS 上**没有 S3**。
// 见 CLAUDE.md 第二·五节 C。
//
// 所以这里自己写一个本地磁盘实现：
//
//	root = os.Getenv("DESIGNKIT_STORAGE_DIR")，默认 /app/data/designkit
//	key  = 相对 root 的路径，例如
//	       designkit/2026/08/13/01J8ZK…-1-1-1.png
//	       designkit/assets/2026/08/13/01J8ZK….jpg
//	       designkit/variants/2026/08/13/01J8ZK…-3x4-o-2048.jpg
//
// key 由 domain.ImageObjectKey / AssetObjectKey / VariantObjectKey 生成并入库，
// **URL 一律运行时生成，绝不入库**。
//
// # 安全
//
// key 虽然大多来自我们自己的数据库，但也可能来自 ERP 传进来的参数，
// **一律当成不可信输入**。ValidateKey 用「白名单字符集 + 逐段检查」把
// `..`、绝对路径、`\`、`:`、控制字符、隐藏文件全挡在外面；写入和读取前
// 还会逐级 Lstat，路径里只要有一个符号链接就拒绝——
// 否则有人在数据目录里建一个软链，就能把整块盘读出去或写进去。
//
// # 原子性
//
// 写入一律「先写同目录临时文件、fsync、再 rename」。
// 直接往目标文件写，进程中途被杀就会留下半张图，而那张图的记录已经入库，
// 界面上看是「出好了」，点开是坏图。
package storage
