from datetime import datetime
from typing import Optional

from fastapi import Depends, Header, HTTPException
from sqlalchemy.orm import Session

from .database import SessionLocal
from .models import ApiKey, User
from .security import decode_token, hash_api_key


def get_db():
    db = SessionLocal()
    try:
        yield db
    finally:
        db.close()


def get_current_user(
    authorization: Optional[str] = Header(None), db: Session = Depends(get_db)
) -> User:
    if not authorization or not authorization.startswith("Bearer "):
        raise HTTPException(status_code=401, detail="未登录")
    claims = decode_token(authorization[len("Bearer "):].strip())
    if claims is None:
        raise HTTPException(status_code=401, detail="登录已过期，请重新登录")
    user = db.get(User, claims["user_id"])
    if user is None or not user.is_active:
        raise HTTPException(status_code=401, detail="账号不存在或已停用")
    # 令牌版本不符（改过密码/被停用又启用）→ 旧令牌失效
    if claims.get("ver", 0) != (user.token_version or 0):
        raise HTTPException(status_code=401, detail="登录状态已失效，请重新登录")
    return user


def require_admin(user: User = Depends(get_current_user)) -> User:
    if user.role != "admin":
        raise HTTPException(status_code=403, detail="需要管理员权限")
    return user


def get_api_client(
    x_api_key: Optional[str] = Header(None), db: Session = Depends(get_db)
) -> ApiKey:
    """对外 API 的鉴权：请求头 X-API-Key。"""
    if not x_api_key:
        raise HTTPException(status_code=401, detail="缺少 X-API-Key 请求头")
    key = db.query(ApiKey).filter(ApiKey.key_hash == hash_api_key(x_api_key.strip())).first()
    if key is None or not key.is_active:
        raise HTTPException(status_code=401, detail="API Key 无效或已停用")
    key.last_used_at = datetime.utcnow()
    db.commit()
    return key
