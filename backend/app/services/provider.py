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
import mimetypes
import re
import textwrap
import time
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional, Tuple

import httpx
from PIL import Image, ImageDraw

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

def _api_base(settings: Dict[str, Any]) -> str:
    base = str(settings.get("openai_base_url") or "").strip().rstrip("/")
    if not base:
        raise ProviderError("尚未配置生图 API 地址（系统设置 → 生图服务）")
    if base.endswith("/v1"):
        base = base[: -len("/v1")]
    return base


def _headers(settings: Dict[str, Any]) -> Dict[str, str]:
    api_key = str(settings.get("openai_api_key") or "").strip()
    if not api_key:
        raise ProviderError("尚未配置生图 API Key（系统设置 → 生图服务）")
    return {"Authorization": "Bearer " + api_key}


def _extract_error(resp: httpx.Response) -> str:
    try:
        data = resp.json()
        if isinstance(data, dict):
            err = data.get("error")
            if isinstance(err, dict) and err.get("message"):
                return str(err["message"])
            if data.get("message"):
                return str(data["message"])
    except Exception:
        pass
    return resp.text[:300] or ("HTTP %d" % resp.status_code)


def _decode_response(resp: httpx.Response, timeout: int) -> List[bytes]:
    try:
        payload = resp.json()
    except Exception:
        raise ProviderError("生图接口返回了无法解析的内容：%s" % resp.text[:200])
    items = payload.get("data") or []
    if not items:
        raise ProviderError("生图接口没有返回任何图片")
    results: List[bytes] = []
    for item in items:
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


def _rejects_multi_image(error: str) -> bool:
    """判断网关的 400 是不是在抱怨「一次要多张图」这个参数。

    只在已经失败的 400 上判断，所以宁可放宽一点：判错最多多发几次同样会失败的
    请求（失败请求不计费），判漏则用户直接拿不到图。
    """
    low = error.lower()
    if "tools[0].n" in low:
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
    - 整体有时间预算，不让 n 张串行请求把耗时叠成 n 倍；
    - 拿够 n 张立刻停手，不多发一次请求。
    """
    single_payload = dict(payload)
    single_payload["n"] = 1
    results: List[bytes] = []
    deadline = time.monotonic() + timeout
    for index in range(n):
        if index and time.monotonic() >= deadline:
            logger.warning("逐张生成已达 %d 秒预算，返回已完成的 %d/%d 张", timeout, len(results), n)
            break
        try:
            resp, single_payload = send(single_payload)
        except httpx.HTTPError as e:
            if results:
                logger.warning("第 %d 张请求失败（%s），返回已完成的 %d 张", index + 1, e, len(results))
                break
            raise
        if resp.status_code != 200:
            message = "生图接口报错（HTTP %d）：%s" % (resp.status_code, _extract_error(resp))
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
    model = str(settings.get("image_model") or "gpt-image-1")
    timeout = int(settings.get("request_timeout") or 300)

    common: Dict[str, Any] = {"model": model, "prompt": prompt, "n": n, "size": size}
    if quality and quality != "auto":
        common["quality"] = quality
    # gpt-image 系列固定返回 b64；老模型（dall-e 系）需要显式要求 b64
    if not model.startswith("gpt-image"):
        common["response_format"] = "b64_json"

    def _send(payload: Dict[str, Any]) -> httpx.Response:
        with httpx.Client(timeout=timeout, follow_redirects=True) as client:
            if input_paths:
                files = []
                field = "image" if len(input_paths) == 1 else "image[]"
                for rel in input_paths:
                    p: Path = abs_path(rel)
                    mime = mimetypes.guess_type(p.name)[0] or "image/png"
                    files.append((field, (p.name, p.read_bytes(), mime)))
                data = {k: str(v) for k, v in payload.items()}
                return client.post(
                    base + "/v1/images/edits", headers=headers, data=data, files=files
                )
            return client.post(
                base + "/v1/images/generations", headers=headers, json=payload
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
        return resp, payload

    try:
        resp, negotiated = _send_with_compat_retry(common)
        # 部分 OpenAI 兼容网关会把 Images API 内部转译为 Responses
        # image_generation tool，但不接受一次要多张图。此时降级为多次单图请求。
        if resp.status_code == 400 and n > 1 and _rejects_multi_image(_extract_error(resp)):
            logger.warning("生图网关不支持单请求多图 n=%d，降级为逐张请求", n)
            return _generate_one_by_one(_send_with_compat_retry, negotiated, n, timeout)
    except httpx.TimeoutException:
        raise ProviderError("生图接口超时（超过 %d 秒），可在系统设置里调大超时时间" % timeout)
    except httpx.HTTPError as e:
        raise ProviderError("无法连接生图接口：%s" % str(e)[:200])

    if resp.status_code != 200:
        raise ProviderError("生图接口报错（HTTP %d）：%s" % (resp.status_code, _extract_error(resp)))
    return _decode_response(resp, timeout)


def test_connection(settings: Dict[str, Any]) -> str:
    """「测试连接」按钮：不花生图的钱，只验证地址和 Key 是否可用。"""
    if settings.get("provider") == "mock":
        return "当前为模拟生图模式，无需外部连接，可直接使用"
    base = _api_base(settings)
    headers = _headers(settings)
    try:
        resp = httpx.get(base + "/v1/models", headers=headers, timeout=20)
    except httpx.HTTPError as e:
        raise ProviderError("无法连接：%s" % str(e)[:200])
    if resp.status_code == 200:
        return "连接成功，API 地址和 Key 有效"
    if resp.status_code == 401:
        raise ProviderError("API Key 无效（HTTP 401）")
    raise ProviderError("连接异常（HTTP %d）：%s" % (resp.status_code, _extract_error(resp)))
