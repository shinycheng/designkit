"""成员账号管理（只有管理员能用）。

在这个文件出现之前，全项目没有任何一条接口能写 users 表——成员账号只能靠
seed 里那个默认管理员，或者直接改数据库。所以「多用户」这件事在界面上是
不存在的。这里补上建号、停用、重置密码、配网关 Key 这几件运营真正要做的事。

════════════════════════════════════════════════════════════════
 两条硬规矩，改这个文件之前先读完
════════════════════════════════════════════════════════════════

**一、成员的网关 Key 一个字符都不返回给前端。**
连「末 4 位」也不行。理由写在 models.py 的 UserGatewayAccount 注释里：
designkit、网关后台、上游账单三个地方各有一份「后 4 位」，凑在一起就足够
把「谁」「哪把 Key」「花了多少钱」串成一条线。界面上要展示的其实只有
「配了没有」这一个信息，用 configured 这个布尔值就够了，不需要泄露 Key 的
任何一部分。所以本文件里既没有 api_key_tail，也没有 masked 之后的 Key。

**二、没有「删除用户」接口，一律用「停用」。**
models.py 里 generation_jobs.user_id / uploads.user_id / api_keys.user_id
都是 ondelete="SET NULL"。真删掉一个人，他名下的历史任务会在同一瞬间变成
无主数据：网页端谁都查不到（列表按 user_id 过滤），图还静静躺在磁盘上占空间，
worker 补投或重跑时也取不到他的网关 Key。而运营真正想要的从来不是「抹掉这个人」，
而是「让他登不进来」——那正是停用干的事，且随时可以撤销。
停用要立刻生效：is_active 置 False 的同时 token_version += 1，
这样他手里那张还没过期的 JWT（deps.py 会比对 ver）和 /files 的取图 Cookie
（file_access 同样校验 ver）会在同一秒一起失效，而不是等 7 天自然过期。

**停用还要连带停掉他名下的 ERP API Key**（见 toggle_user 里的长注释）。
这一条容易被漏掉，因为 API Key 那条路完全不经过登录：token_version 加多少次
都动不了它，只有把 api_keys.is_active 置 False 才断得掉。漏掉的后果是
「停用」这个动作只兑现了一半，而界面上是照着「全部失效」写的。
"""
import logging
import re
from typing import Any, Dict, List, Optional

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel
from sqlalchemy import func
from sqlalchemy.exc import IntegrityError
from sqlalchemy.orm import Session

from ..deps import get_db, require_admin
from ..models import ApiKey, User, UserGatewayAccount
from ..security import hash_password
from ..services import provider, settings_service, user_gateway

logger = logging.getLogger("designkit.users")

router = APIRouter(prefix="/api/web/users", tags=["网页-成员账号"])

# 角色白名单。这一处校验不严的话，P3 开自助注册时就是一个现成的提权漏洞：
# 注册请求里带上 role=admin 就成了管理员。所以哪怕现在只有管理员能调这个接口，
# 也要在这里把取值卡死，而不是等将来换鉴权时才想起来补。
ROLES = ("admin", "member")

# 用户名只允许字母、数字和 . _ -，3~32 位。
# 收得这么紧不是洁癖：登录接口拿 username 做的是**精确匹配**，
# 名字里混进一个空格或全角字符，本人照着自己看到的样子敲是登不进来的，
# 而界面上两者看起来一模一样，这种问题运营同学根本没法自查。
_USERNAME_RE = re.compile(r"^[A-Za-z0-9._-]{3,32}$")

# 初始密码的最短长度。管理员当面把它告诉本人，本人首次登录被强制改掉
#（must_change_password=True → deps.py 的闸门），所以它的寿命很短；
# 但短寿命不等于可以是 123，8 位是底线。
MIN_PASSWORD_LEN = 8


def _iso(dt) -> Optional[str]:
    return dt.isoformat() + "Z" if dt else None


class UserCreateBody(BaseModel):
    username: str
    display_name: str = ""
    role: str = "member"
    password: str


class ResetPasswordBody(BaseModel):
    password: str


class GatewayKeyBody(BaseModel):
    api_key: str = ""


def _validate_password(password: str) -> str:
    """校验并返回密码原文。

    故意不用 pydantic 的 Field(min_length=...)：那样报出来的是英文的
    "String should have at least 8 characters"，而且包在一个 detail 数组里，
    前端渲染出来是一串看不懂的东西。本项目的使用者是非技术背景的运营同学，
    报错必须是一句能照着做的中文。
    """
    if not password or len(password) < MIN_PASSWORD_LEN:
        raise HTTPException(status_code=422, detail="密码至少 %d 位" % MIN_PASSWORD_LEN)
    return password


