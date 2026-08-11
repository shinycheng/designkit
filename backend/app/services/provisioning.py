"""代客在 Sub2API 上开通账号的**可重入状态机**（表见 models.UserGatewayAccount）。

这个模块回答一个问题：「这一行现在该往前走哪一步」。怎么发请求、错在哪一类，
全在 services/sub2api.py；这里只做状态流转、落库、失败分流和退避排期。

════════════════════════════════════════════════════════════════════
 可重入是这个文件的第一需求，不是优化项
════════════════════════════════════════════════════════════════════
worker 随时可能被杀（NAS 断电、容器重启、部署滚动），所以**任何一步都可能
执行到一半就没了**：请求发出去了、响应没收到、库还没写。重跑必须收敛到同一个
结果，而不是每重跑一次就在 Sub2API 上多留一个账号、多发一把 Key。

四条硬保证，改这个文件的人必须逐条守住：

1. **remote_email / remote_password_enc 一旦写入就永不重新生成。**
   重跑时若重新摇一次密码，远端那个已经建好的账号密码就对不上了，
   而 Sub2API 的 admin 侧很可能没有改密接口（设计文档「仍未查明」第 3 条），
   等于这个账号永久失联——既登不进去，也不能删了重建（删是软删）。
   代码上的体现：只有 ensure_account() 会写这两列，且只在它们为空时写。

2. **api_key_enc 只在为 NULL 时写入，永不覆盖。**
   这是「不能把已经拿到的 Key 冲掉」的硬保证。一旦覆盖，旧 Key 还挂在
   Sub2API 上继续计费，而本地已经认不出它了——账单上多一笔谁也认领不了的钱。

3. **409 绝不直接吞掉当成「已经建好了」。**
   建号的 409 必须反查确认那个账号的邮箱确实是我们的；建 Key 的 409 必须
   在本用户名下把那把 key 读回来核对。custom_key 是**跨租户全局唯一**的，
   409 完全可能是「被别人抢注了」——直接吞掉的后果是把别人的 Key 存进我们库里。

4. **全程不调用任何删除接口，也不碰任何充值接口。**
   删是软删（明文不可恢复），充值重复调用会真的重复加钱。这两类接口在本模块里
   一次都不出现，所以「重复扣钱」这一整类风险是结构性地不存在，不是靠小心。

════════════════════════════════════════════════════════════════════
 降级是一等公民
════════════════════════════════════════════════════════════════════
注册链路和开通链路彻底解耦：注册只调 ensure_account()（纯本地写一行），
写完立刻返回。Sub2API 挂了、升级了、接口全改了，表现都必须是
「所有人照常注册、照常登录、照常进工作台，只是生图要等管理员发一把 Key」，
绝不能变成「新用户一个都进不来」。所以本模块里**任何函数都不允许**在注册路径上
发起网络请求，ensure_account() 里一行 HTTP 都没有。

管理员手工填 Key 的通道（user_gateway.upsert_key，「成员账号」页）永远保留。
自动开通只是省掉那一步，不是取代它——所有失败路径的终点都是 manual。

════════════════════════════════════════════════════════════════════
 密码只活很短一会儿
════════════════════════════════════════════════════════════════════
明文密码只在 _password_of() 这个上下文管理器的作用域里存在，用完即弃：
不赋给 ORM 属性、不进模块级缓存、不进任何会被整体序列化的 dict。
全模块只有两处调用它（建号、代登录），一眼能数完。

sub2api_keep_password 默认 false：状态转成 active（冒烟通过）之后**立刻**清空
remote_password_enc。理由是取回 Key 只要管理员 Key，只有「建 Key」那一次需要
用户密码——清掉之后 designkit 长期保管的就只是一堆 Key，而不是一堆可登录凭据。
代价要说清楚：清空后这一行不可能再重建 Key，将来只能走 manual。
所以清空必须严格发生在 active 之后，**绝不能提前**。
"""
import logging
import secrets
import threading
from contextlib import contextmanager
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Any, Dict, Iterator, List, Optional, Tuple

from sqlalchemy.exc import IntegrityError
from sqlalchemy.orm import Session

from .. import config
from ..models import UserGatewayAccount, User, utcnow
from . import secrets_box, settings_service, sub2api, user_gateway
from .sub2api import Sub2ApiClient, Sub2ApiError

logger = logging.getLogger("designkit.provisioning")


# ══════════════════════════════════════════════════════════════════
#  设置项：键名和默认值集中在这里
# ══════════════════════════════════════════════════════════════════
#
# 这些键要由 config.RUNTIME_DEFAULTS 提供（设置页那个任务负责加），本模块只读。
# 之所以在这里再写一份默认值：config 里还没加的那段时间，本模块也要能跑
# ——读不到就用这里的默认值，表现是「自动开通没开」，而不是抛 KeyError
# 把注册链路一起带崩。降级是一等公民，这条从读配置就开始守。
PROVISIONING_SETTING_DEFAULTS: Dict[str, Any] = {
    # 总开关。默认 false：升级当天行为与今天逐字一致，管理员填完地址和管理员 Key
    # 再自己打开，不会有人在毫不知情的情况下被自动建号。
    "sub2api_auto_provision": False,
    # Sub2API 的地址，形如 http://192.168.31.235:3000
    "sub2api_base_url": "",
    # 管理员 Key。**必须进 settings_service.SECRET_KEYS 打码**，
    # 由管理员自己在网页界面填，不写进代码、配置文件或 .env 模板。
    "sub2api_admin_key": "",
    # 目标分组 id。设计文档「仍未查明」第 4 条：admin 侧有没有列分组的接口没查明，
    # 所以先让管理员手填一个数字。填错的表现是建 Key 403 GROUP_NOT_ALLOWED
    # 或冒烟 403「没绑分组」，两条都有明确的人话提示。
    "sub2api_group_id": "",
    # 合成邮箱的域名。只用 [a-z0-9.-]，绝不含 '+'
    #（Sub2API 对所有域名都做 stripEmailPlusSuffix 别名去重，带 + 的会被并成同一个人）。
    "sub2api_email_domain": "designkit.local",
    # 是否长期保管用户在 Sub2API 的密码。默认 false，见文件头。
    "sub2api_keep_password": False,
    # 自动开通最多失败几次就转手工。5 次配合下面的退避阶梯大约覆盖 15 小时，
    # 足够跨过一次夜间维护窗口，又不会无限重试下去。
    "sub2api_max_attempts": 5,
    # 单次 HTTP 超时（秒）。面板接口都很快，15 秒足够；给大了会让 worker 卡住。
    "sub2api_timeout": 15,
    # https 自签证书的内网实例可能要关掉校验。默认开，关掉要管理员自己点。
    "sub2api_verify_tls": True,
}

# 其中哪些是密钥，接口返回前必须打码。设置页任务把它并进 settings_service.SECRET_KEYS。
PROVISIONING_SECRET_SETTING_KEYS = ("sub2api_admin_key",)


