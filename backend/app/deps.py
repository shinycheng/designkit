from datetime import datetime
from typing import Optional

from fastapi import Depends, Header, HTTPException, Request
from sqlalchemy.orm import Session

from .database import SessionLocal
from .models import ApiKey, User
from .security import decode_token, hash_api_key

# 「还没改初始密码」时被拦下来返回的固定文案。
# 固定是有意的：前端靠**整串精确比对**来识别这种 403，从而弹出改密引导
# 而不是弹一个普通的报错提示。改这行字就等于改一个前后端契约，
# 必须同步改 frontend/js/app.js 里的比对处。
MUST_CHANGE_PASSWORD_DETAIL = "请先修改初始密码，再使用其他功能"

# 处在「必须改密」状态时仍然放行的接口。只有这三个：
# - /auth/me            前端启动时要靠它拿到 must_change_password 标记，
#                       不放行的话页面连「去改密码」的界面都渲染不出来
# - /auth/change_password  改密码本身
# - /auth/set_password     预留：管理员重置后由本人设置新密码（当前还没实现，
#                       先写在这里，免得将来加接口时忘了开口子、上线即死锁）
_PASSWORD_GATE_ALLOWED = frozenset({
    "/api/web/auth/me",
    "/api/web/auth/change_password",
    "/api/web/auth/set_password",
})


def get_db():
    db = SessionLocal()
    try:
        yield db
    finally:
        db.close()


def _request_path(request: Request) -> str:
    """取出用于比对的接口路径，去掉反代挂载前缀和末尾斜杠。

    ASGI 规范里 scope["path"] 是**含 root_path 的完整路径**，
    而我们的白名单写的是应用内部路径。群晖上如果把本应用挂到子路径
    （例如 https://nas/designkit/...），不减掉 root_path 就会导致
    /auth/me 也被拦，用户永远改不了密码、直接登不进系统。
    """
    root = request.scope.get("root_path") or ""
    path = request.url.path
    if root and path.startswith(root):
        path = path[len(root):] or "/"
    return path.rstrip("/") or "/"


def get_current_user(
    request: Request,
    authorization: Optional[str] = Header(None),
    db: Session = Depends(get_db),
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
    # 首次登录必须改密，**这道闸门必须在服务端**。
    # 以前它只写在前端（app.js 的 renderPasswordGate），可初始口令
    # admin/admin123456 是公开的（登录页正文、部署文档里都写着），
    # 拿登录接口返回的那个 token 绕过页面直接调接口，所有功能照用不误。
    # 以后后台批量建号发初始密码，这个口子会被放得更大。
    if user.must_change_password and _request_path(request) not in _PASSWORD_GATE_ALLOWED:
        raise HTTPException(status_code=403, detail=MUST_CHANGE_PASSWORD_DETAIL)
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
