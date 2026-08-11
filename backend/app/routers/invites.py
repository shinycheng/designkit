"""邀请码管理（只有管理员能用）。

自助注册的第一版靠邀请码：**没有码就注册不了**。这个文件负责发码、看码、废码，
真正拿码换账号的那一头在 routers/register.py。

════════════════════════════════════════════════════════════════
 三条硬规矩，改这个文件之前先读完
════════════════════════════════════════════════════════════════

**一、码必须是猜不出来的。**
绝不能用自增数字、日期、手机号后四位这类「看一张就能推出下一张」的东西——
自助注册接口是对着公网开的，能猜出码就等于任何人都能开号。所以码是
`secrets` 生成的 12 位随机串（32 个可用字符，约 10^18 种组合），
枚举它比撞密码还难，而注册接口本身还有限速兜着。

**二、码是明文存的，这是有意的取舍（见 models.py 的 InviteCode 注释）。**
运营同学要在界面上看见码、复制下来发给人、还要能再发一次给同一个人；
哈希之后这三件事全做不了。代价是拿到数据库就拿到了所有未用的码，
而这个代价可以接受：一张码只能换来一个**普通成员**账号，管理员随时能作废。
→ 所以这里的接口一律要求管理员权限，一个字都不许给非管理员看。

**三、只有「作废」，没有「删除」。**
删掉一张码，等于把「这张码开出过哪些账号」这份唯一的证据一起删了——
而管理员想删码的那一刻（码被转发到群里刷号了），恰恰是最需要这份证据的时候。
作废是写一个时间戳，码还在、流水还在、谁用过一目了然，而且立刻不能再用。

════════════════════════════════════════════════════════════════
 码为什么长这样：12 位、去掉了 I L O U
════════════════════════════════════════════════════════════════
码是要被人**用微信发出去、用眼睛读、用手打进去**的，所以字符集里不能有
互相看不出区别的字符：数字 0 和字母 O、数字 1 和字母 I / L 是最经典的两对。
这里用的是 Crockford Base32 的字符集（去掉 I L O U，U 是为了避免随机拼出脏字），
并且**校验时会把用户打进来的 I / L 自动当成 1、O 当成 0**——
用户看着一模一样的字符却被告知「邀请码无效」，是这类功能最劝退的一种失败。
展示时每 4 位加一个连字符（ABCD-EFGH-JKMN），只是为了好读；
存进库和比对的永远是**去掉连字符、全大写**的那一串。
"""
import logging
import secrets
from datetime import datetime, timedelta
from typing import Any, Dict, List, Optional, Union

from fastapi import APIRouter, Depends, HTTPException, Query
from pydantic import BaseModel
from sqlalchemy.exc import IntegrityError
from sqlalchemy.orm import Session

from ..deps import get_db, require_admin
from ..models import InviteCode, InviteRedemption, User, utcnow
from ..services import settings_service

logger = logging.getLogger("designkit.invites")

router = APIRouter(prefix="/api/web/invites", tags=["网页-邀请码"])

# 生成码用的字符集：Crockford Base32（0-9 和 A-Z 去掉 I L O U）。
# 32 个字符 × 12 位 ≈ 1.2×10^18 种，随机猜中的概率可以忽略。
_CODE_ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
_CODE_LENGTH = 12
# 用户输入时的「看错了」补救：这三个字符不在上面的字符集里，
# 出现了就说明他把 1 看成了 I/L、把 0 看成了 O，直接换回去。
_CONFUSABLE_INPUT = {"I": "1", "L": "1", "O": "0"}
# 输入里这些分隔符一律丢掉：展示时加的连字符、从聊天记录复制带的空格、
# 中文输入法下的全角空格。留着的话用户复制粘贴过来必然对不上。
_SEPARATORS = frozenset("-_ \t\r\n　")

# 备注长度上限，必须 ≤ models.InviteCode.note 的列宽（128）。
# PostgreSQL 上超长直接报错、SQLite 却照单全收，两边行为不一致的问题最难查，
# 所以在进库之前就拦住并说清楚。
MAX_NOTE_LENGTH = 128

