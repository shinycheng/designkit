"""通用限速：按「scope（限哪一类）+ key（限谁）」计数，状态落数据库。

════════════════════════════════════════════════════════════════════════
 为什么不是进程内的字典
════════════════════════════════════════════════════════════════════════
改造之前，登录限速写在 routers/auth.py 里，是一个 `defaultdict(deque)`。
它在「一个进程、一个容器、内部几个人用」的时候够用，但只要账号能自助注册、
系统开始面向更多人，它有三个具体问题：

  1. **多进程 / 多容器各存一份**。uvicorn 多 worker、或 NAS 上跑两个容器时，
     设置里写的「10 次」实际是「10 × 进程数」——而且界面上看不出来，
     管理员以为自己配的是 10。
  2. **重启即解封**。字典在内存里，服务一重启（升级、`restart: always` 拉起）
     攒下的失败记录全部清零。
  3. **内存只增不减**。原来的实现只在**登录成功**时清除失败记录，key 又是由
     「IP + 用户名」拼出来的，两段都是攻击者能随便写的——不停用随机用户名去撞，
     字典就一直涨，谁也不会去看它有多大。

落库之后：全进程共用同一份计数、重启不丢、过期行由本文件定期删掉，
并且每个 scope 有行数上限（见 MAX_ROWS_PER_SCOPE），不会无限增长。

**不引入 Redis。** 用户是群晖 NAS 单机部署，多一个中间件就多一份她要装、要维护、
要备份、会半夜挂掉的东西。原子性用「带条件的 UPDATE + 看 rowcount」实现，
写法照抄 models.py 里 SyncState 那把任务锁，SQLite 与 PostgreSQL 都适用。

════════════════════════════════════════════════════════════════════════
 这个模块是通用的，不只给登录用
════════════════════════════════════════════════════════════════════════
接口全部是 (db, scope, key)。以后加手机号验证码、开放注册，只要挑一个新的
scope 名字就行，不用改这里一行代码：

    ratelimit.check(db, "sms_code", phone)          # 发短信前先问一句
    ratelimit.record_failure(db, "sms_code", phone) # 发出去了就记一次
    ratelimit.reset(db, "login", key)               # 成功了就清账

阈值和窗口从设置里读，键名的约定是（见 config.py 的 RUNTIME_DEFAULTS）：

    ratelimit_<scope>_max_attempts     多少次之后开始拦
    ratelimit_<scope>_window_minutes   多长时间内累计
    ratelimit_<scope>_block_minutes    触发之后拦多久

设置里没有对应的键时，退回本文件 _SCOPE_POLICY / DEFAULT_POLICY 里的默认值——
所以新加一个 scope 即使忘了配设置项也能跑，只是改不了参数。

════════════════════════════════════════════════════════════════════════
 一条刻意的取舍：数据库出错时**放行**
════════════════════════════════════════════════════════════════════════
下面每个公开函数都把数据库异常吞掉、记一条日志、然后当作「允许」返回。
理由是限速表坏掉（或还没建出来）不该让所有人登不进系统；而真到了数据库不可用
的时候，登录本身也要查 users 表，一样会失败，攻击者并不会因此多拿到什么。
反过来若选择「出错就拦」，一次无关的数据库抖动就能把全体用户关在门外，
运营同学在群晖上完全无从排查。
"""
import logging
import math
import re
import threading
import time
from datetime import datetime, timedelta
from typing import NamedTuple, Optional

from sqlalchemy import delete as sa_delete
from sqlalchemy import func, select
from sqlalchemy import update as sa_update
from sqlalchemy.exc import IntegrityError, SQLAlchemyError
from sqlalchemy.orm import Session

from ..models import RateLimitState
from . import settings_service

logger = logging.getLogger("designkit.ratelimit")


class Policy(NamedTuple):
    """一类限速的三个参数：窗口内允许几次、窗口多长、超了封多久（单位都是秒）。"""

    max_attempts: int
    window_seconds: int
    block_seconds: int


# 没有为某个 scope 单独配置时用这个。10 次 / 15 分钟 / 封 15 分钟，
# 与改造前 routers/auth.py 里写死的那三个数**完全一致**——升级当天行为不变，
# 这样万一新逻辑有问题，也能一眼分清是「参数变了」还是「实现出错了」。
DEFAULT_POLICY = Policy(max_attempts=10, window_seconds=15 * 60, block_seconds=15 * 60)

