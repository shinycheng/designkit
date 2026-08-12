"""后台生成 worker：数据库就是任务队列。

- 网页/对外 API 创建任务后立刻返回（异步），worker 轮询领取并执行；
- 任务状态落库，程序重启不丢任务；启动时把中断的 processing 任务重置回 pending；
- 失败自动重试（次数可配），最终失败会记录可读的错误原因并触发回调。

════════════════════════════════════════════════════════════════
 为什么要有「派发权」这么一层：多开 worker 会把并发数悄悄乘几倍
════════════════════════════════════════════════════════════════
公网部署要开多个 uvicorn worker（`--workers 4`），而 uvicorn 的每个 worker
是一个**独立进程**，各自完整跑一遍 main.py 的 lifespan——也就是各自
`worker.start()` 一次，各自建一个 `ThreadPoolExecutor(max_workers=并发数)`。

后果：管理员在设置页填「同时生成 2 张」，开 4 个进程实际是 **8 张同时在跑**，
而界面上显示的还是 2。这不是性能问题，是**费用和稳定性**问题：
每一张图都是真金白银，网关（Sub2API）那边还可能有并发上限，一超就整批报错，
运营同学看到的却只是「今天怎么老是生成失败」，完全无从查起。

修法：**只有一个进程真正跑派发循环**，其余进程安静待命。
选谁靠数据库里的一把租约锁（`sync_state` 表，任务名 `generation`），
直接复用 scheduler.py 里那套「带条件 UPDATE + owner 令牌 + 过期续租」——
不新增表、不新增依赖，SQLite 和 PostgreSQL 都适用。

三条设计取舍写在这里，改的时候请一并考虑：

1. **租约要短（30 秒）并且持续续租（10 秒一次）**。
   领导者进程被 kill -9 之后，租约最多 30 秒过期，待命进程 5 秒一轮去接管，
   所以最坏 35 秒就恢复出图。租约设成几分钟当然更省数据库写入，但表现会是
   「重启后好几分钟不出图」，而运营完全不知道自己在等什么，只会以为坏了。

2. **续租写在派发循环里，而不是另起一个续租线程**（这点和 scheduler.py 不同）。
   scheduler 那两路是「一跑好几分钟的长任务」，必须有独立线程替它续租；
   这里的循环每秒转一圈、每步都是很快的数据库操作，把续租放在同一个循环里
   反而更安全：万一派发循环卡死了，它同时也就续不上租，另一个进程会顶上，
   而不是「租约还在续、活却没人干」。

3. 交接的瞬间，旧领导者手里已经在跑的图会继续跑完（半路掐断等于白花钱），
   所以极端情况下短时间内可能超出并发上限一点点。这与「进程崩溃」是同一类
   情况，无法也不必消除；正常运行期间上限是严格的。

启动时的两件善后（重置中断任务、补投未送达回调）也一并挪到拿到派发权之后做：
以前每个进程都做一遍，4 个进程就把同一个回调发 4 次给对接方的 ERP。
"""
import logging
import os
import threading
import time
import uuid
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta
from typing import Any, Dict, List, Optional

from sqlalchemy import update as sa_update

from ..database import SessionLocal
from ..models import ApiKey, GeneratedImage, GenerationJob
from ..serializers import job_to_dict
from . import inspiration, prompt_studio, provider, settings_service, storage, webhook
from . import scheduler as scheduler_locks

logger = logging.getLogger("designkit.worker")

POLL_INTERVAL = 1.0
RETRY_BACKOFF_SECONDS = 5