def _gateway_to_dict(account: Optional[UserGatewayAccount]) -> Dict[str, Any]:
    """成员网关账号的对外展示结构。**这里永远不会出现 Key 本身**（原因见文件头）。

    configured 判的是 api_key_enc 有没有值，而不是账号行存不存在：
    clear_key 会保留账号行只清掉密文（好让网关那边已经建好的账号不变成孤儿），
    此时行还在、Key 已经没了，只看行存不存在会显示成「已配置」。
    """
    if account is None:
        return {"configured": False, "state": "", "last_error": "", "updated_at": None}
    return {
        "configured": bool(account.api_key_enc),
        # 取值见 models.GATEWAY_ACCOUNT_STATES。只有 active 才真的能生图
        "state": account.state or "",
        # 自动开通失败的原因，已经是中文人话，直接显示给管理员看
        "last_error": account.last_error or "",
        "updated_at": _iso(account.updated_at),
    }


def _user_to_dict(user: User, account: Optional[UserGatewayAccount]) -> Dict[str, Any]:
    return {
        "id": user.id,
        "username": user.username,
        "display_name": user.display_name or user.username,
        "role": user.role,
        "is_active": bool(user.is_active),
        # 带上它是为了让管理员在列表里一眼看出「这个人还没改初始密码」——
        # 建号之后本人迟迟没登录、或者密码条子丢了，都只能从这里看出来。
        # 这不是敏感信息（它只说明状态，不透露密码本身）。
        "must_change_password": bool(user.must_change_password),
        "created_at": _iso(user.created_at),
        "gateway": _gateway_to_dict(account),
    }


def _get_user_or_404(db: Session, uid: int) -> User:
    user = db.get(User, uid)
    if user is None:
        raise HTTPException(status_code=404, detail="成员不存在")
    return user


def _looks_masked(value: str) -> bool:
    """判断前端传回来的是不是「打码占位符」而非真 Key。

    照抄 settings_router.py 里已经跑了很久的那套约定：前 8 位全是星号就当作
    「用户没改这个字段」。少了这一条，管理员进页面只想改一下显示名、
    顺手点了保存，就会把成员的 Key 覆盖成一串星号——而且页面还会提示保存成功，
    直到那个人下次生图失败才发现。
    """
    return bool(value) and set(value[:8]) == {"*"}


@router.get("")
def list_users(_: User = Depends(require_admin), db: Session = Depends(get_db)) -> List[Dict[str, Any]]:
    """成员列表。按 id 升序，管理员（id=1）排在最前面。"""
    users = db.query(User).order_by(User.id.asc()).all()
    # 一次把网关账号全查出来做成字典，而不是在循环里逐个 get_account：
    # 后者是典型的 N+1 查询，人一多页面就开始肉眼可见地变慢。
    accounts = {a.user_id: a for a in db.query(UserGatewayAccount).all()}
    return [_user_to_dict(u, accounts.get(u.id)) for u in users]


