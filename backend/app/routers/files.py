"""图片访问路由：/files/<相对路径>

取代了原先直接挂在 /files/uploads、/files/outputs、/files/thumbnails 上的三个
静态目录。那三个目录**一点鉴权都没有**，知道地址就能下载任何人的商品图和出图
结果；而地址除了任务号之外全是可枚举的，任务号又在接口返回、给 ERP 的回调、
反向代理的访问日志里到处都是。详细的来龙去脉写在 services/file_access.py 的
文件头，这里不重复。

这条路由干四件事，顺序不能换：

  1. 用白名单正则校验路径形状，不匹配一律 404；
  2. 校验凭证本身有没有效（ERP 签名参数 / 网页端 Cookie / 都没有），
     这一步**只看凭证**，一次库都不为「这张图」查；
  3. 用**同一个**已通过校验的字符串反查这张图属于谁，再判断这个凭证配不配；
  4. 交给 starlette 的 StaticFiles 去读文件（为了保住 304 和 Range）。

第 3 步和第 4 步用的是同一个字符串，中间不做任何拼接或归一化——
这是防路径穿越的关键，理由见 file_access.SAFE_PATH_RE 上面那段注释。

**第 2 步为什么一定要排在第 3 步前面**（这是后来修掉的一个真实缺陷）：
原先的顺序是「先反查归属、查不到就 404，再判凭证、没凭证就 403」，
于是一个完全不带凭证的人只要看状态码就能分辨两件事——
403 = 这个任务号在系统里真实存在，404 = 不存在。任务号在反向代理的访问日志、
转发出去的图片地址、ERP 库里存的老链接里到处都是，捡一个回来打一发就能确认
「这一单确实是这家做的」。这正是 services/file_access.py 的 classify() 上面
写着的那条规矩（「不要返回 403 之类更明确的错误：那等于告诉对方『你猜的
目录是对的』」）被自己破掉的地方。

现在的规矩是：**凭证不成立时，无论文件在不在，回的都是同一句 403。**
只有凭证成立之后，才轮得到 200 / 404 的区分，而那时对方已经是站内可信身份，
本来就该分得清「不是你的图」和「图没了」。
注意第 1 步的形状校验**不能**跟着往后挪：它不查库、不看任何数据，
只判断这串路径长得对不对，泄露不了任何东西；而
tests/test_file_access.py 与 tests/e2e_core.py 里
「/files/designkit.db 与 /files/.secret_key 必须 404」的断言正是靠它。
"""
import stat as stat_module
from typing import NamedTuple, Optional

from fastapi import APIRouter, Depends, HTTPException, Query, Request
from fastapi.staticfiles import StaticFiles
from sqlalchemy.orm import Session

from ..config import DATA_DIR
from ..deps import get_db
from ..models import ApiKey, GenerationJob, PromptTemplate, Upload, User
from ..services import file_access, settings_service

router = APIRouter(prefix="/files", tags=["文件"])


# 复用 starlette 的 StaticFiles 来真正吐文件，而不是自己 new 一个 FileResponse。
#
# 原因是 304：StaticFiles.file_response() 内部会比对请求里的 If-None-Match /
# If-Modified-Since，命中就返回 304 空响应；裸的 FileResponse 只会把 ETag 和
# Last-Modified 设上，**从来不返回 304**——浏览器带着 If-None-Match 回来，
# 拿到的还是 200 加一整张图的字节。表现出来就是历史页每打开一次都要把满屏
# 几十张缩略图全量重传一遍，在 NAS 的家用上行带宽下会被直接感受成「系统变卡了」，
# 而且这种卡跟「鉴权」这次改动的关联，事后没有人能猜得到。
# 顺带还白拿了 Range（断点续传/大图拖动）和 content-type 推断。
#
# check_dir=False：目录是 config.py 启动时建好的，这里不需要再校验一次；
# 万一真不存在，走到 lookup_path 也只是返回 404，不该在 import 阶段就炸掉进程。
_responder = StaticFiles(directory=str(DATA_DIR), check_dir=False)


class _Owner(NamedTuple):
    """这张图属于谁。

    kind:
      "job"    —— 生成任务的产物（出图 / 缩略图）
      "upload" —— 用户上传的商品图
      "shared" —— 提示词模板的封面图，全站共用，谁都能看
    user_id / api_key_id 可能是 None（历史数据、或者本来就没有归属方）。
    """

    kind: str
    user_id: Optional[int]
    api_key_id: Optional[int]


