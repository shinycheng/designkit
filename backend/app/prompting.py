"""提示词渲染：把模板里的 {变量} 替换成用户填写的值。"""
import re
from typing import Any, Dict, List, Optional

from fastapi import HTTPException

from .models import PromptTemplate

_VAR_PATTERN = re.compile(r"\{([a-zA-Z0-9_一-鿿]+)\}")


def render_template_prompt(
    template: PromptTemplate,
    variables: Optional[Dict[str, Any]],
    extra_instructions: Optional[str],
) -> str:
    variables = variables or {}
    defined: List[dict] = list(template.variables or [])
    values: Dict[str, str] = {}
    for var in defined:
        name = str(var.get("name") or "").strip()
        if not name:
            continue
        raw = variables.get(name)
        value = str(raw).strip() if raw is not None else ""
        if not value:
            value = str(var.get("default") or "")
        if not value and var.get("required"):
            raise HTTPException(
                status_code=422,
                detail="缺少必填变量：%s" % (var.get("label") or name),
            )
        values[name] = value

    def _sub(match: "re.Match") -> str:
        key = match.group(1)
        if key in values:
            return values[key]
        # 未在模板变量里定义、但调用方直接传了值的，也允许替换
        if key in variables and variables[key] is not None:
            return str(variables[key])
        return match.group(0)  # 无值则原样保留，方便排查

    prompt = _VAR_PATTERN.sub(_sub, template.prompt_template or "")
    if extra_instructions and extra_instructions.strip():
        prompt = prompt.rstrip() + "\n" + extra_instructions.strip()
    return prompt.strip()


def build_raw_prompt(prompt: str, extra_instructions: Optional[str]) -> str:
    prompt = (prompt or "").strip()
    if extra_instructions and extra_instructions.strip():
        prompt = prompt + "\n" + extra_instructions.strip()
    return prompt.strip()