# ── 派发权租约的参数 ────────────────────────────────────────────────
# 任务名。sync_state 的主键就是任务名，加一个名字就多一把互不干扰的锁，
# 不需要改表结构（灵感库用 inspiration、自动开通用 provisioning）。
LEASE_TASK = "generation"
# 租约有效期。领导者进程被杀之后，别人最多等这么久就能接手。
LEASE_TTL = timedelta(seconds=30)
# 续租间隔，必须**明显小于** TTL：留出至少两次重试的余量，
# 免得数据库偶尔卡一下（备份、检查点）就把派发权抖没了。
LEASE_RENEW_SECONDS = 10
# 待命进程多久尝试接管一次。5 秒 + 30 秒 TTL = 最坏 35 秒恢复出图。
STANDBY_RETRY_SECONDS = 5
# 领导者多久报一次「现在跑着几张、队里还剩几个」。
# 这行日志是将来排查「为什么图出得慢」的唯一线索，所以宁可多打一点；
# 但只在真的有活干的时候打，空闲时一行都不打，免得日志被刷满。
HEARTBEAT_SECONDS = 60
# 启动时重置卡住任务的年龄阈值：超过它仍是 processing 才认定为「上次中断」。
# 要大于生图的最坏合法耗时（降级逐张时预算为 request_timeout(≤900) × n(≤4) = 3600 秒），
# 否则多进程部署下会把还在正常执行的任务误判为中断、重新入队重复计费。
STUCK_RESET_SECONDS = 4500

# 图片签名链接的默认有效期（小时），与 config.py 的 erp_file_link_ttl_hours 默认值一致。
DEFAULT_ERP_LINK_TTL_HOURS = 168


def _erp_link_ttl_hours(settings: Dict[str, Any]) -> int:
    """取「给 ERP 的图片链接有效几小时」，并兜住设置里被填成非数字的情况。

    为什么要兜：settings_router 的数字范围校验里没有 erp_file_link_ttl_hours
    （只覆盖了并发数、重试次数等五项），管理员在设置页填个「七天」就会原样存成
    字符串，而 storage.to_url 遇到非数字会抛 StorageError。这个异常一旦在
    worker.start() 的「补投未送达回调」那段里抛出来，整个应用就起不来，
    群晖上 restart: always 会把它变成无限重启——一个填错的字数导致全站打不开，
    运营同学根本无从下手。

    所以这里回落到默认值并记一条 WARNING：链接照样是签过名的，安全性没有降级，
    只是有效期回到 7 天，而日志里留着这条线索。
    """
    raw = settings.get("erp_file_link_ttl_hours")
    try:
        return int(raw)
    except (TypeError, ValueError):
        logger.warning(
            "设置项 erp_file_link_ttl_hours 不是数字（当前值：%r），"
            "图片链接有效期按默认 %d 小时处理，请到设置页改成整数",
            raw, DEFAULT_ERP_LINK_TTL_HOURS,
        )
        return DEFAULT_ERP_LINK_TTL_HOURS


