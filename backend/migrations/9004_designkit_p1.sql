-- designkit P1 两件事：批次级 keep_transparency 落列 + 高清放大任务落库
-- PostgreSQL 15+
--
-- 【为什么编号是 9xxx】同 9001~9003：上游用 001~2xx，我们从 9001 起。
-- 迁移按「文件名字节序」执行，本文件排在 9001 之后（designkit_jobs 已建好），
-- 除此之外不依赖上游任何一次迁移的产物（新表自己 CREATE TABLE IF NOT EXISTS）。
--
-- 【这个文件跑过一次之后就永远不能再改】
-- schema_migrations 以文件名为主键存内容的 SHA256，改一个字（包括注释、缩进）
-- 校验和就对不上，整个进程起不来。要改需求一律新加一个编号更大的文件（9005…）。


-- ============================================================
-- 1. designkit_jobs.keep_transparency —— 每批可选「保留透明底」
-- ============================================================
--
-- 在此之前 keep_transparency 是公开接口字段但被出图路径忽略（worker 用全局
-- 配置且恒为 false，ERP 传 true 拿到的还是白底、接口不报错）。
-- 加列之后按批生效：预处理产物（designkit_asset_variants）的唯一键里
-- 本来就有 keep_transparency 这一维，worker 改从批次行取值即可。
--
-- 【历史行语义】DEFAULT FALSE = 合成白底，正是这些批次当时的实际行为。
--
-- ⚠ 跟 9003 的约定不同：这一列**要**加进 repository/scan.go 的 jobColumns
-- （worker 取批次走 scanJob，必须扫得到它）。columns_parity_test.go 已改成
-- 「9001 建表列 + 本文件 ALTER 追加列」一起比对，列序 = 物理列序（追加在最后）。
ALTER TABLE designkit_jobs
    ADD COLUMN IF NOT EXISTS keep_transparency BOOLEAN NOT NULL DEFAULT FALSE;


-- ============================================================
-- 2. designkit_upscale_tasks —— 高清放大任务（Real-ESRGAN ×4）
-- ============================================================
--
-- 在此之前放大队列全在内存里，重启丢任务是设计代价。落库之后任务表以数据库
-- 为准：重启时把 queued/running 的任务重新入队接着放（running 的重置回
-- queued——上次进程死在半路），Status 查询也走这张表。
-- 内存里只剩一条 chan 作队列信道。
--
-- 【为什么一次重试一行、不复用旧行】失败重放大 = 插一行新任务，旧的 failed
-- 行留作历史。所以 (user_id, asset_uid) 是普通索引不是唯一索引，
-- 查询一律取「该图最新的一行」（ORDER BY created_at DESC, uid DESC LIMIT 1）。
--
-- 【无外键】asset_uid / result_asset_uid 指向 designkit_assets.uid、
-- user_id 指向上游 users.id，都刻意不写 REFERENCES——放大任务是可丢弃的
-- 流水记录，不该反过来挡商品图的删除；类型跟 designkit_assets.uid
-- （VARCHAR(32)）保持一致，避免跨类型比较。
--
-- 【error_code 一列在拍板清单之外】任务失败时接口返回 error_code（DK_ 前缀，
-- 运营截图报障用，upscale_handler.go 的 upscaleTaskDTO 已是对外契约），
-- 不落库的话重启之后 failed 任务只剩 message 没有 code。现在加成本为零。
CREATE TABLE IF NOT EXISTS designkit_upscale_tasks (
    uid              CHAR(26) PRIMARY KEY,                 -- 任务编号（ULID），对外不暴露，只是主键
    asset_uid        VARCHAR(32) NOT NULL,                 -- 被放大的那张商品图（designkit_assets.uid）
    user_id          BIGINT NOT NULL,                      -- 谁点的（上游 users.id，无外键，理由见文件头）
    origin           VARCHAR(16) NOT NULL DEFAULT 'web',   -- web=界面；erp=外部系统。入库结果记在这个来路上
    status           TEXT NOT NULL CHECK (status IN ('queued','running','done','failed')),
    result_asset_uid VARCHAR(32),                          -- done 时的产物（一条新的商品图，sha256 去重）
    error_code       VARCHAR(64) NOT NULL DEFAULT '',      -- failed 时的我方错误码（DK_ 前缀），空串 = 无
    error_message    TEXT NOT NULL DEFAULT '',             -- failed 时给运营看的中文，空串 = 无
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 重启恢复扫的就是 status IN ('queued','running')，按状态过滤。
CREATE INDEX IF NOT EXISTS idx_designkit_upscale_tasks_status
    ON designkit_upscale_tasks (status);

-- 「这张图最新的放大任务」：入队去重和前端每 5 秒的状态轮询都走它。
CREATE INDEX IF NOT EXISTS idx_designkit_upscale_tasks_user_asset
    ON designkit_upscale_tasks (user_id, asset_uid);
