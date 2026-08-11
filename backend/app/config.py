"""全局路径与基础配置。

优先级：数据库设置(可在界面修改) > .env 环境变量 > 代码默认值。
本文件只负责路径、密钥等启动期就必须确定的配置。
"""
import os
import secrets
import time
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

def _build_database_url() -> str:
    """决定用哪个数据库。

    优先级：完整连接串 > 分开的 PG 参数 > 本机 SQLite。
    分开传参数时用 URL.create 组装——密码里有 @ % : 等字符时手工拼字符串会解析错，
    而且报出来的是「主机连不上」这种完全看不出根因的错误。
    """
    explicit = os.environ.get("DESIGNKIT_DATABASE_URL")
    if explicit:
        return explicit

    host = os.environ.get("DESIGNKIT_DB_HOST")
    if host:
        from sqlalchemy.engine import URL

        return URL.create(
            "postgresql+psycopg",
            username=os.environ.get("DESIGNKIT_DB_USER") or "designkit",
            password=os.environ.get("DESIGNKIT_DB_PASSWORD") or None,
            host=host,
            port=int(os.environ.get("DESIGNKIT_DB_PORT") or 5432),
            database=os.environ.get("DESIGNKIT_DB_NAME") or "designkit",
        ).render_as_string(hide_password=False)

    return "sqlite:///" + str(DATA_DIR / "designkit.db")


DATABASE_URL = _build_database_url()


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


