import ipaddress
import re
import threading
import time
from typing import Any, Dict, List, Optional, Tuple
from urllib.parse import urlparse

from fastapi import APIRouter, Depends, HTTPException
from sqlalchemy.orm import Session

from ..backfill import get_backfill_health
from ..config import SECRET_SETTING_KEYS, TOKEN_EXPIRE_DAYS
from ..deps import get_db, require_admin
from ..models import User, UserGatewayAccount, utcnow
from ..services import (
    file_access, inspiration, jobs, prompt_studio, provider, provisioning,
    settings_service, sizing, sub2api, user_gateway,
)

router = APIRouter(prefix="/api/web/settings", tags=["网页-系统设置"])

# 把本次新增的密钥设置项并进 settings_service.SECRET_KEYS。
#
# 为什么要有这一句：SECRET_KEYS 决定「哪些设置项在接口返回前要打码」。
# 少了它，管理员 Key 会明文出现在 GET /api/web/settings 的响应里——
# 浏览器开发者工具、任何一份前端日志、一次截图求助，都足够把它送出去。
# 放在模块顶部而不是 config.py，是因为 config 不能反过来 import settings_service
# （settings_service 已经 import 了 config，会绕成循环导入）。
settings_service.SECRET_KEYS.update(SECRET_SETTING_KEYS)


def _masked_settings(db: Session) -> Dict[str, Any]:
    """读全部设置并打码。**GET 和 PUT 必须都用这一个函数**。

    两步打码是有意的：
    - masked()：openai_api_key 这类「管理员看自己填的那把」保留末 4 位，
      方便核对有没有填错（这是本来就有的行为，不改）。
    - masked_full()：SECRET_SETTING_KEYS 里的项（Sub2API 管理员 Key）连末 4 位
      都不给，整串星号。它是**整台 Sub2API 的最高权限**，能建号、能翻所有用户，
      泄露的后果比一把生图 Key 大得多，界面上只需要表达「填了没有」。

    两处调用必须用同一个函数，否则会出现这种诡异现象：GET 回去的掩码是 A、
    PUT 里拿来比对「有没有改过」的掩码是 B，前端原样回传 A 却被当成新值存进去，
    结果管理员 Key 被一串星号覆盖掉——而页面还提示保存成功。
    """
    payload = settings_service.masked(settings_service.get_all(db))
    return settings_service.masked_full(payload, keys=SECRET_SETTING_KEYS)


def _settings_payload(db: Session) -> Dict[str, Any]:
    """设置接口的统一返回体：可改的设置项（密钥已打码）+ 几项只读的状态。

    GET 和 PUT 返回同一个形状，是因为前端保存成功后会拿 PUT 的返回值整个替换掉
    本地那份设置（settings.js）。两边形状不一样的话，管理员一保存，
    页面上的告警横幅和统计数字就会莫名其妙消失，重新打开页面又出现。

    只读部分（都不在 RUNTIME_DEFAULTS 里，所以就算前端原样回传，
    settings_service.set_many 也会自动忽略，不会被写进设置表）：

      backfill_ok / backfill_error
          历史数据归属回填的健康状态。False 时管理员界面要显示红色横幅，
          文案直接用 backfill_error（已经是中文人话、自带处置建议）。
          **注意 settings_service.get_all() 拿不到这两个键**——它只复制
          RUNTIME_DEFAULTS 里有的键，所以必须像这里一样显式去 backfill 模块取。

      files_unsigned_access
          {"total","blocked","last_at","last_path"}：本进程启动至今，
          有多少次取图请求没带任何凭证（total）、其中被拦下多少次（blocked）。
          这是判断「敢不敢把 files_signed_only 关掉 / 关掉之后还有没有人在用
          老链接」的唯一客观依据，别靠翻日志猜。进程重启归零是有意的，
          它本来就只是个观察窗口。last_at 是 Unix 秒（0 表示从没发生过），
          怎么显示交给前端。
    """
    payload = _masked_settings(db)
    payload.update(get_backfill_health(db))
    payload["files_unsigned_access"] = file_access.unsigned_access_stats()
    return payload


# ══════════════════════════════════════════════════════════════════
#  「网关自动开通」几个设置项的校验
# ══════════════════════════════════════════════════════════════════
#
# 这几项为什么要校验得比别的设置项都严：填错了**不会当场报错**。
# 地址少个端口、分组 id 填成了分组名字、邮箱域名用了保留后缀——保存都会成功，
# 界面一片正常，然后每一个新注册的用户都在后台静静地开通失败，
# 管理员要等到有人说「我生不了图」才会发现。所以能在保存这一刻拦住的，
# 一律在这一刻拦住，并且报错要直接写出「该填成什么样」。

# 邮箱域名：只允许 [a-z0-9.-]、至少一个点、每段不以 - 开头结尾。
# 总长限 100 是因为 models.UserGatewayAccount.remote_email 是 String(128)，
# 而完整邮箱是 dk<用户编号>@<域名>；域名太长会在插入时被数据库截断或报错，
# 那时错误信息完全指不到这里。
_EMAIL_DOMAIN_RE = re.compile(
    r"^(?=.{1,100}$)[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$"
)

# 保留后缀。.invalid 是硬伤：Sub2API 自己就占了几个 *-connect.invalid 后缀做内部用途，
# 而且这份名单还会增长，撞上了会被建号策略直接拒掉（400），
# 报错里只说「邮箱不被允许」，完全看不出是域名后缀的问题。
# 另外三个是 RFC 2606 保留给文档和测试的，同样不该用来收发真实邮件。
# 默认值 designkit.local 的 .local 不在此列——它是 mDNS 用的私网域名，可以用。
_RESERVED_EMAIL_DOMAIN_SUFFIXES = (".invalid", ".localhost", ".example", ".test")

# 分组 id 只认半角数字。**不能用 str.isdigit()**：它对全角「１２３」和上标「²」
# 也返回 True，那种值存进去之后拼进 URL 会变成一个查不到的分组，
# 而界面上看起来就是几个正常的数字。
_GROUP_ID_RE = re.compile(r"^[0-9]{1,18}$")


def _fail(detail: str) -> "HTTPException":
    return HTTPException(status_code=422, detail=detail)


def _normalize_sub2api_base_url(raw: Any) -> str:
    """校验并规整 Sub2API 地址。留空是允许的（等于「还没配」）。"""
    url = str(raw or "").strip().rstrip("/")
    if not url:
        return ""
    if not url.startswith(("http://", "https://")):
        raise _fail(
            "Sub2API 地址要以 http:// 或 https:// 开头，例如 http://192.168.31.235:3000"
        )
    # 「一定发不出去」的那几种（中文/全角符号、空格、端口不是数字、带用户名密码、
    # 长得离谱、中括号没配对）统一交给 sub2api.base_url_problem 判断——
    # 客户端那边发请求前也用同一个函数再查一遍，两处标准必须是同一份，
    # 否则会出现「设置页存得进去、一点自检就炸」这种最难自查的状态。
    # 尤其是端口：以前这里从没取过 parsed.port，所以 http://x:abc 能存进去，
    # 然后在 httpx 那层抛一个 InvalidURL，界面上只剩一句看不懂的话。
    problem = sub2api.base_url_problem(url)
    if problem:
        raise _fail(problem)
    parsed = urlparse(url)
    if parsed.query or parsed.fragment:
        raise _fail("Sub2API 地址不要带 ? 或 # 后面的内容，填到端口为止就行")
    # 这一条是最容易踩的：管理员常把接口文档里的 http://host:3000/api/v1 整段贴过来。
    # 系统自己会往后面拼 /api/v1/admin/users，贴了就变成 /api/v1/api/v1/admin/users，
    # 表现是所有请求 404，而地址栏里看着完全正常。
    path = (parsed.path or "").rstrip("/")
    if path.endswith("/api") or "/api/v1" in path:
        raise _fail(
            "Sub2API 地址只填到端口为止，不要带 /api 或 /api/v1（接口路径由系统自己拼）。"
            "例如填 http://192.168.31.235:3000"
        )
    return url