@router.post("")
def create_user(
    body: UserCreateBody, admin: User = Depends(require_admin), db: Session = Depends(get_db)
):
    """新建成员账号。密码由管理员设定，本人首次登录时被强制改掉。

    为什么不做「系统随机生成初始密码并显示一次」：那需要管理员把一串随机字符
    抄给同事，抄错的概率极高，而且抄错了只能重置重来。管理员自己定一个
    当面说得清的初始密码，配合 must_change_password 强制改密，安全性一样，
    但少了一整类「密码抄错了」的求助。
    """
    username = (body.username or "").strip()
    if not _USERNAME_RE.match(username):
        raise HTTPException(
            status_code=422,
            detail="用户名只能用字母、数字和 . _ -，长度 3~32 位（例如 zhangsan 或 li.si）",
        )
    role = (body.role or "").strip()
    if role not in ROLES:
        raise HTTPException(status_code=422, detail="角色只能是 admin（管理员）或 member（成员）")
    password = _validate_password(body.password)

    # 不区分大小写地查重。数据库的唯一索引是区分大小写的，也就是说 zhangsan 和
    # Zhangsan 能同时存在——但登录接口是精确匹配，两个人会各自登进不同的账号，
    # 而管理员在列表里看到的是两行几乎一样的名字，排查起来极其痛苦。
    exists = db.query(User).filter(func.lower(User.username) == username.lower()).first()
    if exists is not None:
        raise HTTPException(status_code=409, detail="用户名「%s」已经有人用了，请换一个" % username)

    user = User(
        username=username,
        password_hash=hash_password(password),
        display_name=(body.display_name or "").strip(),
        role=role,
        is_active=True,
        # 走 must_change_password 这条路：初始密码是管理员知道的，
        # 不强制改掉的话，「管理员能登进任何一个成员的账号」这件事会一直成立。
        # 服务端闸门在 deps.py，前端拿到 403 会引导改密。
        must_change_password=True,
    )
    db.add(user)
    try:
        db.commit()
    except IntegrityError:
        # 两个管理员同时建同名账号时，上面的查重会双双通过、由数据库的唯一索引兜底。
        # 不接住的话前端收到的是 HTTP 500，看不出是重名。
        db.rollback()
        raise HTTPException(status_code=409, detail="用户名「%s」已经有人用了，请换一个" % username)
    db.refresh(user)
    logger.info("管理员 %s 创建了成员账号 %s（角色 %s）", admin.username, user.username, user.role)
    return _user_to_dict(user, None)


@router.post("/{uid}/toggle")
def toggle_user(
    uid: int, admin: User = Depends(require_admin), db: Session = Depends(get_db)
):
    """启用 / 停用成员。停用后他手里的令牌、图片链接、ERP API Key **立刻**全部失效。

    token_version += 1 是关键的一步：只把 is_active 置 False 的话，
    他那张还有几天有效期的 JWT 仍然能通过签名校验（deps.py 会查 is_active
    所以接口是拦住了），但 /files 的取图 Cookie 是自带身份的独立凭证，
    只认 user_id + ver —— 不 +1 的话，一个刚被停用的人还能继续把图片链接
    发给外面的人用上好几天。启用时同样 +1，不多不少，逻辑只有一条。

    ────────────────────────────────────────────────────────────
     为什么停用账号要连带停掉他名下的 ERP API Key
    ────────────────────────────────────────────────────────────
    ERP API Key 是一条**完全独立于登录**的入口：deps.py 的 get_api_client 只看
    这把 Key 自己的 is_active，从来不查属主还在不在职；files.py 的签名链接校验
    同理。所以在这一段代码之前，一个刚被停用的人（离职、账号疑似泄露）只要手上
    留着一把自己名下的 Key，就能继续 POST /api/v1/generations 生图——在默认的
    「共用一把 Key」计费方式下花的是系统设置里那把全局 Key 的钱——并继续用
    签名链接把图片下载走。而管理员在界面上刚刚被告知「他的图片链接已立即失效」。

    停用要成为一个**说到做到**的动作，所以这里把他名下还启用着的 Key 一起关掉，
    并把关掉的把数写进 message 让管理员看得见（他名下有几把 Key，界面上本来
    是不显示的）。

    **重新启用时刻意不自动恢复这些 Key**：停用往往就是为了拔掉某条对接，
    自动恢复意味着「误停一下再启用回来」会把已经决定断开的 ERP 又接上，
    而且管理员不会意识到发生了这件事。要恢复请到「API 对接」页显式重开——
    那一页看得见每把 Key 的名字和属主，重开哪一把是一个有意识的选择。
    """
    user = _get_user_or_404(db, uid)
    if user.is_active and user.id == admin.id:
        # 停用自己 = 当场把自己锁在门外（下一次请求就 401），而且如果这是最后一个
        # 管理员，就再也没有人能把任何账号启用回来了——只能去改数据库。
        raise HTTPException(status_code=400, detail="不能停用你自己的账号")
    user.is_active = not user.is_active
    user.token_version = (user.token_version or 0) + 1

    if user.is_active:
        # 这次是「启用」。停用时一并关掉的 Key 不自动恢复（原因见上），
        # 但要如实告诉管理员他名下还躺着几把停用状态的 Key，
        # 否则 ERP 那头一直调不通，没人想得到要去「API 对接」页看一眼。
        disabled_keys = 0
        inactive_keys = (
            db.query(ApiKey)
            .filter(ApiKey.user_id == user.id, ApiKey.is_active.is_(False))
            .count()
        )
    else:
        # 这次是「停用」。只动**还启用着**的那些，管理员自己早先手工停掉的
        # 保持原样（数量也就不会被重复计进 message 里）。
        # 用 ORM 逐行改而不是 bulk update：一个人名下的 Key 最多几把，
        # 和主对象同一个事务提交，SQLite 与 PostgreSQL 行为一致。
        rows = (
            db.query(ApiKey)
            .filter(ApiKey.user_id == user.id, ApiKey.is_active.is_(True))
            .all()
        )
        for row in rows:
            row.is_active = False
        disabled_keys = len(rows)
        # 停用这一路不需要 inactive_keys（提示语说的是「这次关掉了几把」），
        # 置 0 让返回结构在两条路上保持同一个形状。
        inactive_keys = 0

    db.commit()
    db.refresh(user)
    logger.info(
        "管理员 %s 把成员 %s 改为%s%s", admin.username, user.username,
        "启用" if user.is_active else "停用",
        ("，连带停用了 %d 把 API Key" % disabled_keys) if disabled_keys else "",
    )

    who = user.display_name or user.username
    if user.is_active:
        message = "已启用「%s」，他可以重新登录了。" % who
        if inactive_keys:
            message += (
                "注意：他名下还有 %d 把已停用的 API Key，启用账号不会把它们一起打开。"
                "要恢复 ERP 对接，请到「API 对接」页把对应的 Key 逐把重新启用。" % inactive_keys
            )
    else:
        message = "已停用「%s」，他的登录和网页里的图片链接立刻失效。" % who
        if disabled_keys:
            message += (
                "同时停用了他名下的 %d 把 API Key，用这些 Key 对接的 ERP 会立刻调不通。"
                "以后把账号启用回来时这些 Key 不会自动恢复，需要到「API 对接」页逐把重开。"
                % disabled_keys
            )
        else:
            message += "他名下没有启用中的 API Key。"

    data = _user_to_dict(user, user_gateway.get_account(db, user.id))
    # 下面三个是「这一次操作」的说明字段，不是成员本身的状态。
    # 成员本体的字段（is_active 等）保持在顶层原样不动——前端和 e2e 都直接读顶层。
    # 前端靠 disabled_api_keys / inactive_api_keys 这两个数字决定提示要不要常驻，
    # 所以它们必须是数字，不能只把话写进 message 里让前端去匹配字眼。
    data["message"] = message
    data["disabled_api_keys"] = disabled_keys
    data["inactive_api_keys"] = inactive_keys
    return data


