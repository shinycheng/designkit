"""运行时设置：存数据库、界面可改，.env / 默认值作后备。

════════════════════════════════════════════════════════════════════════
 这个文件里的返回值**含明文密钥**，任何情况下都不许缓存。
════════════════════════════════════════════════════════════════════════
get_all() 和 resolve_for_user() 返回的字典里带着 openai_api_key（明文）。
现在 get_all 每次都去查库、每次都 `dict(RUNTIME_DEFAULTS)` 造一个新字典，
所以哪怕两个用户同时生图，各自拿到的也是各自的那份，不会串。

**禁止**给这两个函数加 @lru_cache、@cache、模块级 _cache 字典、
或者任何形式的「进程内存一份、下次直接返回」的优化。
道理是这样的：加了缓存之后，A 先生图，他的 Key 被记进缓存；B 接着生图，
拿到的是 A 的那份缓存 —— B 的图生出来了，钱从 A 的账户扣。
而这个 bug 在只有一个用户的测试环境里**永远复现不出来**，
在生产上则表现为「某个成员的余额莫名其妙掉得飞快」，谁也不会怀疑到缓存头上。

真到了查库成为瓶颈那天（几百 QPS 才谈得上），也只能缓存**不含密钥的那部分**，
密钥必须每次现查。这条约束比性能重要得多。
"""
import logging
from typing import Any, Dict, Iterable, Optional

from sqlalchemy.orm import Session

from ..config import RUNTIME_DEFAULTS
from ..models import AppSetting, User
from . import user_gateway

logger = logging.getLogger("designkit.settings")

# 这些键涉及密钥，接口返回时要打码。
# inspiration_proxy 也在里面，是因为带认证的代理写法就是
# http://用户名:密码@host:port —— 在此之前它是明文出现在设置接口响应里的。
SECRET_KEYS = {"openai_api_key", "inspiration_proxy"}


class GatewayNotReady(Exception):
    """这个任务的归属用户没有可用的网关 Key，不能生图。

    这是一条**故意让任务失败**的异常。它存在的唯一目的，就是堵住
    「找不到用户的 Key 就用全局那把顶上」这条看起来很贴心的退路：
    一旦回落，生图会照常成功、成员余额一分不扣、钱全从管理员账户走，
    而且日志里没有任何痕迹能看出这次是谁花的。
    宁可让这个人看见「你的账号还没开通额度」，也不能让账单变成一笔糊涂账。

    异常消息是直接给非技术用户看的，要写成人话、并给出下一步该找谁。
    """


def get_all(db: Session) -> Dict[str, Any]:
    """读全部运行时设置。**返回值含明文密钥，不许缓存**（原因见文件头）。"""
    values = dict(RUNTIME_DEFAULTS)
    for row in db.query(AppSetting).all():
        if row.key in values and isinstance(row.value, dict) and "v" in row.value:
            values[row.key] = row.value["v"]
    return values


def get(db: Session, key: str) -> Any:
    row = db.get(AppSetting, key)
    if row is not None and isinstance(row.value, dict) and "v" in row.value:
        return row.value["v"]
    return RUNTIME_DEFAULTS.get(key)


def set_many(db: Session, updates: Dict[str, Any]) -> None:
    for key, value in updates.items():
        if key not in RUNTIME_DEFAULTS:
            continue
        row = db.get(AppSetting, key)
        if row is None:
            db.add(AppSetting(key=key, value={"v": value}))
        else:
            row.value = {"v": value}
    db.commit()


