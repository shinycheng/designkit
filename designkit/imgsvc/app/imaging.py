"""发送给生图网关之前的输入图预处理。

## 为什么需要它

自建网关**忽略 size 参数，输出比例完全跟随输入图比例**。想让模型出 3:4 的图，
唯一的手段就是先把输入图变成 3:4——用「补白边」而不是裁切，产品永远完整。

其余四件事是历史上真踩过的坑：

1. Mac 导出的照片常常是 HEIC 却顶着 `.jpg` 扩展名，不装解码器直接报「无法识别」。
2. 带透明底的 PNG 直接转 RGB 会把透明区变成黑块，必须先合成到白底。
3. 手机竖拍 JPEG 靠 EXIF Orientation 标方向，重编码前必须先按它把像素摆正，
   否则发给网关的是横躺的商品图。
4. 什么都不用改的图**原样返回原始字节**——实拍 JPEG 重编码成无损 PNG 会膨胀
   3~8 倍，容易撞网关的请求体上限。

## 与老代码（legacy-python 分支）最关键的两处不同

**① fail-closed。** 老的 `prepare_input_image` 用一个大 try 兜住整个流程，
出任何异常就 `return 原始字节` + 打一行 warning。后果是：补边失败 →
原图什么比例出什么比例 → 运营选了 3:4 拿回 4:3，**钱照扣**，没人会发现。
这里**没有任何兜底 except**：算不出来就把异常抛上去，由 HTTP 层返 5xx，
Go 侧把该 item 置 failed。宁可这张不出，也不出错比例。

**② `parse_aspect` 只认 `'3:4'` 这种比例串。**
老的 `parse_ratio` 只认 `'1536x1024'` 这种像素串，传 `'3:4'` 时它 `int('3:4')`
抛 ValueError → 返回 None → **静默地什么都不补**。这里对认不出的值一律抛
`RatioError`（400），绝不静默放行。两个函数不要混用，新代码里也没有 `parse_ratio`。
"""

from __future__ import annotations

import io
import re
from dataclasses import dataclass, field
from math import gcd
from typing import List, Optional, Tuple

from PIL import Image, ImageOps, UnidentifiedImageError

from .errors import BadRequest, HeifUnsupported, PayloadTooLarge, RatioError, UnsupportedImage

# --------------------------------------------------------------------------
# HEIC / HEIF 解码
# --------------------------------------------------------------------------
try:  # pragma: no cover - 依赖装没装是部署问题，不是代码分支
    import pillow_heif

    pillow_heif.register_heif_opener()
    HEIF_AVAILABLE = True
    HEIF_VERSION = getattr(pillow_heif, "__version__", "unknown")
except Exception:  # noqa: BLE001 - 装不上就降级，但 /healthz 会如实报告
    HEIF_AVAILABLE = False
    HEIF_VERSION = None

try:  # Pillow 10 起 `PIL.Image.__version__` 已删除，版本号在包上
    from PIL import __version__ as PILLOW_VERSION
except ImportError:  # pragma: no cover
    PILLOW_VERSION = "unknown"

# --------------------------------------------------------------------------
# 常量
# --------------------------------------------------------------------------
PAD_COLOR = (255, 255, 255)
PAD_COLOR_TRANSPARENT = (255, 255, 255, 0)
JPEG_QUALITY = 92

#: 界面上开放给运营的五个比例（决策 B3-1）。仅用于 /healthz 展示，
#: 服务本身接受任意合法的 `W:H`——以后加比例是配置问题，不用改这里。
SUPPORTED_RATIOS = ("1:1", "3:4", "4:3", "16:9", "9:16")

#: 这三种格式网关认识，且「什么都不用改」时可以原样直传，不重编码。
PASSTHROUGH_FORMATS = frozenset({"png", "jpeg", "webp"})

#: 有损来源。改过像素之后仍然存成 JPEG，避免 PNG 把实拍图撑大 3~8 倍。
LOSSY_FORMATS = frozenset({"jpeg", "mpo", "webp", "heif", "heic", "avif"})