def _normalize_sub2api_admin_key(raw: Any) -> str:
    """校验并规整管理员 Key。留空是允许的（等于「还没配」）。

    为什么这一项必须在**保存这一刻**就拦住，而不是等点自检时再说：
    这把 Key 是原样拼进 HTTP 请求头 `x-api-key` 发出去的，而请求头**只能放
    ASCII**。里面只要有一个中文字符，httpx 就会在编码那一刻抛 UnicodeEncodeError，
    那是个既不属于我们错误分类体系、也没法翻译成人话的异常——
    它曾经在界面上原样变成「自检过程中出现意外错误（UnicodeEncodeError）。
    请确认 Sub2API 地址填得对不对」，把人往完全错误的方向指（真因在 Key 不在地址）。

    实际踩到的形态是：管理员把输入框里那句灰色提示文字「粘贴网关后台生成的
    管理员 Key」当成内容粘了进来。所以提示语里必须点名这个可能——
    只说「格式不对」的话，人会盯着那串看起来「明明有字」的内容百思不解。

    判断标准和 sub2api.admin_key_problem 是同一份（就是直接调它）：
    同一件事只有一处标准，才不会出现「设置页存得进去、一用就报错」。
    """
    key = str(raw or "").strip()
    if not key:
        return ""
    problem = sub2api.admin_key_problem(key)
    if problem:
        raise _fail(problem)
    return key


def _normalize_sub2api_group_id(raw: Any) -> str:
    """校验目标分组 id。留空是允许的（等于「还没配」）。"""
    value = str(raw or "").strip()
    if not value:
        return ""
    if not _GROUP_ID_RE.match(value):
        raise _fail(
            "目标分组 id 只能填数字（Sub2API 后台分组列表里那个编号），"
            "不要填分组的名字，也不要带空格"
        )
    if int(value) <= 0:
        raise _fail("目标分组 id 要填一个大于 0 的数字，0 不是有效的分组编号")
    return value


def _normalize_sub2api_email_domain(raw: Any) -> str:
    """校验合成邮箱的域名。这一项**不允许留空**（留空会拼出 dk7@ 这种废邮箱）。"""
    domain = str(raw or "").strip().lower().rstrip(".")
    if not domain:
        raise _fail("邮箱域名不能留空，不确定就填默认的 designkit.local")
    if "@" in domain or "+" in domain:
        raise _fail(
            "这里只填域名那一半，不要带 @ 和 +。例如填 designkit.local，"
            "系统会自动拼成 dk7@designkit.local 这样的地址"
        )
    if not _EMAIL_DOMAIN_RE.match(domain):
        raise _fail(
            "邮箱域名只能用小写字母、数字、减号和点，中间至少有一个点，且不超过 100 个字符。"
            "不确定就填默认的 designkit.local"
        )
    for suffix in _RESERVED_EMAIL_DOMAIN_SUFFIXES:
        if domain.endswith(suffix):
            raise _fail(
                "%s 是保留域名后缀，用它建出来的账号会被网关拒掉，而且报错里看不出根因。"
                "请换一个，不确定就填默认的 designkit.local" % suffix
            )
    return domain


def _apply_sub2api_updates(current: Dict[str, Any], updates: Dict[str, Any]) -> None:
    """就地规整 updates 里「网关自动开通」那几项，并做一次跨字段检查。

    current 是**改之前**库里的值（含明文管理员 Key，只用来判断「填了没有」，
    绝不能打日志、更不能回吐给前端）。
    """
    if "sub2api_base_url" in updates:
        updates["sub2api_base_url"] = _normalize_sub2api_base_url(updates["sub2api_base_url"])
    if "sub2api_group_id" in updates:
        updates["sub2api_group_id"] = _normalize_sub2api_group_id(updates["sub2api_group_id"])
    if "sub2api_email_domain" in updates:
        updates["sub2api_email_domain"] = _normalize_sub2api_email_domain(
            updates["sub2api_email_domain"]
        )
    if "sub2api_admin_key" in updates:
        # 前端把掩码原样回传的情况在上面已经 pop 掉了，走到这里的都是新值。
        # 去两头空白是必须的：从网页上复制 Key 常带一个换行，带着它会让每一次请求都
        # 401，而界面上两串字符看起来一模一样。
        updates["sub2api_admin_key"] = _normalize_sub2api_admin_key(updates["sub2api_admin_key"])

    # —— 跨字段：没配齐就不许打开总开关 ——
    # 打开了也只会让每个新用户走一遍必然失败的流程，last_error 里堆一片红字，
    # 而管理员多半会以为是网关坏了。宁可在这里挡住，把话说清楚。
    if not updates.get("sub2api_auto_provision"):
        return
    merged = dict(current)
    merged.update(updates)
    missing = [
        label
        for key, label in (
            ("sub2api_base_url", "Sub2API 地址"),
            ("sub2api_admin_key", "管理员 Key"),
            ("sub2api_group_id", "目标分组 id"),
        )
        if not str(merged.get(key) or "").strip()
    ]
    if missing:
        raise _fail(
            "打开「自动开通」之前，请先填好：%s。填完保存一次，"
            "再点「立即自检」确认能连上，最后再打开这个开关" % "、".join(missing)
        )


# ══════════════════════════════════════════════════════════════════
#  开放自助注册的**硬前置闸门**
# ══════════════════════════════════════════════════════════════════
#
# 打开「自助注册」等于把这个系统的大门对陌生人敞开：谁都能自己建号、登录、
# 建 ERP Key、驱动服务端去访问它指定的地址。所以打开它之前有几件事必须先做完。
#
# **这些前提写在代码里，不写在文档里。** 项目里已经有过教训：文档说了、
# 用户照着做的时候漏了一条，出事之后谁也分不清是文档错了还是自己做错了。
# 只要有一条不满足，这个开关在接口层就**打不开**——不是提示、不是警告，是 422。
#
# 闸门是**双向**的：注册开着的时候，把下面任何一条改回不安全的值同样会被拒。
# 只做「打开时检查一次」等于没做——管理员可以先打开注册、再把内网访问改回来，
# 而那正是最危险的组合，界面上还什么都看不出来。
#
# 三条前提，以及各自「不满足会发生什么」：
#
#   ① allow_internal_targets 必须是 False。
#      开着它，v1 接口的 image_urls / callback_url 可以让**服务端**去访问内网的
#      任意 http 地址——包括同一内网里那台 Sub2API 的管理端口，上面挂着能建号、
#      能改余额、能读任意用户明文 API Key 的接口。而自助注册意味着陌生人能自己
#      拿到 ERP Key。两者叠在一起，就是把内网的门连同钥匙一起发出去。
#
#   ② public_base_url（对外访问地址）必须填成别人真能打开的地址。
#      新注册的用户拿到的图片链接、给 ERP 的签名直链、webhook 回调地址全基于它拼。
#      留着默认的 http://127.0.0.1:8787 的话，每个新用户拿到的链接都指向他自己的
#      电脑，全部打不开，而系统这边一条报错都不会有。
#      公网域名还必须是 https：注册页要提交密码，http 上是明文过网。
#
#   ③ files_signed_only 必须是 True（图片必须带凭证才能看）。
#      关掉它，知道地址就能下载任何人的商品图和出图结果。内部几个人用还能靠
#      「地址没人知道」凑合，一旦谁都能注册，「谁都能注册」也就意味着谁都能进来
#      翻别人的图。
#
# ⚠ 这三项的**默认值一个都没改**（allow_internal_targets 仍是 True）：改默认会
#   静默打断现有的 ERP 内网取图。要开放注册的人自己去把它关掉，这是有意的顺序。

