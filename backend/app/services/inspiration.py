"""YouMind 灵感库同步：拉取开源提示词库 → 转换 → 幂等入库。

数据源是 YouMind 官方开源仓库（CC BY 4.0，每日更新两次）：
    https://github.com/YouMind-OpenLab/gpt-image-2-prompts-search

设计要点：
- 全部分类文件**整体校验通过后才落盘**（data/inspiration/ 缓存），避免半截数据；
- 上游的 ``{argument name="发色" default="silver"}`` 变量语法转换成本项目的
  ``{fa_se}`` 占位 + variables 定义，采用后可直接在生成页填空使用；
- 入库按 ``source_ref``（youmind:<id>）幂等：重复同步只更新有变化的条目；
- 一条提示词常同时属于多个分类：主分类取首个（列表页需要确定归属），
  但**全部标签**存进 source_slugs，按分类筛选与取风格参考时以全部标签为准
  （只认首个会让「电商主图」的 416 条只剩 18 条）；
- 同步在后台线程执行，前端轮询 sync_status；同一时间只允许一个同步在跑。
"""
import json
import random
import logging
import re
import threading
import time
import unicodedata
import urllib.error
import urllib.request
from datetime import datetime
from typing import Any, Dict, List, Optional, Tuple

from ..config import DATA_DIR
from ..database import SessionLocal
from ..models import PromptCategory, PromptTemplate

logger = logging.getLogger("designkit.inspiration")

RAW_BASE = (
    "https://raw.githubusercontent.com/"
    "YouMind-OpenLab/gpt-image-2-prompts-search/main/references"
)
WEB_URL = "https://youmind.com/gpt-image-2-prompts?id={id}"
SOURCE_PREFIX = "youmind:"
CACHE_DIR = DATA_DIR / "inspiration"

# 顺序即上游 manifest 的顺序（主分类取首个，依赖此顺序；筛选用 source_slugs 全量）
CATEGORIES: Dict[str, str] = {
    "profile-avatar": "头像与个人形象",
    "social-media-post": "社交媒体帖子",
    "infographic-edu-visual": "信息图与教育视觉",
    "youtube-thumbnail": "YouTube 封面",
    "comic-storyboard": "漫画与分镜",
    "product-marketing": "产品营销",
    "ecommerce-main-image": "电商主图",
    "game-asset": "游戏素材",
    "poster-flyer": "海报与传单",
    "app-web-design": "应用与网页设计",
    "others": "灵感库·未分类",
}

# 电商用得上的分类，按贴合度排序。生成页默认只显示这几个，其余折叠——
# 上游有一半分类（漫画分镜、游戏素材、YouTube 封面…）拿来出商品图纯属误导。
ECOMMERCE_SLUGS = [
    "ecommerce-main-image",
    "product-marketing",
    "social-media-post",
    "poster-flyer",
    "infographic-edu-visual",
]

# 兼容两种引号形态：裸引号 name="..."，以及整体被转义的 name=\"...\"（上游约 8% 条目），
# 值内允许 \" 等转义字符
_ARG_RE = re.compile(
    r'\{argument\s+name=\\?"((?:[^"\\]|\\.)+?)\\?"'
    r'(?:\s+default=\\?"((?:[^"\\]|\\.)*?)\\?")?\s*\}'
)


def _unescape(value: str) -> str:
    return value.replace('\\"', '"').replace("\\\\", "\\")

# ------------------------------------------------------------------ 同步状态

_status_lock = threading.Lock()
_status: Dict[str, Any] = {"state": "idle", "message": "尚未同步过"}


def get_status() -> Dict[str, Any]:
    with _status_lock:
        return dict(_status)


def _set_status(**kw: Any) -> None:
    with _status_lock:
        _status.update(kw)


# ------------------------------------------------------------------ 变量语法转换

def _collapse(text: Optional[str]) -> str:
    """折叠换行与连续空白——上游约 40 条 title 内含换行，不折会渲染错乱。"""
    return re.sub(r"\s+", " ", str(text or "")).strip()


def _sanitize_var_name(label: str) -> str:
    """把任意变量名（可能含空格/中文/符号）转成本项目占位符允许的形式。"""
    normalized = unicodedata.normalize("NFKC", label).strip().lower()
    key = re.sub(r"[^a-z0-9_一-鿿]+", "_", normalized).strip("_")
    if not key:
        key = "var"
    if key[0].isdigit():
        key = "v_" + key
    return key[:40]


