-- designkit 商品图生成模块 —— 全部数据表（12 张）
-- PostgreSQL 15+
--
-- 【为什么编号是 9xxx】
-- 上游 Sub2API 用 001~2xx，我们从 9001 起，避免跟上游撞号。
-- 但迁移是按「文件名字节序」执行的，不是数值序，所以不能假定我们永远排在最后。
-- => 本文件必须自足：自己 CREATE TABLE IF NOT EXISTS，不依赖上游任何一次迁移的产物。
--
-- 【为什么没有 ent schema】
-- backend/ent/ 下有 200+ 个上游生成文件，新增一个 ent schema 跑 go generate 会把它们
-- 全部重写，每次跟版都是巨型冲突。上游自己新增的表（usage_billing_dedup）也是这么干的。
-- 我们的表 = 迁移 SQL + 手写 repository。
--
-- 【为什么不建到上游表的外键】
-- user_id / api_key_id / handled_by 指向上游 users、api_keys，但刻意不写 REFERENCES：
-- 一旦写了，本文件就依赖「users 表已存在」这个上游迁移的产物，跟上面那条自足要求冲突。
-- 上游自己的 batch_image_jobs.user_id 也是裸 BIGINT，没有外键。
-- 我们自建表之间（jobs -> job_items -> images 等）的外键正常建。
--
-- 【这个文件跑过一次之后就永远不能再改】
-- schema_migrations 以文件名为主键存内容的 SHA256，改一个字（包括注释、缩进）
-- 校验和就对不上，整个进程起不来。要改需求一律新加一个编号更大的文件（9002…）。
--
-- 【约定】
-- - 时间列一律 TIMESTAMPTZ；金额列一律 NUMERIC(20,10)；倍率列 NUMERIC(10,4)（照上游）
-- - 对外只暴露 uid（26 位 ULID），不暴露自增主键 id
-- - object_key 是相对 bucket 的路径（含 prefix、不含域名不含签名）；URL 一律运行时生成，绝不入库


-- ============================================================
-- 1. designkit_assets —— 运营上传的原始商品图
-- ============================================================
CREATE TABLE IF NOT EXISTS designkit_assets (
    id              BIGSERIAL PRIMARY KEY,
    uid             VARCHAR(32) NOT NULL UNIQUE,          -- 对外暴露的 ULID（26 位），接口里只出现它
    user_id         BIGINT NOT NULL,                      -- 上游 users.id（无外键，理由见文件头）
    object_key      VARCHAR(512) NOT NULL,                -- 原图在对象存储里的相对路径
    content_type    VARCHAR(128) NOT NULL,                -- image/png、image/jpeg、image/heic …
    byte_size       BIGINT NOT NULL DEFAULT 0,            -- 原图字节数
    width           INT,                                  -- 原图像素宽；读不出来时留空
    height          INT,                                  -- 原图像素高
    sha256          VARCHAR(64),                          -- 原始字节的 SHA256，用来认出「同一个人又传了同一张图」
    origin          VARCHAR(16) NOT NULL DEFAULT 'web',   -- web=运营在界面上传；erp=外部系统走接口上传
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ                           -- 软删除：用户删了就不再列出，但历史任务仍能引用
);

CREATE INDEX IF NOT EXISTS idx_designkit_assets_user_created
    ON designkit_assets (user_id, created_at DESC, id DESC);   -- 列表页 + 游标分页
CREATE INDEX IF NOT EXISTS idx_designkit_assets_user_sha256
    ON designkit_assets (user_id, sha256);                     -- 重复上传去重


