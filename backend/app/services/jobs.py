"""创建生成任务的共用逻辑（网页端 / 对外 API 都走这里）。"""
from typing import Any, Dict, List, Optional

from fastapi import HTTPException
from sqlalchemy.orm import Session

from ..models import GenerationJob, PromptTemplate, Upload
from ..prompting import build_raw_prompt, render_template_prompt
from . import inspiration, settings_service, sizing

MAX_INPUT_IMAGES = 4
MAX_N = 4
ALLOWED_QUALITIES = {"auto", "low", "medium", "high"}
# 尺寸不再用白名单枚举，改由 sizing.validate_size 按护栏规则判定（见该模块注释）。
# 保留这个名字是因为对外 API 文档和前端预设都引用它，现在它只是「推荐选项」。
ALLOWED_SIZES = {p["value"] for p in sizing.PRESETS}


def create_job(
    db: Session,
    *,
    source: str,
    user_id: Optional[int] = None,
    api_key_id: Optional[int] = None,
    template_id: Optional[int] = None,
    category_slug: Optional[str] = None,
    prompt: Optional[str] = None,
    variables: Optional[Dict[str, Any]] = None,
    extra_instructions: Optional[str] = None,
    input_paths: Optional[List[str]] = None,
    n: Optional[int] = None,
    size: Optional[str] = None,
    quality: Optional[str] = None,
    callback_url: Optional[str] = None,
    external_ref: Optional[str] = None,
) -> GenerationJob:
    input_paths = list(input_paths or [])
    if len(input_paths) > MAX_INPUT_IMAGES:
        raise HTTPException(status_code=422, detail="最多支持 %d 张商品图" % MAX_INPUT_IMAGES)

    # 按分类生成：不选具体模板，只选一个分类。真正的提示词在 worker 里由
    # 文本模型现场合成——它会看商品图，再从该分类的语料里挑几条当风格参考。
    # 这样 1.4 万条提示词全部可用，而不必先一条条「采用为模板」。
    category_name = ""
    if category_slug is not None:
        category_name = inspiration.CATEGORIES.get(category_slug) or ""
        if not category_name:
            raise HTTPException(status_code=422, detail="分类不存在：%s" % category_slug)
        if template_id is not None or prompt:
            raise HTTPException(
                status_code=422, detail="分类、模板、自定义提示词三者只能选其一"
            )

    template: Optional[PromptTemplate] = None
    if template_id is not None:
        template = db.get(PromptTemplate, template_id)
        if template is None or not template.is_enabled:
            raise HTTPException(status_code=404, detail="提示词模板不存在或已停用")
        # 模板归属校验。owner_user_id 为 NULL = 平台公共库，谁都能用；
        # 现阶段所有模板（含灵感库 1.4 万条）都是 NULL，所以这行**当前不改变任何行为**。
        # 提前写在这里，是因为 models.py 加 owner_user_id 那处注释已经承诺
        # 「归属校验在应用层做（services/jobs.py 取模板那一处）」——
        # 等 P2 真给模板分主人时，如果这一处是空白，就会出现「别人的私有模板
        # 换个 id 就能用」，而那时谁也想不起来该补在哪。
        if template.owner_user_id not in (None, user_id):
            raise HTTPException(status_code=404, detail="提示词模板不存在或已停用")

    if category_slug is not None:
        if not input_paths:
            raise HTTPException(status_code=422, detail="按分类生成需要先上传商品图")
        # prompt_final 只是「配置产物」的快照；按分类走时它记录用户的补充要求，
        # 合成失败时也能拿它兜底出图
        final_prompt = build_raw_prompt(
            "Professional e-commerce product photography of the uploaded product.",
            extra_instructions,
        )
    elif template is not None:
        if template.requires_input_image and not input_paths:
            raise HTTPException(status_code=422, detail="该模板需要先上传商品图")
        final_prompt = render_template_prompt(template, variables, extra_instructions)
    else:
        final_prompt = build_raw_prompt(prompt or "", extra_instructions)
    if not final_prompt:
        raise HTTPException(status_code=422, detail="请选择分类或填写提示词")

    defaults = (template.default_params if template is not None else None) or {}
    settings = settings_service.get_all(db)

    n_val = n if n is not None else defaults.get("n") or settings.get("default_n") or 1
    try:
        n_val = int(n_val)
    except (TypeError, ValueError):
        n_val = 1
    n_val = max(1, min(MAX_N, n_val))

    # 先归一化再校验并落库：validate_size 内部会 strip/lower，但落库的是这个变量，
    # "1024X1024" 或带空格的值能过校验、却在几分钟后被网关 400 拒掉还白耗一次重试
    size_val = sizing.normalize_size(
        size or defaults.get("size") or settings.get("default_size") or "1024x1024"
    )
    size_error = sizing.validate_size(size_val)
    if size_error:
        # 非法尺寸必须在这里同步 422 拒绝，不能 202 受理后异步失败——
        # 后者会白白占掉配额和重试次数，用户还要等几分钟才看到错
        raise HTTPException(status_code=422, detail="尺寸不可用：%s" % size_error)
    quality_val = quality or defaults.get("quality")
    if quality_val and quality_val not in ALLOWED_QUALITIES:
        raise HTTPException(
            status_code=422, detail="质量参数只支持 auto / low / medium / high"
        )

    if callback_url:
        cb = str(callback_url).strip()
        if not (cb.startswith("http://") or cb.startswith("https://")):
            raise HTTPException(status_code=422, detail="回调地址必须是 http(s) 链接")
        if len(cb) > 512:  # 列宽 512，PostgreSQL 会直接拒绝超长值
            raise HTTPException(status_code=422, detail="回调地址过长（最多 512 个字符）")
        callback_url = cb

    job = GenerationJob(
        source=source,
        user_id=user_id,
        api_key_id=api_key_id,
        template_id=template.id if template is not None else None,
        template_name=template.name if template is not None else category_name,
        prompt_final=final_prompt,
        params={
            "n": n_val, "size": size_val, "quality": quality_val or "auto",
            **({"category_slug": category_slug} if category_slug else {}),
        },
        input_paths=input_paths,
        status="pending",
        callback_url=callback_url,
        external_ref=(external_ref or None),
    )
    db.add(job)
    db.commit()
    db.refresh(job)
    return job


