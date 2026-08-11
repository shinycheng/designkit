"""自助注册（拿邀请码换一个账号）。**这是全系统唯一一条不需要登录的写接口。**

════════════════════════════════════════════════════════════════
 写这个文件时的假设：请求来自公网上的陌生人
════════════════════════════════════════════════════════════════
其余所有写接口的调用者都是「管理员在后台」或「已登录的成员」，出了事至少知道是谁。
这一条不是：任何人只要能访问到这台服务器，就能往这里发请求，而且可以一秒发一百次。
所以这个文件里的每一处「顺手做一下」都要先问一句「被人一天调十万次会怎样」。
具体落成了下面五条，改这个文件之前先读完：

**一、关着的时候必须真的关死。**
总开关（self_register_enabled）默认关闭，关着时这个接口直接拒绝，
不查库、不建号、不发任何外部请求。

**二、闸门是双向的：开着注册就不许再放内网。**
allow_internal_targets 打开时，对外 API 的 image_urls / callback_url 可以让
**服务端**去访问内网任意 http 地址——包括同一内网里那台 Sub2API 的管理端口，
上面挂着能建号、能改余额、能读任意用户明文 API Key 的接口。
而自助注册意味着陌生人能自己拿到 ERP Key，两者叠在一起就是「把内网的门
连同钥匙一起发出去」。所以这里**每一次注册都重新确认一遍**这个组合不成立，
不是只在打开开关那一刻查一次（详见 config.py 里那两条设置的说明）。
设置接口那边还要挡住「注册开着时把内网改回来」，两头都堵才叫闸门。

**三、限速在最前面，密码哈希在最后面。**
密码哈希是 20 万轮 PBKDF2（security.hash_password），一次约 0.1 秒 CPU。
不限速的话，光靠反复 POST 就能把这台机器的 CPU 吃满，什么漏洞都不需要。
所以顺序是固定的：先看总开关 → 再限速 → 再做各种便宜的校验 → 最后才哈希。

**四、报错不许变成探测器。**
「邀请码不存在」和「邀请码已用完」必须回同一句话——分开说，等于给了攻击者
一个「这串字符是不是有效邀请码」的判定器，可以拿去枚举。
用户名同理：不说「已被占用」，只说「不能用，换一个」，
否则这个接口就成了一份用户名花名册（谁在用这个系统、有没有 admin 这个号）。
代价是正常用户少了点信息，但他换一个名字就过去了，损失有限。

**五、注册链路和开通链路彻底解耦。**
注册成功只写本地数据库，**一次外部请求都不发**：登记一行状态为 pending 的
网关账号就立刻返回，真正去 Sub2API 建号、拿 Key 的那三次请求由后台调度器慢慢做。
这条界限是结构性的——网关关机、升级、换地址、接口全改，表现只能是
「新用户照常注册、照常登录，只是暂时不能生图」，绝不能变成「谁都注册不了」。
（同样的话写在 routers/users.py 的建号接口里，那是同一条规矩的另一半。）
"""
import logging
import math
from datetime import datetime
from typing import Any, Dict, Tuple

from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel
from sqlalchemy import func, select
from sqlalchemy import update as sa_update
from sqlalchemy.exc import IntegrityError, SQLAlchemyError
from sqlalchemy.orm import Session

from ..deps import client_ip, get_db
from ..models import AuthIdentity, InviteCode, InviteRedemption, User, utcnow
from ..security import hash_password
from ..services import provisioning, ratelimit, settings_service
from .invites import normalize_code
# 用户名规则**直接引用建号接口里的那一条**，不在这里另抄一份正则。
# 两处规则一旦写岔（比如那边允许 . 这边不允许），表现是「管理员建得出来的名字，
# 用户自己注册却被拒」，而两边的报错都言之凿凿，谁也看不出差在哪。
# 反过来的代价是：改 users._USERNAME_RE 时这里跟着变，这正是我们要的。
from .users import _USERNAME_RE

