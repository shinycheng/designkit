"""生图服务商抽象。

- mock：本地用 Pillow 画占位图，模拟真实耗时，用于没有 API Key 时验证全流程。
- openai：OpenAI 兼容接口（官方或国内中转平台通用），
  有商品图时走 /v1/images/edits（图生图），没有时走 /v1/images/generations（文生图）。
"""
import base64
import binascii
import hashlib
import io
import logging
import re
import textwrap
import time
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional, Tuple

import httpx
from PIL import Image, ImageDraw

from . import imaging
from .storage import abs_path


logger = logging.getLogger(__name__)


class ProviderError(Exception):
    """带用户可读信息的生图失败。"""


def _parse_size(size: str) -> "tuple":
    try:
        w, h = size.lower().split("x")
        return max(64, min(4096, int(w))), max(64, min(4096, int(h)))
    except Exception:
        return 1024, 1024


def generate_images(
    settings: Dict[str, Any],
    prompt: str,
    input_paths: List[str],
    n: int,
    size: str,
    quality: Optional[str] = None,
) -> List[bytes]:
    provider = settings.get("provider", "mock")
    if provider == "mock":
        return _mock_generate(prompt, n, size)
    if provider == "openai":
        return _openai_generate(settings, prompt, input_paths, n, size, quality)
    raise ProviderError("未知的生图服务商：%s" % provider)


# ---------------------------------------------------------------- mock

def _mock_generate(prompt: str, n: int, size: str) -> List[bytes]:
    width, height = _parse_size(size)
    results = []
    for i in range(n):
        time.sleep(1.2)  # 模拟真实 API 的耗时，让前端进度体验接近真实
        seed = hashlib.md5((prompt + str(i)).encode("utf-8")).digest()
        top = (80 + seed[0] % 120, 80 + seed[1] % 120, 140 + seed[2] % 100)
        bottom = (200 + seed[3] % 55, 180 + seed[4] % 60, 160 + seed[5] % 80)
        im = Image.new("RGB", (width, height))
        for y in range(height):
            t = y / max(1, height - 1)
            row_color = tuple(int(top[c] + (bottom[c] - top[c]) * t) for c in range(3))
            ImageDraw.Draw(im).line([(0, y), (width, y)], fill=row_color)
        draw = ImageDraw.Draw(im)
        margin = int(width * 0.08)
        wrapped = textwrap.fill(prompt[:220], width=28)
        draw.text((margin, int(height * 0.30)), wrapped, fill=(255, 255, 255))
        draw.text((margin, int(height * 0.06)), "MOCK 模拟生图 %d/%d" % (i + 1, n), fill=(255, 255, 255))
        draw.text((margin, height - int(height * 0.08)), "配置真实 API 后此处为 AI 生成图", fill=(255, 255, 255))
        buf = io.BytesIO()
        im.save(buf, "PNG")
        results.append(buf.getvalue())
    return results


# ---------------------------------------------------------------- openai 兼容

# 用户填的地址结尾五花八门：有人填到 /v1，有人只填域名，也有网关自带别的版本段
# （如火山方舟的 /api/v3）。以前的做法是「剥掉 /v1 再补 /v1」，遇到 /api/v3 就会
# 拼成 .../api/v3/v1/images/edits 直接 404，而错误只显示「连不上」，极难排查。
# 现在统一由 _api_base 返回**已含版本段**的前缀，调用处只拼后半截路径。
_VERSION_SUFFIXES = ("/v1", "/api/v3", "/api/plan/v3")


def _api_base(settings: Dict[str, Any]) -> str:
    """返回可直接拼接 /images/edits 这类路径的接口前缀。"""
    base = str(settings.get("openai_base_url") or "").strip().rstrip("/")
    if not base:
        raise ProviderError("尚未配置生图 API 地址（系统设置 → 生图服务）")
    if any(base.endswith(suffix) for suffix in _VERSION_SUFFIXES):
        return base
    return base + "/v1"


def _headers(settings: Dict[str, Any]) -> Dict[str, str]:
    api_key = str(settings.get("openai_api_key") or "").strip()
    if not api_key:
        raise ProviderError("尚未配置生图 API Key（系统设置 → 生图服务）")
    return {"Authorization": "Bearer " + api_key}


