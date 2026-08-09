"""轻量级启动迁移：给已存在的老数据库补新列。

create_all 只建缺失的表，不会给已有表加列；在引入 Alembic 之前，
用「检查列是否存在 → ALTER TABLE ADD COLUMN」的方式兼容老库。
只允许加可空列或带默认值的列——这类变更对老数据永远安全。
"""
import logging

from sqlalchemy import text
from sqlalchemy.engine import Engine

logger = logging.getLogger("designkit.migrations")

# (表名, 列名, 列定义 SQL)
_COLUMN_MIGRATIONS = [
    ("prompt_templates", "source", "VARCHAR(16) NOT NULL DEFAULT 'user'"),
    ("prompt_templates", "source_ref", "VARCHAR(64)"),
    ("generation_jobs", "prompt_sent", "TEXT"),
    ("sync_state", "lock_owner", "VARCHAR(64)"),
]

# 老库补索引（CREATE INDEX IF NOT EXISTS 在 SQLite 与 PostgreSQL 上都支持）
_INDEX_MIGRATIONS = [
    "CREATE INDEX IF NOT EXISTS ix_prompt_templates_source ON prompt_templates (source)",
    "CREATE INDEX IF NOT EXISTS ix_prompt_templates_source_ref ON prompt_templates (source_ref)",
    # 纵深防御：万一哪条同步路径漏网并发跑了，唯一索引会直接拦住重复写入，
    # 而不是静默把上万条灵感库翻倍（source_ref 为 NULL 的自建模板不受影响）
    "CREATE UNIQUE INDEX IF NOT EXISTS uq_prompt_templates_source_ref "
    "ON prompt_templates (source, source_ref)",
]


def run_migrations(engine: Engine) -> None:
    with engine.begin() as conn:
        for table, column, ddl in _COLUMN_MIGRATIONS:
            if engine.dialect.name == "sqlite":
                existing = {
                    row[1]
                    for row in conn.execute(text("PRAGMA table_info(%s)" % table))
                }
            else:  # PostgreSQL 等：走标准 information_schema
                existing = {
                    row[0]
                    for row in conn.execute(
                        text(
                            "SELECT column_name FROM information_schema.columns "
                            "WHERE table_name = :t"
                        ),
                        {"t": table},
                    )
                }
            if not existing or column in existing:
                continue  # 表不存在（create_all 会带新列建出来）或列已存在
            conn.execute(text("ALTER TABLE %s ADD COLUMN %s %s" % (table, column, ddl)))
            logger.info("迁移：%s 表新增列 %s", table, column)
        for ddl in _INDEX_MIGRATIONS:
            conn.execute(text(ddl))
