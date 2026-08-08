from typing import Any, Dict, List, Optional

from fastapi import APIRouter, Depends, HTTPException, Query
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session

from ..deps import get_current_user, get_db
from ..models import GenerationJob, User
from ..serializers import job_to_dict
from ..services import settings_service, storage
from ..services.jobs import create_job, resolve_upload_paths

router = APIRouter(prefix="/api/web/generations", tags=["网页-生成任务"])


class GenerationCreateBody(BaseModel):
    template_id: Optional[int] = None
    prompt: Optional[str] = None
    variables: Dict[str, Any] = {}
    extra_instructions: Optional[str] = None
    upload_ids: List[int] = []
    n: Optional[int] = Field(default=None, ge=1, le=4)
    size: Optional[str] = None
    quality: Optional[str] = None


@router.post("")
def create_generation(
    body: GenerationCreateBody,
    user: User = Depends(get_current_user),
    db: Session = Depends(get_db),
):
    input_paths = resolve_upload_paths(db, body.upload_ids, user_id=user.id)
    job = create_job(
        db,
        source="web",
        user_id=user.id,
        template_id=body.template_id,
        prompt=body.prompt,
        variables=body.variables,
        extra_instructions=body.extra_instructions,
        input_paths=input_paths,
        n=body.n,
        size=body.size,
        quality=body.quality,
    )
    public_base = ""  # 网页端用相对 /files 路径，换访问入口也不会失效
    return job_to_dict(job, public_base)


@router.get("")
def list_generations(
    page: int = Query(1, ge=1),
    page_size: int = Query(12, ge=1, le=50),
    status: Optional[str] = None,
    q: Optional[str] = None,
    user: User = Depends(get_current_user),
    db: Session = Depends(get_db),
):
    query = db.query(GenerationJob)
    # 管理员能看到所有任务（含 ERP 通过 API 提交的），普通成员只看自己的
    if user.role != "admin":
        query = query.filter(GenerationJob.user_id == user.id)
    if status:
        query = query.filter(GenerationJob.status == status)
    if q:
        like = "%%%s%%" % q.strip()
        query = query.filter(
            GenerationJob.prompt_final.like(like) | GenerationJob.template_name.like(like)
        )
    total = query.count()
    rows = (
        query.order_by(GenerationJob.created_at.desc())
        .offset((page - 1) * page_size)
        .limit(page_size)
        .all()
    )
    public_base = ""  # 网页端用相对 /files 路径，换访问入口也不会失效
    return {
        "total": total,
        "page": page,
        "page_size": page_size,
        "items": [job_to_dict(j, public_base) for j in rows],
    }


def _get_own_job(job_id: str, user: User, db: Session) -> GenerationJob:
    job = db.get(GenerationJob, job_id)
    if job is None:
        raise HTTPException(status_code=404, detail="任务不存在")
    if user.role != "admin" and job.user_id != user.id:
        raise HTTPException(status_code=403, detail="无权查看该任务")
    return job


@router.get("/{job_id}")
def get_generation(
    job_id: str, user: User = Depends(get_current_user), db: Session = Depends(get_db)
):
    job = _get_own_job(job_id, user, db)
    return job_to_dict(job, "")  # 相对 /files 路径


@router.post("/{job_id}/retry")
def retry_generation(
    job_id: str, user: User = Depends(get_current_user), db: Session = Depends(get_db)
):
    job = _get_own_job(job_id, user, db)
    if job.status not in ("failed", "succeeded"):
        raise HTTPException(status_code=409, detail="任务还在进行中，不能重试")
    # 清掉旧结果，避免重跑后 images 累积（worker 侧也有兜底清理）
    for img in list(job.images or []):
        storage.delete_file(img.path)
        storage.delete_file(img.thumb_path)
        db.delete(img)
    job.status = "pending"
    job.attempts = 0
    job.error = ""
    job.next_attempt_at = None
    job.finished_at = None
    db.commit()
    return job_to_dict(job, "")  # 相对 /files 路径


@router.delete("/{job_id}")
def delete_generation(
    job_id: str, user: User = Depends(get_current_user), db: Session = Depends(get_db)
):
    job = _get_own_job(job_id, user, db)
    if job.status == "processing":
        raise HTTPException(status_code=409, detail="任务正在执行，无法删除")
    for img in list(job.images or []):
        storage.delete_file(img.path)
        storage.delete_file(img.thumb_path)
    db.delete(job)
    db.commit()
    return {"ok": True}
