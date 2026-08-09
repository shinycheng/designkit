"""后台生成 worker：数据库就是任务队列。

- 网页/对外 API 创建任务后立刻返回（异步），worker 轮询领取并执行；
- 任务状态落库，程序重启不丢任务；启动时把中断的 processing 任务重置回 pending；
- 失败自动重试（次数可配），最终失败会记录可读的错误原因并触发回调。
"""
import logging
import threading
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta
from typing import Any, Dict, List, Optional

from sqlalchemy import update as sa_update

from ..database import SessionLocal
from ..models import ApiKey, GeneratedImage, GenerationJob
from ..serializers import job_to_dict
from . import prompt_studio, provider, settings_service, storage, webhook

logger = logging.getLogger("designkit.worker")

POLL_INTERVAL = 1.0
RETRY_BACKOFF_SECONDS = 5
# 启动时重置卡住任务的年龄阈值：超过它仍是 processing 才认定为「上次中断」。
# 要大于生图的最坏合法耗时（降级逐张时预算为 request_timeout(≤900) × n(≤4) = 3600 秒），
# 否则多进程部署下会把还在正常执行的任务误判为中断、重新入队重复计费。
STUCK_RESET_SECONDS = 4500


class GenerationWorker:
    def __init__(self) -> None:
        self._stop = threading.Event()
        self._executor: Optional[ThreadPoolExecutor] = None
        self._dispatcher: Optional[threading.Thread] = None
        self._lock = threading.Lock()
        self._inflight = 0
        self._concurrency = 2

    # ------------------------------------------------------------ lifecycle

    def start(self) -> None:
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

            # 已完结但回调未送达/中断的任务，启动时补投一次
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

            self._concurrency = max(1, min(8, int(settings_service.get(db, "worker_concurrency") or 2)))
        finally:
            db.close()

        self._executor = ThreadPoolExecutor(
            max_workers=self._concurrency, thread_name_prefix="genworker"
        )
        self._dispatcher = threading.Thread(target=self._dispatch_loop, daemon=True)
        self._dispatcher.start()
        logger.info("生成 worker 已启动，并发数 %d", self._concurrency)

    def stop(self) -> None:
        self._stop.set()
        if self._executor is not None:
            # 取消尚未开始的排队任务；已在执行的生图任务让它自然跑完（下次启动会补投回调）
            try:
                self._executor.shutdown(wait=False, cancel_futures=True)
            except TypeError:  # 老版本 Python 无 cancel_futures 参数
                self._executor.shutdown(wait=False)

    # ------------------------------------------------------------ dispatch

    def _dispatch_loop(self) -> None:
        while not self._stop.wait(POLL_INTERVAL):
            try:
                with self._lock:
                    free_slots = self._concurrency - self._inflight
                if free_slots <= 0:
                    continue
                job_ids = self._claim_jobs(free_slots)
                for job_id in job_ids:
                    with self._lock:
                        self._inflight += 1
                    self._executor.submit(self._run_job, job_id)
            except Exception:
                logger.exception("任务分发循环出现异常（继续运行）")

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
            settings = settings_service.get_all(db)
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
                        transparent=want_transparent,
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
        # 复用查询接口的序列化，保证回调里的 images 结构与 GET 任务完全一致
        payload = job_to_dict(job, public_base, include_inputs=False)
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
