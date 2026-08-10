"""凭据加密盒：把网关 API Key 这类「必须能还原成明文」的凭据加密后再落库。

为什么不是像登录密码那样做哈希：密码只需要「比对」，哈希就够了；但网关 Key 要
原样发给上游网关去调用，必须能解回明文，所以只能加密、不能哈希。

—— 这里有一条踩过就再也回不来的坑，先说清楚 ——

加密用的钥匙是**单独一个文件 data/.enc_key**，不是从 DESIGNKIT_SECRET_KEY 派生的。
SECRET_KEY 同时是登录令牌（JWT）和 webhook 的签名密钥，而「怀疑令牌泄露了就换掉
DESIGNKIT_SECRET_KEY」是随手就能做的应急动作——换完所有人重新登录一次，代价近乎为零。
如果加密钥匙是从它算出来的，那么某天一次例行的密钥轮换，就会让库里全部用户的网关
凭据**永久**解不开。现象是「所有人突然都不能生图了」，而数据库看起来完好无损、
日志里也没有任何异常，根因几乎不可能查出来。所以两把钥匙必须彻底分开：
SECRET_KEY 可以随时换，.enc_key 一旦丢了就等于所有用户的网关 Key 全部作废。

>>> 备份 NAS 数据时，data/.enc_key 和 data/.secret_key 这两个文件必须和数据库一起备份。<<<
最现实的事故是「NAS 重装后先恢复数据库、图片和其他文件稍后再说」——容器一启动就会
静默生成两把全新的钥匙，此后库里的密文谁也解不开，而且没有任何报错提示这件事。

密文长这样：v1:<base64( 12 字节随机 nonce + AES-256-GCM 密文和校验标签 )>

- 前缀 v1: 是留给「明文原地升级成密文」用的：老数据是明文、新写入的是密文，
  用 is_encrypted() 一看就能分辨，不需要停机做一次性全量转换。
  将来若要换算法，就加 v2: 前缀，解密时按前缀分派，老密文照样能读。
- 每次加密都重新随机取 nonce：同一把 Key 加密两次得到的密文完全不同，
  避免从「两个用户的密文一模一样」推断出他们填了同一把 Key。
- 选 GCM 而不是普通 AES：它自带完整性校验，密文被改动一个字节 decrypt 就会报错，
  不会悄悄解出一串垃圾明文再被当成 Key 发给网关。
"""
import base64
import os
from typing import Optional

from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.kdf.hkdf import HKDF

# 故意导入模块本身而不是 `from ..config import ENCRYPTION_KEY`：
# 后者在 import 那一刻就把值抄走了，测试里没法换一把钥匙来验证「换钥匙会怎样」。
from .. import config

# 密文前缀，同时也是版本号。改动下面任何一个常量或派生参数，
# 都会让已经写进库的存量密文解不开——真要改就加 v2 前缀，别改 v1 的含义。
_PREFIX = "v1:"
_NONCE_LEN = 12  # GCM 的标准 nonce 长度
_TAG_LEN = 16  # GCM 校验标签长度，用于判断密文长度是否合理
_KDF_INFO = b"designkit-secrets-box-v1"

_cipher_cache: Optional[AESGCM] = None


class SecretsBoxError(Exception):
    """加解密失败。

    注意：任何时候都不要把凭据明文或密文本身拼进异常消息里——
    异常消息会进日志、也可能被接口回吐给前端。
    """


def _cipher() -> AESGCM:
    """把 .enc_key 里的钥匙材料摊成 32 字节，做成 AES-256-GCM 的加密器。

    钥匙材料是一串随机文本（默认 token_urlsafe(48)），长度不一定正好 32 字节，
    所以用 HKDF 摊一次。HKDF 的参数（算法、长度、info 串）是 v1 格式的一部分，
    改了等于换钥匙，存量密文全部解不开。

    结果缓存在模块级变量里（钥匙一个进程内不会变）。测试里要换钥匙的话，
    改完 config.ENCRYPTION_KEY 记得把 _cipher_cache 置回 None。
    """
    global _cipher_cache
    if _cipher_cache is None:
        material = (config.ENCRYPTION_KEY or "").encode("utf-8")
        if not material:
            raise SecretsBoxError(
                "加密钥匙是空的：请检查环境变量 DESIGNKIT_ENCRYPTION_KEY 或 data/.enc_key 文件"
            )
        key = HKDF(
            algorithm=hashes.SHA256(),
            length=32,
            salt=None,
            info=_KDF_INFO,
        ).derive(material)
        _cipher_cache = AESGCM(key)
    return _cipher_cache


def _b64decode(text: str) -> bytes:
    """解 base64。

    我们写出去的是 urlsafe 变体（用 - _ 代替 + /），这里把标准变体也一并接住：
    万一将来有人换了编码方式，或者密文经过某个会替换字符的中转环节，
    老数据还能读得回来。
    """
    normalized = text.replace("-", "+").replace("_", "/")
    return base64.b64decode(normalized, validate=True)


def is_encrypted(s: Optional[str]) -> bool:
    """判断一个值是不是本系统的密文。

    用途是「明文 → 密文」的原地升级：读出来先问一句是不是密文，
    是就解密，不是就当明文原样用（并顺手加密写回）。

    除了看前缀，还额外验证 base64 能解开、长度够放下 nonce + 校验标签。
    这是为了压低误判概率：万一哪个网关的 Key 恰好以 "v1:" 开头，
    只看前缀就会把明文当密文去解，然后报一个莫名其妙的解密失败。
    """
    if not isinstance(s, str) or not s.startswith(_PREFIX):
        return False
    try:
        raw = _b64decode(s[len(_PREFIX):])
    except Exception:
        return False
    return len(raw) >= _NONCE_LEN + _TAG_LEN


def encrypt(plaintext: str) -> str:
    """加密，返回 v1:<base64> 形式的密文字符串，直接存数据库即可。

    空字符串也能加密（结果是一段合法密文），但调用方通常应该把「没填」
    存成 NULL 而不是加密后的空串——否则「这个用户配没配 Key」就得先解密才知道。
    """
    if not isinstance(plaintext, str):
        raise SecretsBoxError("只能加密字符串")
    nonce = os.urandom(_NONCE_LEN)
    body = _cipher().encrypt(nonce, plaintext.encode("utf-8"), None)
    return _PREFIX + base64.urlsafe_b64encode(nonce + body).decode("ascii")


def decrypt(token: str) -> str:
    """解密。密文被篡改、或者钥匙不对，一律抛 SecretsBoxError，绝不返回半截明文。"""
    if not is_encrypted(token):
        raise SecretsBoxError("这不是本系统的密文（缺少 v1: 前缀或格式不对）")
    raw = _b64decode(token[len(_PREFIX):])
    nonce, body = raw[:_NONCE_LEN], raw[_NONCE_LEN:]
    try:
        plain = _cipher().decrypt(nonce, body, None)
    except Exception:
        # 这里绝大多数情况不是「被攻击」而是「钥匙换了」，所以错误提示直接给出补救办法。
        raise SecretsBoxError(
            "凭据解密失败：最常见的原因是 data/.enc_key 换了一把——"
            "比如只恢复了数据库、没有一起恢复 data/.enc_key 文件，"
            "容器启动时就会自动生成一把新的。"
            "把原来的 .enc_key 放回 data 目录再重启即可；"
            "如果确实找不回来，只能请用户在设置里重新填一次网关 Key。"
        ) from None
    try:
        return plain.decode("utf-8")
    except UnicodeDecodeError:
        raise SecretsBoxError("解出来的内容不是合法文本，密文可能已损坏") from None