def _load_encryption_key() -> str:
    """凭据加密（网关 Key 等）用的主密钥，存 data/.enc_key。

    为什么单独一把、不从上面的 SECRET_KEY 派生：SECRET_KEY 是登录令牌的签名密钥，
    「怀疑令牌泄露就换掉 DESIGNKIT_SECRET_KEY」是随手就能做的应急动作，
    换完大家重新登录一次而已、成本近乎为零。可这把钥匙一换，
    库里所有加密过的网关凭据就永久解不开了（现象是全体用户突然都不能生图，
    而数据库看起来完好无损，根因根本查不出来）。两者绑在一起迟早出这个事，
    所以彻底分开：SECRET_KEY 随时可换，.enc_key 丢了等于凭据全丢。
    详细说明见 services/secrets_box.py 的文件头。

    备份 data 目录时，.secret_key 和 .enc_key 这两个文件必须和数据库一起备份。
    """
    # 前后空白一律去掉：.env 里行尾多一个空格、或从网页上复制时带了换行，
    # 都会算出另一把钥匙。表现是「本机好好的，NAS 上所有 Key 都解不开」，极难想到这里。
    env_key = (os.environ.get("DESIGNKIT_ENCRYPTION_KEY") or "").strip()
    if env_key:
        if len(env_key) < 16:
            # 这把钥匙不做慢哈希拉伸，短口令等于没加密。宁可启动失败也不让它上生产。
            raise RuntimeError(
                "环境变量 DESIGNKIT_ENCRYPTION_KEY 太短（至少 16 位）。"
                "要么删掉它、让程序自动生成 data/.enc_key（推荐），"
                "要么用这条命令生成一串随机的填进去："
                "python3 -c \"import secrets;print(secrets.token_urlsafe(48))\""
            )
        return env_key
    key_file = DATA_DIR / ".enc_key"

    def _read_back() -> str:
        # 极小概率的竞态：另一个进程刚用 O_EXCL 建好文件、还没来得及写内容，
        # 这时读到的是空串。空钥匙会让本次启动的加解密全部算错，
        # 所以宁可等一等、等不到就报错退出，也不能拿空串继续跑。
        for _ in range(20):
            text = key_file.read_text().strip()
            if text:
                return text
            time.sleep(0.05)
        raise RuntimeError(
            "data/.enc_key 是个空文件，无法用于加密。"
            "请把它删掉再重启（会自动重新生成）；"
            "但注意：如果库里已经存过加密的网关 Key，删掉后那些 Key 就解不开了，"
            "需要在设置里重新填一次。"
        )

    if key_file.exists():
        return _read_back()
    key = secrets.token_urlsafe(48)
    # 与 .secret_key 完全相同的写法：O_CREAT|O_EXCL + 0600 原子创建，
    # 既避免「先按默认权限写入、再 chmod」中间那段人人可读的窗口，
    # 也保证多进程（uvicorn 多 worker）同时启动时只有一个能建成、其余读回同一把钥匙，
    # 不会出现两个进程各拿一把、互相解不开对方密文的情况。
    try:
        fd = os.open(str(key_file), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            os.write(fd, key.encode("utf-8"))
        finally:
            os.close(fd)
    except FileExistsError:
        return _read_back()
    return key


ENCRYPTION_KEY = _load_encryption_key()

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
    # 出图底色：auto=交给模型（通常是实底）；transparent=要求输出透明底 PNG。
    # 透明底省掉抠图这道工序（同一张图可换任意背景），但要求网关支持 background 参数；
    # 不支持时会在生成记录里明确报出来，而不是悄悄退回不透明。
    "image_background": os.environ.get("DESIGNKIT_IMAGE_BACKGROUND", "auto"),
    # 对外可访问的地址：生成结果的图片链接、webhook 里的 URL 都基于它拼出来
    "public_base_url": os.environ.get(
        "DESIGNKIT_PUBLIC_BASE_URL", "http://127.0.0.1:%d" % PORT
    ),
    # 是否允许对外 API 的取图/回调指向内网、环回地址。
    # 内部部署默认 True（ERP 常在内网）；转公网 SaaS 时改 False 以全面拦截私网 SSRF。
    # 无论此项如何，云元数据(169.254.x)等链路本地/保留地址始终被拦截。
    "allow_internal_targets": os.environ.get("DESIGNKIT_ALLOW_INTERNAL_TARGETS", "true").lower() in ("1", "true", "yes"),
    # 同步灵感库要访问 raw.githubusercontent.com，部分地区直连不通。
    # 填了就走代理，留空即直连。形如 http://127.0.0.1:7890 或 socks5://127.0.0.1:1080
    # （socks5 需要装 PySocks，一般用 http 代理即可）。只影响灵感库同步，
    # 生图网关是局域网地址，永远直连、不走代理。
    "inspiration_proxy": os.environ.get("DESIGNKIT_INSPIRATION_PROXY", ""),
    # 灵感库自动同步：上游每天更新两次，默认 12 小时对齐即可
    "inspiration_auto_sync": os.environ.get("DESIGNKIT_INSPIRATION_AUTO_SYNC", "true").lower() in ("1", "true", "yes"),
    "inspiration_sync_interval_hours": int(os.environ.get("DESIGNKIT_INSPIRATION_SYNC_INTERVAL_HOURS", "12")),
    "worker_concurrency": int(os.environ.get("DESIGNKIT_WORKER_CONCURRENCY", "2")),
    "max_attempts": int(os.environ.get("DESIGNKIT_MAX_ATTEMPTS", "2")),
    # 自建网关实测单张 70~296 秒（多参考图更慢），默认给足
    "request_timeout": int(os.environ.get("DESIGNKIT_REQUEST_TIMEOUT", "360")),
    "default_size": "1024x1024",
    "default_n": 1,
    # ── 图片访问与多用户隔离 ──
    # 图片链接是否必须带凭证（网页端的 dk_files Cookie，或对外 API 的 ?k=&e=&t= 签名）
    # 才能打开。**默认 True，也就是「没凭证就不给看」。**
    #
    # 为什么一上来就默认打开、不做灰度：改造之前，uploads / outputs / thumbnails
    # 三个目录是零鉴权直接挂出去的——不用登录、不带任何令牌，知道地址就能下载任何
    # 一个人的商品图和出图结果，而地址里除了任务号之外全是可枚举的小集合。
    # 灰度只有在「已经有对接方存着一批老链接、一刀切会让对方的图集体裂开」时才划算，
    # 而本系统**目前没有任何 ERP 对接方**（已确认），根本不存在会被打断的存量链接。
    # 既然没有人会被打断，就没有理由让这个洞多开着一天。
    #
    # 开关保留下来，是留给将来真接了对接方、对方一时半会儿换不完老地址时临时放宽用的：
    # 关掉之后未带凭证的请求会照旧放行，但每一次都会记警告日志并计数
    # （设置页「图片访问」一节看得到累计次数），等计数连续几天不再涨，再关回去。
    # 换句话说，关掉它是一次**有意识、有观察窗口的临时决定**，不是默认状态。
    "files_signed_only": os.environ.get("DESIGNKIT_FILES_SIGNED_ONLY", "true").lower() in ("1", "true", "yes"),
    # 网页端取图凭证（dk_files Cookie）的有效期。和登录令牌的 7 天对齐：
    # 短于登录有效期的话，人还登录着、图却开始裂了，这种故障用户完全无法归因。
    "web_file_cookie_days": int(os.environ.get("DESIGNKIT_WEB_FILE_COOKIE_DAYS", "7")),
    # 给 ERP 的图片直链有效多久（小时）。默认 168 小时 = 7 天：
    # ERP 那边常把链接落库、隔几天才真正去下载，给短了会变成「偶发下载失败」。
    "erp_file_link_ttl_hours": int(os.environ.get("DESIGNKIT_ERP_FILE_LINK_TTL_HOURS", "168")),
    # 允许哪些网站的网页调用本系统接口（CORS）。默认 "*" 与今天的行为一致；
    # 哪天把系统开到公网上，就在这里改成实际域名（多个用英文逗号隔开），
    # 这样别人的网页就没法拿着用户的登录状态偷偷读数据了。
    "allowed_origins": os.environ.get("DESIGNKIT_ALLOWED_ORIGINS", "*"),
    # 生图网关的计费方式：
    #   shared   —— 全体共用系统设置里那一把 Key（今天就是这样，升级当天不变）
    #   per_user —— 每人一把自己的 Key，各花各的钱、账单分得清是谁用的
    # 默认保持 shared，等成员账号和各自的 Key 都配好了再切过去。
    "gateway_mode": os.environ.get("DESIGNKIT_GATEWAY_MODE", "shared"),

    # ══════════════════════════════════════════════════════════════
    #  网关自动开通（在 Sub2API 上代客建号、代客拿 Key）
    # ══════════════════════════════════════════════════════════════
    #
    # 这一组**故意一个都不读环境变量**，全部由管理员在
    # 「系统设置 → 网关自动开通」里填。两个理由：
    # ① 管理员 Key 属于密钥，本项目的约定是「密钥一律让用户自己在网页界面填，
    #    不代填、不写进代码或配置文件」，所以它不能有 .env 后备；
    # ② 其余几项（地址、分组 id、邮箱域名）是跟着那一台 Sub2API 走的一次性配置，
    #    放进 .env 只会多出「env 里填了、库里也存了、到底哪个生效」的困惑——
    #    而这类困惑在这个项目里是真出过事的（文档和代码对不上，用户分不清是谁错了）。
    #
    # 默认值必须和 services/provisioning.py 的 PROVISIONING_SETTING_DEFAULTS
    # **逐项一致**。那边是「config 里还没这个键时的后备」，两边写岔了的表现是
    # 「设置页显示 A、实际按 B 跑」，界面上完全看不出来。改这里就去改那里。

    # 总开关。**默认关闭**：管理员 Key 没填之前打开，只会让每个新用户都走一遍
    # 必然失败的开通流程、last_error 里堆一片红字。填完地址 / 管理员 Key /
    # 分组 id 之后再自己打开（接口层会拦住「没配齐就打开」这个动作）。
    "sub2api_auto_provision": False,
    # Sub2API 管理端地址，形如 http://192.168.31.235:3000（不带末尾斜杠、不带路径）
    "sub2api_base_url": "",
    # Sub2API 的管理员 Key（请求头 x-api-key）。
    # **必须在 SECRET_KEYS 里打码**，见本文件末尾的 SECRET_SETTING_KEYS。
    # 这里永远是空串：不预填、不读 env、不写进任何模板文件。
    "sub2api_admin_key": "",
    # 目标分组 id（数字）。为什么要管理员手填：admin 侧有没有「列出所有分组」的接口
    # 至今没查明，而用户侧那个接口要先有 JWT（鸡生蛋）。填错的表现是建 Key 返回
    # 403 GROUP_NOT_ALLOWED，或者号建好了、Key 也发了、一张图都发不出去
    # （网关回「API Key is not assigned to any group」）——自检里专门查这一项。
    "sub2api_group_id": "",
    # 代客建号时用的合成邮箱域名，最终邮箱形如 dk7@designkit.local。
    # 只允许 [a-z0-9.-]：邮箱里出现 '+' 会被 Sub2API 的别名去重逻辑
    # (stripEmailPlusSuffix) 并成同一个人，两个成员会共用一个远端账号、账单直接算错人头。
    # 另外 .invalid 那一族是保留后缀，用了会被策略拒且报错里看不出根因，接口层会拦。
    "sub2api_email_domain": "designkit.local",
    # 是否长期保管用户在 Sub2API 的密码。**默认 false**。
    # 取回已经发出去的 Key 只要管理员 Key 就够了，只有「新建 Key」那一次需要密码；
    # 所以开通成功之后立刻把密码清掉，designkit 长期保管的就只是一堆 Key，
    # 而不是一堆能登进 Sub2API 的凭据——这套方案最大的安全负担就砍掉了大半。
    # 代价（界面上必须写出来）：清空之后这一行不能再自动重建 Key，只能手工发。
    "sub2api_keep_password": False,
    # 自动开通连续失败几次就转「等管理员手工发 Key」。
    # 5 次配合 1 分钟 / 5 分钟 / 30 分钟 / 2 小时 / 12 小时的退避阶梯约覆盖 15 小时，
    # 足够跨过一次夜间维护窗口，又不会无限重试下去刷日志。
    "sub2api_max_attempts": 5,
    # 单次请求 Sub2API 的超时（秒）。面板接口都很快，15 秒足够；
    # 给大了会让后台 worker 卡在一个连不上的地址上，拖慢所有人的开通。
    "sub2api_timeout": 15,
    # 是否校验 https 证书。默认开；内网自签证书的实例才需要关，且要管理员自己点。
    "sub2api_verify_tls": True,

    # 「共享兜底 Key」——自动开通全线崩溃时的救命开关，**默认关**。
    #
    # 打开之后，所有还没开通成功的用户临时共用管理员填的那一把全局 Key
    # （也就是上面的 openai_api_key），用户完全无感、照常生图。
    # 代价必须原样写在界面开关旁边，不能只写在这里：
    #   无法按用户分账、额度共用、任何一个人跑量都在消耗同一个余额。
    # 建议只在故障期临时打开，故障处理完立刻关回去。
    #
    # ⚠ 这个开关**目前只是存了个值**：真正让它生效的判断在
    # services/settings_service.py 的 resolve_for_user（规则 5 那条硬线）里，
    # 那个文件不属于本次改动范围。接上之前，打开它不会有任何效果。
    "gateway_shared_fallback": False,
}

# 需要在设置接口里打码的密钥类设置项——**本次新增的那些**。
# settings_service.SECRET_KEYS 里已有的 openai_api_key / inspiration_proxy
# 不重复列在这里，免得两处都写一遍、将来改一处漏一处。
#
# 为什么放在 config 而不是直接写进 settings_service：新增一个密钥设置项时，
# 「加默认值」和「记得打码」这两件事就挨在一起，不会加了字段忘了打码——
# 忘一次的后果是管理员 Key 明文出现在浏览器的接口响应里。
# 真正并进 SECRET_KEYS 的动作在 routers/settings_router.py 的模块顶部
# （config 不能反过来 import settings_service，会绕成环）。
SECRET_SETTING_KEYS = ("sub2api_admin_key",)
