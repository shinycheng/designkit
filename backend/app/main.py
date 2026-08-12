import logging
from contextlib import asynccontextmanager
from typing import List
from urllib.parse import urlparse

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
    """给**所有**响应补三条基础安全响应头。

    「所有」是重点：以前只有 routers/files.py 和 routers/v1.py 这两处自己写了
    nosniff，也就是说只有取图和对外 API 带着它，而网页本体（HTML / JS）和
    /api/web/* 的 JSON 一条都没有。放在这里之后，静态文件、接口、404、
    重定向——凡是从这个进程出去的响应，一律都有。

    三条各自管什么：

    ① Referrer-Policy: no-referrer
       图片地址里带着任务号（outputs/年月/<任务号>/0.png），页面地址里也可能带
       业务参数。浏览器默认会把「来源页地址」跟着外链一起发给第三方站点——只要
       页面上有一个指向外部的链接或图片，任务号就跟着进了别人家的访问日志。
       这正是当初「知道地址就能下图」那条链路的传播途径之一，所以整站关掉。

    ② X-Content-Type-Options: nosniff
       禁止浏览器「猜」响应的类型。用户上传的是商品图，但上传的人可以精心构造
       一个既像图片、又像 HTML 的文件；没有这个头时，某些浏览器会按猜出来的
       HTML 去渲染它，于是别人上传的文件就变成了跑在**我们域名下**的网页，
       能读同域的 Cookie、能替用户点接口。上公网、谁都能注册之后，
       「上传的人」就是陌生人，这条从「理论风险」变成「等着被用」。

    ③ X-Frame-Options: SAMEORIGIN
       不许别人的网页把本系统套进 iframe 里。不加的话，攻击者可以做一个页面，
       把我们的设置页透明地叠在他的「点击领奖」按钮下面，用户以为在点他的按钮，
       实际点的是我们页面上的删除 / 保存（点击劫持）。
       用 SAMEORIGIN 不用 DENY：本系统自己将来要嵌自己（预览窗之类）时不会被卡住，
       而挡第三方的效果是一样的。
       注：更现代的写法是 CSP 的 frame-ancestors，但 CSP 要整体规划一遍
       （前端是无构建原生 JS，内联脚本、blob: 图片预览都要一条条放行），
       那是单独一期的事，这里先把不会误伤的这条上了。

    **故意没有加的：Content-Security-Policy 和 Strict-Transport-Security。**
    前者见上一段。后者（HSTS）属于反向代理那一层的事：它是对整个域名的长期承诺，
    一旦发出去，浏览器在 max-age 内会拒绝用 http 打开这个域名，配错了要等它过期，
    应用这边根本救不了。配 https 的时候在 Nginx / Caddy 上加，README 里写。

    写成裸 ASGI 中间件而不是 @app.middleware("http")：后者用的是 BaseHTTPMiddleware，
    它会把响应体整个接管一遍，对 StaticFiles 的 Range / 304 这类响应是额外风险，
    而我们只想改几个响应头。这里只包一层 send，字节流原样穿过。

    一律用 setdefault 不用直接赋值：routers/files.py 已经自己设过其中两个头，
    路由层写的更具体，不该被中间件覆盖掉。
    """

    # 头名一律小写：ASGI 规范里响应头名是小写字节串，MutableHeaders 也按小写比对，
    # 写成 X-Frame-Options 会导致 setdefault 认不出「已经有了」而重复添加。
    _DEFAULTS = (
        ("referrer-policy", "no-referrer"),
        ("x-content-type-options", "nosniff"),
        ("x-frame-options", "SAMEORIGIN"),
    )

    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return

        async def send_wrapper(message):
            if message["type"] == "http.response.start":
                headers = MutableHeaders(scope=message)
                for name, value in self._DEFAULTS:
                    headers.setdefault(name, value)
            await send(message)

        await self.app(scope, receive, send_wrapper)


