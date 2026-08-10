"""对外开放 API（供 ERP 等第三方平台对接）。

鉴权：请求头 X-API-Key: dk_xxx（在网页「API 对接」页创建）。
任务为异步：提交后立即返回 job_id，可轮询查询，也可留 callback_url 等回调。
详细对接文档见项目 docs/erp-api.md，在线调试见 /docs。
"""
import base64
import binascii
import logging
from datetime import datetime
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple

import httpx
from fastapi import APIRouter, Depends, File, HTTPException, UploadFile
from fastapi.responses import FileResponse
from pydantic import BaseModel, Field
from sqlalchemy import or_
from sqlalchemy import update as sa_update
from sqlalchemy.orm import Session

from ..config import ALLOWED_IMAGE_EXTS, MAX_UPLOAD_MB
from ..deps import get_api_client, get_db
from ..models import ApiKey, GeneratedImage, GenerationJob, PromptTemplate, Upload
from ..serializers import job_to_dict, template_to_dict, upload_to_dict
from ..services import inspiration, net_guard, settings_service, storage
from ..services.jobs import MAX_INPUT_IMAGES, create_job, resolve_upload_paths

logger = logging.getLogger("designkit.api")

MAX_UPLOAD_BYTES = MAX_UPLOAD_MB * 1024 * 1024

# 图片签名链接的默认有效期（小时），与 config.py 的 erp_file_link_ttl_hours 默认值一致。
DEFAULT_ERP_LINK_TTL_HOURS = 168

router = APIRouter(prefix="/api/v1", tags=["对外API-ERP对接"])


def _erp_sign(db: Session, key: ApiKey) -> Tuple[int, int]:
    """本次请求返回的图片地址要用的签名参数：(这把 Key 的 id, 有效小时数)。

    **凡是对外返回图片地址的地方都必须带上它。** /files 现在只认凭证，不带签名
    的链接一律 403；而接口本身照样返回 200，对接方看到的是「你们给的地址打不开」，
    是最难归因的一种故障。所以宁可每处都显式写一遍，也不做「自动推断该不该签」。

    第二个元素传的是**有效小时数**，不是到期时间戳——storage.to_url 自己按
    「当前时间 + 小时数 × 3600」算到期时间（见该函数注释）。把算好的时间戳传进去
    会被当成小时数，算出一条几十万年后才过期的链接：表面上完全正常，
    实际上等于把「链接会过期」这层保护整个关掉了。

    有效小时数取自设置项 erp_file_link_ttl_hours。它没有被 settings_router 的
    数字范围校验覆盖，管理员填个「七天」就会存成字符串，storage 那边会抛
    StorageError、整条对外接口 500。这里兜一层回落到默认值并记 WARNING：
    链接仍然是签过名的，安全性没有降级，只是有效期回到 7 天。
    （worker.py 的 _erp_link_ttl_hours 是同样的兜底，两处都要，因为回调那条线
      走的是 worker 自己的设置字典、拿不到 db。）
    """
    raw = settings_service.get(db, "erp_file_link_ttl_hours")
    try:
        hours = int(raw)
    except (TypeError, ValueError):
        logger.warning(
            "设置项 erp_file_link_ttl_hours 不是数字（当前值：%r），"
            "图片链接有效期按默认 %d 小时处理，请到设置页改成整数",
            raw, DEFAULT_ERP_LINK_TTL_HOURS,
        )
        hours = DEFAULT_ERP_LINK_TTL_HOURS
    return (key.id, hours)


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
            # user_id 记的是「这把 Key 是谁的」。只写 api_key_id 的话，这些图在
            # 网页端就是一批无主数据：/files 的归属反查按 user_id 判本人，
            # Key 的主人自己反而打不开自己 ERP 传上来的图。
            user_id=key.user_id,
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
    """可用的提示词模板：平台公共模板 + 这把 Key 的主人自己建的。"""
    rows = (
        db.query(PromptTemplate)
        .filter(PromptTemplate.is_enabled.is_(True))
        .filter(PromptTemplate.source == "user")
        # 属主过滤。owner_user_id 为 NULL = 平台公共模板，谁都能用；
        # 其余只给这把 Key 的主人。少了这一句，任意一把有效 Key 都能读到站内
        # **所有人**自建模板的 prompt_template 全文——提示词正文正是这类账号
        # 最值钱的东西，而且泄露过程完全不留痕迹（对方只是调了一次正常接口）。
        # key.user_id 为 NULL 的存量 Key 只看得到公共模板，是安全的默认。
        .filter(
            or_(
                PromptTemplate.owner_user_id.is_(None),
                PromptTemplate.owner_user_id == key.user_id,
            )
        )
        .order_by(PromptTemplate.sort.asc(), PromptTemplate.id.desc())
        .all()
    )
    public_base = settings_service.get(db, "public_base_url")
    # 模板封面也在 /files 下面，同样要签名（它是全站共用素材，签名校验时按
    # 「公共资源」放行，但**必须带签名参数**才走得到那一步）
    sign = _erp_sign(db, key)
    return [template_to_dict(t, public_base, sign=sign) for t in rows]


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
        # 同 _store_incoming_image：记上这把 Key 属于谁，否则这些图在网页端无人认领
        user_id=key.user_id,
        api_key_id=key.id, original_name=(file.filename or "")[:250], path=rel,
        width=w, height=h, size_bytes=len(data),
    )
    db.add(record)
    db.commit()
    db.refresh(record)
    return upload_to_dict(
        record, settings_service.get(db, "public_base_url"), sign=_erp_sign(db, key)
    )


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
            # user_id 是「这把 Key 是谁的」。不传的话所有 ERP 任务的 job.user_id
            # 恒为 NULL，worker 里「按人取网关 Key」就永远取不到人，整条按人计费
            # 的链路在 ERP 这一侧等于没接上。
            # 存量 Key 已在启动回填里挂到管理员名下，而管理员走 resolve_for_user
            # 的规则 4 回落全局 Key —— 所以升级当天 ERP 的行为与今天逐字一致。
            user_id=key.user_id,
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
    return job_to_dict(job, public_base, include_inputs=False, sign=_erp_sign(db, key))