def convert_prompt(content: str) -> Tuple[str, List[dict]]:
    """把上游的 {argument name=.. default=..} 转成 {占位符} + variables 定义。

    同名变量多次出现共用一个占位符；不同变量清洗后撞名时自动加序号。
    """
    variables: List[dict] = []
    label_to_key: Dict[str, str] = {}
    used_keys: set = set()

    def repl(match: "re.Match") -> str:
        label = _unescape(match.group(1))
        default = _unescape(match.group(2) or "")
        if label in label_to_key:
            return "{%s}" % label_to_key[label]
        key = _sanitize_var_name(label)
        base, i = key, 2
        while key in used_keys:
            key = "%s_%d" % (base, i)
            i += 1
        used_keys.add(key)
        label_to_key[label] = key
        variables.append({
            "name": key, "label": label, "type": "text",
            "default": default, "required": False,
        })
        return "{%s}" % key

    return _ARG_RE.sub(repl, content or "").strip(), variables


# ------------------------------------------------------------------ 拉取

def fetch_to_cache(timeout: int = 120) -> Dict[str, Any]:
    """下载 manifest + 全部分类文件；全部校验合法后才写入缓存目录。"""
    staged: Dict[str, bytes] = {}
    manifest: Dict[str, Any] = {}
    for slug in list(CATEGORIES) + ["manifest"]:
        url = "%s/%s.json" % (RAW_BASE, slug)
        _set_status(message="正在下载 %s.json" % slug)
        try:
            req = urllib.request.Request(url, headers={"User-Agent": "DesignKit/1.0"})
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                raw = resp.read()
        except urllib.error.URLError as exc:
            raise RuntimeError(
                "下载 %s.json 失败：%s（如果服务器访问 GitHub 受限，可配置代理后重试）"
                % (slug, exc)
            ) from exc
        try:
            data = json.loads(raw.decode("utf-8"))
        except json.JSONDecodeError as exc:
            raise RuntimeError("%s.json 内容不完整或不是合法 JSON" % slug) from exc
        staged[slug] = raw
        if slug == "manifest":
            manifest = data

    CACHE_DIR.mkdir(parents=True, exist_ok=True)
    for slug, raw in staged.items():
        (CACHE_DIR / ("%s.json" % slug)).write_bytes(raw)
    return manifest


def load_cache() -> Dict[int, dict]:
    """把缓存读成 {id: 条目}，附带按固定顺序合并的 categories 列表。"""
    merged: Dict[int, dict] = {}
    for slug in CATEGORIES:
        path = CACHE_DIR / ("%s.json" % slug)
        if not path.exists():
            continue
        with open(path, encoding="utf-8") as fh:
            for item in json.load(fh):
                pid = int(item["id"])
                if pid in merged:
                    merged[pid]["_slugs"].append(slug)
                else:
                    entry = dict(item)
                    entry["_slugs"] = [slug]
                    merged[pid] = entry
    return merged


# ------------------------------------------------------------------ 入库

def _get_or_create_categories(db) -> Dict[str, int]:
    """确保 11 个灵感库分类存在，返回 slug → category_id。"""
    existing = {c.name: c for c in db.query(PromptCategory).all()}
    max_sort = max((c.sort or 0 for c in existing.values()), default=0)
    mapping: Dict[str, int] = {}
    for i, (slug, name) in enumerate(CATEGORIES.items()):
        cat = existing.get(name)
        if cat is None:
            cat = PromptCategory(name=name, sort=max_sort + 10 + i)
            db.add(cat)
            db.flush()
            existing[name] = cat
        mapping[slug] = cat.id
    db.commit()
    return mapping


