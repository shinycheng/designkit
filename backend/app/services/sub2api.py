"""Sub2API 网关的 HTTP 客户端：只管「怎么调」和「错在哪一类」。

这个模块**故意不含任何状态机逻辑**——不读数据库、不改 user_gateway_accounts、
不决定「下一步该干什么」、不做退避重试的定时。它只做三件事：

  1. 按已核实的接口契约把请求发出去；
  2. 把响应解析成结构化的返回值；
  3. 把各种失败翻译成一个 `Sub2ApiError`，带上**分类码**（E_*）和
     **能不能重试**（retryable）。

「能不能重试」这一位必须由本模块给出，不能留给调用方按状态码自己猜——
因为同一个 HTTP 状态码在不同接口上的含义完全不同（详见下面 _classify），
猜错的代价是真金白银：把「建 Key 撞了自定义 key 限流」当成普通 429 去重试，
会把这个用户锁死一小时（Sub2API 的自定义 key 冲突计数是 20 次/小时/用户）。

────────────────────── 三个非标准响应形态（会咬人） ──────────────────────
Sub2API 的响应**不是一种格式**，是三种混着来，解析器必须每种都兜住：

  A. 信封格式 {"code":0,"message":"ok","data":{...}}——面板接口（/api/v1/**）
     的常规形态。code==0 才算成功。
  B. 423 合规未确认时，信封里的 `code` 是**字符串** "ADMIN_COMPLIANCE_ACK_REQUIRED"
     而不是整数。任何 `int(body["code"])` 或 `body["code"] != 0` 之类的写法都会
     在这里抛出 TypeError / 把它错认成普通失败。
  C. 429 限流的响应体是 {"error":"rate limit exceeded"} 这套**裸格式**，
     压根没有 code / message / data 三件套。
  D. 网关侧 GET /v1/usage 返回**裸 JSON**，也不是信封。

────────────────────── 两个致命的路径 / 状态码坑 ──────────────────────
  * 建 Key 的路径是 **POST /api/v1/keys**，不是 /api/v1/api-keys。
    Sub2API 源码里那个 handler 的注释写的是 /api/v1/api-keys，是错的，
    照注释写会稳定 404。路径常量集中在下面 _PATH_* 里，别在调用处硬编码。
  * 建号成功返回 **200 不是 201**。写 `if resp.status_code == 201` 会把
    每一次成功都当成失败，然后一直重试到 409，看起来像「怎么都建不上」。

────────────────────── 日志与密文纪律 ──────────────────────
  * 密码、API Key、JWT **绝不进日志，也绝不进异常消息**。本模块的做法是：
    - 所有请求体在写日志前过一遍 `_redact()`（password/custom_key/key/token 变 ***）；
    - 所有装着秘密的 dataclass 字段都标了 `repr=False`，
      这样即使有人顺手写 `logger.info("拿到 %s", issued_key)` 也不会漏；
    - 只允许记末 4 位（`_tail()`）。
  * 明文密码只在调用方的局部变量里活着，本模块不缓存、不存类属性。

安全：Sub2API 的管理员 Key 由管理员自己在 designkit 网页界面填，
本模块只通过构造参数接收它，绝不从代码或配置文件里读死值。
"""
import hashlib
import hmac
import json
import logging
import re
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional, Set, Tuple

import httpx

logger = logging.getLogger("designkit.sub2api")

# ── 路径常量：全部集中在这里，调用处一律引用，不许散落硬编码 ──
# 集中的理由不是「好看」，是 Sub2API 升版本时路由前缀漂移了要能一处改完，
# 而且自检探针 5 就是专门盯着这件事的（见 probe_keys_route）。
_PATH_ADMIN_USERS = "/api/v1/admin/users"
_PATH_ADMIN_COMPLIANCE = "/api/v1/admin/compliance"
_PATH_LOGIN = "/api/v1/auth/login"
_PATH_KEYS = "/api/v1/keys"  # ← 不是 /api/v1/api-keys，源码注释写错了
_PATH_PUBLIC_SETTINGS = "/api/v1/settings/public"
_PATH_GATEWAY_USAGE = "/v1/usage"

# 建号时显式传的值。**不依赖服务端默认**：admin 建号路径不传 concurrency 就会
# 真的写字面 0，而 concurrency=0 在网关侧到底是「不限」还是「一个请求都发不出去」
# 至今没查明（设计文档「仍未查明」第 1 条），所以按最坏情况显式传 5。
# rpm_limit=0 按 Sub2API 的惯例是「不限速」，这一条同样显式传，不留给默认值。
DEFAULT_CONCURRENCY = 5
DEFAULT_RPM_LIMIT = 0

# custom_key 的合法字符集（Sub2API 源码里的正则，逐字抄下来的）
_CUSTOM_KEY_RE = re.compile(r"^[A-Za-z0-9_-]{16,128}$")

# 网关挂了或前面挡着反代时返回的是整页 HTML，不是 JSON。
# 认出来单独报，否则错误信息里会糊一大坨标签，谁也看不出发生了什么。
_HTML_PAGE_RE = re.compile(r"<\s*(!doctype|html|head|body|title)\b", re.I)

# 信封里表示「成功」的 code。字符串形态一并接住（见文件头形态 B）。
_OK_CODES = (0, 200, "0", "200", "ok", "success")

# 日志脱敏要盖掉的字段名（小写比较）。宁可多盖几个也不要漏。
_SECRET_FIELDS = {
    "password", "new_password", "old_password",
    "custom_key", "key", "api_key", "apikey", "secret",
    "access_token", "refresh_token", "token", "jwt",
    "turnstile_token", "captcha_token",
}


# ══════════════════════════════════════════════════════════════════
#  错误分类
# ══════════════════════════════════════════════════════════════════

