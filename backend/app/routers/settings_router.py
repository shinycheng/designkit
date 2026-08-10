from typing import Any, Dict

from fastapi import APIRouter, Depends, HTTPException
from sqlalchemy.orm import Session

from ..backfill import get_backfill_health
from ..config import TOKEN_EXPIRE_DAYS
from ..deps import get_db, require_admin
from ..models import User
from ..services import (
    file_access, inspiration, jobs, prompt_studio, provider, settings_service, sizing,
)

router = APIRouter(prefix="/api/web/settings", tags=["网页-系统设置"])


def _settings_payload(db: Session) -> Dict[str, Any]:
    """设置接口的统一返回体：可改的设置项（密钥已打码）+ 几项只读的状态。

    GET 和 PUT 返回同一个形状，是因为前端保存成功后会拿 PUT 的返回值整个替换掉
    本地那份设置（settings.js）。两边形状不一样的话，管理员一保存，
    页面上的告警横幅和统计数字就会莫名其妙消失，重新打开页面又出现。

    只读部分（都不在 RUNTIME_DEFAULTS 里，所以就算前端原样回传，
    settings_service.set_many 也会自动忽略，不会被写进设置表）：

      backfill_ok / backfill_error
          历史数据归属回填的健康状态。False 时管理员界面要显示红色横幅，
          文案直接用 backfill_error（已经是中文人话、自带处置建议）。
          **注意 settings_service.get_all() 拿不到这两个键**——它只复制
          RUNTIME_DEFAULTS 里有的键，所以必须像这里一样显式去 backfill 模块取。

      files_unsigned_access
          {"total","blocked","last_at","last_path"}：本进程启动至今，
          有多少次取图请求没带任何凭证（total）、其中被拦下多少次（blocked）。
          这是判断「敢不敢把 files_signed_only 关掉 / 关掉之后还有没有人在用
          老链接」的唯一客观依据，别靠翻日志猜。进程重启归零是有意的，
          它本来就只是个观察窗口。last_at 是 Unix 秒（0 表示从没发生过），
          怎么显示交给前端。
    """
    payload = settings_service.masked(settings_service.get_all(db))
    payload.update(get_backfill_health(db))
    payload["files_unsigned_access"] = file_access.unsigned_access_stats()
    return payload


@router.get("")
def get_settings(_: User = Depends(require_admin), db: Session = Depends(get_db)):
    return _settings_payload(db)


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
                     "allow_internal_targets", "inspiration_auto_sync",
                     "files_signed_only"):
        if bool_key in updates and not isinstance(updates[bool_key], bool):
            raise HTTPException(status_code=422, detail="%s 必须是 true 或 false" % bool_key)
    if "provider" in updates and updates["provider"] not in ("mock", "openai"):
        raise HTTPException(status_code=422, detail="provider 只能是 mock 或 openai")
    # 对外访问地址：这一项此前是全部设置里唯一没有校验的，而结果图链接、
    # webhook 回调地址、给 ERP 的签名链接全都基于它拼出来。填错（比如漏了 http://、
    # 或者手滑填成 "192.168.1.10:8787"）不会当场报错，只会让 ERP 那边拿到一堆
    # 打不开的地址，两边都查不出原因。留空是允许的——那时链接退化成相对路径，
    # 网页端照常能用（storage.to_url 的行为），只有对外接口会受影响。
    if "public_base_url" in updates:
        base = str(updates["public_base_url"] or "").strip().rstrip("/")
        if base and not base.startswith(("http://", "https://")):
            raise HTTPException(
                status_code=422,
                detail="对外访问地址要以 http:// 或 https:// 开头，例如 http://192.168.1.10:8787",
            )
        updates["public_base_url"] = base
    if "gateway_mode" in updates and updates["gateway_mode"] not in ("shared", "per_user"):
        raise HTTPException(
            status_code=422, detail="计费方式只能是 shared（共用一把 Key）或 per_user（每人一把）"
        )
    # CORS 白名单。写成 * 表示不限制；多个域名用英文逗号隔开。
    # 逐条要求带协议头，是因为浏览器比对 Origin 时是连协议一起比的：
    # 填 "a.example.com" 永远匹配不上 "https://a.example.com"，
    # 而现象是「跨域调用全被挡了」，管理员看着这行字是对的，根本想不到差在协议头。
    if "allowed_origins" in updates:
        raw = str(updates["allowed_origins"] or "").strip()
        items = [item.strip().rstrip("/") for item in raw.split(",") if item.strip()]
        if not items or "*" in items:
            updates["allowed_origins"] = "*"
        else:
            for item in items:
                host = item.split("://", 1)[1] if "://" in item else ""
                if not item.startswith(("http://", "https://")) or not host or "/" in host:
                    raise HTTPException(
                        status_code=422,
                        detail="允许的来源要写成完整站点地址、不带路径，"
                               "例如 https://designkit.example.com；多个用英文逗号隔开，"
                               "不限制就填 *（改完这一项需要重启服务才生效）",
                    )
            updates["allowed_origins"] = ",".join(items)
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
        # 下限**必须**不小于登录令牌的有效期。取图凭证比登录短的话，
        # 会出现「人还登录着、页面照常能点，可图全裂了」——这种故障
        # 非技术用户完全无法归因，只会觉得整个系统坏了。
        "web_file_cookie_days": (TOKEN_EXPIRE_DAYS, 30, "网页取图凭证有效期（天）"),
        # 给 ERP 的图片直链有效期。上限 8760 小时 = 365 天：再长就等于永久有效，
        # 那这条链接一旦流出去就再也收不回来了（除非停用整把 Key）。
        "erp_file_link_ttl_hours": (1, 8760, "对外图片链接有效期（小时）"),
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
    return _settings_payload(db)


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
