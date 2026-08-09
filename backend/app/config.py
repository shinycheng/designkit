"""全局路径与基础配置。

优先级：数据库设置(可在界面修改) > .env 环境变量 > 代码默认值。
本文件只负责路径、密钥等启动期就必须确定的配置。
"""
import os
import secrets
from pathlib import Path

from dotenv import load_dotenv

BASE_DIR = Path(__file__).resolve().parent.parent.parent  # 项目根目录 designkit/

# 先加载 .env，否则 .env 里的 DESIGNKIT_DATA_DIR 等会被下面的默认值抢先
load_dotenv(BASE_DIR / ".env")

DATA_DIR = Path(os.environ.get("DESIGNKIT_DATA_DIR", str(BASE_DIR / "data")))
UPLOAD_DIR = DATA_DIR / "uploads"
OUTPUT_DIR = DATA_DIR / "outputs"
THUMB_DIR = DATA_DIR / "thumbnails"
FRONTEND_DIR = BASE_DIR / "frontend"

for _d in (DATA_DIR, UPLOAD_DIR, OUTPUT_DIR, THUMB_DIR):
    _d.mkdir(parents=True, exist_ok=True)
try:
    os.chmod(DATA_DIR, 0o700)  # 数据目录含数据库与密钥，限制为仅属主可访问
except OSError:
    pass

DATABASE_URL = os.environ.get(
    "DESIGNKIT_DATABASE_URL", "sqlite:///" + str(DATA_DIR / "designkit.db")
)


def _load_secret_key() -> str:
    """JWT 与 webhook 签名用的主密钥：env 优先，否则生成一次并持久化到 data 目录。"""
    env_key = os.environ.get("DESIGNKIT_SECRET_KEY")
    if env_key:
        return env_key
    key_file = DATA_DIR / ".secret_key"
    if key_file.exists():
        return key_file.read_text().strip()
    key = secrets.token_urlsafe(48)
    # 用 O_CREAT|O_EXCL + 0600 原子创建，避免先默认权限写入再 chmod 的短暂可读窗口
    try:
        fd = os.open(str(key_file), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            os.write(fd, key.encode("utf-8"))
        finally:
            os.close(fd)
    except FileExistsError:
        return key_file.read_text().strip()
    return key


SECRET_KEY = _load_secret_key()

HOST = os.environ.get("DESIGNKIT_HOST", "127.0.0.1")
PORT = int(os.environ.get("DESIGNKIT_PORT", "8787"))

TOKEN_EXPIRE_DAYS = 7
MAX_UPLOAD_MB = 20
ALLOWED_IMAGE_EXTS = {".png", ".jpg", ".jpeg", ".webp", ".heic", ".heif"}

# 可在「系统设置」界面里修改的运行时设置（存数据库），这里是默认值 + env 后备
RUNTIME_DEFAULTS = {
    # 生图服务商：mock=模拟生图（不花钱，用于验证流程）；openai=OpenAI 兼容接口（含国内中转平台）
    "provider": os.environ.get("DESIGNKIT_PROVIDER", "mock"),
    "openai_base_url": os.environ.get("OPENAI_BASE_URL", "https://api.openai.com"),
    "openai_api_key": os.environ.get("OPENAI_API_KEY", ""),
    # 默认对齐用户的自建 Sub2API 网关；接其他平台时在系统设置里改
    "image_model": os.environ.get("DESIGNKIT_IMAGE_MODEL", "gpt-image-2"),
    # 带视觉的文本模型：生成前先看商品图、结合补充要求现场合成提示词
    "text_model": os.environ.get("DESIGNKIT_TEXT_MODEL", "gpt-5.6-sol"),
    # 提示词库只作参考——开启后每次生成都由 AI 按实际商品重写提示词
    "prompt_synthesis": os.environ.get("DESIGNKIT_PROMPT_SYNTHESIS", "true").lower() in ("1", "true", "yes"),
    # 发送前把商品图按所选比例补边（不裁产品）+ 透明底合成白底。
    # 自建网关忽略 size 参数、输出比例跟随输入图，开着才能控制出图比例；
    # 对遵循 size 的官方/中转网关同样安全（输入输出比例一致，无形变）。
    "normalize_input_ratio": os.environ.get("DESIGNKIT_NORMALIZE_INPUT_RATIO", "true").lower() in ("1", "true", "yes"),
    # 对外可访问的地址：生成结果的图片链接、webhook 里的 URL 都基于它拼出来
    "public_base_url": os.environ.get(
        "DESIGNKIT_PUBLIC_BASE_URL", "http://127.0.0.1:%d" % PORT
    ),
    # 是否允许对外 API 的取图/回调指向内网、环回地址。
    # 内部部署默认 True（ERP 常在内网）；转公网 SaaS 时改 False 以全面拦截私网 SSRF。
    # 无论此项如何，云元数据(169.254.x)等链路本地/保留地址始终被拦截。
    "allow_internal_targets": os.environ.get("DESIGNKIT_ALLOW_INTERNAL_TARGETS", "true").lower() in ("1", "true", "yes"),
    "worker_concurrency": int(os.environ.get("DESIGNKIT_WORKER_CONCURRENCY", "2")),
    "max_attempts": int(os.environ.get("DESIGNKIT_MAX_ATTEMPTS", "2")),
    # 自建网关实测单张 70~296 秒（多参考图更慢），默认给足
    "request_timeout": int(os.environ.get("DESIGNKIT_REQUEST_TIMEOUT", "360")),
    "default_size": "1024x1024",
    "default_n": 1,
}
