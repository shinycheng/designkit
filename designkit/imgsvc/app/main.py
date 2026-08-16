"""designkit 图像预处理服务 —— HTTP 层。

契约见 `designkit/docs/设计定型.md` 第七节。要点：

* 容器内监听 8000，**不映射宿主端口**，只挂在 sub2api 的 docker 网络上。
* `GET  /healthz`       —— 报告 pillow / pillow-heif / 放大模型是否可用。
* `POST /v1/preprocess` —— multipart 上传，**成功直接返回图片字节**，
  元数据放响应头（不包 JSON base64：一张 2K 图 base64 之后要多占三分之一内存，
  而 Go 侧拿到还要再解一次）。
* `POST /v1/upscale`    —— 高清放大（Real-ESRGAN ×4）。同样 multipart 进、
  PNG 字节出。**同步阻塞**，一张要几十秒到两分钟——排队由 Go 侧的
  UpscaleService 管（内存队列 + 单 worker），这里不需要也不做异步。
* 失败一律 JSON，且**只有一种格式**：`{"error":{"code":"...","message":"中文"}}`。
* **fail-closed**：任何异常都返 4xx/5xx，**绝不回吐原图**。

这个服务不接触任何对象存储凭据，也不连数据库——图片字节进、图片字节出。
"""

from __future__ import annotations

import base64
import hmac
import io
import json
import logging
import os
import threading
from typing import Optional

from fastapi import Depends, FastAPI, File, Form, Header, Request, UploadFile
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse, Response
from starlette.concurrency import run_in_threadpool

from . import SERVICE_NAME, SERVICE_VERSION
from .errors import BadRequest, ImgSvcError, PayloadTooLarge, ServiceBusy, Unauthorized
from .imaging import (
    HEIF_AVAILABLE,
    HEIF_VERSION,
    PILLOW_VERSION,
    SUPPORTED_RATIOS,
    parse_aspect,
    parse_bool,
    parse_max_dimension,
    preprocess,
)
from . import upscale as upscale_mod

logger = logging.getLogger("designkit.imgsvc")

# --------------------------------------------------------------------------
# 可调参数（全部来自环境变量，没有一个埋成常量）
# --------------------------------------------------------------------------
ENV_TOKEN = "DESIGNKIT_IMGSVC_TOKEN"

#: `max_dimension` 不传时用它。⚠ 它直接决定出图落 2K 还是 4K 计费档。
DEFAULT_MAX_DIMENSION = int(os.getenv("DESIGNKIT_IMGSVC_DEFAULT_MAX_DIMENSION", "2048"))
MIN_MAX_DIMENSION = int(os.getenv("DESIGNKIT_IMGSVC_MIN_MAX_DIMENSION", "256"))
MAX_MAX_DIMENSION = int(os.getenv("DESIGNKIT_IMGSVC_MAX_MAX_DIMENSION", "8192"))

#: 单张上传上限。界面提示写 20MB（见 CLAUDE.md 二·五 E）。
MAX_UPLOAD_BYTES = int(os.getenv("DESIGNKIT_IMGSVC_MAX_UPLOAD_BYTES", str(20 * 1024 * 1024)))

#: 解码前先按「宽×高」拦一道，防止 1MB 的 PNG 解出来是 200MP 把内存吃光。
MAX_PIXELS = int(os.getenv("DESIGNKIT_IMGSVC_MAX_PIXELS", str(80_000_000)))

#: 同时处理几张。一张 2K RGB 图在内存里约 12MB，加上画布和编码缓冲要按 3~4 倍估。
MAX_CONCURRENCY = int(os.getenv("DESIGNKIT_IMGSVC_MAX_CONCURRENCY", "4"))

#: 排队等这么久还轮不到就返 503，让 Go 侧重试，而不是把连接一直挂着。
QUEUE_WAIT_SECONDS = float(os.getenv("DESIGNKIT_IMGSVC_QUEUE_WAIT_SECONDS", "20"))

