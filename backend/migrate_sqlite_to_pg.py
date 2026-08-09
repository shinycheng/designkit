"""把本机 SQLite 的数据整体搬到 PostgreSQL（部署上服务器时用一次）。

用法：
    # 在服务器上用应用容器跑（推荐）：目标库自动取容器已配好的连接，
    # 不用手写连接串，密码里有 @ % 等字符也不会出错
    docker compose run --rm designkit \
        python -m backend.migrate_sqlite_to_pg --source "sqlite:////app/data/designkit.db"

    # 也可以显式指定目标库
    .venv/bin/python -m backend.migrate_sqlite_to_pg \
        --target "postgresql+psycopg://designkit:密码@127.0.0.1:5432/designkit"

    # 只想看会搬多少、不实际写入（不需要连上目标库）：
    .venv/bin/python -m backend.migrate_sqlite_to_pg --source "..." --dry-run

要点：
- 表按外键依赖顺序搬，避免引用不存在的父行；
- 目标表非空时默认中止（防止把生产库搬串），确认要覆盖再加 --truncate；
- 分批提交，1.4 万条灵感库不会撑爆内存；
- 搬完自动把 PostgreSQL 的自增序列推到当前最大 id，否则新建记录会主键冲突
  （这是 SQLite→PG 迁移最容易踩的坑）。
"""
import argparse
import sys
from typing import List

from sqlalchemy import create_engine, func, insert, inspect, select, text
from sqlalchemy.orm import Session

from .app.config import DATABASE_URL
from .app.models import (  # noqa: F401 —— 导入即注册到 Base.metadata
    ApiKey, AppSetting, Base, GeneratedImage, GenerationJob,
    PromptCategory, PromptTemplate, SyncState, Upload, User,
)

BATCH = 500

# 按外键依赖排序：父表在前
ORDERED_TABLES: List[str] = [
    "users", "api_keys", "prompt_categories", "prompt_templates",
    "uploads", "generation_jobs", "generated_images",
    "app_settings", "sync_state",
]


def _table(name):
    return Base.metadata.tables[name]


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description="SQLite → PostgreSQL 数据迁移")
    parser.add_argument("--source", default=DATABASE_URL, help="源库连接串，默认取当前配置")
    parser.add_argument(
        "--target", default=None,
        help="目标 PostgreSQL 连接串，默认取当前配置（在应用容器里跑时正好是它连的那个库）")
    parser.add_argument("--truncate", action="store_true", help="目标表非空时先清空（危险）")
    parser.add_argument("--dry-run", action="store_true", help="只统计不写入")
    args = parser.parse_args(argv)

    target = args.target or str(DATABASE_URL)

    if not args.source.startswith("sqlite"):
        print("源库不是 SQLite：%s" % args.source, file=sys.stderr)
        return 1
    # dry-run 只统计源库，压根不碰目标库，所以不校验它——
    # 这样在 PG 还没搭好之前就能先看清要搬多少数据
    if not args.dry_run and "postgresql" not in target:
        print("目标库不是 PostgreSQL：%s" % target, file=sys.stderr)
        if args.target is None:
            print("（没给 --target，默认用了当前配置的库；请确认已切到 PostgreSQL）", file=sys.stderr)
        return 1

    src = create_engine(args.source, connect_args={"check_same_thread": False}, future=True)

    # 源库可能是老版本、还没有新加的表，跳过即可（不是错误）
    existing = set(inspect(src).get_table_names())
    tables = [t for t in ORDERED_TABLES if t in existing]
    for missing in [t for t in ORDERED_TABLES if t not in existing]:
        print("  %-20s 源库中不存在，跳过" % missing)

    # dry-run 只统计源库，不连目标库——方便在没搭好 PG 之前先看清要搬多少
    if args.dry_run:
        total = 0
        with Session(src) as s_src:
            for name in tables:
                n = s_src.execute(select(func.count()).select_from(_table(name))).scalar_one()
                print("  %-20s %d 条" % (name, n))
                total += n
        print("\n合计 %d 条（dry-run，未连接目标库、未写入）" % total)
        return 0

    dst = create_engine(target, future=True)
    Base.metadata.create_all(bind=dst)  # 目标库缺表就建出来
    print("目标库表结构已就绪\n")

    total_moved = 0
    with Session(src) as s_src, Session(dst) as s_dst:
        # 先检查目标库是否已有数据，避免把生产数据搬乱
        if not args.dry_run:
            occupied = []
            for name in tables:
                n = s_dst.execute(select(func.count()).select_from(_table(name))).scalar_one()
                if n:
                    occupied.append("%s(%d)" % (name, n))
            if occupied:
                if not args.truncate:
                    print("目标库以下表已有数据，已中止：%s" % "、".join(occupied), file=sys.stderr)
                    print("确认要覆盖请加 --truncate", file=sys.stderr)
                    return 2
                for name in reversed(tables):  # 反序删，先删子表
                    s_dst.execute(_table(name).delete())
                s_dst.commit()
                print("已清空目标库旧数据\n")

        for name in tables:
            table = _table(name)
            count = s_src.execute(select(func.count()).select_from(table)).scalar_one()
            if not count:
                print("  %-20s 0 条，跳过" % name)
                continue
            if args.dry_run:
                print("  %-20s %d 条（dry-run 未写入）" % (name, count))
                total_moved += count
                continue

            moved = 0
            for offset in range(0, count, BATCH):
                # 必须带稳定排序：LIMIT/OFFSET 无 ORDER BY 时行序不保证，
                # 分批之间可能漏行或重复行
                order_cols = list(table.primary_key.columns) or [list(table.columns)[0]]
                rows = s_src.execute(
                    select(table).order_by(*order_cols).limit(BATCH).offset(offset)
                ).mappings().all()
                if not rows:
                    break
                s_dst.execute(insert(table), [dict(r) for r in rows])
                s_dst.commit()
                moved += len(rows)
            print("  %-20s %d 条已迁移" % (name, moved))
            total_moved += moved

        # 关键收尾：把自增序列推到最大 id，否则新增记录会撞主键
        if not args.dry_run:
            for name in tables:
                table = _table(name)
                pk = list(table.primary_key.columns)
                if len(pk) != 1:
                    continue
                col = pk[0]
                try:
                    if col.type.python_type is not int:
                        continue  # 字符串主键（如 generation_jobs.id）没有自增序列
                except NotImplementedError:
                    continue
                s_dst.execute(text(
                    "SELECT setval(pg_get_serial_sequence(:t, :c), "
                    "COALESCE((SELECT MAX(%s) FROM %s), 0) + 1, false)" % (col.name, name)
                ), {"t": name, "c": col.name})
            s_dst.commit()
            print("\n已同步自增序列，新建记录不会主键冲突")

    print("\n合计 %d 条%s" % (total_moved, "（dry-run，未实际写入）" if args.dry_run else " 迁移完成"))
    print("提示：data/ 目录里的图片文件需要另外拷到服务器，数据库里存的是相对路径。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