# 一填就等于「谁也访问不到」的主机名。ip6-localhost 是 Debian 系 /etc/hosts 里的写法。
_LOOPBACK_HOSTNAMES = frozenset({"localhost", "localhost.localdomain", "ip6-localhost"})

# 只在局域网里解析得出来的域名后缀（mDNS、路由器自建域、RFC 8375）。
_LAN_DOMAIN_SUFFIXES = (".local", ".lan", ".internal", ".intranet", ".home.arpa", ".localdomain")

# 触发前置检查的几项。只要这次保存动到了其中任何一项，就重算一遍闸门。
#
# 为什么不是「每次保存都查」：万一库里已经处在一个不合法的组合里（老库遗留、
# 或者有人直接改了数据库），那样会把管理员锁死在设置页上——改任何一项都存不了，
# 包括「把注册关掉」这个唯一的出路。只在动到相关项时查，既堵死了所有会造成
# 危险组合的**改动**，又永远留着一条改回来的路。
_SELF_REGISTER_GATE_KEYS = (
    "self_register_enabled", "allow_internal_targets", "files_signed_only", "public_base_url",
)


def _host_is_lan_only(host: str) -> bool:
    """这个主机名/地址是不是「只在局域网里能访问」。

    判断不出来的（一个普通域名）一律当作**公网**——保守方向是把话说严：
    最坏结果是让管理员把内网地址也填成 https，而反过来（把公网域名误判成内网）
    会放过一个明文传密码的部署。
    """
    text = (host or "").strip().lower().strip(".")
    if not text:
        return False
    try:
        # is_private 覆盖 10/8、172.16/12、192.168/16、169.254/16、fc00::/7 等
        return bool(ipaddress.ip_address(text).is_private)
    except ValueError:
        pass
    if "." not in text:
        # 光一个主机名（nas、designkit）只可能在局域网里解析得出来
        return True
    return text.endswith(_LAN_DOMAIN_SUFFIXES)


def _public_base_url_problem(raw: Any) -> str:
    """「对外访问地址」够不够格用来开放注册。没问题返回空串。

    注意这比 update_settings 里那条基础校验（只看有没有 http:// 开头）严得多，
    而且**只在开放注册时才要求**——内部部署填个 http://192.168.x.x:8787 是完全
    正常的，不该因为加了这个功能就把现有部署判成配置错误。
    """
    url = str(raw or "").strip().rstrip("/")
    if not url:
        return (
            "现在是空的。请填成别人在浏览器里访问这个系统时用的那个完整地址，"
            "例如 https://designkit.example.com"
        )
    if not url.startswith(("http://", "https://")):
        return "要以 http:// 或 https:// 开头，例如 https://designkit.example.com"
    parsed = urlparse(url)
    host = (parsed.hostname or "").strip().lower()
    if not host:
        return "里面没看到域名或 IP，例如 https://designkit.example.com"
    if host in _LOOPBACK_HOSTNAMES:
        return (
            "现在填的是 %s，那是「服务器自己」的意思，别人打不开。"
            "请填成别人访问时用的那个地址" % host
        )
    try:
        address = ipaddress.ip_address(host)
    except ValueError:
        address = None
    if address is not None and (address.is_loopback or address.is_unspecified):
        return (
            "现在填的是 %s，那是「服务器自己」的意思，别人打不开。"
            "请填成别人访问时用的那个地址" % host
        )
    if parsed.scheme == "http" and not _host_is_lan_only(host):
        return (
            "现在填的是 http://（明文）。自助注册页要让陌生人在上面填密码，"
            "明文传输等于把密码贴在路上。请配好 https 证书之后，把这里改成 "
            "https://%s" % host
        )
    return ""


def _self_register_blockers(merged: Dict[str, Any]) -> List[str]:
    """按合并后的设置算一遍：还有哪几条前提没满足。每一条都写清「怎么改」。"""
    problems: List[str] = []
    if bool(merged.get("allow_internal_targets")):
        problems.append(
            "①「允许对外接口访问内网地址」还开着，请把它关掉。"
            "开着它，注册进来的陌生人可以让本系统替他去访问你内网里的任意地址"
            "（包括生图网关的管理端口，那上面能建号、改余额、读别人的 Key）。"
        )
    problem = _public_base_url_problem(merged.get("public_base_url"))
    if problem:
        problems.append(
            "②「对外访问地址」还不能用：%s。新用户拿到的图片链接和回调地址都按它拼，"
            "填错了他们会拿到一堆打不开的链接，而系统这边不会有任何报错。" % problem
        )
    if not bool(merged.get("files_signed_only")):
        problems.append(
            "③「图片必须带凭证才能访问」是关的，请把它打开。"
            "关着它，只要知道图片地址就能下载任何人的商品图和出图结果——"
            "内部几个人用还能凑合，谁都能注册之后就等于谁都能进来翻别人的图。"
        )
    return problems


def _apply_self_register_updates(current: Dict[str, Any], updates: Dict[str, Any]) -> None:
    """自助注册的硬前置闸门（双向）。不满足就 422，并说清是哪一条、怎么改。

    current 是**改之前**库里的值，updates 是这次要改的。两者合并之后再判断，
    所以「一次保存里同时打开注册并关掉内网访问」是允许的（也是推荐的做法），
    而「打开注册但内网还开着」和「注册开着却把内网改回来」都会被拦下。
    """
    if not any(key in updates for key in _SELF_REGISTER_GATE_KEYS):
        return
    merged = dict(current)
    merged.update(updates)
    if not bool(merged.get("self_register_enabled")):
        # 注册是关的（或者这次正要关掉）→ 上面那三条都不是前提，随便改。
        # 关掉注册这条路必须永远畅通，否则库里一旦处在危险组合里就没法收场。
        return
    problems = _self_register_blockers(merged)
    if not problems:
        return
    was_on = bool(current.get("self_register_enabled"))
    if not was_on:
        head = "还不能开放自助注册，请先把下面这几条改好（可以在这一页一起改完再保存）："
    else:
        head = (
            "这一步会让「自助注册」失去它的前提条件，所以没有保存。"
            "自助注册现在是开着的，要么先把它关掉，要么保持下面这几条："
        )
    raise _fail(head + "\n" + "\n".join(problems))


@router.get("")
def get_settings(_: User = Depends(require_admin), db: Session = Depends(get_db)):
    return _settings_payload(db)


