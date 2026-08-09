"""对外开放 API（供 ERP 等第三方平台对接）。

鉴权：请求头 X-API-Key: dk_xxx（在网页「API 对接」页创建）。
任务为异步：提交后立即返回 job_id，可轮询查询，也可留 callback_url 等回调。
详细对接文档见项目 docs/erp-api.md，在线调试见 /docs。
"""
import base64
import binascii
from datetime import datetime
from pathlib import Path
from typing import Any, Dict, List, Optional

import httpx
from fastapi import APIRouter, Depends, File, HTTPException, UploadFile
from pydantic import BaseModel, Field
from sqlalchemy import update as sa_update
from sqlalchemy.orm import Session

from ..config import ALLOWED_IMAGE_EXTS, MAX_UPLOAD_MB
from ..deps import get_api_client, get_db
from ..models import ApiKey, GenerationJob, PromptTemplate, Upload
from ..serializers import job_to_dict, template_to_dict, upload_to_dict
from ..services import inspiration, net_guard, settings_service, storage
from ..services.jobs import MAX_INPUT_IMAGES, create_job, resolve_upload_paths

MAX_UPLOAD_BYTES = MAX_UPLOAD_MB * 1024 * 1024

router = APIRouter(prefix="/api/v1", tags=["对外API-ERP对接"])


class V1GenerationBody(BaseModel):
    category_slug: Optional[str] = Field(
        default=None,
        description="提示词库分类（推荐）：平台会看商品图、从该分类里挑风格参考并现场写提示词。"
                    "可选值见 GET /api/v1/categories",
    )
    template_id: Optional[int] = Field(default=None, description="提示词模板 id（三者选一）")
    prompt: Optional[str] = Field(default=None, description="自定义提示词（三者选一）")
    variables: Dict[str, Any] = Field(default={}, description="模板变量取值，如 {\"scene\": \"现代客厅\"}")
    extra_instructions: Optional[str] = Field(default=None, description="追加的补充要求")
    upload_ids: List[int] = Field(default=[], description="先调 /api/v1/uploads 上传得到的图片 id")
    image_urls: List[str] = Field(default=[], description="商品图的公网 URL，由本服务代为下载")
    images_base64: List[str] = Field(default=[], description="商品图 base64（不带 data: 前缀）")
    n: Optional[int] = Field(default=None, ge=1, le=4, description="生成张数 1-4")
    size: Optional[str] = Field(default=None, description="形如 1024x1360 的像素串或 auto；非固定枚举，需满足：宽高为 16 的倍数、最短边≥512、最长边≤2048、长短比≤3:1")
    quality: Optional[str] = Field(default=None, description="auto / low / medium / high")
    callback_url: Optional[str] = Field(default=None, description="任务完成后的回调地址（http/https）")
    external_ref: Optional[str] = Field(default=None, max_length=128, description="对接方自己的单号，回调时原样带回")


def _consume_quota(db: Session, key: ApiKey) -> None:
    """原子扣减本月额度：一条带条件的 UPDATE，避免并发下超额与丢计数。"""
    month_key = datetime.utcnow().strftime("%Y%m")
    # 跨月重置：当记录的月份不是本月时，把本月计数归零
    db.execute(
        sa_update(ApiKey)
        .where(ApiKey.id == key.id, ApiKey.used_month_key != month_key)
        .values(used_month=0, used_month_key=month_key)
    )
    if key.monthly_quota:
        result = db.execute(
            sa_update(ApiKey)
            .where(
                ApiKey.id == key.id,
                ApiKey.used_month < key.monthly_quota,
            )
            .values(
                used_month=ApiKey.used_month + 1,
                used_total=ApiKey.used_total + 1,
                used_month_key=month_key,
            )
        )
        if result.rowcount != 1:
            db.rollback()
            raise HTTPException(
                status_code=429,
                detail="该 API Key 本月额度（%d 次）已用完" % key.monthly_quota,
            )
    else:
        db.execute(
            sa_update(ApiKey)
            .where(ApiKey.id == key.id)
            .values(used_total=ApiKey.used_total + 1, used_month=ApiKey.used_month + 1, used_month_key=month_key)
        )
    db.commit()


