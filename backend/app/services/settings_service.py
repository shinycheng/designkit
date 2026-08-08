"""运行时设置：存数据库、界面可改，.env / 默认值作后备。"""
from typing import Any, Dict

from sqlalchemy.orm import Session

from ..config import RUNTIME_DEFAULTS
from ..models import AppSetting

# 这些键涉及密钥，接口返回时要打码
SECRET_KEYS = {"openai_api_key"}


def get_all(db: Session) -> Dict[str, Any]:
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


def masked(values: Dict[str, Any]) -> Dict[str, Any]:
    out = dict(values)
    for key in SECRET_KEYS:
        raw = str(out.get(key) or "")
        if raw:
            out[key] = ("*" * 8 + raw[-4:]) if len(raw) > 4 else "*" * 8
        else:
            out[key] = ""
    return out
