"""短信客户端：把一条验证码送到用户手机上——或者（**默认**）根本不发。

════════════════════════════════════════════════════════════════════════
 一、这个文件里的每一次调用都是一笔真金白银的支出
════════════════════════════════════════════════════════════════════════
本项目其余的外部调用（生图、网关开号）失败了顶多是「这次没成」，重试一遍就好。
短信不是：**请求一旦发出去就扣钱，而且这个接口不支持幂等**（阿里云官方明确
要求调用方自己做幂等控制）。所以这个文件里有两条铁律：

  1. **绝不自动重试**。网络超时、连接断开、返回一坨看不懂的东西——一律当作
     「这次失败了」交还给调用方，由**用户自己**决定要不要再点一次。
     自动重试的最坏情况是：上游其实已经发出去了，只是回执没回到我们手上，
     于是同一个人收到两条短信、我们扣两次钱、还更容易撞上阿里云的流控。
     （唯一的例外见下面第四节，那一条是**确定没发出去**才重试的。）
  2. **限速不在这个文件里，但一定要有**。三层限速（同一手机号 / 同一 IP /
     全站每日总量）由调用方在**调用本模块之前**用 services/ratelimit.py 完成。
     写在调用方是因为限速要和「写 phone_verification_codes 那一行」排在同一个
     顺序里（先过限速 → 再插行 → 再调本模块），本模块不碰数据库的写。
     ⚠ 设置页那个「试发一条」的按钮同样要限速——它花的是一模一样的钱。

════════════════════════════════════════════════════════════════════════
 二、两种通道，**调试模式是默认的**
════════════════════════════════════════════════════════════════════════
  * debug  —— 不连任何外部服务。验证码写进服务端日志，并且**原样放在返回值里**，
              由调用方显示在页面上，好让整条注册流程在没配短信的情况下先跑通。
  * aliyun —— 阿里云短信服务，真的发、真的花钱。

**返回值里的 debug 标志是一个硬要求，不是顺手加的。** 调用方必须据此在界面上
显著标明「这是调试模式，验证码没有真的发出去」（现成的文案见 DEBUG_MODE_NOTICE）。
不标的话，哪天管理员忘了切回 aliyun，她会一直以为短信发出去了，
而用户那边永远收不到——双方都查不出问题出在哪，这种故障没有人能自查。

════════════════════════════════════════════════════════════════════════
 三、为什么自己实现签名，而不是装阿里云的 SDK
════════════════════════════════════════════════════════════════════════
镜像是 amd64 + arm64 双架构自动构建的，requirements.txt 里写死了
`--only-binary=:all:`（不许现场编译源码）。多一个依赖就多一份「哪天它的 arm64
预编译包没跟上、群晖那半边镜像构建当场失败」的风险，而那种失败只在 GitHub 的
构建机上出现，本机 Mac 永远复现不了。签名本身只用到 hmac / hashlib / base64 /
urllib.parse 四个标准库模块，自己写反而更可控。

签名算法有两版，本文件两版都实现了：
  * **V3（ACS3-HMAC-SHA256）**：阿里云现在推荐的。公共参数全走请求头。
  * **V2（HMAC-SHA1）**：官方标为「不推荐、逐步废弃」但至今可用，
    旧版官方 Python SDK 用的就是它，**对 dysmsapi 这个端点有确凿的实战背书**。

════════════════════════════════════════════════════════════════════════
 四、默认用 V3，撞上 SignatureDoesNotMatch 时自动降到 V2 再试一次
════════════════════════════════════════════════════════════════════════
这是本文件唯一一处自动重试，它和第一节那条铁律不矛盾，理由必须说清楚：
**SignatureDoesNotMatch 是阿里云的 API 网关在校验签名那一步就拒掉的，
根本没走到短信业务，因此可以确定「这一次绝对没有发出任何短信、没有扣任何钱」。**
重试它不会造成重复发送，这和「超时了不知道发没发」是两种完全不同的情况。

为什么值得多写这几行：V3 的算法我们是拿阿里云官方算例逐字节对拍验证过的，
但**没有对 dysmsapi.aliyuncs.com 这个端点做过真实调用验证**（做一次就要花钱发
一条真短信）。万一这个端点不吃 V3，表现会是「管理员把 AccessKey 填得完全正确，
点试发却一直报签名不匹配」——而她既看不懂这个英文错误码，也没有任何办法自己
绕过去。降级到 V2 之后大概率直接成功，日志里会留一条明确的记录说明发生了什么。

════════════════════════════════════════════════════════════════════════
 五、这个文件里绝不能出现的东西
════════════════════════════════════════════════════════════════════════
  * 任何真实的 AccessKey / AccessKeySecret（连测试数据里也不许有）。
    凭据只有一条来路：管理员在网页设置里自己填 → 加密存库 → 这里解密后用完即弃。
  * 日志里的 AccessKey（Id 和 Secret 都不许，一个字都不行）。
  * 日志里的验证码明文——**除了 debug 模式**，那一条是刻意为之的功能。
  * 日志、报错、返回值里的完整手机号：一律用 mask_phone() 脱敏成前 3 后 4。
"""
import base64
import hashlib
import hmac
import json
import logging
import re
import secrets
import time
import uuid
from typing import Any, Dict, List, NamedTuple, Optional, Tuple
from urllib.parse import quote

import httpx
from sqlalchemy.orm import Session

from . import secrets_box, settings_service

logger = logging.getLogger("designkit.sms")


# ══════════════════════════════════════════════════════════════════════
#  常量
# ══════════════════════════════════════════════════════════════════════

# 通道取值，与 models.PHONE_CODE_CHANNELS 一致（那边是数据库的口径，这边是代码的）。
CHANNEL_DEBUG = "debug"
CHANNEL_ALIYUN = "aliyun"

# 调试模式下必须显示在界面上的那句话。**放在后端而不是前端**，理由和
# routers/register.py 里的规则文案一样：文案只有一份，改了不会出现
# 「页面上写的和实际行为对不上」。前端直接把它原样显示成醒目的横幅。
DEBUG_MODE_NOTICE = (
    "当前是调试模式：验证码并没有真的发送到手机上，只显示在这个页面和服务器日志里。"
    "要真的发短信，请管理员到「系统设置」把短信通道改成阿里云并填好配置。"
)

# 阿里云短信的默认接口域名。国内短信所有地域共用这一个公网域名，不需要区分地域，
# SendSms 也没有 RegionId 这个参数。设置项 sms_aliyun_endpoint 存在的唯一目的
# 是让自动化测试把它指向本机的假上游，从而验证签名算法而不真的发短信、不花钱。
DEFAULT_ENDPOINT = "dysmsapi.aliyuncs.com"

_ACTION = "SendSms"
_API_VERSION = "2017-05-25"

# 连接和读取的超时（秒）。官方建议国内短信的调用超时 ≥ 1 秒，我们给得宽松一些：
# 发验证码是用户正盯着屏幕等的动作，超过十几秒就该告诉他「没发出去，再点一次」，
# 而不是让他对着转圈的按钮干等。**超时之后绝不自动重试**（见文件头第一节）。
_CONNECT_TIMEOUT = 5.0
_READ_TIMEOUT = 10.0

