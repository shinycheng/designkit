import uuid
from datetime import datetime
from typing import Optional

from sqlalchemy import (
    JSON,
    Boolean,
    DateTime,
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


class ApiKey(Base):
    """对外 API（ERP 等平台对接）使用的密钥。"""

    __tablename__ = "api_keys"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
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
    thumbnail_path: Mapped[Optional[str]] = mapped_column(String(256), nullable=True)
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
    path: Mapped[str] = mapped_column(String(256))  # 相对 data 目录，如 uploads/202608/xx.png
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

    images = relationship(
        "GeneratedImage", back_populates="job", cascade="all, delete-orphan", lazy="selectin"
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
