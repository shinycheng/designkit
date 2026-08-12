"""手机号 + 短信验证码：发码、注册、登录。**这三条都不需要登录。**

════════════════════════════════════════════════════════════════
 写这个文件时的假设：请求来自公网上的陌生人，而且他知道每一条都花钱
════════════════════════════════════════════════════════════════
routers/register.py 的文件头那五条（关着要真的关死、闸门双向、限速在最前面、
报错不许变成探测器、注册与开通解耦）在这里**一条不少地全部适用**，
下面只写手机号这条路**多出来**的三条。改这个文件之前，先把 register.py 的
文件头读完。

**一、这里的每一次「获取验证码」都是一笔真金白银的支出。**
邀请码注册被刷，最坏是多几个僵尸账号，删掉就完了；短信被刷是直接扣钱，
而且被刷狠了阿里云会以「疑似恶意」为由把签名封掉——那时不是花钱的问题，
是整条手机号通道停摆，重新申请签名要按工作日算。所以三层限速
（同一手机号的冷却 + 每日上限、同一 IP 每小时、全站每日总量）是这条通道
能不能上线的前提，不是调优项。而且**超限时一条都不许发出去**：
必须是「先全部查一遍，都过了才发」，不能是「发了再拦」——后者钱已经花了。

**二、配额在真正发出去之前就要扣掉，不是发成功了再扣。**
调用阿里云超时的时候，我们并不知道短信到底发没发出去（很可能发了、也扣了钱，
只是回执没回到我们手上）。要是「成功才扣配额」，攻击者只要想办法让每次调用
都超时，就能无限次地让我们花钱而配额一次都不减。所以顺序是
「查限速 → 写库 → 扣配额 → 才发送」。代价是发送失败时用户白等一个冷却，
这个方向的错误是安全的，反过来那个方向不是。
（唯一的例外：配置根本没填齐时，我们连请求都不会发出去，那种情况在扣配额
 之前就先拦下来了——那时确定没花钱，不该让用户白等 60 秒。）

**三、绝不能泄露「这个手机号是不是已经注册过」。**
泄露了，这个接口就成了一台「查某个号在不在这个系统里」的机器，
可以拿一份手机号名单跑一遍。所以：
  * **发码接口对已注册和未注册的号，响应逐字一样**（连 HTTP 状态码都一样），
    也照样真的发一条短信出去。少发这一条能省钱，但省下的钱是用泄露换的。
  * **登录失败一律回同一句话**：号没注册过、验证码错、密码错，全是
    「手机号或验证码不正确」。分开说等于把上面那台机器换了个入口。
  * 注册接口有一处**故意的例外**，理由写在 register_by_phone 里：
    那一处必须先验过码才会走到，而验过码就等于对方真的拿着这个手机号——
    对一个能收到这个号短信的人来说，「这个号注册过了」不是秘密。

════════════════════════════════════════════════════════════════
 这个文件不注册路由
════════════════════════════════════════════════════════════════
只对外暴露一个 `router`。挂到 app 上是装配那一步的事（见 main.py 里其余
router 的挂法）。这么分是为了让「接口写好了」和「接口对外可见了」是两个
独立的决定——一个还没配短信、还没做前端的系统不该先把入口露出去。
"""
import hashlib
import hmac
import logging
import secrets
import string
import threading
import time
from datetime import timedelta
from typing import Any, Dict, NamedTuple, Optional, Tuple

from fastapi import APIRouter, Depends, HTTPException, Request, Response
from pydantic import BaseModel
from sqlalchemy import delete as sa_delete
from sqlalchemy import select
from sqlalchemy import update as sa_update
from sqlalchemy.exc import IntegrityError, SQLAlchemyError
from sqlalchemy.orm import Session

from ..config import SECRET_KEY
from ..deps import client_ip, get_db
from ..models import AuthIdentity, InviteRedemption, PhoneVerificationCode, User, utcnow
from ..security import create_token, hash_password, verify_password
from ..services import ratelimit, settings_service, sms
# 取图凭证（dk_files Cookie）必须和登录令牌同进同退，否则表现是
# 「登录成功但一张图都打不开」，而接口全是 200、控制台一个错都不报。
# 直接复用 auth.py 里那两个函数，**不在这里另写一份**：
# 种 Cookie 要判断 Secure 标记、要兜住异常，抄一份迟早和那边走岔。
from .auth import _issue_file_cookie, _user_to_dict
from .invites import normalize_code
# ── 下面这一串是**刻意**从邀请码注册那条路上原样借过来的，不是图省事 ──
# 任务书里写的「共用同一套开放注册的硬前置闸门」落到代码上就是这几行：
# 两条注册路径必须共用同一个 IP 限速桶、同一份全站每日名额、同一句
# 「没开放注册」的话术、同一套邀请码原子扣减。各写一份的表现是：
# 管理员把全站每日名额设成 50，实际却能进来 100 个人（两条路各 50），
# 而界面上完全看不出来。
#
# 代价是这个文件依赖 register.py 的内部函数，那边改了这边要跟着改——
# 这正是我们要的：那几个值本来就该一起变。
from .register import (
    CLOSED_MESSAGE,
    MIN_PASSWORD_LENGTH,
    PASSWORD_RULE_TEXT,
    _consume_daily,
    _guard_daily,
    _guard_ip,
    _refund_invite,
    _register_gateway_account,
    _reserve_invite,
    _setting_int,
    _validate_display_name,
    _validate_password,
    _wait_text,
)

logger = logging.getLogger("designkit.phone")

router = APIRouter(prefix="/api/web/phone", tags=["网页-手机号注册登录"])


# ══════════════════════════════════════════════════════════════════════
#  常量
# ══════════════════════════════════════════════════════════════════════

# 验证码的用途，取值必须在 models.PHONE_CODE_SCOPES 里。
# 校验时 phone + scope 一起匹配：少了 scope，一条「注册用」的码就能被拿去当
# 「登录用」的码提交（详见 models.PhoneVerificationCode 上面那段注释）。
SCOPE_REGISTER = "register"
SCOPE_LOGIN = "login"
_ALLOWED_SCOPES = (SCOPE_REGISTER, SCOPE_LOGIN)

# ── 限速用的四个 scope（services/ratelimit.py 按 scope + key 计数）──
# 三层限速拆成四个桶，因为「同一手机号」这一层本身就有两个不同的口径：
#   冷却  —— 两条之间至少隔多久（挡连点重发按钮）
#   日限  —— 一天最多几条（挡「拿一个号当靶子反复发」，那会招来投诉，
#            而投诉的后果是阿里云封签名，比花掉的钱严重得多）
SCOPE_SMS_COOLDOWN = "sms_code_cooldown"   # key = phone_bucket_key(手机号)，不是明文
SCOPE_SMS_PHONE = "sms_code_phone"         # key = phone_bucket_key(手机号)，不是明文
SCOPE_SMS_IP = "sms_code_ip"               # key = 来源 IP
SCOPE_SMS_GLOBAL = "sms_code_all"          # key 固定，限的是整个站
SMS_GLOBAL_KEY = "all"

