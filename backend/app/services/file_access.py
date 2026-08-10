"""图片访问（/files）的路径校验与凭证签发/校验。

这个模块只做纯计算：正则、HMAC、时间戳。不碰数据库、不碰请求对象，
所以可以单独拿出来测（见 tests/test_file_access.py）。真正的取图与归属判断
在 routers/files.py 里。

─────────────────────────────────────────────────────────────────────
为什么要有这个模块（改之前的状况，值得写下来免得以后又改回去）
─────────────────────────────────────────────────────────────────────
以前 uploads / outputs / thumbnails 三个目录是直接当静态目录挂出去的，
**没有任何鉴权**：不用登录、不带任何令牌，只要知道地址就能下载任何一个人的
商品图和出图结果。而地址几乎不用猜——

    outputs/年月/<任务号>/<第几张>.png
    thumbnails/年月/<任务号>_<第几张>.jpg

除了任务号，其余部分都是很小的可枚举集合（年月十几种、序号 0~9）。
而任务号本身在接口返回里是明文、在给 ERP 的响应体和回调里也是明文、
在反向代理的每一行访问日志里还是明文。也就是说：**泄露一个任务号
≈ 泄露这一单的全部出图**，一百来次无鉴权请求就能全拿走。

所以这里补两套凭证，分别对应两类取图的人：

  网页端 —— httpOnly 的 dk_files Cookie（限定 path=/files）。
      为什么不用「短时效签名链接」：页面上的图是 <img src> 加载的，
      带不上 Authorization 请求头；而签名链接一旦过期，前端内存里
      长期存着的那些图片地址会集体失效——用户开着页面去吃个饭，
      回来满屏裂图，对非技术用户就是「系统坏了」且完全无从解释。
      Cookie 由浏览器自动续着带，没有这个问题。

  ERP  —— 地址后面挂 ?k=<KeyID>&e=<到期时间>&t=<签名> 三个参数。
      ERP 是机器对机器，链接常常先落库、隔几天才真去下载，所以给 7 天。

**两套凭证都绑定了身份**（Cookie 里是 user_id + token_version，
签名里是 api_key_id），这一点是刻意的：只签「路径 + 到期时间」的做法看着
更简单，但那样一来，停用某个账号或吊销某把 Key 之后，他手上已经发出去的
图片链接在有效期内照样能用——最长七天的后门，而且没有任何办法提前作废。
绑上身份之后，改密码 / 停用账号（token_version 变了）、停用 Key（is_active
变了）都会让相关链接**立刻**打不开。
"""
import hashlib
import hmac
import logging
import re
import threading
import time
from typing import Dict, Optional, Pattern

# 故意导入模块本身而不是 `from ..config import SECRET_KEY`：后者在 import 那一刻
# 就把值抄走了，测试里没法临时换一把密钥来验证「换了密钥旧凭证会不会失效」。
# 与 services/secrets_box.py 保持同样的写法。
from .. import config

logger = logging.getLogger("designkit.files")


# ─────────────────────────── 一、路径白名单 ───────────────────────────

# 只有完全长成下面三种样子的相对路径才允许通过，其余一律当作「文件不存在」。
#
# **这是防路径穿越的正解，不要试图用「算出绝对路径再判断在不在 data 目录里」
# 来代替它。** 那种检查在这里完全不起作用，原因是攻击载荷本来就在 data 目录内：
#
#     outputs/202608/<自己的任务号>/../../202607/<别人的任务号>/0.png
#
# 拿这个字符串去切任务号，切出来的是**自己的**任务（归属检查顺利通过）；
# 而路径归一化之后指向的是**别人的**文件，且仍然老老实实待在 data 目录里面
# （包含性检查也顺利通过）。漏洞不在「文件在不在目录里」，而在于
# 「用来判断归属的字符串」和「最后真正打开的文件」是两个不同的东西。
#
# 上面这条正则不允许出现任何 `.` 路径段、不允许多余的斜杠、不允许非十六进制
# 字符，所以那串东西在第一步就被拒了，根本走不到归属判断。
# 由此还引出 routers/files.py 里的一条硬规矩：**归属判断和实际取文件必须
# 使用同一个已经通过正则的字符串，中间不许再拼接、再归一化。**
#
# 两个容易踩空的正则细节，别改：
#   1. 结尾用 \Z 不用 $。Python 的 $ 也匹配「结尾的换行符前面」，
#      也就是说 "uploads/202608/<32位>.png\n" 用 $ 是能过的（地址里写 %0a
#      就能构造出来）。\Z 才是真正的字符串末尾。
#   2. 数字用 [0-9] 不用 \d。Python 3 的 \d 连阿拉伯-印度数字这类 Unicode
#      数字都算，白名单就不再是「白」的了。
#
# 这三条已逐一核对过真实产物，覆盖全部情况：
#   任务号 = uuid4().hex（models.py 的 _uuid）= 32 位小写十六进制；
#   上传文件名 = uuid4().hex + 后缀，后缀在 routers/uploads.py 里先 .lower()
#   再过 ALLOWED_IMAGE_EXTS 白名单；出图扩展名在 services/storage.py 里
#   只可能是 .png / .jpg / .webp 三种。
SAFE_PATH_RE: Dict[str, Pattern] = {
    "uploads": re.compile(
        r"^uploads/[0-9]{6}/[0-9a-f]{32}\.(?:png|jpg|jpeg|webp|heic|heif)\Z"
    ),
    "outputs": re.compile(r"^outputs/[0-9]{6}/[0-9a-f]{32}/[0-9]{1,2}\.(?:png|jpg|webp)\Z"),
    "thumbnails": re.compile(r"^thumbnails/[0-9]{6}/[0-9a-f]{32}_[0-9]{1,2}\.jpg\Z"),
}