logger = logging.getLogger("designkit.register")

router = APIRouter(prefix="/api/web/register", tags=["网页-自助注册"])

# ── 限速用的两个 scope（services/ratelimit.py 按 scope + key 计数）──
# 按 IP：挡「一个脚本拿着一张多次码猛刷」。
SCOPE_IP = "register"
# 全站每天：最后一道兜底，挡「某张码的次数填错了」或者限速被绕过。
# key 固定成一个常量，因为它限的是整个站，不是某一个来源。
SCOPE_DAILY = "register_daily"
DAILY_KEY = "all"

# 自助注册的密码最短长度。**比管理员建号那条（8 位）更长，这是有意的**：
# 管理员设的初始密码寿命只有几分钟（must_change_password 强制本人立刻改掉），
# 而自己注册时设的密码是要一直用下去的，还是在公网上用。
MIN_PASSWORD_LENGTH = 10
# 上限纯粹是防呆和防滥用：哈希本身不怕长密码，但没必要接一段 10KB 的正文。
MAX_PASSWORD_LENGTH = 128
# 显示名长度。models.User.display_name 的列宽是 64，这里卡得更紧一点，
# 因为它会出现在成员列表、任务记录里，太长会把界面撑变形。
MAX_DISPLAY_NAME_LENGTH = 32

# 一眼就能撞开的口令。这份名单**不求全**——真正的强度靠「够长 + 至少两类字符 +
# 不含用户名」那三条撑着，这里只拦最经典的几个，免得有人用 password1234 注册完
# 第二天就被人登进去，然后觉得是系统不安全。
_WEAK_PASSWORDS = frozenset({
    "password", "password1", "password12", "password123", "passw0rd123",
    "1234567890", "12345678901", "123456789012", "qwertyuiop", "qwerty1234",
    "abcd123456", "abc123456", "a1234567890", "admin12345", "administrator",
    "designkit1", "designkit123", "iloveyou123", "welcome123", "letmein123",
})

# 给非技术用户看的规则说明。**GET 和 POST 用的是同一份文案**：
# 前端在表单下面显示的规则，和填错时弹出来的报错，必须逐字一致，
# 否则用户会照着上面写的填、然后被下面的报错说「不对」，谁也说不清该信哪个。
USERNAME_RULE_TEXT = "用户名只能用字母、数字和 . _ -，长度 3~32 位（例如 zhangsan 或 li.si）"
PASSWORD_RULE_TEXT = (
    "密码至少 %d 位，并且要同时包含字母和数字（可以再加符号），"
    "不能和用户名相同或包含用户名" % MIN_PASSWORD_LENGTH
)
# 注册没开放时对外说的话。**不解释为什么没开放**：
# 「因为管理员还没关掉内网访问」这种话对陌生人来说是一份免费的内部情报。
CLOSED_MESSAGE = "现在没有开放自助注册，请联系管理员帮你开通账号。"


class RegisterBody(BaseModel):
    invite_code: str = ""
    username: str = ""
    password: str = ""
    display_name: str = ""


def _fail(detail: str, status_code: int = 422) -> HTTPException:
    """统一的中文报错。理由同 routers/users.py：pydantic 自带的英文校验错误
    对非技术用户等于没提示，所以字段校验一律自己做、自己写话。"""
    return HTTPException(status_code=status_code, detail=detail)


# ────────────────────────────── 开关与闸门 ──────────────────────────────

def _gate_state(db: Session) -> Tuple[bool, str]:
    """现在到底开不开放注册，以及**给自己人看**的原因。

    返回的原因只写日志、只给管理员看，绝不返回给调用方（见 CLOSED_MESSAGE）。
    """
    if not bool(settings_service.get(db, "self_register_enabled")):
        return False, "总开关 self_register_enabled 是关的"
    if bool(settings_service.get(db, "allow_internal_targets")):
        # 双向闸门的这一半。正常路径上设置接口会拦住这个组合，能走到这里说明
        # 是老库里遗留的状态、或者有人直接改了数据库。宁可拒绝所有注册，
        # 也不能在这个组合下放一个陌生人进来（理由见文件头第二条）。
        return False, (
            "「允许访问内网地址」(allow_internal_targets) 还开着，"
            "开着它就不能开放注册"
        )
    return True, ""


