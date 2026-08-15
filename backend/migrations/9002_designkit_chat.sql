-- designkit AI 对话 —— 会话与消息（2 张表）
-- PostgreSQL 15+
--
-- 【为什么编号是 9xxx】
-- 上游 Sub2API 用 001~2xx，我们从 9001 起，避免跟上游撞号。
-- 但迁移是按「文件名字节序」执行的，不是数值序，所以不能假定我们永远排在最后。
-- => 本文件必须自足：自己 CREATE TABLE IF NOT EXISTS，不依赖上游任何一次迁移的产物。
--
-- 【为什么不建到上游表的外键】
-- user_id 指向上游 users.id，但刻意不写 REFERENCES：一旦写了，本文件就依赖
-- 「users 表已存在」这个上游迁移的产物，跟自足要求冲突（同 9001 的约定）。
-- 我们自建表之间（sessions -> messages）的外键正常建。
--
-- 【这个文件跑过一次之后就永远不能再改】
-- schema_migrations 以文件名为主键存内容的 SHA256，改一个字（包括注释、缩进）
-- 校验和就对不上，整个进程起不来。要改需求一律新加一个编号更大的文件（9003…）。
--
-- 【约定】（同 9001）
-- - 时间列一律 TIMESTAMPTZ
-- - 对外只暴露 uid（26 位 ULID），不暴露自增主键 id


-- ============================================================
-- 1. designkit_chat_sessions —— 一段对话
-- ============================================================
CREATE TABLE IF NOT EXISTS designkit_chat_sessions (
    id          BIGSERIAL PRIMARY KEY,
    uid         CHAR(26) UNIQUE NOT NULL,             -- 对外暴露的 ULID，接口里只出现它
    user_id     BIGINT NOT NULL,                      -- 上游 users.id（无外键，理由见文件头）
    title       TEXT NOT NULL DEFAULT '',             -- 会话标题；空串 = 还没起名（通常用首条消息生成）
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),   -- 每插一条消息就摸一下，列表按它倒序
    deleted_at  TIMESTAMPTZ                           -- 软删除：删了就不再列出，消息行保留
);

-- 会话列表页：本人未删的按最近活跃倒序。部分索引把软删的行直接排除在索引外。
CREATE INDEX IF NOT EXISTS idx_designkit_chat_sessions_user_updated
    ON designkit_chat_sessions (user_id, updated_at DESC) WHERE deleted_at IS NULL;


-- ============================================================
-- 2. designkit_chat_messages —— 对话里的一条消息
-- ============================================================
CREATE TABLE IF NOT EXISTS designkit_chat_messages (
    id          BIGSERIAL PRIMARY KEY,                -- 消息顺序就是 id 顺序，读取显式 ORDER BY id ASC
    session_id  BIGINT NOT NULL REFERENCES designkit_chat_sessions(id) ON DELETE CASCADE,
    role        TEXT NOT NULL CHECK (role IN ('user','assistant')),
    content     TEXT NOT NULL,                        -- 消息正文
    asset_uids  JSONB NOT NULL DEFAULT '[]',          -- 消息引用的商品图 uid 数组（designkit_assets.uid）
    request_id  VARCHAR(64) NOT NULL DEFAULT '',      -- 关联的计费编号；没有就空串（长度约束同 usage_logs.request_id）
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 按会话拉全量消息（ORDER BY id ASC 正好走这个索引）。
CREATE INDEX IF NOT EXISTS idx_designkit_chat_messages_session
    ON designkit_chat_messages (session_id, id);
