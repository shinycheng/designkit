"""数据库连接：同时支持 SQLite（本机零安装）与 PostgreSQL（服务器部署）。

选型说明：
- 本机开发默认 SQLite，不用装任何数据库，`./start.sh` 一条命令即可跑；
- 服务器/Docker 用 PostgreSQL，多进程并发与大数据量（灵感库上万条）更稳。
切换只需改 DESIGNKIT_DATABASE_URL，代码零改动。
"""
from sqlalchemy import create_engine, event
from sqlalchemy.engine import make_url
from sqlalchemy.orm import DeclarativeBase, sessionmaker

from .config import DATABASE_URL

_url = make_url(DATABASE_URL)
IS_SQLITE = _url.get_backend_name() == "sqlite"
IS_POSTGRES = _url.get_backend_name() == "postgresql"

if IS_SQLITE:
    engine = create_engine(
        DATABASE_URL,
        # 多线程 worker 共用同一个 SQLite 文件时必须关掉同线程检查
        connect_args={"check_same_thread": False, "timeout": 30},
        pool_pre_ping=True,
        future=True,
    )

    @event.listens_for(engine, "connect")
    def _set_sqlite_pragma(dbapi_connection, connection_record):
        cursor = dbapi_connection.cursor()
        # WAL 模式让「网页请求读」和「后台 worker 写」互不阻塞
        cursor.execute("PRAGMA journal_mode=WAL")
        cursor.execute("PRAGMA busy_timeout=30000")
        # 强制外键约束，让 ondelete=SET NULL/CASCADE 真正生效（SQLite 默认关闭）
        cursor.execute("PRAGMA foreign_keys=ON")
        cursor.close()

else:
    # PostgreSQL 等：worker 线程 + 网页请求会并发取连接，池要够用；
    # pool_recycle 防止云数据库/中间件掐掉长时间空闲的连接后拿到坏连接。
    engine = create_engine(
        DATABASE_URL,
        pool_pre_ping=True,
        pool_size=10,
        max_overflow=10,
        pool_recycle=1800,
        future=True,
    )


SessionLocal = sessionmaker(bind=engine, autoflush=False, expire_on_commit=False, future=True)


class Base(DeclarativeBase):
    pass