def _require_open(db: Session) -> None:
    open_now, reason = _gate_state(db)
    if open_now:
        return
    if reason.startswith("「允许访问内网"):
        # 这一条要吼出来：管理员以为注册已经开了，实际上所有人都被挡在门外，
        # 而界面上（设置页开关是打开的）完全看不出来。
        logger.error(
            "自助注册被拒绝：%s。请到「系统设置」里把「允许访问内网地址」关掉，"
            "或者把自助注册关掉。", reason,
        )
    raise _fail(CLOSED_MESSAGE, status_code=403)


# ────────────────────────────── 限速 ──────────────────────────────

def _setting_int(db: Session, key: str, default: int, low: int, high: int) -> int:
    """读一个整数设置项并夹到范围内；读不出数字就用默认值。

    夹范围不是洁癖：这两个值管理员在网页上能改，填成 0 会变成「一个人都不许注册」，
    填成负数会让限速的判断整个失效，而两种都不会有任何报错。
    """
    raw = settings_service.get(db, key)
    value = default
    if raw is not None and not isinstance(raw, bool):
        try:
            value = int(str(raw).strip())
        except (TypeError, ValueError):
            value = default
    return max(low, min(high, value))


def _ip_policy(db: Session) -> ratelimit.Policy:
    """同一个来源每小时最多**尝试**几次（成功失败都算）。

    参数取自设置项 self_register_ip_per_hour，而不是 ratelimit 自己那套
    ratelimit_<scope>_* 键：那三个键在 RUNTIME_DEFAULTS 里没有对应项，
    管理员在设置页上改不到；而 self_register_ip_per_hour 是这个功能自带的、
    界面上看得见的那一个。所以这里显式构造 Policy 传进去（ratelimit 支持这么用）。
    """
    per_hour = _setting_int(db, "self_register_ip_per_hour", 20, 1, 1000)
    return ratelimit.Policy(max_attempts=per_hour, window_seconds=3600, block_seconds=3600)


def _daily_policy(db: Session) -> ratelimit.Policy:
    """全站每天最多**成功**注册几个（最后一道兜底）。

    窗口是「从当天第一次成功注册算起的 24 小时」，不是自然日零点清零——
    ratelimit 用的是滚动窗口，而库里存的又是 UTC 时间，去凑「北京时间零点」
    要么写一套时区换算，要么在跨时区部署时算错。名额用完时给的提示语里
    会写清楚还要等多久，所以对用户来说不存在「说好零点恢复却没恢复」的困惑。

    ⚠ **名额用完之后，把这个设置项调大不会立刻解封**（限速表里已经写上了
    「封到什么时候」，见 services/ratelimit.py 的 _maybe_block）。这是有意保留的：
    真到了那一刻，管理员想让某个人今天就进来，正确的做法是到「成员账号」页
    直接给他建号——那是一个针对具体某个人的决定，比「把全站的闸门开大」安全得多。
    上面那句提示语里也是这么告诉用户的。
    """
    limit = _setting_int(db, "self_register_daily_limit", 50, 1, 10000)
    return ratelimit.Policy(max_attempts=limit, window_seconds=86400, block_seconds=86400)


def _wait_text(seconds: int) -> str:
    """把「还要等多少秒」说成人话。**必须说出具体时间**，否则用户只会反复重试。"""
    if seconds <= 90 * 60:
        return "%d 分钟" % max(1, int(math.ceil(seconds / 60.0)))
    return "%d 小时" % max(1, int(math.ceil(seconds / 3600.0)))