def _download_image(url: str, allow_internal: bool) -> bytes:
    try:
        net_guard.assert_safe_url(url, allow_internal=allow_internal)
    except net_guard.UnsafeUrlError as e:
        raise HTTPException(status_code=422, detail="图片 URL 不可用（%s）：%s" % (str(e), url[:100]))
    try:
        # 不跟随重定向：避免公网地址 302 跳到内网绕过 SSRF 校验
        with httpx.Client(timeout=30, follow_redirects=False) as client:
            resp = client.get(url)
    except httpx.HTTPError as e:
        raise HTTPException(status_code=422, detail="下载图片失败：%s" % str(e)[:150])
    if 300 <= resp.status_code < 400:
        raise HTTPException(
            status_code=422,
            detail="图片 URL 发生重定向，请提供直接可访问的图片地址：%s" % url[:100],
        )
    if resp.status_code != 200:
        raise HTTPException(status_code=422, detail="下载图片失败（HTTP %d）：%s" % (resp.status_code, url[:100]))
    if len(resp.content) > MAX_UPLOAD_BYTES:
        raise HTTPException(status_code=422, detail="图片超过 %dMB：%s" % (MAX_UPLOAD_MB, url[:100]))
    return resp.content


def _decode_base64_image(b64: str, index: int) -> bytes:
    # partition 不会因缺少逗号而越界（对比 split[1] 会 IndexError → 500）
    raw = b64.partition(",")[2] if b64.startswith("data:") else b64
    if not raw:
        raise HTTPException(status_code=422, detail="第 %d 个 base64 图片内容为空" % (index + 1))
    # base64 解出的字节约为字符串长度的 3/4，先按长度粗判，避免超大串占内存
    if len(raw) > MAX_UPLOAD_BYTES / 3 * 4 + 8:
        raise HTTPException(status_code=422, detail="第 %d 个 base64 图片超过 %dMB" % (index + 1, MAX_UPLOAD_MB))
    try:
        data = base64.b64decode(raw, validate=True)
    except (binascii.Error, ValueError):
        raise HTTPException(status_code=422, detail="第 %d 个 base64 图片无法解码" % (index + 1))
    if len(data) > MAX_UPLOAD_BYTES:
        raise HTTPException(status_code=422, detail="第 %d 个 base64 图片超过 %dMB" % (index + 1, MAX_UPLOAD_MB))
    return data


def _cleanup_stored_image(db: Session, rel: str) -> None:
    """回滚这一请求已落盘并入库的图片（文件 + Upload 行）。"""
    storage.delete_file(rel)
    row = db.query(Upload).filter(Upload.path == rel).first()
    if row is not None:
        db.delete(row)
        db.commit()


def _store_incoming_image(db: Session, key: ApiKey, data: bytes, name: str) -> str:
    if len(data) > MAX_UPLOAD_BYTES:
        raise HTTPException(status_code=422, detail="图片超过 %dMB：%s" % (MAX_UPLOAD_MB, name[:100]))
    try:
        width, height, fmt = storage.validate_image(data)
    except storage.StorageError:
        raise HTTPException(status_code=422, detail="提供的内容不是有效图片：%s" % name[:100])
    # 格式白名单，与上传端点对齐（不认识的格式直接拒绝而非误存成 .webp）
    if fmt not in ("png", "jpeg", "jpg", "webp"):
        raise HTTPException(status_code=422, detail="只支持 png / jpg / webp / heic 图片：%s" % name[:100])
    suffix = ".png" if fmt == "png" else ".jpg" if fmt in ("jpg", "jpeg") else ".webp"
    rel, w, h = storage.save_upload(data, suffix)
    db.add(
        Upload(
            api_key_id=key.id, original_name=name[:250], path=rel,
            width=w, height=h, size_bytes=len(data),
        )
    )
    db.commit()
    return rel


@router.get("/ping")
def ping(key: ApiKey = Depends(get_api_client)):
    return {"ok": True, "key_name": key.name, "message": "API Key 有效"}


@router.get("/categories")
def v1_list_categories(key: ApiKey = Depends(get_api_client), db: Session = Depends(get_db)):
    """可用的提示词库分类。提交任务时把 slug 放进 category_slug 即可。

    这是推荐的用法：不必自己挑提示词，平台会看商品图、从该分类里挑风格参考、
    再结合你的 extra_instructions 现场写提示词。
    """
    items = []
    order = inspiration.ECOMMERCE_SLUGS + [
        s for s in inspiration.CATEGORIES if s not in inspiration.ECOMMERCE_SLUGS
    ]
    for slug in order:
        count = (
            db.query(PromptTemplate)
            .filter(PromptTemplate.source == "youmind")
            .filter(inspiration.slug_filter(slug))
            .count()
        )
        if count:
            items.append({
                "slug": slug,
                "name": inspiration.CATEGORIES[slug],
                "count": count,
                "recommended_for_ecommerce": slug in inspiration.ECOMMERCE_SLUGS,
            })
    return items


