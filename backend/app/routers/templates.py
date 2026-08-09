from typing import Any, Dict, List, Optional

from fastapi import APIRouter, Depends, File, HTTPException, UploadFile
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session

from ..deps import get_current_user, get_db, require_admin
from ..models import PromptCategory, PromptTemplate, User
from ..serializers import template_to_dict
from ..services import settings_service, sizing, storage
from .uploads import read_and_validate

router = APIRouter(prefix="/api/web", tags=["网页-提示词模板"])


@router.get("/size-presets")
def list_size_presets(_: User = Depends(get_current_user)):
    """出图比例预设。前端三处曾各自硬编码同一份清单，加一个比例要改五个地方
    且极易漏改（表现为「设置页能选、提交时 422」）。改为统一从这里取。
    """
    return {"presets": sizing.presets_payload()}


class TemplateBody(BaseModel):
    name: str = Field(min_length=1, max_length=128)
    category_id: Optional[int] = None
    description: str = ""
    prompt_template: str = Field(min_length=1)
    variables: List[Dict[str, Any]] = []
    default_params: Dict[str, Any] = {}
    requires_input_image: bool = True
    is_enabled: bool = True
    sort: int = 0


class CategoryBody(BaseModel):
    name: str = Field(min_length=1, max_length=64)
    sort: int = 0


def _apply_template_body(t: PromptTemplate, body: TemplateBody, db: Session) -> None:
    if body.category_id is not None and db.get(PromptCategory, body.category_id) is None:
        raise HTTPException(status_code=404, detail="分类不存在")
    t.name = body.name.strip()
    t.category_id = body.category_id
    t.description = body.description or ""
    t.prompt_template = body.prompt_template
    t.variables = body.variables or []
    t.default_params = body.default_params or {}
    t.requires_input_image = bool(body.requires_input_image)
    t.is_enabled = bool(body.is_enabled)
    t.sort = int(body.sort or 0)


# ------------------------------------------------------------------ 模板

@router.get("/templates")
def list_templates(
    q: Optional[str] = None,
    category_id: Optional[int] = None,
    include_disabled: bool = False,
    user: User = Depends(get_current_user),
    db: Session = Depends(get_db),
):
    # 只列正式模板；灵感库条目（source=youmind）走 /api/web/inspiration
    query = db.query(PromptTemplate).filter(PromptTemplate.source == "user")
    if not (include_disabled and user.role == "admin"):
        query = query.filter(PromptTemplate.is_enabled.is_(True))
    if category_id is not None:
        query = query.filter(PromptTemplate.category_id == category_id)
    if q:
        keyword = q.strip()
        query = query.filter(
            PromptTemplate.name.icontains(keyword, autoescape=True)
            | PromptTemplate.description.icontains(keyword, autoescape=True)
        )
    rows = query.order_by(PromptTemplate.sort.asc(), PromptTemplate.id.desc()).all()
    public_base = ""  # 网页端用相对 /files 路径，换访问入口也不会失效
    return [template_to_dict(t, public_base) for t in rows]


@router.post("/templates")
def create_template(
    body: TemplateBody, _: User = Depends(require_admin), db: Session = Depends(get_db)
):
    t = PromptTemplate(prompt_template="", name="")
    _apply_template_body(t, body, db)
    db.add(t)
    db.commit()
    db.refresh(t)
    return template_to_dict(t, "")  # 相对 /files 路径


@router.put("/templates/{template_id}")
def update_template(
    template_id: int,
    body: TemplateBody,
    _: User = Depends(require_admin),
    db: Session = Depends(get_db),
):
    t = db.get(PromptTemplate, template_id)
    if t is None or t.source != "user":
        raise HTTPException(status_code=404, detail="模板不存在")
    _apply_template_body(t, body, db)
    db.commit()
    db.refresh(t)
    return template_to_dict(t, "")  # 相对 /files 路径


@router.delete("/templates/{template_id}")
def delete_template(
    template_id: int, _: User = Depends(require_admin), db: Session = Depends(get_db)
):
    t = db.get(PromptTemplate, template_id)
    if t is None or t.source != "user":
        raise HTTPException(status_code=404, detail="模板不存在")
    storage.delete_file(t.thumbnail_path)
    db.delete(t)
    db.commit()
    return {"ok": True}


