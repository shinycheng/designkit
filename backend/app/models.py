import uuid
from datetime import datetime
from typing import Optional

from sqlalchemy import (
    JSON,
    Boolean,
    DateTime,
    Float,
    ForeignKey,
    Integer,
    String,
    Text,
)
from sqlalchemy.orm import Mapped, mapped_column, relationship

from .database import Base


def _uuid() -> str:
    return uuid.uuid4().hex


def utcnow() -> datetime:
    return datetime.utcnow()


class User(Base):
    __tablename__ = "users"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    username: Mapped[str] = mapped_column(String(64), unique=True, index=True)
    password_hash: Mapped[str] = mapped_column(String(256))
    display_name: Mapped[str] = mapped_column(String(64), default="")
    role: Mapped[str] = mapped_column(String(16), default="member")  # admin / member
    is_active: Mapped[bool] = mapped_column(Boolean, default=True)
    # 仍是初始默认密码时为 True，登录后前端强制改密
    must_change_password: Mapped[bool] = mapped_column(Boolean, default=False)
    # 令牌版本：改密码/停用时 +1，使已签发的旧 JWT 立即失效
    token_version: Mapped[int] = mapped_column(Integer, default=0)
    # 预留：转对外产品时的每月生成额度（None = 不限制）
    monthly_quota: Mapped[Optional[int]] = mapped_column(Integer, nullable=True)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow)


# user_gateway_accounts.state 的取值。用字符串而不是布尔，是因为「开通网关账号」
# 天然是个多步骤过程（建号 → 发 Key → 可用），中途会卡在哪一步必须能看出来；
# 用 True/False 只会逼着后面再加第二个、第三个布尔字段，然后互相矛盾。
GATEWAY_ACCOUNT_STATES = (
    "manual",        # 管理员手工把 Key 填进来的，没走自动开通流程
    "pending",       # 已提交开通请求，还没拿到结果
    "user_created",  # 网关那边账号建好了，但 Key 还没发下来
    "key_issued",    # Key 拿到了，还没验证过能不能真的生图
    "active",        # 可以正常用
    "failed",        # 开通失败，失败原因在 last_error 里
    "disabled",      # 管理员主动停用
)


class UserGatewayAccount(Base):
    """每个用户在生图网关（自建 Sub2API）上的账号与专属 Key。

    为什么要单开一张表，而不是给 app_settings 加个用户维度：app_settings 的主键
    就是设置名本身（见本文件末尾的 AppSetting），一个 openai_api_key 全库只有一行，
    「每人一把 Key」在那张表里物理上存不下。而 base_url / image_model / text_model
    这些是全体共用同一个网关的，只有 Key 因人而异——所以拆出来最干净。

    这张表是新表，由 create_all 自动建出、带齐全部列，对存量数据零影响。
    所以这里**故意一次把以后要用的列也建齐**（remote_* 是代客开通账号用的，
    balance_* 是余额快照用的，眼下都还没人读）：等以后再加就得走 ALTER TABLE
    那条路，而那条路在本项目里是有类型陷阱的（详见 migrations.py 文件头）。
    新表随时能建，老表加列才有风险，这是本项目的一条硬规矩。

    安全约定：api_key_enc / remote_password_enc 里存的**永远是密文**
    （由 services/secrets_box.py 加解密），任何时候都不许把明文写进这两列。
    api_key_tail 只留 Key 的末尾几位，用途仅限于后端排错时对上是哪一把，
    **任何接口都不要把它返回给前端**——多个后台把同一把 Key 的后几位摆在一起，
    就足够把人、Key、账单三边对上号了。
    """

    __tablename__ = "user_gateway_accounts"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    # 一个人只能有一个网关账号：唯一索引建在这张全新的空表上，不可能撞上存量重复值
    user_id: Mapped[int] = mapped_column(
        ForeignKey("users.id", ondelete="CASCADE"), unique=True, index=True
    )
    provider: Mapped[str] = mapped_column(String(16), default="sub2api")
    # 取值见上面的 GATEWAY_ACCOUNT_STATES
    state: Mapped[str] = mapped_column(String(16), default="manual")
    api_key_enc: Mapped[Optional[str]] = mapped_column(Text, nullable=True)  # 密文，绝不明文
    api_key_tail: Mapped[str] = mapped_column(String(8), default="")  # 仅后端排错用
    remote_user_id: Mapped[Optional[str]] = mapped_column(String(64), nullable=True)
    remote_email: Mapped[Optional[str]] = mapped_column(String(128), nullable=True)
    remote_password_enc: Mapped[Optional[str]] = mapped_column(Text, nullable=True)  # 密文
    attempts: Mapped[int] = mapped_column(Integer, default=0)  # 自动开通试了几次
    last_error: Mapped[str] = mapped_column(Text, default="")  # 最后一次失败的原因（人话）
    balance_usd: Mapped[Optional[float]] = mapped_column(Float, nullable=True)  # 余额快照
    balance_synced_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow)
    updated_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow, onupdate=utcnow)


