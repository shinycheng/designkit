"""把数据库对象转成接口返回的 JSON 结构（网页端和对外 API 共用）。"""
import re
from typing import Any, Dict, Optional, Tuple

from .models import ApiKey, GeneratedImage, GenerationJob, PromptTemplate, Upload
from .services import storage

# 提示词是否明确让模型「参照上传的那张图」。
# 灵感库里约九成条目是某个具体商品的完整场景描述（例如写死了茶罐、品牌名），
# 直接套用到别的商品上，模型会画它自己描述的商品而不是用户的——必须提前警示。
_INPUT_REF_PATTERN = re.compile(
    r"uploaded image|uploaded photo|reference image|provided image|attached image"
    r"|input image|from the image|the uploaded|user[-\s]provided"
    r"|上传的图|参考图|上传图",
    re.IGNORECASE,
)


def references_input_image(prompt: Optional[str]) -> bool:
    return bool(_INPUT_REF_PATTERN.search(prompt or ""))


# 实测结论（2026-08-09，gpt-image-2）：约束必须放在**开头**才压得住。
# 同一条写死了乌龙茶罐的提示词配吹风机原图：
#   约束追加在末尾 → 仍然输出茶叶详情页（失败）
#   约束前置 + 声明下文只作风格参考 → 输出吹风机，并借用了原提示词的色调（成功）
_ADAPT_PREFIX = (
    "Create a product photo of THE PRODUCT FROM THE UPLOADED IMAGE. "
    "The uploaded product is the ONLY subject; keep its real shape, color, material "
    "and branding exactly as they are. IGNORE any other product described below — "
    "the text below only defines the visual style, mood, lighting and color palette.\n\n"
    "STYLE REFERENCE (do not copy its products):\n"
)


def adapt_prompt_to_uploaded_product(prompt: Optional[str]) -> str:
    """把「自带商品描述」的提示词改造成能套用到用户上传商品的形式。

    已经明确引用上传图的提示词原样返回，不画蛇添足。
    """
    text = (prompt or "").strip()
    if not text or references_input_image(text):
        return text
    return _ADAPT_PREFIX + text


def _iso(dt) -> Optional[str]:
    return dt.isoformat() + "Z" if dt else None


# 下面四个函数都带一个 `sign` 关键字参数，作用是把它原样透传给 storage.to_url：
#   传 None（默认）—— 出来的是普通图片链接，网页端靠登录凭证取图，行为和以前一模一样；
#   传 (api_key_id, ttl_hours) —— 出来的链接带签名参数，给对外 ERP 接口用。
# 之所以做成**带默认值的关键字参数**，是为了让网页端那一堆现有调用点
# （generations.py / uploads.py / templates.py / inspiration.py）一行都不用改。
# 该不该签名由调用方显式决定，不做任何自动推断——原因见 storage.to_url 的说明。


def image_to_dict(
    img: GeneratedImage,
    public_base: str,
    *,
    sign: Optional[Tuple[int, int]] = None,
) -> Dict[str, Any]:
    return {
        "id": img.id,
        "url": storage.to_url(img.path, public_base, sign=sign),
        "thumbnail_url": (
            storage.to_url(img.thumb_path, public_base, sign=sign)
            or storage.to_url(img.path, public_base, sign=sign)
        ),
        "width": img.width,
        "height": img.height,
        "format": img.format,
    }


def job_to_dict(
    job: GenerationJob,
    public_base: str,
    include_inputs: bool = True,
    *,
    sign: Optional[Tuple[int, int]] = None,
) -> Dict[str, Any]:
    data: Dict[str, Any] = {
        "job_id": job.id,
        "source": job.source,
        "status": job.status,
        "error": job.error or "",
        "template_id": job.template_id,
        "template_name": job.template_name or "",
        "prompt": job.prompt_final,
        "prompt_sent": job.prompt_sent or job.prompt_final,
        "params": job.params or {},
        "external_ref": job.external_ref,
        "callback_url": job.callback_url,
        "webhook_status": job.webhook_status or "",
        "attempts": job.attempts or 0,
        "created_at": _iso(job.created_at),
        "started_at": _iso(job.started_at),
        "finished_at": _iso(job.finished_at),
        "images": [image_to_dict(img, public_base, sign=sign) for img in (job.images or [])],
    }
    if include_inputs:
        data["input_images"] = [
            storage.to_url(p, public_base, sign=sign) for p in (job.input_paths or [])
        ]
    return data


def template_to_dict(
    t: PromptTemplate,
    public_base: str,
    *,
    sign: Optional[Tuple[int, int]] = None,
) -> Dict[str, Any]:
    return {
        "id": t.id,
        "name": t.name,
        "category_id": t.category_id,
        "category_name": t.category.name if t.category else None,
        "description": t.description or "",
        "prompt_template": t.prompt_template,
        "variables": t.variables or [],
        "default_params": t.default_params or {},
        "requires_input_image": bool(t.requires_input_image),
        # 提示词是否会参照用户上传的商品图（false 表示它自带商品描述，套用会画错商品）
        "references_input_image": references_input_image(t.prompt_template),
        "thumbnail_url": storage.to_url(t.thumbnail_path, public_base, sign=sign),
        "is_enabled": bool(t.is_enabled),
        "sort": t.sort or 0,
        "updated_at": _iso(t.updated_at),
    }


def upload_to_dict(
    u: Upload,
    public_base: str,
    *,
    sign: Optional[Tuple[int, int]] = None,
) -> Dict[str, Any]:
    return {
        "id": u.id,
        "url": storage.to_url(u.path, public_base, sign=sign),
        "original_name": u.original_name or "",
        "width": u.width,
        "height": u.height,
        "size_bytes": u.size_bytes,
        "created_at": _iso(u.created_at),
    }


def apikey_to_dict(k: ApiKey, *, owner: Optional[str] = None) -> Dict[str, Any]:
    """把一把对外 API Key 转成接口返回结构。

    owner 是**属主的显示名**，由调用方查好后传进来（routers/apikeys.py 里
    一次批量查出，避免每行一次查询）。这里不自己查库有两个原因：
    序列化函数一律不碰 Session（其余四个函数也都不碰），以及 ApiKey 上
    没有定义指向 User 的 relationship（models.py 里只有一个外键列），
    真要用 k.user 得先改模型，那不是这一步该动的东西。

    **只回名字、不回 user_id**：数字 id 对界面没有任何用处，却能让人把
    「几号用户」和别处出现的 id 对上号，属于白送的信息。归属未知时是空串
    ——存量 Key 在 P1-3 回填之前、以及属主被删掉之后（外键是 SET NULL，
    删人不删 Key，因为 ERP 那头还在拿它调接口）都会是这种情况。
    """
    return {
        "id": k.id,
        "name": k.name,
        "key_prefix": k.key_prefix,
        "is_active": bool(k.is_active),
        "monthly_quota": k.monthly_quota,
        "used_total": k.used_total or 0,
        "last_used_at": _iso(k.last_used_at),
        "created_at": _iso(k.created_at),
        # 这把 Key 归谁。空串 = 无主（前端显示成「—」即可）
        "owner": owner or "",
    }