# 验证码长度。**只能是纯数字，而且建议就是 6 位**，这是阿里云的硬约束不是审美：
# 验证码类模板的变量值只允许 [a-zA-Z0-9]，混进中文、空格、连字符会被拒
# （报错原文是 params must be [a-zA-Z0-9] for verification sms）；
# 而变量长度官方好几个页面口径不一致（4~6 位 / 1~32 位 / 1~35 字符），
# 6 位纯数字在所有说法下都合法，是唯一确定安全的选择。
CODE_LENGTH = 6

# 中国大陆手机号：1 开头、第二位 3~9、共 11 位纯数字。
_PHONE_RE = re.compile(r"^1[3-9]\d{9}$")


# ══════════════════════════════════════════════════════════════════════
#  错误分类：把阿里云的英文错误码翻译成「人话 + 该怎么办」
# ══════════════════════════════════════════════════════════════════════
# 使用者是电商运营，看到 isv.AMOUNT_NOT_ENOUGH 只会来问「这是什么意思」。
# 每一条都拆成两句话，因为它们要给两种人看：
#
#   message     —— 给**发起这次操作的人**看（多半是公网上一个正在注册的陌生人）。
#                  只说他能理解、能行动的部分，**不泄露系统内部状态**：
#                  「阿里云余额不足」这种话对陌生人来说是一份免费的内部情报。
#   admin_hint  —— 给**管理员**看（设置页的试发结果、服务端日志）。
#                  这一句要具体到「登录哪里、点哪一项、改成什么」。
#
# category 是给代码判断用的。三种最可能真实遇到的单独立类，调用方可以据此
# 做不同的处理（比如余额不足要在管理后台挂一条醒目的告警）：
#   CATEGORY_BALANCE       —— 欠费/停机，钱的问题
#   CATEGORY_FLOW_CONTROL  —— 被阿里云自己的频率限制拦了
#   CATEGORY_CONFIG        —— 签名/模板没审核通过、填错、类型不匹配
CATEGORY_OK = "ok"
CATEGORY_BALANCE = "balance"
CATEGORY_FLOW_CONTROL = "flow_control"
CATEGORY_CONFIG = "config"
CATEGORY_CREDENTIAL = "credential"      # AccessKey 不对 / 没权限
CATEGORY_PHONE = "phone"                # 号码本身的问题（格式、黑名单）
CATEGORY_SERVER_TIME = "server_time"    # 我们这台机器的时钟不准
CATEGORY_BUSY = "busy"                  # 上游一时忙，等几秒就好
CATEGORY_NETWORK = "network"            # 根本没连上，或者返回看不懂
CATEGORY_BUG = "bug"                    # 只可能是我们自己写错了
CATEGORY_UNKNOWN = "unknown"            # 还没登记过的错误码


class _ErrorInfo(NamedTuple):
    category: str
    retriable: bool
    message: str      # 给最终用户
    admin_hint: str   # 给管理员


# 通用的「别把内部状态说给陌生人听」版本。凡是根因在我们这边（配置、余额、凭据）
# 的错误，对最终用户一律用这一句——他确实什么也做不了，能做的只有换条路。
_USER_SIDE_UNAVAILABLE = "验证码暂时发不出去。请稍后再试，或者联系管理员帮你开通账号。"