# 验证码校验失败时的**唯一一句话**。
# 「没有这条码」「码不对」「过期了」「试太多次了」全都回这一句：
# 分开说等于告诉攻击者「你猜的这一步错在哪」，而对正常用户来说
# 这四种情况的处理方式完全一样——重新获取一条。
CODE_INVALID_MESSAGE = "验证码不正确或已过期，请点「重新获取」再拿一条新的。"

# 登录失败时的**唯一一句话**（理由见文件头第三条）。
LOGIN_FAILED_MESSAGE = "手机号或验证码不正确。如果还没注册过，请先注册。"

# 手机号没开放注册时对外说的话。和邀请码那条路共用同一句，
# 且**不解释为什么没开放**——那种话对陌生人来说是一份免费的内部情报。
PHONE_CLOSED_MESSAGE = CLOSED_MESSAGE

# 「这个系统现在还不能用手机号登录」。只在**全站一个手机号身份都没有**时才说，
# 所以它不泄露任何具体号码的状态（见 _require_phone_login_possible）。
LOGIN_UNAVAILABLE_MESSAGE = "现在还不能用手机号登录，请用用户名和密码登录，或联系管理员。"

# 自动生成的用户名前缀 + 随机长度。手机号注册**不让用户自己起用户名**：
#   ① 多一个字段就多一道劝退，而这条路的卖点就是「填个手机号就能进来」；
#   ② 用户名查重会带出「这个名字有没有人用」的探测面，这条路上没必要引进来。
# 生成的名字必须能通过 users._USERNAME_RE（^[A-Za-z0-9._-]{3,32}$）。
# **绝不能用手机号（或它的一部分）当用户名**：用户名会出现在成员列表、
# 任务记录、日志里，那等于把手机号明文散播到全系统。
_USERNAME_PREFIX = "u"
_USERNAME_RANDOM_LENGTH = 8
_USERNAME_ALPHABET = string.ascii_lowercase + string.digits

# 设置项：手机号注册要不要同时填邀请码。**config.py 里目前还没有这一项**，
# 读不到时按 True（要求填）处理——先紧后松：默认严一点，管理员想放开随时能放，
# 反过来「默认松、出事再收紧」那几天的损失是收不回来的。
SETTING_REQUIRE_INVITE = "phone_register_require_invite"
_REQUIRE_INVITE_DEFAULT = True


class _Body(BaseModel):
    """三个接口的请求体合并成一个类，字段全都可选。

    合并是有意的：这三条路上的字段互相重叠（都要 phone，注册和登录都可能带
    code 或 password），分成三个类只会让「哪个接口收哪些字段」散在三处。
    每个接口自己校验自己真正需要的那几项。
    """

    phone: str = ""
    code: str = ""
    password: str = ""
    invite_code: str = ""
    display_name: str = ""
    scope: str = SCOPE_REGISTER


def _fail(detail: str, status_code: int = 422) -> HTTPException:
    """统一的中文报错（理由同 routers/register.py：pydantic 的英文校验错误
    对非技术用户等于没有提示）。"""
    return HTTPException(status_code=status_code, detail=detail)


# ══════════════════════════════════════════════════════════════════════
#  设置项
# ══════════════════════════════════════════════════════════════════════

def _ttl_seconds(db: Session) -> int:
    """验证码有效期。夹在 60~1800 秒之间：填 0 会让每条码一发出去就过期
    （用户永远填不对，而且看不出为什么），填一天等于给「慢慢猜 6 位数」放行。"""
    return _setting_int(db, "sms_code_ttl_seconds", 300, 60, 1800)


def _max_attempts(db: Session) -> int:
    """一条码最多能试错几次。至少 1（填 0 等于发出去就作废）。"""
    return _setting_int(db, "sms_code_max_attempts", 5, 1, 20)


def _cooldown_seconds(db: Session) -> int:
    """同一个号两条之间至少隔多少秒。下限 10 秒是防手滑填 0——
    填 0 就是「重发按钮可以一直点」，一次连点就是十几条短信的钱。"""
    return _setting_int(db, "sms_code_resend_cooldown_seconds", 60, 10, 3600)


def _require_invite(db: Session) -> bool:
    """手机号注册要不要同时填邀请码。**读不到设置项时默认「要」。**

    这一项在 config.py 的 RUNTIME_DEFAULTS 里还没有（要加的名字就是
    SETTING_REQUIRE_INVITE），所以这里必须自己兜住 None，而且兜的方向只能是
    「要求填」：默认松一点的代价是「某天有人把这功能打开，结果全网都能注册」，
    而那件事发生的时候没有任何报错。
    """
    raw = settings_service.get(db, SETTING_REQUIRE_INVITE)
    if raw is None:
        return _REQUIRE_INVITE_DEFAULT
    if isinstance(raw, bool):
        return raw
    # 前端把开关值传成字符串 "false" 是常态（见 settings_router 里那段注释），
    # 不做这一层转换的话，字符串 "false" 是真值，开关会变成「怎么关都关不掉」。
    return str(raw).strip().lower() not in ("0", "false", "no", "off", "")


# ══════════════════════════════════════════════════════════════════════
#  闸门：什么时候允许走这条路
# ══════════════════════════════════════════════════════════════════════

def _gate_problem(db: Session) -> str:
    """手机号注册现在开不开放，以及**给自己人看**的原因（开放时返回空串）。

    两道闸门叠在一起，缺一不可：

      ① phone_register_enabled —— 手机号这条路自己的总开关，默认关。
         它和邀请码那条路的 self_register_enabled 是**并列**的：
         关掉其中一条不影响另一条。
      ② allow_internal_targets 必须是 False —— 这是两条路**共用**的那道硬闸门。
         开着它，对外 API 的 image_urls / callback_url 能让服务端去访问内网任意
         http 地址（包括 Sub2API 的管理端口，上面能建号、改余额、读别人的 Key），
         而自助注册意味着陌生人能自己拿到 ERP Key。两者叠在一起就是
         「把内网的门连同钥匙一起发出去」。详见 routers/register.py 文件头第二条。

    ⚠ 这里**每一次请求都重新算一遍**，不是只在打开开关那一刻查一次：
    设置页那边虽然也拦着「注册开着时把内网改回来」，但库里可能本来就处在这个
    组合里（老库遗留、或者有人直接改了数据库），那种时候只有这里拦得住。

    判断条件必须和 register._gate_state 里那一条**保持一致**。今天没法直接调用
    它，是因为那个函数上来就要求 self_register_enabled 为真（那是邀请码那条路的
    总开关，手机号这条路不该被它管着）。将来若把「共用的那几条」抽成一个公共
    函数，两边都该改过去用它。
    """
    if not bool(settings_service.get(db, "phone_register_enabled")):
        return "总开关 phone_register_enabled 是关的"
    if bool(settings_service.get(db, "allow_internal_targets")):
        return (
            "「允许访问内网地址」(allow_internal_targets) 还开着，"
            "开着它就不能开放任何形式的自助注册"
        )
    return ""


