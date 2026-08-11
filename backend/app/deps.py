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


# ───────────────────────── 请求来自哪个 IP ─────────────────────────
# IPv6 最长 45 个字符（含 IPv4 映射写法 ::ffff:255.255.255.255）。
# 截断是为了别让一个伪造的超长 X-Forwarded-For 变成超长的限速 key。
_MAX_IP_LENGTH = 45


def _normalize_ip(raw: str) -> str:
    """把一段 X-Forwarded-For 收拾成干净的地址。

    实际会收到的几种写法（各家反代都不一样，没有统一标准）：
        1.2.3.4           普通 IPv4
        1.2.3.4:56789     有的反代会把源端口也带上
        [2001:db8::1]     IPv6，用方括号包着
        [2001:db8::1]:443 IPv6 带端口
    端口必须去掉：同一个人每次请求的源端口都不一样，留着的话每次都算成新的 key，
    限速直接失效，而且会让限速表疯狂涨行。
    """
    text = (raw or "").strip()
    if text.startswith("["):                      # [IPv6] 或 [IPv6]:port
        end = text.find("]")
        if end > 0:
            text = text[1:end]
    elif text.count(":") == 1:                    # IPv4:port（IPv6 冒号多于一个，不能碰）
        text = text.split(":", 1)[0]
    return text[:_MAX_IP_LENGTH]


def client_ip(request: Request, trusted_proxy_hops: int = 0) -> str:
    """这次请求真正来自哪个 IP —— 用于限速等「按来源计数」的场合。

    ════════════════════════════════════════════════════════════════
     为什么不能取 X-Forwarded-For 的**第一个**
    ════════════════════════════════════════════════════════════════
    这个头是一路上每个代理往**右边追加**自己看到的对端地址拼出来的：

        X-Forwarded-For: <客户端自己写的任意内容>, <反代看到的真实来源>

    最左边那一段完全由客户端控制——请求里直接写 `X-Forwarded-For: 1.1.1.1` 就行。
    照最左边算，攻击者每次换一个假 IP，限速就等于不存在（而且限速表还会被撑爆）。
    可信的只有「我们信任的那几层反代自己追加的那几段」，也就是**右边数过来**的。

    所以规则是：`trusted_proxy_hops` 配几层，就从右往左数第几段。
      - 0（默认）：没有反代，直接用连接的对端地址，整个头一个字都不看。
      - 1：群晖反向代理 / 一层 Nginx / Caddy 这种最常见的形态，取最后一段。
      - 2：套了 CDN 再进 Nginx 之类，取倒数第二段。
    数错了的后果不对称，所以默认值取 0、宁可少不可多：
      - 配少了（真有两层却填 1）：拿到的是中间那层反代的地址，所有人共用一个限速桶，
        严一点，但攻击者伪造不出别人的桶来；
      - 配多了（只有一层却填 2）：会去读攻击者自己写的那一段，限速当场作废。

    ════════════════════════════════════════════════════════════════
     和 uvicorn 自带的代理头处理是什么关系
    ════════════════════════════════════════════════════════════════
    uvicorn 也会处理这个头，但只有当**连接的对端**在它的 `--forwarded-allow-ips`
    （默认只有 127.0.0.1）里时才会动手，而且它取的是「从右往左第一个不受信任的地址」，
    与这里的算法方向一致，结论也一致（一层反代时两边都得到同一个地址）。
    所以两者叠加不会打架。**但绝对不要给 uvicorn 加 `--forwarded-allow-ips='*'`**：
    那会让它无条件相信整串头并取最左边那一段，正好就是客户端能随便伪造的那一段，
    我们在这里做的所有事都会被它先一步毁掉。

    request.client 在极少数场景下会是 None（Unix socket、某些测试客户端），
    那时返回 "-"：一个固定的桶总比抛异常把登录接口打挂强。
    """
    peer = _normalize_ip(request.client.host) if request.client else ""
    if trusted_proxy_hops <= 0:
        return peer or "-"
    parts = [p.strip() for p in (request.headers.get("x-forwarded-for") or "").split(",")]
    parts = [p for p in parts if p]
    index = len(parts) - trusted_proxy_hops
    if index < 0:
        # 头比配置的层数还短：要么是请求绕过反代直连进来的，要么是这一项配大了。
        # 两种情况都退回连接对端——那是唯一伪造不了的东西。
        return peer or "-"
    return _normalize_ip(parts[index]) or peer or "-"


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