_ERRORS: Dict[str, _ErrorInfo] = {
    # ── 钱的问题 ──────────────────────────────────────────────────────
    "isv.AMOUNT_NOT_ENOUGH": _ErrorInfo(
        CATEGORY_BALANCE, False, _USER_SIDE_UNAVAILABLE,
        "阿里云短信账户余额不足，发不出去。请登录阿里云控制台给「短信服务」充值，"
        "充值后立刻恢复，不用重启系统。（国内短信约 4 分钱一条。）",
    ),
    "isv.OUT_OF_SERVICE": _ErrorInfo(
        CATEGORY_BALANCE, False, _USER_SIDE_UNAVAILABLE,
        "短信业务已经因为欠费被停用了（比「余额不足」更严重一级）。"
        "请登录阿里云控制台充值，充值后服务会自动恢复。",
    ),
    # ── 被阿里云自己的频率限制拦了 ────────────────────────────────────
    "isv.BUSINESS_LIMIT_CONTROL": _ErrorInfo(
        CATEGORY_FLOW_CONTROL, True,
        "这个手机号收验证码太频繁了，被短信平台暂时限制。请等 1 分钟后再试；"
        "如果一小时内已经获取过 5 次，需要等一小时。",
        "触发了阿里云自身的发送频率限制（同一签名 + 同一手机号默认 1 条/分钟、"
        "5 条/小时、10 条/天）。如果经常出现，说明我们这边的限速比阿里云还松，"
        "请到设置页把「同一手机号」的两项阈值调严一些。",
    ),
    "isv.DAY_LIMIT_CONTROL": _ErrorInfo(
        CATEGORY_FLOW_CONTROL, True,
        "今天的短信发送额度已经用完了。请明天再试，或者联系管理员帮你开通账号。",
        "触发了阿里云控制台「安全设置」里的日发送总量上限。可以在控制台调高，"
        "或者等第二天自动恢复。（这个上限本身是防盗刷的保险丝，别调得太大。）",
    ),
    "isv.MONTH_LIMIT_CONTROL": _ErrorInfo(
        CATEGORY_FLOW_CONTROL, True,
        "本月的短信发送额度已经用完了。请联系管理员帮你开通账号。",
        "触发了阿里云控制台里设定的月发送总量上限，请在控制台调整。",
    ),
    # ── 号码本身的问题 ────────────────────────────────────────────────
    "isv.MOBILE_NUMBER_ILLEGAL": _ErrorInfo(
        CATEGORY_PHONE, False,
        "手机号填写有误，请填写 11 位中国大陆手机号，不要加空格、横线或国家代码。",
        "阿里云判定这个号码格式非法。正常情况下我们在发送前已经校验过一次，"
        "还能走到这里说明号码里有我们没考虑到的形态，请把号码发给开发者看看。",
    ),
    "isv.BLACK_KEY_CONTROL_LIMIT": _ErrorInfo(
        CATEGORY_PHONE, False,
        "这个手机号收不到验证码（多半是以前回复过退订短信，被短信平台拉黑了，"
        "而且没法解除）。请换一个手机号，或者联系管理员帮你开通账号。",
        "该号码在阿里云的黑名单里，无法解除。只能换号，或者到「成员账号」页"
        "直接给这个人建号。",
    ),
    "MOBILE_IN_BLACK": _ErrorInfo(
        CATEGORY_PHONE, False,
        "这个手机号在运营商那边被拦截了，收不到验证码。请换一个手机号，"
        "或者联系管理员帮你开通账号。",
        "号码在运营商侧的黑名单里，我们这边无能为力。只能换号或后台建号。",
    ),
    # ── 签名 / 模板 / 参数：配置没弄对（运营最可能遇到的一类）──────────
    "isv.SMS_SIGNATURE_ILLEGAL": _ErrorInfo(
        CATEGORY_CONFIG, False, _USER_SIDE_UNAVAILABLE,
        "短信签名配置不对。请到设置页检查「短信签名」是否和阿里云控制台里那个"
        "审核通过的签名**一字不差**（注意别多打空格），并确认 AccessKey 和签名"
        "属于同一个阿里云账号、签名已经审核通过。",
    ),
    "isv.SMS_TEMPLATE_ILLEGAL": _ErrorInfo(
        CATEGORY_CONFIG, False, _USER_SIDE_UNAVAILABLE,
        "短信模板配置不对。请到设置页检查「模板 CODE」是否和阿里云控制台里那个"
        "审核通过的模板一致（形如 SMS_123456789），并确认模板已经审核通过、"
        "模板正文里的变量名就是 ${code}。",
    ),
    "isv.TEMPLATE_PARAMS_ILLEGAL": _ErrorInfo(
        CATEGORY_CONFIG, False, _USER_SIDE_UNAVAILABLE,
        "短信模板的变量设置和我们发送的内容对不上。请确认模板里的验证码变量"
        "叫 code、类型选的是「验证码」；我们发过去的是 6 位纯数字。",
    ),
    "isv.SMS_SIGNATURE_SCENE_ILLEGAL": _ErrorInfo(
        CATEGORY_CONFIG, False, _USER_SIDE_UNAVAILABLE,
        "短信签名和模板的类型不匹配（比如拿「通知类」签名去发「验证码类」模板）。"
        "请在阿里云控制台把签名改成「通用」类型，或者换一个验证码类型的签名。",
    ),
    "isv.SMS_CONTENT_ILLEGAL": _ErrorInfo(
        CATEGORY_CONFIG, False, _USER_SIDE_UNAVAILABLE,
        "短信内容不符合运营商规范，被拦截了。请按阿里云的模板规范修改模板文案，"
        "重新提交审核。",
    ),
    "isv.SMS_TEST_NUMBER_LIMIT": _ErrorInfo(
        CATEGORY_CONFIG, False, _USER_SIDE_UNAVAILABLE,
        "现在用的是阿里云的短信「测试」模式，只能发给控制台里绑定的测试号码。"
        "请改用正式审核通过的签名和模板。",
    ),
    "isv.PRODUCT_UN_SUBSCRIPT": _ErrorInfo(
        CATEGORY_CONFIG, False, _USER_SIDE_UNAVAILABLE,
        "这个阿里云账号还没开通「短信服务」。请先到阿里云控制台开通。",
    ),
    "PORT_NOT_REGISTERED": _ErrorInfo(
        CATEGORY_CONFIG, False, _USER_SIDE_UNAVAILABLE,
        "短信通道还没完成运营商的企业实名报备，暂时发不出去。请在阿里云控制台"
        "提交实名报备，一般要 5~10 个工作日。这期间可以先用调试模式或后台建号。",
    ),
    "isv.INVALID_PARAMETERS": _ErrorInfo(
        CATEGORY_CONFIG, False, _USER_SIDE_UNAVAILABLE,
        "发送短信的参数有误。请到设置页核对「短信签名」和「模板 CODE」是否填写正确。",
    ),
    "isv.ACCOUNT_ABNORMAL": _ErrorInfo(
        CATEGORY_CONFIG, False, _USER_SIDE_UNAVAILABLE,
        "阿里云短信账户状态异常（计费服务异常）。请登录阿里云控制台查看账户状态，"
        "或联系阿里云客服。",
    ),
    # ── 凭据：AccessKey 填错、没权限 ──────────────────────────────────
    "SignatureDoesNotMatch": _ErrorInfo(
        CATEGORY_CREDENTIAL, False, _USER_SIDE_UNAVAILABLE,
        "阿里云没能通过我们的身份验证。最常见的原因是 AccessKey ID 或 Secret 填错了、"
        "或者复制的时候带进了空格和换行。请到设置页重新填一遍这两项，保存后再点"
        "「试发一条」。（签名算法我们已经用阿里云官方算例对拍验证过，两种签名版本"
        "也都试过了，所以基本可以排除是程序算错。）",
    ),
    "InvalidAccessKeyId.NotFound": _ErrorInfo(
        CATEGORY_CREDENTIAL, False, _USER_SIDE_UNAVAILABLE,
        "填的 AccessKey ID 在阿里云查不到，可能是填错了，也可能已经被停用或删除。"
        "请到阿里云控制台重新生成一对 AccessKey，再填进设置页。",
    ),
    "isp.RAM_PERMISSION_DENY": _ErrorInfo(
        CATEGORY_CREDENTIAL, False, _USER_SIDE_UNAVAILABLE,
        "这把 AccessKey 没有发短信的权限。请在阿里云 RAM 控制台给这个子账号授予"
        "「AliyunDysmsFullAccess」权限，或者改用主账号的 AccessKey。",
    ),
    # ── 我们这台机器的时钟 ────────────────────────────────────────────
    "InvalidTimeStamp.Expired": _ErrorInfo(
        CATEGORY_SERVER_TIME, True, "验证码暂时发不出去，请过一会儿再试一次。",
        "服务器时间和阿里云对不上（相差超过 15 分钟），签名被判为过期。"
        "请检查这台机器（群晖）的系统时间，把自动对时打开。"
        "⚠ 时钟不准还会影响验证码有效期和限速，务必尽快处理。",
    ),
    # ── 上游一时忙 ────────────────────────────────────────────────────
    "Throttling": _ErrorInfo(
        CATEGORY_BUSY, True, "系统一时太忙，请等几秒再点一次「获取验证码」。",
        "触发了阿里云 OpenAPI 层的每秒调用次数限制。我们这个量级正常碰不到，"
        "如果反复出现，请检查是不是有脚本在刷。",
    ),
    "Throttling.User": _ErrorInfo(
        CATEGORY_BUSY, True, "系统一时太忙，请等几秒再点一次「获取验证码」。",
        "触发了阿里云 OpenAPI 层的每秒调用次数限制。我们这个量级正常碰不到，"
        "如果反复出现，请检查是不是有脚本在刷。",
    ),
    # ── 只可能是我们自己写错了 ────────────────────────────────────────
    "isv.MOBILE_COUNT_OVER_LIMIT": _ErrorInfo(
        CATEGORY_BUG, False, _USER_SIDE_UNAVAILABLE,
        "一次提交的手机号超过了 1000 个。注册流程每次只发一个号码，"
        "出现这个错说明程序有 bug，请把这条日志发给开发者。",
    ),
}


def classify(error_code: str, upstream_message: str = "") -> _ErrorInfo:
    """把阿里云的错误码翻译成人话。**没登记过的码也一定有个说法。**

    错误码集合不是封闭的：阿里云随时可能加新码，而且有几个码（比如
    Throttling.User）连官方文档都没在短信那一页逐字列出。所以这里的兜底分支
    不是可有可无的礼貌，是必须的——否则某天冒出一个新码，用户看到的会是
    一个空白提示或者一串英文，而管理员在日志里也找不到任何线索。
    """
    info = _ERRORS.get(error_code)
    if info is not None:
        return info
    tail = ("阿里云原话：%s" % upstream_message) if upstream_message else ""
    return _ErrorInfo(
        CATEGORY_UNKNOWN, False,
        "验证码发送失败，请稍后再试；一直失败的话请联系管理员。",
        "阿里云返回了一个我们还没登记过的错误码：%s。%s "
        "请把这一整行发给开发者，同时可以在阿里云的「国内消息错误码」文档里"
        "搜索这个码看看官方怎么说。" % (error_code or "(空)", tail),
    )