# 各 scope 的默认值（设置项没填时的后备）。以后加 sms_code / register 就在这里补一行。
_SCOPE_POLICY = {
    "login": DEFAULT_POLICY,
}

# 每个 scope 最多留多少行。key 由攻击者可控的内容拼成（登录是 IP + 用户名），
# 所以必须有上限，否则一轮撞库就能往表里塞几十万行。
# 5000 这个数的来由：正常内部使用一天也就几十行；真被撞库时，超出上限的部分会按
# 「最后活动时间」从旧到新删掉，被删的那些行本来也早就不影响判断了。
MAX_ROWS_PER_SCOPE = 5000
# 一次清理最多删多少行。分批删是为了避免一条 DELETE 锁住整张表太久
# （PostgreSQL 上大批量删除会拖慢同时发生的登录）。
CLEANUP_BATCH = 1000
# 进程内的清理节流：同一个进程最多每 60 秒清一次。清理只在「新建了一行」之后触发，
# 平时登录一次是零额外开销。
_GC_MIN_INTERVAL_SECONDS = 60
# 初值是负无穷、**不是 0**。time.monotonic() 的零点没有规定：macOS 上它是从进程启动
# 开始算的（实测新进程里就是 0.00x），Linux 上是从开机开始算。写 0 的话，
# 在 macOS 上「now - 0 < 60」永远成立，进程活着的头 60 秒里一次清理都不会发生——
# 而单元测试恰好整个都跑在这 60 秒内，于是「表会无限涨」这种问题测不出来。
# 负无穷表示「从来没清理过」，任何时钟起点下都成立。
_gc_last_at = float("-inf")
_gc_lock = threading.Lock()

# key 写进库之前截断到这个长度，必须 ≤ models.RateLimitState.key 的列宽（128）。
# PostgreSQL 上超长会直接报错，SQLite 却照单全收——两边行为不一致的 bug 最难查，
# 所以在进库之前就截齐。
MAX_KEY_LENGTH = 128

# 给用户看的提示语里，这一类限速叫什么。没登记的 scope 就说「操作」。
_SCOPE_LABEL = {
    "login": "登录",
}


class Decision(NamedTuple):
    """一次限速判断的结果。

    allowed      能不能放行
    retry_after  还要等多少**秒**（放行时是 0）。适合直接放进 HTTP 的 Retry-After 头。
    attempts     当前窗口内已经累计了几次
    message      给非技术用户看的中文提示，拦下时直接当 HTTP 报错正文用
    """

    allowed: bool
    retry_after: int
    attempts: int
    message: str


_ALLOWED = Decision(allowed=True, retry_after=0, attempts=0, message="")


# ------------------------------------------------------------------ 配置读取

#: 连续 7 位以上的数字：手机号、身份证号、卡号这类东西的共同形状。
#: IP 地址被点号断开，每段最多 3 位，所以不会被误伤；用户名里也极少有这么长的纯数字串。
_LONG_DIGITS = re.compile(r"\d{7,}")


def mask_key(key: str) -> str:
    """打日志之前给 key 脱敏：把长数字串中间糊掉，只留前 3 后 4。

    为什么要有这个函数：这个模块是**通用**限速，调用方传什么当 key 它并不知道。
    最初只有登录在用，key 是「IP|用户名」，打出来没问题——注释里当时也是这么写的。
    后来手机号注册接进来，key 直接就是手机号，于是完整号码进了日志。
    日志是会被打包发给别人排错的，等于把用户手机号发了出去。

    所以这里改成「默认不信任 key 的内容」。IP 和用户名照常可读（排错要用），
    长数字串一律糊掉。调用方如果传的是已经哈希过的 key（推荐做法，见
    routers/phone.py），这一层就只是第二道保险。
    """
    text = str(key or "")

    def _mask(match):
        digits = match.group(0)
        if len(digits) <= 7:
            return digits[:1] + "*" * (len(digits) - 1)
        return digits[:3] + "*" * (len(digits) - 7) + digits[-4:]

    return _LONG_DIGITS.sub(_mask, text)


def _clamp(value: int, low: int, high: int) -> int:
    return max(low, min(high, value))


