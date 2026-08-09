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

from . import sizing
from .provider import ProviderError, _api_base, _extract_error, _headers
from .storage import abs_path

logger = logging.getLogger("designkit.prompt_studio")

MAX_BRIEF_CHARS = 4000
REQUEST_TIMEOUT = 180  # 视觉识别实测约 36 秒，给足余量

_SYSTEM_PROMPT = """You write image-generation prompts for e-commerce product photography.

You receive a photo of the user's ACTUAL product, a BRIEF (the user's own requirements),
a required OUTPUT FRAMING, and sometimes a numbered list of REFERENCE STYLES sampled
from a prompt library. Write ONE prompt for an image model that will re-photograph
THIS EXACT product.

Rules:
1. The product in the supplied photo is the only subject. Describe it accurately —
   category, form, colour, material, finish, and any visible branding — so the image model
   reproduces it faithfully. Never invent a different product.
2. Treat the BRIEF as the art direction: scene, lighting, mood, colour palette,
   camera angle. If the BRIEF names some other product, ignore that product and borrow
   only its styling.
3. The user's explicit requirements in the BRIEF outrank everything else. Honour them.
4. Compose for the required OUTPUT FRAMING and end the prompt by stating it. Never use
   wording that implies a different shape of frame — avoid describing the picture itself
   as tall, vertical, wide, panoramic, full-length or long-form, and do not request
   multi-section layouts or detail pages. Those words describe the FRAME, not the product;
   you may still describe the product's own proportions.
5. State that the product's real shape, colour, material and branding must be preserved.
6. When REFERENCE STYLES are supplied, first decide which 2-4 of them suit THIS product
   best, then borrow only their staging, lighting, camera angle, colour mood and
   composition. Ignore whatever product, person, brand or text those references depict —
   they are style samples, not subjects. Do not mention the references in your output.
   If none of them fit the product, ignore all of them and follow the BRIEF alone.
7. Plain English only, never any other writing system. One paragraph, under 130 words,
   no markdown, no preamble, no surrounding quotes.
8. If a REQUIRED BACKGROUND line asks for a transparent background, describe ONLY the
   product itself — its form, colour, material, finish and branding, plus the lighting on
   the product. Write no scene, no surface, no table, no floor, no wall, no props,
   no cast shadow and no reflections onto anything. Ignore any scenery in the BRIEF;
   borrow only its lighting and colour mood.

Output ONLY the prompt text."""

# 网关忽略 size 参数，出图比例由「输入图比例」和「提示词里的构图措辞」共同决定，
# 且措辞的优先级更高（实测：1:1 输入 + 「tall / upright / bursting upward」的提示词
# → 输出 2:3 竖图）。所以把目标画幅显式写进提示词，才是可靠的比例控制手段。
# 措辞按尺寸「算出来」而不是查表：查表意味着每放开一个比例都要记得回来加一行，
# 忘了加就退化成完全不约束比例——而比例失控恰恰是最难被发现的那类问题
# （出图本身是好看的，只是尺寸不对，用户往往到上架时才发现）。
def framing_label(size: Optional[str]) -> Optional[str]:
    """例：1024x1360 → "a portrait 3:4 image"。auto 或非法尺寸返回 None。"""
    orientation = sizing.orientation(size)
    ratio = sizing.ratio_label(size)
    if not orientation or not ratio:
        return None
    return "a %s %s image" % (orientation, ratio)


def aspect_clause(size: Optional[str]) -> str:
    """给提示词补一句显式画幅要求；auto 或未知尺寸返回空串。"""
    label = framing_label(size)
    return (" The final image must be %s." % label) if label else ""


# 自带模板与灵感库里大量写着 "pure seamless white background" 这类措辞。
# 要透明底时如果不管它们，参数发了也没用——模型会照着提示词画一块白。
_BACKDROP_WORDS = re.compile(
    # 形容词可以连着堆好几个（"pure seamless white background"），要全部吃掉；
    # 但**不能**把短语前面的空格算进匹配，否则替换后会和前一个词粘在一起
    r"\b(?:(?:pure|seamless|solid|plain|clean|studio|minimal|bright|soft|neutral)\s+)*"
    r"(?:white|off-white|light\s+gr[ae]y|gr[ae]y|beige|cream|ivory)\s+"
    r"(?:background|backdrop|seamless(?:\s+paper)?)\b",
    re.I,
)

_TRANSPARENT_CLAUSE = (
    " Output the subject on a fully transparent background with an alpha channel: "
    "no backdrop, no floor, no wall, no cast shadow onto any surface."
)


def enforce_transparent_background(prompt: str) -> str:
    """把提示词改写成「要透明底」。

    先删掉原文里的白底/灰底措辞再追加要求——只追加不删除的话，
    两句互相打架，模型多半仍会画一块实底。
    """
    text = (prompt or "").strip()
    text = _BACKDROP_WORDS.sub("transparent background", text)
    if "transparent background" not in text.lower():
        text = text.rstrip()
    if _TRANSPARENT_CLAUSE.strip() not in text:
        text = text.rstrip() + _TRANSPARENT_CLAUSE
    return text


