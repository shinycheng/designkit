"""出图尺寸：用「护栏校验」代替「白名单枚举」。

以前只放行 4 个写死的尺寸串，加一个电商版位要同时改后端 2 处、前端 3 处，
漏改一处就变成「设置页能选、提交时 422」。这里改成一套通用规则：
任何尺寸只要过得了护栏就放行，预设表只负责给界面提供好用的默认选项。

护栏的四条规则来自实践而非拍脑袋：
- 边长必须是 16 的倍数：几乎所有扩散模型的隐空间都按 8 或 16 下采样，
  非整除的边长会被服务端自行取整，出图尺寸和你要的对不上；
- 边长和总像素有上下限：太小没法用，太大很多网关直接拒绝或超时；
- 长短边比例不超过 3:1：再极端的画幅模型基本画不好，属于花钱买废图。
"""
from typing import Dict, List, Optional, Tuple

# 与 imaging.MAX_DIMENSION 对齐：输入图最长边会被缩到 2048，
# 出图尺寸没有理由超过它（否则补边后的参考图分辨率反而低于目标）
MAX_EDGE = 2048
MIN_EDGE = 512
STEP = 16
MAX_ASPECT = 3.0
MIN_PIXELS = 512 * 512
MAX_PIXELS = 2048 * 2048

# 短边基准：电商图 1024 足够用，再大只是徒增费用与等待
SHORT_SIDE = 1024

AUTO = "auto"

# 界面上的比例选项。usage 是给非技术用户看的「这个比例拿来干嘛」，
# 不是装饰——运营记不住 1024x1360，但记得住「详情页」。
PRESETS: List[Dict[str, str]] = [
    {"value": "1024x1024", "label": "方图", "ratio": "1:1", "usage": "淘宝/京东主图"},
    {"value": "1024x1360", "label": "竖图", "ratio": "3:4", "usage": "详情页、小红书"},
    {"value": "1024x1280", "label": "竖图", "ratio": "4:5", "usage": "社媒信息流"},
    {"value": "1024x1536", "label": "长竖图", "ratio": "2:3", "usage": "长图详情"},
    {"value": "1024x1824", "label": "超竖图", "ratio": "9:16", "usage": "抖音/快手封面"},
    {"value": "1536x1024", "label": "横图", "ratio": "3:2", "usage": "场景与横幅"},
    {"value": "1360x1024", "label": "横图", "ratio": "4:3", "usage": "PC 端banner"},
    {"value": "1824x1024", "label": "超横图", "ratio": "16:9", "usage": "视频封面"},
    {"value": AUTO, "label": "自动", "ratio": "自适应", "usage": "交给模型决定"},
]

_PRESET_BY_VALUE = {p["value"]: p for p in PRESETS}

# 常见比例表，用于给任意尺寸反查一个人类看得懂的比例标签。
# 顺序无关，取最接近的一个。
_COMMON_RATIOS: List[Tuple[int, int]] = [
    (1, 1), (5, 4), (4, 3), (3, 2), (16, 10), (16, 9), (2, 1), (21, 9),
    (4, 5), (3, 4), (2, 3), (10, 16), (9, 16), (1, 2), (9, 21),
]


def normalize_size(size: Optional[str]) -> str:
    """把用户/接口传来的尺寸串归一化：去空格、转小写。

    校验函数内部会自己归一化，但落库的是**原始串**——"1024X1024" 或带空格的值
    能通过校验、却在几分钟后被网关 400 拒掉，还白耗一次重试。所以入库前统一。
    """
    return str(size or "").strip().lower()


def parse_size(size: Optional[str]) -> Optional[Tuple[int, int]]:
    """把 "1024x1360" 解析成 (1024, 1360)；auto 或无法解析返回 None。"""
    text = str(size or "").strip().lower()
    if not text or text == AUTO or "x" not in text:
        return None
    left, _, right = text.partition("x")
    try:
        width, height = int(left), int(right)
    except ValueError:
        return None
    if width <= 0 or height <= 0:
        return None
    return width, height


