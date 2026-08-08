import logging
from contextlib import asynccontextmanager

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.staticfiles import StaticFiles

from .config import FRONTEND_DIR, OUTPUT_DIR, THUMB_DIR, UPLOAD_DIR
from .database import SessionLocal, engine
from .models import Base
from .routers import apikeys, auth, generations, settings_router, templates, uploads, v1
from .seed import seed
from .services.worker import worker

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
)

@asynccontextmanager
async def lifespan(_app: FastAPI):
    Base.metadata.create_all(bind=engine)
    db = SessionLocal()
    try:
        seed(db)
    finally:
        db.close()
    worker.start()
    yield
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
app.include_router(generations.router)
app.include_router(apikeys.router)
app.include_router(settings_router.router)
app.include_router(v1.router)


# 生成结果 / 上传图片的静态访问；只暴露图片子目录，数据库等文件绝不可对外
app.mount("/files/uploads", StaticFiles(directory=str(UPLOAD_DIR)), name="files_uploads")
app.mount("/files/outputs", StaticFiles(directory=str(OUTPUT_DIR)), name="files_outputs")
app.mount("/files/thumbnails", StaticFiles(directory=str(THUMB_DIR)), name="files_thumbnails")
app.mount("/", StaticFiles(directory=str(FRONTEND_DIR), html=True), name="frontend")
