from typing import Any, Dict

from fastapi import APIRouter, Depends, HTTPException
from sqlalchemy.orm import Session

from ..deps import get_db, require_admin
from ..models import User
from ..services import inspiration, jobs, prompt_studio, provider, settings_service, sizing

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
    # 文本模型名留空会让「AI 写提示词」必然失败（每次都白等一轮再回退），提前拦住
    if "text_model" in updates and not str(updates["text_model"] or "").strip():
        raise HTTPException(status_code=422, detail="文本模型不能为空，例如 gpt-5.6-sol")
    # 布尔开关必须是真布尔：传字符串 "false" 会被 Python 当成真值，开关名存实亡
    for bool_key in ("prompt_synthesis", "normalize_input_ratio",
                     "allow_internal_targets", "inspiration_auto_sync"):
        if bool_key in updates and not isinstance(updates[bool_key], bool):
            raise HTTPException(status_code=422, detail="%s 必须是 true 或 false" % bool_key)
    if "provider" in updates and updates["provider"] not in ("mock", "openai"):
        raise HTTPException(status_code=422, detail="provider 只能是 mock 或 openai")
    if "inspiration_proxy" in updates:
        proxy = str(updates["inspiration_proxy"] or "").strip()
        if proxy and not proxy.startswith(("http://", "https://", "socks5://", "socks5h://")):
            raise HTTPException(
                status_code=422,
                detail="代理地址要以 http:// 或 socks5:// 开头，例如 http://127.0.0.1:7890",
            )
        updates["inspiration_proxy"] = proxy
    if "image_background" in updates and updates["image_background"] not in ("auto", "transparent"):
        raise HTTPException(status_code=422, detail="出图底色只能是 auto 或 transparent")
    if "default_size" in updates:
        updates["default_size"] = sizing.normalize_size(updates["default_size"])
        size_error = sizing.validate_size(updates["default_size"])
        if size_error:
            raise HTTPException(status_code=422, detail="默认尺寸不可用：%s" % size_error)
    # 上限不只是防呆：max_attempts 和 n 直接决定一次任务最多向生图接口发多少次
    # 请求，填错一个数字就可能变成成倍的费用
    int_ranges = {
        "worker_concurrency": (1, 8, "并发生成数"),
        "max_attempts": (1, 5, "失败重试次数"),
        "request_timeout": (30, 900, "生图超时（秒）"),
        "default_n": (1, 4, "默认生成张数"),
        "inspiration_sync_interval_hours": (1, 168, "灵感库自动同步间隔（小时）"),
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


@router.post("/test_text_model")
def test_text_model(_: User = Depends(require_admin), db: Session = Depends(get_db)):
    """测试写提示词用的文本模型（只发一句话，成本可忽略）。"""
    settings = settings_service.get_all(db)
    if settings.get("provider") == "mock":
        return {"ok": True, "message": "当前为模拟生图模式，无需文本模型"}
    try:
        return {"ok": True, "message": prompt_studio.test_text_model(settings)}
    except provider.ProviderError as e:
        return {"ok": False, "message": str(e)}


@router.post("/test_sync_proxy")
def test_sync_proxy(_: User = Depends(require_admin), db: Session = Depends(get_db)):
    """测试能不能拉到上游提示词库（走当前配置的代理）。

    只下载一个很小的 manifest.json，不写缓存、不入库，几秒就有结果——
    比让用户点「同步上游」等两分钟才发现代理不通要好得多。
    """
    proxy = str(settings_service.get(db, "inspiration_proxy") or "").strip()
    try:
        info = inspiration.probe_upstream(proxy)
    except Exception as exc:
        return {"ok": False, "message": str(exc)[:300]}
    return {"ok": True, "message": info}