def _guard_ip(db: Session, ip: str) -> None:
    """按来源 IP 限速，并**当场消耗掉一次配额**（不管这次注册成不成功）。

    为什么成功也算：这个配额挡的是「一个脚本拿着一张多次码猛刷」，
    而刷号成功的那些恰恰是最该被算进去的。真人一次注册撑死试三五次
    （码打错、用户名被占、密码太短），默认 20 次留了足够余量。

    ⚠ 套了反向代理时，取到的 IP 取决于设置项 trusted_proxy_hops 配得对不对
    （见 deps.client_ip）。配成 0 而实际有反代，全站会共用一个桶——
    比放开限速安全，但正常用户会互相挤。公网部署前务必把这一项配对。
    """
    policy = _ip_policy(db)
    decision = ratelimit.check(db, SCOPE_IP, ip, policy)
    if not decision.allowed:
        raise HTTPException(
            status_code=429,
            detail="注册尝试过于频繁，请 %s 后再试。" % _wait_text(decision.retry_after),
            # 标准头，给脚本和反代看的；浏览器不看它，所以话还要写在 detail 里
            headers={"Retry-After": str(max(1, decision.retry_after))},
        )
    ratelimit.record_failure(db, SCOPE_IP, ip, policy)


def _guard_daily(db: Session) -> None:
    """全站每日名额（只看，不消耗——真的注册成功之后才记一笔，见 _consume_daily）。"""
    policy = _daily_policy(db)
    decision = ratelimit.check(db, SCOPE_DAILY, DAILY_KEY, policy)
    if decision.allowed:
        return
    logger.warning(
        "全站每日注册名额已用完（上限 %d 个），本次注册被拒绝", policy.max_attempts)
    raise HTTPException(
        status_code=429,
        detail=(
            "今天开放的新账号名额已经用完了（每天最多 %d 个），请 %s 后再试，"
            "着急的话可以联系管理员直接给你建号。"
            % (policy.max_attempts, _wait_text(decision.retry_after))
        ),
        headers={"Retry-After": str(max(1, decision.retry_after))},
    )


def _consume_daily(db: Session) -> None:
    """注册成功了，占掉一个每日名额。

    放在**最后**、注册整个成功之后才记：记在前面的话，一个填错密码的用户
    会白白吃掉一个全站名额，几十次手滑就能把当天的名额耗光。
    ratelimit 自己会吞掉数据库异常（见那个文件的文件头），所以这里不用 try。
    """
    ratelimit.record_failure(db, SCOPE_DAILY, DAILY_KEY, _daily_policy(db))


# ────────────────────────────── 字段校验 ──────────────────────────────

def _validate_username(raw: str) -> str:
    username = (raw or "").strip()
    if not _USERNAME_RE.match(username):
        raise _fail(USERNAME_RULE_TEXT)
    return username


def _validate_display_name(raw: str) -> str:
    """显示名：可以不填（不填就用用户名），但不能太长、不能带控制字符。

    长度必须在这里卡住：models.User.display_name 是 String(64)，
    超长在 PostgreSQL 上会直接报错（SQLite 却照单全收），
    而那个错会以 500 的形式出现在一个陌生人的注册页上，谁也看不出发生了什么。
    控制字符（换行、制表）会把成员列表和日志排版搞乱，一并挡掉。
    """
    name = (raw or "").strip()
    if not name:
        return ""
    if len(name) > MAX_DISPLAY_NAME_LENGTH:
        raise _fail("显示名不能超过 %d 个字" % MAX_DISPLAY_NAME_LENGTH)
    if any(ord(ch) < 32 or ord(ch) == 127 for ch in name):
        raise _fail("显示名里不能有换行或制表符，请只填名字本身")
    return name