from .backfill import run_backfill
from .config import FRONTEND_DIR, RUNTIME_DEFAULTS
from .database import SessionLocal, engine
from .migrations import run_schema_upgrade
from .routers import (
    account, apikeys, auth, files, generations, inspiration, invites, phone,
    register, settings_router, templates, uploads, users, v1,
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


def _origin_of(url: str) -> str:
    """把「对外访问地址」削成一个 CORS 用的 origin（协议 + 域名 + 端口，不带路径）。

    浏览器发过来的 Origin 头就长这样，白名单里的写法必须逐字对得上，
    多一个末尾斜杠或多一段路径都会匹配不上（而且**不会报错**，只是悄悄不放行）。
    认不出来就返回空串，由调用方决定怎么办。
    """
    parsed = urlparse((url or "").strip())
    if parsed.scheme not in ("http", "https") or not parsed.hostname:
        return ""
    host = parsed.hostname
    # 裸 IPv6 地址在 URL 和 Origin 头里都是带方括号的（http://[::1]:8787），
    # 而 .hostname 会把方括号剥掉。不补回去的话拼出来的白名单项永远匹配不上，
    # 而且不会有任何报错。
    if ":" in host:
        host = "[%s]" % host
    origin = "%s://%s" % (parsed.scheme, host)
    # 端口只在非默认端口时才出现在 Origin 头里（https://a.com:443 的 Origin 是
    # https://a.com）。跟着浏览器的写法来，否则白名单永远匹配不上。
    default_port = 443 if parsed.scheme == "https" else 80
    try:
        port = parsed.port
    except ValueError:      # 端口那一段不是数字，整个地址就是坏的
        return ""
    if port and port != default_port:
        origin += ":%d" % port
    return origin


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

    ══════════════════════════════════════════════════════════════════
     ⚠ 开着自助注册时，`*` **不再被接受**（本次新增的闸门）
    ══════════════════════════════════════════════════════════════════
    默认值仍然是 `*`，一个字没改——改默认会静默打断现有部署，那是这个项目
    反复强调过的红线。变的是**开放注册之后**的处理：这时 `*` 会被自动收窄成
    「只允许对外访问地址（public_base_url）那一个域名」，并在日志里大声说明。

    为什么开放注册之后 `*` 就不能忍了。allow_credentials 是 False，所以别人的
    网页拿不到用户的登录状态，读不了任何要登录的接口——这条今天成立，明天也成立。
    真正的问题在**免登录的那几条写接口**上：

        POST /api/web/phone/code    发一条短信 = 花一笔钱
        POST /api/web/phone/register
        POST /api/web/register

    `*` 意味着**任何一个网站**都可以在它自己的页面里对这几条接口发请求、并且
    读得到返回内容。攻击者只要把一段脚本放进一个有流量的页面，每个访客的浏览器
    都会替他打一次我们的发码接口——用的是**访客自己的 IP**，于是
    sms_code_ip_hourly_limit（同 IP 每小时 20 条）那一层等于不存在，
    只剩「全站每天 200 条」那一道兜着。而这件事发生的时候，服务端日志里看到的是
    一大批来自不同真实 IP 的正常请求，完全看不出是被人当枪使了。
    读得到返回内容这一点同样要紧：脚本能看见「这个手机号是不是已经注册过」
    这类回答，那就成了一个免费的手机号撞库接口。

    收窄成 public_base_url 那一个域名是安全的，不会打断任何人：网页前端和接口
    本来就是**同源**的（都由这个进程提供），同源请求根本不走 CORS；
    而开放注册的硬前置闸门（routers/settings_router.py 的
    _open_register_blockers）已经强制 public_base_url 必须填成别人真能打开的
    公网 https 地址，所以这里取到的一定是个像样的值。
    万一还是取不出来（老库、被人直接改过数据库），就退成「一个都不允许」——
    宁可让某个第三方页面的跨域调用失败（会在浏览器控制台报得清清楚楚），
    也不能让上面那扇门开着。

    真有跨域需求（比如另一个域名的运营后台要调这里的接口）：到设置页把
    allowed_origins 填成那个域名，填了就照填的来，这里不会再插手。
    """
    raw = str(RUNTIME_DEFAULTS.get("allowed_origins") or "*")
    open_register = False
    public_base_url = ""
    db = None
    try:
        db = SessionLocal()
        stored = settings_service.get(db, "allowed_origins")
        if stored is not None:
            raw = str(stored)
        # 两条注册路只要有**任意一条**开着就算「对陌生人开放」，
        # 口径与 settings_router 那道硬前置闸门完全一致（只管一条等于没管）。
        open_register = bool(settings_service.get(db, "self_register_enabled")) or bool(
            settings_service.get(db, "phone_register_enabled")
        )
        public_base_url = str(settings_service.get(db, "public_base_url") or "")
    except Exception:  # noqa: BLE001 —— 表还没建 / 数据库还没起来，都算正常
        logger.info("启动时读不到 allowed_origins 设置（数据库尚未就绪），本次用默认值")
    finally:
        if db is not None:
            db.close()
    items = [item.strip().rstrip("/") for item in raw.split(",") if item.strip()]
    # 只要里面有一个 *，就等于没限制；空着（管理员把这一项清空了）同样按不限制处理，
    # 否则会变成「所有跨域调用全被挡掉」这种没人料到的副作用。
    if items and "*" not in items:
        logger.info("CORS 白名单：%s", "、".join(items))
        return items
    if not open_register:
        return ["*"]
    fallback = _origin_of(public_base_url)
    if fallback:
        logger.warning(
            "自助注册是开着的，所以「允许哪些网站调用本系统接口」不能是 * "
            "（那等于任何网站都能在访客浏览器里替他调我们的注册和发短信接口，"
            "还读得到返回内容）。本次已自动收窄为只允许 %s（取自「对外访问地址」）。"
            "网页界面本身是同源的，不受影响。若确实要让别的域名跨域调用，"
            "请到「系统设置 → 允许哪些网站调用」里把那个域名填上，改完重启服务生效。",
            fallback,
        )
        return [fallback]
    logger.warning(
        "自助注册是开着的，但「对外访问地址」取不出一个能用的域名（现在是 %r），"
        "无法据此收窄跨域白名单，本次按「不允许任何跨域调用」处理。"
        "网页界面是同源的、照常可用；请到「系统设置」里把「对外访问地址」填成"
        "别人在浏览器里访问本系统时用的那个完整地址（例如 https://designkit.example.com），"
        "改完重启服务。",
        public_base_url,
    )
    return []


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
# 自助注册（免登录）与邀请码管理（管理员）。
# register 是**全系统唯一一条不需要登录的写接口**，它自己会在每次请求里重新确认
# 「注册总开关开着 + 允许访问内网关着」这个组合（见 routers/register.py 文件头），
# 所以放在这里不需要额外包一层依赖。开放注册的**前置闸门**在
# routers/settings_router.py 的 _apply_self_register_updates 里，两处是一套闸门的两半：
# 设置层负责「物理上打不开」，注册层负责「万一被绕过也开不了门」。
app.include_router(register.router)
# 手机号 + 短信验证码注册（免登录），与上面邀请码那条路**并列**：
# 两条路共用同一套「开放注册的硬前置闸门」，各自有各自的总开关，
# 关掉其中一条不影响另一条。
#
# 这里一共开出四条不需要登录的路由（GET /api/web/phone/status、
# POST /api/web/phone/code、/register、/login）。加上上面的 register，
# 它们是**全系统仅有的几条免登录写接口**——反向代理 / WAF 那一层的限速规则
# 必须把 /api/web/phone/* 一起覆盖进去，别只写了 /api/web/register。
#
# ⚠ POST /api/web/phone/code **每调用一次就可能花掉一条短信的钱**。
# 应用层已经做了三层限速（同号冷却与日限、同 IP 每小时、全站每日总量，
# 全部落库、多进程安全，见 routers/phone.py），但那是最后一道，不是唯一一道。
app.include_router(phone.router)
app.include_router(invites.router)
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
