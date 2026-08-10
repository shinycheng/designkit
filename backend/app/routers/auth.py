"""登录、改密码、退出登录。

除了签发登录令牌（JWT，前端存 localStorage、放在 Authorization 头里），
这里还负责**种下取图凭证 dk_files Cookie**——页面上的图是 <img src> 加载的，
带不上 Authorization 头，所以取图那条线只能靠 Cookie。两者是一套身份的两种载体，
必须同进同退，任何一处漏了都会表现成「登录正常、图全裂了」。
详细缘由见 services/file_access.py 的文件头。
"""
import logging
import threading
import time
from collections import defaultdict, deque
from typing import Deque, Dict, Optional

from fastapi import APIRouter, Depends, HTTPException, Request, Response
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session

from ..deps import get_current_user, get_db
from ..models import User
from ..security import create_token, hash_password, verify_password
from ..services import file_access, settings_service

logger = logging.getLogger("designkit.auth")

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


class SetPasswordBody(BaseModel):
    new_password: str = Field(min_length=8, description="至少 8 位")


# ───────────────────────── 取图凭证（dk_files Cookie） ─────────────────────────


def _cookie_days(db: Session) -> Optional[int]:
    """取图凭证有效期（天）。读不出数字就返回 None，交给 file_access 用它自己的默认值。"""
    try:
        days = int(settings_service.get(db, "web_file_cookie_days") or 0)
    except (TypeError, ValueError):
        return None
    return days if days > 0 else None


def _cookie_secure(db: Session, request: Optional[Request] = None) -> bool:
    """这个 Cookie 该不该带 Secure 标记——**按本次请求的真实协议判断**。

    两个方向都会出事，所以只能照实判断，不能写死：
      - 整站跑在 http 的内网部署上（群晖里绝大多数就是 http://192.168.x.x:8787）
        带了 Secure，浏览器会**直接丢弃**这个 Cookie：控制台不报任何错、
        接口全是 200，现象只有一个——「登录成功，但一张图都打不开」。
      - 真上了 https 却不带 Secure，Cookie 就有可能被降级到 http 的请求带出去。

    **为什么不能按「对外访问地址」(public_base_url) 这个设置项判断**（这里踩过一次）：
    群晖上最常见的部署形态是「对外套一层 Nginx/Caddy 配 https，同事在局域网直接开
    http://192.168.x.x:8787」。管理员照 README「部署后必做」把对外地址填成 https 之后，
    那一项跟内网同事这一次请求毫无关系——结果内网所有人立刻满屏裂图，
    而且接口全是 200、控制台一个错都不报，运营同学完全无从归因。
    反过来对外地址留空（或填 http）、同事其实走 https 时，发出去的又是没有 Secure 的
    Cookie。所以它只配当兜底：只有**连本次请求都拿不到**时才退回按它判断。
    """
    if request is not None:
        # 反代（Nginx / Caddy / 群晖自带的反向代理）把 https 卸在自己身上、
        # 再用明文 http 转发给我们时，用户浏览器那一段的真实协议只写在这个头里。
        # 多级反代时它是逗号分隔的，第一个才是离用户最近的那一跳。
        forwarded = (request.headers.get("x-forwarded-proto") or "").split(",")[0].strip().lower()
        # 没有反代、TLS 直接卸在 uvicorn 上（--ssl-certfile）时，看连接本身的协议。
        scheme = (request.url.scheme or "").strip().lower()
        if forwarded in ("http", "https") or scheme in ("http", "https"):
            # 只要有一路说是 https 就带 Secure。这个「宁可多带」的取向是有意的：
            # 少带一次是安全问题（Cookie 可能被降级到明文请求上带出去），
            # 多带一次只是**伪造这个头的人自己**看不到图——他既影响不到别人，
            # 也没法用这个头把一个真 https 会话降级成不带 Secure（真 TLS 同样判 True）。
            return forwarded == "https" or scheme == "https"
    base = str(settings_service.get(db, "public_base_url") or "").strip().lower()
    return base.startswith("https://")


def _issue_file_cookie(
    response: Response, db: Session, user: User, request: Optional[Request] = None
) -> None:
    """把取图凭证种到响应上。

    **request 必须传**（形参给了默认值只是为了不让漏传把登录整个打挂）：
    Secure 标记要按本次请求的真实协议决定，理由见 _cookie_secure。

    调用点有三处，**一处都不能少**：
      1. 登录成功——新会话的起点；
      2. GET /me——升级平滑的关键。老用户的登录令牌一直躺在 localStorage 里，
         他们不会重新登录，手上自然没有这个 Cookie；只在 login 里种的话，
         升级当天所有**已经登录着**的人一打开页面就是满屏裂图，
         而且要等到他们主动退出重登才好。前端每次启动都会调一次 /me，
         在这里补种等于「刷新一下就自动修好」，用户全程无感。
      3. 改密码 / 设密码之后——那两个动作会把 token_version +1，
         旧 Cookie 里记的版本号立刻对不上（这是设计如此，为的是让别的设备
         马上失效），所以必须拿新版本号重新种一次，否则用户「改完密码就看不到图」。

    这里刻意兜住所有异常：种 Cookie 失败最多是图片打不开，
    不该连累登录本身失败——那会把「图看不了」升级成「谁也进不来」。
    """
    try:
        file_access.set_web_cookie(
            response,
            user.id,
            user.token_version or 0,
            days=_cookie_days(db),
            secure=_cookie_secure(db, request),
        )
    except Exception:  # noqa: BLE001
        logger.exception("签发取图凭证失败，该用户本次会话可能看不到图片")


