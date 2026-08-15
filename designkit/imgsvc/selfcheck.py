#!/usr/bin/env python3
"""imgsvc 自检脚本 —— 用真实图片跑一遍全部关键行为。

**不需要 pytest**，也不联网、不花钱。分两段：

* 「本地」段：直接 import `app.imaging`，重点验 fail-closed 和比例计算；
* 「HTTP」段：打一个真在跑的服务，验响应头、错误码、原样直传。

用法：

    # 1) 起服务
    uvicorn app.main:app --host 127.0.0.1 --port 8099

    # 2) 另开一个终端
    DK_SELFCHECK_BASE=http://127.0.0.1:8099 python3 selfcheck.py

    # 只跑不需要服务的那一段
    python3 selfcheck.py --local-only

带 token 时，把服务和脚本用同一个值启动：

    DESIGNKIT_IMGSVC_TOKEN=secret uvicorn app.main:app --port 8099
    DESIGNKIT_IMGSVC_TOKEN=secret DK_SELFCHECK_BASE=http://127.0.0.1:8099 python3 selfcheck.py
"""

from __future__ import annotations

import io
import json
import os
import sys
import urllib.error
import urllib.request
import uuid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from PIL import Image  # noqa: E402

from app import imaging  # noqa: E402
from app.errors import ImgSvcError  # noqa: E402

BASE = os.getenv("DK_SELFCHECK_BASE", "http://127.0.0.1:8099").rstrip("/")
TOKEN = os.getenv("DESIGNKIT_IMGSVC_TOKEN", "").strip()

RATIOS = ["1:1", "3:4", "4:3", "16:9", "9:16"]

_passed = 0
_failed: list = []


def check(name: str, condition: bool, detail: str = "") -> None:
    global _passed
    if condition:
        _passed += 1
        print("  ✓ %s" % name)
    else:
        _failed.append(name)
        print("  ✗ %s   %s" % (name, detail))


# --------------------------------------------------------------------------
# 造图
# --------------------------------------------------------------------------
def make_jpeg(width: int, height: int, orientation: int = 0) -> bytes:
    """一张有内容的实拍风格 JPEG（纯色会被 JPEG 压到几百字节，不够真实）。"""
    image = Image.new("RGB", (width, height))
    pixels = image.load()
    for y in range(height):
        for x in range(0, width, 4):
            value = ((x * 7 + y * 3) % 256, (x * 3) % 256, (y * 5) % 256)
            for dx in range(min(4, width - x)):
                pixels[x + dx, y] = value
    buffer = io.BytesIO()
    if orientation:
        exif = Image.Exif()
        exif[0x0112] = orientation
        image.save(buffer, "JPEG", quality=90, exif=exif)
    else:
        image.save(buffer, "JPEG", quality=90)
    return buffer.getvalue()