@router.put("")
def update_settings(
    updates: Dict[str, Any], _: User = Depends(require_admin), db: Session = Depends(get_db)
):
    updates = dict(updates or {})
    # current 含**明文密钥**：只用来比对和判断「填了没有」，绝不打日志、绝不回吐。
    current = settings_service.get_all(db)
    # 前端把打码后的 key 原样传回来表示「没改」，这里忽略掉（与当前掩码精确比对 + 星号前缀兜底）
    masked_current = _masked_settings(db)
    for secret_key in settings_service.SECRET_KEYS:
        value = str(updates.get(secret_key) or "")
        if value and (value == masked_current.get(secret_key) or set(value[:8]) == {"*"}):
            updates.pop(secret_key, None)
    # 文本模型名留空会让「AI 写提示词」必然失败（每次都白等一轮再回退），提前拦住
    if "text_model" in updates and not str(updates["text_model"] or "").strip():
        raise HTTPException(status_code=422, detail="文本模型不能为空，例如 gpt-5.6-sol")
    # 布尔开关必须是真布尔：传字符串 "false" 会被 Python 当成真值，开关名存实亡
    for bool_key in ("prompt_synthesis", "normalize_input_ratio",
                     "allow_internal_targets", "inspiration_auto_sync",
                     "files_signed_only", "sub2api_auto_provision",
                     "sub2api_keep_password", "sub2api_verify_tls",
                     "gateway_shared_fallback",
                     # self_register_enabled 少了这一条会出大事：前端传字符串 "false"
                     # 时 Python 会当成真值存进去，注册就被**静默打开**了——
                     # 而下面那道前置闸门读的也是这个值，会以为管理员确实想开。
                     "self_register_enabled"):
        if bool_key in updates and not isinstance(updates[bool_key], bool):
            raise HTTPException(status_code=422, detail="%s 必须是 true 或 false" % bool_key)
    if "provider" in updates and updates["provider"] not in ("mock", "openai"):
        raise HTTPException(status_code=422, detail="provider 只能是 mock 或 openai")
    # 对外访问地址：这一项此前是全部设置里唯一没有校验的，而结果图链接、
    # webhook 回调地址、给 ERP 的签名链接全都基于它拼出来。填错（比如漏了 http://、
    # 或者手滑填成 "192.168.1.10:8787"）不会当场报错，只会让 ERP 那边拿到一堆
    # 打不开的地址，两边都查不出原因。留空是允许的——那时链接退化成相对路径，
    # 网页端照常能用（storage.to_url 的行为），只有对外接口会受影响。
    if "public_base_url" in updates:
        base = str(updates["public_base_url"] or "").strip().rstrip("/")
        if base and not base.startswith(("http://", "https://")):
            raise HTTPException(
                status_code=422,
                detail="对外访问地址要以 http:// 或 https:// 开头，例如 http://192.168.1.10:8787",
            )
        updates["public_base_url"] = base
    if "gateway_mode" in updates and updates["gateway_mode"] not in ("shared", "per_user"):
        raise HTTPException(
            status_code=422, detail="计费方式只能是 shared（共用一把 Key）或 per_user（每人一把）"
        )
    # CORS 白名单。写成 * 表示不限制；多个域名用英文逗号隔开。
    # 逐条要求带协议头，是因为浏览器比对 Origin 时是连协议一起比的：
    # 填 "a.example.com" 永远匹配不上 "https://a.example.com"，
    # 而现象是「跨域调用全被挡了」，管理员看着这行字是对的，根本想不到差在协议头。
    if "allowed_origins" in updates:
        raw = str(updates["allowed_origins"] or "").strip()
        items = [item.strip().rstrip("/") for item in raw.split(",") if item.strip()]
        if not items or "*" in items:
            updates["allowed_origins"] = "*"
        else:
            for item in items:
                host = item.split("://", 1)[1] if "://" in item else ""
                if not item.startswith(("http://", "https://")) or not host or "/" in host:
                    raise HTTPException(
                        status_code=422,
                        detail="允许的来源要写成完整站点地址、不带路径，"
                               "例如 https://designkit.example.com；多个用英文逗号隔开，"
                               "不限制就填 *（改完这一项需要重启服务才生效）",
                    )
            updates["allowed_origins"] = ",".join(items)
    if "inspiration_proxy" in updates:
        proxy = str(updates["inspiration_proxy"] or "").strip()
        if proxy and not proxy.startswith(("http://", "https://", "socks5://", "socks5h://")):
            raise HTTPException(
                status_code=422,
                detail="代理地址要以 http:// 或 socks5:// 开头，例如 http://127.0.0.1:7890",
            )
        updates["inspiration_proxy"] = proxy
    if "image_background" in updates and updates["image_background"] not in ("auto", "transparent"):
        raise HTTPException(status_code=422, detail="出图底色只能是 auto 或 transparent")
    if "default_size" in updates:
        updates["default_size"] = sizing.normalize_size(updates["default_size"])
        size_error = sizing.validate_size(updates["default_size"])
        if size_error:
            raise HTTPException(status_code=422, detail="默认尺寸不可用：%s" % size_error)
    # 上限不只是防呆：max_attempts 和 n 直接决定一次任务最多向生图接口发多少次
    # 请求，填错一个数字就可能变成成倍的费用
    int_ranges = {
        "worker_concurrency": (1, 8, "并发生成数"),
        "max_attempts": (1, 5, "失败重试次数"),
        "request_timeout": (30, 900, "生图超时（秒）"),
        "default_n": (1, 4, "默认生成张数"),
        "inspiration_sync_interval_hours": (1, 168, "灵感库自动同步间隔（小时）"),
        # 下限**必须**不小于登录令牌的有效期。取图凭证比登录短的话，
        # 会出现「人还登录着、页面照常能点，可图全裂了」——这种故障
        # 非技术用户完全无法归因，只会觉得整个系统坏了。
        "web_file_cookie_days": (TOKEN_EXPIRE_DAYS, 30, "网页取图凭证有效期（天）"),
        # 给 ERP 的图片直链有效期。上限 8760 小时 = 365 天：再长就等于永久有效，
        # 那这条链接一旦流出去就再也收不回来了（除非停用整把 Key）。
        "erp_file_link_ttl_hours": (1, 8760, "对外图片链接有效期（小时）"),
        # 上限 20 与 provisioning.load_config 的钳位一致；再多也没意义，
        # 退避阶梯到第 5 次已经拉到 12 小时，还失败就该人来看了。
        "sub2api_max_attempts": (1, 20, "自动开通最多重试次数"),
        # 下限 3 秒：比这更短会把「网关有点慢」误判成「网关坏了」，
        # 于是白白消耗一次重试次数。上限 120 秒：再长会让后台 worker
        # 卡在一个连不上的地址上，把排在后面的人一起拖住。
        "sub2api_timeout": (3, 120, "连接 Sub2API 的超时（秒）"),
        # ── 自助注册 ──
        # 同一个来源每小时最多尝试几次。下限 1 而不是 0：填 0 等于「一个人都注册不了」，
        # 那应该去把总开关关掉，而不是留着开关、在限速里悄悄堵死。
        # 上限 1000 只是防手滑（填成 100000 等于没限速）。
        "self_register_ip_per_hour": (1, 1000, "同一来源每小时注册尝试次数"),
        # 全站每天最多成功注册几个。这是最后一道兜底，每多一个账号就多一个
        # 要开通网关、要花钱的人头，所以宁可小。上限 10000 同样只是防手滑。
        "self_register_daily_limit": (1, 10000, "全站每天最多新增账号数"),
        # 新建邀请码时预填的默认值（创建时还能改，范围与 invites.py 里一致）。
        "invite_code_default_max_uses": (1, 1000, "邀请码默认可用次数"),
        # 下限是 0，**0 表示永不过期**（invites.py 就是这么解释这个值的）。
        # 这里若写成 1，界面上就再也填不出「永久码」，而接口那边却支持——
        # 两边对不上时用户只会觉得系统坏了。
        "invite_code_default_valid_days": (0, 365, "邀请码默认有效天数（0=永不过期）"),
    }
    for int_key, (low, high, label) in int_ranges.items():
        if int_key not in updates:
            continue
        try:
            value = int(updates[int_key])
        except (TypeError, ValueError):
            raise HTTPException(status_code=422, detail="%s 必须是数字" % label)
        if not low <= value <= high:
            raise HTTPException(
                status_code=422, detail="%s 只能填 %d 到 %d 之间" % (label, low, high)
            )
        updates[int_key] = value
    _apply_sub2api_updates(current, updates)
    # 自助注册的硬前置闸门。**必须排在 public_base_url 那段规整之后**：
    # 它要按规整过的值（去掉末尾斜杠）判断，否则同一个地址会因为多一个 "/"
    # 时而通过时而不过。也必须排在 set_many 之前——闸门的意义就是「存不进去」。
    _apply_self_register_updates(current, updates)
    settings_service.set_many(db, updates)
    # 地址/管理员 Key/分组 id 一改，上一次的自检结论立刻就作废了。
    # 不清掉的话，管理员改完配置马上点自检，看到的还是 60 秒前那份旧结果——
    # 可能是改之前的红灯（以为没修好），也可能是改之前的绿灯（以为已经好了）。
    _selfcheck_clear()
    return _settings_payload(db)