def _error_param(resp: httpx.Response) -> str:
    """取 OpenAI 风格错误里的 error.param —— 比解析文案更可靠的参数名信号。"""
    try:
        err = resp.json().get("error")
        if isinstance(err, dict) and err.get("param"):
            return str(err["param"])
    except Exception:
        pass
    return ""


# 网关挂掉或前面挡着反代时，返回的往往不是 JSON 而是一整页 HTML 错误页。
# 原样贴给用户是几百字的标签噪音，得认出来并只留一句话。
_HTML_PAGE = re.compile(r"<\s*(!doctype|html|head|body|title)\b", re.I)

# 状态码兜底文案：只有当响应体里读不出业务原因时才用。
# 写成「怎么办」而不是「哪里错」——用户是电商运营，不懂 HTTP 语义。
_STATUS_HINTS = {
    401: "API Key 无效或已过期（系统设置 → 生图服务）",
    403: "API Key 没有调用该模型的权限，或套餐不支持",
    404: "接口地址或模型名不对（检查 API 地址结尾、生图模型名称）",
    408: "网关处理超时",
    429: "请求太频繁或额度已用完，稍后再试或去网关那边充值",
    500: "网关内部错误",
    502: "网关无法连接到上游模型服务",
    503: "网关繁忙或正在维护",
    504: "网关等待上游超时",
}


def _extract_error(resp: httpx.Response) -> str:
    try:
        data = resp.json()
        if isinstance(data, dict):
            err = data.get("error")
            if isinstance(err, dict) and err.get("message"):
                return str(err["message"])
            if isinstance(err, str) and err.strip():
                return err.strip()
            for key in ("message", "msg", "detail"):
                if data.get(key):
                    return str(data[key])
    except Exception:
        pass
    text = (resp.text or "").strip()
    if text and _HTML_PAGE.search(text[:400]):
        return "网关返回了 HTML 错误页（多半是网关本身或它前面的反向代理挂了）"
    return text[:300] or ("HTTP %d" % resp.status_code)


def _friendly_error(resp: httpx.Response) -> str:
    """把网关报错整理成一句用户能照着处理的话。

    取值顺序：响应体里的业务原因 > 状态码兜底文案。反过来会把
    「余额不足」这种最有用的信息盖成笼统的「网关内部错误」。
    """
    detail = _extract_error(resp)
    hint = _STATUS_HINTS.get(resp.status_code)
    if not hint:
        return "生图接口报错（HTTP %d）：%s" % (resp.status_code, detail)
    # detail 只是状态码的复述时（如纯 "HTTP 502"）就不重复贴了
    if detail and not detail.startswith("HTTP "):
        return "%s（HTTP %d）：%s" % (hint, resp.status_code, detail)
    return "%s（HTTP %d）" % (hint, resp.status_code)


def _envelope_error(payload: Any) -> Optional[str]:
    """识别「HTTP 200 但业务失败」的国内网关信封，如 {"code":500,"msg":"余额不足"}。

    只在一张图都没取到时才调用——有些网关成功时也带 code（{"code":200,"data":[...]}
    甚至 {"code":"0"}），先按 code 判死会把已经付过钱的图直接丢掉。
    """
    if not isinstance(payload, dict):
        return None
    code = payload.get("code")
    ok_codes = (0, 200, "0", "200", "success", "ok")
    if code is not None and code not in ok_codes:
        for key in ("msg", "message", "error_msg", "detail"):
            if payload.get(key):
                return "生图网关返回失败（code=%s）：%s" % (code, str(payload[key])[:200])
        return "生图网关返回失败（code=%s）" % code
    return None


