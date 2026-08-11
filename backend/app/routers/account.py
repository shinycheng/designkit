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
from datetime import datetime
from typing import Any, Dict, Optional, Tuple

from fastapi import APIRouter, Depends, Response
from sqlalchemy.orm import Session

from ..deps import get_current_user, get_db
from ..models import User, UserGatewayAccount, utcnow
from ..security import create_token
from ..services import prompt_studio, provider, scheduler, settings_service, user_gateway
# 取图 Cookie 的签发只有 auth.py 里那一个实现，这里**直接复用、不要抄一份**：
# 那个函数里攒着有效期、Secure 标记、以及「签发失败不能连累主流程」的兜底，
# 抄一份出来只会在将来某次只改了一边时，变成「改完密码能看图、退出所有设备后满屏裂图」
# 这种谁也想不到要去对比两个文件的怪毛病。
# 反过来的代价是：改 auth._issue_file_cookie 的参数时，本文件下面 logout_all 里
# 那一处调用要跟着改（`grep -rn "_issue_file_cookie" backend/` 能一次找全）。
from .auth import _issue_file_cookie

router = APIRouter(prefix="/api/web/account", tags=["网页-我的账户"])

# 余额快照多久没更新就算「落后了」。
#
# 间隔常量直接从 scheduler 引，**不要在这里另抄一个 10 分钟**：抄一份之后，
# 哪天有人把后台同步间隔调稀到 30 分钟，这一页就会给全体用户显示「同步失败」，
# 而「两个文件里的数字对不上」这件事，光看页面是永远查不出来的。
#
# 取 3 倍是刻意留的余量：后台一轮最多扫 BALANCE_BATCH（25）个人，人一多，
# 个别人天然会晚一两轮才轮到。阈值卡死在一个间隔上，会让一切正常的系统
# 天天在页面上喊「同步失败」——喊多了，真出事那次也没人当回事了。
BALANCE_STALE_AFTER = scheduler.BALANCE_SYNC_INTERVAL * 3


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


def _iso_utc(dt: Optional[datetime]) -> Optional[str]:
    """裸 UTC 时间 → 带 Z 的 ISO 串。

    **这个 Z 一个字母都不能省**：库里存的是不带时区的 UTC（models.utcnow），
    少了它，浏览器会按**本地时区**解释这串时间，中国时区直接差 8 小时——
    「1 分钟前刚同步的余额」在页面上会变成「8 小时前同步」，
    用户以为余额早就停更了。routers/users.py 的 _iso 也是为同一个坑写的。
    """
    return dt.isoformat() + "Z" if dt else None


def _balance_view(account: Optional[UserGatewayAccount]) -> Dict[str, Any]:
    """本人那份专属额度的余额快照。**没有数据时绝不返回 0。**

    这是这一块最要紧的一条规矩：「余额是 0」和「读不到余额」在界面上必须
    长得完全不一样。把「读不到」显示成 0，运营看到的就是「我的钱没了」，
    第一反应是停下手上的活去追着管理员问；而真相可能只是服务器有一分钟
    连不上网关。所以没数据时 usd 一律是 null，由前端改说「余额数据 X 分钟前」。

    state 的取值（前端只看这一个字段决定显示哪一套话）：
      none    —— 本人没有一份在用的专属额度（还没开通，或 Key 已失效/被停用）。
                 这时**连旧数字都不返回**：非 active 的账号后台根本不会再去同步
                 （见 scheduler._balance_due_user_ids 的过滤条件），
                 留一个永远不动的旧数字在页面上，比什么都不显示更误导人。
      pending —— 已开通，但还没拿到过余额数字（刚开通、后台还没扫到；
                 或者网关的响应里压根没有余额字段）。
      ok      —— 有快照，同步时间还新鲜。
      stale   —— 有快照，但最近一次同步明显落后了（多半是连不上网关）。

    为什么 age_seconds 由后端算好、而不是让前端拿时间戳去减：用户那台 Mac 或
    群晖的时钟不一定准，浏览器用自己的「现在」去减服务器给的时间戳，
    时钟差几分钟就能算出「余额数据 -3 分钟前」这种鬼话。后端这两个时间
    出自同一台机器的同一个时钟，怎么都不会算反。
    """
    view: Dict[str, Any] = {
        "state": "none",
        # 单位固定美元，**这里不做任何汇率换算**。人民币定价是以后的事，
        # 现在编一个汇率进来，等真按人民币定价那天，页面上的数和账单上的数
        # 就是两个值，谁也说不清哪个才对。
        "currency": "USD",
        "usd": None,
        "synced_at": None,
        "age_seconds": None,
        # 下面两个给前端写文案用（「后台每 X 分钟同步一次」），
        # 免得前端把间隔也硬编一遍，将来改一处、漏一处。
        "sync_interval_seconds": int(scheduler.BALANCE_SYNC_INTERVAL.total_seconds()),
        "stale_after_seconds": int(BALANCE_STALE_AFTER.total_seconds()),
    }
    # 「有一份在用的专属额度」= active 且有 Key。口径必须和另外两处一致：
    # settings_service.resolve_for_user 只认 active 的账号才用他自己的 Key，
    # scheduler 也只给 active + 有 Key 的账号同步余额。三处口径一旦不一致，
    # 就会出现「页面显示着余额、点生成却说没开通」这种自相矛盾。
    if account is None or not account.api_key_enc or account.state != "active":
        return view

    synced_at = account.balance_synced_at
    age: Optional[int] = None
    if synced_at is not None:
        # 夹到 0：服务器时钟被 NTP 往回拨过时这里会算出负数，
        # 页面上显示「-2 分钟前」比不显示还让人心慌。
        age = max(0, int((utcnow() - synced_at).total_seconds()))
    view["synced_at"] = _iso_utc(synced_at)
    view["age_seconds"] = age

    if account.balance_usd is None:
        # 开通好了但一个数字都还没拿到。对用户来说该做的事只有一件：再等一会儿。
        view["state"] = "pending"
        return view

    view["usd"] = float(account.balance_usd)
    # age 为 None 却有余额，理论上不会发生（provisioning 写余额时一定同时写时间），
    # 真出现了就按「不可信」处理——宁可让页面说「这个数可能旧了」，
    # 也不能让一个来路不明的数字冒充实时余额。
    view["state"] = "ok" if (age is not None and age <= BALANCE_STALE_AFTER.total_seconds()) else "stale"
    return view


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
            # 余额快照（后台定时任务同步的，见 services/provisioning.sync_balance）。
            # 这里给的是**快照 + 快照有多旧**，前端必须把「多旧」也显示出来：
            # 只显示数字，用户会拿它当实时余额，然后为「刚花的钱怎么没扣」来问一圈。
            "balance": _balance_view(account),
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
