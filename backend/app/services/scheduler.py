"""后台定时任务的统一调度与互斥（两件事：灵感库同步、网关自动开通）。

**所有**同步入口（后台定时、管理员手动点按钮）都必须走这里的 `run_sync`，
它们共用同一把数据库锁——否则两路会同时导入，把 1.4 万条灵感库写成重复的两份。
网关自动开通同理，统一入口是 `run_provisioning`。

为什么不用 cron / APScheduler：
项目对非技术用户承诺「一条命令能跑」，不引入额外进程和依赖；这里用一个守护线程
配合数据库里的执行状态，重启不丢进度、多进程/多容器部署也只有一个进程真正执行。

锁的实现：带条件的 UPDATE + rowcount（SQLite 与 PostgreSQL 都适用）。
- 锁带 owner 令牌，释放时校验归属，绝不会清掉别人的锁；
- 锁带过期时间，进程崩溃后自动失效，不会永久悬挂；
- 长任务运行期间会续租，避免跑得久一点就被别人抢走。

sync_state 表的主键是任务名，所以两件事各占一行、各有各的锁：
`inspiration` 和 `provisioning` 互不阻塞（灵感库那一路要拉几十兆的文件，
一跑就是好几分钟，开通那一路等不起）。下面所有锁函数都带 `name=` 参数，
默认值是灵感库，纯粹是为了不改动已经跑了很久的调用方和测试。

════════════════════════════════════════════════════════════════
 网关自动开通：绝不能挡住任何人
════════════════════════════════════════════════════════════════
这里是「注册链路与开通链路彻底解耦」的后半截：注册接口只在本地登记一行
（provisioning.ensure_account，零网络请求），真正要跟 Sub2API 打交道的三次
HTTP 全部由本文件的后台线程慢慢做。所以 Sub2API 挂了、升级了、接口全改了，
表现只能是「所有人照常注册、照常登录、照常进工作台，只是生图要等管理员发一把
Key」，绝不会变成「新用户一个都进不来」。

对应地，本文件里任何一处开通失败都**只记日志、不抛出**：
- 一个用户失败不能连累同一轮里的其他人（每个人单独 try）；
- 一整轮失败不能让线程退出（_loop 里兜底）；
- 启动阶段读不到配置也不能挡住服务起来（start() 里兜底）。
"""
import logging
import threading
import uuid
from datetime import datetime, timedelta
from typing import Any, Dict, List, Optional, Tuple

from sqlalchemy import or_ as sa_or
from sqlalchemy import update as sa_update
from sqlalchemy.exc import IntegrityError

from ..database import SessionLocal
from ..models import SyncState, User, UserGatewayAccount
from . import inspiration, provisioning, settings_service

logger = logging.getLogger("designkit.scheduler")

TASK_NAME = "inspiration"
CHECK_INTERVAL_SECONDS = 300           # 每 5 分钟检查一次是否到期
LOCK_TTL = timedelta(minutes=20)       # 锁的有效期，运行中会持续续租
RENEW_INTERVAL = 300                   # 每 5 分钟续租一次
STALE_RUNNING = timedelta(hours=2)     # 超过它仍是 running 且无锁 → 认定为上次崩溃
FAILURE_BACKOFF = (                    # 连续失败后的退避，避免上游故障时反复重试
    timedelta(minutes=15), timedelta(hours=1), timedelta(hours=6),
)