def classify(rel_path: str) -> Optional[str]:
    """判断这个相对路径属于哪一类（uploads / outputs / thumbnails）。

    形状不对就返回 None，调用方一律按「文件不存在」处理（404）。
    不要返回 403 之类更明确的错误：那等于告诉对方「你猜的目录是对的」。
    """
    if not rel_path or not isinstance(rel_path, str):
        return None
    for kind, pattern in SAFE_PATH_RE.items():
        if pattern.match(rel_path):
            return kind
    return None


def job_id_of(kind: str, rel_path: str) -> Optional[str]:
    """从已通过 classify 的路径里切出任务号。**必须先 classify 再调它。**

    outputs    → outputs/年月/<任务号>/<序号>.png
    thumbnails → thumbnails/年月/<任务号>_<序号>.jpg
                 用 rsplit("_", 1) 从右边切：任务号本身是纯十六进制、不含下划线，
                 但从右边切在任何情况下都对，不用去赌左边有没有别的下划线。
    """
    if kind == "outputs":
        return rel_path.split("/")[2]
    if kind == "thumbnails":
        return rel_path.split("/")[2].rsplit("_", 1)[0]
    return None


# ─────────────────────── 二、网页端 Cookie 凭证 ───────────────────────

WEB_COOKIE_NAME = "dk_files"
# Cookie 的作用域只限定在 /files 下面。这样做有两个好处：
#   1. 业务接口（/api/...）继续只认 Authorization 请求头，压根收不到这个
#      Cookie，所以本次改动不引入任何 CSRF 面——别人的网页即使诱导浏览器
#      带上它，也只能拿到图片、调不动任何接口。
#   2. 前端静态资源请求上不会白白多带一段无用的 Cookie。
WEB_COOKIE_PATH = "/files"

_COOKIE_MSG = "dkfile.v1:%d:%d:%d"  # user_id : token_version : 到期时间戳


def _sig(message: str) -> str:
    """HMAC-SHA256 后取前 32 位十六进制（= 16 字节）。

    截短到 16 字节是为了让 Cookie 和链接不至于太长；对「伪造一个有效签名」
    来说 16 字节的强度绰绰有余（暴力猜中的概率是 2^-128 量级）。
    """
    mac = hmac.new(
        config.SECRET_KEY.encode("utf-8"), message.encode("utf-8"), hashlib.sha256
    )
    return mac.hexdigest()[:32]


def issue_web_cookie(user_id: int, token_version: int, days: Optional[int] = None) -> str:
    """签发网页端取图凭证，返回 Cookie 的值。

    值的样子：<用户id>.<令牌版本>.<到期时间戳>.<签名>
    有效期默认和登录令牌一样是 7 天——**故意对齐**：如果图片凭证比登录短，
    就会出现「人还登录着、页面也能用，可图全裂了」这种故障，
    非技术用户完全无法归因，只会觉得系统坏了。
    """
    ttl_days = int(days) if days else int(config.TOKEN_EXPIRE_DAYS)
    if ttl_days < 1:
        ttl_days = 1
    exp = int(time.time()) + ttl_days * 86400
    uid = int(user_id)
    ver = int(token_version or 0)
    return "%d.%d.%d.%s" % (uid, ver, exp, _sig(_COOKIE_MSG % (uid, ver, exp)))


