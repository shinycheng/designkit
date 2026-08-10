"""文件存储：上传的商品图、生成结果、缩略图。

所有路径都以相对 data 目录的形式存进数据库（如 uploads/202608/xx.png），
对外通过 /files/<相对路径> 访问，换服务器/换磁盘时数据可整体搬迁。
"""
import io
import time
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

# 数据根目录只在导入时解析一次。
# resolve() 会真的去问文件系统（逐段解符号链接），而 abs_path 是热路径——
# 每存一张图、每建一个缩略图、每删一个文件都要走一遍，没必要每次都解析。
_DATA_ROOT = DATA_DIR.resolve()


class StorageError(Exception):
    pass


def _month_dir() -> str:
    return datetime.utcnow().strftime("%Y%m")


def abs_path(rel_path: str) -> Path:
    """把「相对 data 目录的路径」拼成绝对路径，并确认它没跑到 data 目录外面。

    ⚠️ 这里**不是**防路径穿越的地方，别指望它。
    真正拦 `../` 的是取文件路由（routers/files.py）里那三条严格白名单正则：
    穿越构造出来的目标文件本来就落在 data 目录之内（比如
    `outputs/202608/<自己的任务>/../../202607/<别人的任务>/0.png`），
    所以任何「在不在 data 目录里」的检查——不管是老的字符串前缀还是现在的
    relative_to——都判它合法。放行穿越的根源是「切任务号用的字符串」和
    「最终打开的文件」是两个东西，那个问题只能在路由层用正则解决。

    这次改动只是修掉一个潜伏的边界错误：
    老写法 `str(p).startswith(str(DATA_DIR.resolve()))` 是纯字符串前缀比较，
    不认目录分隔符。DATA_DIR 是 `/vol1/designkit/data` 时，
    `/vol1/designkit/data-backup/x.png` 也满足前缀，会被当成「在数据目录内」。
    今天所有入参都是服务端自己生成的（uuid + 固定后缀），不可利用；
    但 delete_file 是会真的 unlink 的，将来任何一处把用户输入接进路径参数，
    这就直接变成任意文件删除。
    relative_to() 按**路径段**比较，data-backup 会老老实实抛 ValueError。
    """
    p = (DATA_DIR / rel_path).resolve()
    try:
        p.relative_to(_DATA_ROOT)
    except ValueError:
        # from None：relative_to 抛的 ValueError 没有任何额外信息，
        # 让它跟在后面只会在日志里多出一段「During handling of…」的噪音
        raise StorageError("非法路径") from None
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
            # 透明底的图直接 convert("RGB")，透明区会变成黑块——列表里看上去像出错了。
            # 缩略图仍存 JPEG（体积小、加载快），所以这里先铺一层白再合成。
            if im.mode in ("RGBA", "LA", "PA") or (im.mode == "P" and "transparency" in im.info):
                rgba = im.convert("RGBA")
                canvas = Image.new("RGB", rgba.size, (255, 255, 255))
                canvas.paste(rgba, mask=rgba.split()[-1])
                im = canvas
            else:
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


def _ttl_seconds(ttl_hours) -> int:
    """把设置里的「有效小时数」换算成秒，顺手兜住笔误。

    设成 0 或负数没有任何合理含义（链接一发出去就已经过期），
    真出现了就当 1 小时处理——总比让 ERP 那边所有图片链接集体打不开强，
    那种故障对方只会看到「你们的图挂了」，根本追不到是设置页填错了一个数。
    """
    try:
        hours = int(ttl_hours)
    except (TypeError, ValueError):
        raise StorageError("图片链接有效期设置不是数字：%r" % (ttl_hours,)) from None
    return max(hours, 1) * 3600


def to_url(
    rel_path: Optional[str],
    public_base_url: str,
    *,
    sign: Optional[Tuple[int, int]] = None,
) -> Optional[str]:
    """把库里存的相对路径拼成对外能访问的链接。

    sign 传 `(api_key_id, ttl_hours)` 时，在链接后面额外拼上给 ERP 用的签名参数
    `?k=<api_key_id>&e=<过期时间>&t=<签名>`；`e` 是 UTC 纪元秒（和 JWT 的 exp 同一套算法，
    校验方 services/file_access.py 按同样口径比对）。签名同时绑定了路径、有效期和
    这把 Key 的 id——所以停用一把 Key，它此前发出去的所有图片链接会立刻失效，
    不用等有效期自然过完。

    sign 不传（默认）时行为和以前**逐字一致**，一个字符都不多。
    默认值是刻意留的：网页端那几个调用点因此一行都不用改，改动的爆炸半径压到最小。

    **为什么不拿 public_base_url 是不是空来自动判断该不该签名**：
    public_base_url 是管理员在设置页能随手清空的（对照 provider / default_size 那些项
    都有校验，唯独它没有）。一旦清空，ERP 拿到的链接会同时变成相对路径**且**不带签名，
    整个过程不报任何错——正好是最难查的那种故障。所以签不签名只认调用方显式传的 sign。
    """
    if not rel_path:
        return None
    # 灵感库条目的缩略图直接引用来源方 CDN 的绝对地址。
    # 这类图不是本机文件，走不到我们的取图路由，既签不了名也不需要签。
    if rel_path.startswith("http://") or rel_path.startswith("https://"):
        return rel_path
    url = public_base_url.rstrip("/") + "/files/" + rel_path
    if sign is None:
        return url

    api_key_id, ttl_hours = sign
    if not api_key_id:
        # 宁可当场报错，也不能悄悄发一条没签名的链接出去。
        # 调用方（v1.py / worker.py）自己判断「这是不是 ERP 任务」，
        # 不是 ERP 任务就该传 sign=None，而不是传一个空的 api_key_id 进来。
        raise StorageError("要签名的图片链接必须带 api_key_id")

    # 延迟导入：storage 是底层模块（provider、prompt_studio、serializers 都依赖它），
    # 而 file_access 属于上层的访问控制。放在函数里导，既不让底层反向依赖上层，
    # 也让「压根没用到签名链接」的调用路径完全不受它影响。
    from .file_access import append_signature

    # 参数名（k/e/t）和拼接方式统一由 file_access 说了算——校验那一侧也在同一个文件里，
    # 两边永远不会写岔。这里只负责算出到期时间。
    exp = int(time.time()) + _ttl_seconds(ttl_hours)
    return append_signature(url, rel_path, int(api_key_id), exp)