# 一次最多发几张码。给上限是防手滑（填成 1000 张，界面会被刷屏，
# 而且这些码全都是真能用的），真要拉一批人可以多点几次。
MAX_BATCH = 20
# 列表一次最多返回多少张。码只增不减（只作废、不删除），
# 不封顶的话用一两年之后这个接口会越来越慢。
MAX_LIST_LIMIT = 500
# 每张码在列表里最多附带多少条使用记录。一张码通常只用一次，
# 这个上限只在「某张码被转发到群里刷号」时才碰得到——那时候前几十条足够定位问题了。
MAX_REDEMPTIONS_PER_CODE = 50


# ────────────────────────────── 码的生成与归一化 ──────────────────────────────

def normalize_code(raw: str) -> str:
    """把用户输进来的码收拾成库里存的那种形式：大写、去分隔符、纠正易混字符。

    **注册接口校验邀请码时必须调用同一个函数**（routers/register.py 就是这么做的）。
    写入和校验用两套规则，是这类功能最容易出的错：管理员发出去的码
    在界面上显示得好好的，用户照着打进去却说无效，两边谁也查不出差在哪。

    截断到 32 位是硬要求：models.InviteCode.code 的列宽就是 32，
    有人粘一段 10KB 的文本进来，不截断会在 PostgreSQL 上直接把接口打成 500。
    """
    text = (raw or "").strip().upper()
    out = []
    for ch in text:
        if ch in _SEPARATORS:
            continue
        out.append(_CONFUSABLE_INPUT.get(ch, ch))
    return "".join(out)[:32]


def format_code(code: str) -> str:
    """展示用：每 4 位加一个连字符（ABCDEFGHJKMN → ABCD-EFGH-JKMN）。

    只影响眼睛看到的样子，**不影响存储和比对**——normalize_code 会把连字符去掉。
    分组是为了让人抄得对：连着 12 位不分组，抄错一位的概率高得多。
    """
    text = code or ""
    return "-".join(text[i:i + 4] for i in range(0, len(text), 4))


def _random_code() -> str:
    """生成一张码。**必须用 secrets，不能用 random**。

    random 是可预测的伪随机（同一个种子出同一串），拿它发邀请码，
    知道算法的人可以从已经拿到的一张码推算出别人的码。secrets 走的是操作系统的
    密码学随机源，没有这个问题，而且这里也没有任何性能压力。
    """
    return "".join(secrets.choice(_CODE_ALPHABET) for _ in range(_CODE_LENGTH))


# ────────────────────────────── 小工具 ──────────────────────────────

def _iso(dt: Optional[datetime]) -> Optional[str]:
    """裸 UTC 时间 → 带 Z 的 ISO 串。

    这个 Z 一个字母都不能省：库里存的是不带时区的 UTC（models.utcnow），
    少了它浏览器会按本地时区解释，中国时区直接差 8 小时——
    「7 天后过期」会显示成「6 天 16 小时后过期」，运营会以为系统算错了。
    routers/users.py 和 routers/account.py 里都有同样的一行，为的是同一个坑。
    """
    return dt.isoformat() + "Z" if dt else None


def _fail(detail: str) -> HTTPException:
    """统一用 422 + 一句中文人话。

    故意不靠 pydantic 的 Field(ge=..., max_length=...) 做校验：那样报出来的是
    英文的 "Input should be greater than or equal to 1"，还包在一个 detail 数组里，
    前端渲染出来是一串看不懂的东西。本项目的使用者是非技术背景的运营同学，
    报错必须是一句能照着做的中文（routers/users.py 的 _validate_password 同理）。
    """
    return HTTPException(status_code=422, detail=detail)