_gate = threading.BoundedSemaphore(max(1, MAX_CONCURRENCY))


app = FastAPI(
    title="designkit 图像预处理服务",
    version=SERVICE_VERSION,
    description="给生图网关准备输入图：HEIC 转码、EXIF 摆正、按比例补边、透明合白、限制最长边。",
    docs_url="/docs",
    redoc_url=None,
    openapi_url="/openapi.json",
)


# --------------------------------------------------------------------------
# 统一错误响应
# --------------------------------------------------------------------------
def _error_response(status_code: int, code: str, message: str) -> JSONResponse:
    return JSONResponse(
        status_code=status_code,
        content={"error": {"code": code, "message": message}},
    )


@app.exception_handler(ImgSvcError)
async def _handle_imgsvc_error(request: Request, exc: ImgSvcError) -> JSONResponse:
    if exc.status_code >= 500:
        logger.exception("预处理失败：%s", exc.message)
    else:
        logger.info("预处理拒绝（%s %s）：%s", exc.status_code, exc.code, exc.message)
    headers = {}
    if isinstance(exc, Unauthorized):
        headers["WWW-Authenticate"] = "Bearer"
    response = _error_response(exc.status_code, exc.code, exc.message)
    for key, value in headers.items():
        response.headers[key] = value
    return response


@app.exception_handler(RequestValidationError)
async def _handle_validation_error(request: Request, exc: RequestValidationError) -> JSONResponse:
    """把 FastAPI 自带的 422 校验错误改写成我们唯一的那种格式。

    不这么做的话，「没带 file 字段」会返回一种结构完全不同的 JSON，
    Go 侧反序列化就会在某个偶发场景炸掉。
    """
    missing = [
        ".".join(str(part) for part in err.get("loc", ()) if part != "body")
        for err in exc.errors()
    ]
    detail = "、".join(name for name in missing if name) or "请求体"
    return _error_response(400, "invalid_request", "请求参数不完整或格式不对：%s。" % detail)


@app.exception_handler(Exception)
async def _handle_unexpected(request: Request, exc: Exception) -> JSONResponse:
    """兜底 500。

    ⚠ 这里**只返错误**。老代码在这个位置返回的是原始图片字节（fail-open），
    结果是补边失败也照样出图、照样扣钱、比例还是错的。绝不重蹈。
    """
    logger.exception("预处理发生未预期异常")
    return _error_response(
        500, "internal_error", "服务端处理图片时出错，这张图没有被发送出去。"
    )


# --------------------------------------------------------------------------
# 鉴权
# --------------------------------------------------------------------------
def require_token(
    authorization: Optional[str] = Header(default=None, alias="Authorization"),
) -> None:
    """`DESIGNKIT_IMGSVC_TOKEN` 非空时校验 `Authorization: Bearer <token>`。

    每次请求现读环境变量：一是方便测试，二是这点开销可以忽略。
    """
    expected = os.getenv(ENV_TOKEN, "").strip()
    if not expected:
        return
    if not authorization:
        raise Unauthorized("缺少 Authorization 头。")
    parts = authorization.split(None, 1)
    if len(parts) != 2 or parts[0].lower() != "bearer":
        raise Unauthorized("Authorization 头必须是 `Bearer <token>` 的形式。")
    if not hmac.compare_digest(parts[1].strip(), expected):
        raise Unauthorized("鉴权失败：token 不对。")