def _validate_password(raw: str, username: str) -> str:
    """密码强度。四条规则，每一条都对应一种真的会被撞开的密码。

    **不做「必须含大写字母 + 必须含符号」那一套。** 那种规则的实际效果是
    大家统一填 Abcd1234!，既没变强，又逼得人把密码写在便利贴上。
    这里要的是「够长 + 不是一眼能猜到的东西 + 和用户名无关」。
    """
    password = raw or ""
    if password != password.strip():
        # 复制粘贴时最常见的意外。不挡住的话，密码里带着一个尾随空格存进去，
        # 用户下次自己手打就登不进来了，而他会一口咬定「我密码没输错」。
        raise _fail("密码开头或结尾不能有空格（多半是复制的时候带进来的）")
    if len(password) < MIN_PASSWORD_LENGTH:
        raise _fail("密码至少 %d 位。%s" % (MIN_PASSWORD_LENGTH, PASSWORD_RULE_TEXT))
    if len(password) > MAX_PASSWORD_LENGTH:
        raise _fail("密码太长了，最多 %d 位" % MAX_PASSWORD_LENGTH)
    has_letter = any(ch.isalpha() for ch in password)
    has_digit = any(ch.isdigit() for ch in password)
    if not (has_letter and has_digit):
        # 纯数字和纯字母都会被这一条挡掉，而它们正是被字典撞开最快的两种。
        raise _fail("密码要同时包含字母和数字。%s" % PASSWORD_RULE_TEXT)
    lowered = password.lower()
    if lowered in _WEAK_PASSWORDS:
        raise _fail("这个密码太常见了，很容易被别人猜到，请换一个")
    if len(set(password)) <= 2:
        # aaaaaaaaaa1 这种：长度够、也有字母有数字，但实际强度约等于零
        raise _fail("密码不能是同几个字符重复，请换一个")
    name = (username or "").lower()
    if name and (name in lowered or lowered in name):
        # 用户名是公开的（登录页要填、别人也叫得出），拿它当密码等于没有密码
        raise _fail("密码不能和用户名相同，也不能包含用户名，请换一个")
    return password


# ────────────────────────────── 邀请码：原子扣减 ──────────────────────────────

# 邀请码出问题时的**唯一一句话**。不存在、打错了、已作废、过期了、名额用完了，
# 全都回这一句——分开说等于给攻击者一个「这串字符是不是有效邀请码」的判定器，
# 他可以拿着它慢慢枚举，而每一次探测在日志里看起来都只是一次普通的注册失败。
INVITE_INVALID_MESSAGE = (
    "邀请码用不了（可能是打错了、已经过期，或者名额已经用完）。"
    "请把码复制完整再试一次，或者找发码给你的人重新要一张。"
)


