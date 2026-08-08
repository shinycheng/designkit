from pathlib import Path

from fastapi import APIRouter, Depends, HTTPException, UploadFile, File
from sqlalchemy.orm import Session

from ..config import ALLOWED_IMAGE_EXTS, MAX_UPLOAD_MB
from ..deps import get_current_user, get_db
from ..models import Upload, User
from ..serializers import upload_to_dict
from ..services import settings_service, storage

router = APIRouter(prefix="/api/web/uploads", tags=["网页-上传"])


async def read_and_validate(file: UploadFile) -> "tuple":
    suffix = Path(file.filename or "image.png").suffix.lower()
    if suffix not in ALLOWED_IMAGE_EXTS:
        raise HTTPException(
            status_code=422,
            detail="只支持 %s 格式的图片" % " / ".join(sorted(ALLOWED_IMAGE_EXTS)),
        )
    data = await file.read()
    if len(data) > MAX_UPLOAD_MB * 1024 * 1024:
        raise HTTPException(status_code=422, detail="图片不能超过 %dMB" % MAX_UPLOAD_MB)
    if not data:
        raise HTTPException(status_code=422, detail="上传内容为空")
    return data, suffix


@router.post("")
async def upload_image(
    file: UploadFile = File(...),
    user: User = Depends(get_current_user),
    db: Session = Depends(get_db),
):
    data, suffix = await read_and_validate(file)
    try:
        rel, width, height = storage.save_upload(data, suffix)
    except storage.StorageError as e:
        raise HTTPException(status_code=422, detail=str(e))
    record = Upload(
        user_id=user.id,
        original_name=file.filename or "",
        path=rel,
        width=width,
        height=height,
        size_bytes=len(data),
    )
    db.add(record)
    db.commit()
    db.refresh(record)
    public_base = ""  # 网页端用相对 /files 路径，换访问入口也不会失效
    return upload_to_dict(record, public_base)