-- ============================================================
-- 2. designkit_asset_variants —— 预处理产物（补边到目标比例之后的图）
--
-- 为什么单独一张表：预处理产物是 (ratio, keep_transparency, max_dimension) 三个参数的函数，
-- 同一张原图在 3:4 任务和 16:9 任务里是两个不同文件。早期设计想在 assets 上放一列
-- processed_key，那会互相覆盖或被错误复用，导致出图比例静默失控。
-- ============================================================
CREATE TABLE IF NOT EXISTS designkit_asset_variants (
    id                 BIGSERIAL PRIMARY KEY,
    asset_id           BIGINT NOT NULL REFERENCES designkit_assets(id) ON DELETE CASCADE,
    ratio              VARCHAR(16) NOT NULL,                -- 目标比例串：'1:1' '3:4' '4:3' '16:9' '9:16'
    keep_transparency  BOOLEAN NOT NULL DEFAULT FALSE,      -- FALSE=透明底合成白底；TRUE=保留透明
    max_dimension      INT NOT NULL,                        -- 最长边上限像素。它直接决定出图落 2K 还是 4K 计费档，
                                                            -- 所以必须是显式参数并入库，不能埋在代码常量里
    object_key         VARCHAR(512) NOT NULL,               -- 处理后图片在对象存储里的相对路径
    width              INT,                                 -- 处理后实际像素宽
    height             INT,                                 -- 处理后实际像素高
    content_type       VARCHAR(128) NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 四个参数唯一确定一个产物：同样的参数第二次进来直接复用，不重复调 Python 预处理服务
CREATE UNIQUE INDEX IF NOT EXISTS idx_designkit_asset_variants_uq
    ON designkit_asset_variants (asset_id, ratio, keep_transparency, max_dimension);


-- ============================================================
-- 3. designkit_prompt_categories —— 灵感库分类
-- ============================================================
CREATE TABLE IF NOT EXISTS designkit_prompt_categories (
    id           BIGSERIAL PRIMARY KEY,
    slug         VARCHAR(64) NOT NULL UNIQUE,     -- 稳定的英文标识，同步时靠它认人，不随中文名变化
    name_zh      VARCHAR(128) NOT NULL,           -- 界面默认显示这个
    name_en      VARCHAR(128) NOT NULL DEFAULT '',
    sort_order   INT NOT NULL DEFAULT 0,          -- 界面排序，越小越靠前
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_designkit_prompt_categories_sort
    ON designkit_prompt_categories (sort_order, id);


-- ============================================================
-- 4. designkit_prompts —— 灵感库提示词（约 1.4 万条，每天从 YouMind 开源库同步）
-- ============================================================
CREATE TABLE IF NOT EXISTS designkit_prompts (
    id             BIGSERIAL PRIMARY KEY,
    uid            VARCHAR(32) NOT NULL UNIQUE,
    category_id    BIGINT REFERENCES designkit_prompt_categories(id) ON DELETE SET NULL,
    title          VARCHAR(255) NOT NULL DEFAULT '',
    body           TEXT NOT NULL,                        -- 提示词正文
    variables      JSONB NOT NULL DEFAULT '[]'::jsonb,   -- 正文里的占位变量定义，形如 [{"name":"品类","example":"连衣裙"}]
    preview_url    VARCHAR(1024),                        -- 上游库给的效果预览图地址（外链，仅展示用）
    source         VARCHAR(16) NOT NULL DEFAULT 'youmind', -- youmind=同步来的；user=运营自己存的
    source_ref     VARCHAR(255),                         -- 上游库里的唯一标识；同步去重、判断是新增还是更新全靠它
    owner_user_id  BIGINT,                               -- source='user' 时的归属人；youmind 来的为空
    is_enabled     BOOLEAN NOT NULL DEFAULT TRUE,        -- 关掉之后灵感库不再列出，但历史任务里的快照不受影响
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at     TIMESTAMPTZ
);

-- 同步的幂等键。注意这是「部分唯一索引」，写 ON CONFLICT 时必须把 WHERE 条件一起带上：
--   ON CONFLICT (source_ref) WHERE source_ref IS NOT NULL AND source_ref <> '' DO UPDATE …
CREATE UNIQUE INDEX IF NOT EXISTS idx_designkit_prompts_source_ref_uq
    ON designkit_prompts (source_ref)
    WHERE source_ref IS NOT NULL AND source_ref <> '';

CREATE INDEX IF NOT EXISTS idx_designkit_prompts_category
    ON designkit_prompts (category_id, id);
CREATE INDEX IF NOT EXISTS idx_designkit_prompts_owner
    ON designkit_prompts (owner_user_id, id)
    WHERE owner_user_id IS NOT NULL;

-- 关键词搜索一律用 ILIKE（PostgreSQL 的 LIKE 区分大小写）。
-- ILIKE '%词%' 走不了普通索引，只有 pg_trgm 的 GIN 索引能加速。
-- 照抄上游 065_add_search_trgm_indexes.sql 的写法：扩展装不上就跳过，不让整个迁移失败
-- （没有索引也能用，1.4 万行全表扫描是毫秒级）。
DO $$
BEGIN
    BEGIN
        CREATE EXTENSION IF NOT EXISTS pg_trgm;
    EXCEPTION
        WHEN OTHERS THEN
            RAISE NOTICE 'pg_trgm extension not created: %', SQLERRM;
    END;

    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm') THEN
        EXECUTE 'CREATE INDEX IF NOT EXISTS idx_designkit_prompts_title_trgm
                 ON designkit_prompts USING gin (title gin_trgm_ops)';
        EXECUTE 'CREATE INDEX IF NOT EXISTS idx_designkit_prompts_body_trgm
                 ON designkit_prompts USING gin (body gin_trgm_ops)';
    ELSE
        RAISE NOTICE 'skip designkit trigram indexes because pg_trgm is unavailable';
    END IF;
END
$$;


-- ============================================================
-- 5. designkit_jobs —— 一次提交（一个批次）
--
-- api_key_id 为什么是 NOT NULL：出图走的是上游网关 Images()，它第一件事就是从上下文
-- 取 API Key，取不到直接 401；usage_logs.api_key_id 也是 NOT NULL + 外键。
-- 所以每个运营用户首次进工作台时，服务端会自动给他建一把内部专用 Key（界面不暴露），
-- 浏览器端的任务也记在那把 Key 上。没有「浏览器端为 NULL」这种情况。
-- ============================================================
CREATE TABLE IF NOT EXISTS designkit_jobs (
    id                        BIGSERIAL PRIMARY KEY,
    uid                       VARCHAR(32) NOT NULL UNIQUE,        -- 对外的任务号（ULID）
    user_id                   BIGINT NOT NULL,                    -- 上游 users.id
    api_key_id                BIGINT NOT NULL,                    -- 上游 api_keys.id，见上方说明，绝不能为空
    origin                    VARCHAR(16) NOT NULL DEFAULT 'web', -- web=运营在界面提交；erp=外部系统提交
    name                      VARCHAR(255) NOT NULL DEFAULT '',   -- 运营给这批起的名字，可为空串
    status                    VARCHAR(32) NOT NULL DEFAULT 'created',
        -- 状态取值：created / holding / running / settling /
        --           succeeded / partially_failed / failed / cancelled
        -- 必须走白名单迁移函数，不许随手 UPDATE status
    ratio                     VARCHAR(16) NOT NULL,               -- 这一批的出图比例，整批统一
    model                     VARCHAR(128) NOT NULL,              -- 出图模型名，例如 gpt-image-2
    item_count                INT NOT NULL DEFAULT 0,             -- 展开后的总张数 = 商品图数 × 提示词数
    success_count             INT NOT NULL DEFAULT 0,             -- 出成功的张数（一律 SET x = x + 1 原子自增）
    fail_count                INT NOT NULL DEFAULT 0,             -- 失败的张数
    cancelled_count           INT NOT NULL DEFAULT 0,             -- 「还没开始就被停掉」的张数；已在跑的不算在这
    idempotency_key           VARCHAR(128),                       -- 客户端送来的 Idempotency-Key（浏览器端也必须带）
    callback_url              VARCHAR(1024),                      -- 任务完成回调地址，主要给 ERP 用；空=不回调
    currency                  VARCHAR(16) NOT NULL DEFAULT 'USD', -- 金额一律美元，不做汇率换算
    estimated_cost            NUMERIC(20,10) NOT NULL DEFAULT 0,  -- 提交时的预估花费（冻结额按它算）
    actual_cost               NUMERIC(20,10),                     -- 结算后的实际花费 = SUM(job_items.billed_cost)，
                                                                  -- 含所有重试；未结算时为空。禁止用「张数 × 单价」算

    -- 定价快照：提交那一刻的价格参数存下来，之后管理员改分组定价也不影响这批的账
    base_unit_price           NUMERIC(20,10) NOT NULL DEFAULT 0,  -- 快照：基础单价
    group_rate_multiplier     NUMERIC(10,4) NOT NULL DEFAULT 1.0, -- 快照：分组/用户专属倍率
    account_rate_multiplier   NUMERIC(10,4) NOT NULL DEFAULT 1.0, -- 快照：上游账号倍率
    billing_mode              VARCHAR(32) NOT NULL DEFAULT '',    -- 快照：计价方式。解析成 'token' 就是配错了，
                                                                  -- 按次价会完全不参与，报价必然失准
    pricing_tier              VARCHAR(16) NOT NULL DEFAULT '',    -- 快照：预估时假定的档位 1K/2K/4K
    price_source              VARCHAR(32) NOT NULL DEFAULT '',    -- 快照：价格来自 channel/group/default 哪一层
    pricing_snapshot_version  INT NOT NULL DEFAULT 0,             -- 快照结构版本号，以后改结构靠它区分

    revision                  INT NOT NULL DEFAULT 0,             -- 每次「重试」把任务从终态拉回 running 时 +1
    last_error_code           VARCHAR(64),                        -- 整批失败的原因码，例如 DK-STALE（进程重启导致中断）
    last_error_message        TEXT,                               -- 上游原文，只给管理员看，界面不显示
    heartbeat_at              TIMESTAMPTZ,                        -- worker 每 30 秒续一次；超过 10 分钟没动静判定为僵尸
    cancel_requested_at       TIMESTAMPTZ,                        -- 运营点了「停止排队」的时刻
    expires_at                TIMESTAMPTZ,                        -- 预留：图片过期时间。当前不自动删，字段先建好
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at                TIMESTAMPTZ,                        -- 第一张开始跑的时刻
    finished_at               TIMESTAMPTZ,                        -- 所有 item 到终态的时刻（终态判定的守卫列）
    settled_at                TIMESTAMPTZ,                        -- 账算清的时刻；非空的任务拒绝重试
    user_deleted_at           TIMESTAMPTZ                         -- 运营在界面删除任务（软删），账和图都还在
);

CREATE INDEX IF NOT EXISTS idx_designkit_jobs_user_created
    ON designkit_jobs (user_id, created_at DESC, id DESC);  -- 列表页 + ERP 游标分页（created_at + id 复合游标）
CREATE INDEX IF NOT EXISTS idx_designkit_jobs_status
    ON designkit_jobs (status);
CREATE INDEX IF NOT EXISTS idx_designkit_jobs_status_heartbeat
    ON designkit_jobs (status, heartbeat_at);               -- 僵尸任务巡检：status IN ('holding','running') 且心跳过期
-- 幂等键这里刻意是「普通索引」不是唯一索引：幂等由上游的幂等协调器负责
-- （scope 里揉进了 user_id）。DB 层再加一把唯一约束会跟协调器的 24 小时窗口打架——
-- 24 小时后 ERP 复用同一个 key 下新单，唯一索引会把新单错认成老单，直接不出图。
-- 所以这里只留一个查询用的索引，不要写 ON CONFLICT (user_id, idempotency_key)。
CREATE INDEX IF NOT EXISTS idx_designkit_jobs_idempotency
    ON designkit_jobs (user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';


-- ============================================================
-- 6. designkit_job_items —— 每一个「商品图 × 提示词」，也就是每一张要出的图
--
-- 展开顺序固定：商品图外层、提示词内层。
--   图1×词1 -> seq 1，图1×词2 -> seq 2，图2×词1 -> seq 3 …
-- 运营是按商品思考的。所有查询必须显式 ORDER BY seq ASC（有专门测试守着），
-- 因为「按序号取第几张图」是发给 ERP 的对外契约。
-- ============================================================
CREATE TABLE IF NOT EXISTS designkit_job_items (
    id                  BIGSERIAL PRIMARY KEY,
    job_id              BIGINT NOT NULL REFERENCES designkit_jobs(id) ON DELETE CASCADE,
    seq                 INT NOT NULL,           -- 批次内序号，从 1 开始（对外文档说的「第 1 张」就是它）
    asset_id            BIGINT,                 -- 用的哪张原图（designkit_assets.id，无外键：原图删了历史照样能看）
    variant_id          BIGINT,                 -- 实际发给网关的是哪个预处理产物（designkit_asset_variants.id）
    prompt_id           BIGINT,                 -- 来自灵感库的哪条提示词；运营手打的为空
    prompt_text         TEXT NOT NULL,          -- 提交那一刻的提示词原文快照。
                                                -- 灵感库里的词以后被改措辞或删掉，历史记录仍显示当时用的原文
    status              VARCHAR(32) NOT NULL DEFAULT 'pending',
        -- 状态取值：pending / running / succeeded / failed / cancelled
    attempt_count       INT NOT NULL DEFAULT 0, -- 已经尝试过几次。在「领取」那一刻就 +1，不是成功后才加——
                                                -- 否则进程崩一次，这一张会被永远重试下去
    max_attempts        INT NOT NULL DEFAULT 3, -- 累计最多试几次；运营点重试也算在内
    last_error_code     VARCHAR(64),            -- 我方错误码，界面显示中文文案时带上它，方便运营截图报障
    last_error_message  TEXT,                   -- 上游返回的英文原文，只给管理员看

    -- 计费
    billed_cost         NUMERIC(20,10),         -- 这一张实际扣了多少钱（从 usage_logs 反查回填）
    billing_request_id  VARCHAR(64),            -- 存「带 client: 前缀的完整值」，如 client:dki:<job_uid>:<seq>:<attempt>，
                                                -- 否则拿它去 usage_logs 一条也查不到。
                                                -- 规则：同一个 (seq, attempt) 无论重投几次都用同一个值（幂等，不重复扣）；
                                                --       attempt +1 就换新值（该扣就得扣）
    billing_mode        VARCHAR(32),            -- 上游实际用的计价方式
    billing_tier        VARCHAR(16),            -- 实际落的计费档 1K/2K/4K（由上游返回图片的像素决定，不是我们请求的 size）

    -- 队列租约（多个 worker 抢同一批任务时靠这三列不打架）
    worker_id           VARCHAR(64),            -- 当前持有租约的 worker 标识。
                                                -- 写回结果必须带 WHERE id=? AND worker_id=? AND status='running'，
                                                -- 影响行数不等于 1 就丢弃结果（说明租约已被别人抢走）
    lease_expires_at    TIMESTAMPTZ,            -- 租约到期时间（默认 180 秒，每 60 秒续）。
                                                -- 过期还停在 running 的就是僵尸，会被别的 worker 回收重跑
    available_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                                                -- 最早可被领取的时间。失败后退避重试就是把它往后推

    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at          TIMESTAMPTZ,
    finished_at         TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_designkit_job_items_job_seq_uq
    ON designkit_job_items (job_id, seq);
-- 领取待办：WHERE status='pending' AND available_at <= NOW() ORDER BY job_id, seq
CREATE INDEX IF NOT EXISTS idx_designkit_job_items_queue
    ON designkit_job_items (status, available_at, job_id, seq);
-- 僵尸回收：WHERE status='running' AND lease_expires_at < NOW()
CREATE INDEX IF NOT EXISTS idx_designkit_job_items_lease
    ON designkit_job_items (status, lease_expires_at);
-- 结算时按 billing_request_id 反查对账
CREATE INDEX IF NOT EXISTS idx_designkit_job_items_billing_req
    ON designkit_job_items (billing_request_id)
    WHERE billing_request_id IS NOT NULL;


-- ============================================================
-- 7. designkit_images —— 出好的结果图
--
-- object_key 格式定死：designkit/<YYYY>/<MM>/<DD>/<job_uid>-<seq>-<attempt>-<image_index>.png
-- （相对 bucket、含 prefix、不含域名不含签名）。URL 一律运行时生成，绝不入库——
-- 上游对象存储默认签发 24 小时失效的链接，存进库过期后拼不回来。
-- ============================================================
CREATE TABLE IF NOT EXISTS designkit_images (
    id            BIGSERIAL PRIMARY KEY,
    uid           VARCHAR(32) NOT NULL UNIQUE,       -- 对外的图片编号（ULID）
    job_id        BIGINT NOT NULL REFERENCES designkit_jobs(id) ON DELETE CASCADE,
    item_id       BIGINT NOT NULL REFERENCES designkit_job_items(id) ON DELETE CASCADE,
    attempt       INT NOT NULL DEFAULT 1,            -- 这张图是第几次尝试出的（跟 job_items.attempt_count 对应）
    image_index   INT NOT NULL DEFAULT 1,            -- 同一次尝试里返回的第几张，从 1 开始。
                                                     -- 自建网关一次会不会返回多张还没实测，先留着，成本为零
    is_current    BOOLEAN NOT NULL DEFAULT TRUE,     -- 重试出了新图后，旧图置 FALSE；界面/接口默认只给当前这版
    object_key    VARCHAR(512) NOT NULL,
    content_type  VARCHAR(128) NOT NULL DEFAULT 'image/png',
    byte_size     BIGINT NOT NULL DEFAULT 0,
    width         INT,                               -- 上游实际返回的像素宽——计费落哪档由它决定，必须记
    height        INT,
    billing_tier  VARCHAR(16),                       -- 由上面的像素分出来的档：最长边 <=1024 是 1K，<=2048 是 2K，更大是 4K
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ,                       -- 软删除
    expires_at    TIMESTAMPTZ                        -- 预留：图片永久保留，字段先建好，哪天硬盘紧了开开关就能清
);

CREATE INDEX IF NOT EXISTS idx_designkit_images_item
    ON designkit_images (item_id, attempt, image_index);
CREATE INDEX IF NOT EXISTS idx_designkit_images_job
    ON designkit_images (job_id, id);
CREATE INDEX IF NOT EXISTS idx_designkit_images_expires
    ON designkit_images (expires_at)
    WHERE expires_at IS NOT NULL;
-- 「每个 item 的每个 image_index 只能有一张当前图」。
-- 注意唯一键里带上了 image_index：如果网关某次真的一口气返回两张图，
-- 少了这一列第二张就会插不进去——钱花了图却丢了。
CREATE UNIQUE INDEX IF NOT EXISTS idx_designkit_images_current_uq
    ON designkit_images (item_id, image_index)
    WHERE is_current AND deleted_at IS NULL;


-- ============================================================
-- 8. designkit_holds —— 我们自己的冻结账
--
-- 为什么不用上游自带的冻结（Reserve/Capture/Release）：四条全部验实、四条全部致命——
--   1. 上游 Reserve 是真的从 users.balance 里把钱划走的；
--   2. 上游每张图出图前的准入判据就是「balance <= 0」。余额 100 冻结 100，
--      本批次每一张都会被判余额不足，一张也出不来；
--   3. 上游 Release 的防幻影守卫把 hold 键硬编码成 batch_image_hold: 前缀，
--      我们的冻结它查不到，会「打一行日志、返回 nil」，钱永久卡在 frozen_balance 里，无任何报错；
--   4. 上游 Capture 在「实际额 > 冻结额」时直接返错回滚，而实测计费档会向上漂，
--      实际额大于冻结额是常态。
-- 所以：users.balance 全程只由上游扣一次，我们只在自己这张表上记账。
--   可用额 = users.balance - SUM(designkit_holds.amount WHERE user_id=? AND status='open')
-- 提交时在一个事务里 SELECT users.balance FOR UPDATE 再算可用额，
-- 那条 FOR UPDATE 就是全部的并发防护——它把上游「50 张并发全部放行」的窗口关死了。
-- ============================================================
CREATE TABLE IF NOT EXISTS designkit_holds (
    id          BIGSERIAL PRIMARY KEY,
    job_id      BIGINT NOT NULL UNIQUE REFERENCES designkit_jobs(id) ON DELETE CASCADE,
                            -- 一个任务一行，重试时对同一行做 ON CONFLICT (job_id) DO UPDATE 把额度加回去，
                            -- 不要再插一行
    user_id     BIGINT NOT NULL,
    amount      NUMERIC(20,10) NOT NULL DEFAULT 0,  -- 当前还冻着多少。每张图计费落地后按张递减
    status      VARCHAR(16) NOT NULL DEFAULT 'open',
        -- open=还占着可用额；settled=已按实际花费结清；released=剩余部分已释放
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_designkit_holds_user_status
    ON designkit_holds (user_id, status);   -- 算可用额时用


-- ============================================================
-- 9. designkit_billing_alerts —— 实际花费超出预估的告警
--
-- 结算时绝不能因为「实际 > 预估」就报错回滚——钱已经被上游扣走了，报错只会让任务卡死。
-- 改成分级记录：超 1.2 倍记一条 warn，超 2 倍记 critical 并额外告警。
-- ============================================================
CREATE TABLE IF NOT EXISTS designkit_billing_alerts (
    id          BIGSERIAL PRIMARY KEY,
    job_id      BIGINT NOT NULL REFERENCES designkit_jobs(id) ON DELETE CASCADE,
    estimated   NUMERIC(20,10) NOT NULL DEFAULT 0,   -- 提交时的预估花费
    actual      NUMERIC(20,10) NOT NULL DEFAULT 0,   -- 结算后的实际花费
    ratio_over  NUMERIC(10,4) NOT NULL DEFAULT 0,    -- actual / estimated，例如 1.35
    level       VARCHAR(16) NOT NULL DEFAULT 'warn', -- warn=超 1.2 倍；critical=超 2 倍
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_designkit_billing_alerts_job
    ON designkit_billing_alerts (job_id);
CREATE INDEX IF NOT EXISTS idx_designkit_billing_alerts_created
    ON designkit_billing_alerts (created_at DESC, id DESC);


-- ============================================================
-- 10. designkit_quota_requests —— 运营点「申请额度」留下的待办
--
-- 为什么要有：充值和兑换码菜单对普通用户是隐藏的，运营看到「余额不足」就是死路一条，
-- 而管理员这边没有任何机制会主动知道有人卡住了。
-- ============================================================
CREATE TABLE IF NOT EXISTS designkit_quota_requests (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL,                       -- 申请人（上游 users.id）
    note        TEXT,                                  -- 运营自己填的说明，可为空
    status      VARCHAR(16) NOT NULL DEFAULT 'pending',-- pending=待处理；handled=已加额度；rejected=驳回
    handled_by  BIGINT,                                -- 处理人（上游 users.id）
    handled_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_designkit_quota_requests_status
    ON designkit_quota_requests (status, created_at);
-- 同一个人同时只能有一条待处理，防止连点刷屏把管理员后台淹掉
CREATE UNIQUE INDEX IF NOT EXISTS idx_designkit_quota_requests_pending_uq
    ON designkit_quota_requests (user_id)
    WHERE status = 'pending';


-- ============================================================
-- 11. designkit_sync_runs —— 灵感库每次同步的记录
--
-- 手动同步和自动同步共用同一把 PostgreSQL advisory lock（pg_try_advisory_lock），
-- 不要用应用内 mutex——多副本部署时应用内锁根本不起作用。
-- （老系统就是各用一把锁，并发把 1.4 万条提示词写重复了。）
-- ============================================================
CREATE TABLE IF NOT EXISTS designkit_sync_runs (
    id           BIGSERIAL PRIMARY KEY,
    kind         VARCHAR(16) NOT NULL DEFAULT 'auto',    -- manual=管理员点的；auto=定时跑的
    status       VARCHAR(16) NOT NULL DEFAULT 'running', -- running / succeeded / failed / skipped
                                                         -- skipped=没抢到锁，说明已经有一次在跑
    started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at  TIMESTAMPTZ,
    fetched      INT NOT NULL DEFAULT 0,   -- 从上游库拉回来多少条
    inserted     INT NOT NULL DEFAULT 0,   -- 新增多少条
    updated      INT NOT NULL DEFAULT 0,   -- 更新多少条
    skipped      INT NOT NULL DEFAULT 0,   -- 内容没变、跳过多少条
    error        TEXT                      -- 失败原因；成功时为空
);

CREATE INDEX IF NOT EXISTS idx_designkit_sync_runs_started
    ON designkit_sync_runs (started_at DESC, id DESC);   -- 「最近一次同步」查这个
CREATE INDEX IF NOT EXISTS idx_designkit_sync_runs_status
    ON designkit_sync_runs (status, started_at DESC);


-- ============================================================
-- 12. designkit_settings —— designkit 自己的配置
--
-- 刻意不碰上游的 settings 表，避免跟版时撞键、也避免我们的键被上游后台页面误改。
-- ============================================================
CREATE TABLE IF NOT EXISTS designkit_settings (
    key         VARCHAR(64) PRIMARY KEY,                    -- 配置项名字
    value       JSONB NOT NULL DEFAULT '{}'::jsonb,         -- 值。标量也用 JSONB 存（数字直接写 4，字符串写 "abc"）
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 初始默认值。ON CONFLICT DO NOTHING 保证重复执行不会覆盖管理员改过的值。
-- 注意：出图单价刻意没有放进来——每个比例实际出多大像素、落 1K/2K/4K 哪一档，
-- 必须真出图实测才知道。测出来之前界面上标「价格待确认」，不许显示任何猜的单价。
INSERT INTO designkit_settings (key, value) VALUES
    ('ratios',               '["1:1","3:4","4:3","16:9","9:16"]'::jsonb),  -- 可选出图比例，界面按这个顺序显示
    ('model',                '"gpt-image-2"'::jsonb),                      -- 默认出图模型
    ('max_batch_items',      '50'::jsonb),                                 -- 单次批量最多多少张，超过直接拒绝提交
    ('item_timeout_seconds', '180'::jsonb),                                -- 单张图的超时秒数（队列租约也按这个量级）
    ('max_dimension',        '2048'::jsonb),                               -- 预处理后最长边上限像素，直接影响计费档
    ('worker_concurrency',   '4'::jsonb),                                  -- 我们自己的出图并发上限。
                                                                           -- 上游每用户默认 5 并发，这里留一格给运营的单张操作
    ('admin_contact',        '""'::jsonb)                                  -- 管理员联系方式，余额不足的提示里会显示，部署时填
ON CONFLICT (key) DO NOTHING;
