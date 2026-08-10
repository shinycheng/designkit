import logging
from contextlib import asynccontextmanager
from typing import List

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.staticfiles import StaticFiles
from starlette.datastructures import MutableHeaders
from starlette.responses import Response


class RevalidateStaticFiles(StaticFiles):
    """前端静态资源加 Cache-Control: no-cache。

    no-cache 不是「不缓存」，而是「每次都用 ETag 重新校验」：没变返回 304 很省流量，
    变了立刻拿到新版本。否则无 Cache-Control 时浏览器会启发式缓存旧 JS，
    重新部署后用户可能因缓存的旧模块白屏。
    """

    def file_response(self, *args, **kwargs) -> Response:
        response = super().file_response(*args, **kwargs)
        response.headers["Cache-Control"] = "no-cache"
        return response


class SecurityHeadersMiddleware:
    """给所有响应补一条 Referrer-Policy: no-referrer。

    图片地址里带着任务号（outputs/年月/<任务号>/0.png），页面地址里也可能带业务参数。
    浏览器默认会把「来源页地址」跟着外链一起发给第三方站点——只要页面上有一个指向
    外部的链接或图片，任务号就跟着进了别人家的访问日志。这正是当初「知道地址就能
    下图」那条链路的传播途径之一，所以整站关掉。

    写成裸 ASGI 中间件而不是 @app.middleware("http")：后者用的是 BaseHTTPMiddleware，
    它会把响应体整个接管一遍，对 StaticFiles 的 Range / 304 这类响应是额外风险，
    而我们只想改一个响应头。这里只包一层 send，字节流原样穿过。

    用 setdefault 不用直接赋值：routers/files.py 已经自己设过这个头，
    路由层写的更具体，不该被中间件覆盖掉。
    """

    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return

        async def send_wrapper(message):
            if message["type"] == "http.response.start":
                MutableHeaders(scope=message).setdefault("referrer-policy", "no-referrer")
            await send(message)

        await self.app(scope, receive, send_wrapper)


from .backfill import run_backfill
from .config import FRONTEND_DIR, RUNTIME_DEFAULTS
from .database import SessionLocal, engine
from .migrations import run_schema_upgrade
from .routers import (
    account, apikeys, auth, files, generations, inspiration, settings_router,
    templates, uploads, users, v1,
)
from .seed import seed
from .services import inspiration as inspiration_service
from .services import settings_service
from .services.scheduler import scheduler
from .services.worker import worker

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
)
logger = logging.getLogger("designkit.startup")


@asynccontextmanager
async def lifespan(_app: FastAPI):
    # 建缺失的表 + 给老数据库补新列补索引。这三件事必须由同一个函数在**同一把
    # PostgreSQL 咨询锁**里做完，所以这里只调这一个入口，不要再自己 create_all。
    # 拆开的话（老写法是先 create_all 再 run_migrations），建表那一步就落在锁外面：
    # 多进程同时启动时会有进程以 relation "xxx" already exists 崩掉，
    # 而加锁本来就是为了防这个。详见 migrations.py 文件头「纪律四」。
    run_schema_upgrade(engine)
    db = SessionLocal()
    try:
        seed(db)
        # 给历史数据补上「这条记录属于谁」。必须排在 seed 之后：全新安装时，
        # 存量数据要归到的那个管理员账号正是 seed 刚建出来的。
        #
        # 不做的话，升级当天所有历史 ERP 任务和图片的 user_id 都是空的，
        # 而「只看得到自己的东西」靠的就是这一列，SQL 里 NULL 不等于任何值——
        # 表现是「数据一条没丢、图也都在，但网页上再也翻不到了」。
        #
        # 失败**不挡启动**：挡死会让容器在 restart: always 下无限重启，
        # 运营同学连界面都进不去，更没辙。失败的事实由 run_backfill 自己写进
        # app_settings，管理员在设置页会看到红色告警（见 backfill.py 文件头）。
        # 这里再套一层 try 是纵深防御——run_backfill 承诺不抛异常，但真抛了
        # 也不能连累启动。
        try:
            if not run_backfill(db):
                logger.error("历史数据归属回填未完成，管理员在设置页会看到告警横幅")
        except Exception:
            logger.exception("历史数据归属回填异常（不挡启动，但历史记录可能在网页上看不到）")
    finally:
        db.close()
    # 老库升级后用本地缓存回填灵感库的分类标签（不联网）。不做的话，
    # 在下次同步之前生成页会一个分类都显示不出来，看起来像坏了。
    try:
        inspiration_service.backfill_slugs_if_needed()
    except Exception:
        logger.exception("回填灵感库分类标签失败（不影响启动，可手动同步一次）")
    worker.start()
    scheduler.start()
    yield
    scheduler.stop()
    worker.stop()


app = FastAPI(
    title="DesignKit · AI 商品图生成平台",
    lifespan=lifespan,
    description=(
        "内部使用的 AI 商品图生成平台。\n\n"
        "「对外API-ERP对接」分组下的接口供第三方系统（ERP 等）调用，"
        "鉴权方式为请求头 X-API-Key。详细对接说明见项目 docs/erp-api.md。"
    ),
    version="0.1.0",
)