@router.post("/test_provider")
def test_provider(_: User = Depends(require_admin), db: Session = Depends(get_db)):
    """用当前已保存的设置测试生图接口连通性（不产生生图费用）。"""
    try:
        message = provider.test_connection(settings_service.get_all(db))
        return {"ok": True, "message": message}
    except provider.ProviderError as e:
        return {"ok": False, "message": str(e)}


@router.post("/test_text_model")
def test_text_model(_: User = Depends(require_admin), db: Session = Depends(get_db)):
    """测试写提示词用的文本模型（只发一句话，成本可忽略）。"""
    settings = settings_service.get_all(db)
    if settings.get("provider") == "mock":
        return {"ok": True, "message": "当前为模拟生图模式，无需文本模型"}
    try:
        return {"ok": True, "message": prompt_studio.test_text_model(settings)}
    except provider.ProviderError as e:
        return {"ok": False, "message": str(e)}


@router.post("/test_sync_proxy")
def test_sync_proxy(_: User = Depends(require_admin), db: Session = Depends(get_db)):
    """测试能不能拉到上游提示词库（走当前配置的代理）。

    只下载一个很小的 manifest.json，不写缓存、不入库，几秒就有结果——
    比让用户点「同步上游」等两分钟才发现代理不通要好得多。
    """
    proxy = str(settings_service.get(db, "inspiration_proxy") or "").strip()
    try:
        info = inspiration.probe_upstream(proxy)
    except Exception as exc:
        return {"ok": False, "message": str(exc)[:300]}
    return {"ok": True, "message": info}


# ══════════════════════════════════════════════════════════════════
#  网关自动开通：自检
# ══════════════════════════════════════════════════════════════════
#
# 这个自检要回答的问题只有一句：**「现在点下去，还能不能自动给新用户开通？」**
#
# 三条硬约束，改这段代码之前先读一遍：
#
# 1. **零副作用**。不建账号、不发 Key、不改余额、不代替任何人登录。
#    全部是 GET。原因不只是「怕搞坏」：代登录会消耗 Sub2API 那个
#    20 次/分钟/IP 的限流额度（designkit 是服务端代调，全站共用一个出口 IP）、
#    写一条登录审计、还会在对端 Redis 里堆一个 TTL 30 天的令牌家族。
#    一个「点着玩」的自检按钮不该有这些后果。所以设计文档里那个
#    「深度自检（真登录一次）」按钮**故意没有实现**——要做也得单独一个按钮、
#    单独限一天一次、并且在按钮旁写明「会在 Sub2API 留一条登录记录」。
#
# 2. **报实际值，不报默认值**。源码里的默认值不等于用户那台实例的现状。
#    所以凡是能读回来的（后台模式、人机验证、两步验证、合规版本、
#    远端账号的并发/限速/分组），一律把**读回来的那个值**摆给管理员看。
#
# 3. **查不了就写「未验证」，绝不画成绿灯**。库里一个开通成功的用户都没有时，
#    好几项是真的没法查。写成绿的话，管理员会以为查过了、没问题，
#    等真出事再回头看这个页面只会更糊涂。

_SELFCHECK_TTL_SECONDS = 60.0

# 自检结果缓存。存的是**已经脱敏过的响应体**，不含任何凭据。
# 缓存 60 秒是为了防「管理员刷页面 → 每次都往 Sub2API 打一串请求」。
# 进程内存，重启即失效，多进程各存各的——这都无所谓，它只是个防抖。
_selfcheck_lock = threading.Lock()
_selfcheck_cache: Dict[str, Any] = {}

# 探针 detail 里凡是名字沾上这些词的，一律不返回给前端。
#
# 眼下最要命的一条是 key_tail：sub2api.probe_gateway_key 会把抽查用的那把 Key
# 的末 4 位放进 detail。而 designkit、Sub2API 后台、上游账单三处各有一份「后 4 位」，
# 凑在一起就能把「谁」「哪把 Key」「花了多少钱」串成一条线——
# services/user_gateway.py 和 settings_service.masked_full 里都立了这条规矩。
# remote_email 同理：那是代客建的账号邮箱，界面上没有任何理由显示它。
#
# 用「名字里包含」而不是白名单，是因为这里过滤的是**别人模块产出的字典**：
# sub2api.py 哪天往 detail 里多塞一个字段，白名单会默认放行（静默泄露），
# 而这份 hints 只会误伤——误伤的表现是少显示一项，代价小得多。
_SENSITIVE_DETAIL_HINTS = ("key", "token", "password", "secret", "tail", "email", "mail")

_LEVEL_TEXT = {"green": "正常", "yellow": "注意", "red": "有问题", "unknown": "未验证"}

# 这几种红灯**不触发自动暂停**：它们是「现在连不上」，不是「前提条件没了」。
# 管理员在 NAS 重启的当口点一下自检，不该把总开关给关掉——
# 关掉之后没人会想起来再打开，表现就是「自动开通某天起就不干活了」。
#
# E_SELFCHECK_CRASH 也在里面：那是**自检自己的代码**出了意外，不是 Sub2API 的问题。
# 因为我们这边的一个 bug 就把全站自动开通关掉，属于误伤中最冤的一种。
_TRANSIENT_ERROR_CODES = frozenset({
    "E_NETWORK", "E_SERVER", "E_RATE_LIMITED", "E_LOGIN_BUSY", "E_IDEMPOTENCY_BUSY",
    "E_SELFCHECK_CRASH",
})