# ── 网关自动开通那一路的参数 ────────────────────────────────────────
TASK_PROVISION = "provisioning"
# 每分钟看一眼。工作台上对用户的承诺是「通常一分钟内完成」，所以这里不能再稀了；
# 而且真正有活干的时候才会去抢锁（见 _tick），空转一次只是两条 SELECT。
PROVISION_INTERVAL_SECONDS = 60
# 开通那一路的锁比灵感库短得多：一轮最多几十个人，正常一两分钟就跑完。
# 短锁的意义是进程被杀之后，另一个进程最多等 10 分钟就能接手，而不是 20 分钟。
PROVISION_LOCK_TTL = timedelta(minutes=10)
PROVISION_RENEW_INTERVAL = 240         # 每 4 分钟续租一次（必须小于 TTL）
# 一轮最多推进多少人。控制上限是因为代登录被全局节流到 ≤10 次/分钟
#（Sub2API 的 20 次/分钟/IP 只留一半给我们用），20 个人差不多就是两分钟。
# 剩下的人下一轮接着来，早一分钟晚一分钟都不影响他们注册和登录。
PROVISION_BATCH = 20
# 余额同步的间隔与每轮条数。走的是网关鉴权的 GET /v1/usage（只读、不吃面板限流），
# **不要**改成 GET /api/v1/usage/stats——那条吃 Global(240rpm)+Heavy(60rpm)
# 两档限流，批量轮询很容易撞上，撞上之后连带把开通那一路也拖下水。
BALANCE_SYNC_INTERVAL = timedelta(minutes=10)
BALANCE_BATCH = 25
# 一轮最多给多少个「一行都还没有」的老用户补登记（见 _backfill_accounts）
BACKFILL_BATCH = 20


# ------------------------------------------------------------------ 状态行

def _ensure_row(db, name: str = TASK_NAME) -> SyncState:
    """取某个任务的状态行；并发首次插入会撞主键，回滚后重取即可（PG 上必须回滚）。

    name 默认是灵感库，这样已有调用方和测试一个字都不用改。
    """
    row = db.get(SyncState, name)
    if row is not None:
        return row
    row = SyncState(name=name, last_status="idle", last_message="")
    db.add(row)
    try:
        db.commit()
    except IntegrityError:
        db.rollback()  # 别的进程/线程刚插入了，PG 事务已中止必须回滚
        row = db.get(SyncState, name)
        if row is None:
            raise
    return row


def get_state() -> dict:
    """给接口用的同步状态（只读，不写库，避免 GET 请求里产生写冲突）。"""
    db = SessionLocal()
    try:
        row = db.get(SyncState, TASK_NAME)
        settings = settings_service.get_all(db)
        interval = max(1, int(settings.get("inspiration_sync_interval_hours") or 12))
        auto = bool(settings.get("inspiration_auto_sync"))
        if row is None:
            return {
                "auto_sync": auto, "interval_hours": interval,
                "last_status": "idle", "last_message": "", "running": False,
                "last_success_at": None, "last_started_at": None,
                "next_due_at": datetime.utcnow().isoformat() + "Z" if auto else None,
                "consecutive_failures": 0,
            }
        running = bool(row.lock_until and row.lock_until > datetime.utcnow())
        return {
            "auto_sync": auto,
            "interval_hours": interval,
            "last_status": row.last_status,
            "last_message": row.last_message or "",
            "running": running,
            "last_success_at": row.last_success_at.isoformat() + "Z" if row.last_success_at else None,
            "last_started_at": row.last_started_at.isoformat() + "Z" if row.last_started_at else None,
            # 下次时间要把失败退避算进去，否则失败时显示的时间是错的
            "next_due_at": (_due_at(row, interval).isoformat() + "Z") if auto else None,
            "consecutive_failures": row.consecutive_failures or 0,
        }
    finally:
        db.close()


def _due_at(row: SyncState, interval_hours: int) -> datetime:
    """算出下次该跑的时间；连续失败时按退避表往后推。"""
    base = row.last_success_at or row.last_started_at
    if base is None:
        return datetime.utcnow()  # 从未同步过：立刻跑一次
    if row.last_status == "failed" and row.consecutive_failures:
        idx = min(row.consecutive_failures, len(FAILURE_BACKOFF)) - 1
        return (row.last_started_at or base) + FAILURE_BACKOFF[idx]
    return base + timedelta(hours=interval_hours)


# ------------------------------------------------------------------ 锁

