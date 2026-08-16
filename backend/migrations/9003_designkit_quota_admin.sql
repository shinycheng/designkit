-- designkit 额度申请：处理详情两列（决策 19 的管理端闭环）
-- PostgreSQL 15+
--
-- 【为什么编号是 9xxx】同 9001/9002：上游用 001~2xx，我们从 9001 起。
-- 迁移按「文件名字节序」执行，本文件排在 9001 之后（表已建好），
-- 除此之外不依赖上游任何一次迁移的产物。
--
-- 【这个文件跑过一次之后就永远不能再改】
-- schema_migrations 以文件名为主键存内容的 SHA256，改一个字（包括注释、缩进）
-- 校验和就对不上，整个进程起不来。要改需求一律新加一个编号更大的文件（9004…）。
--
-- 【为什么是加列而不是改 9001】9001 已经在群晖上跑过，绝对不能动（CLAUDE.md B6）。
--
-- ⚠ 同一个约定要记住：repository/scan.go 的 quotaRequestColumns 常量
-- 只对应 9001 里的建表列（columns_parity_test.go 拿 9001 当唯一真相比对），
-- 这两列**不要**加进那个常量，管理端查询自己单独列列名。

-- 管理员处理时写的备注（驳回原因、加额说明），可为空。
ALTER TABLE designkit_quota_requests
    ADD COLUMN IF NOT EXISTS handle_note TEXT;

-- 通过时实际加的金额（美元）。驳回的行保持 NULL。
-- 精度同上游 users.balance 的用法（金额在系统里一律美元，决策 18）。
ALTER TABLE designkit_quota_requests
    ADD COLUMN IF NOT EXISTS approved_amount NUMERIC(20, 8);