@router.get("/templates")
def v1_list_templates(key: ApiKey = Depends(get_api_client), db: Session = Depends(get_db)):
    rows = (
        db.query(PromptTemplate)
        .filter(PromptTemplate.is_enabled.is_(True))
        .filter(PromptTemplate.source == "user")
        .order_by(PromptTemplate.sort.asc(), PromptTemplate.id.desc())
        .all()
    )
    public_base = settings_service.get(db, "public_base_url")
    return [template_to_dict(t, public_base) for t in rows]


@router.post("/uploads")
async def v1_upload(
    file: UploadFile = File(...),
    key: ApiKey = Depends(get_api_client),
    db: Session = Depends(get_db),
):
    suffix = Path(file.filename or "image.png").suffix.lower()
    if suffix not in ALLOWED_IMAGE_EXTS:
        raise HTTPException(status_code=422, detail="只支持 png / jpg / jpeg / webp / heic 图片")
    data = await file.read()
    if len(data) > MAX_UPLOAD_MB * 1024 * 1024:
        raise HTTPException(status_code=422, detail="图片不能超过 %dMB" % MAX_UPLOAD_MB)
    try:
        width, height, _fmt = storage.validate_image(data)
        rel, w, h = storage.save_upload(data, suffix)
    except storage.StorageError as e:
        raise HTTPException(status_code=422, detail=str(e))
    record = Upload(
        api_key_id=key.id, original_name=(file.filename or "")[:250], path=rel,
        width=w, height=h, size_bytes=len(data),
    )
    db.add(record)
    db.commit()
    db.refresh(record)
    return upload_to_dict(record, settings_service.get(db, "public_base_url"))


@router.post("/generations", status_code=202)
def v1_create_generation(
    body: V1GenerationBody,
    key: ApiKey = Depends(get_api_client),
    db: Session = Depends(get_db),
):
    # 先校验图片总数，超限直接 422（而不是静默丢弃多余的图）
    total_images = len(body.upload_ids) + len(body.image_urls) + len(body.images_base64)
    if total_images > MAX_INPUT_IMAGES:
        raise HTTPException(status_code=422, detail="最多支持 %d 张商品图" % MAX_INPUT_IMAGES)

    allow_internal = bool(settings_service.get(db, "allow_internal_targets"))
    # 这一请求内新落盘的图片，若后续任何步骤失败则回滚删除，避免产生孤儿文件
    saved_paths: List[str] = []
    try:
        input_paths: List[str] = list(resolve_upload_paths(db, body.upload_ids, api_key_id=key.id))
        for url in body.image_urls:
            rel = _store_incoming_image(db, key, _download_image(url, allow_internal), url)
            saved_paths.append(rel)
            input_paths.append(rel)
        for i, b64 in enumerate(body.images_base64):
            rel = _store_incoming_image(db, key, _decode_base64_image(b64, i), "base64_%d" % i)
            saved_paths.append(rel)
            input_paths.append(rel)

        job = create_job(
            db,
            source="api",
            api_key_id=key.id,
            template_id=body.template_id,
            category_slug=body.category_slug,
            prompt=body.prompt,
            variables=body.variables,
            extra_instructions=body.extra_instructions,
            input_paths=input_paths,
            n=body.n,
            size=body.size,
            quality=body.quality,
            callback_url=body.callback_url,
            external_ref=body.external_ref,
        )
    except HTTPException:
        for rel in saved_paths:
            _cleanup_stored_image(db, rel)
        raise

    # 任务已建成再扣额度：参数报错不会白扣。但额度不足时任务已经落库，
    # 必须把它废掉，否则 worker 照样会领走去生图（ERP 收到 429 却仍被扣费）
    try:
        _consume_quota(db, key)
    except HTTPException:
        job.status = "failed"
        job.error = "本月额度已用完，任务未执行"
        job.finished_at = datetime.utcnow()
        db.commit()
        raise
    public_base = settings_service.get(db, "public_base_url")
    return job_to_dict(job, public_base, include_inputs=False)


@router.get("/generations/{job_id}")
def v1_get_generation(
    job_id: str, key: ApiKey = Depends(get_api_client), db: Session = Depends(get_db)
):
    job = db.get(GenerationJob, job_id)
    if job is None or job.api_key_id != key.id:
        raise HTTPException(status_code=404, detail="任务不存在")
    return job_to_dict(job, settings_service.get(db, "public_base_url"), include_inputs=False)
