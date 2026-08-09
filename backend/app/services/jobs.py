"""创建生成任务的共用逻辑（网页端 / 对外 API 都走这里）。"""
from typing import Any, Dict, List, Optional

from fastapi import HTTPException
from sqlalchemy.orm import Session

from ..models import GenerationJob, PromptTemplate, Upload
from ..prompting import build_raw_prompt, render_template_prompt
from . import settings_service

MAX_INPUT_IMAGES = 4
MAX_N = 4
ALLOWED_QUALITIES = {"auto", "low", "medium", "high"}
# gpt-image-1 支持的尺寸；非法尺寸应同步 422 拒绝，而不是 202 受理后异步失败
ALLOWED_SIZES = {"1024x1024", "1536x1024", "1024x1536", "auto"}


def create_job(
    db: Session,
    *,
    source: str,
    user_id: Optional[int] = None,
    api_key_id: Optional[int] = None,
    template_id: Optional[int] = None,
    prompt: Optional[str] = None,
    variables: Optional[Dict[str, Any]] = None,
    extra_instructions: Optional[str] = None,
    input_paths: Optional[List[str]] = None,
    n: Optional[int] = None,
    size: Optional[str] = None,
    quality: Optional[str] = None,
    callback_url: Optional[str] = None,
    external_ref: Optional[str] = None,
) -> GenerationJob:
    input_paths = list(input_paths or [])
    if len(input_paths) > MAX_INPUT_IMAGES:
        raise HTTPException(status_code=422, detail="最多支持 %d 张商品图" % MAX_INPUT_IMAGES)

    template: Optional[PromptTemplate] = None
    if template_id is not None:
        template = db.get(PromptTemplate, template_id)
        if template is None or not template.is_enabled:
            raise HTTPException(status_code=404, detail="提示词模板不存在或已停用")

    if template is not None:
        if template.requires_input_image and not input_paths:
            raise HTTPException(status_code=422, detail="该模板需要先上传商品图")
        final_prompt = render_template_prompt(template, variables, extra_instructions)
    else:
        final_prompt = build_raw_prompt(prompt or "", extra_instructions)
    if not final_prompt:
        raise HTTPException(status_code=422, detail="请选择提示词模板或填写提示词")

    defaults = (template.default_params if template is not None else None) or {}
    settings = settings_service.get_all(db)

    n_val = n if n is not None else defaults.get("n") or settings.get("default_n") or 1
    try:
        n_val = int(n_val)
    except (TypeError, ValueError):
        n_val = 1
    n_val = max(1, min(MAX_N, n_val))

    size_val = str(size or defaults.get("size") or settings.get("default_size") or "1024x1024")
    if size_val not in ALLOWED_SIZES:
        raise HTTPException(
            status_code=422,
            detail="尺寸只支持 1024x1024 / 1536x1024 / 1024x1536 / auto",
        )
    quality_val = quality or defaults.get("quality")
    if quality_val and quality_val not in ALLOWED_QUALITIES:
        raise HTTPException(
            status_code=422, detail="质量参数只支持 auto / low / medium / high"
        )

    if callback_url:
        cb = str(callback_url).strip()
        if not (cb.startswith("http://") or cb.startswith("https://")):
            raise HTTPException(status_code=422, detail="回调地址必须是 http(s) 链接")
        if len(cb) > 512:  # 列宽 512，PostgreSQL 会直接拒绝超长值
            raise HTTPException(status_code=422, detail="回调地址过长（最多 512 个字符）")
        callback_url = cb

    job = GenerationJob(
        source=source,
        user_id=user_id,
        api_key_id=api_key_id,
        template_id=template.id if template is not None else None,
        template_name=template.name if template is not None else "",
        prompt_final=final_prompt,
        params={"n": n_val, "size": size_val, "quality": quality_val or "auto"},
        input_paths=input_paths,
        status="pending",
        callback_url=callback_url,
        external_ref=(external_ref or None),
    )
    db.add(job)
    db.commit()
    db.refresh(job)
    return job


def resolve_upload_paths(
    db: Session,
    upload_ids: List[int],
    *,
    user_id: Optional[int] = None,
    api_key_id: Optional[int] = None,
) -> List[str]:
    """把上传记录 id 换成文件路径；校验归属，避免拿别人的图。"""
    paths: List[str] = []
    for uid in upload_ids:
        upload = db.get(Upload, int(uid))
        if upload is None:
            raise HTTPException(status_code=404, detail="上传的图片不存在（id=%s）" % uid)
        # 越权统一返回 404（与任务查询一致，不泄露资源是否存在）
        if api_key_id is not None and upload.api_key_id != api_key_id:
            raise HTTPException(status_code=404, detail="上传的图片不存在（id=%s）" % uid)
        if api_key_id is None and user_id is not None and upload.user_id != user_id:
            raise HTTPException(status_code=404, detail="上传的图片不存在（id=%s）" % uid)
        paths.append(upload.path)
    return paths
