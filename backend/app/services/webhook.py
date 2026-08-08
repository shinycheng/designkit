"""任务完成后向 ERP 等对接方回调通知。

签名方式：X-DesignKit-Signature: sha256=<HMAC_SHA256(secret, timestamp + "." + body)>
对接方用创建 API Key 时下发的 webhook_secret 做同样计算即可校验。
"""
import json
import logging
import threading
import time
from typing import Any, Dict, Optional

import httpx

from ..database import SessionLocal
from ..models import GenerationJob
from ..security import sign_webhook
from . import net_guard

logger = logging.getLogger("designkit.webhook")

RETRY_DELAYS = (0, 8, 30)  # 立即、8秒后、30秒后，共 3 次


def deliver_async(
    job_id: str, url: str, secret: Optional[str], payload: Dict[str, Any],
    allow_internal: bool = True,
) -> None:
    thread = threading.Thread(
        target=_deliver, args=(job_id, url, secret, payload, allow_internal), daemon=True
    )
    thread.start()


def _deliver(
    job_id: str, url: str, secret: Optional[str], payload: Dict[str, Any],
    allow_internal: bool = True,
) -> None:
    # 回调地址同样做 SSRF 校验：始终拦截云元数据等危险地址；内网地址按设置放行
    try:
        net_guard.assert_safe_url(url, allow_internal=allow_internal)
    except net_guard.UnsafeUrlError as e:
        logger.warning("webhook %s 回调地址被拒绝（%s）: %s", job_id, e, url[:120])
        _save_status(job_id, "rejected_unsafe_url")
        return

    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    status = "failed"
    for attempt, delay in enumerate(RETRY_DELAYS, start=1):
        if delay:
            time.sleep(delay)
        timestamp = str(int(time.time()))
        headers = {"Content-Type": "application/json", "X-DesignKit-Timestamp": timestamp}
        if secret:
            signed = sign_webhook(secret, (timestamp + ".").encode("utf-8") + body)
            headers["X-DesignKit-Signature"] = "sha256=" + signed
        try:
            # 不跟随重定向，避免公网地址跳转到内网
            resp = httpx.post(url, content=body, headers=headers, timeout=15, follow_redirects=False)
            if 200 <= resp.status_code < 300:
                status = "delivered"
                break
            status = "failed_http_%d" % resp.status_code
            logger.warning("webhook %s 第%d次回调返回 HTTP %d", job_id, attempt, resp.status_code)
        except httpx.HTTPError as e:
            status = "failed_unreachable"
            logger.warning("webhook %s 第%d次回调失败: %s", job_id, attempt, e)

    _save_status(job_id, status)


def _save_status(job_id: str, status: str) -> None:
    db = SessionLocal()
    try:
        job = db.get(GenerationJob, job_id)
        if job is not None:
            job.webhook_status = status
            db.commit()
    finally:
        db.close()