def _int_setting(db: Session, key: str) -> Optional[int]:
    """读一个整数设置项；没有、或填了鬼东西就返回 None（由调用方退回默认值）。

    设置项是管理员在网页上填的，填成空串、中文数字、负号都可能发生。
    这里绝不让一个填错的值把限速整个搞瘫，读不出数字就当没配。
    """
    try:
        raw = settings_service.get(db, key)
    except SQLAlchemyError:
        logger.exception("读取限速设置 %s 失败，改用默认值", key)
        return None
    if raw is None or isinstance(raw, bool):  # bool 是 int 的子类，必须先排掉
        return None
    try:
        return int(str(raw).strip())
    except (TypeError, ValueError):
        logger.warning("限速设置 %s 的值不是整数（%r），改用默认值", key, raw)
        return None


def policy_for(db: Session, scope: str) -> Policy:
    """取某个 scope 的限速参数：设置项优先，没配就用代码里的默认值。

    上下限是硬夹住的，理由是这几个值管理员在网页上能改：
      - 次数至少 1（填 0 会变成「一次都不许」，等于把所有人锁在门外）；
      - 窗口至少 10 秒、最多 24 小时（填成 0 会让计数窗口永远处于「已过期」，
        每次失败都重置成第 1 次，限速名存实亡）。
    """
    base = _SCOPE_POLICY.get(scope, DEFAULT_POLICY)
    max_attempts = _int_setting(db, "ratelimit_%s_max_attempts" % scope)
    window_minutes = _int_setting(db, "ratelimit_%s_window_minutes" % scope)
    block_minutes = _int_setting(db, "ratelimit_%s_block_minutes" % scope)
    return Policy(
        max_attempts=_clamp(
            base.max_attempts if max_attempts is None else max_attempts, 1, 10000
        ),
        window_seconds=_clamp(
            base.window_seconds if window_minutes is None else window_minutes * 60,
            10, 24 * 3600,
        ),
        block_seconds=_clamp(
            base.block_seconds if block_minutes is None else block_minutes * 60,
            10, 24 * 3600,
        ),
    )


def trusted_proxy_hops(db: Session) -> int:
    """信任几层反向代理（给 deps.client_ip 用）。读不出来就按 0 = 没有反代。

    默认 0 是刻意的：填多了比填少了危险得多。填少了最多是「所有人共用一个限速桶」
    （严了一点，但拦不住的只有攻击者自己）；填多了会去信一段客户端能随便伪造的
    X-Forwarded-For，攻击者每次请求换一个假 IP，限速就彻底作废了。
    """
    hops = _int_setting(db, "trusted_proxy_hops")
    if hops is None:
        return 0
    # 上限 10 纯粹是防手滑（填成 100 之类），真实部署撑死两三层。
    return _clamp(hops, 0, 10)


# ------------------------------------------------------------------ 内部工具

def _norm_scope(scope: str) -> str:
    return (scope or "default").strip().lower()[:32] or "default"


def _norm_key(key: str) -> str:
    """key 进库前的规整：去空白、小写、**截断**。

    截断是安全要求不是美观要求：登录那一路的 key 是 "IP|用户名" 拼出来的，
    用户名来自请求体，攻击者塞一个 10KB 的用户名进来，不截断就会拿它去写数据库
    （PostgreSQL 直接报错、整个登录接口 500）。
    截断带来的最坏情况是两个超长 key 撞成同一行、共用一个计数桶——而登录 key 的
    前一段是 IP，要撞上先得来自同一个 IP，也就是攻击者自己跟自己撞，无所谓。
    """
    text = (key or "").strip().lower()
    return text[:MAX_KEY_LENGTH] or "-"


def _label(scope: str) -> str:
    return _SCOPE_LABEL.get(scope, "操作")


def _blocked_message(scope: str, retry_after: int) -> str:
    """拦下时给用户看的话。**必须说清楚要等多久**，否则用户只会反复重试。"""
    minutes = max(1, int(math.ceil(retry_after / 60.0)))
    return "%s尝试过于频繁，请 %d 分钟后再试" % (_label(scope), minutes)


def _seconds_until(moment: datetime, now: datetime) -> int:
    return max(1, int(math.ceil((moment - now).total_seconds())))


class _Snapshot(NamedTuple):
    """一行限速记录当前的样子（只取判断用得上的三列）。"""

    window_start: datetime
    attempts: int
    blocked_until: Optional[datetime]