def enforce_aspect(prompt: str, size: Optional[str]) -> str:
    """把目标画幅同时钉在提示词的开头和结尾。

    开头那句是主力：实测（乌龙茶提示词覆盖商品的那次）证明位于开头的指令效力最强，
    足以压过正文里的构图描述；结尾再重申一次做兜底。auto 尺寸不做任何约束。
    """
    label = framing_label(size)
    if not label:
        return prompt

    text = (prompt or "").strip()
    head = "Compose %s." % label          # 例：Compose a square 1:1 image.
    tail = aspect_clause(size).strip()    # 例：The final image must be a square 1:1 image.

    if not text.startswith(head):
        text = head + " " + text
    if tail not in text:
        text = text.rstrip() + " " + tail
    return text

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
            resp = client.post(base + "/chat/completions", headers=headers, json=payload)
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


_PICK_PROMPT = """You browse a numbered catalogue of prompt titles and pick the ones whose
photographic STYLE would suit the product in the supplied photo.

Judge only by staging, lighting, mood and composition implied by the title. Ignore what
product, person or brand each title mentions — you are choosing a look, not a subject.

Reply with ONLY the numbers of the {count} best matches, comma-separated, best first
(e.g. "42, 7, 118"). No words, no explanation. If nothing fits, reply "none"."""

_ID_RE = re.compile(r"\d+")


def pick_references(
    settings: Dict[str, Any],
    digest: List[dict],
    input_paths: Optional[List[str]] = None,
    count: int = 4,
) -> List[int]:
    """第一段：让模型通览该分类的全部标题，挑出风格最搭的几条，返回它们的 id。

    只发标题不发正文——正文全给要几十万 token，而挑选阶段只需要知道「是什么风格」。
    选中的那几条随后才取全文（fetch_references），交给第二段照着写。
    """
    if not digest:
        return []
    listing = "\n".join("%d. %s" % (item["id"], item["title"]) for item in digest)
    content: List[dict] = []
    for rel in (input_paths or [])[:1]:
        part = _image_part(rel)
        if part:
            content.append({"type": "text", "text": "The product to shoot:"})
            content.append(part)
    content.append({"type": "text", "text": "CATALOGUE:\n" + listing})

    reply = _chat(settings, [
        {"role": "system", "content": _PICK_PROMPT.format(count=count)},
        {"role": "user", "content": content},
    ])
    valid = {item["id"] for item in digest}
    picked, seen = [], set()
    for token in _ID_RE.findall(reply or ""):
        pid = int(token)
        if pid in valid and pid not in seen:
            seen.add(pid)
            picked.append(pid)
        if len(picked) >= count:
            break
    return picked


def synthesize_prompt(
    settings: Dict[str, Any],
    brief: str,
    input_paths: Optional[List[str]] = None,
    size: Optional[str] = None,
    transparent: bool = False,
    references: Optional[List[dict]] = None,
) -> str:
    """看商品图 + brief（模板风格 + 用户补充要求）+ 目标画幅 → 合成最终提示词。

    brief 就是原本要直接发给生图模型的那段提示词；这里把它降级为「art direction」，
    真正的主体以图为准。size 决定画幅，会同时写进指令和成品提示词。

    transparent=True 时必须在**合成阶段**就把要求传进去。只在事后用正则删白底措辞
    是不够的：视觉模型会写出「大理石台面、暖调轮廓光、柔和投影」这类场景描述，
    正则删不掉，最终提示词变成「一整段场景 + 一句别画背景」自相矛盾，
    模型多半照样画实底——而这一次是要计费的。
    """
    brief = (brief or "").strip()
    if not brief:
        raise ProviderError("没有可用的提示词内容")

    content: List[dict] = [{"type": "text", "text": "BRIEF:\n%s" % brief[:MAX_BRIEF_CHARS]}]
    label = framing_label(size)
    if label:
        content.append({"type": "text", "text": "REQUIRED OUTPUT FRAMING: %s" % label})
    if transparent:
        content.append({"type": "text", "text":
            "REQUIRED BACKGROUND: fully transparent PNG with an alpha channel. "
            "No scene, no surface, no floor, no wall, no props, no cast shadow."})
    if references:
        # 只给标题 + 摘要：这些提示词开头就是布景/光线/构图的核心，
        # 给全文会让单次调用涨到 2 万 token 以上，而对风格判断毫无增益
        listing = "\n\n".join(
            "%d. %s\n%s" % (i + 1, r.get("title") or "(untitled)", r.get("excerpt") or "")
            for i, r in enumerate(references)
        )
        content.append({"type": "text", "text": "REFERENCE STYLES:\n" + listing})
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
    # 兜底：模型偶尔会忘记声明画幅/背景要求，这里补上
    result = enforce_aspect(result, size)
    if transparent:
        result = enforce_transparent_background(result)
    return result


def test_text_model(settings: Dict[str, Any]) -> str:
    """「测试文本模型」：只发一句极短的话，成本可忽略。"""
    reply = _chat(settings, [{"role": "user", "content": "Reply with exactly: OK"}])
    return "文本模型 %s 可用（返回：%s）" % (_text_model(settings), reply[:40])
