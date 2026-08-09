"""文件存储：上传的商品图、生成结果、缩略图。

所有路径都以相对 data 目录的形式存进数据库（如 uploads/202608/xx.png），
对外通过 /files/<相对路径> 访问，换服务器/换磁盘时数据可整体搬迁。
"""
import io
import uuid
from datetime import datetime
from pathlib import Path
from typing import Optional, Tuple

from PIL import Image

from ..config import DATA_DIR

try:  # Mac 相册导出的 HEIC（常伪装成 .jpg）：装了 pillow-heif 即可直接识别
    from pillow_heif import register_heif_opener

    register_heif_opener()
except ImportError:  # pragma: no cover
    pass

THUMB_MAX = 480


class StorageError(Exception):
    pass


def _month_dir() -> str:
    return datetime.utcnow().strftime("%Y%m")


def abs_path(rel_path: str) -> Path:
    p = (DATA_DIR / rel_path).resolve()
    if not str(p).startswith(str(DATA_DIR.resolve())):
        raise StorageError("非法路径")
    return p


def _looks_like_heif(data: bytes) -> bool:
    # ISO BMFF：前 4 字节为 box 长度，随后是 'ftyp' + 品牌（heic/heix/mif1…）
    return len(data) > 12 and data[4:8] == b"ftyp" and data[8:12] in (
        b"heic", b"heix", b"hevc", b"heim", b"heis", b"mif1", b"msf1",
    )


def validate_image(data: bytes) -> Tuple[int, int, str]:
    """校验是不是真图片，返回 (宽, 高, 格式)。"""
    try:
        with Image.open(io.BytesIO(data)) as im:
            im.verify()
        with Image.open(io.BytesIO(data)) as im:
            return im.width, im.height, (im.format or "PNG").lower()
    except Exception:
        if _looks_like_heif(data):
            raise StorageError(
                "这是 iPhone/Mac 的 HEIC 照片，服务器缺少解码组件："
                "请运行 pip install pillow-heif 后重启，或先把照片导出为 JPG"
            )
        raise StorageError("文件不是有效的图片")


def save_upload(data: bytes, suffix: str) -> Tuple[str, int, int]:
    """保存上传的商品图，返回 (相对路径, 宽, 高)。"""
    width, height, _fmt = validate_image(data)
    rel = "uploads/%s/%s%s" % (_month_dir(), uuid.uuid4().hex, suffix)
    target = abs_path(rel)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(data)
    return rel, width, height


def save_output(job_id: str, index: int, data: bytes) -> Tuple[str, Optional[str], int, int, str]:
    """保存一张生成结果 + 生成缩略图。返回 (路径, 缩略图路径, 宽, 高, 格式)。"""
    width, height, fmt = validate_image(data)
    ext = ".png" if fmt == "png" else ".jpg" if fmt in ("jpeg", "jpg") else ".webp"
    rel = "outputs/%s/%s/%d%s" % (_month_dir(), job_id, index, ext)
    target = abs_path(rel)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(data)

    thumb_rel: Optional[str] = None
    try:
        with Image.open(io.BytesIO(data)) as im:
            im = im.convert("RGB")
            im.thumbnail((THUMB_MAX, THUMB_MAX))
            thumb_rel = "thumbnails/%s/%s_%d.jpg" % (_month_dir(), job_id, index)
            thumb_abs = abs_path(thumb_rel)
            thumb_abs.parent.mkdir(parents=True, exist_ok=True)
            im.save(thumb_abs, "JPEG", quality=85)
    except Exception:
        thumb_rel = None  # 缩略图失败不影响主流程

    return rel, thumb_rel, width, height, fmt.lstrip(".")


def delete_file(rel_path: Optional[str]) -> None:
    if not rel_path:
        return
    try:
        p = abs_path(rel_path)
        if p.is_file():
            p.unlink()
    except Exception:
        pass  # 清理失败不阻塞业务


def to_url(rel_path: Optional[str], public_base_url: str) -> Optional[str]:
    if not rel_path:
        return None
    # 灵感库条目的缩略图直接引用来源方 CDN 的绝对地址
    if rel_path.startswith("http://") or rel_path.startswith("https://"):
        return rel_path
    return public_base_url.rstrip("/") + "/files/" + rel_path