# ══════════════════════════════════════════════════════════════════════
#  手机号：归一化与脱敏
# ══════════════════════════════════════════════════════════════════════

def normalize_phone(raw: Optional[str]) -> str:
    """把用户填的手机号收拾成库里存的样子：**11 位纯数字**。不合法就返回空串。

    这个函数是全系统关于「手机号长什么样」的**唯一口径**，
    phone_verification_codes.phone 和 auth_identities.identifier 存的都必须是它的
    输出。两处口径一旦写岔，表现是「验证码验过了，注册却说这个号已被占用」
    （或者反过来），而两条记录看起来一模一样，谁也看不出差在哪。
    其他模块要用请 `from .sms import normalize_phone`，**不要再抄一份正则**。

    宽进严出：用户会填 138 0013 8000、+86 13800138000、86-13800138000，
    这些都是同一个号，全部收成 13800138000。
    """
    text = (raw or "").strip()
    if not text:
        return ""
    # 只留数字：空格、连字符、括号、+ 号一律扔掉。
    digits = re.sub(r"\D", "", text)
    # 去掉国际区号前缀。只在「去掉之后正好剩 11 位」时才去，
    # 否则 008613800138000 这种和一个恰好以 86 开头的 13 位乱码分不开。
    for prefix in ("0086", "86"):
        if digits.startswith(prefix) and len(digits) == len(prefix) + 11:
            digits = digits[len(prefix):]
            break
    return digits if _PHONE_RE.match(digits) else ""


def mask_phone(phone: Optional[str]) -> str:
    """脱敏成前 3 后 4（13800138000 → 138****8000）。

    **凡是手机号要出现在日志、报错、返回给前端的文字里，一律先过这个函数。**
    库里存完整号码的地方只有两处（phone_verification_codes 和 auth_identities），
    别让第三处冒出来——日志文件是会被打包发给别人排查的。

    对不完整/不合法的输入也要给个能看的结果：这个函数经常在报错路径上被调用，
    绝不能因为号码本身有问题而再抛一个异常，把真正的错误盖掉。
    """
    text = re.sub(r"\s", "", str(phone or ""))
    if not text:
        return "(空)"
    if len(text) <= 7:
        # 太短，露前 1 位意思一下就行，多了等于没脱敏
        return text[:1] + "*" * (len(text) - 1)
    return text[:3] + "*" * (len(text) - 7) + text[-4:]


def generate_code(length: int = CODE_LENGTH) -> str:
    """生成一个验证码：**纯数字，默认 6 位，用密码学安全的随机源**。

    用 secrets 而不是 random：random 是可预测的伪随机（梅森旋转），
    知道几个历史输出就能推出后面的——验证码用它等于没有验证码。
    这个错误不会有任何症状，代码跑起来一模一样，只有被攻击时才显形。

    为什么只能是纯数字，见 CODE_LENGTH 的注释（阿里云验证码模板的硬约束）。
    """
    length = max(4, min(8, int(length)))
    upper = 10 ** length
    return str(secrets.randbelow(upper)).zfill(length)


# ══════════════════════════════════════════════════════════════════════
#  配置
# ══════════════════════════════════════════════════════════════════════

class SmsConfig(NamedTuple):
    """发一条短信需要的全部信息。**access_key_secret 是明文，绝不许进日志。**"""

    provider: str            # 'debug' | 'aliyun'
    access_key_id: str
    access_key_secret: str
    sign_name: str
    template_code: str
    endpoint: str
    # Secret 解密失败时的人话说明（正常为空串）。不在 load_config 里抛异常，
    # 是为了让设置页的自检能把这一条和「还没填」一起列出来给管理员看。
    secret_error: str = ""


def load_config(db: Session) -> SmsConfig:
    """从运行时设置里读出短信配置，顺手把 AccessKey Secret 解密。

    ── 为什么要判断「是不是密文」而不是直接解密 ──
    约定是「写设置时 secrets_box.encrypt，发送时 decrypt」，但库里可能存着
    没加密的老值：功能上线之前手工塞进去的、或者哪一版写设置的代码漏了加密。
    直接解密会让这些值全部报错，而报错信息（「.enc_key 换了一把」）会把人
    引到完全错误的方向上。所以照 secrets_box 文档里那套「明文原地升级成密文」
    的做法：是密文就解，不是就当明文用，并且吼一声让管理员重新保存一次设置。
    """
    values = settings_service.get_all(db)

    def _text(key: str, fallback: str = "") -> str:
        raw = values.get(key)
        if raw is None or isinstance(raw, bool):
            return fallback
        return str(raw).strip()

    secret_raw = _text("sms_aliyun_access_key_secret")
    secret = ""
    secret_error = ""
    if secret_raw:
        if secrets_box.is_encrypted(secret_raw):
            try:
                secret = secrets_box.decrypt(secret_raw)
            except secrets_box.SecretsBoxError as exc:
                # 异常消息里只有补救办法，没有密文本身（见 secrets_box 的约定）
                secret_error = str(exc)
                logger.error("短信 AccessKey Secret 解密失败：%s", secret_error)
        else:
            secret = secret_raw
            logger.warning(
                "短信 AccessKey Secret 在数据库里是**明文**存着的。"
                "请到设置页把它重新填一遍并保存，保存后会自动加密。"
            )

    provider = _text("sms_provider", CHANNEL_DEBUG).lower()
    if provider not in (CHANNEL_DEBUG, CHANNEL_ALIYUN):
        # 设置项被改成了不认识的值。**退回 debug 而不是 aliyun**：
        # 猜错方向的代价不对称——退回 debug 最多是「短信没发出去，但页面上
        # 明确写着调试模式」，退到 aliyun 则是拿着一份可疑的配置去花钱。
        logger.warning("短信通道设置成了不认识的值（%r），本次按调试模式处理", provider)
        provider = CHANNEL_DEBUG

    return SmsConfig(
        provider=provider,
        access_key_id=_text("sms_aliyun_access_key_id"),
        access_key_secret=secret,
        sign_name=_text("sms_aliyun_sign_name"),
        template_code=_text("sms_aliyun_template_code"),
        endpoint=_text("sms_aliyun_endpoint", DEFAULT_ENDPOINT) or DEFAULT_ENDPOINT,
        secret_error=secret_error,
    )


def config_problems(config: SmsConfig) -> List[str]:
    """走阿里云通道还差哪几项？返回一串人话；**空列表表示配齐了**。

    两个地方要用它：
      ① 设置页要把短信通道从 debug 切到 aliyun 时，先拿它挡一道——
         没配齐就切过去，表现是「每个人点获取验证码都失败」，而失败原因是
         一串英文错误码，管理员完全不知道是自己没填完；
      ② 手机号注册总开关（phone_register_enabled）打开之前也要过这一道
         （见 config.py 里那一项的说明）。
    """
    problems: List[str] = []
    if not config.access_key_id:
        problems.append("还没填「阿里云 AccessKey ID」")
    if config.secret_error:
        problems.append("「阿里云 AccessKey Secret」读不出来：" + config.secret_error)
    elif not config.access_key_secret:
        problems.append("还没填「阿里云 AccessKey Secret」")
    if not config.sign_name:
        problems.append("还没填「短信签名」（阿里云控制台里审核通过的那个，形如「XX电商」）")
    if not config.template_code:
        problems.append("还没填「短信模板 CODE」（形如 SMS_123456789）")
    if not config.endpoint:
        problems.append("「短信接口域名」是空的，正常应该是 " + DEFAULT_ENDPOINT)
    return problems


