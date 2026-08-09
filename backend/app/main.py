import logging
from contextlib import asynccontextmanager

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.staticfiles import StaticFiles
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

from .config import FRONTEND_DIR, OUTPUT_DIR, THUMB_DIR, UPLOAD_DIR
from .database import SessionLocal, engine
from .migrations import run_migrations
from .models import Base
from .routers import (
    apikeys, auth, generations, inspiration, settings_router, templates, uploads, v1,
)
from .seed import seed
from .services.scheduler import scheduler
from .services.worker import worker

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
)

@asynccontextmanager
async def lifespan(_app: FastAPI):
    Base.metadata.create_all(bind=engine)
    run_migrations(engine)  # 给老数据库补新列（create_all 不会改已有表）
    db = SessionLocal()
    try:
        seed(db)
    finally:
        db.close()
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

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=False,
    allow_methods=["*"],
    allow_headers=["*"],
)

app.include_router(auth.router)
app.include_router(uploads.router)
app.include_router(templates.router)
app.include_router(inspiration.router)
app.include_router(generations.router)
app.include_router(apikeys.router)
app.include_router(settings_router.router)
app.include_router(v1.router)


@app.get("/healthz", include_in_schema=False)
def healthz():
    """容器健康检查。故意只做一次真实的数据库往返：

    进程活着但连不上数据库时，页面会一直转圈却不报错，用户完全看不出问题；
    让编排层把这种状态判为「不健康」并重启，比让人对着转圈猜要好。
    """
    from sqlalchemy import text

    from .database import SessionLocal

    db = SessionLocal()
    try:
        db.execute(text("SELECT 1"))
    finally:
        db.close()
    return {"status": "ok"}


# 生成结果 / 上传图片的静态访问；只暴露图片子目录，数据库等文件绝不可对外
app.mount("/files/uploads", StaticFiles(directory=str(UPLOAD_DIR)), name="files_uploads")
app.mount("/files/outputs", StaticFiles(directory=str(OUTPUT_DIR)), name="files_outputs")
app.mount("/files/thumbnails", StaticFiles(directory=str(THUMB_DIR)), name="files_thumbnails")
app.mount("/", RevalidateStaticFiles(directory=str(FRONTEND_DIR), html=True), name="frontend")