class ApiKey(Base):
    """对外 API（ERP 等平台对接）使用的密钥。"""

    __tablename__ = "api_keys"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    # 这把 Key 属于谁。没有它，ERP 建的任务 job.user_id 恒为 NULL，
    # 「按人取网关 Key、按人算账」整条链路在数据模型层就断了。
    # 可空是为了兼容存量 Key（升级后由启动期回填补上归属）；删用户时置空而不是
    # 连带删 Key —— ERP 那头还在用同一把 Key 调接口，突然 401 没人查得出原因。
    # 注意：老库是走 ALTER TABLE 补的这一列，只有 INTEGER、**没有外键约束**
    #（见 migrations.py），所以归属校验一律在应用层做，不要指望数据库兜底。
    user_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("users.id", ondelete="SET NULL"), nullable=True, index=True
    )
    name: Mapped[str] = mapped_column(String(64))  # 例如「XX ERP 生产环境」
    key_prefix: Mapped[str] = mapped_column(String(16))  # 展示用前缀 dk_xxxx
    key_hash: Mapped[str] = mapped_column(String(64), unique=True, index=True)  # sha256
    webhook_secret: Mapped[str] = mapped_column(String(64))  # 回调签名密钥
    is_active: Mapped[bool] = mapped_column(Boolean, default=True)
    monthly_quota: Mapped[Optional[int]] = mapped_column(Integer, nullable=True)  # None=不限
    used_total: Mapped[int] = mapped_column(Integer, default=0)
    used_month: Mapped[int] = mapped_column(Integer, default=0)  # 当月已用次数
    used_month_key: Mapped[str] = mapped_column(String(8), default="")  # 如 202608
    last_used_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow)


class PromptCategory(Base):
    __tablename__ = "prompt_categories"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    name: Mapped[str] = mapped_column(String(64), unique=True)
    sort: Mapped[int] = mapped_column(Integer, default=0)


class PromptTemplate(Base):
    """提示词模板。prompt_template 中可用 {变量名} 占位，由 variables 定义。

    source 说明：
    - user     用户自己创建/采用的正式模板（生成页可见，取决于 is_enabled）
    - youmind  从 YouMind 开源库同步来的灵感库条目（默认 is_enabled=False，
               只出现在「灵感库」页；采用时会复制成一条 source=user 的新模板）
    """

    __tablename__ = "prompt_templates"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    name: Mapped[str] = mapped_column(String(128))
    source: Mapped[str] = mapped_column(String(16), default="user", index=True)
    # 外部来源标识，如 youmind:29918；同步时按它幂等更新，采用的副本也带上它用于标记「已采用」
    source_ref: Mapped[Optional[str]] = mapped_column(String(64), nullable=True, index=True)
    # 上游的全部分类标签（JSON 数组）。一条提示词常同时属于多个分类，
    # 只记一个会让「电商主图」这类分类看起来几乎是空的（实测 416 条只剩 18 条）
    source_slugs: Mapped[Optional[str]] = mapped_column(Text, nullable=True)
    # 模板属于谁：NULL = 平台公共库（存量 14573 条全是 NULL，升级后行为完全不变）。
    # 眼下只写不读、还没有任何地方按它过滤，先加列是因为「加列」这件事在老库上
    # 有成本、在新库上没有，早加早省事；等到真要分库时才加，就得赶在功能上线前
    # 抢着做一次有风险的迁移。
    # 故意**不加外键约束**：模板是内容资产，删一个用户不该把他建过的模板一起带走，
    # 也不该因为外键的存在让「删用户」变成一个要小心翼翼评估级联的动作。
    # 归属校验在应用层做（services/jobs.py 取模板那一处）。
    owner_user_id: Mapped[Optional[int]] = mapped_column(Integer, nullable=True)
    category_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("prompt_categories.id", ondelete="SET NULL"), nullable=True
    )
    description: Mapped[str] = mapped_column(Text, default="")
    prompt_template: Mapped[str] = mapped_column(Text)
    # [{"name":"scene","label":"场景","type":"text|select","options":[...],"default":"","required":true}]
    variables: Mapped[list] = mapped_column(JSON, default=list)
    # {"size":"1024x1024","n":1,"quality":"high"}
    default_params: Mapped[dict] = mapped_column(JSON, default=dict)
    requires_input_image: Mapped[bool] = mapped_column(Boolean, default=True)
    # 加索引是给「按图片路径反查这张图属于谁」用的：/files 鉴权路由每放行一张缩略图
    # 都要来查一次，而这张表有 14573 行，不加索引等于每张图都全表扫一遍，
    # 历史页一屏几十张缩略图会卡到用户以为系统坏了
    thumbnail_path: Mapped[Optional[str]] = mapped_column(
        String(256), nullable=True, index=True
    )
    is_enabled: Mapped[bool] = mapped_column(Boolean, default=True)
    sort: Mapped[int] = mapped_column(Integer, default=0)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow)
    updated_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow, onupdate=utcnow)

    category = relationship("PromptCategory", lazy="joined")