def _user_to_dict(user: User) -> dict:
    return {
        "id": user.id,
        "username": user.username,
        "display_name": user.display_name or user.username,
        "role": user.role,
        "must_change_password": bool(user.must_change_password),
    }


@router.post("/login")
def login(
    body: LoginBody,
    request: Request,
    response: Response,
    db: Session = Depends(get_db),
):
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
    _issue_file_cookie(response, db, user, request)
    return {"token": create_token(user.id, user.token_version or 0), "user": _user_to_dict(user)}


@router.get("/me")
def me(
    request: Request,
    response: Response,
    user: User = Depends(get_current_user),
    db: Session = Depends(get_db),
):
    """当前登录用户。**顺带补种一次取图凭证**——理由见 _issue_file_cookie 的第 2 条。

    这里补种时同样要按本次请求的协议决定 Secure：同一个人在公司走 https、
    在家走 http 是常事，每次 /me 都会照当前这一次的协议重新种一张对的。
    """
    _issue_file_cookie(response, db, user, request)
    return _user_to_dict(user)


@router.post("/change_password")
def change_password(
    body: ChangePasswordBody,
    request: Request,
    response: Response,
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
    # token_version 变了 → 手上那张取图 Cookie 里记的版本号立刻作废，
    # 这里必须用新版本号重种一次，否则用户会「改完密码图就全裂了」。
    _issue_file_cookie(response, db, user, request)
    # 当前设备换发新令牌，避免自己被登出
    return {"ok": True, "message": "密码已修改", "token": create_token(user.id, user.token_version)}


@router.post("/set_password")
def set_password(
    body: SetPasswordBody,
    request: Request,
    response: Response,
    user: User = Depends(get_current_user),
    db: Session = Depends(get_db),
):
    """给「还没有登录密码」的账号设一个密码，不校验原密码。

    今天用不上（现有账号都是管理员在后台建的，建的时候就带密码），
    这是给以后微信 / 手机号注册的用户留的口子：那种账号一开始只有第三方身份、
    没有密码，想加一个密码登录方式时，「请输入原密码」这一步无从谈起。

    **有密码的账号一律走 /change_password。** 这条判断是这个接口唯一的安全线——
    少了它，任何人只要拿到一个有效令牌就能把密码改掉，等于把「改密码要知道原密码」
    这道防线整个作废（令牌可能是从别处泄露的，原密码校验正是为这种情况准备的）。
    """
    if user.password_hash:
        raise HTTPException(
            status_code=400, detail="你的账号已经有登录密码了，请到「修改密码」并填写原密码"
        )
    user.password_hash = hash_password(body.new_password)
    user.must_change_password = False
    user.token_version = (user.token_version or 0) + 1
    db.commit()
    _issue_file_cookie(response, db, user, request)
    return {"ok": True, "message": "密码已设置", "token": create_token(user.id, user.token_version)}


@router.post("/logout")
def logout(response: Response):
    """退出登录：**只删取图 Cookie，不动 token_version。**

    为什么不顺手把 token_version +1（那样才能真正吊销令牌）：
    token_version 是**「人」级别**的，不是「会话」级别的（见 deps.py 的校验）。
    加一次，这个人在所有设备上的登录会同时失效——他在手机上点一下退出，
    电脑上那个正在填参数、图刚生成到一半的会话会当场 401 被踢，
    参数全丢，而且他完全想不到是自己刚才在手机上点的那一下造成的。
    多用户之后这是稳定复现、又百分之百归因不到的投诉源。

    真正的「退出所有设备」应该是账户页上一个单独的、用户主动点的按钮
    （届时在 account 路由里做 token_version +1），语义清楚、后果可预期。

    这个接口**故意不要求登录**：令牌已经过期、或者浏览器里只剩一个 Cookie 的时候，
    用户照样该能把凭证清干净。而它能做的事只有「删掉调用者自己浏览器上的那个
    Cookie」，没有任何可被滥用的余地。
    """
    file_access.clear_web_cookie(response)
    return {"ok": True, "message": "已退出登录"}