def is_ready_for_aliyun(config: SmsConfig) -> bool:
    """阿里云那四项配齐了没有（不校验对不对，那只能靠试发一条）。"""
    return not config_problems(config)


# ══════════════════════════════════════════════════════════════════════
#  百分号编码：这套签名最经典的坑就在这里
# ══════════════════════════════════════════════════════════════════════

def percent_encode(value: Any) -> str:
    """阿里云签名要求的 RFC3986 百分号编码。

    ════════════════════════════════════════════════════════════════
     写错的表现是「一直 403 SignatureDoesNotMatch，而且看不出哪里错」
    ════════════════════════════════════════════════════════════════
    签名不匹配时阿里云不会告诉你是哪个参数编码错了，只会甩一句
    「Specified signature is not matched with our calculation」。
    而请求发出去的样子和签名算出来的样子只要差一个字节就会这样，
    所以这个函数必须单独拎出来、单独测试。

    规则：不编码的只有 A-Z a-z 0-9 和 - _ . ~ 四个符号；
    其余全部编成 %XY（大写十六进制），非 ASCII 先按 UTF-8 转字节再逐字节编。

    ── 和 Python 标准库那几个函数的差别（三个都真的会踩到）──

    1. **不能用 urllib.parse.quote_plus**：它把空格编成 `+`。
       阿里云要 `%20`。签名当场就不对。

    2. **不能用 quote(s) 的默认参数**：默认 `safe='/'`，斜杠不会被编码。
       TemplateParam 是一段 JSON，里面出现斜杠（比如转义字符）就会签错。
       所以 safe 必须显式写成 '-_.~'。

    3. **不要照抄 Java / Node 版文档里那三行 replace**
       （`.replace("+","%20").replace("*","%2A").replace("%7E","~")`）。
       那是给 Java 的 URLEncoder 打的补丁——它做的是 form 编码，
       和 RFC3986 差着三处。Python 的 quote(s, safe='-_.~') 天然就是对的，
       再叠一层 replace 反而会把已经正确的结果改坏
       （比如把值里本来就有的 `+` 号错误地换成 `%20`）。

    Python 3.7+ 的 quote 已经把 `~` 放进「永远不编码」的集合，`*` 会被编成
    `%2A`，正好符合阿里云的要求，所以这里就是一行。
    """
    return quote(str(value), safe="-_.~", encoding="utf-8")


def _canonical_query(params: Dict[str, str]) -> str:
    """把参数排序 + 逐个编码，拼成 `k=v&k=v`。空值的参数直接不发。

    排序必须是**参数名的普通字典序**（Python 的 sorted 就是），
    因为服务端也是这么排的——顺序不一致，签出来的东西就不一样。

    值为空串的参数（比如没传 OutId）干脆不放进来：既省事，
    又避免「URL 里带了空参数、签名里没带」这类不一致。
    """
    items = [(k, v) for k, v in params.items() if v is not None and str(v) != ""]
    items.sort(key=lambda kv: kv[0])
    return "&".join(
        "%s=%s" % (percent_encode(k), percent_encode(v)) for k, v in items
    )


# ══════════════════════════════════════════════════════════════════════
#  V3 签名：ACS3-HMAC-SHA256
# ══════════════════════════════════════════════════════════════════════

_EMPTY_BODY_SHA256 = hashlib.sha256(b"").hexdigest()


def _utc_stamp() -> str:
    """签名用的时间戳，格式 2026-08-11T03:22:32Z。

    🔴 **必须是 UTC**，用 time.gmtime()，不许用 time.localtime()。
    这是国内开发最容易犯、也最难查的一个错：服务器时区是 UTC+8 的话，
    用本地时间会让时间戳超前 8 小时，直接被判过期（InvalidTimeStamp.Expired）。
    而且这个错在本机（同样是 UTC+8）稳定复现，非常容易被误判成签名算法写错了，
    然后对着签名代码查上半天。
    """
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def build_v3_request(
    config: SmsConfig,
    params: Dict[str, str],
    host: str,
    stamp: Optional[str] = None,
    nonce: Optional[str] = None,
) -> Tuple[str, Dict[str, str], str]:
    """按 V3 规则算出 (canonical_query, 请求头, 待签名串)。

    拆成一个纯函数是为了能对着它逐字节写测试——签名这种东西，
    「跑通了」和「算对了」是两回事，前者在假上游那里可能只是我们自己跟自己对上了。

    stamp / nonce 允许传进来，同样是为了测试能固定住输入。生产上一律不传。
    """
    canonical_query = _canonical_query(params)
    stamp = stamp or _utc_stamp()
    # 防重放随机数，每次请求都必须不同。用 uuid4().hex（32 位十六进制）而不是
    # 时间戳：并发时时间戳会撞，撞了就被当成重放请求拒掉。
    nonce = nonce or uuid.uuid4().hex

    # 参与签名的请求头：host + 所有 x-acs-*。
    # content-type 只要设了就必须参与签名，body 是空的时候干脆不设（少一处能出错的地方）。
    headers = {
        "host": host,
        "x-acs-action": _ACTION,
        "x-acs-version": _API_VERSION,
        "x-acs-date": stamp,
        "x-acs-signature-nonce": nonce,
        "x-acs-content-sha256": _EMPTY_BODY_SHA256,
    }
    names = sorted(headers.keys())
    # ⚠ 每一行都以 \n 结尾，**包括最后一行**；下面拼 canonical_request 时还会
    # 再加一个 \n，所以中间会出现一个空行。少一个换行就签不对，而且看不出来。
    canonical_headers = "".join("%s:%s\n" % (n, headers[n].strip()) for n in names)
    signed_headers = ";".join(names)

    canonical_request = "\n".join([
        "POST",              # 方法，全大写
        "/",                 # RPC 风格接口的路径恒为一个斜杠
        canonical_query,
        canonical_headers,   # 它自己已经以 \n 结尾，join 再补一个 → 空行
        signed_headers,
        _EMPTY_BODY_SHA256,  # body 的 sha256，末尾不加 \n
    ])
    hashed_request = hashlib.sha256(canonical_request.encode("utf-8")).hexdigest()
    string_to_sign = "ACS3-HMAC-SHA256\n" + hashed_request

    # ⚠ 和 V2 相反的两处：HMAC 的 key 就是 Secret 本身（不加 & 后缀），
    # 输出是小写十六进制（不是 Base64）。
    signature = hmac.new(
        config.access_key_secret.encode("utf-8"),
        string_to_sign.encode("utf-8"),
        hashlib.sha256,
    ).hexdigest()

    # 逗号两边没有空格，只有算法名和 Credential= 之间有一个空格。
    headers["authorization"] = (
        "ACS3-HMAC-SHA256 Credential=%s,SignedHeaders=%s,Signature=%s"
        % (config.access_key_id, signed_headers, signature)
    )
    # 让上游回 JSON 而不是 XML。accept 不参与签名（阿里云只要求 host + x-acs-*
    # 以及设了的 content-type 参与），发一个没签名的头是允许的。
    headers["accept"] = "application/json"
    return canonical_query, headers, canonical_request


