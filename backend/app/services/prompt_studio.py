"""AI 提示词合成：看商品图 + 你的要求 + 风格参考 → 现场写出贴合这件商品的提示词。

为什么需要它：
提示词库（含灵感库上万条、也包括自带模板）本质上只是**参考**——它们描述的是别人的
商品或通用场景。直接发给生图模型，轻则不贴合，重则画出参考里那个商品（实测踩过）。

做法：把用户上传的商品图交给带视觉的文本模型，让它先看清这是什么（品类/外形/颜色/
材质/品牌），再结合用户的补充要求与参考风格，输出一条专为这件商品写的提示词。

用的是同一个网关的 chat/completions（文本比生图便宜得多）。任何一步失败都回退到
原提示词继续出图——合成只做加分项，不能挡住生成。
"""
import base64
import logging
import mimetypes
import re
from pathlib import Path
from typing import Any, Dict, List, Optional

import httpx

from .provider import ProviderError, _api_base, _extract_error, _headers
from .storage import abs_path

logger = logging.getLogger("designkit.prompt_studio")

MAX_BRIEF_CHARS = 4000
REQUEST_TIMEOUT = 180  # 视觉识别实测约 36 秒，给足余量

_SYSTEM_PROMPT = """You write image-generation prompts for e-commerce product photography.

You receive a photo of the user's ACTUAL product, plus a BRIEF (desired style, scene and
the user's own requirements). Write ONE prompt for an image model that will re-photograph
THIS EXACT product.

Rules:
1. The product in the supplied photo is the only subject. Describe it accurately —
   category, form, colour, material, finish, and any visible branding — so the image model
   reproduces it faithfully. Never invent a different product.
2. Treat the BRIEF as the art direction: scene, lighting, mood, colour palette,
   composition, camera angle. If the BRIEF names some other product, ignore that product
   and borrow only its styling.
3. The user's explicit requirements in the BRIEF outrank everything else. Honour them.
4. State that the product's real shape, colour, material and branding must be preserved.
5. Plain English only, never any other writing system. One paragraph, under 130 words,
   no markdown, no preamble, no surrounding quotes.

Output ONLY the prompt text."""

# 偶发 token 泄漏：混入天城文/西里尔/阿拉伯/希伯来/泰文等。
# 中日韩不拦——用户可能要求画面里出现中文。
_GARBLED_SCRIPT = re.compile(r"[ऀ-ॿЀ-ӿ؀-ۿ֐-׿฀-๿]")


def _text_model(settings: Dict[str, Any]) -> str:
    return str(settings.get("text_model") or "gpt-5.6-sol")


def _chat(settings: Dict[str, Any], messages: list) -> str:
    base = _api_base(settings)
    headers = _headers(settings)
    payload = {"model": _text_model(settings), "messages": messages}
    try:
        with httpx.Client(timeout=REQUEST_TIMEOUT, follow_redirects=True) as client:
            resp = client.post(base + "/v1/chat/completions", headers=headers, json=payload)
    except httpx.TimeoutException:
        raise ProviderError("AI 写提示词超时（超过 %d 秒）" % REQUEST_TIMEOUT)
    except httpx.HTTPError as e:
        raise ProviderError("无法连接文本模型：%s" % str(e)[:200])

    if resp.status_code != 200:
        raise ProviderError(
            "AI 写提示词失败（HTTP %d）：%s。请确认「系统设置 → 生图服务」里的"
            "文本模型在你的网关上可用。" % (resp.status_code, _extract_error(resp))
        )
    try:
        return (resp.json()["choices"][0]["message"]["content"] or "").strip()
    except (KeyError, IndexError, ValueError, TypeError):
        raise ProviderError("文本模型返回了无法解析的内容")


def _image_part(rel_path: str) -> Optional[dict]:
    """把商品图读成 data URI，供带视觉的模型识别。"""
    try:
        path: Path = abs_path(rel_path)
        data = path.read_bytes()
        mime = mimetypes.guess_type(path.name)[0] or "image/png"
        return {
            "type": "image_url",
            "image_url": {"url": "data:%s;base64,%s" % (mime, base64.b64encode(data).decode())},
        }
    except Exception as exc:
        logger.warning("读取商品图失败，跳过视觉识别：%s", exc)
        return None


def synthesize_prompt(
    settings: Dict[str, Any],
    brief: str,
    input_paths: Optional[List[str]] = None,
) -> str:
    """看商品图 + brief（模板风格 + 用户补充要求）→ 合成最终提示词。

    brief 就是原本要直接发给生图模型的那段提示词；这里把它降级为「art direction」，
    真正的主体以图为准。
    """
    brief = (brief or "").strip()
    if not brief:
        raise ProviderError("没有可用的提示词内容")

    content: List[dict] = [{"type": "text", "text": "BRIEF:\n%s" % brief[:MAX_BRIEF_CHARS]}]
    # 只看第一张：多图会显著拖慢且第一张通常就是主体商品
    for rel in (input_paths or [])[:1]:
        part = _image_part(rel)
        if part:
            content.append({"type": "text", "text": "The user's actual product:"})
            content.append(part)

    messages = [
        {"role": "system", "content": _SYSTEM_PROMPT},
        {"role": "user", "content": content},
    ]
    result = _chat(settings, messages)

    if _GARBLED_SCRIPT.search(result):
        logger.warning("合成结果混入非预期文字，重试一次")
        retried = _chat(settings, messages + [
            {"role": "assistant", "content": result},
            {"role": "user", "content":
                "That output contained non-English characters. Rewrite it in plain English only."},
        ])
        if retried and not _GARBLED_SCRIPT.search(retried):
            result = retried

    if not result:
        raise ProviderError("文本模型没有返回内容")
    return result


def test_text_model(settings: Dict[str, Any]) -> str:
    """「测试文本模型」：只发一句极短的话，成本可忽略。"""
    reply = _chat(settings, [{"role": "user", "content": "Reply with exactly: OK"}])
    return "文本模型 %s 可用（返回：%s）" % (_text_model(settings), reply[:40])