def _require_register_open(db: Session) -> None:
    problem = _gate_problem(db)
    if not problem:
        return
    if problem.startswith("「允许访问内网"):
        # 这一条要吼出来：管理员以为手机号注册已经开了，实际上所有人都被挡在门外，
        # 而界面上（开关是打开的）完全看不出来。
        logger.error(
            "手机号注册被拒绝：%s。请到「系统设置」里把「允许访问内网地址」关掉，"
            "或者把手机号注册关掉。", problem,
        )
    raise _fail(PHONE_CLOSED_MESSAGE, status_code=403)


def _phone_login_possible(db: Session) -> bool:
    """全站有没有**任何一个**手机号身份。

    这不是在查某一个号——查的是「这个系统里有没有人是用手机号登录的」，
    是一条全局事实，不泄露任何具体号码的状态。

    要它是为了堵住一个很实际的窟窿：手机号注册从没打开过的部署里，
    一个人都不可能用手机号登录，但「发登录验证码」这个接口照样会真的发短信——
    那就是一个白送的短信水龙头。有了这一条，那种部署下水龙头是关死的。
    """
    row = db.execute(
        select(AuthIdentity.id).where(AuthIdentity.provider == "phone").limit(1)
    ).first()
    return row is not None


def _require_phone_login_possible(db: Session) -> None:
    if _phone_login_possible(db):
        return
    raise _fail(LOGIN_UNAVAILABLE_MESSAGE, status_code=403)


# ══════════════════════════════════════════════════════════════════════
#  三层限速：**先全部查一遍，都过了才发；发之前就把配额扣掉**
# ══════════════════════════════════════════════════════════════════════
#
# 为什么是三层、少一层会怎样（原话抄自 config.py，改这里就去改那里）：
#   只限手机号 → 攻击者拿一万个号轮着发，每个号都没超限，一晚上就是一万条的账单；
#   只限 IP    → 换一批代理 IP 就绕过去了，成本几乎为零；
#   只限全站   → 一个人就能把当天的名额占光，正常用户全被挡在门外。


def _sms_policies(db: Session) -> Tuple[
    ratelimit.Policy, ratelimit.Policy, ratelimit.Policy, ratelimit.Policy
]:
    """四个桶的参数。阈值全部来自设置页上看得见的那几项（不是 ratelimit 自己的
    ratelimit_<scope>_* 约定键），所以这里显式构造 Policy 传进去。"""
    cooldown = _cooldown_seconds(db)
    return (
        # 冷却：窗口内只允许 1 条，窗口和封锁时长都等于冷却时间。
        # 于是「发过一条 → 立刻被封 → 冷却结束自动解封」，正好是要的语义。
        ratelimit.Policy(max_attempts=1, window_seconds=cooldown, block_seconds=cooldown),
        # 同一个号一天最多几条
        ratelimit.Policy(
            max_attempts=_setting_int(db, "sms_code_phone_daily_limit", 10, 1, 100),
            window_seconds=86400, block_seconds=86400,
        ),
        # 同一个来源 IP 每小时最多几条（不同手机号也一起算）
        ratelimit.Policy(
            max_attempts=_setting_int(db, "sms_code_ip_hourly_limit", 20, 1, 1000),
            window_seconds=3600, block_seconds=3600,
        ),
        # 全站一天最多几条，最后一道兜底
        ratelimit.Policy(
            max_attempts=_setting_int(db, "sms_code_global_daily_limit", 200, 1, 100000),
            window_seconds=86400, block_seconds=86400,
        ),
    )


def _too_many(message: str, retry_after: int) -> HTTPException:
    return HTTPException(
        status_code=429,
        detail=message,
        # 标准头，给脚本和反代看；浏览器不看它，所以话还要写在 detail 里
        headers={"Retry-After": str(max(1, retry_after))},
    )


def _guard_sms(db: Session, phone: str, ip: str) -> None:
    """三层限速的「查」。**任何一层不过就一条都不发**（文件头第一条）。

    四层的提示语各不相同，因为用户能做的事不一样：冷却是「等一会儿再点」，
    日限是「今天别再试了，换条路」。都写成「操作过于频繁」的话，
    撞上日限的人会一直等、一直点，直到确信系统坏了。
    """
    cooldown, phone_daily, ip_hourly, global_daily = _sms_policies(db)

    decision = ratelimit.check(db, SCOPE_SMS_COOLDOWN, phone_bucket_key(phone), cooldown)
    if not decision.allowed:
        raise _too_many(
            "验证码刚刚已经发过一条了，请等 %d 秒再点「重新获取」。"
            "短信偶尔会晚到一两分钟，先看看有没有收到。" % max(1, decision.retry_after),
            decision.retry_after,
        )

    decision = ratelimit.check(db, SCOPE_SMS_PHONE, phone_bucket_key(phone), phone_daily)
    if not decision.allowed:
        raise _too_many(
            "这个手机号今天收到的验证码已经够多了（每天最多 %d 条），请 %s 后再试，"
            "着急的话可以联系管理员直接给你建号。"
            % (phone_daily.max_attempts, _wait_text(decision.retry_after)),
            decision.retry_after,
        )

    decision = ratelimit.check(db, SCOPE_SMS_IP, ip, ip_hourly)
    if not decision.allowed:
        logger.warning(
            "来源 IP %s 一小时内请求验证码超过 %d 次，已拦下", ip, ip_hourly.max_attempts)
        raise _too_many(
            "获取验证码太频繁了，请 %s 后再试。" % _wait_text(decision.retry_after),
            decision.retry_after,
        )

    decision = ratelimit.check(db, SCOPE_SMS_GLOBAL, SMS_GLOBAL_KEY, global_daily)
    if not decision.allowed:
        # 全站名额被打满是件要让管理员看见的事：正常用量根本碰不到，
        # 碰到了要么是有人在刷，要么是阈值被填错了。
        logger.error(
            "全站当天的短信名额已经用完（上限 %d 条），本次发码被拒绝。"
            "正常用量碰不到这个数，请检查是不是有人在刷。", global_daily.max_attempts)
        raise _too_many(
            "今天的验证码发送量已经到上限了，请 %s 后再试，"
            "或者联系管理员直接给你建号。" % _wait_text(decision.retry_after),
            decision.retry_after,
        )


def _consume_sms_quota(db: Session, phone: str, ip: str) -> None:
    """四个桶各记一笔。**必须在真正调用上游之前调用**（文件头第二条）。

    ratelimit 自己会吞掉数据库异常（见那个文件的文件头「数据库出错时放行」），
    所以这里不用 try——它绝不会因为限速表出问题而把发码接口打成 500。
    """
    cooldown, phone_daily, ip_hourly, global_daily = _sms_policies(db)
    ratelimit.record_failure(db, SCOPE_SMS_COOLDOWN, phone_bucket_key(phone), cooldown)
    ratelimit.record_failure(db, SCOPE_SMS_PHONE, phone_bucket_key(phone), phone_daily)
    ratelimit.record_failure(db, SCOPE_SMS_IP, ip, ip_hourly)
    ratelimit.record_failure(db, SCOPE_SMS_GLOBAL, SMS_GLOBAL_KEY, global_daily)


# ══════════════════════════════════════════════════════════════════════
#  验证码：存哈希、校验、清理
# ══════════════════════════════════════════════════════════════════════

