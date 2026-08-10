"""每个用户在生图网关上的账号与专属 Key 的读写（表见 models.UserGatewayAccount）。

这个模块只负责「存和取」，**不决定某个任务该用谁的 Key**——那条规则在
services/settings_service.py 的 resolve_for_user 里，只有一处，方便一眼看全。

安全约定（照抄自 models.py，因为这里是唯一会写这两列的地方）：
- api_key_enc 列里**永远是密文**，加解密统一走 services/secrets_box.py。
  任何时候都不要为了「方便排查」把明文写进去：那一刻起，一次数据库导出、
  一份发给别人排错的备份，就等于把所有成员的网关 Key 都送出去了。
- api_key_tail 只留末尾 4 位，**仅供后端日志排错**（对上「这次用的是哪一把」）。
  任何接口都不要把它返回给前端：designkit、网关后台、上游账单三个地方
  各有一份「后 4 位」，凑在一起就能把人、Key、花的钱三边对上号。

明文 Key 只在两个瞬间存在：用户在网页上填进来的那一次（upsert_key 的入参），
以及 worker 取出来发给网关的那一次（decrypt_api_key 的返回值）。
除此之外不要把它放进任何变量、日志、异常消息或接口返回值。
"""
import logging
from typing import Optional

from sqlalchemy.orm import Session

from ..models import GATEWAY_ACCOUNT_STATES, UserGatewayAccount
from . import secrets_box

logger = logging.getLogger("designkit.user_gateway")

# 末尾保留几位用于排错。4 位是权衡：再少了两把 Key 容易撞、对不上号；
# 再多了就接近「能凭它认出这把 Key」了。
_TAIL_LEN = 4


def get_account(db: Session, user_id: Optional[int]) -> Optional[UserGatewayAccount]:
    """取某个用户的网关账号行，没有就返回 None。

    user_id 允许传 None（ERP 无主任务会这样），直接返回 None，
    免得每个调用方都先写一遍 if user_id is None。
    """
    if user_id is None:
        return None
    return (
        db.query(UserGatewayAccount)
        .filter(UserGatewayAccount.user_id == user_id)
        .one_or_none()
    )


def decrypt_api_key(account: Optional[UserGatewayAccount]) -> str:
    """把账号行里的密文解成明文 Key；没配或解不开都返回空串，不抛异常。

    为什么这里吞掉异常：调用方（resolve_for_user）拿到空串后会走
    「这个用户没有可用的 Key」这条路——也就是**报错拦住**，而不是回落到全局 Key。
    所以吞异常不会导致任何人偷偷花别人的钱，只会让错误信息更统一。

    解不开最常见的原因不是被攻击，而是 data/.enc_key 换了一把
    （只恢复了数据库、没恢复钥匙文件）。这里按 WARNING 记一条，
    好让管理员在日志里看见「是解密失败」而不是以为自己没填 Key。
    异常消息本身不含密文，可以安全入日志（见 secrets_box.SecretsBoxError）。
    """
    if account is None or not account.api_key_enc:
        return ""
    try:
        return secrets_box.decrypt(account.api_key_enc)
    except secrets_box.SecretsBoxError as exc:
        logger.warning(
            "用户 %s 的网关 Key 解密失败（末 4 位 %s）：%s",
            account.user_id, account.api_key_tail or "?", exc,
        )
        return ""


def upsert_key(
    db: Session,
    user_id: int,
    plaintext_key: str,
    state: str = "active",
    provider: str = "sub2api",
) -> UserGatewayAccount:
    """给某个用户写入（或更新）网关 Key，自动加密并记下末 4 位。

    state 默认 "active" 是刻意的：能把明文 Key 填进来，说明这把 Key 已经拿到手了，
    立刻就该能用。若默认成 "manual"，管理员在「成员账号」页认认真真填完 Key、
    页面也提示保存成功，可该成员一点生成还是被拦——因为 resolve_for_user 只认
    state == "active"。这种「配了但不生效、界面上看不出原因」的坑必须堵死在这里。
    代客自动开通那条流程如果想先落库再自检，显式传 state="key_issued" 即可。

    这里**不 commit**，由调用方（接口层）统一提交：一次请求里往往还要同时改
    用户表或写审计，分开提交会出现「Key 写进去了、状态没跟上」的半截数据。
    """
    key = (plaintext_key or "").strip()
    if not key:
        # 空 Key 加密后是一段合法密文，存进去就变成「看着配了、其实是空」，
        # 而 provider.py 只在 Key 为空串时才报错——排查起来极其绕。要清空请用 clear_key。
        raise ValueError("网关 Key 不能为空；要清空请用 clear_key")
    if state not in GATEWAY_ACCOUNT_STATES:
        # 状态写错了不会立刻报错，只会让这个人永远生不了图（resolve 只认 active），
        # 所以在写库前就拦住。
        raise ValueError("未知的网关账号状态：%s" % state)

    account = get_account(db, user_id)
    if account is None:
        account = UserGatewayAccount(user_id=user_id, provider=provider)
        db.add(account)
    account.api_key_enc = secrets_box.encrypt(key)
    account.api_key_tail = key[-_TAIL_LEN:]
    account.state = state
    # 上一次开通失败的原因留着会一直显示在界面上，让人以为现在还是坏的
    account.last_error = ""
    db.flush()  # 让调用方在 commit 前就能拿到 account.id
    logger.info("已更新用户 %s 的网关 Key（末 4 位 %s，状态 %s）", user_id, account.api_key_tail, state)
    return account


def clear_key(db: Session, user_id: int) -> Optional[UserGatewayAccount]:
    """清掉某个用户的网关 Key，账号行本身保留。没有这一行则返回 None。

    为什么不删整行：remote_user_id / remote_email 这些是代客在网关上开的账号，
    删了本地这行，网关那边的账号就变成没人认领的孤儿——下次再给同一个人开通，
    要么重复建号、要么撞上「邮箱已存在」而失败，两种都很难查。

    状态改成 disabled 而不是留着 active：active 但没有 Key 是个自相矛盾的状态，
    界面上会显示「正常」，用户点生成却被拦住。清了 Key 就等于停用，
    界面照实显示；之后 upsert_key 会把它改回 active。

    同样不 commit，由调用方统一提交。
    """
    account = get_account(db, user_id)
    if account is None:
        return None
    account.api_key_enc = None
    account.api_key_tail = ""
    account.state = "disabled"
    db.flush()
    logger.info("已清除用户 %s 的网关 Key", user_id)
    return account