def _reserve_invite(db: Session, code_text: str, now: datetime) -> Tuple[int, str]:
    """抢一个名额，成功返回 (邀请码 id, 码本身)，抢不到就抛 INVITE_INVALID_MESSAGE。

    ════════════════════════════════════════════════════════════════
     为什么是一条带条件的 UPDATE，而不是「先查出来看看够不够，再写回去」
    ════════════════════════════════════════════════════════════════
    先查再写在并发下是错的：两个人同时用一张只剩一次名额的码，
    两边都会查到「还剩 1 次」、都通过检查、都写回 used_count+1，
    结果一张一次性码开出了两个账号——而这件事在测试里几乎不可能复现，
    只在真有人拿脚本刷的时候发生，也就是最不希望它发生的时候。

    把「判断」和「修改」压进同一条 SQL，由数据库保证只有一个人改得成，
    再看 rowcount 就知道自己是不是那个抢到的人。
    这个套路在本项目里已经用了三处（models.SyncState 的任务锁、
    services/ratelimit.py 的计数、scheduler 的调度锁），SQLite 和 PostgreSQL 都适用。

    四个条件缺一不可：码对得上、没作废、没过期、还有余量。
    过期判断写成 `expires_at IS NULL OR expires_at > now`，
    因为 NULL 在 SQL 里和任何值比较都是「未知」——漏掉 IS NULL 那一半，
    所有永不过期的码会全部失效，而且报错还是「邀请码用不了」，根本查不到这里。
    """
    result = db.execute(
        sa_update(InviteCode)
        .where(
            InviteCode.code == code_text,
            InviteCode.revoked_at.is_(None),
            (InviteCode.expires_at.is_(None)) | (InviteCode.expires_at > now),
            InviteCode.used_count < InviteCode.max_uses,
        )
        .values(used_count=InviteCode.used_count + 1)
        # synchronize_session=False：这条 UPDATE 的判断全在数据库里做，
        # 不需要 SQLAlchemy 再把结果同步回内存里的 ORM 对象（我们紧接着就提交、
        # 而且下面只查一个 id 列）。不写死这一项的话，它会自己去猜同步策略，
        # 而「expires_at 是 NULL」这类条件在两种策略下的行为并不一样。
        .execution_options(synchronize_session=False)
    )
    db.commit()
    if result.rowcount != 1:
        # 没抢到。**不去查这张码到底怎么了**：查出来也不能告诉用户（会变成探测器），
        # 而多一次查询只是给刷码的人多一份负担在我们自己身上。
        raise _fail(INVITE_INVALID_MESSAGE)
    row = db.execute(
        select(InviteCode.id).where(InviteCode.code == code_text)
    ).first()
    if row is None:
        # 扣减成功之后这张码不见了 = 同一瞬间被人从数据库里删了（界面上没有删除入口）。
        # 名额已经扣掉、也退不回去了（行都没了），继续注册即可，只是流水里
        # 记不上 invite_code_id——所以下面用 0 表示「码已不在」，由调用方存 None。
        logger.warning("邀请码 %s 在扣减之后消失了，注册照常进行", code_text)
        return 0, code_text
    return int(row[0]), code_text


def _refund_invite(db: Session, code_id: int) -> None:
    """把刚才抢到的名额还回去（注册最终失败时调用）。

    没有这一步的话，一个用户名撞车（两个人同时注册同一个名字）就会白白吃掉
    一张一次性码的唯一名额——用户看到「换个名字再试」，换完却发现码已经废了，
    而他手里根本没有第二张码可用。

    条件里的 `used_count > 0` 是防负数：万一同一行被别处同时改过，
    也不会把计数减到负数（负数会让这张码永远「还有余量」）。
    """
    if not code_id:
        return
    try:
        db.execute(
            sa_update(InviteCode)
            .where(InviteCode.id == code_id, InviteCode.used_count > 0)
            .values(used_count=InviteCode.used_count - 1)
            .execution_options(synchronize_session=False)
        )
        db.commit()
    except SQLAlchemyError:
        db.rollback()
        # 退不回去也不能让注册接口再抛一个新错误，把原本那个「用户名不能用」
        # 的提示盖掉——用户看到的应该是他能处理的那一条。
        logger.exception("邀请码 %s 的名额回退失败，这张码会少一次可用次数", code_id)


# ────────────────────────────── 建号 ──────────────────────────────

def _username_taken(db: Session, username: str) -> bool:
    """不区分大小写地查重（口径与 routers/users.py 的建号接口一致）。

    数据库的唯一索引是区分大小写的，也就是说 zhangsan 和 Zhangsan 能同时存在，
    但登录是精确匹配——两个人会各自登进不同的账号，而管理员在成员列表里
    看到的是两行几乎一样的名字，排查起来极其痛苦。
    """
    return db.query(User).filter(func.lower(User.username) == username.lower()).first() is not None