def _read(db: Session, scope: str, key: str) -> Optional[_Snapshot]:
    """读一行的最新值。

    **故意不用 db.get(RateLimitState, (scope, key))**：这个项目的 SessionLocal 配的是
    `expire_on_commit=False`（见 database.py），提交之后 ORM 对象里的属性仍是提交前
    那一份缓存；而本模块所有的写都是 UPDATE 语句，根本不经过那个对象。
    用 ORM 读的话，「刚 +1 完读回来还是旧数字」——限速会永远差一拍，而且只在
    同一个请求里连着读写时才复现，测试很容易漏掉。直接查列就没有这个问题。
    """
    row = db.execute(
        select(
            RateLimitState.window_start,
            RateLimitState.attempts,
            RateLimitState.blocked_until,
        ).where(RateLimitState.scope == scope, RateLimitState.key == key)
    ).first()
    if row is None:
        return None
    return _Snapshot(
        window_start=row[0] or datetime.utcnow(),
        attempts=int(row[1] or 0),
        blocked_until=row[2],
    )


# ------------------------------------------------------------------ 对外接口

def check(db: Session, scope: str, key: str, policy: Optional[Policy] = None) -> Decision:
    """问一句「现在能不能做」。**只读，不计数**。

    放在真正的动作（校验密码、发短信）之前调用。只读是有意的：这样连续两次
    check 不会把人越问越死，也不会在 GET 请求里产生写库动作。
    """
    scope = _norm_scope(scope)
    key = _norm_key(key)
    policy = policy or policy_for(db, scope)
    now = datetime.utcnow()
    try:
        row = _read(db, scope, key)
        if row is None:
            return _ALLOWED
        # ① 正在封锁期内
        if row.blocked_until is not None and row.blocked_until > now:
            retry = _seconds_until(row.blocked_until, now)
            return Decision(False, retry, row.attempts, _blocked_message(scope, retry))
        # ② 没写封锁标记，但本窗口内已经超过阈值。
        # 正常情况下 record_failure 会在超阈值的那一刻写上 blocked_until，走不到这里；
        # 留着这一条是为了「阈值被管理员改小了」——已经攒了 12 次的人，
        # 阈值从 20 改成 10 之后应该立刻被拦住，而不是等下一次失败才生效。
        window_end = row.window_start + timedelta(seconds=policy.window_seconds)
        if window_end > now and row.attempts >= policy.max_attempts:
            retry = _seconds_until(window_end, now)
            return Decision(False, retry, row.attempts, _blocked_message(scope, retry))
        return Decision(True, 0, row.attempts, "")
    except SQLAlchemyError:
        # 见文件头「数据库出错时放行」。rollback 是必须的：PostgreSQL 上事务一旦出错，
        # 同一个 Session 后面所有语句都会直接报错——不回滚的话，这里放行了，
        # 紧接着查 users 表的登录逻辑会莫名其妙全部失败。
        db.rollback()
        logger.exception("限速检查失败（scope=%s），本次放行", scope)
        return _ALLOWED


def record_failure(
    db: Session, scope: str, key: str, policy: Optional[Policy] = None
) -> Decision:
    """记一次「失败 / 消耗一次配额」，返回记完之后的状态。

    登录那一路在密码校验不通过时调用。返回值里的 allowed=False 表示**这一次之后**
    已经被封了——调用方通常不用管（本次请求已经因为密码错误返回 401 了），
    但拿它写日志或改提示语都行。
    """
    scope = _norm_scope(scope)
    key = _norm_key(key)
    policy = policy or policy_for(db, scope)
    now = datetime.utcnow()
    try:
        created = _bump(db, scope, key, policy, now)
        row = _read(db, scope, key)
        if row is None:
            # 理论上不会发生（刚写完就没了 = 同一瞬间被清理删掉）。
            # 当作放行，下一次失败会重新建行。
            return _ALLOWED
        decision = _maybe_block(db, scope, key, policy, row, now)
        if created:
            # 只在**新建了一行**之后才顺手清一次（还有 60 秒的进程内节流），
            # 因为只有新 key 会让表变大。命中老行的失败不需要做任何额外的事。
            _maybe_cleanup(db, scope, policy)
        return decision
    except SQLAlchemyError:
        db.rollback()
        logger.exception("限速计数失败（scope=%s），本次不计数", scope)
        return _ALLOWED


