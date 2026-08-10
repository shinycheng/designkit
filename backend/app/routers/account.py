"""「我的账户」——每个人看自己的那一页。

这个页面上**不会出现任何 API Key 输入框**，这是有意的设计，不是漏做。

道理是这样的：成员的生图额度是管理员在后台替他开通的，那把网关 Key 从头到尾
不是他自己申请的，他甚至不需要知道底下跑的是哪家网关。让他在页面上看到一个
「API Key」输入框，只会有两个结果——要么他去问管理员要一串密钥然后自己粘进来
（凭空多出一条密钥在聊天记录里流转的路径），要么他填错东西把自己的账号搞坏。

顺带也正好落在项目「密钥不代填」那条约定的正面：这里不是「我们替他填」，
而是**根本没有需要他填的东西**。

所以这一页只回答三个问题：我是谁、我现在能不能生图、不能的话该找谁。
另外还管一件本人自己的安全事：把其他设备上的登录一次性踢掉（/logout_all）。
"""
from typing import Any, Dict, Tuple

from fastapi import APIRouter, Depends, Response
from sqlalchemy.orm import Session

from ..deps import get_current_user, get_db
from ..models import User
from ..security import create_token
from ..services import prompt_studio, provider, settings_service, user_gateway
# 取图 Cookie 的签发只有 auth.py 里那一个实现，这里**直接复用、不要抄一份**：
# 那个函数里攒着有效期、Secure 标记、以及「签发失败不能连累主流程」的兜底，
# 抄一份出来只会在将来某次只改了一边时，变成「改完密码能看图、退出所有设备后满屏裂图」
# 这种谁也想不到要去对比两个文件的怪毛病。
# 反过来的代价是：改 auth._issue_file_cookie 的参数时，本文件下面 logout_all 里
# 那一处调用要跟着改（`grep -rn "_issue_file_cookie" backend/` 能一次找全）。
from .auth import _issue_file_cookie

router = APIRouter(prefix="/api/web/account", tags=["网页-我的账户"])


def _readiness(db: Session, user_id: int) -> Tuple[bool, str]:
    """现在这个人到底能不能生图，以及不能的话原因是什么（人话）。

    判断一律走 settings_service.resolve_for_user —— 它是全系统唯一决定
    「这次生图用谁的 Key」的地方。**绝对不要在这里另写一套判断**
    （比如「有没有 active 的网关账号」）：那样写出来的页面会和 worker 的实际行为
    渐行渐远，最典型的就是 mock 模式和 shared 计费模式下明明能生图，
    页面却红着脸说「你还没开通」，用户跑去找管理员，管理员查了半天什么问题也没有。

    这个函数**只返回布尔值和一句话，绝不把 resolve_for_user 的返回值放出去**——
    那个字典里带着明文密钥（见 settings_service 文件头）。
    """
    try:
        settings_service.resolve_for_user(db, user_id)
    except settings_service.GatewayNotReady as exc:
        return False, str(exc)
    return True, ""


@router.get("")
def my_account(user: User = Depends(get_current_user), db: Session = Depends(get_db)) -> Dict[str, Any]:
    """我的账户信息 + 生图额度状态。

    返回值里没有、将来也不要加任何和 Key 本身有关的字段——既不返回明文，
    也不返回打码值，更不返回末 4 位（原因见 models.py 里 UserGatewayAccount
    的注释）。用户想知道的只是「能不能用」。
    """
    account = user_gateway.get_account(db, user.id)
    can_generate, message = _readiness(db, user.id)
    return {
        "id": user.id,
        "username": user.username,
        "display_name": user.display_name or user.username,
        "role": user.role,
        "must_change_password": bool(user.must_change_password),
        "gateway": {
            # 「本人有没有一份专属额度」。注意它和 can_generate 不是一回事：
            # 共用一把 Key（gateway_mode=shared）或模拟生图模式下，
            # status 是 not_configured，但照样能正常生图。
            # 界面上该显示成「能不能用」的地方一律看 can_generate。
            "status": "configured" if (account is not None and account.api_key_enc) else "not_configured",
            # 现在点「生成」会不会被拦住。前端就看这一个字段。
            "can_generate": can_generate,
            # 不能生图时的原因，已经是给非技术用户看的中文，直接显示即可
            "message": message,
        },
    }