class Upload(Base):
    __tablename__ = "uploads"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    user_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("users.id", ondelete="SET NULL"), nullable=True
    )
    api_key_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("api_keys.id", ondelete="SET NULL"), nullable=True
    )
    original_name: Mapped[str] = mapped_column(String(256), default="")
    # 相对 data 目录，如 uploads/202608/xx.png。
    # 加索引同样是给 /files 鉴权路由反查归属用的：这张表有十几万行历史记录，
    # 每次取图都全表扫的话，页面会慢到没法用（详见 prompt_templates.thumbnail_path）
    path: Mapped[str] = mapped_column(String(256), index=True)
    width: Mapped[int] = mapped_column(Integer, default=0)
    height: Mapped[int] = mapped_column(Integer, default=0)
    size_bytes: Mapped[int] = mapped_column(Integer, default=0)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow)


JOB_STATUSES = ("pending", "processing", "succeeded", "failed")


class GenerationJob(Base):
    __tablename__ = "generation_jobs"

    id: Mapped[str] = mapped_column(String(32), primary_key=True, default=_uuid)
    source: Mapped[str] = mapped_column(String(8), default="web")  # web / api
    user_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("users.id", ondelete="SET NULL"), nullable=True
    )
    api_key_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("api_keys.id", ondelete="SET NULL"), nullable=True
    )
    template_id: Mapped[Optional[int]] = mapped_column(Integer, nullable=True)
    template_name: Mapped[str] = mapped_column(String(128), default="")  # 快照，模板删了也能看
    prompt_final: Mapped[str] = mapped_column(Text)  # 模板渲染 + 变量 + 补充要求（配置产物）
    # 实际发给生图模型的提示词。开启 AI 合成时是「看图重写」后的版本，否则同 prompt_final
    prompt_sent: Mapped[Optional[str]] = mapped_column(Text, nullable=True)
    params: Mapped[dict] = mapped_column(JSON, default=dict)  # {"size","n","quality"}
    input_paths: Mapped[list] = mapped_column(JSON, default=list)  # 商品图相对路径列表
    status: Mapped[str] = mapped_column(String(16), default="pending", index=True)
    error: Mapped[str] = mapped_column(Text, default="")
    attempts: Mapped[int] = mapped_column(Integer, default=0)
    next_attempt_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    callback_url: Mapped[Optional[str]] = mapped_column(String(512), nullable=True)
    external_ref: Mapped[Optional[str]] = mapped_column(String(128), nullable=True)  # ERP 单号等
    webhook_status: Mapped[str] = mapped_column(String(64), default="")
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow, index=True)
    started_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    finished_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)

    # order_by 是对外契约的一部分，不要删：
    # docs/erp-api.md 承诺「直取图端点的 index 与查询任务返回的 images 数组下标
    # 一一对应」。查询接口走的是这里的 job.images，直取图端点走的是
    # routers/v1.py 里显式的 order_by(GeneratedImage.id.asc())——两边必须是同一个
    # 顺序。不写 order_by 时顺序由数据库决定（SQLite 和 PostgreSQL 不保证一致），
    # 对接方按 index=1 取回来的就可能不是它在查询结果里看到的第 2 张图，
    # **而且两边都不会报错**，出了问题也查不到这里。
    # id 升序 = 入库顺序 = 生成时的第几张（storage 存盘用的也是同一个序号）。
    images = relationship(
        "GeneratedImage",
        back_populates="job",
        cascade="all, delete-orphan",
        lazy="selectin",
        order_by="GeneratedImage.id",
    )


class GeneratedImage(Base):
    __tablename__ = "generated_images"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    job_id: Mapped[str] = mapped_column(
        ForeignKey("generation_jobs.id", ondelete="CASCADE"), index=True
    )
    path: Mapped[str] = mapped_column(String(256))
    thumb_path: Mapped[Optional[str]] = mapped_column(String(256), nullable=True)
    width: Mapped[int] = mapped_column(Integer, default=0)
    height: Mapped[int] = mapped_column(Integer, default=0)
    format: Mapped[str] = mapped_column(String(8), default="png")
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow)

    job = relationship("GenerationJob", back_populates="images")


class AppSetting(Base):
    __tablename__ = "app_settings"

    key: Mapped[str] = mapped_column(String(64), primary_key=True)
    value: Mapped[dict] = mapped_column(JSON)  # {"v": ...} 包一层，兼容任意类型


class SyncState(Base):
    """后台定时任务的执行状态与分布式锁。

    锁用「带条件的 UPDATE + rowcount」实现，SQLite 和 PostgreSQL 都适用：
    多进程部署（gunicorn/多容器）时只有抢到锁的那个进程会真正执行同步，
    lock_until 到期自动释放，避免进程崩溃后锁永久悬挂。
    """

    __tablename__ = "sync_state"

    name: Mapped[str] = mapped_column(String(64), primary_key=True)  # 如 inspiration
    lock_until: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    # 持锁者令牌：释放时校验归属，避免清掉别人的锁
    lock_owner: Mapped[Optional[str]] = mapped_column(String(64), nullable=True)
    last_started_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    last_success_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    last_status: Mapped[str] = mapped_column(String(16), default="idle")  # idle/running/success/failed
    last_message: Mapped[str] = mapped_column(Text, default="")
    consecutive_failures: Mapped[int] = mapped_column(Integer, default=0)