def _create_user(
    db: Session,
    username: str,
    password: str,
    display_name: str,
    code_id: int,
    code_text: str,
    ip: str,
    user_agent: str,
) -> User:
    """建账号 + 记一行登录方式 + 记一笔邀请码使用流水。**一个事务，要么全成要么全不成。**

    三张表必须一起提交：只建了 User 没建 AuthIdentity，这个人就成了「不知道
    从哪冒出来的账号」；只建了 User 没记流水，出事时查不出他是用哪张码进来的，
    而那正是 InviteRedemption 存在的唯一理由。
    """
    user = User(
        username=username,
        password_hash=hash_password(password),
        display_name=display_name,
        # 角色写死 member，**不读请求体里的任何字段**。
        # 这一行是这个接口最要紧的一行：从请求里取 role，就等于任何人
        # 在注册时加一个 "role": "admin" 就能拿到管理员权限。
        role="member",
        is_active=True,
        # 密码是他自己刚设的，没有「初始密码」这回事，不要强制他再改一次。
        # （管理员建号那条路才需要 must_change_password=True，因为那个密码
        #  是管理员知道的，不改掉的话「管理员能登进任何人的账号」一直成立。）
        must_change_password=False,
    )
    db.add(user)
    db.flush()  # 先拿到 user.id，下面两张表都要引用它

    db.add(AuthIdentity(
        user_id=user.id,
        provider="password",
        # 归一化成小写存（models.AuthIdentity.identifier 的注释要求写入方负责）。
        # 不归一的话 Tom 和 tom 会各占一行，将来把登录搬到这张表时按哪一行查就成了玄学。
        identifier=username.lower(),
        # 恒为 NULL：密码的唯一真相在 users.password_hash。两处都存，
        # 迟早出现「改了一处、另一处还是老密码」的鬼故事（见 models.py 的注释）。
        credential=None,
        # 密码方式没有「核验」这一说，建行时直接填创建时间
        verified_at=utcnow(),
        extra=None,
    ))
    db.add(InviteRedemption(
        # 码在扣减之后被人删掉时 code_id 是 0，这时存 None，
        # 下面的 code_snapshot 仍然留着码的字面值，照样对得上号。
        invite_code_id=code_id or None,
        code_snapshot=code_text[:32],
        user_id=user.id,
        username_snapshot=username[:64],
        # 两个字段都要截断：列宽在 PostgreSQL 上是硬约束，
        # 一个伪造的超长 User-Agent 能把这条 INSERT 直接打成 500。
        client_ip=(ip or "")[:45],
        user_agent=(user_agent or "")[:256],
    ))
    db.commit()
    db.refresh(user)
    return user


def _register_gateway_account(db: Session, user: User) -> None:
    """给新账号登记一行网关账号（状态 pending），**登记完立刻返回，不等开通**。

    这里一行网络请求都没有（provisioning.ensure_account 只写本地库），
    真正的开通是后台调度器的事——理由见文件头第五条，以及
    routers/users.py 里 _register_gateway_account 那一大段。

    登记失败**绝不回滚注册**：账号已经建好并提交了，这里再抛异常只会让一个
    刚注册成功的人看到 500，然后换个名字再注册一遍，多出一堆僵尸账号。
    缺的那一行由后台每分钟一轮自动补上（scheduler._users_without_account），
    补不上还有「管理员手工填 Key」这条路。
    """
    try:
        provisioning.ensure_account(db, user.id)
        db.commit()  # ensure_account 只 flush，提交由调用方负责
    except Exception:  # noqa: BLE001
        db.rollback()
        logger.exception(
            "新注册用户 %s 的网关账号行登记失败（账号已建好，后台会自动补登记）", user.username)


# ────────────────────────────── 接口 ──────────────────────────────

@router.get("")
def register_status(db: Session = Depends(get_db)) -> Dict[str, Any]:
    """现在开不开放注册 + 填写规则。**不需要登录**，登录页据此决定要不要显示「注册」入口。

    这个接口只回答「开不开放」，**不解释为什么不开放**：
    「因为管理员还没关掉内网访问」这种话，对着公网说等于免费送情报。
    真实原因写在服务端日志里，管理员看得到（见 _require_open）。

    规则文案由后端给，不是让前端自己写一份：前端抄一份的话，
    哪天规则改了就会出现「页面上写着至少 8 位、填了 8 位却报错要 10 位」。
    """
    open_now, _reason = _gate_state(db)
    return {
        "enabled": open_now,
        "message": "" if open_now else CLOSED_MESSAGE,
        "username_rule": USERNAME_RULE_TEXT,
        "password_rule": PASSWORD_RULE_TEXT,
        "min_password_length": MIN_PASSWORD_LENGTH,
    }