# ══════════════════════════════════════════════════════════════════
#  本地分类码：sub2api.ERROR_CODES 之外，状态机自己会产生的失败
# ══════════════════════════════════════════════════════════════════
#
# 这几种失败根本没发出去过请求（或者是本地数据的问题），所以不属于
# HTTP 客户端的分类表。合并成一张总表，是为了让 last_error 里出现的分类码
# 永远能在一张表里查到人话——运营看到 "E_LOCAL_NO_PASSWORD" 却查不到是什么，
# 和没有报错是一样的。
LOCAL_ERROR_CODES: Dict[str, str] = {
    "E_LOCAL_CONFIG": "designkit 这边还没把网关自动开通配置齐（地址/管理员 Key/分组 id）",
    "E_LOCAL_NO_PASSWORD": "本地没有这个用户在 Sub2API 的密码，无法代他登录建 Key",
    "E_LOCAL_EMAIL_GHOST": "远端说这个邮箱已存在，但反查不到——多半是被软删了，需要人工处理",
    "E_LOCAL_KEY_SQUATTED": "算出来的 Key 名被别的租户占了，换了几轮仍然占着",
    "E_LOCAL_MAX_ATTEMPTS": "自动开通连续失败次数已达上限，已转为等管理员手工发 Key",
    "E_LOCAL_PAUSED": "自动开通被暂停了（自检发现前提条件不满足）",
}

# 分类码总表 = 客户端的 + 本地的。写 last_error 之前一律 assert 一下，防手滑拼错。
ALL_ERROR_CODES: Dict[str, str] = dict(sub2api.ERROR_CODES)
ALL_ERROR_CODES.update(LOCAL_ERROR_CODES)

# 哪些分类码可以自动退避重试。
#
# 为什么要在这里再列一张表，而不是直接用 Sub2ApiError.retryable：
# retryable 只在异常对象上活着，进程一重启就没了；而重试排期是**下一次** worker
# 轮询时才做的决定，那时手上只有库里的 last_error 字符串。所以「能不能重试」
# 必须能从分类码本身还原出来。
#
# 这张表能成立的前提是「同一个分类码的可重试性是恒定的」——sub2api.py 特意把
# 建 Key 的 429 单独分成 E_KEY_RATE_LIMITED（永不重试），就是为了不破坏这个前提。
# 加新码时先问一句：它在所有接口上都是同一个答案吗？
RETRYABLE_ERROR_CODES = frozenset({
    "E_NETWORK",            # 网络不通/超时，多半是 NAS 在重启
    "E_SERVER",             # 5xx
    "E_RATE_LIMITED",       # 普通限流（**不含**建 Key 的 429）
    "E_IDEMPOTENCY_BUSY",   # 幂等存储忙，原样重发即可
    "E_KEY_REJECTED",       # 只在建 Key/读 Key 时表示 JWT 过期，重登一次就好
    "E_LOGIN_BUSY",         # 本地登录节流排队超时
})

# 这几种「失败」不该记进 attempts：它们不是「远端拒绝了我们」，而是
# 「现在压根不该跑」。要是也算一次，管理员忘了填分组 id 的那五分钟里，
# 全站用户会被 worker 一路刷到 attempts 上限、集体降级成 manual，
# 而他们本来什么问题都没有。
NON_COUNTING_ERROR_CODES = frozenset({
    "E_LOCAL_CONFIG",
    "E_LOCAL_PAUSED",
    "E_LOGIN_BUSY",
})

# 退避阶梯（秒），下标 = 已失败次数。1 分钟 / 5 分钟 / 30 分钟 / 2 小时 / 12 小时。
# 前两档密是为了兜住「网关刚好在重启」这种几分钟就自愈的情况；
# 后面拉长是因为再往后基本都是要人来处理的问题，密集重试只会刷日志。
_BACKOFF_LADDER = (60, 300, 1800, 7200, 43200)

# 需要 worker 继续推进的状态（active/manual/disabled 都不在里面）
_IN_FLIGHT_STATES = ("pending", "user_created", "key_issued")


# ══════════════════════════════════════════════════════════════════
#  全局暂停闸：一个用户失败 vs 整套前提没了
# ══════════════════════════════════════════════════════════════════
#
# halt_all 类错误（后台模式、验证码开着、合规未签、管理员 Key 失效、路由 404）
# 意味着**每一个**用户都会以同样的方式失败。不拦住的话，worker 会挨个把全站用户
# 刷到 attempts 上限，等管理员发现问题时人已经全被降级成 manual 了——
# 而这本来是一个改一个开关就能修好的问题。
#
# 这是进程内存里的闸，重启会清零。持久的那个开关是设置项
# sub2api_auto_provision，由设置页任务提供；这里在能写的时候顺手也写一下
# （见 _persist_pause），写不写得进去都不影响内存闸生效。
_halt_lock = threading.Lock()
_halt_reason: str = ""
_halt_at: Optional[datetime] = None


def is_halted() -> bool:
    """自动开通是否被本进程的全局闸暂停了。"""
    with _halt_lock:
        return bool(_halt_reason)


def halt_reason() -> Tuple[str, Optional[datetime]]:
    """返回（暂停原因的人话，暂停时间）。没暂停时是 ("", None)。"""
    with _halt_lock:
        return _halt_reason, _halt_at


def set_halt(reason: str) -> None:
    """置位全局暂停闸。reason 会显示在设置页上，要写「该去哪里点什么」。"""
    global _halt_reason, _halt_at
    with _halt_lock:
        if _halt_reason == reason:
            return
        _halt_reason = reason
        _halt_at = utcnow()
    logger.error("自动开通已全局暂停：%s", reason)


def clear_halt() -> None:
    """管理员处理完之后手动解除暂停（设置页的按钮调这里）。"""
    global _halt_reason, _halt_at
    with _halt_lock:
        _halt_reason = ""
        _halt_at = None


def _reset_state_for_tests() -> None:
    """测试用：清掉全局闸和用户锁，让每个用例都从干净状态开始。"""
    clear_halt()
    with _user_locks_guard:
        _user_locks.clear()


# 每个用户一把锁：同一个用户不允许两个线程同时推进，否则两边都看到
# api_key_enc 为 NULL，就会各建一把 Key（其中一把从此没人认领）。
#
# 说清楚这把锁**管不到什么**：它只在一个进程内有效。多进程/多容器同时跑 worker 时，
# 真正兜底的是「确定性 + 冲突读回」——邮箱是确定性的（重复建号只会 409），
# custom_key 是确定性的（重复建 Key 只会 409 然后读回同一把）。
# 也就是说锁只是省掉几次无谓的 409，不是正确性的依赖。这一点别搞反了。
_user_locks: Dict[int, threading.Lock] = {}
_user_locks_guard = threading.Lock()


def _lock_for(user_id: int) -> threading.Lock:
    with _user_locks_guard:
        lock = _user_locks.get(user_id)
        if lock is None:
            lock = threading.Lock()
            _user_locks[user_id] = lock
        return lock