# ══════════════════════════════════════════════════════════════════════
#  V2 签名：HMAC-SHA1（备用；官方标为「不推荐」但至今可用）
# ══════════════════════════════════════════════════════════════════════

def build_v2_query(
    config: SmsConfig,
    params: Dict[str, str],
    method: str = "POST",
    stamp: Optional[str] = None,
    nonce: Optional[str] = None,
) -> Tuple[str, str]:
    """按 V2 规则算出 (带 Signature 的完整查询串, 待签名串)。

    V2 的好处是完全不依赖请求头，所有东西都在 URL 里，用什么 HTTP 客户端都不会
    出岔子。坏处是有两个经典坑，就是下面标了 🔴 的那两行——
    「一直 403」的绝大多数案例都出在它们身上。
    """
    stamp = stamp or _utc_stamp()
    nonce = nonce or uuid.uuid4().hex
    full = dict(params)
    full.update({
        "Action": _ACTION,
        "Version": _API_VERSION,
        "AccessKeyId": config.access_key_id,
        # 不传 Format 的话默认返回 XML，我们解析 JSON，必须显式要 JSON
        "Format": "JSON",
        "SignatureMethod": "HMAC-SHA1",
        "SignatureVersion": "1.0",
        "SignatureNonce": nonce,
        "Timestamp": stamp,
    })
    canonical_query = _canonical_query(full)

    # 🔴 坑一：**整个规范化查询串要被再百分号编码一遍**。
    # 也就是说串里的 `%3A`（冒号）在这一步会变成 `%253A`。
    # 漏掉这一层是最常见的错误，而且看起来完全正常。
    string_to_sign = "%s&%s&%s" % (
        method.upper(), percent_encode("/"), percent_encode(canonical_query)
    )
    # 🔴 坑二：**HMAC 的 key 必须是 AccessKeySecret + "&"**，尾巴上那个 & 不能少。
    # 输出是 Base64（不是十六进制），这也和 V3 相反。
    signature = base64.b64encode(hmac.new(
        (config.access_key_secret + "&").encode("utf-8"),
        string_to_sign.encode("utf-8"),
        hashlib.sha1,
    ).digest()).decode("ascii")

    # Signature 本身不参与签名计算，算完之后当普通参数拼进 URL（同样要编码）。
    return canonical_query + "&Signature=" + percent_encode(signature), string_to_sign


# ══════════════════════════════════════════════════════════════════════
#  发送
# ══════════════════════════════════════════════════════════════════════

class SmsResult(NamedTuple):
    """一次发送的结果。调用方要用到的信息全在这里，不用再去解析什么。

    ⚠ **debug 和 channel 必须原样传下去**：
      * channel          直接写进 phone_verification_codes.channel；
      * debug=True       调用方**必须**在界面上显著标明「验证码没有真的发出去」
                         （现成文案 DEBUG_MODE_NOTICE），这是本期的硬要求；
      * code             只在调试模式下有值（要显示在页面上）。真实模式恒为空串——
                         真发出去的验证码**绝不能**回吐给前端，那等于把验证码
                         直接送给了任何一个知道手机号的人，整道验证形同虚设。
      * provider_msg_id  阿里云的 BizId，存进那一行。用户说「没收到」时，
                         拿它去阿里云控制台查投递状态，是唯一能分清
                         「我们没发」和「运营商没送到」的凭据。
      * message          给最终用户看的话（成功和失败都有，一律是中文人话）。
      * admin_hint       给管理员看的具体修复步骤，失败时才有值。
                         **不要**把它显示给公网上的注册用户（见错误表的注释）。
    """

    ok: bool
    debug: bool
    channel: str
    code: str = ""
    provider_msg_id: str = ""
    request_id: str = ""
    error_code: str = ""
    category: str = CATEGORY_OK
    retriable: bool = False
    message: str = ""
    admin_hint: str = ""


SUCCESS_MESSAGE = "验证码已发送，请注意查收短信。60 秒内没收到可以重新获取。"


def _debug_result(code: str) -> SmsResult:
    return SmsResult(
        ok=True, debug=True, channel=CHANNEL_DEBUG, code=code,
        category=CATEGORY_OK,
        message="调试模式：验证码是 %s（没有真的发短信）。" % code,
        admin_hint=DEBUG_MODE_NOTICE,
    )


def _failure(info: _ErrorInfo, error_code: str = "", request_id: str = "") -> SmsResult:
    return SmsResult(
        ok=False, debug=False, channel=CHANNEL_ALIYUN,
        request_id=request_id, error_code=error_code,
        category=info.category, retriable=info.retriable,
        message=info.message, admin_hint=info.admin_hint,
    )


def _split_endpoint(endpoint: str) -> Tuple[str, str]:
    """把设置里的域名拆成 (scheme, host[:port])。

    正常情况下设置里就是 `dysmsapi.aliyuncs.com`，走 https。
    允许写成 `http://127.0.0.1:9xxx` 是**专门为自动化测试留的**：
    测试要把接口指向本机的假上游，用来验证签名算法而不真的发短信、不花钱，
    而给假上游签一张 https 证书纯属自找麻烦。

    🔴 明文 http **只允许指向本机回环地址**。少了这道判断，
    一个填错（或被人改错）的设置就会让 AccessKey 和验证码明文过网络。
    """
    text = (endpoint or "").strip()
    scheme = "https"
    if text.lower().startswith("https://"):
        text = text[len("https://"):]
    elif text.lower().startswith("http://"):
        scheme = "http"
        text = text[len("http://"):]
    # 去掉路径和末尾斜杠：签名里的 CanonicalURI 恒为 "/"，域名后面不能带别的
    text = text.split("/", 1)[0].strip()
    if not text:
        raise ValueError("短信接口域名是空的，正常应该是 " + DEFAULT_ENDPOINT)
    if scheme == "http":
        host_only = text.rsplit(":", 1)[0] if text.count(":") == 1 else text
        host_only = host_only.strip("[]").lower()
        if host_only not in ("localhost", "127.0.0.1", "::1") and not host_only.startswith("127."):
            raise ValueError(
                "短信接口域名不能用不加密的 http（现在填的是 %s）。"
                "http 会让 AccessKey 和验证码明文过网络，请改成 https:// 开头，"
                "或者就填默认的 %s。" % (text, DEFAULT_ENDPOINT)
            )
    return scheme, text