def code_hash(phone: str, scope: str, code: str) -> str:
    """验证码的**带密钥哈希**，格式由 models.PhoneVerificationCode 的类文档钉死：

        hmac_sha256(SECRET_KEY, "手机号|用途|验证码") → 64 位十六进制

    🔴 **不能用裸 sha256(code)**，这是算术性的：6 位验证码一共 100 万种可能，
    拿到一份数据库备份的人用一台笔记本几秒钟就能把全表反查成明文，
    等于根本没存哈希。密钥（SECRET_KEY）不在数据库里，光有库算不出码。

    手机号和用途一起拌进去，是为了让一条哈希只在**它自己那一行**成立：
    否则两个人恰好拿到同一个 6 位码时哈希一模一样，A 的码能验开 B 的记录。

    副作用要知情：换掉 DESIGNKIT_SECRET_KEY 会让**已经发出去、还没用的码**
    全部作废。影响面只有几分钟（有效期就那么长），用户重发一次即可。
    """
    material = "%s|%s|%s" % (phone, scope, code)
    return hmac.new(
        str(SECRET_KEY).encode("utf-8"), material.encode("utf-8"), hashlib.sha256
    ).hexdigest()


def phone_bucket_key(phone: str) -> str:
    """给限速用的手机号 key：**带密钥哈希，不是明文号码**。

    为什么不直接把手机号当 key（这是实测发现的问题，不是洁癖）：
    rate_limit_state 是一张**长期留存**的表，而且会为「每一个曾经请求过验证码的
    号码」留一行——包括打错的、试探的、发完就再没回来的。也就是说它会慢慢攒出
    一份「所有来过的手机号」名单，而这些人里绝大多数从没注册成功、
    系统本来就没有任何理由保存他们的号码。

    同样理由，这个 key 也会被 services/ratelimit.py 写进日志（那边现在也做了脱敏，
    是第二道保险；但真正的解法是这里根本不给它明文）。

    🔴 **必须用 HMAC，不能用裸 sha256(phone)**：中国大陆手机号的可能取值不到
    百亿，用一台笔记本几分钟就能把哈希全表反查回明文，等于没哈希。
    密钥不在数据库里，只有库的人算不出来。

    截到 32 位十六进制：撞不上（2^128 分之一），又省表空间。
    """
    return hmac.new(
        str(SECRET_KEY).encode("utf-8"),
        ("ratelimit|" + phone).encode("utf-8"),
        hashlib.sha256,
    ).hexdigest()[:32]


def _store_code(
    db: Session,
    phone: str,
    scope: str,
    code: str,
    ip: str,
    channel: str,
    user_id: Optional[int],
    ttl_seconds: int,
) -> int:
    """写下这一行，返回它的 id。**发送之前写，不是发成功了再写。**

    理由见 models.PhoneVerificationCode 类文档第二节：先写行、再发送，
    最坏是库里多一条没送达的记录（用户重发即可）；反过来一旦写库失败，
    钱花了、短信到了用户手机上，系统这边却查无此码——用户填了正确的验证码
    却被告知「验证码错误」，这种故障没有任何人能自查出来。

    顺手把这个号这个用途下**所有还没用掉的旧码作废**。这样「同一时刻只有最新
    那一条有效」就成了事实，而不是靠校验时「只查最新一条」的约定去兜——
    那个约定在「最新一条被用掉之后」会漏：次新的那条会重新变成「最新的未用码」，
    于是一条几分钟前发出去的旧码又活过来了。
    """
    now = utcnow()
    db.execute(
        sa_update(PhoneVerificationCode)
        .where(
            PhoneVerificationCode.phone == phone,
            PhoneVerificationCode.scope == scope,
            PhoneVerificationCode.consumed_at.is_(None),
        )
        .values(consumed_at=now)
        .execution_options(synchronize_session=False)
    )
    row = PhoneVerificationCode(
        phone=phone,
        scope=scope,
        code_hash=code_hash(phone, scope, code),
        expires_at=now + timedelta(seconds=ttl_seconds),
        attempts=0,
        consumed_at=None,
        channel=channel,
        provider_msg_id=None,
        user_id=user_id,
        # 列宽 45（IPv6 的最大长度）。截断是硬要求：PostgreSQL 上超长直接报错，
        # SQLite 却照单全收，两边行为不一致的 bug 最难查。
        client_ip=(ip or "")[:45],
        created_at=now,
    )
    db.add(row)
    db.commit()
    db.refresh(row)
    _maybe_cleanup(db)
    return int(row.id)


def _mark_consumed(db: Session, row_id: int) -> None:
    """把某一行标成已用（发送失败时调用，免得一条没送出去的码白占着有效期）。"""
    try:
        db.execute(
            sa_update(PhoneVerificationCode)
            .where(
                PhoneVerificationCode.id == row_id,
                PhoneVerificationCode.consumed_at.is_(None),
            )
            .values(consumed_at=utcnow())
            .execution_options(synchronize_session=False)
        )
        db.commit()
    except SQLAlchemyError:
        db.rollback()
        logger.exception("作废验证码记录 %s 失败（不影响本次请求的结果）", row_id)


def _record_sent(db: Session, row_id: int, channel: str, msg_id: str) -> None:
    """把真实的发送通道和回执号补回那一行。

    channel 这一列是本期的核心安全要求之一：调试模式下短信根本没发出去，
    而库里的记录和真发过一模一样。不记下渠道，事后没有任何办法回答
    「那天用户说没收到短信，到底是没发还是运营商吞了」。
    """
    try:
        db.execute(
            sa_update(PhoneVerificationCode)
            .where(PhoneVerificationCode.id == row_id)
            .values(channel=channel[:16], provider_msg_id=(msg_id or None))
            .execution_options(synchronize_session=False)
        )
        db.commit()
    except SQLAlchemyError:
        db.rollback()
        logger.exception("回填验证码记录 %s 的发送结果失败", row_id)


class CodeCheck(NamedTuple):
    """一次验证码校验的结果。

    ok      验过了没有
    user_id 这条码申请时记下的人（注册用的码恒为 None）
    """

    ok: bool
    user_id: Optional[int] = None