# --------------------------------------------------------------------------
# 健康检查
# --------------------------------------------------------------------------
@app.get("/healthz")
async def healthz() -> JSONResponse:
    """始终返 200；能不能干活看 `status` 字段。

    容器 HEALTHCHECK 只看 HTTP 状态码——没装 HEIC 解码器时服务仍能处理
    JPG/PNG，不该被判成不健康、然后被编排系统反复重启。
    """
    body = {
        "status": "ok" if HEIF_AVAILABLE else "degraded",
        "service": SERVICE_NAME,
        "version": SERVICE_VERSION,
        "pillow": {"available": True, "version": PILLOW_VERSION},
        "pillow_heif": {"available": HEIF_AVAILABLE, "version": HEIF_VERSION},
        "heif_supported": HEIF_AVAILABLE,
        "auth_required": bool(os.getenv(ENV_TOKEN, "").strip()),
        "defaults": {
            "max_dimension": DEFAULT_MAX_DIMENSION,
            "max_dimension_min": MIN_MAX_DIMENSION,
            "max_dimension_max": MAX_MAX_DIMENSION,
            "max_upload_bytes": MAX_UPLOAD_BYTES,
            "max_pixels": MAX_PIXELS,
            "max_concurrency": MAX_CONCURRENCY,
        },
        "supported_ratios": list(SUPPORTED_RATIOS),
    }
    # 「高清放大」的可用性。不可用**不算不健康**：预处理照常干活，
    # 放大端点会返 503 upscale_unavailable，Go 侧翻成「还没准备好」。
    upscale_ok, upscale_reason = upscale_mod.availability()
    body["upscale"] = {
        "available": upscale_ok,
        "onnxruntime": upscale_mod.ORT_VERSION,
        "scale": upscale_mod.SCALE,
        "tile": upscale_mod.TILE_SIZE,
        "max_input_pixels": upscale_mod.MAX_INPUT_PIXELS,
    }
    if not upscale_ok:
        body["upscale"]["reason"] = upscale_reason
    if not HEIF_AVAILABLE:
        body["message"] = "pillow-heif 没装上，iPhone 拍的 HEIC 照片会被拒绝（422 heif_unsupported）。"
    return JSONResponse(status_code=200, content=body)


# --------------------------------------------------------------------------
# 预处理
# --------------------------------------------------------------------------
def _b64(text: str) -> str:
    """中文说明走 base64。

    HTTP 头按 RFC 只保证 latin-1，中文直接塞进去在不同的服务器/客户端上
    表现不一样（有的报错、有的乱码）。base64 之后全是 ASCII，零歧义。
    """
    return base64.b64encode(text.encode("utf-8")).decode("ascii")


@app.post("/v1/preprocess")
async def preprocess_endpoint(
    request: Request,
    file: UploadFile = File(..., description="要处理的原图"),
    ratio: str = Form(default="", description="目标比例，如 3:4；留空或 auto 表示不补边"),
    keep_transparency: str = Form(default="false", description="true 时保留透明底并补透明边"),
    max_dimension: str = Form(default="", description="输出最长边上限，默认 2048"),
    _auth: None = Depends(require_token),
) -> Response:
    # ---- 先看 Content-Length，超了就别费劲把 body 收完 ----------------------
    declared = request.headers.get("content-length")
    if declared and declared.isdigit() and int(declared) > MAX_UPLOAD_BYTES + 64 * 1024:
        raise PayloadTooLarge(
            "上传内容超过 %d 字节的上限。" % MAX_UPLOAD_BYTES, code="file_too_large"
        )

    # ---- 参数（解析失败直接 400，绝不用默认值蒙混过去）---------------------
    aspect = parse_aspect(ratio)
    keep_alpha = parse_bool(keep_transparency, default=False)
    limit = parse_max_dimension(
        max_dimension, DEFAULT_MAX_DIMENSION, MIN_MAX_DIMENSION, MAX_MAX_DIMENSION
    )

    raw = await file.read()
    await file.close()
    if len(raw) > MAX_UPLOAD_BYTES:
        raise PayloadTooLarge(
            "图片 %d 字节，超过 %d 字节的上限。" % (len(raw), MAX_UPLOAD_BYTES),
            code="file_too_large",
        )
    if not raw:
        raise BadRequest("上传的文件是空的。", code="empty_file")

    def _work():
        # 并发闸门：拿不到名额就 503，让 Go 侧退避重试，
        # 而不是几十张图一起解码把容器 OOM 掉。
        if not _gate.acquire(timeout=QUEUE_WAIT_SECONDS):
            raise ServiceBusy("图像服务正忙，请稍后重试。")
        try:
            return preprocess(
                data=raw,
                aspect=aspect,
                keep_transparency=keep_alpha,
                max_dimension=limit,
                max_pixels=MAX_PIXELS,
            )
        finally:
            _gate.release()

    result = await run_in_threadpool(_work)

    headers = {
        "X-Dk-Changed": "true" if result.changed else "false",
        "X-Dk-Width": str(result.width),
        "X-Dk-Height": str(result.height),
        # 中文说明（JSON 数组）经 base64；空数组是 "W10="
        "X-Dk-Actions": _b64(json.dumps(result.action_notes, ensure_ascii=False)),
        # 同一批动作的 ASCII 机器码，逗号分隔，不用解码就能判断
        "X-Dk-Action-Codes": ",".join(result.action_codes),
        "X-Dk-Source-Width": str(result.source_width),
        "X-Dk-Source-Height": str(result.source_height),
        "X-Dk-Source-Format": result.source_format or "unknown",
        "X-Dk-Format": result.mime.split("/", 1)[-1],
        "X-Dk-Suffix": result.suffix,
        "X-Dk-Bytes": str(len(result.data)),
        "X-Dk-Ratio": ("%d:%d" % aspect) if aspect else "auto",
        "X-Dk-Max-Dimension": str(limit),
        "Content-Length": str(len(result.data)),
        "Cache-Control": "no-store",
    }
    return Response(content=result.data, media_type=result.mime, headers=headers)


