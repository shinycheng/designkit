"""灵感库：浏览/搜索 YouMind 开源提示词库，看中的一键采用为自己的模板。"""
from typing import Optional

from fastapi import APIRouter, Depends, HTTPException, Query
from sqlalchemy import func, or_
from sqlalchemy.orm import Session

from ..deps import get_current_user, get_db, require_admin
from ..models import PromptCategory, PromptTemplate, User
from ..serializers import adapt_prompt_to_uploaded_product, template_to_dict
from ..services import inspiration, scheduler as sync_scheduler

router = APIRouter(prefix="/api/web/inspiration", tags=["网页-灵感库"])


def _to_public_dict(t: PromptTemplate, adopted_refs: set) -> dict:
    data = template_to_dict(t, "")
    ref = t.source_ref or ""
    data["source"] = t.source
    data["adopted"] = ref in adopted_refs
    if ref.startswith(inspiration.SOURCE_PREFIX):
        data["source_url"] = inspiration.WEB_URL.format(
            id=ref[len(inspiration.SOURCE_PREFIX):]
        )
    return data


@router.get("")
def browse(
    q: Optional[str] = None,
    category_id: Optional[int] = None,
    requires_image: Optional[bool] = None,
    adaptable_only: bool = False,
    page: int = Query(1, ge=1),
    page_size: int = Query(24, ge=1, le=50),
    user: User = Depends(get_current_user),
    db: Session = Depends(get_db),
):
    query = db.query(PromptTemplate).filter(PromptTemplate.source == "youmind")
    if category_id is not None:
        query = query.filter(PromptTemplate.category_id == category_id)
    if requires_image is not None:
        query = query.filter(PromptTemplate.requires_input_image.is_(requires_image))
    if adaptable_only:
        # 只看「会参照你上传的商品图」的条目——库里约九成条目自带具体商品描述，
        # 直接套用会画出它自己的商品，这个开关把它们过滤掉
        clauses = [
            PromptTemplate.prompt_template.icontains(kw, autoescape=True)
            for kw in ("uploaded image", "uploaded photo", "reference image",
                       "provided image", "attached image", "input image",
                       "from the image", "the uploaded", "上传的图", "参考图")
        ]
        query = query.filter(or_(*clauses))
    if q and q.strip():
        keyword = q.strip()
        # icontains：转义 % 和 _，且在 SQLite/PostgreSQL 上都不区分大小写
        query = query.filter(
            PromptTemplate.name.icontains(keyword, autoescape=True)
            | PromptTemplate.description.icontains(keyword, autoescape=True)
            | PromptTemplate.prompt_template.icontains(keyword, autoescape=True)
        )
    total = query.count()
    rows = (
        query.order_by(PromptTemplate.id.desc())
        .offset((page - 1) * page_size)
        .limit(page_size)
        .all()
    )

    # 本页条目里哪些已被采用过（存在同 source_ref 的 user 模板）
    refs = [t.source_ref for t in rows if t.source_ref]
    adopted_refs = set()
    if refs:
        adopted_refs = {
            r[0]
            for r in db.query(PromptTemplate.source_ref)
            .filter(PromptTemplate.source == "user")
            .filter(PromptTemplate.source_ref.in_(refs))
            .all()
        }

    # 各分类的条目数（用于前端分类筛选）
    facets = [
        {"id": cid, "name": name, "count": count}
        for cid, name, count in (
            db.query(PromptCategory.id, PromptCategory.name, func.count(PromptTemplate.id))
            .join(PromptTemplate, PromptTemplate.category_id == PromptCategory.id)
            .filter(PromptTemplate.source == "youmind")
            .group_by(PromptCategory.id)
            .order_by(PromptCategory.sort.asc())
            .all()
        )
    ]

    return {
        "total": total,
        "page": page,
        "page_size": page_size,
        "items": [_to_public_dict(t, adopted_refs) for t in rows],
        "categories": facets,
        "sync": inspiration.get_status(),
        "auto_sync": sync_scheduler.get_state(),
    }


@router.post("/sync")
def trigger_sync(_: User = Depends(require_admin)):
    """后台拉取 YouMind 上游并导入；用 sync_status 轮询进度。"""
    if not inspiration.start_sync():
        raise HTTPException(status_code=409, detail="已有同步任务在进行中")
    return inspiration.get_status()


@router.get("/sync_status")
def sync_status(_: User = Depends(get_current_user)):
    status = dict(inspiration.get_status())
    status["auto_sync"] = sync_scheduler.get_state()
    return status


@router.post("/{template_id}/adopt")
def adopt(
    template_id: int,
    _: User = Depends(require_admin),
    db: Session = Depends(get_db),
):
    """把灵感库条目复制成一条自己的正式模板（可自由改，不受后续同步影响）。"""
    src = db.get(PromptTemplate, template_id)
    if src is None or src.source != "youmind":
        raise HTTPException(status_code=404, detail="灵感库条目不存在")

    existing = (
        db.query(PromptTemplate)
        .filter(PromptTemplate.source == "user")
        .filter(PromptTemplate.source_ref == src.source_ref)
        .first()
    )
    if existing is not None:
        return {"ok": True, "already": True, "template": template_to_dict(existing, "")}

    # 上游同名条目不少（截断后 261 组重名），循环探测直到名字不冲突，
    # 否则重名模板会在「导出→导入」备份恢复时被按名合并、静默丢内容
    name = src.name
    suffix_index = 1
    while db.query(PromptTemplate).filter(
        PromptTemplate.source == "user", PromptTemplate.name == name
    ).first():
        tag = "（灵感库）" if suffix_index == 1 else "（灵感库%d）" % suffix_index
        name = (src.name[: 128 - len(tag)] + tag)
        suffix_index += 1

    prompt_text = adapt_prompt_to_uploaded_product(src.prompt_template)

    copy = PromptTemplate(
        name=name,
        source="user",
        source_ref=src.source_ref,
        category_id=src.category_id,
        description=src.description,
        prompt_template=prompt_text,
        variables=list(src.variables or []),
        default_params=dict(src.default_params or {}),
        requires_input_image=bool(src.requires_input_image),
        thumbnail_path=src.thumbnail_path,
        is_enabled=True,
        sort=0,
    )
    db.add(copy)
    db.commit()
    db.refresh(copy)
    return {"ok": True, "already": False, "template": template_to_dict(copy, "")}