def _load_allowed_origins() -> List[str]:
    """启动时读一次「允许哪些网站的网页调用本系统接口」（CORS 白名单）。

    ⚠️ **改了这项设置要重启服务才生效**，README 的设置说明里写清楚了。
    原因是 CORS 中间件在应用装配的那一刻就把白名单抄进自己的实例里了，
    而设置存在数据库里。要做到「改完立刻生效」，就得自己实现一遍
    预检请求（OPTIONS）、Vary、各种响应头的组合逻辑——CORS 是那种
    「自己写一遍必踩坑、踩了还不报错只是悄悄放行」的东西，
    为了省一次重启去手写它不划算。

    读不到就退回默认值（"*"），绝不因为读设置失败而挡住启动：
    全新安装时表还没建（create_all 在 lifespan 里才跑），这里必然读不到，
    这是正常路径不是故障。
    数据库连不上时也会走这条退路，但不会因此留下「白名单静默失效」的状态——
    紧接着 lifespan 里的 create_all 会连不上并抛错，进程直接退出重来，
    等数据库起来之后这里自然会读到真正的设置。

    值的写法：`*` 表示不限制（默认，与改造前行为一致）；
    多个域名用英文逗号隔开，例如 `https://a.example.com,https://b.example.com`。
    """
    raw = str(RUNTIME_DEFAULTS.get("allowed_origins") or "*")
    db = None
    try:
        db = SessionLocal()
        stored = settings_service.get(db, "allowed_origins")
        if stored is not None:
            raw = str(stored)
    except Exception:  # noqa: BLE001 —— 表还没建 / 数据库还没起来，都算正常
        logger.info("启动时读不到 allowed_origins 设置（数据库尚未就绪），本次用默认值")
    finally:
        if db is not None:
            db.close()
    items = [item.strip().rstrip("/") for item in raw.split(",") if item.strip()]
    # 只要里面有一个 *，就等于没限制；空着（管理员把这一项清空了）同样按不限制处理，
    # 否则会变成「所有跨域调用全被挡掉」这种没人料到的副作用。
    if not items or "*" in items:
        return ["*"]
    return items


ALLOWED_ORIGINS = _load_allowed_origins()

app.add_middleware(
    CORSMiddleware,
    allow_origins=ALLOWED_ORIGINS,
    # allow_credentials 保持 False，而且**不要**跟着 allowed_origins 一起打开。
    # 网页前端和接口是同源的（都由本进程提供），压根用不到跨域带凭证；
    # 取图用的 dk_files Cookie 是 SameSite=Lax + path=/files，别人的网页
    # 既带不上它、也读不到返回的字节。一旦这里改成 True，浏览器就会允许
    # 第三方页面拿着用户的登录状态调我们的接口，等于自己开一个 CSRF 面。
    allow_credentials=False,
    allow_methods=["*"],
    allow_headers=["*"],
)
app.add_middleware(SecurityHeadersMiddleware)

app.include_router(auth.router)
app.include_router(account.router)
app.include_router(users.router)
app.include_router(uploads.router)
app.include_router(templates.router)
app.include_router(inspiration.router)
app.include_router(generations.router)
app.include_router(apikeys.router)
app.include_router(settings_router.router)
app.include_router(v1.router)
# 取图路由。**必须在文件末尾那个 app.mount("/", ...) 之前注册**——
# Starlette 按注册顺序匹配，挂在 "/" 上的前端静态目录是个 catch-all，
# 排在它后面的路由永远匹配不到。放在这一批 include_router 里天然满足。
app.include_router(files.router)


@app.get("/healthz", include_in_schema=False)
def healthz():
    """容器健康检查。故意做一次真实的数据库往返：

    进程活着但连不上数据库时，页面会一直转圈却不报错，用户完全看不出问题。
    有了它，`docker compose ps` 会把容器标成 unhealthy，一眼能定位到根因。
    （Docker 本身不会因为 unhealthy 就重启容器，这里只负责把状态暴露出来。）
    """
    from sqlalchemy import text

    from .database import SessionLocal

    db = SessionLocal()
    try:
        db.execute(text("SELECT 1"))
    finally:
        db.close()
    return {"status": "ok"}


# 图片访问以前是这里三个 StaticFiles mount（/files/uploads、/files/outputs、
# /files/thumbnails），**一点鉴权都没有**：不用登录、不带任何令牌，知道地址就能
# 下载任何一个人的商品图和出图结果。现在换成了上面 include 的 files.router，
# 由它逐个请求判断归属和凭证。来龙去脉见 services/file_access.py 的文件头。
# 千万别为了「省事」再把静态目录挂回来——那等于把整个洞原样打开。
app.mount("/", RevalidateStaticFiles(directory=str(FRONTEND_DIR), html=True), name="frontend")