def _resolve_owner(db: Session, kind: str, rel_path: str) -> Optional[_Owner]:
    """反查归属。查不到就返回 None，调用方按 404 处理。"""
    if kind in ("outputs", "thumbnails"):
        job_id = file_access.job_id_of(kind, rel_path)
        job = db.get(GenerationJob, job_id)  # 主键查询，代价可以忽略
        if job is None:
            return None
        return _Owner("job", job.user_id, job.api_key_id)

    # uploads/ 目录下有两类东西，第二类特别容易漏，漏了会出大事：
    #   a) 用户上传的商品图 —— uploads 表里有对应记录；
    #   b) 提示词模板的封面 —— routers/templates.py 的上传封面接口走的也是
    #      storage.save_upload()，文件同样落在 uploads/ 下，但**不会**在
    #      uploads 表里留一行。要是这里不做特判，改完鉴权之后全站模板封面
    #      会对所有人 404，而「模板封面不见了」和「图片鉴权」之间的关系，
    #      光看现象根本联想不到。
    up = db.query(Upload).filter(Upload.path == rel_path).first()
    if up is not None:
        return _Owner("upload", up.user_id, up.api_key_id)
    tpl = (
        db.query(PromptTemplate)
        .filter(PromptTemplate.thumbnail_path == rel_path)
        .first()
    )
    if tpl is not None:
        return _Owner("shared", None, None)
    return None


class _Credential(NamedTuple):
    """这次请求带的是什么身份。**只描述凭证本身，与「要哪张图」无关。**

    kind:
      "erp"  —— 地址上的 ?k=&e=&t= 签名参数验过了，api_key_id 是签名里那把 Key
      "web"  —— dk_files Cookie 验过了（含查库确认账号还在、没停用、令牌版本没变）
      "open" —— 没有任何凭证，但管理员把 files_signed_only 关掉了，所以照样放行
    """

    kind: str
    api_key_id: Optional[int]
    user: Optional[User]


def _verify_erp_signature(
    db: Session, rel_path: str, key_id: Optional[str], exp: Optional[str], sig: str,
) -> _Credential:
    """校验 ERP 签名链接**这张凭证本身**。通不过就抛 403。

    刻意不接收 owner 参数：这里一行都不许查「这张图属于谁」，
    否则又会变回「先看文件在不在、再看凭证」的顺序，把存在性漏出去。
    """
    try:
        k = int(key_id)
        e = int(exp)
    except (TypeError, ValueError):
        # 参数不是数字：只可能是被人改过或截断了。当成签名无效，
        # 不要让 FastAPI 去做类型校验返回 422——那会暴露参数的含义。
        raise HTTPException(status_code=403, detail="图片链接的签名无效")

    if not file_access.verify_erp(rel_path, k, e, sig):
        raise HTTPException(status_code=403, detail="图片链接的签名无效")
    if file_access.is_expired(e):
        raise HTTPException(
            status_code=403,
            detail="图片链接已过期，请重新调用接口获取最新的图片地址",
        )

    key = db.get(ApiKey, k)
    if key is None or not key.is_active:
        # 这一步就是「绑定身份」的兑现之处：Key 一停用，它此前发出去的所有
        # 图片链接立刻全部打不开，不用等签名自然过期。
        raise HTTPException(status_code=403, detail="这把 API Key 已停用，图片链接同时失效")

    return _Credential("erp", k, None)


def _verify_web_cookie(db: Session, cookie_value: Optional[str]) -> Optional[_Credential]:
    """校验网页端 dk_files Cookie**这张凭证本身**。不成立就返回 None。

    同样刻意不接收 owner：能不能看这张图是下一步的事。
    """
    claims = file_access.parse_web_cookie(cookie_value)
    if not claims:
        return None
    user = db.get(User, claims["user_id"])
    if user is None or not user.is_active:
        return None
    # 令牌版本必须一致。这一条让 deps.py 里那套「改密码 / 停用账号就让旧登录
    # 立刻失效」的机制**第一次覆盖到图片**：以前撤销做得再认真，/files 上也是
    # 断的，人被停用之后他手上的图片地址照样能打开。
    if int(claims.get("ver", 0)) != int(user.token_version or 0):
        return None
    return _Credential("web", None, user)


def _owner_allows(cred: _Credential, owner: _Owner) -> bool:
    """凭证已经成立，再看这张图配不配给他。返回 False 由调用方按 404 处理。"""
    if owner.kind == "shared":
        return True  # 模板封面是公共素材，登录了就能看

    if cred.kind == "erp":
        # 签名有效但这张图不是这把 Key 的（签名是我们自己签发的，正常不会走到
        # 这里，除非数据被改过）。
        return owner.api_key_id is not None and owner.api_key_id == cred.api_key_id

    if cred.kind == "web":
        user = cred.user
        if owner.user_id is not None and owner.user_id == user.id:
            return True
        # 管理员能看所有人的图，是因为日常要帮成员排障（「我这张图怎么糊的」
        # 这类问题，看不到图就没法回答）。同时也覆盖了 owner.user_id 为 None
        # 的历史数据——那些图除了管理员没人认领得了。
        return user.role == "admin"

    # cred.kind == "open"：files_signed_only 被管理员关掉了，这时候本来就是
    # 「谁都能取」的状态，再判归属没有意义。
    return True