def _acquire(
    db,
    owner: str,
    now: Optional[datetime] = None,
    name: str = TASK_NAME,
    ttl: Optional[timedelta] = None,
) -> bool:
    """抢锁：只有把「空或已过期」的锁改成自己的那个调用者算抢到。"""
    now = now or datetime.utcnow()
    ttl = ttl or LOCK_TTL
    _ensure_row(db, name)
    result = db.execute(
        sa_update(SyncState)
        .where(
            SyncState.name == name,
            (SyncState.lock_until.is_(None)) | (SyncState.lock_until < now),
        )
        .values(lock_until=now + ttl, lock_owner=owner,
                last_started_at=now, last_status="running")
    )
    db.commit()
    return result.rowcount == 1


def _renew(db, owner: str, name: str = TASK_NAME, ttl: Optional[timedelta] = None) -> bool:
    """续租：长时间执行期间保住锁，避免跑久了被别人抢走导致两路同时跑。"""
    result = db.execute(
        sa_update(SyncState)
        .where(SyncState.name == name, SyncState.lock_owner == owner)
        .values(lock_until=datetime.utcnow() + (ttl or LOCK_TTL))
    )
    db.commit()
    return result.rowcount == 1


def _release(db, owner: str, ok: bool, message: str, name: str = TASK_NAME) -> None:
    """释放锁：只释放自己持有的那把（校验 owner），避免清掉别人的锁。"""
    now = datetime.utcnow()
    values = {
        "lock_until": None, "lock_owner": None,
        "last_status": "success" if ok else "failed",
        "last_message": message[:500],
    }
    if ok:
        values["last_success_at"] = now
        values["consecutive_failures"] = 0
    db.execute(
        sa_update(SyncState)
        .where(SyncState.name == name, SyncState.lock_owner == owner)
        .values(**values)
    )
    if not ok:
        db.execute(
            sa_update(SyncState)
            .where(SyncState.name == name)
            .values(consecutive_failures=(SyncState.consecutive_failures + 1))
        )
    db.commit()


def _renew_loop(owner: str, stop: threading.Event, name: str,
                interval: int, ttl: timedelta) -> None:
    """续租线程的循环体。两路任务共用同一段代码，免得改了一处忘了另一处。"""
    while not stop.wait(interval):
        renew_db = SessionLocal()
        try:
            if not _renew(renew_db, owner, name=name, ttl=ttl):
                logger.warning("任务锁已丢失（可能已超时被接管）：%s", name)
                return
        except Exception:
            logger.exception("续租任务锁失败：%s", name)
        finally:
            renew_db.close()


def recover_stale(db) -> None:
    """启动时把上次崩溃遗留的 running 状态清理掉（所有任务一起清）。

    否则 last_status 会永远卡在 running，界面显示「正在同步」但其实没有任何进程在跑。
    """
    now = datetime.utcnow()
    result = db.execute(
        sa_update(SyncState)
        .where(
            SyncState.last_status == "running",
            (SyncState.lock_until.is_(None)) | (SyncState.lock_until < now),
        )
        .values(lock_until=None, lock_owner=None, last_status="failed",
                last_message="上次执行被中断（服务重启或进程退出）")
    )
    db.commit()
    if result.rowcount:
        logger.info("已清理 %d 个中断的任务状态", result.rowcount)


# ------------------------------------------------------------------ 执行

def run_sync(trigger: str = "auto") -> Tuple[bool, str]:
    """执行一次同步。所有入口都必须走这里，保证全局只有一路在跑。

    返回 (是否真的执行了, 说明)。抢不到锁说明别人正在同步。
    """
    owner = "%s-%s" % (trigger, uuid.uuid4().hex[:12])
    db = SessionLocal()
    try:
        if not _acquire(db, owner):
            return False, "已有同步任务在进行中"
    finally:
        db.close()

    # 让手动同步的前端进度条也能看到自动同步的进展
    inspiration._set_status(state="running", message="正在同步…")

    stop_renew = threading.Event()
    renewer = threading.Thread(
        target=_renew_loop,
        args=(owner, stop_renew, TASK_NAME, RENEW_INTERVAL, LOCK_TTL),
        daemon=True, name="sync-lock-renew",
    )
    renewer.start()

    try:
        # 代理只用于访问 raw.githubusercontent.com；生图网关是局域网地址，不受影响
        proxy_db = SessionLocal()
        try:
            proxy = str(settings_service.get(proxy_db, "inspiration_proxy") or "").strip()
        finally:
            proxy_db.close()
        manifest = inspiration.fetch_to_cache(proxy=proxy)
        stats = inspiration.import_cache_to_db()
        message = "新增 %d、更新 %d、未变化 %d（上游快照 %s）" % (
            stats["created"], stats["updated"], stats["unchanged"],
            manifest.get("updatedAt", "未知"),
        )
        ok = True
        logger.info("灵感库同步完成（%s）：%s", trigger, message)
    except Exception as exc:
        message = str(exc)[:400]
        ok = False
        logger.warning("灵感库同步失败（%s）：%s", trigger, message)
    finally:
        stop_renew.set()

    inspiration._set_status(
        state="success" if ok else "failed", message=message,
        finished_at=datetime.utcnow().isoformat() + "Z",
    )
    db = SessionLocal()
    try:
        _release(db, owner, ok, message)
    finally:
        db.close()
    return True, message