# ══════════════════════════════════════════════════════════════════
#  配置
# ══════════════════════════════════════════════════════════════════

@dataclass
class ProvisioningConfig:
    """一次开通要用到的全部配置。admin_key 标了 repr=False，防止被打进日志。"""
    enabled: bool = False
    base_url: str = ""
    admin_key: str = field(default="", repr=False)
    group_id: str = ""
    email_domain: str = "designkit.local"
    keep_password: bool = False
    max_attempts: int = 5
    timeout: float = 15.0
    verify_tls: bool = True

    def missing(self) -> str:
        """缺什么？返回一句人话；配齐了返回空串。

        分组 id 也算必填项：设计文档「仍未查明」第 2 条说未绑分组的 Key 很可能
        一张图都发不出去，所以宁可在这里拦住，也不要建出一堆废账号来。
        """
        if not self.base_url:
            return "还没填 Sub2API 的地址"
        if not self.admin_key:
            return "还没填 Sub2API 的管理员 Key"
        if not self.group_id:
            return "还没填目标分组 id"
        return ""


def _setting(db: Session, key: str) -> Any:
    """读一个设置项；config.RUNTIME_DEFAULTS 里还没这个键时用本模块的默认值。"""
    if key in config.RUNTIME_DEFAULTS:
        value = settings_service.get(db, key)
        if value is not None:
            return value
    return PROVISIONING_SETTING_DEFAULTS.get(key)