@router.get("/generations/{job_id}")
def v1_get_generation(
    job_id: str, key: ApiKey = Depends(get_api_client), db: Session = Depends(get_db)
):
    job = db.get(GenerationJob, job_id)
    if job is None or job.api_key_id != key.id:
        raise HTTPException(status_code=404, detail="任务不存在")
    return job_to_dict(
        job, settings_service.get(db, "public_base_url"),
        include_inputs=False, sign=_erp_sign(db, key),
    )


@router.get("/generations/{job_id}/images/{index}", summary="直接下载任务的某一张结果图")
def v1_get_generation_image(
    job_id: str,
    index: int,
    thumbnail: bool = False,
    key: ApiKey = Depends(get_api_client),
    db: Session = Depends(get_db),
):
    """凭 X-API-Key 按「任务号 + 第几张」直接取图，返回图片文件本身。

    这条端点是「链接会过期怎么办」的正解：对接方**不需要**把我们返回的图片
    地址存进自己的库，只要记住 job_id，随时可以凭 Key 重新取图，永不过期、
    也不受签名有效期影响。反过来，正因为有它兜底，签名链接的有效期才可以
    放心地设短。

    - index 从 0 开始，与查询任务时返回的 images 数组下标一一对应；
    - thumbnail=true 取缩略图（没有缩略图时自动回退成原图）；
    - 不消耗生成额度（这只是下载，不产生任何生图费用）。
    """
    job = db.get(GenerationJob, job_id)
    # 越权和不存在都回同一个 404：不确认「这个任务号存在」，
    # 否则拿一把有效 Key 就能逐个试出别人的任务号。
    if job is None or job.api_key_id != key.id:
        raise HTTPException(status_code=404, detail="任务不存在")

    # 显式按 id 升序取，不用 job.images：那个关系没写 order_by，顺序由数据库
    # 决定（SQLite 和 PostgreSQL 不保证一致）。序号必须和查询接口返回的
    # images 数组对得上，否则对接方按 index=1 取到的可能不是同一张图。
    # id 升序 = 入库顺序 = 生成时的第几张（storage 存盘用的也是同一个序号）。
    images = (
        db.query(GeneratedImage)
        .filter(GeneratedImage.job_id == job.id)
        .order_by(GeneratedImage.id.asc())
        .all()
    )
    if not images:
        # 把任务当前状态和失败原因一起说出来：对接方最常见的情况是任务还在跑
        # 就来取图，只回一句「没有图」会让人以为是我们弄丢了。
        raise HTTPException(
            status_code=404,
            detail="这个任务还没有可下载的图片（当前状态：%s%s）"
                   % (job.status, "；失败原因：" + job.error if job.error else ""),
        )
    if index < 0 or index >= len(images):
        raise HTTPException(
            status_code=404,
            detail="这个任务只有 %d 张图，序号从 0 开始（你要的是第 %d 个）"
                   % (len(images), index),
        )

    img = images[index]
    # 缩略图可能生成失败（storage.save_output 里失败不影响主流程），这时回退取原图：
    # 让对接方拿到一张大一点的图，总好过对着 404 猜是不是任务出问题了。
    use_thumb = bool(thumbnail and img.thumb_path)
    rel = img.thumb_path if use_thumb else img.path
    try:
        full_path = storage.abs_path(rel)
    except storage.StorageError:
        # 库里存了一条越界的路径：只可能是数据被改过。不暴露细节。
        raise HTTPException(status_code=404, detail="图片文件不存在")
    if not full_path.is_file():
        raise HTTPException(
            status_code=404,
            detail="图片文件已不在服务器上（可能已被清理），请重新提交任务生成",
        )

    # 缩略图统一是 JPEG（见 services/storage.py 的 save_output）
    fmt = "jpeg" if use_thumb else str(img.format or "png").lower()
    media_type = {
        "png": "image/png", "webp": "image/webp",
        "jpg": "image/jpeg", "jpeg": "image/jpeg",
    }.get(fmt, "application/octet-stream")

    # 用 FileResponse 而不是像 /files 那样复用 StaticFiles：这条端点是
    # 机器对机器的一次性下载，不需要 StaticFiles 那套 304 协商（那是为了
    # 网页里几十张缩略图反复加载才必须的）。FileResponse 本身支持 Range。
    return FileResponse(
        full_path,
        media_type=media_type,
        # inline 而不是默认的 attachment：对接方在浏览器里粘贴这个地址核对时
        # 能直接看到图，用程序下载则不受影响。
        content_disposition_type="inline",
        filename="%s_%d%s" % (job.id, index, full_path.suffix),
        headers={
            # 图片内容不会变（同一个 job 的同一张图是不可变的），但仍限定 private：
            # 这个地址带着 Key 才能取，不能让中间的反向代理缓存一份发给别人。
            "Cache-Control": "private, max-age=300",
            "X-Content-Type-Options": "nosniff",
            "Referrer-Policy": "no-referrer",
        },
    )