# ------------------------------------------------------------------ 调度线程

class InspirationScheduler:
    def __init__(self) -> None:
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None
        self._busy = threading.Event()  # 停机时用来判断要不要等一等

    def start(self) -> None:
        db = SessionLocal()
        try:
            recover_stale(db)
        except Exception:
            logger.exception("清理中断同步状态失败（不影响启动）")
        finally:
            db.close()
        self._thread = threading.Thread(target=self._loop, daemon=True, name="inspiration-sync")
        self._thread.start()
        logger.info("灵感库自动同步调度器已启动（每 %d 分钟检查一次）", CHECK_INTERVAL_SECONDS // 60)

    def stop(self, wait_seconds: float = 10.0) -> None:
        """停机：先示意退出；若正在同步则短暂等待，让它有机会把状态写回。"""
        self._stop.set()
        if self._busy.is_set() and self._thread is not None:
            self._thread.join(timeout=wait_seconds)

    def _loop(self) -> None:
        # 启动后先等一会儿再检查，避免服务刚起来就抢网络与 CPU
        if self._stop.wait(30):
            return
        while True:
            try:
                self._tick()
            except Exception:
                logger.exception("自动同步检查出错（继续运行）")
            if self._stop.wait(CHECK_INTERVAL_SECONDS):
                return

    def _tick(self) -> None:
        db = SessionLocal()
        try:
            settings = settings_service.get_all(db)
            if not settings.get("inspiration_auto_sync"):
                return
            interval = max(1, int(settings.get("inspiration_sync_interval_hours") or 12))
            row = _ensure_row(db)
            if datetime.utcnow() < _due_at(row, interval):
                return
        finally:
            db.close()

        self._busy.set()
        try:
            run_sync(trigger="auto")
        finally:
            self._busy.clear()


# ══════════════════════════════════════════════════════════════════
#  网关自动开通：领活 → 推进状态机 → 同步余额
# ══════════════════════════════════════════════════════════════════
#
# 这一路做三件事，全部只读或幂等，随时可以被杀掉重来：
#   ① 给「一行都还没有」的老用户补登记（纯本地，见 _backfill_accounts）
#   ② 把 pending / 半路停下 / 可重试的 failed 往前推（provisioning.provision_due）
#   ③ 给已开通的用户同步余额，顺带探测 Key 还灵不灵（provisioning.sync_balance）
#
# 三件事都放在同一把锁里，是因为它们打的是同一个 Sub2API：分开跑等于把
# 代登录的节流额度和面板限流额度切成两半，谁也不知道另一半用掉多少。


def _balance_due_user_ids(db, now: Optional[datetime] = None,
                          limit: int = BALANCE_BATCH) -> List[int]:
    """该同步余额的用户：已开通、有 Key、且上次同步是很久以前（或从没同步过）。

    排序用 updated_at 而不是 balance_synced_at：后者可以为 NULL，而
    「NULL 排在前面还是后面」SQLite 和 PostgreSQL 的默认答案正好相反，
    加 NULLS FIRST 又要求 SQLite ≥ 3.30（群晖上的版本不保证）。
    updated_at 一定有值（default + onupdate），按它升序就是「最久没碰过的先来」，
    两个数据库结果一致，也不会有人被一直排在队尾饿死。
    """
    now = now or datetime.utcnow()
    cutoff = now - BALANCE_SYNC_INTERVAL
    rows = (
        db.query(UserGatewayAccount)
        .filter(
            UserGatewayAccount.state == "active",
            UserGatewayAccount.api_key_enc.isnot(None),
            sa_or(
                UserGatewayAccount.balance_synced_at.is_(None),
                UserGatewayAccount.balance_synced_at < cutoff,
            ),
        )
        .order_by(UserGatewayAccount.updated_at.asc())
        .limit(limit)
        .all()
    )
    return [row.user_id for row in rows]


def _users_without_account(db, limit: int = BACKFILL_BATCH) -> List[int]:
    """还没有网关账号行的**在职**用户。

    两种来源：① 自动开通这个功能上线之前就已经存在的老用户；
    ② 建号那一刻登记失败了（provisioning.ensure_account 的兜底分支，
    建号本身照常成功——绝不让开通拖累建号）。
    只补 is_active 的人：停用的人本来就不该生图，给他在网关上建一个账号
    纯属浪费一个名额，而且没人认领。
    """
    rows = (
        db.query(User.id)
        .outerjoin(UserGatewayAccount, UserGatewayAccount.user_id == User.id)
        .filter(UserGatewayAccount.id.is_(None), User.is_active.is_(True))
        .order_by(User.id.asc())
        .limit(limit)
        .all()
    )
    return [row[0] for row in rows]


def _backfill_accounts(db, cfg) -> int:
    """给缺行的在职用户补一行登记（纯本地，零网络请求）。返回补了几行。

    ensure_account 自己会判断总开关：开着就登记成 pending（等着被自动开通），
    关着就登记成 manual（等管理员手工发 Key）。所以这里不需要再判一次。

    一行都补不上也没关系：失败只记日志，本轮的其他两件事照常做。
    """
    created = 0
    for user_id in _users_without_account(db):
        try:
            provisioning.ensure_account(db, user_id, cfg=cfg)
            db.commit()
            created += 1
        except Exception:
            db.rollback()
            logger.exception("给用户 %s 补登记网关账号行失败（不影响其他人）", user_id)
    if created:
        logger.info("已给 %d 个还没有网关账号行的成员补上登记", created)
    return created


def run_provisioning(trigger: str = "auto") -> Tuple[bool, str]:
    """跑一轮网关自动开通。所有入口都走这里，全局只有一路在跑。

    返回 (是否真的执行了, 说明)。抢不到锁说明别的进程/线程正在跑这一轮。
    说明里**只有数字**，绝不会带上任何密码、Key 或用户在网关的邮箱。
    """
    owner = "%s-%s" % (trigger, uuid.uuid4().hex[:12])
    db = SessionLocal()
    try:
        if not _acquire(db, owner, name=TASK_PROVISION, ttl=PROVISION_LOCK_TTL):
            return False, "已有开通任务在进行中"
    finally:
        db.close()

    stop_renew = threading.Event()
    renewer = threading.Thread(
        target=_renew_loop,
        args=(owner, stop_renew, TASK_PROVISION,
              PROVISION_RENEW_INTERVAL, PROVISION_LOCK_TTL),
        daemon=True, name="provision-lock-renew",
    )
    renewer.start()

    ok = True
    db = SessionLocal()
    try:
        cfg = provisioning.load_config(db)
        if not cfg.enabled:
            # 开关关着时仍然补登记（登记成 manual），这样管理员一打开开关，
            # 所有人已经有行了，下一轮直接开通，不用等第二轮。
            _backfill_accounts(db, cfg)
            message = "自动开通总开关没打开，本轮只补了登记"
        elif provisioning.is_halted():
            # 全局暂停：每个用户都会以同样的方式失败（后台模式、验证码、合规、
            # 管理员 Key 失效……），挨个去试只会把所有人的 attempts 白白刷高，
            # 等管理员发现问题时人已经全被降级成 manual 了。
            message = "自动开通已全局暂停：%s" % provisioning.halt_reason()[0]
        else:
            backfilled = _backfill_accounts(db, cfg)
            results = provisioning.provision_due(db, limit=PROVISION_BATCH)
            done = sum(1 for r in results if r.outcome == "active")
            failed = sum(1 for r in results if r.outcome == "failed")
            synced, broken = _sync_balances(db, cfg)
            message = (
                "补登记 %d 人；推进 %d 人（开通成功 %d、失败 %d）；"
                "同步余额 %d 人（发现 %d 人的 Key 已失效）"
                % (backfilled, len(results), done, failed, synced, broken)
            )
            if results or synced or broken:
                logger.info("网关自动开通完成（%s）：%s", trigger, message)
    except Exception as exc:  # noqa: BLE001 —— 一轮失败绝不能让线程退出
        ok = False
        message = str(exc)[:400]
        logger.exception("网关自动开通这一轮出错（%s）", trigger)
    finally:
        stop_renew.set()
        db.close()

    db = SessionLocal()
    try:
        _release(db, owner, ok, message, name=TASK_PROVISION)
    finally:
        db.close()
    return True, message


def _sync_balances(db, cfg) -> Tuple[int, int]:
    """给已开通的用户同步余额，返回 (同步成功几人, 发现几人的 Key 失效了)。

    一条连接跑完一批。单个人出错只记日志继续下一个——一个人的 Key 被删了
    不该让其他人的余额也停止更新。
    """
    user_ids = _balance_due_user_ids(db)
    if not user_ids:
        return 0, 0
    synced = 0
    broken = 0
    client = provisioning.build_client(cfg)
    try:
        for user_id in user_ids:
            try:
                result = provisioning.sync_balance(db, user_id, client=client, cfg=cfg)
            except Exception:
                db.rollback()
                logger.exception("同步用户 %s 的余额出错（不影响其他人）", user_id)
                continue
            if result.outcome == "active":
                synced += 1
            elif result.outcome == "failed":
                broken += 1
    finally:
        client.close()
    return synced, broken


def get_provisioning_state() -> Dict[str, Any]:
    """这一路的执行状态（只读，不写库）。给设置页那个面板显示「上次跑于几点」。"""
    db = SessionLocal()
    try:
        row = db.get(SyncState, TASK_PROVISION)
        if row is None:
            return {"last_status": "idle", "last_message": "", "running": False,
                    "last_started_at": None, "last_success_at": None,
                    "interval_seconds": PROVISION_INTERVAL_SECONDS}
        return {
            "last_status": row.last_status,
            "last_message": row.last_message or "",
            "running": bool(row.lock_until and row.lock_until > datetime.utcnow()),
            "last_started_at": row.last_started_at.isoformat() + "Z" if row.last_started_at else None,
            "last_success_at": row.last_success_at.isoformat() + "Z" if row.last_success_at else None,
            "interval_seconds": PROVISION_INTERVAL_SECONDS,
        }
    finally:
        db.close()


class ProvisioningScheduler:
    """每分钟看一眼「有没有人要开通 / 有没有余额该同步了」。

    和灵感库那个调度器分开成两个线程，是因为灵感库一跑就是几分钟（下载几十兆），
    合在一起的话新注册的人要干等它跑完才轮得到开通——而工作台上写着
    「通常一分钟内完成」。

    支持 wake()：管理员在成员页点「重新开通」之后，不用干等下一个整分钟。
    """

    def __init__(self) -> None:
        # 一个事件同时承担「该干活了」和「该退出了」两件事，靠 _stopping 区分。
        # 用两个 Event 的话，「等 60 秒、但这两件事任一发生就提前醒」写起来
        # 要么轮询要么容易漏事件，一个信号 + 一个标志位最简单也最不容易错。
        self._signal = threading.Event()
        self._stopping = False
        self._thread: Optional[threading.Thread] = None
        self._busy = threading.Event()

    def start(self) -> None:
        self._thread = threading.Thread(
            target=self._loop, daemon=True, name="gateway-provisioning")
        self._thread.start()
        logger.info("网关自动开通调度器已启动（每 %d 秒检查一次）", PROVISION_INTERVAL_SECONDS)

    def stop(self, wait_seconds: float = 10.0) -> None:
        self._stopping = True
        self._signal.set()
        if self._busy.is_set() and self._thread is not None:
            self._thread.join(timeout=wait_seconds)

    def wake(self) -> None:
        """立刻跑一轮（管理员点了「重新开通」）。只是提前醒，不在调用方线程里干活。

        接口线程绝不能同步等这一轮：开通要打三次外部接口，其中代登录还带
        全局节流，最坏情况要等两分钟——那时候管理员的浏览器早就超时了，
        而他会以为是「重试失败」。所以这里只负责叫醒，结果去列表里看。
        """
        self._signal.set()

    def _loop(self) -> None:
        # 启动后先缓一会儿：建表、seed、历史数据回填都在 lifespan 里排在前面，
        # 那时候去查用户表纯属和它们抢锁。
        self._signal.wait(20)
        self._signal.clear()
        while not self._stopping:
            try:
                self._tick()
            except Exception:
                logger.exception("网关自动开通检查出错（继续运行）")
            if self._stopping:
                return
            self._signal.wait(PROVISION_INTERVAL_SECONDS)
            self._signal.clear()

    def _tick(self) -> None:
        """先用两条只读查询判断有没有活干，有才去抢锁。

        绝大多数轮次是没活干的（一个稳定运行的系统，人早就都开通好了），
        这样每分钟只多两条 SELECT，不会每分钟往 sync_state 里写一次。
        """
        db = SessionLocal()
        try:
            cfg = provisioning.load_config(db)
            if not cfg.enabled:
                return
            if provisioning.is_halted():
                return
            has_work = bool(
                provisioning.due_user_ids(db, limit=1)
                or _balance_due_user_ids(db, limit=1)
                or _users_without_account(db, limit=1)
            )
        except Exception:
            # 读配置/查库失败（数据库正在重启之类）不该让线程退出，下一轮再来
            logger.exception("检查是否有待开通的成员时出错")
            return
        finally:
            db.close()
        if not has_work:
            return

        self._busy.set()
        try:
            run_provisioning(trigger="auto")
        finally:
            self._busy.clear()


class BackgroundSchedulers:
    """两个后台调度器打包成一个，好让 main.py 那一句 scheduler.start() 不用改。

    main.py 的 lifespan 里只认 scheduler.start() / scheduler.stop() 这一对入口
    （见该文件 lifespan 函数）。新增一路后台任务时把它加进这里，
    而不是去 lifespan 里再加一行——启动顺序的坑（建表、seed、回填必须先跑完）
    已经在那边处理好了，能不动就不动。

    **任何一路启动失败都不许挡住服务启动**：一路挂了顶多是那件事不自动做了，
    整个服务起不来则是所有人都进不去。
    """

    def __init__(self) -> None:
        self.inspiration = InspirationScheduler()
        self.provisioning = ProvisioningScheduler()

    def start(self) -> None:
        for name, item in (("灵感库同步", self.inspiration),
                           ("网关自动开通", self.provisioning)):
            try:
                item.start()
            except Exception:
                logger.exception("%s调度器启动失败（不影响服务启动）", name)

    def stop(self, wait_seconds: float = 10.0) -> None:
        for item in (self.inspiration, self.provisioning):
            try:
                item.stop(wait_seconds=wait_seconds)
            except Exception:
                logger.exception("停止后台调度器时出错")


scheduler = BackgroundSchedulers()


def wake_provisioning() -> None:
    """叫醒开通调度器（给接口层用，比如管理员点了「重新开通」）。

    单独包一层是为了让调用方只 import 这个模块，不用知道 scheduler 里面
    有几个调度器、叫什么名字。叫不醒也不报错：最多等一分钟自然轮到。
    """
    try:
        scheduler.provisioning.wake()
    except Exception:
        logger.exception("叫醒网关自动开通调度器失败（不影响排队，最多等一轮）")