def resolve_for_user(db: Session, user_id: Optional[int]) -> Dict[str, Any]:
    """按任务归属决定这次生图用哪一把网关 Key。

    **返回值含明文密钥，不许缓存、不许回吐给前端**（原因见文件头）。

    返回的是一份完整的设置字典，可以直接当作 provider.generate_images /
    prompt_studio.* 的第一个参数——它们读 Key 的地方只有 provider._headers 一处，
    读的就是这个字典里的 openai_api_key。所以「谁的 Key」这件事，
    全系统只在这一个函数里决定。

    下面五条规则的**顺序不能改**，每一条都是踩过或算过的：

    1. provider == "mock"：直接放行。mock 模式根本走不到 _headers（provider.py
       在 provider=="mock" 时直接返回模拟图），而 config.py 里 provider 的默认值
       就是 "mock"。少了这条豁免，全新部署下任何成员点生成都会被拦成
       「你的账号还没有开通生图额度」——README 和 PRODUCT 里「先用 mock 免费
       跑通全流程」这条上手路径直接废掉。而管理员自己试是不会有事的（走规则 4
       回落全局 Key），所以这个坑管理员永远复现不出来。
    2. gateway_mode != "per_user"：原样返回全局设置。默认值是 "shared"，
       也就是升级当天的行为与今天**逐字一致**，不需要任何人先去配什么。
    3. 这个用户有 state=="active" 的网关账号、且 Key 能解开 → 用他自己的。
    4. user_id 为 None（ERP 无主任务），或这个人是管理员但没配个人 Key
       → 用全局 Key。全局 Key 本来就是管理员配的、也是他在付钱；
       ERP 那条线没有归属人，只能靠它兜底，否则对接方会集体失败。
    5. 其余一律抛 GatewayNotReady，**绝不回落全局 Key**。
       （成员没配 Key、状态不是 active、密文解不开、user_id 指向的用户不存在）
    """
    values = get_all(db)

    # —— 规则 1：mock 模式不需要任何 Key ——
    if values.get("provider") == "mock":
        return values

    # —— 规则 2：没开「每人一把 Key」就什么都不用管 ——
    if values.get("gateway_mode") != "per_user":
        return values

    # —— 规则 3：用这个人自己的 Key ——
    account = user_gateway.get_account(db, user_id)
    if account is not None and account.state == "active":
        personal_key = user_gateway.decrypt_api_key(account)
        if personal_key:
            values["openai_api_key"] = personal_key
            return values

    # —— 规则 4：只有这两种情况能用全局 Key ——
    if user_id is None:
        # ERP 建的无主任务、以及归属人已被删除的历史任务（jobs.user_id 是 SET NULL）
        return values
    user = db.get(User, user_id)
    if user is not None and user.role == "admin":
        # 管理员本来就是全局 Key 的主人。他若配了个人 Key，规则 3 已经用上了。
        return values

    # —— 规则 5：硬线，到这里只能失败 ——
    # 这里绝对不能写成「找不到就 return values」。provider.py 的 _headers 只检查
    # Key 是不是空串，非空它不管这是谁的——一旦回落，生图照常成功、成员余额
    # 一分不扣、钱全从管理员账户走，账单上还分不出是谁花的，事后无从追查。
    if user is None:
        # user_id 有值却查不到人：数据不一致（正常删用户会把 job.user_id 置空）。
        # 既然连是不是管理员都确认不了，就不能替他花钱。
        logger.warning("任务归属用户 %s 不存在，拒绝回落到全局网关 Key", user_id)
    else:
        logger.warning(
            "用户 %s（%s）没有可用的网关 Key（账号状态：%s），拒绝回落到全局 Key",
            user_id, user.username, account.state if account is not None else "未开通",
        )
    raise GatewayNotReady("你的账号还没有开通生图额度，请联系管理员在「成员账号」页配置")


def _mask_secret(key: str, raw: str, keep_tail: bool) -> str:
    """把一个密钥值打码。keep_tail=True 时保留末 4 位（方便本人核对自己填的是哪把）。

    inspiration_proxy 单独处理：它的敏感部分只有 http://用户名:密码@ 这一段，
    地址和端口本身不是秘密。如果整串都打成星号，管理员就看不出自己配的是哪个代理
    （更要命的是前端「运行参数」那一节会拿打码后的值去校验「必须以 http:// 开头」，
    校验不过就连并发数这些字段都保存不了）。所以这里只糊掉认证信息、保留 http://host:port。
    """
    if not raw:
        return ""
    if key == "inspiration_proxy":
        return _mask_proxy(raw)
    if keep_tail and len(raw) > 4:
        return "*" * 8 + raw[-4:]
    return "*" * 8


def _mask_proxy(raw: str) -> str:
    """http://user:pass@127.0.0.1:7890 → http://***:***@127.0.0.1:7890；没有认证信息就原样返回。"""
    head, sep, rest = raw.partition("://")
    if not sep:
        return raw
    userinfo, at, hostpart = rest.rpartition("@")
    if not at or not userinfo:
        return raw  # 没有用户名密码，整串都不敏感
    return "%s://***:***@%s" % (head, hostpart)


def masked(values: Dict[str, Any]) -> Dict[str, Any]:
    """给管理员看自己配的那份设置：密钥打码，但保留末 4 位方便核对。

    保留末 4 位在「一个管理员看自己的 Key」这个场景下是纯粹的便利功能。
    但**不要**拿它去展示别人的 Key —— 那种场景用下面的 masked_full。
    """
    out = dict(values)
    for key in SECRET_KEYS:
        out[key] = _mask_secret(key, str(out.get(key) or ""), keep_tail=True)
    return out


def masked_full(values: Dict[str, Any], keys: Optional[Iterable[str]] = None) -> Dict[str, Any]:
    """全星号打码，**不保留末 4 位**。管理员查看成员账号时一律用这个。

    为什么末 4 位在这里就不能给了：单个管理员看自己的 Key 时，后 4 位只是
    「确认没填错」；可一旦管理员能翻看**所有成员**的账户，这 4 位就成了同一把 Key
    在 designkit、网关后台、上游账单三处之间的关联凭据——足以把「谁」「哪把 Key」
    「花了多少钱」串成一条线。展示需求（确认这个人配了没）用「已配置 / 未配置」
    就够了，不需要泄露 Key 的任何一部分。

    keys 不传时按 SECRET_KEYS 处理；也可以显式传一组键名
    （比如成员账号接口里那些不叫 openai_api_key 的字段）。
    """
    out = dict(values)
    for key in (SECRET_KEYS if keys is None else keys):
        if key not in out:
            continue
        out[key] = "*" * 8 if str(out.get(key) or "") else ""
    return out