def _verify_code(db: Session, phone: str, scope: str, code: str, max_attempts: int) -> CodeCheck:
    """校验一条验证码，验过了就**当场把它标成已用**（一条码只能用一次）。

    四种失败（没有这条码 / 码不对 / 过期了 / 试太多次）对调用方是同一个 False，
    调用方一律回 CODE_INVALID_MESSAGE——理由见那个常量上面的注释。

    两处必须是「带条件的 UPDATE + 看 rowcount」，不能先读后写：
      * 累加 attempts：先读后写在并发下会丢次数，攻击者只要并发猜就能让
        「最多试 5 次」形同虚设；
      * 标记 consumed_at：两个请求同时提交同一个正确的码时，先查后写会双双
        通过，一个码开出两个号。
    """
    digits = (code or "").strip()
    if not digits.isdigit():
        # 连格式都不对，不去查库（省一次查询，也不给刷接口的人留计数入口）
        return CodeCheck(False)
    now = utcnow()
    row = db.execute(
        select(
            PhoneVerificationCode.id,
            PhoneVerificationCode.code_hash,
            PhoneVerificationCode.attempts,
            PhoneVerificationCode.user_id,
        )
        .where(
            PhoneVerificationCode.phone == phone,
            PhoneVerificationCode.scope == scope,
            PhoneVerificationCode.consumed_at.is_(None),
            PhoneVerificationCode.expires_at > now,
        )
        # 只认最新的那一条。索引 (phone, scope, id) 就是按这条语句排的。
        .order_by(PhoneVerificationCode.id.desc())
        .limit(1)
    ).first()
    if row is None:
        return CodeCheck(False)
    row_id, stored_hash, attempts, code_user_id = int(row[0]), row[1] or "", int(row[2] or 0), row[3]

    if attempts >= max_attempts:
        # 试错次数用完了。**直接作废**，不让它继续挂在那里被慢慢猜：
        # 6 位数只有 100 万种可能，不限次数地猜是猜得完的。
        _mark_consumed(db, row_id)
        logger.warning(
            "手机号 %s 的验证码试错次数用完（%d 次），这条码已作废",
            sms.mask_phone(phone), attempts)
        return CodeCheck(False)

    # compare_digest 而不是 ==：字符串的 == 会在第一个不同的字节上提前返回，
    # 理论上能被计时攻击一点点问出哈希。这里代价是零，没有不用它的理由。
    if not hmac.compare_digest(stored_hash, code_hash(phone, scope, digits)):
        try:
            db.execute(
                sa_update(PhoneVerificationCode)
                .where(
                    PhoneVerificationCode.id == row_id,
                    PhoneVerificationCode.consumed_at.is_(None),
                )
                .values(attempts=PhoneVerificationCode.attempts + 1)
                .execution_options(synchronize_session=False)
            )
            db.commit()
        except SQLAlchemyError:
            db.rollback()
            logger.exception("累加验证码试错次数失败（本次仍按验证失败处理）")
        return CodeCheck(False)

    result = db.execute(
        sa_update(PhoneVerificationCode)
        .where(
            PhoneVerificationCode.id == row_id,
            PhoneVerificationCode.consumed_at.is_(None),
        )
        .values(consumed_at=now)
        .execution_options(synchronize_session=False)
    )
    db.commit()
    if result.rowcount != 1:
        # 同一瞬间被另一个请求用掉了。两个请求都拿着正确的码，但名额只有一个——
        # 让后到的那个失败，而不是两个都放过去。
        return CodeCheck(False)
    return CodeCheck(True, int(code_user_id) if code_user_id is not None else None)


# ── 清理过期记录 ──────────────────────────────────────────────────────
# 做法照抄 services/ratelimit.py 里那套已经在跑的（models 的类文档第三节点名要求
# 这么做）：每次成功插入一行之后顺手清一次，进程内 60 秒节流。
# 平时零额外开销，而只有「有人在发码」的时候表才会变大，触发时机正好对得上。
#
# 初值是负无穷、**不是 0**：time.monotonic() 的零点没有规定，macOS 上它从进程启动
# 开始算，写 0 会让进程活着的头 60 秒里一次清理都不发生——而单元测试恰好整个都
# 跑在这 60 秒内，于是「表会一直长」这种问题测不出来。（同 ratelimit 的注释。）
_CLEANUP_MIN_INTERVAL_SECONDS = 60
_CLEANUP_BATCH = 1000
# 过期之后再多留一天：出事时第一个要回答的问题是「这个号今晚到底被发了几条」，
# 全删干净就答不了了；另外群晖上 NTP 校时把系统时间往回拨过，留一天能扛住时钟回拨。
_CLEANUP_RETENTION = timedelta(hours=24)
_cleanup_last_at = float("-inf")
_cleanup_lock = threading.Lock()


def _maybe_cleanup(db: Session) -> None:
    global _cleanup_last_at
    now = time.monotonic()
    with _cleanup_lock:
        if now - _cleanup_last_at < _CLEANUP_MIN_INTERVAL_SECONDS:
            return
        _cleanup_last_at = now
    try:
        cleanup_expired(db)
    except SQLAlchemyError:
        db.rollback()
        # 清理失败绝不能连累发码本身——用户正等着收短信。
        logger.exception("清理过期验证码记录失败")


def cleanup_expired(db: Session) -> int:
    """删掉过期超过 24 小时的验证码记录，返回删了几行（一次最多 _CLEANUP_BATCH 行）。

    分批删是因为 PostgreSQL 上一条大批量 DELETE 会把表锁住一会儿，
    正好卡住同时在发码的人。
    """
    cutoff = utcnow() - _CLEANUP_RETENTION
    victims = db.execute(
        select(PhoneVerificationCode.id)
        .where(PhoneVerificationCode.expires_at < cutoff)
        .limit(_CLEANUP_BATCH)
    ).scalars().all()
    if not victims:
        return 0
    # 先查 id 再按 id 删：三种数据库对「删除的表出现在自己的子查询里」限制各不相同，
    # 分两步是都稳的写法（同 ratelimit.cleanup）。
    result = db.execute(
        sa_delete(PhoneVerificationCode).where(
            PhoneVerificationCode.id.in_(list(victims))
        )
    )
    db.commit()
    return int(result.rowcount or 0)


# ══════════════════════════════════════════════════════════════════════
#  手机号 ↔ 账号
# ══════════════════════════════════════════════════════════════════════

def _find_user_by_phone(db: Session, phone: str) -> Optional[User]:
    """按手机号找人。找不到返回 None。

    查的是 auth_identities（provider='phone'），不是 users 上的某一列——
    手机号是一种**登录方式**，不是用户的属性，见 models.AuthIdentity 的类文档。
    """
    row = db.execute(
        select(AuthIdentity.user_id).where(
            AuthIdentity.provider == "phone", AuthIdentity.identifier == phone
        )
    ).first()
    if row is None:
        return None
    return db.query(User).filter(User.id == int(row[0])).first()


def _new_username(db: Session) -> str:
    """给手机号注册的人造一个用户名。

    **不含手机号的任何一位**：用户名会出现在成员列表、任务记录和日志里，
    用手机号当用户名等于把它明文散播到全系统（而库里存完整号码的地方
    本来只有两处，见 models.PhoneVerificationCode 类文档第四节）。

    撞名了就再来一次。最后仍然撞上就抛错——不静默地凑一个奇怪的名字出来：
    真发生了说明随机数出了问题，那是要人来看的事。
    并发下还有数据库的唯一索引兜底（见 register_by_phone 里的 IntegrityError 分支）。
    """
    for _ in range(8):
        suffix = "".join(secrets.choice(_USERNAME_ALPHABET) for _ in range(_USERNAME_RANDOM_LENGTH))
        candidate = _USERNAME_PREFIX + suffix
        exists = db.query(User).filter(User.username == candidate).first()
        if exists is None:
            return candidate
    raise _fail("注册暂时不可用，请稍后再试或联系管理员。", status_code=500)