def _decode_response(resp: httpx.Response, timeout: int) -> List[bytes]:
    try:
        payload = resp.json()
    except Exception:
        text = (resp.text or "").strip()
        if _HTML_PAGE.search(text[:400]):
            raise ProviderError("生图接口返回了 HTML 页面而不是数据，请检查 API 地址是否填对")
        raise ProviderError("生图接口返回了无法解析的内容：%s" % text[:200])

    # 不同网关放图的字段名不一样：OpenAI 用 data，部分中转用 images / results
    items = []
    if isinstance(payload, dict):
        for key in ("data", "images", "results", "output"):
            value = payload.get(key)
            if isinstance(value, list) and value:
                items = value
                break
    if not items:
        envelope = _envelope_error(payload)
        if envelope:
            raise ProviderError(envelope)
        # 把顶层字段名列出来：遇到异步任务式网关（返回 task_id 让你去轮询）时，
        # 这行字是唯一能看出「不是没图，是接口形态不对」的线索
        keys = ", ".join(str(k) for k in payload.keys())[:120] if isinstance(payload, dict) else ""
        if keys:
            raise ProviderError("生图接口没有返回图片（返回字段：%s）" % keys)
        raise ProviderError("生图接口没有返回任何图片")
    results: List[bytes] = []
    for item in items:
        # 少数网关直接给一串 URL 而不是对象，统一成对象再处理
        if isinstance(item, str):
            item = {"url": item} if item.startswith("http") else {"b64_json": item}
        if not isinstance(item, dict):
            continue
        if item.get("b64_json"):
            try:
                results.append(base64.b64decode(item["b64_json"]))
            except (binascii.Error, ValueError):
                raise ProviderError("生图接口返回的图片数据无法解码")
        elif item.get("url"):
            # 部分中转平台返回图片链接而不是 base64，这里代为下载
            try:
                r = httpx.get(item["url"], timeout=timeout, follow_redirects=True)
            except httpx.HTTPError as e:
                raise ProviderError("下载生成图片失败：%s" % str(e)[:150])
            if r.status_code != 200:
                raise ProviderError("下载生成图片失败（HTTP %d）" % r.status_code)
            results.append(r.content)
    if not results:
        raise ProviderError("生图接口返回数据里没有图片内容")
    return results


# 「网关在抱怨 background 参数」的判定。
#
# 不能简单地判「错误文本里有 background」：透明底模式下提示词必然含有
# "transparent background"，而很多网关会把提示词原样回显进错误体，于是任何一个
# 400（比如内容审核不过）都会被误判成「不支持透明底」，进而在逐张降级路径里
# 把已付费的图全部丢掉。
#
# 但也不能只认 "unknown parameter" 这种标准措辞——实测用户的自建网关回的是
# "Transparent background is not supported for this model."，一个字都不沾。
# 折中办法：要求「拒绝措辞」和 background **挨在一起**（同一句、60 字符内）。
# 提示词回显里 background 后面跟的是 "with an alpha channel: no backdrop…"，
# 不会命中这些拒绝词。
_BACKGROUND_REJECTED = re.compile(
    r"(?:unknown|unsupported|unrecognized|invalid|unexpected|not\s+supported|"
    r"not\s+available|does\s+not\s+support|cannot\s+support)[^.]{0,60}background"
    r"|background[^.]{0,60}(?:is\s+)?(?:not\s+supported|unsupported|not\s+available|"
    r"not\s+allowed|is\s+invalid|is\s+unknown|is\s+not\s+available)",
    re.I,
)


def _rejects_background(resp: httpx.Response) -> bool:
    if _error_param(resp) == "background":
        return True
    error = _extract_error(resp)
    if "background" not in error.lower():
        return False
    return bool(_BACKGROUND_REJECTED.search(error))


def _rejects_multi_image(error: str) -> bool:
    """判断网关的 400 是不是在抱怨「一次要多张图」这个参数。

    只在已经失败的 400 上判断，所以宁可放宽一点：判错最多多发几次同样会失败的
    请求（失败请求不计费），判漏则用户直接拿不到图。
    """
    low = error.lower()
    if "tools[0].n" in low:
        return True
    # 各家网关拒绝多图的常见措辞：
    #   dall-e-3: You must provide n=1 for this model.
    #   其他:     Only n=1 is supported / n must be 1 for this model
    if re.search(r"(?<![a-z0-9_])n\s*(?:=|must\s+be)\s*1(?![0-9])", low):
        return True
    if not any(
        hint in low
        for hint in ("unknown parameter", "unsupported parameter", "invalid parameter",
                     "unsupported value", "not supported", "unknown argument")
    ):
        return False
    # 参数名 n 单独出现（含被引号包裹的形式），避免误伤 prompt 里的普通单词
    return re.search(r"""(?<![a-z0-9_])['"`]?n['"`]?(?![a-z0-9_])""", low) is not None