def _as_int(value: Any, label: str, low: int, high: int, default: int) -> int:
    """把前端传来的数字收拾成整数并夹到范围内。

    **要能接住字符串**：前端是无构建的原生 JS，输入框读出来的是字符串 "7"，
    不是数字 7。只认 int 的话，运营在界面上填什么都会得到一个 422。
    空值（没填 / 清空了输入框）按默认值处理，而不是报错——
    「不填就用默认」是这几个字段最自然的语义。
    """
    if value is None or (isinstance(value, str) and not value.strip()):
        return default
    if isinstance(value, bool):  # bool 是 int 的子类，必须先排掉
        raise _fail("%s 必须是数字" % label)
    try:
        number = int(str(value).strip())
    except (TypeError, ValueError):
        raise _fail("%s 必须是数字" % label)
    if not low <= number <= high:
        raise _fail("%s 只能填 %d 到 %d 之间的数字" % (label, low, high))
    return number


def _setting_int(db: Session, key: str, default: int) -> int:
    """读一个整数设置项；读不出数字就用默认值（设置是人填的，可能填成鬼东西）。"""
    raw = settings_service.get(db, key)
    if raw is None or isinstance(raw, bool):
        return default
    try:
        return int(str(raw).strip())
    except (TypeError, ValueError):
        return default


def _status(code: InviteCode, now: datetime) -> Dict[str, str]:
    """这张码现在能不能用，以及一句给人看的说明。

    判断顺序就是「哪个原因更要紧」的顺序：作废是管理员主动做的，最优先说；
    然后是过期，最后才是用完。一张既过期又用完的码，说「已过期」更贴近事实
    （它是先到期的），也更能提示管理员「以后有效期可以给长一点」。
    """
    if code.revoked_at is not None:
        return {"state": "revoked", "label": "已作废"}
    if code.expires_at is not None and code.expires_at <= now:
        return {"state": "expired", "label": "已过期"}
    if (code.used_count or 0) >= (code.max_uses or 0):
        return {"state": "used_up", "label": "已用完"}
    return {"state": "active", "label": "可用"}


def _code_to_dict(
    code: InviteCode,
    now: datetime,
    creators: Dict[int, str],
    redemptions: Dict[int, List[Dict[str, Any]]],
) -> Dict[str, Any]:
    status = _status(code, now)
    # 建码的人可能已经被删掉了（created_by_user_id 是 SET NULL，会变成 None）。
    # 那时显示「已注销的管理员」而不是留空——留空会让人以为这张码是系统自己冒出来的。
    if code.created_by_user_id is None:
        created_by = ""
    else:
        created_by = creators.get(code.created_by_user_id) or "已注销的管理员"
    return {
        "id": code.id,
        # 码本身。**这是整个接口里唯一敏感的字段**，所以这个路由只给管理员。
        "code": code.code,
        "code_display": format_code(code.code),
        "max_uses": int(code.max_uses or 0),
        "used_count": int(code.used_count or 0),
        # 还能用几次。前端不用自己减，免得两边算法不一致（比如作废之后该显示几次）
        "remaining": max(0, int(code.max_uses or 0) - int(code.used_count or 0)),
        "state": status["state"],
        "state_label": status["label"],
        "note": code.note or "",
        "expires_at": _iso(code.expires_at),
        "revoked_at": _iso(code.revoked_at),
        "created_at": _iso(code.created_at),
        "created_by": created_by,
        # 谁用了这张码。出事时（码被转发到群里）唯一能定位到人的东西。
        "redemptions": redemptions.get(code.id, []),
    }


# ────────────────────────────── 接口 ──────────────────────────────

class InviteCreateBody(BaseModel):
    """新建邀请码。四个字段全都可以不填，不填就用「系统设置」里的默认值。

    类型写成 Union[int, str, None] 是刻意的：前端是原生 JS，从 <input> 里读出来的
    是字符串。只声明 int 的话，pydantic 会在进函数之前就抛一个英文 422，
    我们精心写的中文报错一个都用不上。转换和范围检查统一由 _as_int 做。
    """

    max_uses: Optional[Union[int, str]] = None
    valid_days: Optional[Union[int, str]] = None
    count: Optional[Union[int, str]] = None
    note: str = ""