@router.post("/{uid}/reset_password")
def reset_password(
    uid: int,
    body: ResetPasswordBody,
    admin: User = Depends(require_admin),
    db: Session = Depends(get_db),
):
    """管理员重置某个成员的密码（用于「忘记密码」）。

    重置之后这个人的状态和刚建号时完全一样：拿着新的初始密码登录，
    第一件事被强制改掉。token_version +1 让他所有设备上的旧登录立即失效——
    「忘记密码」的场景里，本来就该假设旧会话可能已经不在本人手上了。
    """
    user = _get_user_or_404(db, uid)
    if user.id == admin.id:
        # 给自己「重置」会把自己踢下线并强制再改一次密码，纯属绕远路。
        # 这句话必须指到**真实存在**的位置：右上角那个用户菜单里只有「接口文档」
        # 和「退出登录」两项（见 frontend/js/core/app-shell.js），没有「修改密码」。
        # 指错地方比不给提示更糟——非技术用户会相信提示，反复在错的地方找。
        raise HTTPException(
            status_code=400,
            detail="给自己改密码请到左侧导航最下面的「我的」页（管理员也可以在「系统设置」页最下面改）",
        )
    password = _validate_password(body.password)
    user.password_hash = hash_password(password)
    user.must_change_password = True
    user.token_version = (user.token_version or 0) + 1
    db.commit()
    db.refresh(user)
    logger.info("管理员 %s 重置了成员 %s 的密码", admin.username, user.username)
    return {
        "ok": True,
        "message": "已重置。请把新密码当面告诉本人，他登录后会被要求立即修改。",
        "user": _user_to_dict(user, user_gateway.get_account(db, user.id)),
    }


