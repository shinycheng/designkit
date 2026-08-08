from typing import Optional

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session

from ..deps import get_db, require_admin
from ..models import ApiKey, User
from ..security import hash_api_key, new_api_key, new_webhook_secret
from ..serializers import apikey_to_dict

router = APIRouter(prefix="/api/web/apikeys", tags=["网页-对外APIKey"])


class ApiKeyCreateBody(BaseModel):
    name: str = Field(min_length=1, max_length=64, description="例如：XX ERP 生产环境")
    monthly_quota: Optional[int] = Field(default=None, ge=1)


@router.get("")
def list_keys(_: User = Depends(require_admin), db: Session = Depends(get_db)):
    rows = db.query(ApiKey).order_by(ApiKey.id.desc()).all()
    return [apikey_to_dict(k) for k in rows]


@router.post("")
def create_key(
    body: ApiKeyCreateBody, _: User = Depends(require_admin), db: Session = Depends(get_db)
):
    raw_key = new_api_key()
    secret = new_webhook_secret()
    record = ApiKey(
        name=body.name.strip(),
        key_prefix=raw_key[:11],
        key_hash=hash_api_key(raw_key),
        webhook_secret=secret,
        monthly_quota=body.monthly_quota,
    )
    db.add(record)
    db.commit()
    db.refresh(record)
    data = apikey_to_dict(record)
    # 完整 Key 和回调密钥只在创建时返回一次，之后无法再查看
    data["api_key"] = raw_key
    data["webhook_secret"] = secret
    return data


@router.post("/{key_id}/toggle")
def toggle_key(key_id: int, _: User = Depends(require_admin), db: Session = Depends(get_db)):
    k = db.get(ApiKey, key_id)
    if k is None:
        raise HTTPException(status_code=404, detail="API Key 不存在")
    k.is_active = not k.is_active
    db.commit()
    return apikey_to_dict(k)


@router.delete("/{key_id}")
def delete_key(key_id: int, _: User = Depends(require_admin), db: Session = Depends(get_db)):
    k = db.get(ApiKey, key_id)
    if k is None:
        raise HTTPException(status_code=404, detail="API Key 不存在")
    db.delete(k)
    db.commit()
    return {"ok": True}