def resolve_upload_paths(
    db: Session,
    upload_ids: List[int],
    *,
    user_id: Optional[int] = None,
    api_key_id: Optional[int] = None,
) -> List[str]:
    """把上传记录 id 换成文件路径；校验归属，避免拿别人的图。

    调用方**必须**说明自己是谁（网页端传 user_id，ERP 传 api_key_id）。
    """
    # fail-closed：两个归属参数都没传就直接拒，不是「没归属所以全放行」。
    # 以前下面两条判断都以「调用方传了参数」为前提，都为 None 时循环里
    # 一路 append 不做任何校验。今天的两个调用点（routers/generations.py 的
    # 建任务、routers/v1.py 的 ERP 建任务）都传了，所以还利用不了；
    # 但只要多用户跑起来，代客生成、批量任务、后台补图这类新调用方一定会加，
    # 谁漏传一次，就等于任何人可以拿任何 upload_id 建任务、把别人的商品图
    # 当输入图使用。这种「忘了传参 = 静默关掉鉴权」的写法必须从函数入口堵死。
    if user_id is None and api_key_id is None:
        raise HTTPException(status_code=403, detail="缺少调用方归属")

    paths: List[str] = []
    for uid in upload_ids:
        upload = db.get(Upload, int(uid))
        if upload is None:
            raise HTTPException(status_code=404, detail="上传的图片不存在（id=%s）" % uid)
        # 越权统一返回 404（与任务查询一致，不泄露资源是否存在）
        if api_key_id is not None and upload.api_key_id != api_key_id:
            raise HTTPException(status_code=404, detail="上传的图片不存在（id=%s）" % uid)
        if api_key_id is None and user_id is not None and upload.user_id != user_id:
            raise HTTPException(status_code=404, detail="上传的图片不存在（id=%s）" % uid)
        paths.append(upload.path)
    return paths