def parse_web_cookie(value: Optional[str]) -> Optional[Dict[str, int]]:
    """校验并解析取图 Cookie，返回 {"user_id", "ver", "exp"}；不合法返回 None。

    只负责「这串东西是不是我们自己签发的、有没有过期」。
    「这个用户还在不在、有没有被停用、令牌版本对不对」由调用方查库判断——
    那才是停用账号后链接立刻失效的关键一步，不能省。
    """
    if not value:
        return None
    parts = value.split(".")
    if len(parts) != 4:
        return None
    try:
        uid = int(parts[0])
        ver = int(parts[1])
        exp = int(parts[2])
    except (TypeError, ValueError):
        return None
    # compare_digest 而不是 ==：普通字符串比较是逐字符短路的，耗时会随着
    # 「前面猜对了几位」变化，理论上可以被一位一位地试出来。
    if not hmac.compare_digest(parts[3], _sig(_COOKIE_MSG % (uid, ver, exp))):
        return None
    if exp <= int(time.time()):
        return None
    return {"user_id": uid, "ver": ver, "exp": exp}


def set_web_cookie(
    response, user_id: int, token_version: int, days: Optional[int] = None,
    secure: bool = False,
) -> None:
    """把取图 Cookie 种到响应上（登录成功、以及每次 /auth/me 都该调一次）。

    httponly=True：页面上的 JS 读不到它，就算前端某处被注入脚本也偷不走。
    samesite="lax"：同站的图片请求照常带上，别人网站发起的跨站请求带不上。
    secure：只有整站跑在 https 上时才能设 True——在 http 的内网部署上设了
            它，浏览器会**直接丢弃**这个 Cookie，表现为「登录成功但所有图
            都打不开」，而且控制台什么错都不报，极难查。所以默认 False，
            由调用方按 public_base_url 是不是 https 来决定。
    """
    ttl_days = int(days) if days else int(config.TOKEN_EXPIRE_DAYS)
    if ttl_days < 1:
        ttl_days = 1
    response.set_cookie(
        key=WEB_COOKIE_NAME,
        value=issue_web_cookie(user_id, token_version, ttl_days),
        max_age=ttl_days * 86400,
        path=WEB_COOKIE_PATH,
        httponly=True,
        samesite="lax",
        secure=secure,
    )


def clear_web_cookie(response) -> None:
    """退出登录时清掉取图 Cookie。path 必须和种的时候完全一致，否则删不掉。"""
    response.delete_cookie(key=WEB_COOKIE_NAME, path=WEB_COOKIE_PATH)


# ────────────────────────── 三、ERP 签名链接 ──────────────────────────

SIGN_PARAM_KEY = "k"    # api_key_id
SIGN_PARAM_EXP = "e"    # 到期时间戳（秒）
SIGN_PARAM_SIG = "t"    # 签名

_ERP_MSG = "dkfilesig.v1:%s:%d:%d"  # 相对路径 : api_key_id : 到期时间戳


def erp_exp(ttl_hours: Optional[int] = None) -> int:
    """算出 ERP 链接的到期时间戳。默认 168 小时（7 天）。

    为什么给这么长：ERP 那边普遍是「先把图片地址存进自己的库、隔几天走批处理
    再去下载」。给短了不会表现成「链接过期」这么直白的错误，而是变成
    「偶尔有几张图下不下来」，两边都查不出原因。
    """
    hours = int(ttl_hours) if ttl_hours else 168
    if hours < 1:
        hours = 1
    return int(time.time()) + hours * 3600


def sign_erp(rel_path: str, api_key_id: int, exp: int) -> str:
    """给 ERP 的图片直链签名。

    签的内容同时包含**路径、Key 的 id、到期时间**三样，缺一不可：
      - 带上路径 → 一条链接的签名换到另一张图上用不了；
      - 带上到期时间 → 链接不会永久有效；
      - 带上 Key 的 id → 这是关键的一条。校验时会回头查这把 Key 还在不在、
        是不是还启用着，所以**停用一把 Key，它此前发出去的所有图片链接立刻
        全部失效**。只签路径和到期时间的话，停用之后还有最长七天的窗口期，
        期间没有任何补救手段。
    """
    return _sig(_ERP_MSG % (rel_path, int(api_key_id), int(exp)))


def verify_erp(rel_path: str, api_key_id: int, exp: int, sig: str) -> bool:
    """只校验签名本身对不对。是否过期请另外调 is_expired()。

    故意分成两个函数：签名不对（有人在伪造）和链接过期（对接方拿了老地址）
    是完全不同的两件事，对外给的提示语也该不一样，合成一个布尔值就分不出来了。
    """
    if not sig:
        return False
    try:
        expected = sign_erp(rel_path, int(api_key_id), int(exp))
    except (TypeError, ValueError):
        return False
    return hmac.compare_digest(str(sig), expected)