@router.post("/templates/{template_id}/thumbnail")
async def upload_thumbnail(
    template_id: int,
    file: UploadFile = File(...),
    _: User = Depends(require_admin),
    db: Session = Depends(get_db),
):
    t = db.get(PromptTemplate, template_id)
    if t is None or t.source != "user":
        raise HTTPException(status_code=404, detail="模板不存在")
    data, suffix = await read_and_validate(file)
    try:
        rel, _w, _h = storage.save_upload(data, suffix)
    except storage.StorageError as e:
        raise HTTPException(status_code=422, detail=str(e))
    storage.delete_file(t.thumbnail_path)
    t.thumbnail_path = rel
    db.commit()
    return template_to_dict(t, "")  # 相对 /files 路径


@router.post("/templates/import")
def import_templates(
    items: List[TemplateBody], _: User = Depends(require_admin), db: Session = Depends(get_db)
):
    """批量导入提示词库：传模板数组，同名模板会被更新，其余新建。"""
    created, updated = 0, 0
    seen: dict = {}  # 本批次内已处理的同名模板，避免同批重复创建
    for body in items:
        name = body.name.strip()
        existing = seen.get(name)
        if existing is None:
            existing = (
                db.query(PromptTemplate)
                .filter(PromptTemplate.source == "user")  # 同名更新只针对自己的模板
                .filter(PromptTemplate.name == name)
                .first()
            )
        if existing is not None:
            _apply_template_body(existing, body, db)
            updated += 1
            seen[name] = existing
        else:
            t = PromptTemplate(prompt_template="", name="")
            _apply_template_body(t, body, db)
            db.add(t)
            db.flush()  # 拿到对象供同批次后续同名项更新
            created += 1
            seen[name] = t
    db.commit()
    return {"ok": True, "created": created, "updated": updated}


@router.get("/templates/export")
def export_templates(_: User = Depends(require_admin), db: Session = Depends(get_db)):
    # 只导出自己的正式模板；1.4 万条灵感库数据不属于用户资产，也不该进备份文件
    rows = (
        db.query(PromptTemplate)
        .filter(PromptTemplate.source == "user")
        .order_by(PromptTemplate.id.asc())
        .all()
    )
    return [
        {
            "name": t.name,
            "category_id": t.category_id,
            "description": t.description or "",
            "prompt_template": t.prompt_template,
            "variables": t.variables or [],
            "default_params": t.default_params or {},
            "requires_input_image": bool(t.requires_input_image),
            "is_enabled": bool(t.is_enabled),
            "sort": t.sort or 0,
        }
        for t in rows
    ]


# ------------------------------------------------------------------ 分类

@router.get("/categories")
def list_categories(user: User = Depends(get_current_user), db: Session = Depends(get_db)):
    rows = db.query(PromptCategory).order_by(PromptCategory.sort.asc(), PromptCategory.id.asc()).all()
    return [{"id": c.id, "name": c.name, "sort": c.sort or 0} for c in rows]


@router.post("/categories")
def create_category(
    body: CategoryBody, _: User = Depends(require_admin), db: Session = Depends(get_db)
):
    if db.query(PromptCategory).filter(PromptCategory.name == body.name.strip()).first():
        raise HTTPException(status_code=409, detail="分类已存在")
    c = PromptCategory(name=body.name.strip(), sort=body.sort)
    db.add(c)
    db.commit()
    db.refresh(c)
    return {"id": c.id, "name": c.name, "sort": c.sort or 0}


@router.put("/categories/{category_id}")
def update_category(
    category_id: int,
    body: CategoryBody,
    _: User = Depends(require_admin),
    db: Session = Depends(get_db),
):
    c = db.get(PromptCategory, category_id)
    if c is None:
        raise HTTPException(status_code=404, detail="分类不存在")
    c.name = body.name.strip()
    c.sort = body.sort
    db.commit()
    return {"id": c.id, "name": c.name, "sort": c.sort or 0}


@router.delete("/categories/{category_id}")
def delete_category(
    category_id: int, _: User = Depends(require_admin), db: Session = Depends(get_db)
):
    c = db.get(PromptCategory, category_id)
    if c is None:
        raise HTTPException(status_code=404, detail="分类不存在")
    db.query(PromptTemplate).filter(PromptTemplate.category_id == category_id).update(
        {PromptTemplate.category_id: None}
    )
    db.delete(c)
    db.commit()
    return {"ok": True}
