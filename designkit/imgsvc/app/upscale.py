"""高清放大：Real-ESRGAN 小模型（realesr-general-x4v3）的 ONNX CPU 推理。

## 模型

`models/realesr-general-x4v3.onnx`（4.9MB，BSD-3-Clause，来源和校验值见
`models/LICENSE-realesrgan.md`）。接口：

    input  [1, 3, H, W]   NCHW、RGB、float32、取值 [0,1]，H/W 动态
    output [1, 3, 4H, 4W] 固定放大 4 倍；图内没有 clip，转 uint8 前要自己 clamp

## 为什么切块（tile）推理

整张图一次进模型，中间层激活的内存占用跟像素数成正比：一张 2048×2048
直接推理要几个 GB，容器当场 OOM。所以按 512×512 切块、块间重叠 8 像素
（消除接缝），一块一块推、拼回大图。**逐块串行，不并发**——
单块推理的峰值内存已按 1~2GB 估（设计冻结时定的），并发就是叠加。

## 为什么整个服务同一时刻只放大一张

`_infer_lock` 把推理串成一条队列。排队本身由 Go 侧的 UpscaleService 管
（内存队列 + 单 worker），这把锁只是兜底：万一有第二个调用方绕过 Go 侧
直接打这个接口，也不会两张图同时推理把内存顶爆。

## fail-closed

跟 preprocess 同一条纪律：任何一步失败都抛异常、由 HTTP 层返错，
**绝不返回原图冒充放大结果**——那会让运营拿到一张跟原图一模一样的
「放大图」，还以为功能是好的。
"""

from __future__ import annotations

import os
import threading
from typing import Optional, Tuple

from PIL import Image

from .errors import ImgSvcError

# --------------------------------------------------------------------------
# 可调参数（全部来自环境变量，没有一个埋成常量）
# --------------------------------------------------------------------------

#: 模型文件路径。默认取本文件上两级的 models/ 目录（Dockerfile 里 COPY 的位置）。
MODEL_PATH = os.getenv(
    "DESIGNKIT_UPSCALE_MODEL_PATH",
    os.path.join(
        os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
        "models",
        "realesr-general-x4v3.onnx",
    ),
)

#: 输入图的像素数上限，默认 2048×2048。
#: ⚠ 这个上限跟 preprocess 的 MAX_PIXELS（8000 万）**不是一回事**：
#: 放大是 ×4，一张 2048×2048 进来就是 8192×8192 出去（uint8 画布 200MB），
#: 再放宽输出内存直接失控。gpt-image-2 的出图最长边 ≤2048，正好够用。
MAX_INPUT_PIXELS = int(os.getenv("DESIGNKIT_UPSCALE_MAX_PIXELS", str(2048 * 2048)))

#: 切块边长与重叠宽度。512+8 是设计冻结值，改之前先量内存。
TILE_SIZE = int(os.getenv("DESIGNKIT_UPSCALE_TILE", "512"))
TILE_OVERLAP = int(os.getenv("DESIGNKIT_UPSCALE_OVERLAP", "8"))

#: 模型的固定放大倍数。跟着模型走，不是配置项——换模型才需要改。
SCALE = 4


class UpscaleUnavailable(ImgSvcError):
    """onnxruntime 没装上 / 模型文件不在。503，属于部署问题不是图的问题。"""

    status_code = 503
    code = "upscale_unavailable"


# --------------------------------------------------------------------------
# onnxruntime 会话：全局单例、懒加载
# --------------------------------------------------------------------------
#
# 懒加载的原因：模型加载要几百毫秒、常驻 ~50MB。预处理接口完全用不到它，
# 没人点「高清放大」的部署（比如还没发新前端）不该白扛这份内存。

try:  # pragma: no cover - 依赖装没装是部署问题，不是代码分支
    import numpy as np

    NUMPY_AVAILABLE = True
except Exception:  # noqa: BLE001
    NUMPY_AVAILABLE = False

try:  # pragma: no cover
    import onnxruntime as ort

    ORT_AVAILABLE = True
    ORT_VERSION = getattr(ort, "__version__", "unknown")
except Exception:  # noqa: BLE001
    ORT_AVAILABLE = False
    ORT_VERSION = None

_session = None
_session_lock = threading.Lock()
#: 推理串行锁：同一时刻只放大一张（内存原因，见文件头）。
_infer_lock = threading.Lock()


def availability() -> Tuple[bool, Optional[str]]:
    """能不能干活。(True, None) 或 (False, 中文原因)。/healthz 也用它。"""
    if not NUMPY_AVAILABLE:
        return False, "numpy 没装上，无法做放大推理。"
    if not ORT_AVAILABLE:
        return False, "onnxruntime 没装上，无法做放大推理。"
    if not os.path.isfile(MODEL_PATH):
        return False, "找不到放大模型文件：%s。" % MODEL_PATH
    return True, None


