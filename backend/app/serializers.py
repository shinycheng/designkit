"""把数据库对象转成接口返回的 JSON 结构（网页端和对外 API 共用）。"""
import re
from typing import Any, Dict, Optional

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


def image_to_dict(img: GeneratedImage, public_base: str) -> Dict[str, Any]:
    return {
        "id": img.id,
        "url": storage.to_url(img.path, public_base),
        "thumbnail_url": storage.to_url(img.thumb_path, public_base) or storage.to_url(img.path, public_base),
        "width": img.width,
        "height": img.height,
        "format": img.format,
    }


def job_to_dict(job: GenerationJob, public_base: str, include_inputs: bool = True) -> Dict[str, Any]:
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
        "images": [image_to_dict(img, public_base) for img in (job.images or [])],
    }
    if include_inputs:
        data["input_images"] = [
            storage.to_url(p, public_base) for p in (job.input_paths or [])
        ]
    return data


def template_to_dict(t: PromptTemplate, public_base: str) -> Dict[str, Any]:
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
        "thumbnail_url": storage.to_url(t.thumbnail_path, public_base),
        "is_enabled": bool(t.is_enabled),
        "sort": t.sort or 0,
        "updated_at": _iso(t.updated_at),
    }


def upload_to_dict(u: Upload, public_base: str) -> Dict[str, Any]:
    return {
        "id": u.id,
        "url": storage.to_url(u.path, public_base),
        "original_name": u.original_name or "",
        "width": u.width,
        "height": u.height,
        "size_bytes": u.size_bytes,
        "created_at": _iso(u.created_at),
    }


def apikey_to_dict(k: ApiKey) -> Dict[str, Any]:
    return {
        "id": k.id,
        "name": k.name,
        "key_prefix": k.key_prefix,
        "is_active": bool(k.is_active),
        "monthly_quota": k.monthly_quota,
        "used_total": k.used_total or 0,
        "last_used_at": _iso(k.last_used_at),
        "created_at": _iso(k.created_at),
    }
