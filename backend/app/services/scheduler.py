"""灵感库同步的统一调度与互斥。

**所有**同步入口（后台定时、管理员手动点按钮）都必须走这里的 `run_sync`，
它们共用同一把数据库锁——否则两路会同时导入，把 1.4 万条灵感库写成重复的两份。

为什么不用 cron / APScheduler：
项目对非技术用户承诺「一条命令能跑」，不引入额外进程和依赖；这里用一个守护线程
配合数据库里的执行状态，重启不丢进度、多进程/多容器部署也只有一个进程真正执行。

锁的实现：带条件的 UPDATE + rowcount（SQLite 与 PostgreSQL 都适用）。
- 锁带 owner 令牌，释放时校验归属，绝不会清掉别人的锁；
- 锁带过期时间，进程崩溃后自动失效，不会永久悬挂；
- 长任务运行期间会续租，避免跑得久一点就被别人抢走。
"""
import logging
import threading
import uuid
from datetime import datetime, timedelta
from typing import Optional, Tuple

from sqlalchemy import update as sa_update
from sqlalchemy.exc import IntegrityError

from ..database import SessionLocal
from ..models import SyncState
from . import inspiration, settings_service

logger = logging.getLogger("designkit.scheduler")

TASK_NAME = "inspiration"
CHECK_INTERVAL_SECONDS = 300           # 每 5 分钟检查一次是否到期
LOCK_TTL = timedelta(minutes=20)       # 锁的有效期，运行中会持续续租
RENEW_INTERVAL = 300                   # 每 5 分钟续租一次
STALE_RUNNING = timedelta(hours=2)     # 超过它仍是 running 且无锁 → 认定为上次崩溃
FAILURE_BACKOFF = (                    # 连续失败后的退避，避免上游故障时反复重试
    timedelta(minutes=15), timedelta(hours=1), timedelta(hours=6),
)


# ------------------------------------------------------------------ 状态行

def _ensure_row(db) -> SyncState:
    """取单例状态行；并发首次插入会撞主键，回滚后重取即可（PG 上必须回滚）。"""
    row = db.get(SyncState, TASK_NAME)
    if row is not None:
        return row
    row = SyncState(name=TASK_NAME, last_status="idle", last_message="")
    db.add(row)
    try:
        db.commit()
    except IntegrityError:
        db.rollback()  # 别的进程/线程刚插入了，PG 事务已中止必须回滚
        row = db.get(SyncState, TASK_NAME)
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

def _acquire(db, owner: str, now: Optional[datetime] = None) -> bool:
    """抢锁：只有把「空或已过期」的锁改成自己的那个调用者算抢到。"""
    now = now or datetime.utcnow()
    _ensure_row(db)
    result = db.execute(
        sa_update(SyncState)
        .where(
            SyncState.name == TASK_NAME,
            (SyncState.lock_until.is_(None)) | (SyncState.lock_until < now),
        )
        .values(lock_until=now + LOCK_TTL, lock_owner=owner,
                last_started_at=now, last_status="running")
    )
    db.commit()
    return result.rowcount == 1


def _renew(db, owner: str) -> bool:
    """续租：长时间同步期间保住锁，避免跑久了被别人抢走导致并发导入。"""
    result = db.execute(
        sa_update(SyncState)
        .where(SyncState.name == TASK_NAME, SyncState.lock_owner == owner)
        .values(lock_until=datetime.utcnow() + LOCK_TTL)
    )
    db.commit()
    return result.rowcount == 1


def _release(db, owner: str, ok: bool, message: str) -> None:
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
        .where(SyncState.name == TASK_NAME, SyncState.lock_owner == owner)
        .values(**values)
    )
    if not ok:
        db.execute(
            sa_update(SyncState)
            .where(SyncState.name == TASK_NAME)
            .values(consecutive_failures=(SyncState.consecutive_failures + 1))
        )
    db.commit()


def recover_stale(db) -> None:
    """启动时把上次崩溃遗留的 running 状态清理掉。

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
                last_message="上次同步被中断（服务重启或进程退出）")
    )
    db.commit()
    if result.rowcount:
        logger.info("已清理 %d 个中断的同步状态", result.rowcount)


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

    def _renew_loop():
        while not stop_renew.wait(RENEW_INTERVAL):
            renew_db = SessionLocal()
            try:
                if not _renew(renew_db, owner):
                    logger.warning("同步锁已丢失（可能已超时被接管）")
                    return
            except Exception:
                logger.exception("续租同步锁失败")
            finally:
                renew_db.close()

    renewer = threading.Thread(target=_renew_loop, daemon=True, name="sync-lock-renew")
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


scheduler = InspirationScheduler()