def _get_session():
    """取（第一次调用时创建）onnxruntime 会话。不可用时抛 UpscaleUnavailable。"""
    global _session
    ok, reason = availability()
    if not ok:
        raise UpscaleUnavailable(reason or "放大功能不可用。")
    if _session is not None:
        return _session
    with _session_lock:
        if _session is None:
            options = ort.SessionOptions()
            # 日志压到 warning 以上，不然每块 tile 都刷一屏 verbose。
            options.log_severity_level = 3
            _session = ort.InferenceSession(
                MODEL_PATH,
                sess_options=options,
                providers=["CPUExecutionProvider"],
            )
    return _session


def reset_session_for_tests() -> None:
    """selfcheck 用：把会话丢掉，验证懒加载路径。生产代码不调它。"""
    global _session
    with _session_lock:
        _session = None


# --------------------------------------------------------------------------
# 解码
# --------------------------------------------------------------------------
def decode_to_rgb(data: bytes) -> Image.Image:
    """解码 + EXIF 摆正 + 透明合白，产出 RGB。

    像素上限用**放大自己的** MAX_INPUT_PIXELS（默认 2048×2048），
    不是 preprocess 那个 8000 万——放大是 ×4，上限必须严得多（见常量注释）。
    透明合白直接复用 imaging 里那一份，两个入口对透明底的处理永远一致。
    """
    from PIL import ImageOps

    from .imaging import _flatten_alpha, open_image

    image = open_image(data, MAX_INPUT_PIXELS)
    # exif_transpose 处理全部 8 种 EXIF 朝向；没有 EXIF 时原样返回。
    transposed = ImageOps.exif_transpose(image)
    if transposed is not None:
        image = transposed
    return _flatten_alpha(image)


# --------------------------------------------------------------------------
# 推理
# --------------------------------------------------------------------------
def upscale_image(image: Image.Image) -> Image.Image:
    """把一张 RGB 图放大 4 倍。

    输入必须已经是 RGB（透明合白之类由调用方做，跟 preprocess 的
    `_flatten_alpha` 共用同一套规则）。逐块推理、串行执行。
    """
    if image.mode != "RGB":
        raise ValueError("upscale_image 只接受 RGB 图，收到的是 %s" % image.mode)

    width, height = image.size
    if width <= 0 or height <= 0:
        raise ValueError("图片尺寸不合法（宽或高为 0）。")

    session = _get_session()
    source = np.asarray(image, dtype=np.uint8)  # HWC

    out_w, out_h = width * SCALE, height * SCALE
    # 输出画布用 uint8：一张 8192×8192 RGB 是 200MB，float32 就是 800MB。
    canvas = np.empty((out_h, out_w, 3), dtype=np.uint8)

    input_name = session.get_inputs()[0].name

    with _infer_lock:
        for y0 in range(0, height, TILE_SIZE):
            for x0 in range(0, width, TILE_SIZE):
                y1 = min(y0 + TILE_SIZE, height)
                x1 = min(x0 + TILE_SIZE, width)
                # 输入块四周各多带 overlap 像素，输出时把这圈裁掉——接缝就没了。
                iy0 = max(y0 - TILE_OVERLAP, 0)
                ix0 = max(x0 - TILE_OVERLAP, 0)
                iy1 = min(y1 + TILE_OVERLAP, height)
                ix1 = min(x1 + TILE_OVERLAP, width)

                block = source[iy0:iy1, ix0:ix1]
                # HWC uint8 → 1CHW float32 [0,1]
                tensor = block.astype(np.float32) / 255.0
                tensor = np.transpose(tensor, (2, 0, 1))[np.newaxis, ...]
                tensor = np.ascontiguousarray(tensor)

                (result,) = session.run(None, {input_name: tensor})
                # result: [1, 3, 4*th, 4*tw]，图内没有 clip，先 clamp 再转 uint8。
                oy = (y0 - iy0) * SCALE
                ox = (x0 - ix0) * SCALE
                core = result[0, :, oy : oy + (y1 - y0) * SCALE, ox : ox + (x1 - x0) * SCALE]
                core = np.clip(core * 255.0 + 0.5, 0.0, 255.0).astype(np.uint8)
                canvas[y0 * SCALE : y1 * SCALE, x0 * SCALE : x1 * SCALE] = np.transpose(
                    core, (1, 2, 0)
                )

    return Image.fromarray(canvas, mode="RGB")