class GenerationWorker:
    def __init__(self) -> None:
        self._stop = threading.Event()
        self._executor: Optional[ThreadPoolExecutor] = None
        self._dispatcher: Optional[threading.Thread] = None
        self._lock = threading.Lock()
        self._inflight = 0
        self._concurrency = 2
        # ── 派发权 ──
        # 令牌带上进程号，只是为了让日志和数据库里的 lock_owner 能一眼对上
        # 「现在是哪个进程在派发」；随机后缀防止进程号被系统复用后认错人。
        self._owner = "pid%d-%s" % (os.getpid(), uuid.uuid4().hex[:8])
        self._is_leader = False
        self._renew_at = 0.0        # 单调时钟：到点就续租（不受系统改时间影响）
        self._heartbeat_at = 0.0
        self._standby_logged = False  # 待命日志只打第一次，之后闭嘴

    # ------------------------------------------------------------ lifecycle

    def start(self) -> None:
        db = SessionLocal()
        try:
            self._concurrency = max(1, min(8, int(settings_service.get(db, "worker_concurrency") or 2)))
        finally:
            db.close()

        # 线程池在每个进程里都建，但**只有拿到派发权的那个进程会往里丢任务**，
        # 所以待命进程的线程池是空的（ThreadPoolExecutor 的线程是提交时才创建的，
        # 空池不占资源）。这样万一本进程后来接管了派发权，可以立刻开工。
        self._executor = ThreadPoolExecutor(
            max_workers=self._concurrency, thread_name_prefix="genworker"
        )
        self._dispatcher = threading.Thread(
            target=self._dispatch_loop, daemon=True, name="generation-dispatch")
        self._dispatcher.start()
        logger.info(
            "生成 worker 已启动（进程 %d，令牌 %s）：正在竞争派发权；"
            "拿到派发权的那个进程会把全局同时生成数控制在 %d 张",
            os.getpid(), self._owner, self._concurrency,
        )

    def stop(self) -> None:
        self._stop.set()
        # 主动交还派发权：不还的话，重启后新进程要干等租约过期（最多 30 秒）
        # 才敢开工，表现就是「重启完半分钟不出图」。还了就是立刻接上。
        if self._is_leader:
            self._is_leader = False
            self._release_lease()
        if self._executor is not None:
            # 取消尚未开始的排队任务；已在执行的生图任务让它自然跑完（下次启动会补投回调）
            try:
                self._executor.shutdown(wait=False, cancel_futures=True)
            except TypeError:  # 老版本 Python 无 cancel_futures 参数
                self._executor.shutdown(wait=False)

    # ------------------------------------------------------------ 派发权

    def _ensure_leadership(self) -> bool:
        """返回「本进程现在能不能派发任务」。顺带完成续租 / 尝试接管。

        数据库出错时**不改变**当前身份，只让异常往上抛给循环里的兜底：
        领导者不该因为一次数据库抖动就放弃派发权（那会让出图停顿一整个 TTL），
        真丢了派发权的判据只有一个——续租的 UPDATE 明确影响了 0 行，
        也就是这把锁已经被别人拿走了。
        """
        # 正在停机就不要再去抢了：stop() 刚把租约交还出去，这里再抢回来的话，
        # 进程一退，这把锁会一直挂到过期，接班的进程白等 30 秒。
        if self._stop.is_set():
            return False
        now = time.monotonic()

        if self._is_leader:
            if now < self._renew_at:
                return True
            db = SessionLocal()
            try:
                renewed = scheduler_locks._renew(
                    db, self._owner, name=LEASE_TASK, ttl=LEASE_TTL)
            finally:
                db.close()
            if renewed:
                self._renew_at = time.monotonic() + LEASE_RENEW_SECONDS
                return True
            # 续租失败 = 本进程曾经卡住超过 30 秒，租约过期被别人接管了。
            # 立刻停止领取新任务，手里在跑的让它跑完（掐断等于白花钱）。
            with self._lock:
                inflight = self._inflight
            self._is_leader = False
            self._standby_logged = False
            logger.warning(
                "本进程（%d）失去了生成派发权（续租时发现锁已易主），停止领取新任务转入待命；"
                "手上还在生成的 %d 张会跑完", os.getpid(), inflight,
            )
            return False

        # 待命中：试着接管。抢不到说明别的进程好好活着，什么也不用做。
        db = SessionLocal()
        try:
            acquired = scheduler_locks._acquire(
                db, self._owner, name=LEASE_TASK, ttl=LEASE_TTL)
        finally:
            db.close()
        if not acquired:
            if not self._standby_logged:
                logger.info(
                    "生成派发权在别的进程手里，本进程（%d）转入待命：每 %d 秒尝试接管一次，"
                    "领导者进程一旦退出最多 %d 秒后由别人接手（本条日志不再重复打印）",
                    os.getpid(), STANDBY_RETRY_SECONDS, int(LEASE_TTL.total_seconds()),
                )
                self._standby_logged = True
            return False

        self._is_leader = True
        self._standby_logged = False
        self._renew_at = time.monotonic() + LEASE_RENEW_SECONDS
        self._heartbeat_at = 0.0
        logger.info(
            "本进程（%d，令牌 %s）取得生成派发权：全局同时生成上限 %d 张；"
            "租约 %d 秒、每 %d 秒续租一次",
            os.getpid(), self._owner, self._concurrency,
            int(LEASE_TTL.total_seconds()), LEASE_RENEW_SECONDS,
        )
        self._on_became_leader()
        return True

    def _release_lease(self) -> None:
        """交还派发权。校验 owner，绝不会清掉别人的锁（复用 scheduler 的释放逻辑）。"""
        db = SessionLocal()
        try:
            scheduler_locks._release(
                db, self._owner, True, "派发进程正常退出，已交还派发权", name=LEASE_TASK)
        except Exception:
            # 交还失败无所谓：租约 30 秒后自然过期，别人照样接得上
            logger.warning("交还生成派发权时出错（不影响：租约到期后会自动释放）", exc_info=True)
        finally:
            db.close()

    def _on_became_leader(self) -> None:
        """拿到派发权之后的善后。只有领导者做，所以多进程部署下只做一遍。

        失败不能让派发循环退出——善后没做成顶多是几个旧任务晚点恢复，
        而循环死了就是**从此不再出图**，两者严重程度差着数量级。
        """
        try:
            self._recover_interrupted()
        except Exception:
            logger.exception("恢复中断任务/补投回调失败（不影响新任务的生成）")

    def _recover_interrupted(self) -> None:
        db = SessionLocal()
        try:
            # 上次异常退出时卡在 processing 的任务，重置回队列。
            # 只重置 started_at 早于阈值的，避免多进程部署时误抢别的进程正在跑的新任务。
            cutoff = datetime.utcnow() - timedelta(seconds=STUCK_RESET_SECONDS)
            stuck = (
                db.query(GenerationJob)
                .filter(GenerationJob.status == "processing")
                .filter(
                    (GenerationJob.started_at.is_(None))
                    | (GenerationJob.started_at <= cutoff)
                )
                .all()
            )
            for job in stuck:
                job.status = "pending"
            if stuck:
                db.commit()
                logger.info("恢复了 %d 个中断的任务", len(stuck))

            # 已完结但回调未送达/中断的任务，补投一次。
            # 这段必须只有领导者做：以前每个进程启动时都做一遍，开 4 个 worker
            # 就是同一条回调发 4 次给对接方的 ERP（筛选条件里含 "sending"，
            # 别的进程刚置上的 "sending" 挡不住后面的进程）。
            pending_hooks = (
                db.query(GenerationJob)
                .filter(GenerationJob.status.in_(("succeeded", "failed")))
                .filter(GenerationJob.callback_url.isnot(None))
                .filter(GenerationJob.webhook_status.in_(("", "sending")))
                .all()
            )
            settings = settings_service.get_all(db)
            for job in pending_hooks:
                self._fire_webhook(db, job, settings)
            if pending_hooks:
                logger.info("补投了 %d 个未完成的回调", len(pending_hooks))
        finally:
            db.close()

    def _heartbeat(self) -> None:
        """定期把「谁在派发、跑着几张、还剩几个」写进日志。

        运营唯一能描述的现象是「图出得好慢」，而慢有三种完全不同的原因：
        队列积压、并发被调小了、或者派发权跑到了另一个进程上。
        这一行日志同时回答这三个问题，没有它只能靠猜。
        """
        now = time.monotonic()
        if now < self._heartbeat_at:
            return
        self._heartbeat_at = now + HEARTBEAT_SECONDS
        with self._lock:
            inflight = self._inflight
        db = SessionLocal()
        try:
            pending = (
                db.query(GenerationJob)
                .filter(GenerationJob.status == "pending")
                .count()
            )
        finally:
            db.close()
        if inflight or pending:  # 闲着的时候一个字都不打
            logger.info(
                "生成派发中（进程 %d）：正在生成 %d/%d 张，队列里还有 %d 个任务等着",
                os.getpid(), inflight, self._concurrency, pending,
            )

    # ------------------------------------------------------------ dispatch

    def _dispatch_loop(self) -> None:
        while not self._stop.is_set():
            interval = POLL_INTERVAL
            try:
                if not self._ensure_leadership():
                    # 待命：慢一点转，只为了定期看看领导者还在不在
                    interval = STANDBY_RETRY_SECONDS
                else:
                    self._heartbeat()
                    with self._lock:
                        free_slots = self._concurrency - self._inflight
                    if free_slots > 0:
                        for job_id in self._claim_jobs(free_slots):
                            with self._lock:
                                self._inflight += 1
                            self._executor.submit(self._run_job, job_id)
            except Exception:
                # 出错时把节奏放慢：数据库重启/磁盘满的时候，每秒一条异常栈
                # 乘以 4 个进程，几分钟就能把日志刷到没法看，反而盖住真正的原因。
                interval = STANDBY_RETRY_SECONDS
                logger.exception("任务分发循环出现异常（继续运行）")
            self._stop.wait(interval)

    def _claim_jobs(self, limit: int) -> List[str]:
        db = SessionLocal()
        try:
            now = datetime.utcnow()
            candidate_ids = [
                row[0]
                for row in db.query(GenerationJob.id)
                .filter(GenerationJob.status == "pending")
                .filter(
                    (GenerationJob.next_attempt_at.is_(None))
                    | (GenerationJob.next_attempt_at <= now)
                )
                .order_by(GenerationJob.created_at.asc())
                .limit(limit)
                .all()
            ]
            claimed: List[str] = []
            # 逐条原子更新（status 条件写在 WHERE 里），多进程部署也不会重复领取
            for job_id in candidate_ids:
                result = db.execute(
                    sa_update(GenerationJob)
                    .where(GenerationJob.id == job_id, GenerationJob.status == "pending")
                    .values(status="processing", started_at=now)
                )
                if result.rowcount == 1:
                    claimed.append(job_id)
            db.commit()
            return claimed
        finally:
            db.close()

    # ------------------------------------------------------------ execution

    def _run_job(self, job_id: str) -> None:
        try:
            self._process(job_id)
        except Exception:
            logger.exception("任务 %s 出现未捕获异常", job_id)
        finally:
            with self._lock:
                self._inflight -= 1

    def _process(self, job_id: str) -> None:
        db = SessionLocal()
        try:
            job = db.get(GenerationJob, job_id)
            if job is None or job.status != "processing":
                return
            # 清理上一次尝试遗留的结果图，避免重试/重跑后 images 累积重复
            for old in list(job.images or []):
                storage.delete_file(old.path)
                storage.delete_file(old.thumb_path)
                db.delete(old)
            db.commit()
            # ══════════════════════════════════════════════════════════════
            #  这一行决定「这次生图花谁的钱」——全系统只有这一处做这个决定。
            # ══════════════════════════════════════════════════════════════
            # 不能写成 settings_service.get_all(db)：那样拿到的永远是管理员配的
            # 全局 Key，成员生的图照样出得来、他自己的余额一分不扣，账全记在管理员
            # 头上，而且日志里没有任何痕迹能看出这次是谁花的。表面上一切正常，
            # 是这一期最容易漏、漏了之后最难发现的一步。
            #
            # 下面这一份 settings 会被本次任务的三条线共用（挑风格参考、AI 写提示词、
            # 生图），所以只取一次：同一个任务的三次外部调用必须落在同一个人的账上。
            # 注意下面合成提示词那一段中途会 db.close() 再开一个新 session，
            # 但 settings 是纯 Python 字典、不依附于 session，跨 session 继续有效，
            # 不需要（也不该）重新取一次。
            try:
                settings = settings_service.resolve_for_user(db, job.user_id)
            except settings_service.GatewayNotReady as exc:
                # 「这个人没开通生图额度」不是网络抖动，重试多少次都是同一句话，
                # 只会让他多等两轮退避才看到结果。所以这里传一个只含 max_attempts=1
                # 的字典，让 _handle_failure 直接把任务判死、不再回队列。
                # （_handle_failure 只从 settings 里读 max_attempts 这一个键，
                #   传完整设置反而要求这个分支再去查一次库。）
                # error 写异常原文：那句话本来就是写给非技术用户看的人话。
                self._handle_failure(db, job_id, str(exc), {"max_attempts": 1})
                return
            params = job.params or {}
            n = int(params.get("n") or 1)
            size = str(params.get("size") or settings.get("default_size") or "1024x1024")
            quality = params.get("quality")

            # 提示词库只作参考：先让带视觉的文本模型看商品图，结合补充要求
            # 现场写出贴合这件商品的提示词。失败则沿用原提示词，不阻断出图。
            want_transparent = str(settings.get("image_background") or "auto").lower() == "transparent"
            prompt_to_send = job.prompt_final
            # 补图任务的 prompt_final 已经是上一批实际发出的最终提示词：
            # 再合成一次既多花一次文本模型的钱，也会让补出来的图和上一批不是一套设定
            reuse_prompt = bool(params.get("reuse_prompt"))
            # 按分类生成时，先从该分类的语料里抽一批候选，交给文本模型自己挑最贴合的。
            # 种子固定为「输入图 + 分类 + 补充要求」的哈希：同样的输入拿到同样的候选，
            # 批量出图时风格才可复现。
            references = []
            category_slug = params.get("category_slug")
            if category_slug and not reuse_prompt and settings.get("provider") != "mock":
                # 两段式：先让模型通览该分类的全部标题挑出风格最搭的几条，
                # 再取这几条的**全文**交给合成环节。只发标题是为了省 token——
                # 一个分类的正文全给要几十万 token，而挑选阶段只需要知道是什么风格。
                digest = inspiration.category_digest(db, category_slug)
                try:
                    picked = prompt_studio.pick_references(
                        settings, digest, list(job.input_paths or [])
                    )
                    references = inspiration.fetch_references(db, picked)
                    logger.info("任务 %s 从 %d 条「%s」里挑出 %d 条参考",
                                job_id, len(digest), category_slug, len(references))
                except Exception as exc:
                    # 挑选失败不该挡住出图：退回「只按补充要求写」，画质会差一点但仍能出
                    logger.warning("任务 %s 挑选风格参考失败，改为只按补充要求出图：%s",
                                   job_id, exc)

            if not reuse_prompt and settings.get("prompt_synthesis") and settings.get("provider") != "mock":
                base_prompt = job.prompt_final
                job_input_paths = list(job.input_paths or [])
                # 合成要调用视觉模型、最长可达 180 秒；先把连接还回池里，
                # 否则并发几个任务就会把连接池占满，网页请求全部卡住
                db.commit()
                db.close()
                try:
                    prompt_to_send = prompt_studio.synthesize_prompt(
                        settings, base_prompt, job_input_paths, size,
                        transparent=want_transparent, references=references,
                    )
                    logger.info("任务 %s 已按实际商品重写提示词", job_id)
                except Exception as exc:
                    logger.warning("任务 %s 提示词合成失败，沿用原提示词：%s", job_id, exc)
                    prompt_to_send = base_prompt
                db = SessionLocal()
                job = db.get(GenerationJob, job_id)
                if job is None or job.status != "processing":
                    return  # 期间被删除或被别的进程接管
            # 无论是否走了 AI 合成，都把目标画幅显式写进提示词——网关忽略 size 参数，
            # 而提示词里的构图措辞对出图比例影响最大（实测可压过输入图比例）
            prompt_to_send = prompt_studio.enforce_aspect(prompt_to_send, size)
            # 要透明底时，光发 background 参数不够——模板里的「纯白背景」措辞
            # 会让模型照样画一块实白。合成路径已在合成阶段就带上了这个要求，
            # 这里是兜底：覆盖关掉合成、合成失败回退、以及补图沿用旧提示词三种情况
            if want_transparent:
                prompt_to_send = prompt_studio.enforce_transparent_background(prompt_to_send)
            job.prompt_sent = prompt_to_send
            db.commit()

            try:
                images = provider.generate_images(
                    settings, prompt_to_send, list(job.input_paths or []), n, size, quality
                )
                for idx, data in enumerate(images):
                    rel, thumb_rel, width, height, fmt = storage.save_output(job.id, idx, data)
                    db.add(
                        GeneratedImage(
                            job_id=job.id, path=rel, thumb_path=thumb_rel,
                            width=width, height=height, format=fmt,
                        )
                    )
                job.status = "succeeded"
                job.error = ""
                job.finished_at = datetime.utcnow()
                db.commit()
                logger.info("任务 %s 完成，生成 %d 张图", job.id, len(images))
            except provider.ProviderError as e:
                self._handle_failure(db, job_id, str(e), settings)
            except Exception as e:
                logger.exception("任务 %s 执行失败", job.id)
                self._handle_failure(db, job_id, "系统内部错误：%s" % str(e)[:200], settings)

            # 过期会话缓存：本函数开头为清理旧图曾把 images 关系加载为空并缓存，
            # 新图是按 FK 直接 add 的、未同步到该缓存集合，这里强制重新从库读取。
            db.expire_all()
            job = db.get(GenerationJob, job_id)
            if job is None:
                return

            if job.status in ("succeeded", "failed") and job.callback_url:
                self._fire_webhook(db, job, settings)
        finally:
            db.close()

    def _handle_failure(self, db, job_id: str, error: str, settings: Dict[str, Any]) -> None:
        # 先 rollback：进入这里往往是因为上一步 commit 失败，session 处于待回滚状态
        db.rollback()
        job = db.get(GenerationJob, job_id)
        if job is None:
            return
        job.attempts = (job.attempts or 0) + 1
        max_attempts = max(1, int(settings.get("max_attempts") or 1))
        if job.attempts < max_attempts:
            job.status = "pending"  # 回队列稍后重试
            job.error = error
            job.next_attempt_at = datetime.utcnow() + timedelta(seconds=RETRY_BACKOFF_SECONDS)
            logger.warning("任务 %s 失败将重试（第 %d/%d 次）：%s", job.id, job.attempts, max_attempts, error)
        else:
            job.status = "failed"
            job.error = error
            job.finished_at = datetime.utcnow()
            logger.warning("任务 %s 最终失败：%s", job.id, error)
        db.commit()

    def _fire_webhook(self, db, job: GenerationJob, settings: Dict[str, Any]) -> None:
        public_base = str(settings.get("public_base_url") or "")
        # 回调里的图片链接必须带签名，否则对接方收到回调、去下载图片时会全部 403
        # （/files 现在默认只认凭证）。而回调是「机器发给机器」的一次性通知，
        # 出了问题对方只会看到「你们的图挂了」，最难归因，所以这里不能漏。
        #
        # job.api_key_id 为空时**必须**传 sign=None：storage.to_url 收到
        # (None, ttl) 会直接抛 StorageError。这是双方约好的刻意设计——宁可当场
        # 报错，也不能悄悄发一条谁都能打开的无签名链接出去。
        # 正常情况下走不到这个分支：callback_url 只有对外 API 才能设置，网页端
        # 建的任务没有回调地址。留着是兜底（比如历史数据、或将来网页端也支持回调）。
        sign = None
        if job.api_key_id:
            sign = (job.api_key_id, _erp_link_ttl_hours(settings))
        # 复用查询接口的序列化，保证回调里的 images 结构与 GET 任务完全一致
        payload = job_to_dict(job, public_base, include_inputs=False, sign=sign)
        payload["event"] = "generation.finished"
        secret = None
        if job.api_key_id:
            key = db.get(ApiKey, job.api_key_id)
            if key is not None:
                secret = key.webhook_secret
        job.webhook_status = "sending"
        db.commit()
        allow_internal = bool(settings.get("allow_internal_targets"))
        webhook.deliver_async(job.id, job.callback_url, secret, payload, allow_internal)


worker = GenerationWorker()