def reset(db: Session, scope: str, key: str) -> None:
    """清账（登录成功时调用）：直接把这一行删掉。

    删掉而不是把 attempts 归零：这张表里没有任何需要保留的历史，行少了清理压力也小。
    """
    scope = _norm_scope(scope)
    key = _norm_key(key)
    try:
        db.execute(
            sa_delete(RateLimitState).where(
                RateLimitState.scope == scope, RateLimitState.key == key
            )
        )
        db.commit()
    except SQLAlchemyError:
        db.rollback()
        logger.exception("清除限速记录失败（scope=%s）", scope)


# ------------------------------------------------------------------ 原子计数

def _bump(db: Session, scope: str, key: str, policy: Policy, now: datetime) -> bool:
    """把计数 +1。返回「是不是新建了一行」。

    三条路径，每一条都是**带条件的 UPDATE + 看 rowcount**（写法照抄 scheduler.py
    里那把任务锁）。为什么非要这么写：两个进程同时处理同一个人的失败登录时，
    「先 SELECT 出 attempts，再写回 attempts+1」会丢掉其中一次——
    攻击者只要并发去撞，就能让计数永远追不上真实次数。
    条件 UPDATE 把「判断」和「修改」压进一条 SQL 里交给数据库去保证。
    """
    window_floor = now - timedelta(seconds=policy.window_seconds)

    def _bump_in_window() -> bool:
        # 窗口还没过期 → 直接 +1。attempts + 1 是在数据库里算的，不是在 Python 里。
        result = db.execute(
            sa_update(RateLimitState)
            .where(
                RateLimitState.scope == scope,
                RateLimitState.key == key,
                RateLimitState.window_start > window_floor,
            )
            .values(attempts=RateLimitState.attempts + 1, updated_at=now)
        )
        db.commit()
        return result.rowcount == 1

    def _restart_window() -> bool:
        # 窗口过期了 → 整行重置成「新窗口的第 1 次」。
        # 条件里带上「没在封锁期内」：封锁时间可以配得比窗口长，那时不能因为窗口
        # 到点就把 blocked_until 抹掉——否则封 1 小时会被自动缩短成 15 分钟。
        result = db.execute(
            sa_update(RateLimitState)
            .where(
                RateLimitState.scope == scope,
                RateLimitState.key == key,
                RateLimitState.window_start <= window_floor,
                (RateLimitState.blocked_until.is_(None))
                | (RateLimitState.blocked_until <= now),
            )
            .values(window_start=now, attempts=1, blocked_until=None, updated_at=now)
        )
        db.commit()
        return result.rowcount == 1

    def _insert_first() -> bool:
        # 还没有这一行 → 插入。并发下会撞主键，撞了就回滚（PostgreSQL 上事务已中止，
        # 不回滚后面全废），交给外层再走一遍 +1 的路径。
        db.add(
            RateLimitState(
                scope=scope, key=key, window_start=now, attempts=1,
                blocked_until=None, updated_at=now,
            )
        )
        try:
            db.commit()
            return True
        except IntegrityError:
            db.rollback()
            return False

    if _bump_in_window():
        return False
    if _restart_window():
        return False
    if _insert_first():
        return True
    # 走到这里只有两种可能：① 插入时和别的进程撞了主键；② 这一行正处在封锁期
    # （窗口过期但 blocked_until 还没到，_restart_window 的条件不成立）。
    # 两种都再试一次「窗口内 +1」，成不成都不再往下折腾——封锁期内多记一次少记一次
    # 不影响结论（本来就已经拦住了）。
    _bump_in_window()
    return False


