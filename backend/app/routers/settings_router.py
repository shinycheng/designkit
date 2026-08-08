from typing import Any, Dict

from fastapi import APIRouter, Depends, HTTPException
from sqlalchemy.orm import Session

from ..deps import get_db, require_admin
from ..models import User
from ..services import jobs, provider, settings_service

router = APIRouter(prefix="/api/web/settings", tags=["网页-系统设置"])


@router.get("")
def get_settings(_: User = Depends(require_admin), db: Session = Depends(get_db)):
    return settings_service.masked(settings_service.get_all(db))


@router.put("")
def update_settings(
    updates: Dict[str, Any], _: User = Depends(require_admin), db: Session = Depends(get_db)
):
    updates = dict(updates or {})
    # 前端把打码后的 key 原样传回来表示「没改」，这里忽略掉（与当前掩码精确比对 + 星号前缀兜底）
    masked_current = settings_service.masked(settings_service.get_all(db))
    for secret_key in settings_service.SECRET_KEYS:
        value = str(updates.get(secret_key) or "")
        if value and (value == masked_current.get(secret_key) or set(value[:8]) == {"*"}):
            updates.pop(secret_key, None)
    if "provider" in updates and updates["provider"] not in ("mock", "openai"):
        raise HTTPException(status_code=422, detail="provider 只能是 mock 或 openai")
    if "default_size" in updates and updates["default_size"] not in jobs.ALLOWED_SIZES:
        raise HTTPException(
            status_code=422, detail="默认尺寸只支持 1024x1024 / 1536x1024 / 1024x1536 / auto"
        )
    # 上限不只是防呆：max_attempts 和 n 直接决定一次任务最多向生图接口发多少次
    # 请求，填错一个数字就可能变成成倍的费用
    int_ranges = {
        "worker_concurrency": (1, 8, "并发生成数"),
        "max_attempts": (1, 5, "失败重试次数"),
        "request_timeout": (30, 900, "生图超时（秒）"),
        "default_n": (1, 4, "默认生成张数"),
    }
    for int_key, (low, high, label) in int_ranges.items():
        if int_key not in updates:
            continue
        try:
            value = int(updates[int_key])
        except (TypeError, ValueError):
            raise HTTPException(status_code=422, detail="%s 必须是数字" % label)
        if not low <= value <= high:
            raise HTTPException(
                status_code=422, detail="%s 只能填 %d 到 %d 之间" % (label, low, high)
            )
        updates[int_key] = value
    settings_service.set_many(db, updates)
    return settings_service.masked(settings_service.get_all(db))


@router.post("/test_provider")
def test_provider(_: User = Depends(require_admin), db: Session = Depends(get_db)):
    """用当前已保存的设置测试生图接口连通性（不产生生图费用）。"""
    try:
        message = provider.test_connection(settings_service.get_all(db))
        return {"ok": True, "message": message}
    except provider.ProviderError as e:
        return {"ok": False, "message": str(e)}