@router.put("/{uid}/gateway")
def set_gateway_key(
    uid: int,
    body: GatewayKeyBody,
    admin: User = Depends(require_admin),
    db: Session = Depends(get_db),
):
    """给某个成员配置他专属的生图网关 Key。

    state 用 upsert_key 的默认值 "active"，**不要显式传 "manual"**。
    settings_service.resolve_for_user 只认 active；传 manual 的话页面会提示
    保存成功，而该成员一点生成还是被拦住，且界面上完全看不出原因。
    （models.py 里 "manual" 那个状态是留给「记录来源」的语义的，不是给这里用的。）
    """
    user = _get_user_or_404(db, uid)
    raw = (body.api_key or "").strip()
    if not raw or _looks_masked(raw):
        # 没填、或者把界面上的打码占位符原样交回来 → 什么都不做。
        return {
            "ok": True,
            "changed": False,
            "message": "没有填写新的 Key，原有配置保持不变。",
            "gateway": _gateway_to_dict(user_gateway.get_account(db, user.id)),
        }
    account = user_gateway.upsert_key(db, user.id, raw)
    db.commit()  # user_gateway 只 flush，提交由调用方负责（见该模块注释）
    db.refresh(account)
    logger.info("管理员 %s 更新了成员 %s 的网关 Key", admin.username, user.username)

    message = "已保存。建议点一次「测试连接」确认这把 Key 可用。"
    if settings_service.get(db, "gateway_mode") != "per_user":
        # 这一句是防「配了但没生效」的：计费方式还停在 shared 时，
        # 全体依旧共用系统设置里那把全局 Key，这里配的个人 Key 一个字都不会被用到。
        # 不提醒的话，管理员会以为已经切成各花各的了。
        message += "注意：当前计费方式还是「共用一把 Key」，个人 Key 要等在系统设置里改成「每人一把」后才会生效。"
    return {
        "ok": True,
        "changed": True,
        "message": message,
        "gateway": _gateway_to_dict(account),
    }


@router.delete("/{uid}/gateway")
def clear_gateway_key(
    uid: int, admin: User = Depends(require_admin), db: Session = Depends(get_db)
):
    """清掉某个成员的专属 Key（账号行保留，状态置 disabled）。

    不删整行的原因见 user_gateway.clear_key：网关那边已经建好的账号
    （remote_user_id / remote_email）如果在本地失去记录，下次再给同一个人开通
    要么重复建号、要么撞上「邮箱已存在」，两种都很难查。
    """
    user = _get_user_or_404(db, uid)
    account = user_gateway.clear_key(db, user.id)
    db.commit()  # 同上：clear_key 只 flush
    logger.info("管理员 %s 清除了成员 %s 的网关 Key", admin.username, user.username)
    return {
        "ok": True,
        "message": "已清除。该成员在「每人一把 Key」模式下将无法生图，直到重新配置。",
        "gateway": _gateway_to_dict(account),
    }


@router.post("/{uid}/gateway/test")
def test_gateway(
    uid: int, _: User = Depends(require_admin), db: Session = Depends(get_db)
):
    """测试某个成员现在到底能不能生图（不产生生图费用）。

    走的是 provider.test_connection，它只 GET 一次 /models，不出图、不花钱，
    可以随便点。

    关键在于用 resolve_for_user(db, uid) 取设置，而不是全局设置——它就是
    worker 真正生图时决定「用谁的 Key」的那一个函数，所以这里测出来的结果
    和这个人点「生成」时的结果是同一条路径，不会出现「测试通过、一生成就失败」。
    """
    user = _get_user_or_404(db, uid)
    # 照实说清楚这次到底测的是哪把 Key。这句话在**成功和失败两条路上都要加**：
    # 失败时反而更要紧——管理员在某个成员那一行看到「无法连接」，第一反应必然是
    # 「这个人的 Key 填错了」，于是去重填、重发，折腾半天；而真实原因是全局网关
    # 本身连不上，和这个人一点关系都没有。
    note = ""
    if settings_service.get(db, "gateway_mode") != "per_user":
        note = "（当前计费方式是「共用一把 Key」，这次测的是系统设置里的全局 Key，不是该成员的个人 Key）"

    try:
        settings = settings_service.resolve_for_user(db, user.id)
    except settings_service.GatewayNotReady as exc:
        # 这个人没有可用的 Key。这条路径是**故意失败**的，绝不能退回全局 Key 去测——
        # 那样会测出「连接成功」，然后他一点生成就报错，等于测了个寂寞。
        # 这条分支只在 per_user 模式下才可能走到，所以不加 note。
        return {"ok": False, "message": str(exc)}
    try:
        return {"ok": True, "message": provider.test_connection(settings) + note}
    except provider.ProviderError as exc:
        return {"ok": False, "message": str(exc) + note}