def _selfcheck_clear() -> None:
    with _selfcheck_lock:
        _selfcheck_cache.clear()


def _selfcheck_cached() -> Optional[Dict[str, Any]]:
    with _selfcheck_lock:
        at = _selfcheck_cache.get("at")
        payload = _selfcheck_cache.get("payload")
    if not at or not isinstance(payload, dict):
        return None
    if time.monotonic() - at > _SELFCHECK_TTL_SECONDS:
        return None
    out = dict(payload)
    out["cached"] = True
    return out


def _selfcheck_store(payload: Dict[str, Any]) -> None:
    with _selfcheck_lock:
        _selfcheck_cache["at"] = time.monotonic()
        _selfcheck_cache["payload"] = payload


def _safe_detail(detail: Any) -> Dict[str, Any]:
    """把探针的 detail 过一遍，只留下能安全显示给管理员的标量。"""
    out: Dict[str, Any] = {}
    if not isinstance(detail, dict):
        return out
    for name, value in detail.items():
        low = str(name).lower()
        if any(hint in low for hint in _SENSITIVE_DETAIL_HINTS):
            continue
        if value is None or isinstance(value, (bool, int, float)):
            out[str(name)] = value
        elif isinstance(value, str):
            out[str(name)] = value[:120]
        elif isinstance(value, (list, tuple)):
            out[str(name)] = [str(v)[:60] for v in list(value)[:20]]
    return out


def _item(name: str, level: str, summary: str,
          actual: Any = None, expected: str = "", detail: Any = None) -> Dict[str, Any]:
    """自检结果里的一行。前端直接照着显示，不要再包一层技术措辞。"""
    return {
        "name": name,
        "level": level,
        "level_text": _LEVEL_TEXT.get(level, level),
        "summary": summary,
        "actual": actual,          # 这台实例上**读回来的实际值**（没读到就是 None）
        "expected": expected,      # 期望是什么，给管理员对照
        "detail": _safe_detail(detail),
    }


def _combine_level(levels: List[str]) -> str:
    """三色总判定：任一红→红；无红有黄→黄；其余（含「未验证」）→绿。"""
    if "red" in levels:
        return "red"
    if "yellow" in levels:
        return "yellow"
    return "green"


def _pick_sample_account(db: Session) -> Tuple[Optional[UserGatewayAccount], str]:
    """借一个已经开通成功的用户做只读抽查，返回（账号行，明文 Key）。

    优先挑「自动开通出来」的那种（有 remote_user_id 和 remote_email）：
    只有它们能验证建号时传的 concurrency / allowed_groups 到底有没有被对端采纳。
    实在没有就退而求其次挑一个手工填了 Key 的，至少能验证「Key 打网关通不通」。
    一个都没有时返回 (None, "")，对应的几项照实标成「未验证」。

    明文 Key 只在本次请求的局部变量里活着，用完即弃，**不进任何返回值**。
    """
    rows = (
        db.query(UserGatewayAccount)
        .filter(UserGatewayAccount.state == "active")
        .order_by(UserGatewayAccount.updated_at.desc())
        .limit(20)
        .all()
    )
    fallback: Tuple[Optional[UserGatewayAccount], str] = (None, "")
    for row in rows:
        key = user_gateway.decrypt_api_key(row)
        if not key:
            continue  # 密文解不开（多半是只恢复了库、没恢复 data/.enc_key）
        if row.remote_user_id and row.remote_email:
            return row, key
        if fallback[0] is None:
            fallback = (row, key)
    return fallback


