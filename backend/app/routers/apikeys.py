"""对外 API Key（ERP 等平台对接用）的网页管理接口。

这个文件做的事只有一件：把 api_keys.user_id 这一列**真正用起来**——
列是 P1-1 加的，存量归属是 P1-3 回填的，到这里才有人读它。
具体是三处：列表按属主过滤、toggle 与 delete 做归属判断、创建时写上属主。

为什么现在就写：整条路由今天仍然挂在 require_admin 下面，全站只有管理员
一个人进得来，这些判断一条都不会被触发。但恰恰是这种「反正现在进不来」的
地方最容易埋雷——哪天为了让成员自助创建自己的对接 Key，把 require_admin
换成 get_current_user（那是一行改动，看上去人畜无害），api_keys.id 是从 1
开始的连续整数自增主键，成员 B 从 1 数到 100 就能把成员 A 的对接 Key 挨个
停用或删掉，而 ERP 那头只会看到「API Key 无效或已停用」，没人查得出原因。
把过滤和归属判断现在就写对，将来放宽入口时不需要再想起这件事。

另外记一句会影响到账单的事实（文档里也要写）：**ERP Key 归谁，ERP 建的
任务就花谁的网关余额**。今天所有 Key 都归管理员，而管理员按 P1-6 的规则会
回落到全局 Key，所以行为和以前完全一样；但哪天管理员给自己配了个人 Key，
ERP 的费用就会开始从管理员的个人余额里走——这是预期行为，不是 bug。
"""
from typing import Dict, List, Optional

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session

from ..deps import get_db, require_admin
from ..models import ApiKey, User
from ..security import hash_api_key, new_api_key, new_webhook_secret
from ..serializers import apikey_to_dict

router = APIRouter(prefix="/api/web/apikeys", tags=["网页-对外APIKey"])

# 越权和不存在返回**同一个 404、同一句话**，不用 403。
# 理由和 routers/generations.py 的 _JOB_NOT_FOUND 完全一致（口径必须统一，
# 否则两个接口一对照又能推出东西来）：如果「不存在→404、越权→403」，
# 两个状态码一比就是一台「这个 id 的 Key 存不存在」的探测器；而 Key 的 id
# 是从 1 开始的连续整数，从头数一遍就能数出全站一共发了多少把对接 Key、
# 哪些还活着——这些信息本身就不该漏出去。
_KEY_NOT_FOUND = "API Key 不存在"


class ApiKeyCreateBody(BaseModel):
    name: str = Field(min_length=1, max_length=64, description="例如：XX ERP 生产环境")
    monthly_quota: Optional[int] = Field(default=None, ge=1)


def _owner_names(db: Session, rows: List[ApiKey]) -> Dict[int, str]:
    """一次查出这批 Key 的属主显示名，返回 {user_id: 显示名}。

    单独查一次是为了避免 N+1：Key 列表页一屏可能十几行，每行一次查询在
    SQLite 上感觉不出来，在 NAS 的 PostgreSQL 上就是十几个来回。
    （ApiKey 上也没有 relationship 可用，见 serializers.apikey_to_dict 的说明。）

    显示名用 display_name，没填则回落 username——和 auth.py 的 /me
    以及前端 app-shell.js 的取法保持一致，同一个人在各处显示的名字要一样。
    """
    ids = sorted({k.user_id for k in rows if k.user_id is not None})
    if not ids:
        return {}
    pairs = (
        db.query(User.id, User.display_name, User.username)
        .filter(User.id.in_(ids))
        .all()
    )
    return {uid: (display or username or "") for uid, display, username in pairs}


def _get_own_key(key_id: int, user: User, db: Session) -> ApiKey:
    """取一把**自己有权处置**的 Key：本人的；管理员则可处置所有人的（含无主的）。

    管理员保留全量处置权是有意的，和 generations.py 里「retry / supplement
    连管理员也不许代做」的不对称并不冲突：那两个动作会立刻花掉任务归属人的
    网关余额、发出去就收不回来；停用或删除一把 Key 不花任何人的钱，而且
    Key 一旦泄露或对接方跑路，必须有人能马上拔掉它，不能等属主自己上线。

    无主 Key（user_id 为 NULL，即回填补不上的历史 Key、或属主已被删除的 Key）
    同理只有管理员动得了，否则它们会变成谁也停不掉的僵尸。
    """
    k = db.get(ApiKey, key_id)
    if k is None:
        raise HTTPException(status_code=404, detail=_KEY_NOT_FOUND)
    if user.role != "admin" and k.user_id != user.id:
        raise HTTPException(status_code=404, detail=_KEY_NOT_FOUND)
    return k


@router.get("")
def list_keys(user: User = Depends(require_admin), db: Session = Depends(get_db)):
    q = db.query(ApiKey)
    if user.role != "admin":
        # 非管理员只看自己的。无主 Key 也不给看：处置权在管理员手里（见 _get_own_key），
        # 列出来却点不动只会让人以为界面坏了。
        q = q.filter(ApiKey.user_id == user.id)
    rows = q.order_by(ApiKey.id.desc()).all()
    owners = _owner_names(db, rows)
    return [apikey_to_dict(k, owner=owners.get(k.user_id, "")) for k in rows]


@router.post("")
def create_key(
    body: ApiKeyCreateBody, user: User = Depends(require_admin), db: Session = Depends(get_db)
):
    raw_key = new_api_key()
    secret = new_webhook_secret()
    record = ApiKey(
        # 谁建的就归谁。这一行是整条「按人算账」链路的起点：
        # v1.py 建任务时会把 key.user_id 写进 job.user_id，worker 再按它取网关 Key。
        user_id=user.id,
        name=body.name.strip(),
        key_prefix=raw_key[:11],
        key_hash=hash_api_key(raw_key),
        webhook_secret=secret,
        monthly_quota=body.monthly_quota,
    )
    db.add(record)
    db.commit()
    db.refresh(record)
    data = apikey_to_dict(record, owner=user.display_name or user.username)
    # 完整 Key 和回调密钥只在创建时返回一次，之后无法再查看
    data["api_key"] = raw_key
    data["webhook_secret"] = secret
    return data


@router.post("/{key_id}/toggle")
def toggle_key(key_id: int, user: User = Depends(require_admin), db: Session = Depends(get_db)):
    k = _get_own_key(key_id, user, db)
    k.is_active = not k.is_active
    db.commit()
    return apikey_to_dict(k, owner=_owner_names(db, [k]).get(k.user_id, ""))


@router.delete("/{key_id}")
def delete_key(key_id: int, user: User = Depends(require_admin), db: Session = Depends(get_db)):
    k = _get_own_key(key_id, user, db)
    db.delete(k)
    db.commit()
    return {"ok": True}