@router.post("")
def register(
    body: RegisterBody, request: Request, db: Session = Depends(get_db)
) -> Dict[str, Any]:
    """拿邀请码注册一个账号。**不需要登录。**

    下面这个顺序是刻意排的，不要随手调换：
      1. 总开关 + 内网闸门 —— 关着的时候什么都不做，连库都不多查一次；
      2. 按 IP 限速 —— 必须排在密码哈希（20 万轮 PBKDF2，约 0.1 秒 CPU）前面，
         否则光靠反复 POST 就能把 CPU 吃满；
      3. 全站每日名额 —— 只看不扣，扣在最后成功的时候；
      4. 各种便宜的字段校验 —— 让填错的人在扣掉邀请码名额之前就被挡回去；
      5. 用户名查重 —— 同上，**必须在扣名额之前**，否则一次重名就废掉一张一次性码；
      6. 原子扣减邀请码名额 —— 从这一步开始才算「占用了资源」，后面失败要退回去；
      7. 建号（哈希密码 + 三张表一个事务）；
      8. 登记网关账号行 + 记一笔每日名额 —— 都不影响注册本身的成败。
    """
    now = utcnow()
    _require_open(db)

    ip = client_ip(request, ratelimit.trusted_proxy_hops(db))
    _guard_ip(db, ip)
    _guard_daily(db)

    username = _validate_username(body.username)
    display_name = _validate_display_name(body.display_name)
    password = _validate_password(body.password, username)
    code_text = normalize_code(body.invite_code)
    if not code_text:
        raise _fail("请填写邀请码（管理员发给你的那一串字母数字）")

    if _username_taken(db, username):
        # **不说「已被占用」**：说了，这个接口就成了一份用户名花名册
        #（谁在用这个系统、有没有 admin 这个号），而那份名单是撞库的第一步。
        # 只说「不能用」，正常用户换一个就过去了。
        raise _fail("这个用户名不能用，请换一个（可以加上数字，比如 zhangsan2024）", status_code=409)

    code_id, code_snapshot = _reserve_invite(db, code_text, now)
    try:
        user = _create_user(
            db, username, password, display_name, code_id, code_snapshot,
            ip, request.headers.get("user-agent") or "",
        )
    except IntegrityError:
        # 两个人在同一瞬间注册同一个用户名：上面的查重会双双通过，
        # 由数据库的唯一索引兜底。名额要退回去，否则这张一次性码就白废了。
        db.rollback()
        _refund_invite(db, code_id)
        logger.info("用户名 %s 在并发注册中撞车，本次注册回退", username)
        raise _fail("这个用户名不能用，请换一个（可以加上数字，比如 zhangsan2024）", status_code=409)
    except Exception:
        db.rollback()
        _refund_invite(db, code_id)
        raise

    _register_gateway_account(db, user)
    _consume_daily(db)
    logger.info(
        "新用户 %s 通过邀请码 %s 完成自助注册（来源 IP %s）", user.username, code_snapshot, ip)

    return {
        "ok": True,
        # 这句话要把「接下来干什么」说完整：注册页和登录页是两个页面，
        # 不说清楚的话，用户会停在一个写着「注册成功」的页面上等着自己被跳转。
        "message": (
            "注册成功！请用刚才设置的用户名和密码登录。"
            "生图额度会在后台自动开通，通常一两分钟内完成；"
            "如果登录后提示还不能生图，等一会儿刷新一下，或者联系管理员。"
        ),
        "username": user.username,
        "display_name": user.display_name or user.username,
        # 注册接口**故意不发登录令牌**：注册和登录是两件事，
        # 让用户立刻用新密码登一次，既确认了他真的记住了密码，
        # 也让「谁在什么时候登录过」这条线从一开始就是干净的。
        "next": "login",
    }