def _check_assumptions(
    client: "sub2api.Sub2ApiClient",
    cfg: "provisioning.ProvisioningConfig",
    account: Optional[UserGatewayAccount],
    probes: List[Any],
) -> List[Dict[str, Any]]:
    """验证设计文档里那六条「仍未查明」的假设，并把**实际值**报出来。

    全部只读。查不到的照实标「未验证」。
    """
    items: List[Dict[str, Any]] = []
    # 按内容认领每个探针的 detail，**不要 update 到一个字典里**：
    # 「公开设置」和「合规确认」两个探针的 detail 里都有一个叫 version 的字段，
    # 前者是 Sub2API 的版本号、后者是合规承诺的版本号，合并就会互相覆盖，
    # 界面上显示的还是一个像模像样的版本号，根本看不出串了。
    # 也不按下标取：探针顺序是别的模块决定的，哪天加一项这里就会静默错位。
    public_detail: Dict[str, Any] = {}
    compliance_detail: Dict[str, Any] = {}
    for probe in probes:
        detail = getattr(probe, "detail", None) or {}
        if "backend_mode_enabled" in detail:
            public_detail = detail
        elif "required" in detail:
            compliance_detail = detail
    probe_detail = public_detail

    # ── 假设 6：这台实例上那些开关的实际取值 ──
    # 源码默认值不等于实例现状，所以照实摆出来给管理员看。
    def _switch(value: Any) -> str:
        if value is None:
            return "读不到"
        return "开" if value else "关"

    version = probe_detail.get("version")
    items.append(_item(
        "这台 Sub2API 的实际设置",
        "green" if probe_detail else "unknown",
        # 这段是直接显示在页面上的，别写 Markdown 记号（面板不做渲染，会原样显示成星号）
        "以上是从这台实例上真实读回来的取值，不是源码里的默认值。"
        "后台模式或人机验证只要开着，系统就没法代用户登录，也就发不出 Key。"
        "另外这两个版本号请留意：Sub2API 升级之后自动开通可能整个失效，"
        "合规承诺版本一变则说明之前签的已经作废、要重新签一次"
        if probe_detail else "这次没读到实例的公开设置，下面几项无从对照",
        actual="后台模式：%s；Turnstile：%s；腾讯验证码：%s；阿里验证码：%s；两步验证：%s；"
               "Sub2API 版本：%s；合规承诺版本：%s" % (
            _switch(probe_detail.get("backend_mode_enabled")),
            _switch(probe_detail.get("turnstile_enabled")),
            _switch(probe_detail.get("tencent_captcha_enabled")),
            _switch(probe_detail.get("aliyun_captcha_enabled")),
            _switch(probe_detail.get("totp_enabled")),
            version if version else "读不到",
            compliance_detail.get("version") or "读不到",
        ),
        expected="后台模式关、三个人机验证都关",
    ))

    if account is None or not account.remote_user_id:
        # 一个自动开通成功的用户都还没有 —— 下面几项是真的查不了。
        # **必须写「未验证」**：写成绿的话，管理员会以为分组、并发都查过了。
        items.append(_item(
            "远端账号的实际参数（并发 / 限速 / 分组）", "unknown",
            "库里还没有「自动开通成功」的用户可供抽查。等第一个用户开通成功之后，"
            "回到这里再点一次自检，这几项就能查了",
            expected="并发大于 0、绑定了你填的那个分组",
        ))
        items.append(_item(
            "按邮箱反查用户", "unknown",
            "同上，还没有可抽查的账号。这一项验证的是「建号撞上『邮箱已存在』时，"
            "系统能不能准确找回原来那个账号」",
            expected="用完整邮箱能精确查回同一个人",
        ))
    else:
        remote = None
        fetch_error = None
        try:
            remote = client.admin_get_user(str(account.remote_user_id))
        except sub2api.Sub2ApiError as exc:
            fetch_error = exc

        if remote is None:
            level = "yellow"
            summary = "抽查一个已开通用户的远端资料失败：%s" % (
                fetch_error.message if fetch_error else "对端没有返回这个用户"
            )
            items.append(_item("远端账号的实际参数（并发 / 限速 / 分组）", level, summary,
                               expected="并发大于 0、绑定了你填的那个分组",
                               detail={"code": fetch_error.code if fetch_error else ""}))
        else:
            # ── 假设 1：concurrency 到底写进去没有 ──
            # admin 建号路径不传 concurrency 会**真的写字面 0**，而 0 在网关侧
            # 到底是「不限并发」还是「一个请求都发不出去」至今没查明。
            # 系统建号时显式传了 5，这里就是看对端有没有采纳。
            if remote.concurrency is None:
                items.append(_item(
                    "远端账号的并发上限", "unknown",
                    "这台 Sub2API 的接口没有回这个字段，没法核对",
                    expected="大于 0（系统建号时显式传的是 5）",
                ))
            elif remote.concurrency <= 0:
                items.append(_item(
                    "远端账号的并发上限", "yellow",
                    "抽查到的这个账号并发是 0。系统建号时明明传的是 5，说明这台实例"
                    "没有采纳这个值。0 在网关侧可能意味着「一张图都发不出去」，"
                    "请用这个账号在 Sub2API 后台确认能不能真的出一张图；"
                    "不能的话，把并发手工改成 5 再开通新用户",
                    actual=remote.concurrency, expected="大于 0（系统传的是 5）",
                ))
            else:
                items.append(_item(
                    "远端账号的并发上限", "green",
                    "系统建号时传的并发被正常写进去了",
                    actual=remote.concurrency, expected="大于 0（系统传的是 5）",
                ))

            items.append(_item(
                "远端账号的限速（rpm_limit）",
                "green" if remote.rpm_limit is not None else "unknown",
                "系统建号时传的是 0（表示不限每分钟请求数）。这里是对端的实际值"
                if remote.rpm_limit is not None
                else "这台 Sub2API 的接口没有回这个字段，没法核对",
                actual=remote.rpm_limit, expected="0（不额外限速）",
            ))

            # ── 假设 2 + 假设 4：分组到底绑上没有、填的 id 对不对 ──
            groups = [str(g) for g in (remote.allowed_groups or [])]
            shown = "、".join(groups) if groups else "（空）"
            if not groups:
                items.append(_item(
                    "远端账号绑定的分组", "yellow",
                    "抽查到的这个账号没有绑定任何分组。未绑分组的 Key 很可能一张图都发不出去"
                    "（网关会回「API Key is not assigned to any group」）。"
                    "请到 Sub2API 后台确认这个分组 id 填得对不对",
                    actual=shown, expected="包含你填的分组 id：%s" % (cfg.group_id or "（还没填）"),
                ))
            elif cfg.group_id and cfg.group_id not in groups:
                items.append(_item(
                    "远端账号绑定的分组", "yellow",
                    "设置里填的分组 id 和这个账号实际绑的对不上。多半是分组 id 填错了，"
                    "或者这个账号是更早以前用别的分组开出来的。"
                    "请到 Sub2API 后台核对一下分组编号",
                    actual=shown, expected="包含你填的分组 id：%s" % cfg.group_id,
                ))
            else:
                items.append(_item(
                    "远端账号绑定的分组", "green",
                    "抽查的账号绑着你填的那个分组，分组 id 填对了",
                    actual=shown, expected="包含你填的分组 id：%s" % cfg.group_id,
                ))

        # ── 假设 5：search 是模糊匹配，精确二次比对到底靠不靠谱 ──
        # 这一步验证的是「建号撞 409 之后能不能准确找回原来那个人」。
        # 找错人的后果是把 Key 建到别人账号上，而且一点报错都没有。
        if account.remote_email:
            try:
                found = client.admin_find_user_by_email(account.remote_email)
            except sub2api.Sub2ApiError as exc:
                items.append(_item(
                    "按邮箱反查用户", "yellow",
                    "反查失败：%s" % exc.message,
                    expected="用完整邮箱能精确查回同一个人",
                    detail={"code": exc.code},
                ))
            else:
                if found is None:
                    items.append(_item(
                        "按邮箱反查用户", "yellow",
                        "用完整邮箱查不到这个已开通的账号。它可能在 Sub2API 那边被删了；"
                        "这条路走不通的话，将来撞上「邮箱已存在」时就只能人工处理",
                        actual="没查到", expected="查回同一个人",
                    ))
                elif str(found.id) != str(account.remote_user_id):
                    items.append(_item(
                        "按邮箱反查用户", "yellow",
                        "按邮箱查回来的账号和本地记的不是同一个（远端账号多半被删过又重建）。"
                        "这一个用户需要重新开通，不影响其他人",
                        actual="查到的是另一个账号", expected="查回同一个人",
                    ))
                else:
                    items.append(_item(
                        "按邮箱反查用户", "green",
                        "用完整邮箱能精确查回同一个人，「邮箱已存在」时的兜底路径是通的",
                        actual="精确命中", expected="查回同一个人",
                    ))

    # ── 假设 3：admin 侧能不能替用户重置密码 —— 没法零副作用验证 ──
    items.append(_item(
        "用户自己改了密码之后能不能自愈", "unknown",
        "这一项没法在「不改动任何东西」的前提下验证，所以系统按最坏情况处理："
        "只要某个用户自己改了在 Sub2API 的密码，这一行就直接转成「等管理员手工发 Key」，"
        "不会反复重试。这是设计里就认了的，不是故障",
        expected="按最坏情况处理（转手工）",
    ))
    return items


def _selfcheck_actions(items: List[Dict[str, Any]]) -> List[str]:
    """把所有红黄项的人话说明汇成一张「该怎么办」清单，红的排前面。"""
    return (
        [i["summary"] for i in items if i["level"] == "red"]
        + [i["summary"] for i in items if i["level"] == "yellow"]
    )