_MIME_BY_FORMAT = {
    "png": "image/png",
    "jpeg": "image/jpeg",
    "webp": "image/webp",
}
_SUFFIX_BY_FORMAT = {
    "png": ".png",
    "jpeg": ".jpg",
    "webp": ".webp",
}

# `3:4`、`16 : 9`，也接受中文全角冒号（运营从文档里复制粘贴时常见）。
# **故意不接受 `1536x1024`**：那是像素串，语义不同，宁可 400 也不要猜。
_ASPECT_RE = re.compile(r"^(\d{1,5})\s*[:：]\s*(\d{1,5})$")

# ISO BMFF 的 HEIF/AVIF 品牌标识，用来在解码失败时判断「这是不是一张 HEIC」。
_HEIF_BRANDS = frozenset(
    {
        b"heic", b"heix", b"heim", b"heis", b"hevc", b"hevx", b"hevm", b"hevs",
        b"mif1", b"msf1", b"miaf", b"avif", b"avis",
    }
)

# 动作码（ASCII，给程序看）→ 不带参数的中文说明（给人看）。
# 带参数的两条（padded / downscaled / transcoded）在下面现场拼。
ACTION_EXIF_ROTATED = "exif_rotated"
ACTION_ALPHA_FLATTENED = "alpha_flattened"
ACTION_ALPHA_KEPT = "alpha_kept"
ACTION_PADDED = "padded"
ACTION_DOWNSCALED = "downscaled"
ACTION_TRANSCODED = "transcoded"


@dataclass
class PreparedImage:
    """预处理结果。`data` 直接就是要发给网关的图片字节。"""

    data: bytes
    mime: str
    suffix: str
    width: int
    height: int
    source_width: int
    source_height: int
    source_format: str
    changed: bool
    action_codes: List[str] = field(default_factory=list)
    action_notes: List[str] = field(default_factory=list)


# --------------------------------------------------------------------------
# 参数解析
# --------------------------------------------------------------------------
def parse_aspect(value: Optional[str]) -> Optional[Tuple[int, int]]:
    """把 `'3:4'` 解析成最简整数比 `(3, 4)`。

    - `None` / 空串 / `'auto'` → 返回 `None`，表示**明确要求不补边**；
    - 其余认不出来的写法（包括老格式 `'1536x1024'`）→ 抛 `RatioError`（400）。

    为什么要抛而不是返回 None：静默不补边 = 运营选了 3:4 却拿回 4:3 且照样扣钱。
    """
    if value is None:
        return None
    text = str(value).strip()
    if text == "" or text.lower() in ("auto", "none", "null"):
        return None

    match = _ASPECT_RE.match(text)
    if not match:
        raise RatioError(
            "比例格式不对：%r。请用「宽:高」的写法，例如 %s。"
            "（注意不是 1536x1024 这种像素尺寸）" % (text, "、".join(SUPPORTED_RATIOS))
        )
    width = int(match.group(1))
    height = int(match.group(2))
    if width <= 0 or height <= 0:
        raise RatioError("比例的宽和高都必须大于 0，收到的是 %r。" % text)

    divisor = gcd(width, height)
    return width // divisor, height // divisor


def parse_bool(value: Optional[str], default: bool = False) -> bool:
    """表单里的布尔值。认不出来就抛 400，不猜。"""
    if value is None:
        return default
    text = str(value).strip().lower()
    if text == "":
        return default
    if text in ("1", "true", "t", "yes", "y", "on"):
        return True
    if text in ("0", "false", "f", "no", "n", "off"):
        return False
    raise BadRequest(
        "keep_transparency 只能填 true 或 false，收到的是 %r。" % value,
        code="invalid_keep_transparency",
    )