def send_via_aliyun(
    config: SmsConfig,
    phone: str,
    code: str,
    out_id: str = "",
    signature_version: str = "v3",
    allow_signature_fallback: bool = True,
) -> SmsResult:
    """真的调阿里云发一条验证码短信。**这一步开始花钱。**

    参数里的 config 带着明文 AccessKey——它只在这个函数的调用栈里存在，
    不写日志、不放进返回值、不进异常消息。

    signature_version 选 'v3'（默认，阿里云现在推荐的）或 'v2'（旧版官方 SDK 用的）。
    allow_signature_fallback=True 时，V3 撞上 SignatureDoesNotMatch 会自动用 V2
    再试一次——那个错误是网关在校验签名那一步拒的，**可以确定没有发出任何短信、
    没有花任何钱**，所以重试它不违反「绝不自动重试」那条铁律（见文件头第四节）。
    """
    masked = mask_phone(phone)
    # 业务参数全部放 query（阿里云 RPC 风格接口，POST 的 body 是空的）。
    # TemplateParam 必须是**序列化好的 JSON 字符串**，不是 JSON 对象；
    # 键名要和模板里 ${...} 的变量名逐字一致。
    params = {
        "PhoneNumbers": phone,
        "SignName": config.sign_name,
        "TemplateCode": config.template_code,
        "TemplateParam": json.dumps({"code": code}, separators=(",", ":"), ensure_ascii=False),
    }
    if out_id:
        # 外部流水号，会原样出现在回执里。传我们自己那条验证码记录的 id，
        # 日后排查「到底发没发出去」时能把两边对上。
        params["OutId"] = str(out_id)[:64]

    try:
        scheme, host = _split_endpoint(config.endpoint)
    except ValueError as exc:
        logger.error("短信接口域名配置有问题：%s", exc)
        return _failure(_ErrorInfo(
            CATEGORY_CONFIG, False, _USER_SIDE_UNAVAILABLE, str(exc)))

    version = (signature_version or "v3").lower()
    result = _post_once(config, params, scheme, host, version, masked)
    if (
        not result.ok
        and result.error_code == "SignatureDoesNotMatch"
        and version == "v3"
        and allow_signature_fallback
    ):
        logger.warning(
            "阿里云拒绝了 V3 签名（SignatureDoesNotMatch），改用 V2 签名重试一次。"
            "这一步没有发出任何短信、没有产生费用。手机号 %s", masked)
        result = _post_once(config, params, scheme, host, "v2", masked)
        if result.ok:
            logger.warning(
                "V2 签名发送成功——说明这个端点不接受 V3 签名（不是 AccessKey 填错）。"
                "以后每一条短信都会多一次无用的 V3 尝试，建议在设置里加一项"
                "「签名版本」并固定成 V2。")
    return result


def _post_once(
    config: SmsConfig,
    params: Dict[str, str],
    scheme: str,
    host: str,
    version: str,
    masked_phone: str,
) -> SmsResult:
    """发一次请求并解析结果。**不重试**（重试的判断在上层，且只对签名错误）。"""
    if version == "v2":
        query, _sts = build_v2_query(config, params)
        headers = {"accept": "application/json", "host": host}
    else:
        query, headers, _cr = build_v3_request(config, params, host)

    # 🔴 URL 里的查询串和签名里用的那一串必须**逐字节一致**，所以这里直接拿
    # 算好的字符串拼 URL，绝不把参数交给 httpx 让它自己去拼（它的编码规则
    # 和阿里云要求的不一样，尤其是空格和星号）。
    url = "%s://%s/?%s" % (scheme, host, query)
    # host 头也要和签名里的那个字符串一致：httpx 自己补的 Host 在某些情况下
    # 会带上 :443，而签名里没有，那就会一直 SignatureDoesNotMatch。显式设死。
    headers["host"] = host

    timeout = httpx.Timeout(
        connect=_CONNECT_TIMEOUT, read=_READ_TIMEOUT,
        write=_READ_TIMEOUT, pool=_CONNECT_TIMEOUT,
    )
    try:
        with httpx.Client(timeout=timeout, follow_redirects=False) as client:
            # body 为空：业务参数全在 query 里（已用官方 SDK 源码确认，
            # SendSmsRequest 的每个 setter 用的都是 add_query_param）。
            resp = client.post(url, headers=headers, content=b"")
    except httpx.TimeoutException:
        # ⚠ 超时**不代表没发出去**：可能上游已经受理、只是回执没回到我们手上。
        # 所以既不能自动重试（会重复扣费），也不能当成功。老老实实告诉用户
        # 「可能没发出去，过一会儿自己再点一次」，把决定权交给他。
        logger.warning("调用阿里云短信接口超时（手机号 %s）。这条短信可能发出去了，"
                       "也可能没有——我们不会自动重发。", masked_phone)
        return _failure(_ErrorInfo(
            CATEGORY_NETWORK, True,
            "验证码发送超时了，可能没发出去。请过一会儿再点一次「获取验证码」；"
            "如果稍后收到了两条短信，用最新的那条即可。",
            "调用阿里云超时（连接 %.0f 秒 / 读取 %.0f 秒）。我们**故意不自动重试**："
            "这个接口不支持幂等，重试可能变成同一条短信发两次、扣两次钱。"
            "请检查这台服务器能不能访问外网。" % (_CONNECT_TIMEOUT, _READ_TIMEOUT),
        ))
    except httpx.HTTPError as exc:
        logger.warning("调用阿里云短信接口失败（手机号 %s）：%s",
                       masked_phone, type(exc).__name__)
        return _failure(_ErrorInfo(
            CATEGORY_NETWORK, True,
            "验证码发送失败（连不上短信服务）。请过一会儿再试一次。",
            "连不上阿里云短信接口（%s）。请检查这台服务器的网络和 DNS，"
            "以及设置里的「短信接口域名」有没有被改过（正常是 %s）。"
            % (type(exc).__name__, DEFAULT_ENDPOINT),
        ))

    return _parse_response(resp, masked_phone)


def _parse_response(resp: httpx.Response, masked_phone: str) -> SmsResult:
    """解析上游返回。**只认 body 里的 Code == "OK"，不看 HTTP 状态码。**

    为什么不看状态码：阿里云短信的业务错误（isv.* 那一族）到底返回 200 还是 4xx，
    官方文档没有给出对照表，社区里两种都有人遇到。而 Code 字段是官方文档明确
    定义、唯一有契约保证的成功标志。只认 Code 在两种情况下都正确，零风险。

    解析不出 JSON 一律当**失败**：网关返回 HTML 错误页、中间层截断、
    连了个假域名……这些情况下「没报错」绝不等于「发出去了」。
    """
    try:
        data = resp.json()
    except Exception:  # noqa: BLE001 —— json 解析会抛什么取决于底层库，一律接住
        data = None
    if not isinstance(data, dict):
        snippet = (resp.text or "")[:200].replace("\n", " ").replace("\r", " ")
        logger.error(
            "阿里云短信接口返回了看不懂的内容（HTTP %s，手机号 %s）：%s",
            resp.status_code, masked_phone, snippet)
        return _failure(_ErrorInfo(
            CATEGORY_NETWORK, True,
            "验证码发送失败，请稍后再试一次。",
            "上游返回的不是预期的 JSON（HTTP %s）。前 200 个字符：%s。"
            "常见原因是「短信接口域名」被填错了，或者中间有一层代理/防火墙"
            "把请求拦了下来并返回了自己的错误页。"
            % (resp.status_code, snippet or "(空)"),
        ))

    code = str(data.get("Code") or "")
    request_id = str(data.get("RequestId") or "")[:64]
    if code == "OK":
        biz_id = str(data.get("BizId") or "")[:64]
        # ⚠ Code == OK 只代表**短信平台受理了这次请求**，不代表用户已经收到。
        # 真正的投递结果要靠 BizId 事后去查，所以这个 id 一定要存下来。
        logger.info("阿里云已受理验证码短信（手机号 %s，回执号 %s）", masked_phone, biz_id or "-")
        return SmsResult(
            ok=True, debug=False, channel=CHANNEL_ALIYUN,
            provider_msg_id=biz_id, request_id=request_id,
            category=CATEGORY_OK, message=SUCCESS_MESSAGE,
        )

    upstream_message = str(data.get("Message") or "")
    info = classify(code, upstream_message)
    # 日志里带上错误码和 RequestId：找阿里云客服排查时必须提供 RequestId。
    # 手机号已脱敏，验证码和 AccessKey 一个字都没有。
    logger.error(
        "阿里云拒绝发送验证码：Code=%s Message=%s RequestId=%s 手机号=%s → %s",
        code or "(空)", upstream_message or "(空)", request_id or "-",
        masked_phone, info.admin_hint)
    return _failure(info, error_code=code, request_id=request_id)