def _run_selfcheck(db: Session) -> Dict[str, Any]:
    """跑一次自检。全程只读，不建号、不发 Key、不代登录、不改余额。"""
    cfg = provisioning.load_config(db)   # 含明文管理员 Key，只在本函数里活着
    checked_at = utcnow().isoformat()
    halt_text, halt_at = provisioning.halt_reason()
    base: Dict[str, Any] = {
        "checked_at": checked_at,
        "cached": False,
        "auto_provision": cfg.enabled,
        "keep_password": cfg.keep_password,
        "email_domain": cfg.email_domain,
        "group_id": cfg.group_id,
        "halted": bool(halt_text),
        "halt_reason": halt_text,
        "halted_at": halt_at.isoformat() if halt_at else None,
        "paused_now": False,
    }

    # —— 先看本地配置。没配齐就没必要往外发请求 ——
    problem = cfg.missing()
    if problem:
        base.update({
            "configured": False,
            "level": "red",
            "message": "还不能自动开通：%s。请在上面填好之后保存，再点一次自检" % problem,
            "probes": [_item("designkit 这边的配置", "red",
                            "%s。填完保存一次再点自检" % problem)],
            "actions": ["%s。填完保存一次再点自检" % problem],
            "unverified": 0,
        })
        return base

    account, sample_key = _pick_sample_account(db)
    client = provisioning.build_client(cfg)
    try:
        # sample_jwt 恒为 None：**绝不为了点亮探针 5 去真的登录一次**。
        # 登录有三个真实副作用（限流额度、审计日志、对端 Redis 里的令牌家族），
        # 一个自检按钮不该有这些后果。查不了就照实标「未验证」。
        # run_probes 的总判定这里不用：下面还要把「六条假设」的结论一起并进来算。
        _, probes = client.run_probes(sample_api_key=sample_key or None, sample_jwt=None)
        items = [
            _item(p.name, p.level, p.summary, detail=p.detail)
            for p in probes
        ]
        items.extend(_check_assumptions(client, cfg, account, probes))
    except Exception as exc:  # noqa: BLE001 —— 自检本身崩了也要给人话，不能回 500
        items = [_item("自检本身出错了", "red",
                       "自检过程中出现意外错误（%s）。请确认 Sub2API 地址填得对不对，"
                       "或者稍后再试" % type(exc).__name__,
                       detail={"code": "E_SELFCHECK_CRASH"})]
    finally:
        client.close()

    overall = _combine_level([i["level"] for i in items])
    unverified = sum(1 for i in items if i["level"] == "unknown")

    # —— 红灯 → 自动暂停自动开通 ——
    # 不暂停的话，worker 会挨个把全站用户刷到重试上限、集体降级成「等人工发 Key」，
    # 而这本来只是一个开关改回来就能修好的问题。
    # 但「连不上」这类瞬时红灯不算：管理员在 NAS 重启的当口点一下自检，
    # 不该把总开关给关掉——关掉之后没人会想起来再打开。
    blocking = [
        i for i in items
        if i["level"] == "red" and i["detail"].get("code") not in _TRANSIENT_ERROR_CODES
    ]
    if blocking:
        reason = blocking[0]["summary"]
        provisioning.set_halt(reason)
        if cfg.enabled:
            settings_service.set_many(db, {"sub2api_auto_provision": False})
        base.update({"paused_now": True, "auto_provision": False,
                     "halted": True, "halt_reason": reason,
                     "halted_at": (provisioning.halt_reason()[1] or utcnow()).isoformat()})

    if overall == "red":
        overall_text = "现在不能自动开通。先处理下面标红的问题，处理完回到这里再点一次自检"
    elif overall == "yellow":
        overall_text = "现在可以自动开通，但有几处需要注意（见下）"
    else:
        overall_text = "现在可以自动开通"
    if unverified:
        # 这句必须留着。有几项是真的没查（库里还没有开通成功的用户、
        # 手上没有登录令牌），画成绿灯会让管理员以为查过了。
        overall_text += "。另有 %d 项这次没法验证，已照实标成「未验证」——不代表已经查过" % unverified
    if base["halted"] and overall != "red":
        overall_text += "。注意：自动开通目前仍处于暂停状态，确认没问题后请点「我已处理，恢复自动开通」"

    base.update({
        "configured": True,
        "level": overall,
        "message": overall_text,
        "probes": items,
        "actions": _selfcheck_actions(items),
        "unverified": unverified,
    })
    return base


@router.post("/test_provisioning")
def test_provisioning(_: User = Depends(require_admin), db: Session = Depends(get_db)):
    """点一下就知道现在还能不能自动开通。**全程只读，没有任何副作用。**

    这一条**每次都真跑一遍，不吃缓存**。管理员是刚去 Sub2API 后台改完设置
    才回来点这个按钮的，给他一份 60 秒前的旧结论，只会让他以为「改了没用」。
    设计文档里那个「缓存 60 秒防刷页面」的目的由 GET /provisioning 承担——
    面板加载走的是那一条，它只读数据库、一个网络请求都不发。
    这里跑完仍然会把结果存进缓存，好让随后打开的面板能显示「刚才那次的结论」。

    **永远返回 HTTP 200**，成败看响应体里的 level——和「测试连接」那两个接口
    一个规矩。前端是按这个约定写的：走到 catch 就说明接口本身挂了，
    而不是「自检发现有问题」，两者的提示文案完全不同。

    响应体（前端 settings.js 按这份字段名渲染，改名要连它一起改）：
      level        green / yellow / red —— 三色灯
      message      一句人话结论，直接显示
      probes       检查项清单，每项 {name, level, level_text, summary, actual, expected, detail}
      actions      所有红黄项的「该怎么办」，红的排前面
      unverified   这次没法验证的项数（界面上要照实说「未验证」）
      configured   本地配置齐不齐
      paused_now   这次自检是不是把自动开通给暂停了
      summary      各状态人数 + 卡住的人（和 GET /provisioning 同一份）
    """
    payload = _run_selfcheck(db)
    # 顺带把概览一起带上：自检之后各状态的人数往往就变了（比如刚被暂停），
    # 让前端少发一次请求，也避免它拿着自检结果配旧概览显示出自相矛盾的两句话。
    payload["summary"] = provisioning.summary(db)
    _selfcheck_store(payload)
    return payload


@router.get("/provisioning")
def get_provisioning_status(_: User = Depends(require_admin), db: Session = Depends(get_db)):
    """「网关自动开通」面板的状态，**不发任何网络请求**，可以随页面一起加载。

    包含：各状态的人数、卡住的用户清单（含人话报错）、全局暂停状态、
    本地配置齐不齐，以及上一次自检的结果（如果 60 秒内跑过）。
    """
    cfg = provisioning.load_config(db)   # 含明文管理员 Key，只用来判断齐不齐
    payload = provisioning.summary(db)
    payload.update({
        "configured": not cfg.missing(),
        "config_problem": cfg.missing(),
        "auto_provision": cfg.enabled,
        "keep_password": cfg.keep_password,
        "shared_fallback": bool(settings_service.get(db, "gateway_shared_fallback")),
        "email_domain": cfg.email_domain,
        "group_id": cfg.group_id,
        "selfcheck": _selfcheck_cached(),   # 没跑过或已过期就是 null
    })
    return payload


@router.post("/provisioning/resume")
def resume_provisioning(_: User = Depends(require_admin), db: Session = Depends(get_db)):
    """「我已处理，恢复自动开通」按钮。

    一次点击做两件事，缺一不可：
    ① 解除进程内存里的全局暂停闸；
    ② 把持久化的总开关重新打开。

    为什么必须一起做：红灯自动暂停时，内存闸和数据库开关是**两处都被关掉**的
    （worker 侧的 _persist_pause 也会关数据库那个）。只解内存闸的话，
    管理员点完按钮、页面提示已恢复，可重启一次服务又回到暂停——
    这种「点了没用、还看不出为什么」的坑必须堵死在这里。

    配置没填齐时不允许恢复：打开了也只会一路失败。
    """
    cfg = provisioning.load_config(db)
    problem = cfg.missing()
    if problem:
        raise _fail("%s。填好并保存之后再恢复自动开通" % problem)
    provisioning.clear_halt()
    settings_service.set_many(db, {"sub2api_auto_provision": True})
    _selfcheck_clear()   # 上一次那份「红灯 + 已暂停」的结论已经不成立了
    # 返回的是**恢复之后**的那份概览：前端点完按钮直接拿它重画面板，
    # 那条「已暂停」的红条会当场消失。回一个 {"ok": true} 的话，
    # 红条要等到下次刷新才没，管理员会以为按钮没生效又点一遍。
    payload = provisioning.summary(db)
    payload.update({"ok": True, "auto_provision": True,
                    "message": "自动开通已恢复。建议再点一次「立即自检」确认"})
    return payload