def validate_size(size: Optional[str]) -> Optional[str]:
    """校验尺寸是否可用。通过返回 None，不通过返回中文原因。

    返回原因而不是抛异常，是为了让调用方自己决定是 422 还是仅提示。
    """
    text = str(size or "").strip().lower()
    if text == AUTO:
        return None
    parsed = parse_size(text)
    if not parsed:
        return "尺寸格式不正确，应形如 1024x1024，或填 auto"

    width, height = parsed
    if width % STEP or height % STEP:
        return "宽高必须是 %d 的倍数（模型会自行取整，否则出图尺寸对不上）" % STEP
    if min(width, height) < MIN_EDGE:
        return "最短边不能小于 %d" % MIN_EDGE
    if max(width, height) > MAX_EDGE:
        return "最长边不能大于 %d" % MAX_EDGE

    aspect = max(width, height) / float(min(width, height))
    if aspect > MAX_ASPECT:
        return "长短边比例不能超过 %g:1（过于狭长的画幅模型画不好）" % MAX_ASPECT

    pixels = width * height
    if pixels < MIN_PIXELS or pixels > MAX_PIXELS:
        return "总像素超出可用范围"
    return None


def resolve_ratio(ratio: str, short_side: int = SHORT_SIDE) -> Optional[str]:
    """把 "3:4" 反算成合法的像素尺寸串。算不出合法值时返回 None。"""
    text = str(ratio or "").strip()
    if ":" not in text:
        return None
    left, _, right = text.partition(":")
    try:
        rw, rh = float(left), float(right)
    except ValueError:
        return None
    if rw <= 0 or rh <= 0:
        return None

    if rw >= rh:
        height = short_side
        width = int(round(short_side * rw / rh / STEP)) * STEP
    else:
        width = short_side
        height = int(round(short_side * rh / rw / STEP)) * STEP

    candidate = "%dx%d" % (width, height)
    return candidate if validate_size(candidate) is None else None


def framing(size: Optional[str]) -> Optional[Tuple[str, str]]:
    """一次性给出**自洽的** (方向, 比例)，如 ("portrait", "3:4")。auto 返回 None。

    必须一起算：以前 ratio_label 带 2% 容差把 1040x1024 吸附成 "1:1"，
    orientation 却按精确像素判成 "landscape"，拼出来是自相矛盾的
    "a landscape 1:1 image"——而这句话会被钉在提示词的开头和结尾各一次，
    等于把最有效的比例控制手段变成一句废话。
    """
    preset = _PRESET_BY_VALUE.get(normalize_size(size))
    if preset and preset["value"] != AUTO:
        ratio = preset["ratio"]
    else:
        parsed = parse_size(size)
        if not parsed:
            return None
        width, height = parsed
        actual = width / float(height)
        best, best_delta = None, None
        for rw, rh in _COMMON_RATIOS:
            delta = abs(actual - rw / float(rh))
            if best_delta is None or delta < best_delta:
                best, best_delta = (rw, rh), delta
        # 不设容差门槛：与其在冷门尺寸上退回 gcd 算出 "64:65" 这类模型读不懂的比例，
        # 不如始终吸附到最接近的常见比例——画幅措辞的作用是让模型有个明确目标
        ratio = "%d:%d" % best if best else None
        if not ratio:
            return None

    left, _, right = ratio.partition(":")
    try:
        rw, rh = int(left), int(right)
    except ValueError:
        return None
    if rw == rh:
        return "square", ratio
    return ("landscape" if rw > rh else "portrait"), ratio


def ratio_label(size: Optional[str]) -> Optional[str]:
    """给尺寸配一个人类可读的比例标签，如 "3:4"。auto 返回 None。"""
    result = framing(size)
    return result[1] if result else None


def orientation(size: Optional[str]) -> Optional[str]:
    """square / landscape / portrait，用于拼英文提示词。与 ratio_label 保证自洽。"""
    result = framing(size)
    return result[0] if result else None


def presets_payload() -> List[Dict[str, str]]:
    """给前端的比例清单。前端不再各自硬编码，避免和后端校验漂移。"""
    return [dict(p) for p in PRESETS]