def is_expired(exp: int) -> bool:
    try:
        return int(exp) <= int(time.time())
    except (TypeError, ValueError):
        return True


def signed_query(rel_path: str, api_key_id: int, exp: int) -> str:
    """拼出签名参数串（不带问号），形如 k=3&e=1760000000&t=8f2c...

    给 services/storage.py 的 to_url() 用。
    """
    return "%s=%d&%s=%d&%s=%s" % (
        SIGN_PARAM_KEY, int(api_key_id),
        SIGN_PARAM_EXP, int(exp),
        SIGN_PARAM_SIG, sign_erp(rel_path, api_key_id, exp),
    )


def append_signature(url: str, rel_path: str, api_key_id: int, exp: int) -> str:
    """把签名参数接到一个已经拼好的地址后面，自动判断该用 ? 还是 &。"""
    sep = "&" if "?" in url else "?"
    return url + sep + signed_query(rel_path, api_key_id, exp)


# ──────────────────── 四、未带凭证访问的日志与计数 ────────────────────
#
# files_signed_only 这个开关的意义：默认 True（凭证必须有，没有就 403），
# 但保留把它关掉的可能——万一将来真接了某个 ERP，对方库里还存着一批老地址，
# 可以临时关掉让它们先能用，同时靠下面这个计数器和日志盯着「到底还有没有人
# 在用老链接」，等连续几天为零了再关回去。
#
# 日志要做节流：历史页一屏就有几十张缩略图，一张一条 WARNING 会瞬间把日志刷爆，
# 反而看不见真正要紧的信息。所以第一次立刻打，之后每 60 秒最多打一条，
# 并在那一条里带上这段时间被压掉了多少次。

_LOG_INTERVAL_SECONDS = 60
_counter_lock = threading.Lock()
_counter = {
    "total": 0,          # 进程启动至今，未带凭证的取图请求总数
    "blocked": 0,        # 其中被 403 挡下的（files_signed_only 为 True 时）
    "last_at": 0.0,      # 最后一次发生的时间戳
    "last_path": "",     # 最后一次的路径前缀（不含任务号，避免日志里再泄一次）
    "_logged_at": 0.0,   # 上次真正写日志的时间
    "_suppressed": 0,    # 上次写日志之后被压掉的次数
}


def _path_prefix(rel_path: str) -> str:
    """只取路径的前两段（如 outputs/202608/…）。

    任务号本身就是这次事故的钥匙，没必要在日志里再抄一遍——反向代理的访问
    日志里已经有一份了，那正是当初「知道地址就能下图」的传播途径之一。
    """
    parts = rel_path.split("/")
    return "/".join(parts[:2]) + "/…" if len(parts) > 2 else rel_path


def note_unsigned_access(rel_path: str, referer: str = "", blocked: bool = False) -> None:
    """记一次「没带任何凭证的取图请求」：计数 +1，并按节流规则写 WARNING 日志。"""
    now = time.time()
    with _counter_lock:
        _counter["total"] += 1
        if blocked:
            _counter["blocked"] += 1
        _counter["last_at"] = now
        _counter["last_path"] = _path_prefix(rel_path)
        due = now - _counter["_logged_at"] >= _LOG_INTERVAL_SECONDS
        if due:
            suppressed = _counter["_suppressed"]
            _counter["_logged_at"] = now
            _counter["_suppressed"] = 0
        else:
            _counter["_suppressed"] += 1
            return
    logger.warning(
        "未带凭证访问图片%s：%s 来源页=%s%s（累计 %d 次）",
        "（已拒绝）" if blocked else "（已放行，files_signed_only 处于关闭状态）",
        _path_prefix(rel_path),
        referer[:200] or "(无)",
        "，另有 %d 次同类请求被日志节流省略" % suppressed if suppressed else "",
        _counter["total"],
    )


def unsigned_access_stats() -> Dict[str, object]:
    """给运维/健康检查看的进程内统计。进程重启就归零，本来就只是个观察窗口。"""
    with _counter_lock:
        return {
            "total": _counter["total"],
            "blocked": _counter["blocked"],
            "last_at": _counter["last_at"],
            "last_path": _counter["last_path"],
        }


def reset_unsigned_stats() -> None:
    """只给测试用。"""
    with _counter_lock:
        _counter.update({
            "total": 0, "blocked": 0, "last_at": 0.0, "last_path": "",
            "_logged_at": 0.0, "_suppressed": 0,
        })
