import threading
import time
from collections import defaultdict, deque
from typing import Deque, Dict, Tuple

from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session

from ..deps import get_current_user, get_db
from ..models import User
from ..security import create_token, hash_password, verify_password

router = APIRouter(prefix="/api/web/auth", tags=["网页-登录"])

# 简单的内存登录限速：同一 IP+用户名 15 分钟内最多 10 次失败，超过锁 15 分钟。
# 内部工具够用；对外规模化时应换成 Redis/反代层限速。
_LOGIN_WINDOW = 15 * 60
_LOGIN_MAX_FAILS = 10
_login_fails: Dict[str, Deque[float]] = defaultdict(deque)
_login_lock = threading.Lock()


def _rate_key(request: Request, username: str) -> str:
    ip = request.client.host if request.client else "?"
    return "%s|%s" % (ip, username.lower())


def _check_login_allowed(key: str) -> None:
    now = time.time()
    with _login_lock:
        fails = _login_fails[key]
        while fails and now - fails[0] > _LOGIN_WINDOW:
            fails.popleft()
        if len(fails) >= _LOGIN_MAX_FAILS:
            raise HTTPException(status_code=429, detail="登录尝试过于频繁，请 15 分钟后再试")


def _record_login_fail(key: str) -> None:
    with _login_lock:
        _login_fails[key].append(time.time())


def _clear_login_fails(key: str) -> None:
    with _login_lock:
        _login_fails.pop(key, None)


class LoginBody(BaseModel):
    username: str
    password: str


class ChangePasswordBody(BaseModel):
    old_password: str
    new_password: str = Field(min_length=8, description="至少 8 位")


def _user_to_dict(user: User) -> dict:
    return {
        "id": user.id,
        "username": user.username,
        "display_name": user.display_name or user.username,
        "role": user.role,
        "must_change_password": bool(user.must_change_password),
    }


@router.post("/login")
def login(body: LoginBody, request: Request, db: Session = Depends(get_db)):
    username = body.username.strip()
    key = _rate_key(request, username)
    _check_login_allowed(key)
    user = db.query(User).filter(User.username == username).first()
    if user is None or not verify_password(body.password, user.password_hash):
        _record_login_fail(key)
        raise HTTPException(status_code=401, detail="用户名或密码错误")
    if not user.is_active:
        raise HTTPException(status_code=403, detail="账号已停用")
    _clear_login_fails(key)
    return {"token": create_token(user.id, user.token_version or 0), "user": _user_to_dict(user)}


@router.get("/me")
def me(user: User = Depends(get_current_user)):
    return _user_to_dict(user)


@router.post("/change_password")
def change_password(
    body: ChangePasswordBody,
    user: User = Depends(get_current_user),
    db: Session = Depends(get_db),
):
    if not verify_password(body.old_password, user.password_hash):
        raise HTTPException(status_code=400, detail="原密码不正确")
    if body.new_password == body.old_password:
        raise HTTPException(status_code=400, detail="新密码不能与原密码相同")
    user.password_hash = hash_password(body.new_password)
    user.must_change_password = False
    user.token_version = (user.token_version or 0) + 1  # 让其他设备上的旧令牌失效
    db.commit()
    # 当前设备换发新令牌，避免自己被登出
    return {"ok": True, "message": "密码已修改", "token": create_token(user.id, user.token_version)}