def _generate_one_by_one(
    send: Callable[[Dict[str, Any]], Tuple[httpx.Response, Dict[str, Any]]],
    payload: Dict[str, Any],
    n: int,
    timeout: int,
) -> List[bytes]:
    """网关不支持一次多张时，逐张请求凑够 n 张。

    关键约束（都直接关系到用户的钱和等待时间）：
    - 已经拿到的图绝不丢弃：中途失败就返回已成功的部分，让上游已计费的图不白花；
    - 有整体时间上限，避免 worker 线程被无限期占住；
    - 拿够 n 张立刻停手，不多发一次请求。

    预算按「每张一份 timeout」给（而不是全部 n 张共用一份），否则正常速度的网关
    也会在最后几张被静默截断——那等于用户花了钱却拿不到应有的张数。
    """
    single_payload = dict(payload)
    single_payload["n"] = 1
    results: List[bytes] = []
    budget = timeout * n
    deadline = time.monotonic() + budget
    for index in range(n):
        if index and time.monotonic() >= deadline:
            logger.warning("逐张生成已达 %d 秒总预算，返回已完成的 %d/%d 张", budget, len(results), n)
            break
        try:
            resp, single_payload = send(single_payload)
        except (httpx.HTTPError, ProviderError) as e:
            # ProviderError 也要接住：send 内部的兼容性判定（如「网关不支持透明底」）
            # 会抛它，若让它穿透出去，前面已经出好、已经付过钱的图会被全部丢弃，
            # 随后 worker 还会把整单重排队再付一次
            if results:
                logger.warning("第 %d 张请求失败（%s），返回已完成的 %d 张", index + 1, e, len(results))
                break
            raise
        if resp.status_code != 200:
            message = _friendly_error(resp)
            if results:
                logger.warning("第 %d 张失败（%s），返回已完成的 %d 张", index + 1, message, len(results))
                break
            raise ProviderError(message)
        try:
            results.extend(_decode_response(resp, timeout))
        except ProviderError:
            if results:
                logger.warning("第 %d 张解析失败，返回已完成的 %d 张", index + 1, len(results))
                break
            raise
        if len(results) >= n:
            break
    if not results:
        raise ProviderError("生图接口没有返回任何图片")
    return results[:n]