def send_code(
    db: Session,
    phone: str,
    code: str,
    out_id: str = "",
    config: Optional[SmsConfig] = None,
) -> SmsResult:
    """把验证码送出去——按当前设置决定是真发还是只写日志。**这是主入口。**

    ⚠ 调用之前必须已经做完两件事（顺序也不能换）：
       1. 三层限速（同一手机号 / 同一 IP / 全站每日总量），走 services/ratelimit.py；
       2. 往 phone_verification_codes 插好那一行。
    先写行再发送的理由见 models.PhoneVerificationCode 的类文档第二节：
    反过来的话，一旦写库失败，钱花了、短信到了用户手机上，而系统这边查无此码，
    用户填了正确的验证码却被告知「验证码错误」——这种故障没有人能自查出来。

    **不抛异常**（除非调用方传了根本不能用的参数）：所有失败形态都用 SmsResult
    表达，调用方拿到的永远是一个能直接显示给用户的中文句子。
    发短信这条路上的失败太多样了（网络、余额、审核、黑名单……），
    要求每个调用方都去 try 一遍等于迟早漏一种，然后用户看到一个 500。
    """
    normalized = normalize_phone(phone)
    if not normalized:
        # 走到这里说明调用方没校验就传进来了。这不是用户的错，但话还是要说给用户听。
        return SmsResult(
            ok=False, debug=False, channel=CHANNEL_DEBUG,
            category=CATEGORY_PHONE, retriable=False,
            message="请填写 11 位中国大陆手机号（不要加空格、横线或国家代码）。",
            admin_hint="normalize_phone 没能把 %s 归一成 11 位号码。" % mask_phone(phone),
        )
    if not (code or "").isdigit():
        # 验证码类模板的变量只接受 [a-zA-Z0-9]，我们的约定更严：就是纯数字。
        # 这一条挡的是「以后有人改了生成规则却没改这里」，属于程序自身的错。
        return SmsResult(
            ok=False, debug=False, channel=CHANNEL_DEBUG,
            category=CATEGORY_BUG, retriable=False,
            message="验证码发送失败，请联系管理员。",
            admin_hint="验证码必须是纯数字（阿里云验证码模板的硬约束），"
                       "现在传进来的不是。这是程序 bug，请发给开发者。",
        )

    config = config or load_config(db)
    if config.provider != CHANNEL_ALIYUN:
        # ── 调试模式：不连任何外部服务 ──
        # 验证码进日志是**刻意的功能**，不是疏忽：这是「不发短信也能把流程走通」
        # 的唯一依据。用 warning 级别，让它在日志里显眼一点，免得哪天真的上了
        # 生产还停在调试模式没人发现。
        logger.warning(
            "【调试模式，短信没有真的发出去】手机号 %s 的验证码是 %s。"
            "要真的发短信，请到系统设置把短信通道改成阿里云。",
            mask_phone(normalized), code)
        return _debug_result(code)

    problems = config_problems(config)
    if problems:
        # 通道设成了 aliyun 但没配齐。正常路径上设置接口会拦住这个组合，
        # 能走到这里说明是直接改数据库改出来的，或者 .enc_key 丢了导致解不开 Secret。
        hint = "短信通道设成了阿里云，但配置没齐：" + "；".join(problems) + "。请到「系统设置」补齐。"
        logger.error("%s", hint)
        return _failure(_ErrorInfo(CATEGORY_CONFIG, False, _USER_SIDE_UNAVAILABLE, hint))

    return send_via_aliyun(config, normalized, code, out_id=out_id)


# ══════════════════════════════════════════════════════════════════════
#  设置页的「试发一条」
# ══════════════════════════════════════════════════════════════════════

def send_test_code(db: Session, phone: str) -> SmsResult:
    """管理员在设置页点「试发一条」时走这里：**真的发一条短信到指定号码**。

    存在的理由：阿里云那四项（AccessKey Id / Secret / 签名 / 模板 CODE）填得对不对，
    除了真发一条出去之外没有任何办法验证——签名、模板、账号三者的匹配关系
    只有上游知道。与其让第一个真实用户去当小白鼠，不如让管理员先花四分钱确认。

    ⚠ 三件事调用方必须做到：
      1. **按钮上要写清楚「会真的发一条短信、会产生费用」**。这个函数即使当前
         处在调试模式也照发不误——因为它的全部意义就是验证阿里云那条链路，
         而管理员通常正是在「还没切过去」的时候需要验证。
      2. **同样要限速**（走 services/ratelimit.py）。它花的是一模一样的钱，
         而这个接口在管理员后台，被人拿到 Cookie 一样能刷。
      3. 结果里的 admin_hint 是给管理员看的，可以完整显示在设置页上；
         message 那一句是给普通用户的，这里反而不必显示。
    """
    normalized = normalize_phone(phone)
    if not normalized:
        return SmsResult(
            ok=False, debug=False, channel=CHANNEL_ALIYUN,
            category=CATEGORY_PHONE, retriable=False,
            message="请填写 11 位中国大陆手机号。",
            admin_hint="试发的目标号码不合法，请填 11 位中国大陆手机号。",
        )

    config = load_config(db)
    problems = config_problems(config)
    if problems:
        # 还没配齐就先别发——发出去也是必然失败，白白让人以为是阿里云的问题。
        return _failure(_ErrorInfo(
            CATEGORY_CONFIG, False, _USER_SIDE_UNAVAILABLE,
            "还不能试发，配置没填齐：" + "；".join(problems) + "。填完保存后再试。",
        ))

    code = generate_code()
    logger.warning(
        "管理员发起短信试发：目标手机号 %s。**这会真的发出一条短信并产生费用。**",
        mask_phone(normalized))
    result = send_via_aliyun(config, normalized, code, out_id="test")
    if result.ok:
        # 试发成功的提示要说清楚「成功」的边界：受理 ≠ 送达。
        return result._replace(
            message="试发成功：阿里云已经受理这条短信（回执号 %s）。"
                    "请查看那台手机有没有真的收到——收到了就说明配置全对，"
                    "可以把短信通道切到阿里云了。" % (result.provider_msg_id or "无"),
            admin_hint="⚠ 「已受理」只代表阿里云收下了请求，不代表手机已经收到。"
                       "如果手机上没收到，拿回执号（BizId）到阿里云控制台的"
                       "「发送记录查询」里查这条短信的投递状态。",
        )
    return result