class Sub2ApiError(Exception):
    """调 Sub2API 失败。**分类码 + 可否重试** 才是这个异常的价值所在。

    - code：E_* 分类码，调用方（状态机）按它分流，不要去 parse message。
    - retryable：True = 这是瞬时故障，退避后重跑同一步是安全且可能成功的；
                 False = 再试一百次也是这个结果，必须转人工或告警。
    - status：HTTP 状态码，没发出去（网络层失败）时为 None。
    - retry_after：服务端 Retry-After 头解析出的秒数，没有就是 None。
    - halt_all：True 表示这不是「这个用户不行」而是「整套自动开通的前提没了」
                （后台模式、验证码开了、管理员 Key 失效、合规未签），
                状态机应当直接把全局开关置成暂停并告警，而不是一个个用户去试。

    message 是**给管理员看的人话**，会原样落进 last_error 并显示在设置页上，
    所以要写「该去哪里点什么」，不要写 HTTP 语义。
    绝不允许把密码 / Key / JWT 拼进 message——它会进日志也会上界面。
    """

    def __init__(
        self,
        code: str,
        message: str,
        retryable: bool = False,
        status: Optional[int] = None,
        retry_after: Optional[float] = None,
        halt_all: bool = False,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.retryable = retryable
        self.status = status
        self.retry_after = retry_after
        self.halt_all = halt_all

    def as_last_error(self) -> str:
        """拼成设计文档规定的 last_error 落库格式：「分类码|人话」。"""
        return "%s|%s" % (self.code, self.message)

    def __repr__(self) -> str:  # 只给排错看，不含任何凭据
        return "Sub2ApiError(code=%s, status=%s, retryable=%s)" % (
            self.code, self.status, self.retryable,
        )


# 分类码总表。写在这里是为了让「有哪些失败形态」这件事能一眼看全，
# 也让状态机可以 `assert code in ERROR_CODES` 防手滑拼错。
# retryable 的值是**默认值**，个别接口会覆盖（最重要的一处见 op="key_create" 的 429）。
ERROR_CODES: Dict[str, str] = {
    # —— 传输层 / 协议层 ——
    "E_CONFIG": "designkit 这边的网关配置不全（地址或管理员 Key 没填）",
    "E_NETWORK": "连不上 Sub2API（超时或网络不通）",
    "E_SERVER": "Sub2API 内部错误（5xx）",
    "E_RATE_LIMITED": "被 Sub2API 限流了",
    "E_PROTOCOL": "Sub2API 的响应不是预期格式（多半是版本升了或地址填错）",
    "E_ROUTE_MISSING": "接口路径 404（Sub2API 换版本挪了路由）",
    # —— admin 建号 / 反查 ——
    "E_ADMIN_AUTH": "管理员 Key 无效或没有权限",
    "E_COMPLIANCE": "Sub2API 要求先签署合规承诺，admin 接口全被拦住",
    "E_EMAIL_EXISTS": "这个邮箱在 Sub2API 已存在（预期分支，不是失败）",
    "E_USER_NOT_FOUND": "按 id 查这个远端用户查不到（可能已被删）",
    "E_BAD_REQUEST": "建号参数被 Sub2API 的策略拒了（邮箱后缀/域名限量等）",
    # —— 代登录 ——
    "E_LOGIN_2FA": "该用户在 Sub2API 开了两步验证，代登录不可用",
    "E_PASSWORD_MISMATCH": "远端密码和本地存的对不上（用户自己改过密码）",
    "E_BACKEND_MODE": "Sub2API 开了后台模式，普通用户登录被禁",
    "E_CAPTCHA_ON": "Sub2API 开了人机验证，代登录必然失败",
    "E_LOGIN_BUSY": "本地登录节流排队超时（并发太多，等下一轮）",
    # —— 建 Key / 读回 ——
    "E_KEY_EXISTS": "这个 custom_key 已存在（预期分支，要去列表里读回来核对）",
    "E_KEY_RATE_LIMITED": "自定义 Key 冲突次数打满（20 次/小时/用户），必须立刻停手",
    "E_GROUP_DENIED": "这个用户不被允许使用目标分组",
    "E_KEY_INVALID": "custom_key 不合法——这是 designkit 自己的生成逻辑写错了",
    "E_IDEMPOTENCY_BUSY": "幂等存储忙或同键正在处理中，退避后原样重发",
    "E_IDEMPOTENCY_CONFLICT": "同一个幂等键配了不同的请求体（代码 bug）",
    # —— 网关冒烟 ——
    "E_NO_GROUP": "这把 Key 没绑任何分组，网关不受理（建了号但一张图也发不出去）",
    "E_KEY_REJECTED": "网关不认这把 Key（被删或被禁用）",
}


# ══════════════════════════════════════════════════════════════════
#  返回值（装秘密的字段一律 repr=False，防止被顺手打进日志）
# ══════════════════════════════════════════════════════════════════

@dataclass
class RemoteUser:
    """Sub2API 侧的一个用户。字段全部按「可能缺失」解析，缺了就是 None。"""
    id: str
    email: str
    username: str = ""
    concurrency: Optional[int] = None
    rpm_limit: Optional[int] = None
    allowed_groups: List[str] = field(default_factory=list)
    deleted: bool = False
    # 原始字典留着给自检和排错用；repr=False 是怕将来 Sub2API 往里加敏感字段，
    # 一句 logger.info("%s", user) 就全漏出去了。
    raw: Dict[str, Any] = field(default_factory=dict, repr=False)


@dataclass
class LoginResult:
    """代登录的结果。access_token 只允许放进程内存，**绝不落库**。"""
    access_token: str = field(repr=False, default="")
    expires_in: Optional[int] = None
    requires_2fa: bool = False


@dataclass
class IssuedKey:
    """一把 Sub2API API Key。

    Sub2API 的 Key 是**明文入库、列表接口原样返回完整 key**，
    所以 409 之后能把已经建好的那把原样读回来，不存在「只显示一次」的问题——
    这是整条链幂等性的基石，别按「只显示一次」的思路去设计重试。
    """
    key: str = field(repr=False, default="")
    id: str = ""
    name: str = ""
    group_id: Optional[str] = None

    @property
    def tail(self) -> str:
        """末 4 位，唯一允许出现在日志里的部分。"""
        return _tail(self.key)


@dataclass
class UsageSnapshot:
    """GET /v1/usage 的结果。注意响应是**裸 JSON 不是信封**。"""
    balance_usd: Optional[float] = None
    raw: Dict[str, Any] = field(default_factory=dict, repr=False)


@dataclass
class ProbeResult:
    """自检探针的单项结果。

    level：green / yellow / red / unknown。
    - unknown 表示「这一项这次没法验证」（比如库里还没有 active 用户可借 Key），
      **不等于绿也不等于红**，界面上要照实写「未验证」，
      不然管理员会以为查过了、其实根本没查。
    """
    name: str
    level: str
    summary: str
    detail: Dict[str, Any] = field(default_factory=dict)


# ══════════════════════════════════════════════════════════════════
#  小工具
# ══════════════════════════════════════════════════════════════════

def _tail(secret: Optional[str], n: int = 4) -> str:
    """取末 4 位。这是本模块允许对凭据做的唯一一种「展示」。"""
    s = secret or ""
    return s[-n:] if len(s) > n else "?"


def _redact(payload: Any) -> Any:
    """请求/响应体写日志前的脱敏：秘密字段一律换成 ***。

    递归处理，因为登录响应是 {"data": {"access_token": ...}} 这种嵌套形态。
    这是日志五道防线里的第 ②③ 道（见设计文档「安全要求」）。
    """
    if isinstance(payload, dict):
        out = {}
        for k, v in payload.items():
            if isinstance(k, str) and k.lower() in _SECRET_FIELDS:
                out[k] = "***"
            else:
                out[k] = _redact(v)
        return out
    if isinstance(payload, list):
        return [_redact(v) for v in payload]
    return payload


def _parse_json(resp: httpx.Response) -> Any:
    """解析响应体，解不出来返回 None（调用方据此报 E_PROTOCOL）。"""
    try:
        return resp.json()
    except Exception:
        return None


def _error_tokens(body: Any) -> Set[str]:
    """把响应体里所有「可能是错误标识」的字符串收集成一个大写集合。

    为什么要收集成集合而不是读某一个固定字段：Sub2API 的错误标识在不同接口上
    出现的位置不一样——有时在 `reason`，有时在 `code`（而且 423 那次 code 是
    **字符串**，见文件头形态 B），限流那次连信封都没有、只有一个裸 `error`。
    与其为每种接口写一套取值逻辑，不如全收进来做包含判断，漏判的概率低得多。
    """
    tokens: Set[str] = set()
    if not isinstance(body, dict):
        return tokens
    for key in ("reason", "code", "error", "error_code", "errorCode", "type"):
        val = body.get(key)
        if isinstance(val, str) and val.strip():
            tokens.add(val.strip().upper())
    # message 里也可能塞着标识或关键短语（比如「Backend mode ...」）
    for key in ("message", "msg", "detail", "error"):
        val = body.get(key)
        if isinstance(val, str) and val.strip():
            tokens.add(val.strip().upper())
    data = body.get("data")
    if isinstance(data, dict):
        for key in ("reason", "code", "error"):
            val = data.get(key)
            if isinstance(val, str) and val.strip():
                tokens.add(val.strip().upper())
    return tokens


def _body_message(body: Any, resp: httpx.Response) -> str:
    """从响应体里取一句能给人看的原因；取不到就退回状态码。

    注意这句话会进 last_error、会上界面，所以最长截到 200 字。
    """
    if isinstance(body, dict):
        for key in ("message", "msg", "detail", "reason", "error"):
            val = body.get(key)
            if isinstance(val, str) and val.strip():
                return val.strip()[:200]
    text = (resp.text or "").strip()
    if text and _HTML_PAGE_RE.search(text[:400]):
        return "返回了 HTML 页面而不是数据"
    return (text[:200] or ("HTTP %d" % resp.status_code))


def _retry_after(resp: httpx.Response) -> Optional[float]:
    """解析 Retry-After 头（只认「秒数」这种写法，HTTP-date 形态忽略）。

    忽略 HTTP-date 不是偷懒：解析日期要处理时区和时钟漂移，
    而拿不到这个值的后果只是「按我们自己的退避表来」，完全可以接受。
    """
    raw = resp.headers.get("Retry-After") or resp.headers.get("retry-after")
    if not raw:
        return None
    try:
        val = float(str(raw).strip())
    except ValueError:
        return None
    return val if val >= 0 else None


def _extract_items(data: Any) -> List[Dict[str, Any]]:
    """从各种分页信封里把列表那一段抠出来。

    Sub2API 不同接口的列表字段名不统一（items / list / users / records / data），
    而且有的接口直接返回数组。这里全兜住——列表字段名猜错的表现是
    「明明建好了却总说查不到」，然后触发一次本可避免的重建。
    """
    if isinstance(data, list):
        return [x for x in data if isinstance(x, dict)]
    if isinstance(data, dict):
        for key in ("items", "list", "users", "keys", "records", "rows", "data"):
            val = data.get(key)
            if isinstance(val, list):
                return [x for x in val if isinstance(x, dict)]
    return []


def _as_str_id(value: Any) -> str:
    """远端 id 可能是 int 也可能是 str，本地列是 String(64)，统一成字符串。"""
    if value is None:
        return ""
    if isinstance(value, bool):  # 防御：True 会被 str() 成 "True"
        return ""
    return str(value).strip()


def _as_int(value: Any) -> Optional[int]:
    try:
        if value is None or isinstance(value, bool):
            return None
        return int(value)
    except (TypeError, ValueError):
        return None


def _parse_user(item: Dict[str, Any]) -> RemoteUser:
    """把 admin 接口返回的一条用户记录解析成 RemoteUser。全部字段按缺失兜底。"""
    groups_raw = item.get("allowed_groups")
    groups: List[str] = []
    if isinstance(groups_raw, list):
        for g in groups_raw:
            if isinstance(g, dict):
                gid = _as_str_id(g.get("id") or g.get("group_id"))
            else:
                gid = _as_str_id(g)
            if gid:
                groups.append(gid)
    elif isinstance(groups_raw, str) and groups_raw.strip():
        groups = [g.strip() for g in groups_raw.split(",") if g.strip()]
    return RemoteUser(
        id=_as_str_id(item.get("id") or item.get("user_id")),
        email=str(item.get("email") or "").strip(),
        username=str(item.get("username") or ""),
        concurrency=_as_int(item.get("concurrency")),
        rpm_limit=_as_int(item.get("rpm_limit")),
        allowed_groups=groups,
        deleted=bool(item.get("deleted_at") or item.get("deleted")),
        raw=item,
    )


def _parse_key(item: Dict[str, Any]) -> IssuedKey:
    return IssuedKey(
        key=str(item.get("key") or item.get("api_key") or ""),
        id=_as_str_id(item.get("id")),
        name=str(item.get("name") or ""),
        group_id=_as_str_id(item.get("group_id")) or None,
    )


# 余额可能出现的字段名。按「越具体越优先」排；找不到就返回 None，
# **绝不猜一个 0 出来**——0 会被界面显示成「余额已用完」，比「暂无数据」误导得多。
_BALANCE_FIELDS = (
    "balance_usd", "remaining_balance", "remaining", "balance",
    "credit_remaining", "credits", "quota_remaining",
)


def _parse_usage(body: Any) -> UsageSnapshot:
    """解析 GET /v1/usage 的**裸 JSON**（不是信封，别去取 body["data"]["..."]）。"""
    if not isinstance(body, dict):
        return UsageSnapshot()
    # 少数实现会多包一层 data/usage，两种都试一遍
    candidates: List[Dict[str, Any]] = [body]
    for key in ("data", "usage"):
        nested = body.get(key)
        if isinstance(nested, dict):
            candidates.append(nested)
    for scope in candidates:
        for name in _BALANCE_FIELDS:
            if name in scope:
                try:
                    return UsageSnapshot(balance_usd=float(scope[name]), raw=body)
                except (TypeError, ValueError):
                    continue
    return UsageSnapshot(raw=body)


def derive_custom_key(server_salt: str, designkit_user_id: int, salt_round: int = 0) -> str:
    """算出这个用户的确定性 custom_key（纯函数，不碰任何状态）。

    形态："dk" + HMAC-SHA256(salt, "<user_id>:<round>") 的前 40 位十六进制 = 42 字符，
    满足 ^[A-Za-z0-9_-]{16,128}$。

    为什么用 HMAC 而不是「dk-用户id」这种可预测值：custom_key 在 Sub2API 是
    **全局唯一、跨租户**的。可预测就意味着任何人都能抢先把 "dk-7" 注册掉，
    从此这个用户永远建不出 Key，而且我们还会一次次撞 409、
    最后把自己撞进「20 次/小时」的锁里。盐必须由调用方传进来（服务端独立派生），
    本模块不去读配置——这样测试里换一把盐是零成本的。

    salt_round 是留给「真的被别人抢注了」的逃生口：加 1 重算得到一个全新的 key。
    注意它只在 T4b 读回确认「那把 key 不是我们的」之后才允许加，
    绝不能因为撞了 429 就换后缀重试（那会把锁坐实一小时）。
    """
    if not server_salt:
        raise ValueError("custom_key 的服务端盐不能为空")
    material = "%d:%d" % (int(designkit_user_id), int(salt_round))
    digest = hmac.new(
        server_salt.encode("utf-8"), material.encode("utf-8"), hashlib.sha256
    ).hexdigest()
    return "dk" + digest[:40]


def default_idempotency_key(custom_key: str) -> str:
    """由 custom_key 推出幂等键。**必须是确定性的，绝不能用 uuid4**。

    幂等键的意义就是「同一件事重放时能被认出来」。每次生成一个新 uuid，
    等于每次都是一件新事，幂等保护完全失效——重放会真的再建一次。

    这里为什么要**再哈希一次**，而不是直接把 custom_key 拼进去：
    custom_key 在 Sub2API 里**就是这把 API Key 本身**（建完之后
    data.key == custom_key，读回时也是拿它去比对）。而幂等键是放在
    `Idempotency-Key` 请求头里发出去的，请求头恰恰是反向代理、
    Nginx access log、抓包工具最爱原样记下来的东西。直接拼等于把用户的
    API Key 明文抄进一堆我们管不着的日志里。
    先过一遍 SHA-256，既保住了确定性，又不泄露原值。
    （这条是自验脚本里「全流程日志里不含 Key 明文」那一项当场抓出来的。）
    """
    digest = hashlib.sha256(custom_key.encode("utf-8")).hexdigest()
    return "dk-key-" + digest[:32]


# ══════════════════════════════════════════════════════════════════
#  代登录的全局节流闸
# ══════════════════════════════════════════════════════════════════
#
# POST /api/v1/auth/login 的限流是 **20 次/分钟/IP**。而 designkit 是服务端
# 代用户去登录的，全站所有 worker 共用同一个出口 IP，也就是共用同一个桶。
# 所以节流必须是**模块级全局**的，不能做成「每个 client 实例一份」——
# 一个进程里有两个 client 实例，各自限 10 次/分钟，合起来就是 20，正好打满。
#
# 取 ≤10 次/分钟（间隔 6 秒）而不是贴着 20：留一半余量给别的用途
# （管理员自己登 Sub2API 后台、深度自检、同一台 NAS 上的其他服务）。
# 打满的后果不只是我们自己失败，是把管理员也一起锁在门外。
#
# 另外「全局串行」还有一层意思：登录有三个真实副作用（消耗限流额度、
# 写审计日志、在 Redis 里堆一个 TTL 30 天的 refresh token 家族），
# 并发去登只会让副作用成倍放大，不会更快。
_LOGIN_MIN_INTERVAL_SEC = 6.0
_login_lock = threading.Lock()
_login_last_at = 0.0  # time.monotonic()


def _login_gate_acquire(wait_timeout: float) -> None:
    """拿到「可以发一次登录请求」的许可；拿不到就抛 E_LOGIN_BUSY（可重试）。

    实现上是「全局锁 + 距上次至少 6 秒」。锁在整个 HTTP 调用期间都持着，
    这就是设计文档要求的「全局串行」。代价是登录会排队，但登录本来就是
    整条链上最稀有的一步（一个用户一辈子就登一两次），排队完全可接受。
    """
    deadline = time.monotonic() + max(0.0, wait_timeout)
    if not _login_lock.acquire(timeout=max(0.0, wait_timeout)):
        raise Sub2ApiError(
            "E_LOGIN_BUSY",
            "同时要开通的用户太多，代登录在排队；稍后会自动重试",
            retryable=True,
        )
    # 锁拿到了，再看节流间隔。这里若超时必须把锁还回去，否则整个进程的
    # 登录能力就永久死锁了——这是最容易写漏的一行。
    try:
        global _login_last_at
        wait = _LOGIN_MIN_INTERVAL_SEC - (time.monotonic() - _login_last_at)
        if wait > 0:
            if time.monotonic() + wait > deadline:
                raise Sub2ApiError(
                    "E_LOGIN_BUSY",
                    "代登录节流排队超时；稍后会自动重试",
                    retryable=True,
                )
            time.sleep(wait)
        _login_last_at = time.monotonic()
    except BaseException:
        _login_lock.release()
        raise


def _login_gate_release() -> None:
    try:
        _login_lock.release()
    except RuntimeError:
        # 没持有锁时 release 会抛 RuntimeError。吞掉：这只可能是代码路径写错了，
        # 而为此让一次本来成功的开通失败，得不偿失。
        logger.warning("登录节流锁重复释放（代码路径异常，不影响本次结果）")


def _reset_login_gate_for_tests() -> None:
    """仅供测试：把节流计时清零，免得每个用例都要真等 6 秒。"""
    global _login_last_at
    _login_last_at = 0.0


# ══════════════════════════════════════════════════════════════════
#  客户端
# ══════════════════════════════════════════════════════════════════

class Sub2ApiClient:
    """Sub2API 的薄客户端。一个实例对应一台 Sub2API。

    admin_key 由管理员在 designkit 网页界面填、加密存库，调用方解密后传进来。
    **不要**在本模块里读环境变量或配置文件去拿它。
    """

    def __init__(
        self,
        base_url: str,
        admin_key: str = "",
        timeout: float = 15.0,
        verify_tls: bool = True,
        login_wait_timeout: float = 120.0,
    ) -> None:
        self.base_url = (base_url or "").strip().rstrip("/")
        self._admin_key = (admin_key or "").strip()
        self.timeout = timeout
        self.verify_tls = verify_tls
        self.login_wait_timeout = login_wait_timeout
        self._client: Optional[httpx.Client] = None
        self._client_lock = threading.Lock()

    # ── 生命周期 ──

    def _http(self) -> httpx.Client:
        with self._client_lock:
            if self._client is None:
                self._client = httpx.Client(
                    timeout=self.timeout,
                    verify=self.verify_tls,
                    # 绝不跟随重定向：跟随会把 Authorization / x-api-key 头
                    # 原样带到另一个地址去。内网环境下一个配错的反代就足够把
                    # 管理员 Key 送到别处，而且日志里看不出任何异常。
                    follow_redirects=False,
                )
            return self._client

    def close(self) -> None:
        with self._client_lock:
            if self._client is not None:
                self._client.close()
                self._client = None

    def __enter__(self) -> "Sub2ApiClient":
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.close()

    # ── 底层请求 ──

    def _require_base_url(self) -> None:
        if not self.base_url:
            raise Sub2ApiError(
                "E_CONFIG",
                "还没有填写 Sub2API 的地址（系统设置 → 网关自动开通）",
                retryable=False,
            )
        if not (self.base_url.startswith("http://") or self.base_url.startswith("https://")):
            raise Sub2ApiError(
                "E_CONFIG",
                "Sub2API 地址必须以 http:// 或 https:// 开头",
                retryable=False,
            )

    def _admin_headers(self) -> Dict[str, str]:
        """admin 接口的鉴权头是 **x-api-key**，不是 Authorization: Bearer。"""
        if not self._admin_key:
            raise Sub2ApiError(
                "E_CONFIG",
                "还没有填写 Sub2API 的管理员 Key（系统设置 → 网关自动开通）",
                retryable=False,
            )
        return {"x-api-key": self._admin_key}

    def _send(
        self,
        method: str,
        path: str,
        op: str,
        headers: Optional[Dict[str, str]] = None,
        json_body: Optional[Dict[str, Any]] = None,
        params: Optional[Dict[str, Any]] = None,
        envelope: bool = True,
        timeout: Optional[float] = None,
    ) -> Any:
        """发一个请求，成功时返回「信封里的 data」（envelope=True）或裸 JSON。

        op 决定错误分类的上下文（同一个状态码在不同接口上含义不同），
        取值见 _classify 的注释。
        """
        self._require_base_url()
        url = self.base_url + path
        try:
            resp = self._http().request(
                method, url,
                headers=headers, json=json_body, params=params,
                timeout=timeout if timeout is not None else self.timeout,
            )
        except httpx.TimeoutException:
            raise Sub2ApiError(
                "E_NETWORK", "连接 Sub2API 超时，稍后自动重试",
                retryable=True,
            )
        except httpx.HTTPError as exc:
            # 异常文本里只有地址，没有请求体，可以安全入日志/入 last_error
            raise Sub2ApiError(
                "E_NETWORK", "连不上 Sub2API（%s）" % type(exc).__name__,
                retryable=True,
            )

        body = _parse_json(resp)
        if logger.isEnabledFor(logging.DEBUG):
            # 请求体和响应体都过一遍脱敏。这是日志防线 ②③。
            logger.debug(
                "sub2api %s %s -> %d, req=%s, resp=%s",
                method, path, resp.status_code,
                json.dumps(_redact(json_body or {}), ensure_ascii=False)[:400],
                json.dumps(_redact(body), ensure_ascii=False)[:400] if body is not None else "<非JSON>",
            )

        if resp.status_code >= 400:
            raise _classify(resp, body, op)

        if not envelope:
            # 网关侧 /v1/usage：裸 JSON，没有 code 可查
            if body is None:
                raise Sub2ApiError(
                    "E_PROTOCOL",
                    "网关的用量接口没有返回 JSON：%s" % _body_message(body, resp),
                    retryable=False,
                )
            return body

        if not isinstance(body, dict):
            raise Sub2ApiError(
                "E_PROTOCOL",
                "Sub2API 返回的不是预期的 {code,message,data} 信封：%s"
                % _body_message(body, resp),
                retryable=False,
            )
        code = body.get("code")
        # code 缺失按成功处理：个别接口成功时不带 code。
        # 注意这里用 `not in _OK_CODES` 而不是 `!= 0`——423 那次 code 是字符串
        # （文件头形态 B），`!= 0` 虽然也能判出「不成功」，但 int() 一类的写法会直接炸。
        if code is not None and code not in _OK_CODES:
            raise _classify(resp, body, op)
        return body.get("data")

    # ══════════════ admin：建号 / 反查 / 查单个 ══════════════

    def admin_create_user(
        self,
        email: str,
        password: str,
        username: str,
        group_id: Optional[str] = None,
        concurrency: int = DEFAULT_CONCURRENCY,
        rpm_limit: int = DEFAULT_RPM_LIMIT,
        notes: str = "designkit auto",
    ) -> RemoteUser:
        """POST /api/v1/admin/users —— 代客建号。

        走 admin 路径而不是 /api/v1/auth/register 的三个理由：
        ① admin 路由组没有那个 5 次/分钟/IP 的注册限流；
        ② 绕开注册开关、邮箱后缀白名单、域名限量、邮件验证、验证码、邀请码全部策略；
        ③ 不受 backend_mode 的注册禁令影响。

        **成功是 200，不是 201。**

        concurrency / rpm_limit / allowed_groups 一律显式传，
        不依赖服务端默认值（原因见 DEFAULT_CONCURRENCY 上面那段注释）。
        建完之后建议立刻用 admin_get_user() 核对一次这几个字段有没有真按我们
        传的值写进去——这是设计文档要求的运行时验证。

        password 明文只在这个函数的参数里活一下，不会被记进任何日志
        （_redact 会把它换成 ***）。
        """
        payload = {
            "email": email,
            "password": password,
            "username": username,
            "role": "user",
            "concurrency": int(concurrency),
            "rpm_limit": int(rpm_limit),
            "allowed_groups": [group_id] if group_id else [],
            "notes": notes,
        }
        data = self._send(
            "POST", _PATH_ADMIN_USERS, op="admin_create",
            headers=self._admin_headers(), json_body=payload,
        )
        item = data if isinstance(data, dict) else {}
        user = _parse_user(item)
        if not user.id:
            raise Sub2ApiError(
                "E_PROTOCOL",
                "Sub2API 说建号成功了，但没返回用户 id，无法继续开通",
                retryable=False,
            )
        logger.info("Sub2API 建号成功：remote_user_id=%s", user.id)
        return user

    def admin_find_user_by_email(
        self, email: str, page_size: int = 100, max_pages: int = 20
    ) -> Optional[RemoteUser]:
        """GET /api/v1/admin/users?search=... —— 按邮箱反查，找不到返回 None。

        **search 是模糊匹配**（具体是不是 LIKE '%x%' 没追到 SQL），
        所以绝对不能直接取第一条：搜 "dk7@designkit.local" 完全可能先命中
        "dk17@designkit.local"。必须在客户端按 email 精确二次比对——
        取错一条的后果是把 Key 建到别人账号上、账单算到别人头上。

        同时跳过软删的记录：Sub2API 的邮箱唯一索引是
        `users_email_unique_active ... WHERE deleted_at IS NULL`，
        同一个邮箱删掉之后可以再建，列表里就可能同时躺着新旧两条。
        """
        target = (email or "").strip().lower()
        if not target:
            return None
        headers = self._admin_headers()
        for page in range(1, max_pages + 1):
            data = self._send(
                "GET", _PATH_ADMIN_USERS, op="admin_read",
                headers=headers,
                params={"search": email, "page": page, "page_size": page_size},
            )
            items = _extract_items(data)
            for item in items:
                user = _parse_user(item)
                if user.deleted:
                    continue
                if user.email.strip().lower() == target and user.id:
                    return user
            if len(items) < page_size:
                break  # 最后一页了
        return None

    def admin_get_user(self, remote_user_id: str) -> RemoteUser:
        """GET /api/v1/admin/users/:id —— 查单个用户。

        两个用途：
        ① 建号后核对 concurrency / rpm_limit / allowed_groups 是不是真按我们传的
           值写进去了（设计文档「仍未查明」第 1、2 条的运行时验证）；
        ② 定期对账：本地存的 remote_user_id 和 remote_email 是否还对得上。
           返回 404（E_USER_NOT_FOUND）或邮箱对不上，说明远端用户被删或被改，
           本地 id 已经悬空，必须整个重新开通。
        """
        data = self._send(
            "GET", "%s/%s" % (_PATH_ADMIN_USERS, remote_user_id), op="admin_read",
            headers=self._admin_headers(),
        )
        item = data if isinstance(data, dict) else {}
        user = _parse_user(item)
        if not user.id:
            # 有些实现 404 会包成 200+空 data，这里一并按「查不到」处理
            raise Sub2ApiError(
                "E_USER_NOT_FOUND",
                "在 Sub2API 上查不到这个用户（id=%s），可能已被删除，需要重新开通"
                % remote_user_id,
                retryable=False,
            )
        return user

    # ══════════════ 代登录 ══════════════

    def login(self, email: str, password: str) -> LoginResult:
        """POST /api/v1/auth/login —— 代用户登录换 JWT。

        这是整条链上**最脆的一步**：它同时依赖「没开验证码、没开 backend mode、
        用户没自己开 TOTP、用户没自己改密码」这几件事，其中后两件是用户自己就能
        触发、designkit 既无法阻止也无法自愈的。所以调用方要有心理准备：
        一小撮用户会永久停在这里，只能转手工发 Key，这是设计的一部分而不是 bug。

        三件必须做对的事：
        1. 全局串行 + ≤10 次/分钟节流（见 _login_gate_acquire 上面那段）；
        2. 成功也要检查 **data.requires_2fa**——它是 200 且 code==0 的「成功」响应，
           不检查的话会拿到一个空的 access_token，然后在建 Key 那步报一个
           莫名其妙的 401，根因永远查不到；
        3. 返回的 access_token 只能放进程内存，**绝不落库**，
           也绝不用 refresh token（它会轮转，多 worker 并发必然互相踢掉）。
        """
        _login_gate_acquire(self.login_wait_timeout)
        try:
            data = self._send(
                "POST", _PATH_LOGIN, op="login",
                json_body={"email": email, "password": password},
            )
        finally:
            _login_gate_release()

        item = data if isinstance(data, dict) else {}
        if item.get("requires_2fa") is True:
            raise Sub2ApiError(
                "E_LOGIN_2FA",
                "该用户在 Sub2API 开启了两步验证，系统无法代他登录；"
                "请管理员在「成员账号」页手工给他填一把 Key",
                retryable=False,
            )
        token = str(item.get("access_token") or "")
        if not token:
            raise Sub2ApiError(
                "E_PROTOCOL",
                "Sub2API 说登录成功了，但没返回访问令牌",
                retryable=False,
            )
        logger.info("Sub2API 代登录成功（令牌已取得，仅存进程内存）")
        return LoginResult(
            access_token=token,
            expires_in=_as_int(item.get("expires_in")),
            requires_2fa=False,
        )

    # ══════════════ 用户侧：建 Key / 读回 ══════════════

    def create_key(
        self,
        jwt: str,
        custom_key: str,
        group_id: Optional[str] = None,
        name: str = "designkit",
        idempotency_key: Optional[str] = None,
    ) -> IssuedKey:
        """POST **/api/v1/keys** —— 给这个用户建一把 API Key。

        路径再强调一次：是 /api/v1/keys。源码里 handler 的注释写的是
        /api/v1/api-keys，那是错的，照注释写会稳定 404。

        请求体的字段和顺序**必须固定**：Sub2API 的幂等指纹是
        sha256(method + route + actor + json(payload))，少传一个可选字段
        （比如 quota / expires_in_days）重放时就会撞 409 IDEMPOTENCY_KEY_CONFLICT。
        所以 quota 和 expires_in_days 即使是 null 也要显式写进去。

        idempotency_key 不传时按 custom_key 确定性推导（见 default_idempotency_key），
        **绝不能用 uuid4**。

        失败里最危险的一个：429 API_KEY_RATE_LIMITED（自定义 key 冲突计数
        20 次/小时/用户）。本模块把 create_key 上的**任何** 429 都标成
        retryable=False，就是为了让调用方不可能「换个后缀再试一次」——
        那一试就把这个用户锁死一小时。
        """
        if not _CUSTOM_KEY_RE.match(custom_key or ""):
            # 这是我们自己的生成逻辑坏了，不是远端的问题。在发出去之前就拦住，
            # 免得白白消耗一次「20 次/小时」的冲突额度。
            raise Sub2ApiError(
                "E_KEY_INVALID",
                "内部错误：生成的 Key 名不符合网关要求，请联系技术支持",
                retryable=False,
            )
        payload = {
            "name": name,
            "custom_key": custom_key,
            "group_id": group_id,
            "quota": None,
            "expires_in_days": None,
        }
        headers = {
            "Authorization": "Bearer " + jwt,
            "Idempotency-Key": idempotency_key or default_idempotency_key(custom_key),
        }
        data = self._send(
            "POST", _PATH_KEYS, op="key_create",
            headers=headers, json_body=payload,
        )
        item = data if isinstance(data, dict) else {}
        issued = _parse_key(item)
        if not issued.key:
            raise Sub2ApiError(
                "E_PROTOCOL",
                "Sub2API 说建 Key 成功了，但没把 Key 返回来",
                retryable=False,
            )
        logger.info("Sub2API 建 Key 成功（末 4 位 %s）", issued.tail)
        return issued

    def list_keys(self, jwt: str, page_size: int = 100, max_pages: int = 20) -> List[IssuedKey]:
        """GET /api/v1/keys —— 列出这个用户名下的所有 Key（含**完整明文**）。

        Sub2API 的 Key 是明文入库、列表原样返回完整 key，所以 409 之后能把
        已经建好的那把原样读回来——这一点是整条链能做到幂等的关键。
        """
        headers = {"Authorization": "Bearer " + jwt}
        out: List[IssuedKey] = []
        for page in range(1, max_pages + 1):
            data = self._send(
                "GET", _PATH_KEYS, op="key_read",
                headers=headers, params={"page": page, "page_size": page_size},
            )
            items = _extract_items(data)
            for item in items:
                out.append(_parse_key(item))
            if len(items) < page_size:
                break
        return out

    def find_key(self, jwt: str, custom_key: str) -> Optional[IssuedKey]:
        """在这个用户名下找出 key == custom_key 的那一条；找不到返回 None。

        409 API_KEY_EXISTS **绝不能直接吞掉当成「已经建好了」**——custom_key 是
        全局唯一的，409 也可能意味着这个值被**别的租户**抢注了。
        必须靠这一步确认那把 key 确实挂在本用户名下。
        返回 None 就是「被别人占了」，调用方该把 salt_round 加 1 重算。
        """
        for issued in self.list_keys(jwt):
            if issued.key == custom_key:
                return issued
        return None

    # ══════════════ 网关侧：冒烟 / 余额 ══════════════

    def gateway_usage(self, api_key: str) -> UsageSnapshot:
        """GET /v1/usage —— 用**这把 Key 自己**做 Bearer 鉴权打网关。

        这是整个开通流程里**唯一真正证明「开通成功」的动作**：
        前面每一步都只能证明 HTTP 200，证明不了这把 Key 真能发出请求去。
        典型的反例就是 403「API Key is not assigned to any group」——
        号建好了、Key 也发了，但一张图也生不出来。

        它走网关鉴权，**不吃面板的限流**，所以也适合拿来做定时余额同步。
        注意**不要**改用 GET /api/v1/usage/stats：那条吃 Global(240rpm) +
        Heavy(60rpm) 两档限流，批量轮询很容易撞上。

        响应是**裸 JSON 不是信封**（文件头形态 D）。
        """
        body = self._send(
            "GET", _PATH_GATEWAY_USAGE, op="gateway",
            headers={"Authorization": "Bearer " + api_key},
            envelope=False,
        )
        return _parse_usage(body)

    # ══════════════ 自检探针（全部只读、零副作用） ══════════════
    #
    # 目标：不建任何账号、不发任何 Key、不改任何余额，就回答
    # 「现在还能不能自动开通」。刻意**不包含真实登录**——真登录会消耗
    # 20/min/IP 的限流额度、写一条审计日志、在 Redis 里堆一个 TTL 30 天的
    # refresh token 家族。想验证代登录只能靠管理员手动点「深度自检」，
    # 那是上层的事，不在这个模块自动跑。

    def probe_public_settings(self) -> ProbeResult:
        """探针 1：GET /api/v1/settings/public（无鉴权）。

        判定：backend mode 或任一验证码开关为 true → 红（代登录必然全断）；
        totp_enabled 为 true → 黄（不是立刻断，但用户随时可能自己开 2FA
        把自己踢出自动化）。
        """
        name = "公开设置"
        try:
            data = self._send("GET", _PATH_PUBLIC_SETTINGS, op="public", envelope=True)
        except Sub2ApiError as exc:
            return ProbeResult(name, "red", "读不到 Sub2API 的公开设置：" + exc.message,
                               {"code": exc.code})
        item = data if isinstance(data, dict) else {}
        backend_mode = bool(item.get("backend_mode_enabled"))
        captchas = {
            "turnstile_enabled": bool(item.get("turnstile_enabled")),
            "tencent_captcha_enabled": bool(item.get("tencent_captcha_enabled")),
            "aliyun_captcha_enabled": bool(item.get("aliyun_captcha_enabled")),
        }
        totp = bool(item.get("totp_enabled"))
        detail = {"backend_mode_enabled": backend_mode, "totp_enabled": totp,
                  "version": item.get("version")}
        detail.update(captchas)
        if backend_mode:
            return ProbeResult(name, "red",
                               "Sub2API 开着「后台模式」，代登录和建 Key 两头都会被拒。"
                               "请管理员到 Sub2API 后台关闭它", detail)
        on = [k for k, v in captchas.items() if v]
        if on:
            return ProbeResult(name, "red",
                               "Sub2API 开着人机验证（%s），系统无法代用户登录。"
                               "请管理员到 Sub2API 后台关闭，或改用手工发 Key" % "、".join(on),
                               detail)
        if totp:
            return ProbeResult(name, "yellow",
                               "Sub2API 允许用户自己开两步验证；开了的那些用户无法自动开通，"
                               "需要手工发 Key", detail)
        return ProbeResult(name, "green", "代登录的环境是通的", detail)

    def probe_admin_compliance(self) -> ProbeResult:
        """探针 2：GET /api/v1/admin/compliance（admin key）。

        这条是 AdminComplianceGuard 的**唯一豁免路径**——即使全站 admin 接口
        都在返回 423，它也能通，所以能用来准确回答「是不是卡在合规确认上」。
        data.version 一变就说明上游推了新版本、所有已签署状态已失效。
        """
        name = "合规确认"
        try:
            data = self._send("GET", _PATH_ADMIN_COMPLIANCE, op="admin_read",
                              headers=self._admin_headers())
        except Sub2ApiError as exc:
            return ProbeResult(name, "red", "查不到合规状态：" + exc.message, {"code": exc.code})
        item = data if isinstance(data, dict) else {}
        required = bool(item.get("required"))
        detail = {"required": required, "version": item.get("version")}
        if required:
            return ProbeResult(name, "red",
                               "Sub2API 要求先签署合规承诺，所有管理接口都被拦住了。"
                               "请管理员登录 Sub2API 后台重新签署一次", detail)
        return ProbeResult(name, "green", "合规承诺已签署", detail)

    def probe_admin_users(self) -> ProbeResult:
        """探针 3：拿一个几乎不可能命中的关键词去搜用户列表。

        一条请求同时验证四件事：admin key 有效、合规守卫没在拦、
        面板限流没打满、响应仍是 {code,message,data} 信封格式。
        搜不到结果是**预期的正常结果**，不是失败。
        """
        name = "管理员 Key"
        try:
            data = self._send(
                "GET", _PATH_ADMIN_USERS, op="admin_read",
                headers=self._admin_headers(),
                params={"search": "__designkit_selfcheck_never_matches__",
                        "page": 1, "page_size": 1},
            )
        except Sub2ApiError as exc:
            return ProbeResult(name, "red", "管理员 Key 不可用：" + exc.message,
                               {"code": exc.code})
        return ProbeResult(name, "green", "管理员 Key 有效，接口格式正常",
                           {"matched": len(_extract_items(data))})

    def probe_gateway_key(self, api_key: Optional[str]) -> ProbeResult:
        """探针 4：拿一个**已经 active 的存量用户**的 Key 打 GET /v1/usage。

        这一项回答的是「已经发出去的 Key 现在还能不能用、分组还在不在」。
        库里一个 active 用户都没有时传 None，结果标 unknown（未验证）——
        **不要标绿**，那会让管理员以为查过了。
        """
        name = "已发 Key 可用性"
        if not api_key:
            return ProbeResult(name, "unknown", "还没有开通成功的用户，这一项没法验证", {})
        try:
            snap = self.gateway_usage(api_key)
        except Sub2ApiError as exc:
            level = "red" if exc.code in ("E_NO_GROUP", "E_KEY_REJECTED") else "yellow"
            return ProbeResult(name, level, "抽查已发的 Key 失败：" + exc.message,
                               {"code": exc.code, "key_tail": _tail(api_key)})
        return ProbeResult(name, "green", "已发出去的 Key 现在可以正常调用网关",
                           {"balance_usd": snap.balance_usd, "key_tail": _tail(api_key)})

    def probe_keys_route(self, jwt: Optional[str]) -> ProbeResult:
        """探针 5：路由前缀漂移。用内存里**已有的**任意一个有效 JWT 打 GET /api/v1/keys。

        内存里没有 JWT 时标 unknown——**绝不要为了这一项去真的登录一次**
        （登录有三个真实副作用，见 login 的注释）。
        """
        name = "建 Key 路由"
        if not jwt:
            return ProbeResult(name, "unknown", "手上没有可用的登录令牌，这一项没法验证（正常）", {})
        try:
            self._send("GET", _PATH_KEYS, op="key_read",
                       headers={"Authorization": "Bearer " + jwt},
                       params={"page": 1, "page_size": 1})
        except Sub2ApiError as exc:
            level = "red" if exc.code == "E_ROUTE_MISSING" else "yellow"
            return ProbeResult(name, level, "%s 探测失败：%s" % (_PATH_KEYS, exc.message),
                               {"code": exc.code})
        return ProbeResult(name, "green", "%s 路由正常" % _PATH_KEYS, {})

    def run_probes(
        self,
        sample_api_key: Optional[str] = None,
        sample_jwt: Optional[str] = None,
    ) -> Tuple[str, List[ProbeResult]]:
        """跑完五项探针，返回 (总体等级, 各项结果)。

        总体判定（照抄设计文档）：任一红 → 红（上层应自动暂停自动开通）；
        无红有黄 → 黄（继续开通，但面板上挂提示）；
        全绿、或只剩「未验证」→ 绿。

        全部只读，跑完约 1 秒。结果缓存 60 秒这件事由上层做，不在这里。
        """
        results = [
            self.probe_public_settings(),
            self.probe_admin_compliance(),
            self.probe_admin_users(),
            self.probe_gateway_key(sample_api_key),
            self.probe_keys_route(sample_jwt),
        ]
        levels = {r.level for r in results}
        if "red" in levels:
            overall = "red"
        elif "yellow" in levels:
            overall = "yellow"
        else:
            overall = "green"
        return overall, results


# ══════════════════════════════════════════════════════════════════
#  错误分类的核心
# ══════════════════════════════════════════════════════════════════

def _classify(resp: httpx.Response, body: Any, op: str) -> Sub2ApiError:
    """把 (状态码 + 响应体形态 + 哪个接口) 映射成一个带分类码的异常。

    op 的取值和含义：
      admin_create —— POST /api/v1/admin/users
      admin_read   —— GET  /api/v1/admin/users[/:id]、/api/v1/admin/compliance
      login        —— POST /api/v1/auth/login
      key_create   —— POST /api/v1/keys
      key_read     —— GET  /api/v1/keys
      gateway      —— GET  /v1/usage（裸 JSON）
      public       —— GET  /api/v1/settings/public

    **为什么必须按 op 分**：同一个 403 在三个接口上是三件完全不同的事——
    admin 上是「管理员 Key 没权限」（告警、停机），
    login 上是「开了后台模式」（全局暂停），
    建 Key 上是「这个用户不许用这个分组」（改配置），
    /v1/usage 上是「这把 Key 没绑分组」（改配置）。
    只看状态码去分类，四种情况会被糊成一句「403 无权限」，谁也不知道该动哪里。
    """
    status = resp.status_code
    tokens = _error_tokens(body)
    detail = _body_message(body, resp)
    after = _retry_after(resp)

    def has(*names: str) -> bool:
        """响应体里是否出现了这些标识之一（子串匹配，兼容 message 里夹带的写法）。"""
        return any(any(n in t for t in tokens) for n in names)

    # ── 429：两种完全不同的东西，绝不能混 ──
    if status == 429:
        if op == "key_create":
            # 建 Key 的 429 是「自定义 key 冲突计数打满」，20 次/小时/用户。
            # 这里**一律**标成不可重试，哪怕响应体里没写 API_KEY_RATE_LIMITED——
            # 按最坏情况兜住。再试一次的代价是把这个用户锁死一小时，
            # 而放弃这一轮的代价只是晚几分钟开通，两边不对等。
            return Sub2ApiError(
                "E_KEY_RATE_LIMITED",
                "建 Key 的次数在网关那边被限制了（每小时上限），已停止重试；"
                "一小时后可在设置页点「重试」，着急的话请管理员手工发一把 Key",
                retryable=False, status=status, retry_after=after,
            )
        # 其余接口的 429 是普通限流。响应体是 {"error":"rate limit exceeded"}
        # 这套**裸格式**（文件头形态 C），没有 code/message/data，
        # 所以上面 _body_message 会从 "error" 字段里取词——这是它兜住的场景之一。
        return Sub2ApiError(
            "E_RATE_LIMITED",
            "Sub2API 限流了（%s），稍后自动重试" % detail,
            retryable=True, status=status, retry_after=after,
        )

    # ── 423：合规未确认。code 是**字符串** ADMIN_COMPLIANCE_ACK_REQUIRED ──
    # （文件头形态 B。这里靠 _error_tokens 把字符串 code 收进来，
    #  所以不会踩到 int(code) 那个坑。）
    if status == 423 or has("ADMIN_COMPLIANCE_ACK_REQUIRED"):
        return Sub2ApiError(
            "E_COMPLIANCE",
            "Sub2API 要求先签署合规承诺，管理接口全部被拦住了。"
            "请管理员登录 Sub2API 后台重新签署一次，然后回来点「重试」",
            retryable=False, status=status, halt_all=True,
        )

    # ── 404：路由漂移。照 handler 注释写成 /api/v1/api-keys 就是这个下场 ──
    if status == 404:
        if op == "admin_read":
            return Sub2ApiError(
                "E_USER_NOT_FOUND",
                "在 Sub2API 上找不到这个对象（可能已被删除）",
                retryable=False, status=status,
            )
        return Sub2ApiError(
            "E_ROUTE_MISSING",
            "Sub2API 的接口地址不存在（404）。多半是 Sub2API 升级后挪了路由，"
            "或者网关地址填错了",
            retryable=False, status=status, halt_all=True,
        )

    # ── 代登录的两种「环境级」失败：必须在按状态码分流之前先认出来 ──
    # 坑在这里：这两种失败**不一定是 401/403**。实测契约里
    # TURNSTILE_NOT_CONFIGURED 这类验证码错误是随 **400** 回来的，
    # 如果只在 401/403 分支里认它，就会被下面的 400 分支糊成
    # 「E_BAD_REQUEST：Sub2API 拒绝了这次请求」——于是管理员看到的建议是
    # 「核对邮箱域名」，而真正该做的是去关掉人机验证，方向完全反了。
    # 所以这里对 login 的整个 4xx 段统一先匹配标识。
    if op == "login" and 400 <= status < 500:
        if has("BACKEND_MODE"):
            return Sub2ApiError(
                "E_BACKEND_MODE",
                "Sub2API 开着「后台模式」，普通用户不能登录，自动开通已暂停。"
                "请管理员到 Sub2API 后台关闭后台模式",
                retryable=False, status=status, halt_all=True,
            )
        if has("TURNSTILE", "CAPTCHA"):
            return Sub2ApiError(
                "E_CAPTCHA_ON",
                "Sub2API 开着人机验证，系统无法代用户登录，自动开通已暂停。"
                "请管理员到 Sub2API 后台关闭人机验证，或改用手工发 Key",
                retryable=False, status=status, halt_all=True,
            )

    # ── 409：三种冲突，含义天差地别 ──
    if status == 409:
        if has("IDEMPOTENCY_IN_PROGRESS"):
            return Sub2ApiError(
                "E_IDEMPOTENCY_BUSY",
                "同一个请求正在处理中，稍后原样重发",
                retryable=True, status=status, retry_after=after,
            )
        if has("IDEMPOTENCY_KEY_CONFLICT"):
            # 同一个幂等键配了不同的 body。这只可能是我们自己少传/多传了字段。
            return Sub2ApiError(
                "E_IDEMPOTENCY_CONFLICT",
                "内部错误：同一个幂等键对应了不同的请求内容，请联系技术支持",
                retryable=False, status=status,
            )
        if op == "admin_create" or has("EMAIL_EXISTS"):
            # 预期分支：这个邮箱已经建过了。调用方应当去反查拿 id，而不是当失败。
            return Sub2ApiError(
                "E_EMAIL_EXISTS",
                "这个邮箱在 Sub2API 已经存在（正常情况，改为反查已有账号）",
                retryable=False, status=status,
            )
        if op == "key_create" or has("API_KEY_EXISTS"):
            # 预期分支：这个 custom_key 已存在。**绝不能直接当成「已经建好了」**，
            # 必须用 find_key 确认那把 key 确实在本用户名下（可能被别的租户抢注）。
            return Sub2ApiError(
                "E_KEY_EXISTS",
                "这个 Key 名已存在（正常情况，改为读回已有的那把）",
                retryable=False, status=status,
            )
        return Sub2ApiError(
            "E_BAD_REQUEST", "Sub2API 报冲突：%s" % detail,
            retryable=False, status=status,
        )

    # ── 401 / 403：同一个码在四个接口上是四件事 ──
    if status in (401, 403):
        if op == "gateway":
            # 网关侧。403「API Key is not assigned to any group」是
            # 「建了号但一张图也发不出去」的典型形态。
            if status == 403 or has("GROUP", "NOT ASSIGNED"):
                return Sub2ApiError(
                    "E_NO_GROUP",
                    "这把 Key 没有绑定任何分组，网关不受理它的请求。"
                    "请管理员在 Sub2API 后台把该用户加进目标分组，"
                    "并核对设置页填的分组 id 是否正确",
                    retryable=False, status=status,
                )
            return Sub2ApiError(
                "E_KEY_REJECTED",
                "网关不认这把 Key（可能已被删除或停用），需要重新开通",
                retryable=False, status=status,
            )
        if op == "login":
            # BACKEND_MODE / 验证码这两种已经在上面那个「login 的整个 4xx 段」里
            # 先认掉了，走到这里的 401/403 就只剩密码对不上：
            # 用户自己在 Sub2API 改过密码。
            # admin 侧多半没有改密接口，所以这条**不可自愈**，直接转手工。
            return Sub2ApiError(
                "E_PASSWORD_MISMATCH",
                "这个用户在 Sub2API 的密码和系统里记录的对不上（多半是他自己改过），"
                "无法代他登录；请管理员手工给他填一把 Key",
                retryable=False, status=status,
            )
        if op == "key_create":
            if has("GROUP_NOT_ALLOWED", "GROUP"):
                return Sub2ApiError(
                    "E_GROUP_DENIED",
                    "这个用户不被允许使用目标分组。请管理员核对设置页填的分组 id，"
                    "并确认该用户的可用分组里包含它",
                    retryable=False, status=status,
                )
            # 建 Key 的 401 是 JWT 过期/无效。JWT 是内存缓存的，重登一次就好，
            # 所以这里可以重试（调用方会先丢掉缓存的令牌）。
            return Sub2ApiError(
                "E_KEY_REJECTED",
                "登录令牌失效，需要重新登录后再试",
                retryable=True, status=status,
            )
        if op == "key_read":
            return Sub2ApiError(
                "E_KEY_REJECTED", "登录令牌失效，需要重新登录后再试",
                retryable=True, status=status,
            )
        # admin_create / admin_read / public
        return Sub2ApiError(
            "E_ADMIN_AUTH",
            "Sub2API 的管理员 Key 无效或没有权限（%s）。"
            "请管理员到系统设置里重新填一次" % detail,
            retryable=False, status=status, halt_all=True,
        )

    # ── 400 / 422：参数被策略拒了 ──
    if status in (400, 422):
        if has("API_KEY_INVALID_CHARS", "API_KEY_TOO_SHORT", "INVALID_CHARS", "TOO_SHORT"):
            return Sub2ApiError(
                "E_KEY_INVALID",
                "内部错误：生成的 Key 名不符合网关要求，请联系技术支持",
                retryable=False, status=status,
            )
        return Sub2ApiError(
            "E_BAD_REQUEST",
            "Sub2API 拒绝了这次请求：%s。"
            "常见原因是邮箱后缀不在白名单、域名建号数量到上限，"
            "或用的是保留域名——请管理员核对设置页里填的邮箱域名" % detail,
            retryable=False, status=status,
        )

    # ── 5xx ──
    if status >= 500:
        if has("IDEMPOTENCY_STORE_UNAVAILABLE"):
            return Sub2ApiError(
                "E_IDEMPOTENCY_BUSY",
                "网关的幂等存储暂时不可用，稍后原样重发",
                retryable=True, status=status, retry_after=after,
            )
        # 500 且读不出任何原因，在建号这一步有个特定的坑：密码超过 bcrypt 的
        # 72 字节硬上限会被包成一个 reason 为空的 internal error，伪装成服务端故障。
        # 本设计的密码固定 43 字符，不该出现；真出现了要当代码 bug 查，别无脑重试。
        if op == "admin_create" and status == 500 and not tokens:
            logger.warning("建号返回了没有原因的 500——按契约不该出现，请检查参数生成逻辑")
        return Sub2ApiError(
            "E_SERVER",
            "Sub2API 内部错误（HTTP %d）：%s，稍后自动重试" % (status, detail),
            retryable=True, status=status, retry_after=after,
        )

    # ── 兜底 ──
    return Sub2ApiError(
        "E_PROTOCOL",
        "Sub2API 返回了预料之外的结果（HTTP %d）：%s" % (status, detail),
        retryable=False, status=status,
    )