@router.post("")
def create_invites(
    body: InviteCreateBody,
    admin: User = Depends(require_admin),
    db: Session = Depends(get_db),
) -> Dict[str, Any]:
    """发邀请码。一次可以发多张（每张都是独立的码）。

    ────────────────────────────────────────────────────────────
     为什么默认是「一张码只能用一次」，想拉一批人请多发几张
    ────────────────────────────────────────────────────────────
    把一张码的次数调大看起来更省事，但那张码一旦被转发到群里，
    你只知道「有 20 个号是它开的」，不知道该停哪一个；而 20 张一次性码，
    每张发给谁都记在备注里，出事时精确到人。所以默认值是 1，
    这个接口也把 count（发几张）和 max_uses（每张能用几次）分成了两个字段。

    有效期默认 7 天（可改）。填 0 表示永不过期——接口允许，但会在返回的提示语里
    明确说出代价：一张永久有效的码就是一张躺在库里的长期通行证，
    发出去就再也收不回来了（除非管理员记得回来作废，而没人会记得）。
    """
    now = utcnow()
    default_max_uses = _setting_int(db, "invite_code_default_max_uses", 1)
    default_valid_days = _setting_int(db, "invite_code_default_valid_days", 7)

    # 上限 1000 次纯粹是防手滑：真需要一张能用 1000 次的码，本质上就是「不要邀请码」，
    # 那该做的是另一个决定（关掉邀请码机制），而不是在这里填一个大数。
    max_uses = _as_int(body.max_uses, "可用次数", 1, 1000, default_max_uses)
    # 上限 365 天：再长和「永久」没有实质区别，而永久要走填 0 这条显式的路。
    valid_days = _as_int(body.valid_days, "有效天数", 0, 365, default_valid_days)
    count = _as_int(body.count, "生成张数", 1, MAX_BATCH, 1)
    note = (body.note or "").strip()
    if len(note) > MAX_NOTE_LENGTH:
        raise _fail("备注不能超过 %d 个字" % MAX_NOTE_LENGTH)

    expires_at = now + timedelta(days=valid_days) if valid_days > 0 else None

    created: List[InviteCode] = []
    for _ in range(count):
        created.append(_insert_one(db, admin, max_uses, expires_at, note))
    db.commit()
    for row in created:
        db.refresh(row)
    logger.info(
        "管理员 %s 生成了 %d 张邀请码（每张可用 %d 次，%s）",
        admin.username, count, max_uses,
        "永不过期" if expires_at is None else ("%d 天后过期" % valid_days),
    )

    message = "已生成 %d 张邀请码，把码发给对方即可。" % count
    if expires_at is None:
        message += (
            "注意：这批码永不过期，发出去就收不回来了（只能事后作废），"
            "建议尽快用完，或者用完之后回来点「作废」。"
        )
    else:
        message += "有效期 %d 天，过期后自动失效。" % valid_days
    if max_uses > 1:
        message += (
            "每张码可以用 %d 次——同一张码开出来的账号，事后只知道是这张码开的，"
            "分不清谁是谁；要精确到人，请改成「一张码用一次」多发几张。" % max_uses
        )
    if not _self_register_on(db):
        message += "另外：「自助注册」总开关还没打开，别人现在拿着码也注册不了，请到「系统设置」里打开。"

    return {
        "ok": True,
        "message": message,
        "items": [
            _code_to_dict(row, now, {admin.id: admin.display_name or admin.username}, {})
            for row in created
        ],
    }