def _as_bool(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    return str(value or "").strip().lower() in ("1", "true", "yes", "on")


def load_config(db: Session) -> ProvisioningConfig:
    """把设置项读成一个 ProvisioningConfig。**含明文管理员 Key，不许缓存。**

    不缓存的理由和 settings_service 文件头是同一个：管理员在界面上换了一把 Key，
    下一次开通就必须用新的那把；缓存住了会表现成「明明改了还是报 Key 无效」。
    """
    domain = str(_setting(db, "sub2api_email_domain") or "").strip().lower()
    domain = _sanitize_domain(domain)
    try:
        max_attempts = int(_setting(db, "sub2api_max_attempts") or 5)
    except (TypeError, ValueError):
        max_attempts = 5
    try:
        timeout = float(_setting(db, "sub2api_timeout") or 15)
    except (TypeError, ValueError):
        timeout = 15.0
    return ProvisioningConfig(
        enabled=_as_bool(_setting(db, "sub2api_auto_provision")),
        base_url=str(_setting(db, "sub2api_base_url") or "").strip().rstrip("/"),
        admin_key=str(_setting(db, "sub2api_admin_key") or "").strip(),
        group_id=str(_setting(db, "sub2api_group_id") or "").strip(),
        email_domain=domain,
        keep_password=_as_bool(_setting(db, "sub2api_keep_password")),
        max_attempts=max(1, min(20, max_attempts)),
        timeout=max(3.0, min(120.0, timeout)),
        verify_tls=_as_bool(_setting(db, "sub2api_verify_tls")),
    )


# 保留域名后缀：Sub2API 有 4 个 *-connect.invalid 的保留后缀，而且这份名单会增长。
# 用了它建号会被策略拒（400），而且从错误信息里完全看不出是域名的问题。
_FORBIDDEN_DOMAIN_SUFFIXES = (".invalid",)


def _sanitize_domain(domain: str) -> str:
    """把管理员填的域名收拾干净；不合法就回落到默认值。

    只允许 [a-z0-9.-]：邮箱里出现 '+' 会被 Sub2API 的 stripEmailPlusSuffix
    当成别名去重，两个用户会被并成同一个远端账号——账单直接算错人头。
    """
    cleaned = "".join(ch for ch in domain if ch.isalnum() or ch in ".-").strip(".-")
    if not cleaned or "." not in cleaned:
        return "designkit.local"
    for suffix in _FORBIDDEN_DOMAIN_SUFFIXES:
        if cleaned.endswith(suffix):
            logger.warning("邮箱域名 %s 用了保留后缀，已回落到 designkit.local", cleaned)
            return "designkit.local"
    return cleaned


def _custom_key_salt() -> str:
    """custom_key 的服务端盐：从加密钥匙独立派生一份，不复用它本身。

    独立派生（换一个 info 串）的意义是：即使某天这个盐因为要排错被打印出来，
    也推不回 data/.enc_key，库里的密文不会跟着遭殃。

    盐必须**跨重启稳定**，否则重跑会算出一个全新的 custom_key，
    于是每次重启都在 Sub2API 上多发一把 Key。ENCRYPTION_KEY 是文件落盘的，
    满足这一点；.enc_key 丢了的话本来所有密文也都解不开了，不是这里的问题。
    """
    material = (config.ENCRYPTION_KEY or "").strip()
    if not material:
        raise Sub2ApiError(
            "E_LOCAL_CONFIG",
            "加密钥匙是空的，无法生成 Key 名：请检查 data/.enc_key 文件",
            retryable=False,
        )
    return "provisioning-custom-key-salt-v1|" + material


# ══════════════════════════════════════════════════════════════════
#  结果对象
# ══════════════════════════════════════════════════════════════════

@dataclass
class StepResult:
    """跑一轮状态机的结果。给 worker 和设置页看，不含任何凭据。"""
    user_id: int
    state: str                 # 跑完之后这一行的状态
    outcome: str               # active / progress / failed / skipped
    changed: bool = False      # 这一轮有没有改动过库
    error_code: str = ""       # 失败时的分类码（在 ALL_ERROR_CODES 里）
    message: str = ""          # 人话，会原样显示给管理员
    halt_all: bool = False     # 这次失败是不是「整套前提没了」

    @property
    def ok(self) -> bool:
        return self.outcome in ("active", "progress")


# ══════════════════════════════════════════════════════════════════
#  T1：注册时登记（纯本地，一行 HTTP 都没有）
# ══════════════════════════════════════════════════════════════════

def ensure_account(
    db: Session, user_id: int, cfg: Optional[ProvisioningConfig] = None
) -> UserGatewayAccount:
    """给一个用户登记网关账号行；已经有了就原样返回，**绝不覆盖任何字段**。

    注册接口调这里，调完立刻返回——这是「降级第一层」的结构保证：
    这个函数不发任何网络请求，所以 Sub2API 无论怎么坏，都不影响任何人注册。

    自动开通没开时，登记成 manual（含义是「Key 的来源是人」），
    而且**不生成密码**——没打算自动开通就没必要造一份可登录凭据存着。
    管理员后来打开了开关，用 resume_auto() 把这一行拉回 pending。

    不 commit，由调用方（注册接口）统一提交：注册本来就是一个事务，
    分开提交会出现「用户建好了、网关行没写上」的半截数据。
    """
    account = user_gateway.get_account(db, user_id)
    if account is not None:
        return account
    if cfg is None:
        cfg = load_config(db)

    account = UserGatewayAccount(user_id=user_id, provider="sub2api")
    if cfg.enabled:
        account.state = "pending"
        account.remote_email = _make_remote_email(user_id, cfg.email_domain)
        # 43 个 URL-safe 字符：远高于 Sub2API 的 min=6，远低于 bcrypt 的 72 字节
        # 硬上限（超了会变成一个 reason 为空的 500，伪装成服务端故障骗你一直重试）。
        # 绝不从用户在 designkit 的密码或 user_id 派生——一旦可预测，
        # 任何拿到源码的人就能登录所有用户的 Sub2API 账号。
        account.remote_password_enc = secrets_box.encrypt(secrets.token_urlsafe(32))
    else:
        account.state = "manual"
    account.attempts = 0
    account.last_error = ""
    db.add(account)
    try:
        # 并发注册/重复调用时 user_id 上的唯一索引会拦住第二条。
        # 用 SAVEPOINT 包住，撞了就回滚这一小段并读回已有的那行——
        # 绝不能让它把整个注册事务带崩，更不能重新生成一份凭据覆盖上去。
        with db.begin_nested():
            db.flush()
    except IntegrityError:
        existing = user_gateway.get_account(db, user_id)
        if existing is None:
            raise
        logger.info("用户 %s 的网关账号行已存在，沿用原有登记", user_id)
        return existing
    logger.info("已登记用户 %s 的网关账号（状态 %s）", user_id, account.state)
    return account


def _make_remote_email(user_id: int, domain: str) -> str:
    """合成远端邮箱：dk<用户id>@<域名>。确定性——重跑必须算出同一个值。

    只用 [a-z0-9]，**绝不含 '+'**：Sub2API 对所有域名都做 stripEmailPlusSuffix
    别名去重，dk7+x@ 和 dk7@ 会被认成同一个人。

    这里**再过一次** _sanitize_domain，虽然 load_config 已经洗过了：
    ProvisioningConfig 是个普通 dataclass，谁都能直接构造一个出来（测试、
    将来的自检页、别的调用方），一旦绕开 load_config，保留后缀和 '+' 就会
    直接进到邮箱里。合成邮箱只有这一个出口，把校验钉在出口上才防得住。
    """
    return "dk%d@%s" % (int(user_id), _sanitize_domain(str(domain or "").strip().lower()))


@contextmanager
def _password_of(account: UserGatewayAccount) -> Iterator[str]:
    """把这一行的远端密码解成明文，只在 with 块里有效。

    做成上下文管理器不是为了「更安全」（Python 的字符串擦不掉），
    而是为了让明文的**作用域一眼可见**：全模块只有两处 with，
    审代码的人能在三秒内数完哪里碰过密码。

    解不开（多半是 data/.enc_key 换了一把）按不可重试处理：再试一百次也一样。
    """
    if not account.remote_password_enc:
        raise Sub2ApiError(
            "E_LOCAL_NO_PASSWORD",
            "系统里没有这个用户在网关的登录密码，无法自动建 Key；"
            "请管理员在「成员账号」页手工给他填一把 Key",
            retryable=False,
        )
    try:
        plaintext = secrets_box.decrypt(account.remote_password_enc)
    except secrets_box.SecretsBoxError as exc:
        # 异常消息本身不含密文，可以安全入 last_error（见 secrets_box.SecretsBoxError）
        raise Sub2ApiError(
            "E_LOCAL_NO_PASSWORD", "网关登录密码解密失败：%s" % exc, retryable=False,
        )
    try:
        yield plaintext
    finally:
        del plaintext


# ══════════════════════════════════════════════════════════════════
#  跑一轮状态机
# ══════════════════════════════════════════════════════════════════

def provision_user(
    db: Session,
    user_id: int,
    client: Optional[Sub2ApiClient] = None,
    cfg: Optional[ProvisioningConfig] = None,
) -> StepResult:
    """把一个用户的开通往前推进到底（能推多远推多远），返回这一轮的结果。

    **从任何状态进来都是安全的**：每一步都先看库里已经有什么，
    只做「确认已存在」而不是「重新创建」。断电重跑、并发重跑、手动重跑，
    收敛到的结果都一样。

    client 传进来是为了让一批用户共用一条连接（也方便测试注入假服务器）；
    不传就按配置现造一个，用完关掉。
    """
    if cfg is None:
        cfg = load_config(db)

    lock = _lock_for(user_id)
    if not lock.acquire(blocking=False):
        # 另一个线程正在推进这个用户。直接跳过而不是排队：排队等于把 worker 的
        # 并发额度占着干等，而这一轮跳过去，下一轮自然会重新领到。
        return StepResult(user_id, _current_state(db, user_id), "skipped",
                          message="这个用户正在开通中，本轮跳过")
    owns_client = False
    try:
        account = user_gateway.get_account(db, user_id)
        if account is None:
            return StepResult(user_id, "", "skipped", message="这个用户还没有网关账号行")

        skip = _skip_reason(account, cfg)
        if skip:
            return StepResult(user_id, account.state, "skipped", message=skip)

        if client is None:
            client = build_client(cfg)
            owns_client = True

        return _run_steps(db, account, client, cfg)
    finally:
        if owns_client and client is not None:
            client.close()
        lock.release()


def build_client(cfg: ProvisioningConfig) -> Sub2ApiClient:
    """按配置造一个客户端。管理员 Key 只从配置传进去，不落任何全局变量。"""
    return Sub2ApiClient(
        base_url=cfg.base_url,
        admin_key=cfg.admin_key,
        timeout=cfg.timeout,
        verify_tls=cfg.verify_tls,
    )


def _current_state(db: Session, user_id: int) -> str:
    account = user_gateway.get_account(db, user_id)
    return account.state if account is not None else ""


def _skip_reason(account: UserGatewayAccount, cfg: ProvisioningConfig) -> str:
    """这一行现在该不该跑？返回不跑的理由（人话），该跑就返回空串。"""
    if account.state == "manual":
        # 「Key 的来源是人」。worker 永远不碰这一行，自检也不统计它。
        return "这个用户是手工发 Key 模式，不参与自动开通"
    if account.state == "disabled":
        return "这个用户的生图权限已被管理员关闭"
    if account.state == "active":
        return "这个用户已经开通好了（余额同步走 sync_balance）"
    if not cfg.enabled:
        return "自动开通总开关没打开"
    if is_halted():
        return "自动开通已全局暂停：%s" % halt_reason()[0]
    return ""


def _run_steps(
    db: Session,
    account: UserGatewayAccount,
    client: Sub2ApiClient,
    cfg: ProvisioningConfig,
) -> StepResult:
    """真正的三步：建号 → 建 Key → 冒烟。失败统一交给 _record_failure。"""
    user_id = account.user_id
    missing = cfg.missing()
    if missing:
        # 注意这里走的是 E_LOCAL_CONFIG，属于「不计入 attempts」那一类：
        # 管理员忘填一个分组 id 不该把全站用户刷成 manual。
        return _record_failure(
            db, account,
            Sub2ApiError("E_LOCAL_CONFIG",
                         "%s（系统设置 → 网关自动开通）" % missing, retryable=False),
            cfg,
        )

    try:
        changed = False
        # ── T2 / T2b：确认远端账号存在 ──
        if not account.remote_user_id:
            _step_create_user(db, account, client, cfg)
            changed = True

        # ── T3 / T4 / T4b：拿 JWT，确认 Key 存在 ──
        # 判据是 api_key_enc 而不是 state：state 可能因为上一轮崩在半路而落后，
        # 但「库里已经有一把 Key」这件事是明确的，绝不能再建一把。
        if not account.api_key_enc:
            _step_issue_key(db, account, client, cfg)
            changed = True

        # ── T5：冒烟。只有这一步通过才算开通成功 ──
        _step_smoke(db, account, client, cfg)
        return StepResult(user_id, account.state, "active", changed=True,
                          message="已开通，可以正常生图")
    except Sub2ApiError as exc:
        return _record_failure(db, account, exc, cfg)
    except Exception as exc:  # noqa: BLE001 —— 兜底，绝不让 worker 因为一个用户挂掉
        logger.exception("用户 %s 的自动开通出现未预期的错误", user_id)
        return _record_failure(
            db, account,
            Sub2ApiError("E_PROTOCOL",
                         "自动开通遇到未预期的错误（%s），已记录日志" % type(exc).__name__,
                         retryable=False),
            cfg,
        )


# ── T2 / T2b：建号，或确认已经建过 ──────────────────────────────────

def _step_create_user(
    db: Session,
    account: UserGatewayAccount,
    client: Sub2ApiClient,
    cfg: ProvisioningConfig,
) -> None:
    """确认 Sub2API 上有这个账号，把 remote_user_id 落库。

    「确认」两个字是关键：先尝试建，撞 409 就去反查确认那个账号的邮箱确实是我们的。
    409 直接吞掉当成「已经建好了」是不行的——反查这一步同时也是在核对身份。
    """
    if not account.remote_email:
        # 理论上 ensure_account 已经写好了。走到这里说明这一行是自动开通没开的时候
        # 建的（manual 无凭据），后来被 resume_auto 拉回来却没补上凭据——
        # 那是 resume_auto 的 bug，这里报出来而不是偷偷补一个。
        raise Sub2ApiError(
            "E_LOCAL_NO_PASSWORD",
            "这一行没有登记远端邮箱，无法自动开通；请管理员手工发 Key",
            retryable=False,
        )
    email = account.remote_email
    username = "designkit-%d" % account.user_id

    remote = None
    try:
        with _password_of(account) as password:
            remote = client.admin_create_user(
                email=email,
                password=password,
                username=username,
                group_id=cfg.group_id,
                concurrency=sub2api.DEFAULT_CONCURRENCY,
                rpm_limit=sub2api.DEFAULT_RPM_LIMIT,
            )
    except Sub2ApiError as exc:
        if exc.code != "E_EMAIL_EXISTS":
            raise
        # ── T2b：反查确认 ──
        remote = client.admin_find_user_by_email(email)
        if remote is None:
            # 邮箱唯一索引是 users_email_unique_active ... WHERE deleted_at IS NULL，
            # 也就是说这个邮箱**曾被建过又被软删**，删后同邮箱可以重建。
            # 允许再建一次；再撞 409 且仍查不到就是真的说不清了，转人工。
            logger.info("远端说邮箱已存在但反查为空，按「曾被软删」再建一次")
            try:
                with _password_of(account) as password:
                    remote = client.admin_create_user(
                        email=email, password=password, username=username,
                        group_id=cfg.group_id,
                        concurrency=sub2api.DEFAULT_CONCURRENCY,
                        rpm_limit=sub2api.DEFAULT_RPM_LIMIT,
                    )
            except Sub2ApiError as exc2:
                if exc2.code != "E_EMAIL_EXISTS":
                    raise
                remote = client.admin_find_user_by_email(email)
        if remote is None:
            raise Sub2ApiError(
                "E_LOCAL_EMAIL_GHOST",
                "网关说这个邮箱已存在，但按邮箱怎么也查不到对应账号"
                "（多半是被删过），需要管理员手工处理",
                retryable=False,
            )

    # 反查回来的必须逐字对上本地登记的邮箱。admin_find_user_by_email 内部已经做过
    # 精确比对，这里再核一次是**深度防御**：这一步一旦取错人，后面会把 Key 建到
    # 别人账号上、账单算到别人头上，而且完全没有任何报错。
    if remote.email.strip().lower() != email.strip().lower():
        raise Sub2ApiError(
            "E_LOCAL_EMAIL_GHOST",
            "网关返回的账号邮箱和本地登记的对不上，已中止以免把 Key 建到别人账号上",
            retryable=False,
        )

    account.remote_user_id = remote.id
    account.state = "user_created"
    account.last_error = ""
    db.commit()
    logger.info("用户 %s 的远端账号已确认（remote_user_id=%s）", account.user_id, remote.id)

    _verify_remote_user(client, remote, cfg)


def _verify_remote_user(
    client: Sub2ApiClient, remote: sub2api.RemoteUser, cfg: ProvisioningConfig
) -> None:
    """运行时验证设计文档「仍未查明」的第 1、2 条，只记日志，不阻断流程。

    为什么不阻断：真正能证明「这个号能用」的只有 T5 冒烟，前面全是间接证据。
    在这里把流程停掉，只会把一个「可能没事」的猜测变成一次确定的失败。
    但这两条一旦不对，冒烟会 403，那时日志里有这条 warning 就能一眼定位。
    """
    try:
        fresh = client.admin_get_user(remote.id)
    except Sub2ApiError as exc:
        logger.warning("建号后核对失败（不影响流程）：%s", exc.message)
        return
    if fresh.concurrency is not None and fresh.concurrency <= 0:
        logger.warning(
            "远端用户 %s 的并发被写成了 %s——若冒烟报错，先查这里"
            "（admin 建号不传 concurrency 会写字面 0）",
            fresh.id, fresh.concurrency,
        )
    if cfg.group_id and fresh.allowed_groups and cfg.group_id not in fresh.allowed_groups:
        logger.warning(
            "远端用户 %s 的可用分组是 %s，不含设置里填的 %s——建 Key 多半会 403",
            fresh.id, fresh.allowed_groups, cfg.group_id,
        )


# ── T3 / T4 / T4b：代登录换 JWT，建 Key 或读回 ────────────────────

# 一个 custom_key 被别的租户抢注时最多换几轮盐。3 轮之后还占着，
# 大概率不是巧合（HMAC 撞三次的概率可以忽略），该让人来看看了。
_MAX_SALT_ROUNDS = 3


def _step_issue_key(
    db: Session,
    account: UserGatewayAccount,
    client: Sub2ApiClient,
    cfg: ProvisioningConfig,
) -> None:
    """拿到一把属于这个用户的 Key 并落库。

    幂等性的三重保障（照设计文档实现，别削弱任何一条）：
    ① custom_key 确定性推导，重跑要么建成、要么 409；
    ② 409 之后能在本用户名下把那把 key 原样读回来（Sub2API 的 Key 明文入库）；
    ③ 写库时**只在 api_key_enc 为 NULL 时写入**，永不覆盖。
    """
    salt = _custom_key_salt()

    # JWT 只在这个函数的局部变量里活着，绝不落库、绝不进缓存、绝不用 refresh token。
    with _password_of(account) as password:
        login = client.login(account.remote_email or "", password)
    jwt = login.access_token

    issued = None
    for salt_round in range(_MAX_SALT_ROUNDS):
        custom_key = sub2api.derive_custom_key(salt, account.user_id, salt_round)
        try:
            issued = client.create_key(jwt, custom_key, group_id=cfg.group_id or None)
            break
        except Sub2ApiError as exc:
            if exc.code != "E_KEY_EXISTS":
                # 这里**绝不能**做「换个后缀再试一次」：建 Key 的 429 是
                # 20 次/小时/用户的自定义 key 冲突计数，多试一次就把这个用户
                # 锁死一小时。sub2api.py 把那个 429 标成不可重试，这里原样抛上去。
                raise
            # 409 不等于「已经建好了」——custom_key 是跨租户全局唯一的，
            # 必须在**本用户名下**找到它，才能证明那把 key 是我们的。
            found = client.find_key(jwt, custom_key)
            if found is not None:
                issued = found
                logger.info("用户 %s 的 Key 已存在，直接读回（末 4 位 %s）",
                            account.user_id, found.tail)
                break
            # 找不到 = 被别的租户抢注了。换一轮盐重算一个全新的 key 名。
            logger.warning("用户 %s 的 Key 名被别人占了，换第 %d 轮盐重算",
                           account.user_id, salt_round + 1)

    if issued is None or not issued.key:
        raise Sub2ApiError(
            "E_LOCAL_KEY_SQUATTED",
            "为这个用户生成的 Key 名连续被占用，自动开通放弃；请管理员手工发一把 Key",
            retryable=False,
        )

    # ★ 硬保证：只在为空时写入。并发重跑时另一个线程/另一个容器可能已经写好了，
    #   这里直接沿用它的，绝不覆盖——覆盖会让被冲掉的那把 Key 变成
    #   「还挂在网关上继续计费、但本地已经认不出」的孤儿。
    #
    #   commit 在 refresh 之前是必须的：Session 是 expire_on_commit=False，
    #   而且这中间隔着好几秒的 HTTP 调用，手上很可能还开着一个早就过时的
    #   读事务。不先结束它，refresh 读到的仍是别的进程写入**之前**的快照，
    #   判空就形同虚设。这里没有待提交的改动，commit 只是关掉那个读事务。
    db.commit()
    db.refresh(account)
    if account.api_key_enc:
        logger.info("用户 %s 的 Key 已由别处写入，保留原值不覆盖", account.user_id)
    else:
        account.api_key_enc = secrets_box.encrypt(issued.key)
        account.api_key_tail = issued.key[-4:]
        account.state = "key_issued"
        account.last_error = ""
        db.commit()
        logger.info("用户 %s 的 Key 已入库（末 4 位 %s）", account.user_id, issued.key[-4:])


# ── T5：冒烟，唯一能证明「真的能用」的一步 ────────────────────────

def _step_smoke(
    db: Session,
    account: UserGatewayAccount,
    client: Sub2ApiClient,
    cfg: ProvisioningConfig,
) -> None:
    """用这把 Key 自己打 GET /v1/usage。通过才算 active。

    前面每一步都只能证明 HTTP 200，证明不了这把 Key 真能发出请求去。
    典型反例：403「API Key is not assigned to any group」——号建好了、Key 也发了，
    但一张图也生不出来。少了这一步，用户会在点「生成」的时候才发现，
    而那时错误信息是从生图链路里冒出来的，谁也想不到是开通没做完。
    """
    plaintext = user_gateway.decrypt_api_key(account)
    if not plaintext:
        raise Sub2ApiError(
            "E_KEY_REJECTED",
            "本地存的网关 Key 解不开（多半是 data/.enc_key 换过），需要重新开通",
            retryable=False,
        )
    usage = client.gateway_usage(plaintext)

    account.state = "active"
    account.last_error = ""
    account.attempts = 0  # 开通成功，计数归零，将来真出问题时从头数
    if usage.balance_usd is not None:
        # 取不到就保持原值（哪怕是 None）。**绝不猜一个 0**：
        # 界面上 0 会被读成「余额已用完」，比「暂无数据」误导得多。
        account.balance_usd = usage.balance_usd
    account.balance_synced_at = utcnow()

    if not cfg.keep_password:
        # ★ 严格发生在 active 之后。提前清会让「建 Key」这一步永远做不成。
        # 清掉之后这一行不可能再走代登录/建 Key，将来要换 Key 只能走 manual——
        # 这是刻意的取舍：长期保管一堆可登录凭据的风险，远大于偶尔手工发一把 Key 的麻烦。
        account.remote_password_enc = None
    db.commit()
    logger.info("用户 %s 已开通完成（冒烟通过%s）", account.user_id,
                "，密码已按设置清空" if not cfg.keep_password else "")


# ══════════════════════════════════════════════════════════════════
#  失败落库与重试排期
# ══════════════════════════════════════════════════════════════════

def _record_failure(
    db: Session,
    account: UserGatewayAccount,
    exc: Sub2ApiError,
    cfg: ProvisioningConfig,
) -> StepResult:
    """把一次失败写进库，并决定接下来是退避重试、等管理员、还是直接转手工。

    last_error 存的是「分类码|人话」，不是原始异常字符串：前半截给程序分流用
    （重试排期要靠它），后半截给运营看。原始异常字符串两边都不满足——
    程序没法可靠地 parse，人也看不懂。
    """
    code = exc.code
    # 拼错一个分类码，后果是它永远不匹配任何重试规则，静静地卡在那里。
    # 所以在写库前就断言一次。
    assert code in ALL_ERROR_CODES, "未登记的分类码：%s" % code

    if exc.halt_all:
        # 「不是这个用户不行，是整套前提没了」。先置全局闸，免得 worker 挨个把
        # 全站用户刷到上限。
        set_halt(exc.message)
        _persist_pause(db)

    counted = code not in NON_COUNTING_ERROR_CODES
    if counted:
        account.attempts = int(account.attempts or 0) + 1
    account.last_error = exc.as_last_error()

    if counted and account.attempts >= cfg.max_attempts:
        # 到上限：转 manual，含义是「Key 的来源改成人」。这不是放弃这个用户，
        # 而是把他交给一定能成功的那条路（管理员手工填 Key）。
        account.state = "manual"
        account.last_error = "%s|自动开通连续失败 %d 次，已转为等管理员手工发 Key。最后一次的原因：%s" % (
            "E_LOCAL_MAX_ATTEMPTS", account.attempts, exc.message,
        )
        db.commit()
        logger.error("用户 %s 自动开通失败 %d 次，转手工：%s",
                     account.user_id, account.attempts, exc.message)
        return StepResult(account.user_id, "manual", "failed", changed=True,
                          error_code="E_LOCAL_MAX_ATTEMPTS", message=account.last_error,
                          halt_all=exc.halt_all)

    if counted:
        # 只有「真的被远端拒绝了」才落 failed。配置没填好这类不改状态——
        # 那一行本来就还没开始，标成 failed 只会让管理员以为出了故障。
        account.state = "failed"
    db.commit()

    level = logger.warning if exc.retryable else logger.error
    level("用户 %s 的自动开通失败（%s）：%s", account.user_id, code, exc.message)
    return StepResult(account.user_id, account.state, "failed", changed=True,
                      error_code=code, message=exc.message, halt_all=exc.halt_all)


def _persist_pause(db: Session) -> None:
    """顺手把持久化的总开关也关掉——**只在这个设置项已经存在时**。

    settings_service.set_many 会静默丢掉 RUNTIME_DEFAULTS 里没有的键，
    所以这里先判断一次；判断不过就只靠内存闸（is_halted），
    功能不受影响，只是重启后会再撞一次同样的错误。
    """
    if "sub2api_auto_provision" not in config.RUNTIME_DEFAULTS:
        return
    try:
        settings_service.set_many(db, {"sub2api_auto_provision": False})
        logger.error("已自动关闭「网关自动开通」总开关，处理完请到设置页重新打开")
    except Exception:  # noqa: BLE001 —— 关不掉也不能影响主流程
        logger.exception("关闭自动开通总开关失败")


def parse_last_error(text: str) -> Tuple[str, str]:
    """把 last_error 拆成（分类码，人话）。老数据没有分类码时前一项是空串。"""
    raw = (text or "").strip()
    if not raw:
        return "", ""
    code, sep, message = raw.partition("|")
    if sep and code in ALL_ERROR_CODES:
        return code, message
    return "", raw


def backoff_seconds(attempts: int) -> int:
    """已失败 attempts 次时，下一次重试要等多久。"""
    idx = max(0, int(attempts or 0) - 1)
    if idx >= len(_BACKOFF_LADDER):
        idx = len(_BACKOFF_LADDER) - 1
    return _BACKOFF_LADDER[idx]


def retry_due_at(account: UserGatewayAccount) -> Optional[datetime]:
    """这一行下次该在什么时候自动重试；不自动重试就返回 None。

    没有单独的 next_attempt_at 列，所以基准取 updated_at（失败那一刻刚写过库，
    所以它就是「上次尝试时间」）。少一列少一次 ALTER TABLE，而这个项目
    给老表加列是有类型陷阱的（见 migrations.py 文件头）。
    """
    if account.state != "failed":
        return None
    code, _ = parse_last_error(account.last_error)
    if code and code not in RETRYABLE_ERROR_CODES:
        # 不可重试类（2FA、密码对不上、后台模式、验证码、合规、分组、Key 被抢注）
        # 只能由管理员在界面上手动点重试。自动重试一百次也是同一个结果，
        # 只会刷日志、刷限流额度，还会把 attempts 刷到上限白白降级。
        return None
    base = account.updated_at or account.created_at or utcnow()
    return base + timedelta(seconds=backoff_seconds(account.attempts))


def due_user_ids(db: Session, now: Optional[datetime] = None, limit: int = 50) -> List[int]:
    """worker 这一轮该处理哪些用户。

    两类：
    ① 进行中的（pending / user_created / key_issued）——立刻处理，不退避。
       它们要么刚注册、要么上一轮崩在半路，早一点推进早一点能生图。
    ② failed 且分类码可重试且退避时间到了。
    manual / active / disabled 一律不在其中。
    """
    if now is None:
        now = utcnow()
    rows = (
        db.query(UserGatewayAccount)
        .filter(UserGatewayAccount.state.in_(list(_IN_FLIGHT_STATES) + ["failed"]))
        .order_by(UserGatewayAccount.updated_at.asc())
        .all()
    )
    out: List[int] = []
    for row in rows:
        if row.state in _IN_FLIGHT_STATES:
            out.append(row.user_id)
        else:
            due = retry_due_at(row)
            if due is not None and due <= now:
                out.append(row.user_id)
        if len(out) >= limit:
            break
    return out


def provision_due(db: Session, limit: int = 50) -> List[StepResult]:
    """把这一轮该处理的用户全跑一遍，共用一条连接。给 worker 调。

    全局闸一置位就整轮不跑：既然每个用户都会以同样的方式失败，
    挨个去试只是把 attempts 白白刷高。
    """
    cfg = load_config(db)
    if not cfg.enabled or is_halted():
        return []
    user_ids = due_user_ids(db, limit=limit)
    if not user_ids:
        return []
    results: List[StepResult] = []
    with build_client(cfg) as client:
        for user_id in user_ids:
            results.append(provision_user(db, user_id, client=client, cfg=cfg))
            if is_halted():
                # 半路发现前提没了，剩下的这一轮不跑了
                break
    return results


# ══════════════════════════════════════════════════════════════════
#  active → active：余额同步 / 失效探测
# ══════════════════════════════════════════════════════════════════

def sync_balance(
    db: Session,
    user_id: int,
    client: Optional[Sub2ApiClient] = None,
    cfg: Optional[ProvisioningConfig] = None,
) -> StepResult:
    """给一个 active 用户同步余额，顺带探测这把 Key 还灵不灵。

    只读，走网关鉴权，不消耗面板的限流额度。
    **不要**改用 GET /api/v1/usage/stats：那条吃 Global(240rpm)+Heavy(60rpm)
    两档限流，批量轮询很容易撞上。

    网络错误时**保持 active**，只是不更新 balance_synced_at——界面据此显示
    「余额数据 X 分钟前」。把网络抖动当成 Key 失效，会让所有人在网关重启的
    那一分钟里集体掉出 active，然后一个个走完整条重试流程。
    """
    if cfg is None:
        cfg = load_config(db)
    account = user_gateway.get_account(db, user_id)
    if account is None or account.state != "active":
        return StepResult(user_id, account.state if account else "", "skipped",
                          message="只有已开通的用户才同步余额")
    plaintext = user_gateway.decrypt_api_key(account)
    if not plaintext:
        return StepResult(user_id, account.state, "skipped", message="这个用户没有可用的 Key")

    owns_client = client is None
    if client is None:
        client = build_client(cfg)
    try:
        usage = client.gateway_usage(plaintext)
    except Sub2ApiError as exc:
        if exc.code in ("E_KEY_REJECTED", "E_NO_GROUP"):
            # 这把 Key 被删了 / 分组配置被改了。**不清空 api_key_enc**——
            # 清了就再也读不回来，而问题多半只是分组配错、改回去就好了。
            account.state = "failed"
            account.last_error = exc.as_last_error()
            db.commit()
            logger.error("用户 %s 的网关 Key 失效（%s）：%s", user_id, exc.code, exc.message)
            return StepResult(user_id, "failed", "failed", changed=True,
                              error_code=exc.code, message=exc.message)
        logger.warning("用户 %s 的余额同步失败（%s），保持已开通状态", user_id, exc.code)
        return StepResult(user_id, account.state, "skipped",
                          error_code=exc.code, message=exc.message)
    finally:
        if owns_client:
            client.close()

    if usage.balance_usd is not None:
        account.balance_usd = usage.balance_usd
    account.balance_synced_at = utcnow()
    db.commit()
    return StepResult(user_id, "active", "active", changed=True)


# ══════════════════════════════════════════════════════════════════
#  管理员动作：重试 / 转手工 / 重新启用
# ══════════════════════════════════════════════════════════════════

def request_retry(db: Session, user_id: int, reset_attempts: bool = False) -> bool:
    """failed → pending（设置页那个「重试」按钮调这里）。

    reset_attempts=True 用在「管理员确实去把问题修好了」的场景（比如改了分组 id）：
    不清零的话，一个已经攒了 4 次失败的用户，修好之后再失败一次就直接被降级了。
    """
    account = user_gateway.get_account(db, user_id)
    if account is None or account.state not in ("failed", "pending"):
        return False
    account.state = "pending"
    account.last_error = ""
    if reset_attempts:
        account.attempts = 0
    db.commit()
    logger.info("用户 %s 的自动开通已重新排队", user_id)
    return True


def resume_auto(db: Session, user_id: int, cfg: Optional[ProvisioningConfig] = None) -> bool:
    """manual → pending：把一行拉回自动开通。成功返回 True。

    有一种情况**必须拒绝**：这一行有 remote_email 却没有 remote_password_enc
    （active 之后按 sub2api_keep_password=false 清掉了）。此时既不能用旧密码登录，
    又绝不允许重新摇一个密码——重摇会让远端那个账号永久失联（admin 侧多半没有
    改密接口）。所以只能留在 manual，由管理员手工发 Key。这是设计的一部分。
    """
    account = user_gateway.get_account(db, user_id)
    if account is None:
        return False
    if cfg is None:
        cfg = load_config(db)

    if account.remote_email and not account.remote_password_enc and not account.api_key_enc:
        logger.warning(
            "用户 %s 已登记远端邮箱但没有密码，不能重新自动开通（绝不重摇密码），保持手工模式",
            user_id,
        )
        return False
    if account.api_key_enc and not account.remote_email:
        # 这一行的 Key 是人填进来的，远端根本没有对应的 designkit 账号。
        # 这时候切自动开通会在 Sub2API 上凭空建一个**永远用不上**的账号
        #（Key 属于另一个账号），白白占一个名额还没人认领。
        # 要真想改成自动，得先在「成员账号」页把手工填的 Key 清掉。
        logger.warning("用户 %s 用的是手工填的 Key，切自动开通前请先清掉它", user_id)
        return False
    if not account.remote_email:
        # 从没登记过凭据的行（自动开通没开的时候建的），这时候补一份是安全的：
        # 远端还什么都没有，不存在「密码对不上」的问题。
        account.remote_email = _make_remote_email(user_id, cfg.email_domain)
        account.remote_password_enc = secrets_box.encrypt(secrets.token_urlsafe(32))
    account.state = "pending"
    account.attempts = 0
    account.last_error = ""
    db.commit()
    logger.info("用户 %s 已切回自动开通", user_id)
    return True


def demote_to_manual(db: Session, user_id: int, reason: str = "") -> bool:
    """任何状态 → manual：交给管理员手工发 Key。

    只改状态，**不动 api_key_enc / remote_user_id / remote_email**：
    远端那个账号和 Key 还在，删掉本地记录只会制造一个没人认领的孤儿账号，
    下次给同一个人开通时要么重复建号、要么撞 409，两种都很难查。
    """
    account = user_gateway.get_account(db, user_id)
    if account is None:
        return False
    account.state = "manual"
    if reason:
        # 这里**不加分类码前缀**：这是人做的决定，不是某种失败。
        # 硬安一个 E_* 上去，界面上会显示成「系统出错了」，还会被
        # parse_last_error 当成失败分类，误导后来看日志的人。
        account.last_error = reason
    db.commit()
    logger.info("用户 %s 已转为手工发 Key 模式", user_id)
    return True


# ══════════════════════════════════════════════════════════════════
#  给界面看的序列化（白名单）
# ══════════════════════════════════════════════════════════════════

# 白名单而不是黑名单：黑名单会在将来给表加字段时**静默泄露**新字段，
# 而加字段的人根本不会想到去改这里。
_VIEW_FIELDS = (
    "state", "attempts", "balance_usd", "balance_synced_at", "remote_user_id",
)


def account_view(account: Optional[UserGatewayAccount]) -> Dict[str, Any]:
    """把一行序列化成能给管理员界面看的字典。

    刻意**不返回** api_key_tail：designkit、Sub2API 后台、上游账单三处各有一份
    「后 4 位」，凑在一起就能把人、Key、花的钱三边对上号（这条纪律写在
    services/user_gateway.py 和 settings_service.masked_full 里）。
    界面要表达的其实只是「配了没有」，一个布尔值就够了。

    更不返回 remote_email / remote_password_enc：远端密码在**任何接口**里都不出现，
    这不是「藏起来」，是压根没有这个功能。
    """
    if account is None:
        return {"state": "", "configured": False}
    out: Dict[str, Any] = {}
    for name in _VIEW_FIELDS:
        value = getattr(account, name, None)
        if isinstance(value, datetime):
            value = value.isoformat()
        out[name] = value
    code, message = parse_last_error(account.last_error)
    out["error_code"] = code
    out["error_message"] = message  # 人话那半截，直接显示给运营
    out["configured"] = bool(account.api_key_enc)
    out["auto"] = account.state != "manual"
    out["retry_due_at"] = (
        retry_due_at(account).isoformat() if retry_due_at(account) else None
    )
    return out


def summary(db: Session) -> Dict[str, Any]:
    """全站开通情况的概览，给设置页的「网关自动开通」面板用。"""
    counts: Dict[str, int] = {}
    stuck: List[Dict[str, Any]] = []
    for row in db.query(UserGatewayAccount).all():
        counts[row.state] = counts.get(row.state, 0) + 1
        if row.state in ("failed",) or (row.state == "manual" and not row.api_key_enc):
            user = db.get(User, row.user_id)
            item = account_view(row)
            item["user_id"] = row.user_id
            item["username"] = user.username if user is not None else ""
            stuck.append(item)
    reason, at = halt_reason()
    return {
        "counts": counts,
        "stuck": stuck,          # 卡住的人，管理员一眼看到有几个
        "halted": bool(reason),
        "halt_reason": reason,
        "halted_at": at.isoformat() if at else None,
    }