def _create_phone_user(
    db: Session,
    phone: str,
    username: str,
    password: str,
    display_name: str,
    invite_code_id: int,
    invite_code_text: str,
    ip: str,
    user_agent: str,
) -> User:
    """建账号 + 记登录方式 +（用了邀请码的话）记一笔流水。**一个事务，要么全成要么全不成。**

    只建了 User 没建 AuthIdentity，这个人就成了「不知道从哪冒出来的账号」，
    而且他的手机号谁也查不到——下次他用同一个号来注册会再开一个新账号。
    """
    now = utcnow()
    user = User(
        username=username,
        # 没设密码就存空串。verify_password("", "") 恒为 False（split 会抛 ValueError
        # 被它自己接住），所以空哈希登不进去，不是一个「空密码后门」。
        # 之后想加密码走 auth.py 的 /set_password（它正是为这种账号留的）。
        password_hash=hash_password(password) if password else "",
        display_name=display_name,
        # 角色写死 member，**不读请求体里的任何字段**。这一行是这个接口最要紧的
        # 一行：从请求里取 role，任何人加一个 "role": "admin" 就是管理员。
        role="member",
        is_active=True,
        # 密码（如果设了）是他自己刚设的，没有「初始密码」这回事。
        must_change_password=False,
        created_at=now,
    )
    db.add(user)
    db.flush()  # 先拿到 user.id

    db.add(AuthIdentity(
        user_id=user.id,
        provider="phone",
        # 存归一化后的 11 位纯数字。口径必须和 phone_verification_codes.phone
        # 完全一致（两边都只用 sms.normalize_phone 的输出），否则表现是
        # 「验证码验过了，注册却说这个号已被占用」，而两条记录看起来一模一样。
        identifier=phone,
        credential=None,
        # 走到这里必然是刚验过码，也就是他真的拿着这个手机号
        verified_at=now,
        extra=None,
        created_at=now,
    ))
    if password:
        # 设了密码就再记一行 password 方式，和邀请码注册那条路口径一致。
        # credential 恒为 NULL：密码的唯一真相在 users.password_hash，
        # 两处都存必然出现「改了一处、另一处还是老密码」的鬼故事。
        db.add(AuthIdentity(
            user_id=user.id,
            provider="password",
            identifier=username.lower(),
            credential=None,
            verified_at=now,
            extra=None,
            created_at=now,
        ))
    if invite_code_text:
        db.add(InviteRedemption(
            # 码在扣减之后被人删掉时 code_id 是 0，这时存 None，
            # code_snapshot 仍留着码的字面值，照样对得上号。
            invite_code_id=invite_code_id or None,
            code_snapshot=invite_code_text[:32],
            user_id=user.id,
            username_snapshot=username[:64],
            # 两个字段都要截断：列宽在 PostgreSQL 上是硬约束，一个伪造的超长
            # User-Agent 能把这条 INSERT 直接打成 500。
            client_ip=(ip or "")[:45],
            user_agent=(user_agent or "")[:256],
        ))
    db.commit()
    db.refresh(user)
    return user


def _touch_identity(db: Session, user_id: int) -> None:
    """记一下「这个人最近一次是用手机号登进来的」。失败不影响登录本身。"""
    try:
        db.execute(
            sa_update(AuthIdentity)
            .where(AuthIdentity.user_id == user_id, AuthIdentity.provider == "phone")
            .values(last_login_at=utcnow())
            .execution_options(synchronize_session=False)
        )
        db.commit()
    except SQLAlchemyError:
        db.rollback()
        logger.exception("更新手机号登录时间失败（不影响本次登录）")


# ══════════════════════════════════════════════════════════════════════
#  接口一：发验证码
# ══════════════════════════════════════════════════════════════════════

@router.get("/status")
def phone_status(db: Session = Depends(get_db)) -> Dict[str, Any]:
    """这条路现在开不开、要不要填邀请码、验证码多久过期。**不需要登录。**

    前端据此决定登录页要不要显示「手机号注册」入口、注册表单里要不要显示邀请码
    那一栏、重发按钮的倒计时从几秒开始。规则由后端给，不是让前端自己写一份——
    前端抄一份的话，哪天改了设置就会出现「页面上写 60 秒、实际要等 120 秒」。

    **不解释为什么没开放**（见 PHONE_CLOSED_MESSAGE），对着公网说等于送情报。

    debug_notice 那一项是本期的硬要求：处在调试模式时，前端**必须**把它显著地
    显示在发码按钮旁边。不标的话，哪天管理员忘了切回真实通道，她会一直以为短信
    发出去了，而用户那边永远收不到——双方都查不出问题出在哪。
    """
    open_now = not _gate_problem(db)
    # 这里**故意不调 sms.load_config**：那个函数会顺手解密 AccessKey Secret，
    # 而这是一个不需要登录的接口，没有任何理由让它去碰密钥。
    # 认不出来的通道值一律当调试模式（和 load_config 的取向一致：猜错方向的代价
    # 不对称——当成 debug 最多是「短信没发出去但页面上写明了」，当成 aliyun 就是
    # 拿着一份可疑配置去花钱）。
    provider = str(settings_service.get(db, "sms_provider") or sms.CHANNEL_DEBUG).strip().lower()
    debug_mode = provider != sms.CHANNEL_ALIYUN
    return {
        "enabled": open_now,
        "message": "" if open_now else PHONE_CLOSED_MESSAGE,
        # 已经注册过的人即使关掉注册也要能登录，所以这两项是分开的
        "login_enabled": _phone_login_possible(db),
        "require_invite": _require_invite(db),
        "debug_mode": debug_mode,
        "debug_notice": sms.DEBUG_MODE_NOTICE if debug_mode else "",
        "code_ttl_seconds": _ttl_seconds(db),
        "resend_after_seconds": _cooldown_seconds(db),
        "password_optional": True,
        "password_rule": PASSWORD_RULE_TEXT,
        "min_password_length": MIN_PASSWORD_LENGTH,
    }