def _insert_one(
    db: Session,
    admin: User,
    max_uses: int,
    expires_at: Optional[datetime],
    note: str,
) -> InviteCode:
    """插一张码，撞了重摇。

    12 位随机码撞车的概率小到可以忽略，这个重试循环几乎永远跑不到第二轮；
    留着它是因为「撞了会怎样」的答案必须是明确的——没有它的话，
    极小概率下管理员会看到一个 500，而重来一次又好了，谁也说不清发生了什么。
    用 SAVEPOINT（begin_nested）包住，撞车只回滚这一小段，
    不会把同一批里已经建好的其他码一起带走（写法照抄 provisioning.ensure_account）。
    """
    for attempt in range(6):
        row = InviteCode(
            code=_random_code(),
            max_uses=max_uses,
            used_count=0,
            expires_at=expires_at,
            revoked_at=None,
            note=note,
            created_by_user_id=admin.id,
        )
        db.add(row)
        try:
            with db.begin_nested():
                db.flush()
            return row
        except IntegrityError:
            logger.warning("邀请码撞车了（第 %d 次），重新生成一张", attempt + 1)
    raise HTTPException(status_code=500, detail="生成邀请码失败，请再试一次")


@router.get("")
def list_invites(
    limit: int = Query(200, ge=1, le=MAX_LIST_LIMIT),
    _: User = Depends(require_admin),
    db: Session = Depends(get_db),
) -> Dict[str, Any]:
    """邀请码列表：每张码用了几次、被谁用了、还能不能用。

    返回的是一个对象而不是裸数组，因为除了码本身，这一页还要显示两样东西：
      - defaults：新建时的预填值（从系统设置里读），免得前端把默认值再抄一遍，
        将来管理员改了设置、界面上却还是老数字；
      - notice：「码建好了但注册总开关没开」这类**看列表看不出来**的状态。
        少了它，管理员会发完码坐等，然后收到一句「我注册不了」。
    """
    now = utcnow()
    codes = (
        db.query(InviteCode)
        .order_by(InviteCode.created_at.desc(), InviteCode.id.desc())
        .limit(limit)
        .all()
    )
    # 这两个查询**必须在循环外面各查一次**，不能写进下面的推导式里——
    # 写进去就是每张码各查一遍，也就是最经典的 N+1。
    creators = _creators(db, codes)
    redemptions = _redemptions(db, codes)
    return {
        "items": [_code_to_dict(row, now, creators, redemptions) for row in codes],
        "defaults": {
            "max_uses": _setting_int(db, "invite_code_default_max_uses", 1),
            "valid_days": _setting_int(db, "invite_code_default_valid_days", 7),
        },
        "self_register_enabled": _self_register_on(db),
        "notice": _notice(db, codes, now),
    }


def _creators(db: Session, codes: List[InviteCode]) -> Dict[int, str]:
    """一次把发码人的名字全查出来。

    不在循环里逐个 db.get(User, ...)：那是典型的 N+1 查询，
    码一多这一页就会肉眼可见地变慢（routers/users.py 的列表接口踩过同一个坑）。
    """
    ids = {row.created_by_user_id for row in codes if row.created_by_user_id}
    if not ids:
        return {}
    users = db.query(User).filter(User.id.in_(list(ids))).all()
    return {u.id: (u.display_name or u.username) for u in users}


def _redemptions(db: Session, codes: List[InviteCode]) -> Dict[int, List[Dict[str, Any]]]:
    """一次把这批码的使用记录全查出来，再按码分组（同样是为了避开 N+1）。

    记录里带上 client_ip 是给排查用的：同一张码的几次使用如果来自同一个 IP，
    多半是一个人在刷号。**但它只是线索不是判据**——套了反向代理时，
    这一列会全站显示成反代自己的地址（见 models.InviteRedemption 的注释）。
    """
    ids = [row.id for row in codes]
    if not ids:
        return {}
    rows = (
        db.query(InviteRedemption)
        .filter(InviteRedemption.invite_code_id.in_(ids))
        .order_by(InviteRedemption.created_at.desc(), InviteRedemption.id.desc())
        .all()
    )
    grouped: Dict[int, List[Dict[str, Any]]] = {}
    for row in rows:
        bucket = grouped.setdefault(row.invite_code_id, [])
        if len(bucket) >= MAX_REDEMPTIONS_PER_CODE:
            continue
        bucket.append({
            # 账号可能事后被删（user_id 是 SET NULL），所以显示一律用快照，
            # 而 user_id 只是给「点进去看这个人」用的，可能是 null。
            "user_id": row.user_id,
            "username": row.username_snapshot or "",
            "client_ip": row.client_ip or "",
            "created_at": _iso(row.created_at),
        })
    return grouped