@router.get("/{rel_path:path}", summary="获取图片文件")
def get_file(
    rel_path: str,
    request: Request,
    k: Optional[str] = Query(None, description="对外 API：签名链接里的 API Key 编号"),
    e: Optional[str] = Query(None, description="对外 API：签名链接的到期时间戳（秒）"),
    t: Optional[str] = Query(None, description="对外 API：签名"),
    db: Session = Depends(get_db),
):
    """按凭证返回图片。网页端靠登录时种下的 dk_files Cookie，
    对外 API 靠地址里的 ?k=&e=&t= 签名参数。"""
    # ① 形状校验。放在最前面，且不匹配一律 404——数据库文件、密钥文件、
    #    任何带 .. 的构造都在这一步就被挡掉（tests/e2e_core.py 里
    #    /files/designkit.db 与 /files/.secret_key 必须 404 的断言靠的就是它）。
    kind = file_access.classify(rel_path)
    if kind is None:
        raise HTTPException(status_code=404, detail="文件不存在")

    # ② 凭证判断。**必须排在反查归属之前**，理由见文件头那段说明：
    #    凭证不成立时不许再往下走一步，否则「图在 / 图不在」会从状态码上漏出去。
    if t:
        # 带了签名参数 → 走 ERP 分支，不再回落到 Cookie 或未签名分支：
        # 签名对不上说明有人在动手脚，这时候「悄悄换一种方式放行」是最坏的选择。
        cred = _verify_erp_signature(db, rel_path, k, e, t)
    else:
        cred = _verify_web_cookie(db, request.cookies.get(file_access.WEB_COOKIE_NAME))
        if cred is None:
            # 既没有签名、Cookie 也无效/过期/不存在。
            # files_signed_only 默认 True（=没凭证就不给），因为目前没有任何
            # ERP 对接方，不存在「老链接会被打断」这回事。保留这个开关是给
            # 将来真接了对接方时临时放宽用的，放宽期间靠下面这条 WARNING 日志
            # 和计数器盯着还有多少人在用没凭证的老地址。
            signed_only = bool(settings_service.get(db, "files_signed_only"))
            file_access.note_unsigned_access(
                rel_path, request.headers.get("referer", ""), blocked=signed_only
            )
            if signed_only:
                # 这一句是**匿名侧唯一的出口**：图在不在、任务号真不真，
                # 从这里一个字都看不出来。别在这里按情况改成 404，
                # 那就等于把存在性又送回去了。
                raise HTTPException(
                    status_code=403,
                    detail="图片需要登录后查看；如果你正打开的是很久以前保存的链接，请重新登录本系统再试",
                )
            cred = _Credential("open", None, None)

    # ③ 凭证已经成立，才轮到用同一个字符串反查归属、判断这张图配不配给他。
    #    到这一步对方已经是站内可信身份，200 / 404 的区分不再泄露任何东西。
    owner = _resolve_owner(db, kind, rel_path)
    if owner is None:
        raise HTTPException(status_code=404, detail="文件不存在")
    if not _owner_allows(cred, owner):
        # 404 而不是 403：不向已登录的别人确认「这张图存在」。
        raise HTTPException(status_code=404, detail="文件不存在")

    # ④ 取文件。仍然是同一个 rel_path，中间没有做过任何改写。
    full_path, stat_result = _responder.lookup_path(rel_path)
    if stat_result is None or not stat_module.S_ISREG(stat_result.st_mode):
        # 数据库里有记录、磁盘上文件没了（被清理过、或迁移时漏拷）也走这里
        raise HTTPException(status_code=404, detail="文件不存在")

    response = _responder.file_response(full_path, stat_result, request.scope)
    # private：只允许浏览器自己缓存，不允许中间的反向代理/CDN 存一份——
    #          否则同一个地址的图有可能被缓存后发给下一个没有凭证的人，
    #          等于绕过刚做完的这套鉴权。
    # 300 秒：短到改了图很快能看到新的，长到一次翻页里的重复请求不用反复回源。
    response.headers["Cache-Control"] = "private, max-age=300"
    # 不让浏览器去猜文件类型：万一有一天混进来一个能被当成 HTML 解析的文件，
    # 猜类型会把它变成同源下的一段脚本。
    response.headers["X-Content-Type-Options"] = "nosniff"
    # 图片地址里带着任务号，不要顺着外链把它带到第三方站点的日志里去。
    response.headers["Referrer-Policy"] = "no-referrer"
    return response
