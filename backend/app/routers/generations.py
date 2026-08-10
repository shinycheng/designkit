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
    category_slug: Optional[str] = None
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
        category_slug=body.category_slug,
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
        keyword = q.strip()
        # icontains：转义通配符，且跨 SQLite/PostgreSQL 都不区分大小写
        query = query.filter(
            GenerationJob.prompt_final.icontains(keyword, autoescape=True)
            | GenerationJob.template_name.icontains(keyword, autoescape=True)
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


# 越权和不存在返回同一个 404。
# 为什么不用 403：如果「不存在→404、越权→403」，两个码一比就是一台任务存在性
# 探测器——而 job_id 会出现在 ERP 的 202 响应体、webhook 回调、反代访问日志里，
# 拿到一串 id 就能挨个试出哪些是真的、进而摸清别人有多少单。
# services/jobs.py 的上传归属校验和 v1.py 的任务查询本来就是统一 404，这里对齐。
_JOB_NOT_FOUND = "任务不存在"


def _get_readable_job(job_id: str, user: User, db: Session) -> GenerationJob:
    """取一条**可读**的任务：本人的，管理员则可读所有人的。

    管理员放行是刻意保留的：成员说「我的图出不来」时，得有人能点开那条任务
    看 error 和参数，否则排障只能去翻数据库。
    """
    job = db.get(GenerationJob, job_id)
    if job is None:
        raise HTTPException(status_code=404, detail=_JOB_NOT_FOUND)
    if user.role != "admin" and job.user_id != user.id:
        raise HTTPException(status_code=404, detail=_JOB_NOT_FOUND)
    return job


def _get_writable_job(job_id: str, user: User, db: Session) -> GenerationJob:
    """取一条**可写**的任务：一律要求是本人的，**管理员也不例外**。

    和 _get_readable_job 的不对称是故意的，不是漏改：
    「重新生成」和「补图」会立刻拿**任务归属人**的网关额度去出图，花的是那个
    成员的真金白银，且发出去就收不回来（retry 只判状态、不计次也不问一句）。
    管理员点错一下，账单落在别人头上，事后谁也说不清。
    删除不花钱，所以 delete 仍然走 readable，留给管理员做清理。

    job.user_id 为 None 的是历史上没有主人的 ERP 任务（P1-3 的回填会把绝大多数
    补上），这类没人认领的任务只让管理员操作，否则它们会彻底动不了。
    """
    job = _get_readable_job(job_id, user, db)
    if job.user_id != user.id and not (job.user_id is None and user.role == "admin"):
        raise HTTPException(status_code=404, detail=_JOB_NOT_FOUND)
    return job


@router.get("/{job_id}")
def get_generation(
    job_id: str, user: User = Depends(get_current_user), db: Session = Depends(get_db)
):
    job = _get_readable_job(job_id, user, db)
    return job_to_dict(job, "")  # 相对 /files 路径


@router.post("/{job_id}/retry")
def retry_generation(
    job_id: str, user: User = Depends(get_current_user), db: Session = Depends(get_db)
):
    job = _get_writable_job(job_id, user, db)  # 重跑要花归属人的钱，管理员也不能代劳
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


class SupplementBody(BaseModel):
    n: int = Field(default=1, ge=1, le=4)


@router.post("/{job_id}/supplement")
def supplement_generation(
    job_id: str,
    body: SupplementBody,
    user: User = Depends(get_current_user),
    db: Session = Depends(get_db),
):
    """在已完成的任务上「再补几张」，而不是整单重跑。

    为什么需要它：以前 4 张里坏了 1 张，只能点「重新生成」把 4 张全部重跑一遍——
    等于为 1 张图付 4 张的钱。这里新建一个只出所缺张数的任务，并**沿用上一次
    实际发出的提示词**（prompt_sent），所以既不用再花一次 AI 写提示词的开销，
    出来的图也和上一批是同一套设定，能直接混在一起用。
    """
    source = _get_writable_job(job_id, user, db)  # 补图同样是花归属人的钱
    if source.status != "succeeded":
        raise HTTPException(status_code=409, detail="只有已完成的任务可以补图")

    params = dict(source.params or {})
    params["n"] = body.n
    # 让 worker 跳过 AI 重写：prompt_final 这里放的已经是上次实际发出的最终提示词
    params["reuse_prompt"] = True
    params["supplement_of"] = source.id

    job = GenerationJob(
        # 一律记成网页端任务。不能继承 source="api"——那样它会在记录页被标成
        # 「来自 ERP」，可 ERP 用自己的 Key 查它必定 404（api_key_id 没继承、
        # 也不该继承），还不占该 Key 的配额，三方对不上账
        source="web",
        user_id=user.id,
        template_id=source.template_id,
        template_name=source.template_name,
        prompt_final=source.prompt_sent or source.prompt_final,
        params=params,
        input_paths=list(source.input_paths or []),
        status="pending",
        # 补图是网页端动作，不继承原任务的 ERP 回调与单号，
        # 否则对接方会收到一条它没有提交过的任务的回调
        callback_url=None,
        external_ref=None,
    )
    db.add(job)
    db.commit()
    db.refresh(job)
    return job_to_dict(job, "")  # 相对 /files 路径


@router.delete("/{job_id}")
def delete_generation(
    job_id: str, user: User = Depends(get_current_user), db: Session = Depends(get_db)
):
    job = _get_readable_job(job_id, user, db)  # 删除不花钱，保留给管理员清理
    if job.status == "processing":
        raise HTTPException(status_code=409, detail="任务正在执行，无法删除")
    for img in list(job.images or []):
        storage.delete_file(img.path)
        storage.delete_file(img.thumb_path)
    db.delete(job)
    db.commit()
    return {"ok": True}