def make_png_with_alpha(width: int, height: int) -> bytes:
    """四周透明、中间一块不透明红色的 PNG。"""
    image = Image.new("RGBA", (width, height), (255, 255, 255, 0))
    inner = Image.new("RGBA", (width // 2, height // 2), (220, 30, 30, 255))
    image.paste(inner, (width // 4, height // 4))
    buffer = io.BytesIO()
    image.save(buffer, "PNG")
    return buffer.getvalue()


def make_fake_heic() -> bytes:
    """一个 ftyp 头写着 heic、但内容是垃圾的文件。

    用来验证「认出来是 HEIC 但解不了」这条分支。装了 pillow-heif 时它会
    解码失败（415），没装时会走到 422 heif_unsupported。
    """
    return b"\x00\x00\x00\x18ftypheic\x00\x00\x00\x00heicmif1" + b"\x00" * 512


# --------------------------------------------------------------------------
# 第一段：本地（不需要服务）
# --------------------------------------------------------------------------
def run_local() -> None:
    print("\n[1] parse_aspect —— 老格式必须报错，不许静默不补边")
    check("'3:4' → (3,4)", imaging.parse_aspect("3:4") == (3, 4))
    check("'16:9' → (16,9)", imaging.parse_aspect("16:9") == (16, 9))
    check("'1920:1080' 约简成 (16,9)", imaging.parse_aspect("1920:1080") == (16, 9))
    check("全角冒号 '3：4' 也认", imaging.parse_aspect("3：4") == (3, 4))
    check("空串 → None（明确不补边）", imaging.parse_aspect("") is None)
    check("'auto' → None", imaging.parse_aspect("auto") is None)
    for bad in ("1536x1024", "3/4", "abc", "3:", "0:4", "3:0"):
        try:
            imaging.parse_aspect(bad)
            check("%r 必须抛 RatioError" % bad, False, "居然通过了")
        except ImgSvcError as exc:
            check("%r → 400 %s" % (bad, exc.code), exc.status_code == 400)

    print("\n[2] plan_canvas —— 五个比例的画布尺寸必须是精确整数比")
    for ratio in RATIOS:
        aspect = imaging.parse_aspect(ratio)
        cw, ch, padded, scaled = imaging.plan_canvas(1200, 800, aspect, 2048)
        exact = cw * aspect[1] == ch * aspect[0]
        check(
            "1200x800 → %s 得 %dx%d（补边=%s 缩小=%s）" % (ratio, cw, ch, padded, scaled),
            exact and max(cw, ch) <= 2048 and cw >= 1 and ch >= 1,
            "比例不精确或超过 max_dimension",
        )

    print("\n[3] fail-closed —— 内部异常必须抛出去，绝不返回原图字节")
    original = Image.new
    payload = make_jpeg(600, 400)

    def boom(*args, **kwargs):
        raise MemoryError("模拟补边时内存不够")

    Image.new = boom
    try:
        imaging.preprocess(
            data=payload,
            aspect=(1, 1),
            keep_transparency=False,
            max_dimension=2048,
            max_pixels=80_000_000,
        )
        check("补边失败必须抛异常", False, "居然返回了结果 —— 这就是老代码的 fail-open")
    except MemoryError:
        check("补边失败抛异常（没有吞掉、没有回吐原图）", True)
    except Exception as exc:  # noqa: BLE001
        check("补边失败抛异常", False, "抛的却是 %r" % exc)
    finally:
        Image.new = original

    print("\n[4] 极端比例必须被拒，不能把内存撑爆")
    try:
        imaging.plan_canvas(2000, 2000, (1, 10000), 2048)
        check("1:10000 必须被拒", False, "居然通过了")
    except ImgSvcError as exc:
        check("1:10000 → 400 %s" % exc.code, exc.status_code == 400)


# --------------------------------------------------------------------------
# 第二段：HTTP
# --------------------------------------------------------------------------
def post(
    payload: bytes,
    ratio: str = "",
    keep_transparency: str = "",
    max_dimension: str = "",
    filename: str = "photo.jpg",
    token: str = "",
    omit_file: bool = False,
):
    """返回 (status, headers, body_bytes)。

    headers 是 `http.client.HTTPMessage`，取值**大小写不敏感**——
    别改成 `dict(...)`，那样 `X-Dk-Width` 会取不到（服务端发出来是小写的）。
    """
    boundary = "----dkselfcheck" + uuid.uuid4().hex
    chunks = []
    fields = {}
    if ratio:
        fields["ratio"] = ratio
    if keep_transparency:
        fields["keep_transparency"] = keep_transparency
    if max_dimension:
        fields["max_dimension"] = max_dimension
    for key, value in fields.items():
        chunks.append(
            ('--%s\r\nContent-Disposition: form-data; name="%s"\r\n\r\n%s\r\n' % (boundary, key, value)).encode()
        )
    if not omit_file:
        chunks.append(
            (
                '--%s\r\nContent-Disposition: form-data; name="file"; filename="%s"\r\n'
                "Content-Type: application/octet-stream\r\n\r\n" % (boundary, filename)
            ).encode()
        )
        chunks.append(payload)
        chunks.append(b"\r\n")
    chunks.append(("--%s--\r\n" % boundary).encode())
    body = b"".join(chunks)

    request = urllib.request.Request(BASE + "/v1/preprocess", data=body, method="POST")
    request.add_header("Content-Type", "multipart/form-data; boundary=" + boundary)
    effective = token if token else TOKEN
    if effective and token != "-":
        request.add_header("Authorization", "Bearer " + effective)
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            return response.status, response.headers, response.read()
    except urllib.error.HTTPError as exc:
        return exc.code, exc.headers, exc.read()


def open_result(body: bytes) -> Image.Image:
    image = Image.open(io.BytesIO(body))
    image.load()
    return image


def run_http() -> None:
    print("\n[5] /healthz")
    try:
        with urllib.request.urlopen(BASE + "/healthz", timeout=10) as response:
            health = json.loads(response.read())
    except Exception as exc:  # noqa: BLE001
        check("能连上 %s" % BASE, False, repr(exc))
        return
    check("status 字段存在", health.get("status") in ("ok", "degraded"), str(health))
    check("报告 pillow 可用性", health.get("pillow", {}).get("available") is True)
    check("报告 pillow-heif 可用性", "available" in health.get("pillow_heif", {}))
    print("      pillow=%s pillow-heif=%s(%s) 默认 max_dimension=%s" % (
        health["pillow"]["version"],
        health["pillow_heif"]["available"],
        health["pillow_heif"]["version"],
        health["defaults"]["max_dimension"],
    ))
    heif_available = bool(health["pillow_heif"]["available"])

    print("\n[6] 五个比例的补边结果（源图 1200x800 JPEG，max_dimension=2048）")
    source = make_jpeg(1200, 800)
    expected = {
        "1:1": (1200, 1200),
        "3:4": (1200, 1600),
        "4:3": (1200, 900),
        "16:9": (1424, 801),
        "9:16": (1152, 2048),
    }
    for ratio in RATIOS:
        status, headers, body = post(source, ratio=ratio, max_dimension="2048")
        if status != 200:
            check("%s → 200" % ratio, False, "得到 %s %s" % (status, body[:200]))
            continue
        image = open_result(body)
        a, b = (int(x) for x in ratio.split(":"))
        want = expected[ratio]
        check(
            "%s → %dx%d（期望 %dx%d）" % (ratio, image.width, image.height, want[0], want[1]),
            (image.width, image.height) == want,
        )
        check(
            "%s 比例精确 & 最长边 ≤2048" % ratio,
            image.width * b == image.height * a and max(image.size) <= 2048,
        )
        check(
            "%s 响应头宽高与图片一致" % ratio,
            headers.get("X-Dk-Width") == str(image.width)
            and headers.get("X-Dk-Height") == str(image.height),
            "头里是 %s x %s" % (headers.get("X-Dk-Width"), headers.get("X-Dk-Height")),
        )
        check("%s X-Dk-Changed=true" % ratio, headers.get("X-Dk-Changed") == "true")
        check(
            "%s 补的是白边" % ratio,
            image.convert("RGB").getpixel((0, 0)) == (255, 255, 255)
            if (image.width, image.height) != (1200, 800)
            else True,
        )
        import base64 as _b64

        notes = json.loads(_b64.b64decode(headers.get("X-Dk-Actions", "W10=")).decode("utf-8"))
        check("%s X-Dk-Actions 能解出中文" % ratio, isinstance(notes, list) and len(notes) > 0, str(notes))

    print("\n[7] max_dimension 是显式参数（它决定落 2K 还是 4K 计费档）")
    status, headers, body = post(source, ratio="1:1", max_dimension="1024")
    check("max_dimension=1024 → 1024x1024", status == 200 and open_result(body).size == (1024, 1024),
          "得到 %s %s" % (status, headers.get("X-Dk-Width")))
    status, headers, body = post(source, ratio="1:1", max_dimension="4096")
    check("max_dimension=4096 → 1200x1200（只补边不放大）",
          status == 200 and open_result(body).size == (1200, 1200))
    status, headers, body = post(source, ratio="1:1", max_dimension="abc")
    check("max_dimension=abc → 400", status == 400, str(status))

    print("\n[8] 透明底")
    alpha_png = make_png_with_alpha(600, 400)
    status, headers, body = post(alpha_png, ratio="1:1", filename="cutout.png")
    check("默认（不保留透明）→ 200", status == 200, str(status))
    if status == 200:
        image = open_result(body)
        check("输出 600x600", image.size == (600, 600), str(image.size))
        check("模式是 RGB（透明通道已合掉）", image.mode == "RGB", image.mode)
        check("原透明区变白", image.convert("RGB").getpixel((5, 5)) == (255, 255, 255),
              str(image.convert("RGB").getpixel((5, 5))))
        check("产品区仍是红色", image.convert("RGB").getpixel((300, 300))[0] > 150,
              str(image.convert("RGB").getpixel((300, 300))))

    status, headers, body = post(alpha_png, ratio="1:1", keep_transparency="true", filename="cutout.png")
    check("keep_transparency=true → 200", status == 200, str(status))
    if status == 200:
        image = open_result(body)
        check("输出 600x600 且是 RGBA", image.size == (600, 600) and image.mode == "RGBA",
              "%s %s" % (image.size, image.mode))
        check("补的是透明边（角落 alpha=0）", image.getpixel((5, 5))[3] == 0,
              str(image.getpixel((5, 5))))

    print("\n[9] 超大图必须被缩到 max_dimension 以内")
    big = make_jpeg(5000, 3000)
    status, headers, body = post(big, ratio="4:3", max_dimension="2048", filename="big.jpg")
    check("5000x3000 + 4:3 → 2048x1536", status == 200 and open_result(body).size == (2048, 1536),
          "得到 %s %s" % (status, headers.get("X-Dk-Width")))
    check("动作里带 downscaled", "downscaled" in (headers.get("X-Dk-Action-Codes") or ""),
          headers.get("X-Dk-Action-Codes", ""))
    status, headers, body = post(big, ratio="", max_dimension="2048", filename="big.jpg")
    check("不传 ratio 时只缩不补边 → 2048x1229",
          status == 200 and open_result(body).size == (2048, 1229),
          "得到 %s %s" % (status, headers.get("X-Dk-Width")))
    check("X-Dk-Ratio=auto", headers.get("X-Dk-Ratio") == "auto", headers.get("X-Dk-Ratio", ""))

    print("\n[10] 什么都不用改的图 → 原样直传（不重编码，体积不膨胀）")
    exact = make_jpeg(900, 1200)
    status, headers, body = post(exact, ratio="3:4", max_dimension="2048")
    check("900x1200 + 3:4 → 200", status == 200, str(status))
    check("X-Dk-Changed=false", headers.get("X-Dk-Changed") == "false", headers.get("X-Dk-Changed", ""))
    check("返回的就是原始字节", body == exact, "长度 %d vs %d" % (len(body), len(exact)))
    check("Content-Type=image/jpeg", (headers.get("Content-Type") or "").startswith("image/jpeg"),
          headers.get("Content-Type", ""))

    print("\n[11] EXIF 摆正")
    rotated = make_jpeg(1200, 800, orientation=6)
    status, headers, body = post(rotated, ratio="3:4", max_dimension="2048")
    check("竖拍照片 → 200", status == 200, str(status))
    check("摆正后源尺寸是 800x1200",
          headers.get("X-Dk-Source-Width") == "800" and headers.get("X-Dk-Source-Height") == "1200",
          "%s x %s" % (headers.get("X-Dk-Source-Width"), headers.get("X-Dk-Source-Height")))
    check("动作里带 exif_rotated", "exif_rotated" in (headers.get("X-Dk-Action-Codes") or ""),
          headers.get("X-Dk-Action-Codes", ""))

    print("\n[12] fail-closed —— 出错时只回 JSON，绝不回吐图片字节")
    status, headers, body = post(source, ratio="1536x1024")
    check("老格式 ratio → 400（不是静默不补边）", status == 400, str(status))
    check("响应是 JSON 不是图片", (headers.get("Content-Type") or "").startswith("application/json"),
          headers.get("Content-Type", ""))
    if status == 400:
        payload = json.loads(body)
        check("错误码是 invalid_ratio", payload["error"]["code"] == "invalid_ratio", str(payload))
    check("响应体不是原图", body != source)

    status, headers, body = post(b"this is definitely not an image" * 20, ratio="1:1")
    check("垃圾文件 → 415", status == 415, "得到 %s %s" % (status, body[:160]))
    check("垃圾文件的响应不是原图", b"this is definitely" not in body or status != 200)

    status, headers, body = post(b"", ratio="1:1")
    check("空文件 → 400", status == 400, str(status))

    status, headers, body = post(b"", ratio="1:1", omit_file=True)
    check("不带 file 字段 → 400（且是我们统一的错误格式）", status == 400, str(status))
    if status == 400:
        payload = json.loads(body)
        check("统一错误格式 {'error':{'code','message'}}",
              set(payload.get("error", {})) >= {"code", "message"}, str(payload))

    print("\n[13] HEIC")
    status, headers, body = post(make_fake_heic(), ratio="1:1", filename="IMG_0001.jpg")
    if heif_available:
        check("装了 pillow-heif：坏 HEIC → 415", status == 415, "得到 %s %s" % (status, body[:160]))
    else:
        check("没装 pillow-heif：HEIC → 422 heif_unsupported",
              status == 422 and json.loads(body)["error"]["code"] == "heif_unsupported",
              "得到 %s %s" % (status, body[:160]))

    print("\n[14] 鉴权")
    if TOKEN:
        status, headers, body = post(source, ratio="1:1", token="-")
        check("不带 Authorization → 401", status == 401, str(status))
        status, headers, body = post(source, ratio="1:1", token="wrong-token")
        check("token 不对 → 401", status == 401, str(status))
        status, headers, body = post(source, ratio="1:1")
        check("token 正确 → 200", status == 200, str(status))
    else:
        print("      （没设 DESIGNKIT_IMGSVC_TOKEN，跳过；服务此时不校验鉴权）")


def main() -> int:
    local_only = "--local-only" in sys.argv
    run_local()
    if not local_only:
        run_http()
    print("\n" + "=" * 60)
    if _failed:
        print("失败 %d 项：" % len(_failed))
        for name in _failed:
            print("  - %s" % name)
        print("通过 %d 项。" % _passed)
        return 1
    print("全部通过：%d 项。" % _passed)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