def _openai_generate(
    settings: Dict[str, Any],
    prompt: str,
    input_paths: List[str],
    n: int,
    size: str,
    quality: Optional[str],
) -> List[bytes]:
    base = _api_base(settings)
    headers = _headers(settings)
    model = str(settings.get("image_model") or "gpt-image-2")
    timeout = int(settings.get("request_timeout") or 360)
    normalize_ratio = bool(settings.get("normalize_input_ratio", True))

    want_transparent = str(settings.get("image_background") or "auto").lower() == "transparent"

    common: Dict[str, Any] = {"model": model, "prompt": prompt, "n": n, "size": size}
    if quality and quality != "auto":
        common["quality"] = quality
    # 只在真要透明底时才发这个参数：老网关不认识它会直接 400，
    # 平时不发就不会给自己找麻烦
    if want_transparent:
        common["background"] = "transparent"
    # gpt-image 系列固定返回 b64；老模型（dall-e 系）需要显式要求 b64
    if not model.startswith("gpt-image"):
        common["response_format"] = "b64_json"

    # 输入图只预处理一次（方向摆正、HEIC 解码、透明处理、按比例补边），重试/降级时复用
    prepared_images: List[Tuple[str, bytes, str]] = []
    for rel in input_paths:
        p: Path = abs_path(rel)
        prepared = imaging.prepare_input_image(
            p.read_bytes(), size, normalize_ratio, original_suffix=p.suffix,
            keep_transparency=want_transparent,
        )
        prepared_images.append((p.stem + prepared.suffix, prepared.data, prepared.mime))
        if prepared.note != "无需处理":
            logger.info("输入图 %s：%s", p.name, prepared.note)

    def _send(payload: Dict[str, Any]) -> httpx.Response:
        with httpx.Client(timeout=timeout, follow_redirects=True) as client:
            if prepared_images:
                field = "image" if len(prepared_images) == 1 else "image[]"
                files = [
                    (field, (name, content, mime))
                    for name, content, mime in prepared_images
                ]
                data = {k: str(v) for k, v in payload.items()}
                return client.post(
                    base + "/images/edits", headers=headers, data=data, files=files
                )
            return client.post(
                base + "/images/generations", headers=headers, json=payload
            )

    def _send_with_compat_retry(payload: Dict[str, Any]) -> Tuple[httpx.Response, Dict[str, Any]]:
        """发一次请求；若网关不认识 response_format 则去掉重试。

        返回 (响应, 实际生效的 payload)——把协商结果交还给调用方复用，
        否则后续每次请求都要重复踩一次必然失败的 400（费用与耗时都会翻倍）。
        """
        resp = _send(payload)
        if (
            resp.status_code == 400
            and "response_format" in payload
            and "response_format" in _extract_error(resp)
        ):
            payload = {k: v for k, v in payload.items() if k != "response_format"}
            resp = _send(payload)
        # 网关不认识 background 时**故意不静默去掉重试**：那样会出一张白底图，
        # 用户却以为拿到了透明底，直到上架时才发现——不如当场说清楚。
        if resp.status_code == 400 and "background" in payload and _rejects_background(resp):
            raise ProviderError(
                "当前生图网关不支持透明底（background 参数）。"
                "请到「系统设置 → 生图服务」把「出图底色」改回「跟随模型」，"
                "或换用支持该参数的模型。"
            )
        return resp, payload

    try:
        resp, negotiated = _send_with_compat_retry(common)
        # 部分 OpenAI 兼容网关会把 Images API 内部转译为 Responses
        # image_generation tool，但不接受一次要多张图。此时降级为多次单图请求。
        rejects_multi = resp.status_code == 400 and n > 1 and (
            _error_param(resp) in ("n", "tools[0].n")
            or _rejects_multi_image(_extract_error(resp))
        )
        if rejects_multi:
            logger.warning("生图网关不支持单请求多图 n=%d，降级为逐张请求", n)
            return _generate_one_by_one(_send_with_compat_retry, negotiated, n, timeout)
    except httpx.TimeoutException:
        raise ProviderError("生图接口超时（超过 %d 秒），可在系统设置里调大超时时间" % timeout)
    except httpx.HTTPError as e:
        raise ProviderError("无法连接生图接口：%s" % str(e)[:200])

    if resp.status_code != 200:
        raise ProviderError(_friendly_error(resp))
    return _decode_response(resp, timeout)


def test_connection(settings: Dict[str, Any]) -> str:
    """「测试连接」按钮：不花生图的钱，只验证地址和 Key 是否可用。"""
    if settings.get("provider") == "mock":
        return "当前为模拟生图模式，无需外部连接，可直接使用"
    base = _api_base(settings)
    headers = _headers(settings)
    try:
        resp = httpx.get(base + "/models", headers=headers, timeout=20)
    except httpx.HTTPError as e:
        raise ProviderError("无法连接：%s" % str(e)[:200])
    if resp.status_code == 200:
        return "连接成功，API 地址和 Key 有效"
    if resp.status_code in (401, 403):
        raise ProviderError("API Key 无效或没有权限（HTTP %d）" % resp.status_code)
    if resp.status_code in (404, 405):
        # 不是所有网关都提供 /models。这里既不能报「配置错误」吓唬用户，
        # 也不能拍胸脯说「可用」——地址填错同样是 404。
        raise ProviderError(
            "该地址没有提供模型列表接口（HTTP %d），无法据此确认连通性。"
            "如果你的网关本来就没有 /models，属正常现象，可直接试着生成一张图；"
            "否则请检查 API 地址是否填对。" % resp.status_code
        )
    raise ProviderError(_friendly_error(resp))
