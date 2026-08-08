import hashlib
import hmac
import secrets
from datetime import datetime, timedelta
from typing import Optional

import jwt

from .config import SECRET_KEY, TOKEN_EXPIRE_DAYS

_PBKDF2_ITERATIONS = 200_000


def hash_password(password: str) -> str:
    salt = secrets.token_hex(16)
    digest = hashlib.pbkdf2_hmac(
        "sha256", password.encode("utf-8"), bytes.fromhex(salt), _PBKDF2_ITERATIONS
    ).hex()
    return "pbkdf2_sha256$%d$%s$%s" % (_PBKDF2_ITERATIONS, salt, digest)


def verify_password(password: str, stored: str) -> bool:
    try:
        _algo, iterations, salt, digest = stored.split("$")
        computed = hashlib.pbkdf2_hmac(
            "sha256", password.encode("utf-8"), bytes.fromhex(salt), int(iterations)
        ).hex()
        return hmac.compare_digest(computed, digest)
    except (ValueError, TypeError):
        return False


def create_token(user_id: int, token_version: int = 0) -> str:
    payload = {
        "sub": str(user_id),
        "ver": int(token_version),
        "exp": datetime.utcnow() + timedelta(days=TOKEN_EXPIRE_DAYS),
        "iat": datetime.utcnow(),
    }
    return jwt.encode(payload, SECRET_KEY, algorithm="HS256")


def decode_token(token: str) -> Optional[dict]:
    """返回 {"user_id", "ver"}，失败返回 None。"""
    try:
        payload = jwt.decode(token, SECRET_KEY, algorithms=["HS256"])
        return {"user_id": int(payload["sub"]), "ver": int(payload.get("ver", 0))}
    except (jwt.PyJWTError, KeyError, ValueError):
        return None


def new_api_key() -> str:
    return "dk_" + secrets.token_urlsafe(30)


def hash_api_key(key: str) -> str:
    return hashlib.sha256(key.encode("utf-8")).hexdigest()


def new_webhook_secret() -> str:
    return "whsec_" + secrets.token_urlsafe(24)


def sign_webhook(secret: str, body: bytes) -> str:
    """对回调请求体做 HMAC-SHA256 签名，ERP 侧用同样算法校验来源。"""
    return hmac.new(secret.encode("utf-8"), body, hashlib.sha256).hexdigest()