def _maybe_block(
    db: Session, scope: str, key: str, policy: Policy, row: _Snapshot, now: datetime
) -> Decision:
    """次数够了就写上封锁时间。同样是条件 UPDATE，保证只有一个人写得成、只记一条日志。"""
    if row.blocked_until is not None and row.blocked_until > now:
        retry = _seconds_until(row.blocked_until, now)
        return Decision(False, retry, row.attempts, _blocked_message(scope, retry))
    if row.attempts < policy.max_attempts:
        return Decision(True, 0, row.attempts, "")

    blocked_until = now + timedelta(seconds=policy.block_seconds)
    result = db.execute(
        sa_update(RateLimitState)
        .where(
            RateLimitState.scope == scope,
            RateLimitState.key == key,
            RateLimitState.attempts >= policy.max_attempts,
            (RateLimitState.blocked_until.is_(None))
            | (RateLimitState.blocked_until <= now),
        )
        .values(blocked_until=blocked_until, updated_at=now)
    )
    db.commit()
    if result.rowcount == 1:
        # 记一条日志：管理员来问「我为什么登不上」时，这是唯一的线索。
        logger.warning(
            "限速触发：scope=%s key=%s 在 %d 秒内失败 %d 次，封锁至 %s",
            scope, mask_key(key), policy.window_seconds, row.attempts,
            blocked_until.isoformat(),
        )
    retry = _seconds_until(blocked_until, now)
    return Decision(False, retry, row.attempts, _blocked_message(scope, retry))


# ------------------------------------------------------------------ 清理

def _maybe_cleanup(db: Session, scope: str, policy: Policy) -> None:
    """节流版清理：同一个进程最多 60 秒清一次，失败只记日志、绝不连累调用方。"""
    global _gc_last_at
    now = time.monotonic()
    with _gc_lock:
        if now - _gc_last_at < _GC_MIN_INTERVAL_SECONDS:
            return
        _gc_last_at = now
    try:
        cleanup(db, scope, policy)
    except SQLAlchemyError:
        db.rollback()
        logger.exception("清理限速表失败（scope=%s）", scope)


def cleanup(db: Session, scope: str, policy: Optional[Policy] = None) -> int:
    """删掉一个 scope 下已经不起作用的行，返回删了几行。

    两步，缺一不可：
      1. **按时间删**：窗口和封锁都早已过期的行，留着也不会影响任何判断。
         保留时长取「窗口和封锁里较长的那个 × 2」，留一倍余量是为了防时钟回拨
         （群晖上 NTP 校时把系统时间往回调过，这种事真会发生）。
      2. **按行数封顶**：光按时间删挡不住短时间内的撞库——攻击者一分钟塞十万个
         不同的用户名进来，那些行个个都还「没过期」。所以再按最后活动时间从旧到新
         删到 MAX_ROWS_PER_SCOPE 以内。被删掉的是最久没动过的那些，
         它们最可能是攻击者一次性造出来的噪声；真在反复尝试的那个 key 会一直被刷新，
         排在最新的一头，删不到它。
    """
    scope = _norm_scope(scope)
    policy = policy or policy_for(db, scope)
    now = datetime.utcnow()
    retention = timedelta(seconds=max(policy.window_seconds, policy.block_seconds) * 2)
    removed = 0

    result = db.execute(
        sa_delete(RateLimitState).where(
            RateLimitState.scope == scope,
            RateLimitState.updated_at < now - retention,
        )
    )
    db.commit()
    removed += int(result.rowcount or 0)

    total = db.execute(
        select(func.count()).select_from(RateLimitState).where(RateLimitState.scope == scope)
    ).scalar_one()
    if total > MAX_ROWS_PER_SCOPE:
        # 先把要删的 key 查出来再删，**不写成 DELETE ... WHERE key IN (子查询带 LIMIT)**：
        # MySQL/SQLite/PostgreSQL 对「删除的表出现在自己的子查询里」限制各不相同，
        # 分两步是三种数据库上都稳的写法，代价只是多一次查询（而且只在超限时才发生）。
        victims = db.execute(
            select(RateLimitState.key)
            .where(RateLimitState.scope == scope)
            .order_by(RateLimitState.updated_at.desc())
            .offset(MAX_ROWS_PER_SCOPE)
            .limit(CLEANUP_BATCH)
        ).scalars().all()
        if victims:
            result = db.execute(
                sa_delete(RateLimitState).where(
                    RateLimitState.scope == scope,
                    RateLimitState.key.in_(list(victims)),
                )
            )
            db.commit()
            removed += int(result.rowcount or 0)
            logger.warning(
                "限速表 scope=%s 超过 %d 行（当前 %d 行），已删掉最久未活动的 %d 行。"
                "短时间内出现大量不同的来源/用户名，通常意味着正在被撞库。",
                scope, MAX_ROWS_PER_SCOPE, total, removed,
            )
    return removed