def import_cache_to_db() -> Dict[str, int]:
    """把缓存导入模板表（source=youmind, is_enabled=False），按 source_ref 幂等。"""
    entries = load_cache()
    if not entries:
        raise RuntimeError("缓存为空，请先完成下载")

    db = SessionLocal()
    created = updated = unchanged = 0
    try:
        slug_to_cat = _get_or_create_categories(db)
        existing: Dict[str, PromptTemplate] = {
            t.source_ref: t
            for t in db.query(PromptTemplate)
            .filter(PromptTemplate.source == "youmind")
            .all()
        }
        total = len(entries)
        for i, (pid, item) in enumerate(sorted(entries.items())):
            if i % 500 == 0:
                _set_status(message="正在入库 %d / %d" % (i, total))
                db.commit()
            ref = SOURCE_PREFIX + str(pid)
            prompt_text, variables = convert_prompt(item.get("content") or "")
            if not prompt_text:
                continue
            name = (_collapse(item.get("title")) or ("Prompt #%d" % pid))[:128]
            media = item.get("sourceMedia") or []
            thumb = media[0] if media and str(media[0]).startswith("http") else None
            # 列宽 256：上游偶发的超长 URL 会让整轮同步在 PostgreSQL 上失败，
            # 宁可这条没有缩略图，也不能拖垮全库同步
            if thumb and len(thumb) > 256:
                thumb = None
            description = _collapse(item.get("description"))
            attribution = "来源：YouMind 提示词库（CC BY 4.0）%s" % WEB_URL.format(id=pid)
            description = (description + "\n" + attribution) if description else attribution

            # 主分类仍取第一个标签（列表页要有个确定归属），但把**全部**标签也存下来，
            # 按分类筛选时用全部标签判断——否则「电商主图」的 416 条里有 398 条
            # 会被排在它前面的分类文件抢走，用户按分类找几乎什么都找不到
            slugs = list(item["_slugs"])
            slugs_json = json.dumps(slugs, ensure_ascii=False)
            target_category = slug_to_cat.get(slugs[0])
            row = existing.get(ref)
            if row is None:
                db.add(PromptTemplate(
                    name=name,
                    source="youmind",
                    source_ref=ref,
                    category_id=target_category,
                    source_slugs=slugs_json,
                    description=description,
                    prompt_template=prompt_text,
                    variables=variables,
                    default_params={},
                    requires_input_image=bool(item.get("needReferenceImages")),
                    thumbnail_path=thumb,
                    is_enabled=False,  # 灵感库条目不进生成页，采用后才有正式副本
                    sort=0,
                ))
                created += 1
            else:
                if (
                    row.name == name
                    and row.prompt_template == prompt_text
                    and row.description == description
                    and row.requires_input_image == bool(item.get("needReferenceImages"))
                    and row.thumbnail_path == thumb
                    and row.category_id == target_category  # 分类被误删后同步能修回来
                    and row.source_slugs == slugs_json
                ):
                    unchanged += 1
                    continue
                row.name = name
                row.prompt_template = prompt_text
                row.description = description
                row.variables = variables
                row.requires_input_image = bool(item.get("needReferenceImages"))
                row.thumbnail_path = thumb
                row.category_id = target_category
                row.source_slugs = slugs_json
                updated += 1
        db.commit()
        return {"created": created, "updated": updated, "unchanged": unchanged, "total": total}
    finally:
        db.close()


# ------------------------------------------------------------------ 后台同步

def start_sync() -> bool:
    """手动触发同步。

    实际执行与加锁都在 services.scheduler.run_sync 里——手动与自动必须共用同一把
    数据库锁，否则两路会同时导入、把上万条灵感库写成重复的两份。
    这里放在函数内导入是为了避免与 scheduler 形成模块级循环依赖。
    """
    from . import scheduler as _scheduler

    holder = {}

    def _worker():
        holder["ran"], holder["msg"] = _scheduler.run_sync(trigger="manual")

    thread = threading.Thread(target=_worker, daemon=True)
    thread.start()
    thread.join(timeout=2)  # 抢锁很快，短等一下就能知道有没有抢到
    if holder.get("ran") is False:
        return False
    return True


# ------------------------------------------------------- 按分类取风格参考

# 挑中的参考给全文（带截断保护），模型要照着它写布景与光线；
# 条数少，所以即便给全文 token 也可控。
REFERENCE_FULL_CHARS = 1800
REFERENCE_PICK_COUNT = 4

# 一次性发给模型「浏览」的标题条数上限。电商主图 416 条 ≈ 3400 token，完全够用；
# 社交媒体帖子这类上万条的会先按「像不像商品摄影」预筛，避免单次调用膨胀到几万 token。
DIGEST_LIMIT = 700


def slug_filter(slug: str):
    """按「全部标签」筛选的 SQL 条件。

    source_slugs 是 JSON 数组文本，用 LIKE 匹配带引号的完整 slug，
    避免 product-marketing 误命中 product-marketing-extra 这类前缀。
    14.5k 行的全表 LIKE 在 SQLite 与 PostgreSQL 上都在毫秒级，不值得为它建索引。
    """
    return PromptTemplate.source_slugs.like('%"' + slug + '"%')


