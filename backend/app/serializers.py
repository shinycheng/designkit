"""把数据库对象转成接口返回的 JSON 结构（网页端和对外 API 共用）。"""
from typing import Any, Dict, Optional

from .models import ApiKey, GeneratedImage, GenerationJob, PromptTemplate, Upload
from .services import storage


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