@router.post("/code")
def send_verification_code(
    body: _Body, request: Request, db: Session = Depends(get_db)
) -> Dict[str, Any]:
    """发一条验证码短信。**不需要登录，而且每一次都花钱。**

    顺序是刻意排的，不要随手调换：
      1. 手机号格式 —— 最便宜的一步，格式不对连闸门都不用查；
      2. 闸门（注册总开关 / 有没有人用手机号登录）；
      3. **三层限速，只查不扣** —— 任何一层不过就一条都不发；
      4. 短信配置有没有配齐 —— 没配齐的话我们连请求都发不出去，
         这时**还没扣配额**，不该让用户白等一个 60 秒的冷却；
      5. 写下那一行（含哈希后的码）；
      6. **扣掉四个桶的配额** —— 必须在真正调用上游之前，理由见文件头第二条；
      7. 才真的发送。

    ⚠ **响应对已注册和未注册的手机号完全一样**，连「有没有真的发出去」都一样。
    这里不查这个号注册没注册，是有意的：查了就得决定「注册过要不要照发」，
    而任何一种不同的处理都会从响应里漏出去（见文件头第三条）。
    """
    scope = (body.scope or SCOPE_REGISTER).strip().lower()
    if scope not in _ALLOWED_SCOPES:
        # 只可能是前端传错了。**不要静默当成 register**：那会让「登录码」和
        # 「注册码」混用，而区分这两者正是 scope 这一列存在的理由。
        raise _fail("验证码用途不对（只能是 register 或 login），请刷新页面重试。")

    phone = sms.normalize_phone(body.phone)
    if not phone:
        raise _fail("请填写 11 位中国大陆手机号（不要加空格、横线或国家代码）")

    if scope == SCOPE_REGISTER:
        _require_register_open(db)
    else:
        # 登录这条路**不看 phone_register_enabled**：管理员在拉完人之后
        # 多半会把注册关掉，已经注册的人当然还要能登录。
        _require_phone_login_possible(db)

    ip = client_ip(request, ratelimit.trusted_proxy_hops(db))
    _guard_sms(db, phone, ip)

    config = sms.load_config(db)
    if config.provider == sms.CHANNEL_ALIYUN:
        problems = sms.config_problems(config)
        if problems:
            # 通道设成了阿里云但没配齐。设置页那边拦着这个组合，能走到这里说明是
            # 直接改数据库改出来的，或者 .enc_key 丢了导致 Secret 解不开。
            # 这一步**在扣配额之前**：我们确定一条短信都没发出去。
            logger.error(
                "短信通道设成了阿里云但配置没齐，本次发码直接失败：%s", "；".join(problems))
            raise HTTPException(
                status_code=503,
                detail="验证码暂时发不出去。请稍后再试，或者联系管理员帮你开通账号。",
            )

    # 登录码记下是谁在申请（注册码那时人还不存在，恒为 None）。
    # 这只是给事后排查用的线索，**查到查不到都不改变任何响应**——
    # 一旦让它影响响应，这个接口就变成了「这个号注册没注册」的判定器。
    code_user_id = None
    if scope == SCOPE_LOGIN:
        existing = _find_user_by_phone(db, phone)
        code_user_id = existing.id if existing is not None else None

    channel = sms.CHANNEL_ALIYUN if config.provider == sms.CHANNEL_ALIYUN else sms.CHANNEL_DEBUG
    code = sms.generate_code()
    ttl = _ttl_seconds(db)
    row_id = _store_code(db, phone, scope, code, ip, channel, code_user_id, ttl)

    _consume_sms_quota(db, phone, ip)

    # out_id 传我们这条记录的 id：它会原样出现在阿里云的回执里，
    # 事后排查「到底发没发出去」时能把两边对上。
    result = sms.send_code(db, phone, code, out_id=str(row_id), config=config)
    if not result.ok:
        # 这条码没送出去，直接作废，免得它白占着有效期（用户重发会拿到新的一条）。
        _mark_consumed(db, row_id)
        # admin_hint 是给管理员看的具体修复步骤，**只写日志、不回给调用方**——
        # 「阿里云余额不足」这种话对公网上的陌生人来说是一份免费的内部情报。
        logger.error(
            "发送验证码失败（手机号 %s，用途 %s）：%s", sms.mask_phone(phone), scope, result.admin_hint)
        raise HTTPException(
            status_code=429 if result.category == sms.CATEGORY_FLOW_CONTROL else 502,
            detail=result.message,
        )

    _record_sent(db, row_id, result.channel, result.provider_msg_id)
    logger.info(
        "已发出验证码（手机号 %s，用途 %s，通道 %s）", sms.mask_phone(phone), scope, result.channel)

    return {
        "ok": True,
        "message": result.message,
        # ⚠ 这三项是本期的硬要求：调试模式下前端必须显著标明「短信没有真的发出去」。
        # code 只在调试模式下有值——真发出去的验证码**绝不回吐给前端**，
        # 那等于把码直接送给任何一个知道手机号的人，整道验证形同虚设。
        "debug": bool(result.debug),
        "debug_notice": sms.DEBUG_MODE_NOTICE if result.debug else "",
        "code": result.code if result.debug else "",
        "channel": result.channel,
        "phone": sms.mask_phone(phone),   # 回脱敏形式，让用户确认自己没填错号
        "expires_in_seconds": ttl,
        "resend_after_seconds": _cooldown_seconds(db),
    }


# ══════════════════════════════════════════════════════════════════════
#  接口二：手机号注册
# ══════════════════════════════════════════════════════════════════════