# --------------------------------------------------------------------------
# 高清放大（Real-ESRGAN ×4）
# --------------------------------------------------------------------------
@app.post("/v1/upscale")
async def upscale_endpoint(
    request: Request,
    file: UploadFile = File(..., description="要放大的图"),
    _auth: None = Depends(require_token),
) -> Response:
    """一张图进、放大 4 倍的 PNG 出。

    - **同步阻塞**：一张 1254×1254 在 NAS 上要一两分钟。排队、超时、
      去重都在 Go 侧的 UpscaleService 里，这里只管算。
    - 推理内部自带串行锁（upscale.py 的 _infer_lock），
      **不占** preprocess 那个并发闸门——放大慢，占了会把补边请求全堵住。
    - 输出一律 PNG：放大的意义就是保细节，再走有损压缩是自相矛盾。
    """
    # 先看 Content-Length，超了就别费劲把 body 收完（跟 preprocess 同一道闸）。
    declared = request.headers.get("content-length")
    if declared and declared.isdigit() and int(declared) > MAX_UPLOAD_BYTES + 64 * 1024:
        raise PayloadTooLarge(
            "上传内容超过 %d 字节的上限。" % MAX_UPLOAD_BYTES, code="file_too_large"
        )

    raw = await file.read()
    await file.close()
    if len(raw) > MAX_UPLOAD_BYTES:
        raise PayloadTooLarge(
            "图片 %d 字节，超过 %d 字节的上限。" % (len(raw), MAX_UPLOAD_BYTES),
            code="file_too_large",
        )
    if not raw:
        raise BadRequest("上传的文件是空的。", code="empty_file")

    def _work():
        image = upscale_mod.decode_to_rgb(raw)
        source_width, source_height = image.size
        enlarged = upscale_mod.upscale_image(image)
        buffer = io.BytesIO()
        enlarged.save(buffer, "PNG")
        return source_width, source_height, enlarged.size, buffer.getvalue()

    source_width, source_height, (out_width, out_height), data = await run_in_threadpool(_work)

    headers = {
        "X-Dk-Width": str(out_width),
        "X-Dk-Height": str(out_height),
        "X-Dk-Source-Width": str(source_width),
        "X-Dk-Source-Height": str(source_height),
        "X-Dk-Scale": str(upscale_mod.SCALE),
        "X-Dk-Bytes": str(len(data)),
        "Content-Length": str(len(data)),
        "Cache-Control": "no-store",
    }
    return Response(content=data, media_type="image/png", headers=headers)