def _self_register_on(db: Session) -> bool:
    return bool(settings_service.get(db, "self_register_enabled"))


def _notice(db: Session, codes: List[InviteCode], now: datetime) -> str:
    """列表页顶部的提醒。**只说列表本身看不出来的事**，别把每一行都复述一遍。"""
    parts: List[str] = []
    if not _self_register_on(db):
        parts.append(
            "「自助注册」总开关还没打开，别人拿着邀请码也注册不了。"
            "要开放注册请到「系统设置」里打开这个开关。"
        )
    elif bool(settings_service.get(db, "allow_internal_targets")):
        # 这是一条真正危险的组合（陌生人能注册 + 服务端愿意去访问内网地址），
        # 详见 config.py 里 allow_internal_targets 那一条的说明。
        # 正常情况下设置接口就该拦住它，走到这里说明是老库里遗留的状态，
        # 所以管理员这一页要用最直白的话喊出来。
        parts.append(
            "危险组合：自助注册已开放，但「允许访问内网地址」还开着。"
            "请立刻到「系统设置」里把它关掉，否则注册进来的陌生人可以让本系统"
            "去访问你内网里的任意地址。"
        )
    live = [c for c in codes if _status(c, now)["state"] == "active"]
    forever = [c for c in live if c.expires_at is None]
    if forever:
        parts.append(
            "有 %d 张永不过期的码还在生效中，发出去就收不回来了，"
            "用完记得回来点「作废」。" % len(forever)
        )
    return "".join(parts)


@router.post("/{code_id}/revoke")
def revoke_invite(
    code_id: int, admin: User = Depends(require_admin), db: Session = Depends(get_db)
) -> Dict[str, Any]:
    """作废一张码：立刻不能再用来注册，但**已经用它注册成功的账号一个都不动**。

    这两件事必须分开，界面上也要说清楚：作废是「把还没用掉的名额收回来」，
    不是「把用这张码开的号一起停掉」。真要停号，请到「成员账号」页逐个停用——
    那是一个针对具体某个人的决定，不该由「作废一张码」顺手替管理员做了。

    **没有删除接口**（原因见文件头）：删掉码等于删掉「这张码开了哪些号」这份证据。
    """
    code = db.get(InviteCode, code_id)
    if code is None:
        raise HTTPException(status_code=404, detail="这张邀请码不存在（可能已经被别人处理过了）")
    if code.revoked_at is not None:
        # 重复点「作废」不报错：管理员多点一次不该看到红字，
        # 而且这个接口本来就是幂等的（码已经废了，再废一次结果一样）。
        return {
            "ok": True,
            "message": "这张码之前已经作废了，现在依然是作废状态。",
            "item": _code_to_dict(code, utcnow(), _creators(db, [code]), _redemptions(db, [code])),
        }
    used = int(code.used_count or 0)
    code.revoked_at = utcnow()
    db.commit()
    db.refresh(code)
    logger.info(
        "管理员 %s 作废了邀请码 %s（此前已被使用 %d 次）", admin.username, code.code, used)

    message = "已作废，这张码不能再用来注册了。"
    if used:
        message += (
            "注意：此前已经有 %d 个账号是用这张码注册的，作废不会把它们停掉——"
            "要停请到「成员账号」页逐个操作（这一行的「被谁用了」里有名单）。" % used
        )
    return {
        "ok": True,
        "message": message,
        "item": _code_to_dict(code, utcnow(), _creators(db, [code]), _redemptions(db, [code])),
    }