@router.post("/register")
def register_by_phone(
    body: _Body, request: Request, response: Response, db: Session = Depends(get_db)
) -> Dict[str, Any]:
    """用手机号 + 验证码注册一个账号。**不需要登录。**

    顺序（和 routers/register.py 那条路是同一套道理，不要随手调换）：
      1. 闸门 —— 关着的时候什么都不做；
      2. 按 IP 限速 + 全站每日名额 —— **和邀请码注册共用同一个桶**，
         必须排在密码哈希（20 万轮 PBKDF2，约 0.1 秒 CPU）前面；
      3. 便宜的字段校验；
      4. **验证码** —— 这一步是这条路的身份证明，排在扣邀请码名额之前，
         免得一个猜码的人白白废掉别人的一张一次性码；
      5. 手机号有没有被注册过；
      6. 原子扣减邀请码名额（如果要求填的话）；
      7. 建号（三张表一个事务）；
      8. 登记网关账号行（pending）+ 记一笔每日名额，**都不等开通**。

    ── 第 5 步是文件头第三条的**唯一例外**，说清楚为什么 ──
    这里会明确告诉对方「这个手机号已经注册过了，请直接登录」。看起来违反了
    「不许泄露某个号注册没注册」，其实不是：走到这一步的前提是他刚提交了一条
    **正确的验证码**，而那条码只发到了这个手机号上。也就是说，能看到这句话的人
    本来就拿着这个手机号——对他来说这不是秘密。
    反过来，如果这里也含混地回「验证码不正确」，一个忘了自己注册过的人会
    卡在注册页反复重发短信（每一条都花钱），而且永远不知道该去登录。
    """
    _require_register_open(db)

    ip = client_ip(request, ratelimit.trusted_proxy_hops(db))
    # 这两道和邀请码注册共用同一个 scope + key（SCOPE_IP / SCOPE_DAILY）：
    # 不共用的话，管理员设的「全站每天最多 50 个新号」实际会变成 100 个
    # （两条路各 50），而界面上完全看不出来。
    _guard_ip(db, ip)
    _guard_daily(db)

    phone = sms.normalize_phone(body.phone)
    if not phone:
        raise _fail("请填写 11 位中国大陆手机号（不要加空格、横线或国家代码）")
    display_name = _validate_display_name(body.display_name)

    require_invite = _require_invite(db)
    code_text = normalize_code(body.invite_code) if require_invite else ""
    if require_invite and not code_text:
        raise _fail("请填写邀请码（管理员发给你的那一串字母数字）")

    username = _new_username(db)
    password = (body.password or "")
    if password:
        # 密码是可选的：只用手机号验证码也能登录。设了就按和邀请码注册
        # 完全一样的强度规则校验（同一份文案，见 register.PASSWORD_RULE_TEXT）。
        password = _validate_password(password, username)
        if phone in password or phone[-4:] in password:
            # 手机号（哪怕只是后四位）出现在密码里，等于把密码写在门牌上：
            # 这条路上任何人都知道他的手机号。
            raise _fail("密码里不能包含手机号，请换一个")

    if not _verify_code(db, phone, SCOPE_REGISTER, body.code, _max_attempts(db)).ok:
        raise _fail(CODE_INVALID_MESSAGE)

    if _find_user_by_phone(db, phone) is not None:
        # 例外，理由见上面那一大段。给的是「该怎么办」，不是「你输错了」。
        raise _fail(
            "这个手机号已经注册过了，请直接用手机号登录（登录时同样是收验证码）。",
            status_code=409,
        )

    invite_code_id = 0
    invite_code_snapshot = ""
    if require_invite:
        # 原子扣减：`used_count < max_uses` 绝不能先 SELECT 再 UPDATE，
        # 否则两个人同时用最后一次名额会双双通过，一张一次性码开出两个号。
        # 直接用邀请码那条路的实现，不在这里另抄一份。
        invite_code_id, invite_code_snapshot = _reserve_invite(db, code_text, utcnow())

    try:
        user = _create_phone_user(
            db, phone, username, password, display_name,
            invite_code_id, invite_code_snapshot,
            ip, request.headers.get("user-agent") or "",
        )
    except IntegrityError:
        # 两个请求在同一瞬间用同一个手机号注册：上面的查重会双双通过，
        # 由 auth_identities 上 (provider, identifier) 的唯一索引兜底。
        # 邀请码名额要退回去，否则这张一次性码就白废了。
        db.rollback()
        _refund_invite(db, invite_code_id)
        logger.info("手机号 %s 在并发注册中撞车，本次注册回退", sms.mask_phone(phone))
        raise _fail(
            "这个手机号已经注册过了，请直接用手机号登录（登录时同样是收验证码）。",
            status_code=409,
        )
    except Exception:
        db.rollback()
        _refund_invite(db, invite_code_id)
        raise

    # 登记一行状态为 pending 的网关账号就**立刻返回**：这里一次外部请求都不发，
    # 真正去 Sub2API 建号、拿 Key 的那三次请求由后台调度器慢慢做。
    # 这条界限是结构性的——网关关机、升级、换地址、接口全改，表现只能是
    # 「新用户照常注册、照常登录，只是暂时不能生图」，绝不能变成「谁都注册不了」。
    _register_gateway_account(db, user)
    _consume_daily(db)
    logger.info(
        "新用户 %s 通过手机号 %s 完成注册（来源 IP %s）",
        user.username, sms.mask_phone(phone), ip)

    # ── 这里**直接发登录令牌**，和邀请码那条路不一样，是有意的 ──
    # 那条路让用户注册完再登一次，是为了确认他真的记住了自己设的密码。
    # 手机号这条路上密码是可选的：不发令牌的话，他注册完还得再收一条验证码
    # 才能进来——多花一条短信的钱，还多一道可能收不到的坎。
    # 而他刚刚已经用验证码证明过自己了，再证一遍没有任何安全上的收益。
    _issue_file_cookie(response, db, user, request)
    return {
        "ok": True,
        "message": (
            "注册成功，已经帮你登录进来了。"
            "生图额度会在后台自动开通，通常一两分钟内完成；"
            "如果提示还不能生图，等一会儿刷新一下，或者联系管理员。"
        ),
        "token": create_token(user.id, user.token_version or 0),
        "user": _user_to_dict(user),
        # 用户名是系统随机给的，得让他看见（以后要用「用户名 + 密码」登录时要填）
        "username": user.username,
        "has_password": bool(password),
    }


# ══════════════════════════════════════════════════════════════════════
#  接口三：手机号登录
# ══════════════════════════════════════════════════════════════════════

@router.post("/login")
def login_by_phone(
    body: _Body, request: Request, response: Response, db: Session = Depends(get_db)
) -> Dict[str, Any]:
    """手机号登录：**验证码登录**，或者「手机号 + 注册时设的密码」。**不需要登录。**

    两种方式都回同一句失败提示（LOGIN_FAILED_MESSAGE）：号没注册过、验证码错、
    密码错，对外一律一样。分开说的话，这个接口就成了一台「查某个手机号在不在
    这个系统里」的机器，可以拿一份号码名单跑一遍。

    限速走的是**登录那个 scope**（和用户名密码登录同一份阈值设置），
    key 是「来源 IP|phone:手机号」——和用户名那条路的 key 天然不会撞
    （那边是「IP|用户名」），所以一个人反复输错自己的验证码不会连累别人。
    """
    phone = sms.normalize_phone(body.phone)
    code = (body.code or "").strip()
    password = body.password or ""
    if not phone:
        raise _fail("请填写 11 位中国大陆手机号（不要加空格、横线或国家代码）")
    if not code and not password:
        raise _fail("请填写收到的验证码；如果注册时设过密码，也可以直接用密码登录。")

    key = "%s|phone:%s" % (client_ip(request, ratelimit.trusted_proxy_hops(db)), phone)
    # **必须在校验密码之前**：verify_password 是 20 万轮 PBKDF2，一次约 0.1 秒 CPU，
    # 不先拦住的话光靠反复 POST 就能把机器压满。
    decision = ratelimit.check(db, "login", key)
    if not decision.allowed:
        raise HTTPException(
            status_code=429,
            detail=decision.message,
            headers={"Retry-After": str(max(1, decision.retry_after))},
        )

    user = _find_user_by_phone(db, phone)
    if code:
        # 验证码必须**无论这个号存不存在都照验**：只在「查到了人」时才验，
        # 两条路径的耗时和行为会不一样，那就是一个可以被问出来的差别。
        ok = _verify_code(db, phone, SCOPE_LOGIN, code, _max_attempts(db)).ok
        if not ok or user is None:
            ratelimit.record_failure(db, "login", key)
            raise _fail(LOGIN_FAILED_MESSAGE, status_code=401)
    else:
        # 密码登录。没设过密码的账号 password_hash 是空串，verify_password 恒为 False，
        # 所以这里不需要（也不该）单独说一句「你没有设密码」——那又是一个探测面。
        if user is None or not verify_password(password, user.password_hash or ""):
            ratelimit.record_failure(db, "login", key)
            raise _fail("手机号或密码不正确。如果没设过密码，请用验证码登录。", status_code=401)

    if not user.is_active:
        # 停用账号**不计一次失败**：凭据是对的，这不是撞库，是本人在敲自己的门。
        # 计进去只会让他连「你的账号已停用」这句话都看不到（被 429 顶掉）。
        raise _fail("账号已停用", status_code=403)

    ratelimit.reset(db, "login", key)
    _touch_identity(db, user.id)
    # 取图凭证必须和令牌同时发：漏了它的表现是「登录成功，但一张图都打不开」，
    # 而接口全是 200、控制台一个错都不报。
    _issue_file_cookie(response, db, user, request)
    logger.info("用户 %s 通过手机号 %s 登录", user.username, sms.mask_phone(phone))
    return {
        "token": create_token(user.id, user.token_version or 0),
        "user": _user_to_dict(user),
    }