# 相关度预筛：上游语料以人像/动漫/美食居多，直接随机抽会浪费大半个参考位
# （实测：电商主图 66% 是商品摄影，产品营销只有 31%、海报与传单只有 17%）。
# 先挑「像商品摄影」的，不够再用其余补齐。纯正则、几毫秒，不需要额外的列。
_PRODUCT_WORDS = re.compile(
    r"\b(product (?:photo|shot|photograph|photography)|packshot|packaging|"
    r"studio (?:light|lighting|setup|background|shot)|commercial photograph|"
    r"e-?commerce|bottle|can\b|jar|tube|cosmetic|skincare|perfume|sneaker|watch|"
    r"softbox|seamless background|white background|catalog|hero shot)\b",
    re.I,
)
# 标题里出现这些，基本可以断定主体不是商品
_OFF_TOPIC_TITLE = re.compile(
    r"\b(anime|manga|character|portrait|woman|man|girl|boy|schoolgirl|comic|"
    r"storyboard|avatar|selfie|recipe|diagram|slide|resume|cv)\b",
    re.I,
)


def _is_product_like(row) -> bool:
    if _OFF_TOPIC_TITLE.search(row.name or ""):
        return False
    return bool(_PRODUCT_WORDS.search(row.prompt_template or ""))


def _digest_rows(db, slug: str) -> List[PromptTemplate]:
    """取该分类下的条目。超过 DIGEST_LIMIT 时先按「像不像商品摄影」预筛，
    避免把上万条标题塞进一次调用（社交媒体帖子这类有 5000+ 条）。"""
    rows = (
        db.query(PromptTemplate)
        .filter(PromptTemplate.source == "youmind")
        .filter(slug_filter(slug))
        .order_by(PromptTemplate.id.asc())
        .all()
    )
    if len(rows) <= DIGEST_LIMIT:
        return rows
    preferred = [r for r in rows if _is_product_like(r)]
    if len(preferred) >= DIGEST_LIMIT:
        return preferred[:DIGEST_LIMIT]
    rest = [r for r in rows if not _is_product_like(r)]
    return preferred + rest[: DIGEST_LIMIT - len(preferred)]


def category_digest(db, slug: str) -> List[dict]:
    """该分类的「精简版清单」：每条只有编号和标题。

    上游每条自带 title（100% 覆盖、平均 33 字），本身就是一句话概括，
    不需要再花 1.4 万次调用去生成摘要。电商主图 416 条 ≈ 3400 token，
    可以一次性发给模型让它通览后挑选。
    """
    return [
        {"id": row.id, "title": _collapse(row.name)[:80]}
        for row in _digest_rows(db, slug)
    ]


def fetch_references(db, ids: List[int]) -> List[dict]:
    """按模型挑出的编号取回**完整**提示词，供它照着写布景与光线。"""
    if not ids:
        return []
    rows = (
        db.query(PromptTemplate)
        .filter(PromptTemplate.source == "youmind")
        .filter(PromptTemplate.id.in_(ids[:REFERENCE_PICK_COUNT]))
        .all()
    )
    by_id = {r.id: r for r in rows}
    picked = []
    for pid in ids[:REFERENCE_PICK_COUNT]:      # 保持模型给出的先后顺序
        row = by_id.get(pid)
        if row is None:
            continue
        picked.append({
            "title": _collapse(row.name),
            "excerpt": _collapse(row.prompt_template)[:REFERENCE_FULL_CHARS],
        })
    return picked


def backfill_slugs_if_needed() -> int:
    """老库升级后回填 source_slugs：用**本地缓存**补，不联网。

    为什么必须自愈：加了这一列之后、在下一次同步之前，所有条目的标签都是空的，
    而生成页的分类清单正是按标签统计条数的——用户会看到「一个分类都没有」，
    看起来就像系统坏了。而下次自动同步最长要等 12 小时。
    缓存文件就在 data/inspiration/ 下，本地重放一遍即可，十几秒。
    """
    db = SessionLocal()
    try:
        pending = (
            db.query(PromptTemplate)
            .filter(PromptTemplate.source == "youmind")
            .filter(PromptTemplate.source_slugs.is_(None))
            .count()
        )
    finally:
        db.close()
    if not pending:
        return 0
    if not any((CACHE_DIR / ("%s.json" % s)).exists() for s in CATEGORIES):
        logger.warning("有 %d 条灵感库条目缺少分类标签，但本地缓存不存在；"
                       "请到「灵感库」点一次「同步上游」", pending)
        return 0
    logger.info("正在用本地缓存回填 %d 条灵感库条目的分类标签…", pending)
    result = import_cache_to_db()
    logger.info("分类标签回填完成：%s", result)
    return pending
