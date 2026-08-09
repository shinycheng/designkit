"""发送给生图接口前的输入图预处理。

实战背景（来自自建 Sub2API 网关的踩坑记录）：
1. 该类网关**忽略 size 参数，输出比例完全跟随输入图比例**——想要 1:1 输出，
   必须先把输入图变成 1:1。用「补边 pad」而不是裁切，产品永远完整。
   对遵循 size 的官方/中转网关同样安全：输入与请求的比例一致，不会形变。
2. Mac 导出的照片常是 HEIC 却顶着 .jpg 扩展名，不装解码器直接报「无法识别」。
3. 带透明底的 PNG 直接转 RGB 会把透明区变成黑块，必须先合成到白底。
4. 手机竖拍 JPEG 靠 EXIF Orientation 标方向，重编码前必须先按它把像素摆正，
   否则发给网关的是横躺的商品图。
5. 什么都不用改的图**原样返回原始字节**——实拍 JPEG 重编码成无损 PNG 会膨胀
   3~8 倍，容易撞网关的请求体上限；确实动过像素的 JPEG 也按 JPEG 保存。
"""
import io
import logging
from dataclasses import dataclass
from typing import Optional, Tuple

from PIL import Image, ImageOps

logger = logging.getLogger("designkit.imaging")

try:  # HEIC/HEIF 解码：装了 pillow-heif 就自动支持，没装则优雅降级
    from pillow_heif import register_heif_opener

    register_heif_opener()
    HEIF_SUPPORTED = True
except ImportError:  # pragma: no cover - 依赖未安装时的兜底
    HEIF_SUPPORTED = False

# 补边后的最长边上限：防止 pad 之后图片过大拖慢上传与生图
MAX_DIMENSION = 2048
PAD_COLOR = (255, 255, 255)
JPEG_QUALITY = 92

_MIME_BY_SUFFIX = {
    ".png": "image/png",
    ".jpg": "image/jpeg",
    ".jpeg": "image/jpeg",
    ".webp": "image/webp",
    ".heic": "image/heic",
    ".heif": "image/heif",
}


@dataclass
class PreparedImage:
    data: bytes
    suffix: str      # 含点，如 ".png"
    mime: str
    note: str


def parse_ratio(size: str) -> Optional[Tuple[int, int]]:
    """把 '1536x1024' 解析成 (宽, 高)；'auto' 或非法值返回 None（不做比例处理）。"""
    try:
        w_str, h_str = str(size).lower().split("x")
        w, h = int(w_str), int(h_str)
        if w > 0 and h > 0:
            return w, h
    except (ValueError, AttributeError):
        pass
    return None


def _has_alpha(im: Image.Image) -> bool:
    return im.mode in ("RGBA", "LA", "PA") or (im.mode == "P" and "transparency" in im.info)


def flatten_alpha(im: Image.Image) -> Image.Image:
    """把透明通道合成到白底（透明区直接转 RGB 会变黑块）。"""
    if _has_alpha(im):
        rgba = im.convert("RGBA")
        background = Image.new("RGB", rgba.size, PAD_COLOR)
        background.paste(rgba, mask=rgba.split()[-1])
        return background
    return im.convert("RGB") if im.mode != "RGB" else im


def pad_to_ratio(im: Image.Image, ratio: Tuple[int, int]) -> Image.Image:
    """居中补白边到目标宽高比，不裁切、不缩放产品本身。"""
    target_w, target_h = ratio
    width, height = im.size
    current = width / height
    target = target_w / target_h
    if abs(current - target) < 0.01:
        return im
    if current < target:  # 图太窄 → 左右补边
        new_width = round(height * target)
        canvas = Image.new("RGB", (new_width, height), PAD_COLOR)
        canvas.paste(im, ((new_width - width) // 2, 0))
    else:  # 图太宽 → 上下补边
        new_height = round(width / target)
        canvas = Image.new("RGB", (width, new_height), PAD_COLOR)
        canvas.paste(im, (0, (new_height - height) // 2))
    return canvas


def _fallback(data: bytes, original_suffix: str, note: str) -> PreparedImage:
    suffix = original_suffix if original_suffix in _MIME_BY_SUFFIX else ".png"
    return PreparedImage(data, suffix, _MIME_BY_SUFFIX.get(suffix, "image/png"), note)


def prepare_input_image(
    data: bytes, size: str, normalize_ratio: bool, original_suffix: str = ".png"
) -> PreparedImage:
    """预处理一张输入图。

    - 没有任何需要改的（方向正、无透明、比例合、尺寸合、格式网关认识）→
      **原样返回原始字节**，不重编码；
    - 动过像素时：原图是 JPEG 存 JPEG（quality 92），否则存 PNG；
    - 任一步失败都回退原始字节——预处理只应锦上添花，不能挡住出图。
    """
    original_suffix = original_suffix.lower()
    try:
        with Image.open(io.BytesIO(data)) as im:
            im.load()
            source_format = (im.format or "").lower()
            notes = []

            oriented = ImageOps.exif_transpose(im)  # 手机竖拍靠 EXIF 标方向，先摆正像素
            if oriented.size != im.size or oriented is not im:
                if oriented.size != im.size:
                    notes.append("已按拍摄方向摆正")
            had_alpha = _has_alpha(oriented)
            flattened = flatten_alpha(oriented)
            if had_alpha:
                notes.append("透明底已合成白底")

            ratio = parse_ratio(size) if normalize_ratio else None
            if ratio is not None:
                before = flattened.size
                flattened = pad_to_ratio(flattened, ratio)
                if flattened.size != before:
                    notes.append("已按 %s 比例补边" % size)

            if max(flattened.size) > MAX_DIMENSION:
                flattened.thumbnail((MAX_DIMENSION, MAX_DIMENSION), Image.LANCZOS)
                notes.append("已缩小到最长边 %d" % MAX_DIMENSION)

            gateway_friendly = source_format in ("png", "jpeg", "webp")
            if not gateway_friendly:
                notes.append("已从 %s 转码" % (source_format or "未知格式"))

            if not notes and gateway_friendly:
                # 原样直传：不重编码就不会膨胀体积、不丢任何信息
                suffix = ".png" if source_format == "png" else ".jpg" if source_format == "jpeg" else ".webp"
                return PreparedImage(data, suffix, _MIME_BY_SUFFIX[suffix], "无需处理")

            buf = io.BytesIO()
            # 实拍图（原为 JPEG）继续存 JPEG：无损 PNG 会膨胀 3~8 倍，易撞网关请求体上限
            if source_format == "jpeg":
                flattened.save(buf, "JPEG", quality=JPEG_QUALITY)
                return PreparedImage(buf.getvalue(), ".jpg", "image/jpeg", "；".join(notes))
            flattened.save(buf, "PNG")
            return PreparedImage(buf.getvalue(), ".png", "image/png", "；".join(notes))
    except Exception as exc:  # 预处理失败不阻断出图
        logger.warning("输入图预处理失败，按原图发送：%s", exc)
        return _fallback(data, original_suffix, "预处理失败，已按原图发送")