@router.post("/logout_all")
def logout_all(
    response: Response,
    user: User = Depends(get_current_user),
    db: Session = Depends(get_db),
) -> Dict[str, Any]:
    """退出其他所有设备：把这个人**除当前这台以外**的所有登录一次性作废。

    做法就一句话：token_version +1。deps.get_current_user 每次都拿令牌里的 ver
    和库里的 token_version 比对，加一之后，别处那些还揣在 localStorage 里的令牌
    立刻全部对不上号；取图 Cookie 里同样记着版本号（见 services/file_access.py），
    所以别人手上已经打开的图片链接也会在同一瞬间失效——这两条线必须同时断，
    只断令牌的话，对方照样能用浏览器里那个 Cookie 继续翻你的图。

    **这和 auth.py 的 /logout 是刻意分开的两件事**，别把这段逻辑挪过去：
    /logout 只删调用者自己浏览器上的 Cookie、不动 token_version，因为
    token_version 是「人」级别的——他在手机上点一下退出，电脑上那个正在填参数、
    图刚生成到一半的会话就会当场被踢掉，而且他完全想不到是自己刚才那一下造成的。
    「全部失效」这种后果只能由用户明确地、单独地点一次才发生，也就是这个接口。

    当前这台设备**故意保住**：换发一张新令牌 + 重种取图 Cookie。
    点这个按钮的人正处在「我怀疑账号被别人用了」的紧张时刻，把他自己也踢到登录页，
    他接下来最该做的那件事（顺手改一次密码）反而被打断了；而且万一他一时想不起密码，
    等于自己把自己锁在门外，只会更慌。
    """
    user.token_version = (user.token_version or 0) + 1
    db.commit()
    # 版本号变了，手上这张取图 Cookie 里记的旧版本号立刻作废，
    # 必须用新版本号重种一次，否则本人会「点完按钮自己满屏裂图」。
    _issue_file_cookie(response, db, user)
    return {
        "ok": True,
        "message": "其他设备上的登录已经全部退出",
        # 当前设备换发新令牌，避免把自己也登出（理由见上面的说明）
        "token": create_token(user.id, user.token_version),
    }


@router.post("/test_provider")
def test_provider(user: User = Depends(get_current_user), db: Session = Depends(get_db)):
    """本人自测：现在能不能连上生图接口（不产生生图费用）。

    和管理员那个 /api/web/settings/test_provider 的区别只有一处，但很要紧：
    这里用 resolve_for_user(db, user.id) 而不是全局 get_all——测的必须是
    「我点生成时真正会用到的那把 Key」。用全局设置去测，成员会看到「连接成功」，
    然后一生成就报「你的账号还没开通额度」，这种自相矛盾最消耗信任。
    管理员那条接口保留原样，它测的是网关本身健不健康，是另一个问题。
    """
    try:
        settings = settings_service.resolve_for_user(db, user.id)
    except settings_service.GatewayNotReady as exc:
        return {"ok": False, "message": str(exc)}
    try:
        return {"ok": True, "message": provider.test_connection(settings)}
    except provider.ProviderError as exc:
        return {"ok": False, "message": str(exc)}


@router.post("/test_text_model")
def test_text_model(user: User = Depends(get_current_user), db: Session = Depends(get_db)):
    """本人自测：写提示词用的文本模型能不能用（只发一句话，成本可忽略）。"""
    try:
        settings = settings_service.resolve_for_user(db, user.id)
    except settings_service.GatewayNotReady as exc:
        return {"ok": False, "message": str(exc)}
    if settings.get("provider") == "mock":
        # mock 模式下根本不会去调文本模型，这时候报「连不上」纯属吓唬人
        return {"ok": True, "message": "当前为模拟生图模式，无需文本模型"}
    try:
        return {"ok": True, "message": prompt_studio.test_text_model(settings)}
    except provider.ProviderError as exc:
        return {"ok": False, "message": str(exc)}