def parse_max_dimension(
    value: Optional[str], default: int, minimum: int, maximum: int
) -> int:
    """解析 `max_dimension`。

    ⚠ 这个参数**直接决定出图落 2K 还是 4K 计费档**（最长边 ≤1024→1K、
    ≤2048→2K、更大→4K），所以它必须是显式入参，不能埋成常量。
    """
    if value is None:
        return default
    text = str(value).strip()
    if text == "":
        return default
    try:
        number = int(text, 10)
    except ValueError:
        raise BadRequest(
            "max_dimension 必须是整数，收到的是 %r。" % value,
            code="invalid_max_dimension",
        ) from None
    if number < minimum or number > maximum:
        raise BadRequest(
            "max_dimension 必须在 %d~%d 之间，收到的是 %d。" % (minimum, maximum, number),
            code="invalid_max_dimension",
        )
    return number


# --------------------------------------------------------------------------
# 小工具
# --------------------------------------------------------------------------
def _ceil_div(numerator: int, denominator: int) -> int:
    return -(-numerator // denominator)


def _looks_like_heif(data: bytes) -> bool:
    """看文件头判断是不是 HEIC/HEIF/AVIF。

    ISO BMFF 的结构是：4 字节 box 长度 + `ftyp` + 4 字节 major brand。
    """
    if len(data) < 12 or data[4:8] != b"ftyp":
        return False
    if data[8:12] in _HEIF_BRANDS:
        return True
    # major brand 可能是 `mp42` 之类，真正的标识在后面的 compatible brands 里
    head = data[:64]
    return any(brand in head for brand in _HEIF_BRANDS)


def _exif_orientation(image: Image.Image) -> int:
    """读 EXIF Orientation（0x0112）。读不到或不合法一律当作 1（不需要旋转）。"""
    try:
        exif = image.getexif()
    except Exception:  # noqa: BLE001 - 坏 EXIF 不该拖垮整张图
        return 1
    if not exif:
        return 1
    try:
        value = int(exif.get(0x0112, 1))
    except (TypeError, ValueError):
        return 1
    return value if 1 <= value <= 8 else 1


def _has_alpha(image: Image.Image) -> bool:
    if image.mode in ("RGBA", "LA", "PA"):
        return True
    return "transparency" in image.info


def _flatten_alpha(image: Image.Image) -> Image.Image:
    """把透明通道合成到白底，返回 RGB。没有透明通道就单纯转 RGB。"""
    if _has_alpha(image):
        rgba = image.convert("RGBA")
        background = Image.new("RGB", rgba.size, PAD_COLOR)
        background.paste(rgba, mask=rgba.split()[-1])
        return background
    return image if image.mode == "RGB" else image.convert("RGB")


def open_image(data: bytes, max_pixels: int) -> Image.Image:
    """解码入参字节。失败一律抛我们自己的异常，绝不返回 None。"""
    if not data:
        raise BadRequest("上传的文件是空的。", code="empty_file")

    try:
        image = Image.open(io.BytesIO(data))
    except Image.DecompressionBombError as exc:
        raise PayloadTooLarge("图片像素数过大，服务拒绝解码。", code="image_too_large") from exc
    except UnidentifiedImageError as exc:
        if _looks_like_heif(data) and not HEIF_AVAILABLE:
            raise HeifUnsupported(
                "这是一张 HEIC/HEIF 照片，但服务端没有装 HEIC 解码器（pillow-heif），无法处理。"
            ) from exc
        raise UnsupportedImage(
            "认不出这个文件的图片格式。支持 JPG / PNG / WEBP / HEIC 等常见格式。"
        ) from exc
    except (OSError, ValueError) as exc:
        raise UnsupportedImage("图片文件损坏或不完整，无法解码。") from exc

    width, height = image.size
    if width <= 0 or height <= 0:
        image.close()
        raise UnsupportedImage("图片尺寸不合法（宽或高为 0）。")
    if width * height > max_pixels:
        image.close()
        raise PayloadTooLarge(
            "图片像素数 %d×%d 超过上限 %d，服务拒绝解码。" % (width, height, max_pixels),
            code="image_too_large",
        )

    try:
        image.load()
    except Image.DecompressionBombError as exc:
        image.close()
        raise PayloadTooLarge("图片像素数过大，服务拒绝解码。", code="image_too_large") from exc
    except (OSError, ValueError) as exc:
        image.close()
        if _looks_like_heif(data) and not HEIF_AVAILABLE:
            raise HeifUnsupported(
                "这是一张 HEIC/HEIF 照片，但服务端没有装 HEIC 解码器（pillow-heif），无法处理。"
            ) from exc
        raise UnsupportedImage("图片文件损坏或不完整，无法解码。") from exc

    return image


# --------------------------------------------------------------------------
# 补边画布的尺寸计算
# --------------------------------------------------------------------------
def plan_canvas(
    width: int, height: int, aspect: Tuple[int, int], max_dimension: int
) -> Tuple[int, int, bool, bool]:
    """算出补边后的画布尺寸。

    返回 `(画布宽, 画布高, 是否需要补边, 是否需要缩小)`。

    做法：把目标比例约到最简整数比 `a:b`，画布尺寸只取 `(k*a, k*b)` 这种形式，
    这样**出来的比例是精确的整数比**，不会有「1000×1333，其实是 0.75019 不是 0.75」
    这种舍入残差。

    - `k_fit = max(ceil(宽/a), ceil(高/b))` —— 刚好装得下原图的最小 k（纯补边不缩图）
    - `k_cap = max_dimension // max(a, b)` —— 最长边不超过 max_dimension 的最大 k
    - `k = min(k_fit, k_cap)`；`k < k_fit` 就意味着装不下，得先把原图缩小

    因为 k 被 `k_cap` 卡死，**画布面积天然有上界**，不存在「传个 1:10000
    把内存撑爆」的问题。
    """
    a, b = aspect
    longest = max(a, b)
    if longest > max_dimension:
        raise RatioError(
            "比例 %d:%d 过于极端（约简后最长边 %d 已经超过 max_dimension=%d），无法处理。"
            % (a, b, longest, max_dimension)
        )

    k_fit = max(_ceil_div(width, a), _ceil_div(height, b))
    k_cap = max_dimension // longest
    k = min(k_fit, k_cap)
    if k < 1:
        k = 1

    canvas_w, canvas_h = k * a, k * b
    # a、b 互质，所以「原图已经正好是这个比例」等价于 a | 宽 且 b | 高。
    already_exact = width * b == height * a
    return canvas_w, canvas_h, (not already_exact), (k < k_fit)


# --------------------------------------------------------------------------
# 主流程
# --------------------------------------------------------------------------
def preprocess(
    data: bytes,
    aspect: Optional[Tuple[int, int]],
    keep_transparency: bool,
    max_dimension: int,
    max_pixels: int,
) -> PreparedImage:
    """预处理一张输入图。

    **这个函数里没有兜底 except**。任何算不出来的情况都会把异常抛给调用方，
    HTTP 层据此返 4xx/5xx，**绝不回吐原图**。
    """
    image = open_image(data, max_pixels)
    try:
        source_format = (image.format or "").lower()

        # ---- 第一步：EXIF 摆正 -------------------------------------------
        orientation = _exif_orientation(image)
        oriented = ImageOps.exif_transpose(image)
        if oriented is None:  # pragma: no cover - 只有 in_place=True 才会是 None
            oriented = image
        rotated = orientation != 1
        width, height = oriented.size

        had_alpha = _has_alpha(oriented)
        keep_alpha = bool(keep_transparency) and had_alpha
        need_flatten = had_alpha and not keep_alpha

        # ---- 第二步：先只算「要做哪些事」，还不碰像素 ---------------------
        codes: List[str] = []
        notes: List[str] = []

        if rotated:
            codes.append(ACTION_EXIF_ROTATED)
            notes.append("已按拍摄方向摆正")
        if need_flatten:
            codes.append(ACTION_ALPHA_FLATTENED)
            notes.append("透明底已合成白底")

        canvas_w = canvas_h = None
        need_pad = False
        need_scale = False
        if aspect is not None:
            canvas_w, canvas_h, need_pad, need_scale = plan_canvas(
                width, height, aspect, max_dimension
            )
            if need_pad:
                codes.append(ACTION_PADDED)
                notes.append(
                    "已补%s边到 %d:%d 比例" % ("透明" if keep_alpha else "白", aspect[0], aspect[1])
                )
            if need_scale:
                codes.append(ACTION_DOWNSCALED)
                notes.append("已缩小到最长边 %d" % max(canvas_w, canvas_h))
        else:
            need_scale = max(width, height) > max_dimension
            if need_scale:
                codes.append(ACTION_DOWNSCALED)
                notes.append("已缩小到最长边 %d" % max_dimension)

        if source_format not in PASSTHROUGH_FORMATS:
            codes.append(ACTION_TRANSCODED)
            notes.append("已从 %s 转码" % (source_format or "未知格式"))

        # ---- 第三步：一件都不用做 → 原样直传原始字节 ----------------------
        if not codes:
            mime = _MIME_BY_FORMAT[source_format]
            suffix = _SUFFIX_BY_FORMAT[source_format]
            extra_codes: List[str] = []
            extra_notes: List[str] = []
            if keep_alpha:
                extra_codes.append(ACTION_ALPHA_KEPT)
                extra_notes.append("已保留透明底")
            return PreparedImage(
                data=data,
                mime=mime,
                suffix=suffix,
                width=width,
                height=height,
                source_width=width,
                source_height=height,
                source_format=source_format,
                changed=False,
                action_codes=extra_codes,
                action_notes=extra_notes,
            )

        # ---- 第四步：真的动像素 -------------------------------------------
        if keep_alpha:
            canvas_mode = "RGBA"
            fill = PAD_COLOR_TRANSPARENT
            base = oriented if oriented.mode == "RGBA" else oriented.convert("RGBA")
            codes.insert(0, ACTION_ALPHA_KEPT)
            notes.insert(0, "已保留透明底")
        else:
            canvas_mode = "RGB"
            fill = PAD_COLOR
            base = _flatten_alpha(oriented)

        if aspect is not None:
            assert canvas_w is not None and canvas_h is not None
            if need_scale:
                scale = min(canvas_w / base.width, canvas_h / base.height)
                new_w = max(1, min(canvas_w, round(base.width * scale)))
                new_h = max(1, min(canvas_h, round(base.height * scale)))
                if (new_w, new_h) != base.size:
                    base = base.resize((new_w, new_h), Image.LANCZOS)
            if base.size != (canvas_w, canvas_h):
                canvas = Image.new(canvas_mode, (canvas_w, canvas_h), fill)
                canvas.paste(base, ((canvas_w - base.width) // 2, (canvas_h - base.height) // 2))
                base = canvas
        elif need_scale:
            base = base.copy()
            base.thumbnail((max_dimension, max_dimension), Image.LANCZOS)

        # ---- 第五步：编码 --------------------------------------------------
        buffer = io.BytesIO()
        if keep_alpha:
            # 有透明通道就只能存 PNG（JPEG 没有 alpha）
            base.save(buffer, "PNG")
            out_format = "png"
        elif source_format in LOSSY_FORMATS:
            # 实拍图继续存 JPEG：无损 PNG 会膨胀 3~8 倍，容易撞网关请求体上限
            base.save(buffer, "JPEG", quality=JPEG_QUALITY, optimize=True)
            out_format = "jpeg"
        else:
            base.save(buffer, "PNG")
            out_format = "png"

        payload = buffer.getvalue()
        if not payload:  # pragma: no cover - 编码器不该产出空字节
            raise RuntimeError("图片编码后为空字节")

        return PreparedImage(
            data=payload,
            mime=_MIME_BY_FORMAT[out_format],
            suffix=_SUFFIX_BY_FORMAT[out_format],
            width=base.width,
            height=base.height,
            source_width=width,
            source_height=height,
            source_format=source_format,
            changed=True,
            action_codes=codes,
            action_notes=notes,
        )
    finally:
        image.close()
