"""轻量级启动迁移：给已存在的老数据库补新列、补索引。

`create_all` 只建缺失的**表**，不会给已有表加**列**；在引入 Alembic 之前，
用「检查列是否存在 → ALTER TABLE ADD COLUMN」的方式兼容老库。
只允许加可空列或带默认值的列——这类变更对老数据永远安全。

本机开发用 SQLite，群晖 NAS 生产用 PostgreSQL，**这里写的每一条 SQL 两边都要能跑**。
下面四条纪律都是踩过坑或复盘出来的，改这个文件之前请先读完。

────────────────────────────────────────────────────────────────────────
纪律一：ADD COLUMN 的类型白名单（违反的话「本机全绿、NAS 起不来」）
────────────────────────────────────────────────────────────────────────
`_COLUMN_MIGRATIONS` 里的列定义**只许**用这几种类型：

    VARCHAR(n)   TEXT   INTEGER   TIMESTAMP   FLOAT

**明令禁止**（两条都是 SQLite 收、PostgreSQL 拒的写法）：

  - `DATETIME`        → PostgreSQL 根本没有这个类型名，直接报错。请写 `TIMESTAMP`。
  - `BOOLEAN DEFAULT 0` → PostgreSQL 不会把整数 0 隐式转成布尔，直接报错。

为什么要单独立一条规矩：SQLite 对 DDL 里的类型名几乎来者不拒，写错了在
monica 的 Mac 上一路绿灯，推到 NAS 上启动就崩；而 docker-compose.yml 里是
`restart: always`，表现出来就是**容器无限重启**，运营同学只看得到「一直在重启」，
完全没法归因到某一行 SQL。为此下面还加了一道 `_check_column_type_whitelist()`
开机自检：类型写错的话本机第一次启动就会直接报错，把问题拦在 Mac 上。

还有一条延伸规矩：**给已有表加列时一律不要用布尔语义**。即使哪天 PostgreSQL
那条 DDL 侥幸被接受，SQLAlchemy 模型里的 `Boolean` 在 PG 上生成的是 `BOOLEAN`，
而这里 ALTER 出来的可能是 `INTEGER`，两边类型对不上，问题会拖到很久以后才炸。
要表达开关/状态，请用 `VARCHAR(16)` 存 'on'/'off'、'active'/'disabled' 这类字符串。

────────────────────────────────────────────────────────────────────────
纪律二：哪些 DDL 必须挡死启动（fail-closed），哪些不许挡死
────────────────────────────────────────────────────────────────────────
`backend/app/main.py` 里 `run_schema_upgrade(engine)` 是**裸调用、没有 try/except**，
这是有意的设计：库结构没补齐就不该让应用跑起来，否则会以「某个接口莫名 500」
的形式在几天后爆出来，离根因十万八千里。

  - `create_all`（建缺失的表）
    → 单独一个事务，出错就让它抛出去挡死启动（fail-closed，别加 try/except）。

  - `_COLUMN_MIGRATIONS` + `_INDEX_MIGRATIONS_SAFE`
    → 同一个事务里跑，出错就让它抛出去挡死启动（fail-closed，别加 try/except）。
      加可空列、建普通索引，对存量数据永远安全，正常情况下不可能失败。

  - `_INDEX_MIGRATIONS_RISKY`（唯一索引这类**可能撞上存量重复值**的 DDL）
    → 每条**单独开一个事务** + try/except，失败只打一条中文人话日志然后继续。
      不这么做的话：一条唯一索引撞上历史重复数据 → 整批 DDL 回滚 → 启动被挡死 →
      `restart: always` 无限重启。为了「加一层可有可无的防护」把整个系统弄挂，
      这笔账怎么算都不划算。

────────────────────────────────────────────────────────────────────────
纪律三：能建新表就别改老表
────────────────────────────────────────────────────────────────────────
`Base.metadata.create_all(checkfirst=True)` 随时可以多建一张新表，自动带齐所有列、
对存量数据零影响、零迁移成本。真正有风险的只有「给老表加列」这条路——因为这里
拼的是不做方言适配的裸 SQL（见纪律一）。所以设计表结构时，宁可多开一张新表，
也不要给老表塞一堆将来才用得上的列。

────────────────────────────────────────────────────────────────────────
纪律四：**建新表（create_all）和补老列一样要在锁里**
────────────────────────────────────────────────────────────────────────
启动时改库结构的唯一入口是 `run_schema_upgrade(engine)`：它先拿 PostgreSQL 咨询锁，
**然后**才 `create_all` 建新表、补老列、补索引。千万别把 `create_all` 挪到锁外面
（比如图省事在 main.py 里先建表再调迁移）——那样锁只保护了一半。

为什么 create_all 也会撞车：`checkfirst=True` 同样是「先查后改」——先查表在不在，
再 CREATE TABLE。两个进程同时启动，可能都查到「表不存在」，然后第二个进程建表时
报 `relation "xxx" already exists`（psycopg 抛 DuplicateTable）或者
`duplicate key value violates unique constraint "pg_class_relname_nsp_index"`，
进程直接崩掉。实测：让 4 个进程同时启动、跑 8 轮，8 轮全部有进程崩。

今天单进程 uvicorn 撞不上（Dockerfile 和 start.sh 都没有 --workers），
但「为了扛并发加个 --workers」是个看起来完全无害的改动，加了之后，
下一次新增表的那一版升级就会当场炸给使用者看。
"""
import logging
import time
from typing import NamedTuple, Optional, Set, Tuple

from sqlalchemy import text
from sqlalchemy.engine import Connection, Engine

from .models import Base

logger = logging.getLogger("designkit.migrations")

# 允许出现在 _COLUMN_MIGRATIONS 列定义开头的类型名（详见文件头「纪律一」）
_ALLOWED_COLUMN_TYPES = ("VARCHAR", "TEXT", "INTEGER", "TIMESTAMP", "FLOAT")

# (表名, 列名, 列定义 SQL)
# 列定义的第一个词必须在 _ALLOWED_COLUMN_TYPES 里，开机自检会当场拦下写错的。
_COLUMN_MIGRATIONS = [
    ("prompt_templates", "source", "VARCHAR(16) NOT NULL DEFAULT 'user'"),
    ("prompt_templates", "source_ref", "VARCHAR(64)"),
    ("prompt_templates", "source_slugs", "TEXT"),
    ("generation_jobs", "prompt_sent", "TEXT"),
    ("sync_state", "lock_owner", "VARCHAR(64)"),
    # 下面两列是「每个人只能看到自己的东西」这件事的地基。
    # 都只写 INTEGER、**不带外键约束**：老库上加外键要先扫全表校验存量数据，
    # 而且这里拼的是不做方言适配的裸 SQL，ADD CONSTRAINT 的写法两边并不一致。
    # 模型里声明了外键、老库里没有，这个差异是有意接受的——归属校验一律走应用层。
    ("api_keys", "user_id", "INTEGER"),
    ("prompt_templates", "owner_user_id", "INTEGER"),
]

# 老库补索引（CREATE INDEX IF NOT EXISTS 在 SQLite 与 PostgreSQL 上都支持）。
# 这一组是**普通索引**：只是加快查询，建不出来才是真出事了，所以留在主事务里
# 跟着 ADD COLUMN 一起 fail-closed。
_INDEX_MIGRATIONS_SAFE = [
    "CREATE INDEX IF NOT EXISTS ix_prompt_templates_source ON prompt_templates (source)",
    "CREATE INDEX IF NOT EXISTS ix_prompt_templates_source_ref ON prompt_templates (source_ref)",
    # 这三条都是**非唯一**索引，撞不上存量重复值，所以放心留在主事务里。
    # 后两条是给「按图片路径反查这张图属于谁」用的：/files 改成鉴权路由之后，
    # 每放行一张图都要按 path 查一次归属。uploads 有十几万行历史记录、
    # prompt_templates 有 14573 行，不加索引就是每张缩略图全表扫一遍——
    # 历史页一屏几十张图，用户看到的表现是「点开就转圈，像是坏了」。
    "CREATE INDEX IF NOT EXISTS ix_api_keys_user_id ON api_keys (user_id)",
    "CREATE INDEX IF NOT EXISTS ix_uploads_path ON uploads (path)",
    "CREATE INDEX IF NOT EXISTS ix_prompt_templates_thumbnail_path "
    "ON prompt_templates (thumbnail_path)",
]


class _RiskyIndex(NamedTuple):
    """一条「可能因为存量数据而建不出来」的索引。

    唯一索引是典型代表：库里只要有一组重复值，这条 DDL 就必然失败。
    它失败不该挡死整个系统，所以每条单独跑、单独兜住（见文件头「纪律二」）。

    字段说明（what/howto 是给运营同学看的，务必写人话、不要写术语）：
      name    —— 索引名，只用来打日志
      ddl     —— 真正执行的建索引语句
      table   —— 出错时去哪张表数重复记录
      columns —— 按哪几列判重（NULL 值不参与判重，与唯一索引的行为一致）
      what    —— 出了什么事，一句话
      howto   —— 运营同学能自己做的处理办法
    """

    name: str
    ddl: str
    table: str
    columns: Tuple[str, ...]
    what: str = "库里有重复记录"
    howto: str = "请联系技术同学清理重复数据"


_INDEX_MIGRATIONS_RISKY = [
    # 纵深防御：万一哪条同步路径漏网并发跑了，唯一索引会直接拦住重复写入，
    # 而不是静默把上万条灵感库翻倍（source_ref 为 NULL 的自建模板不受影响）。
    # 注意它只是「多一层保险」：真正防重复的是 scheduler.py 里的同步锁，
    # 所以这条建不上也不该影响任何人正常用。
    _RiskyIndex(
        name="uq_prompt_templates_source_ref",
        ddl=(
            "CREATE UNIQUE INDEX IF NOT EXISTS uq_prompt_templates_source_ref "
            "ON prompt_templates (source, source_ref)"
        ),
        table="prompt_templates",
        columns=("source", "source_ref"),
        what="灵感库里存在重复的提示词",
        # 注意别把「重新同步一次就好了」写进来：inspiration.py 的入库是按 source_ref
        # 建字典去匹配的（import_cache_to_db 里的 existing），一个 ref 对应多行时它只会
        # 更新其中一行，另一行原样留着——所以同步并不会清掉重复。这里只能如实说。
        howto="把这条日志发给技术同学清理重复条目即可，不着急",
    ),
]

# PostgreSQL 咨询锁的编号。数字本身没有含义，随手挑的；
# 要紧的是**永远不要改动它**——改了就等于新旧两个版本的进程各锁各的，锁形同虚设。
_PG_ADVISORY_LOCK_KEY = 8150924133
_LOCK_WAIT_SECONDS = 120   # 等别的进程升级完最多等这么久
_LOCK_POLL_SECONDS = 0.5   # 每隔多久重试一次抢锁


# ---------------------------------------------------------------- 开机自检

def _check_column_type_whitelist() -> None:
    """检查 _COLUMN_MIGRATIONS 里的列类型是否都在白名单里（详见文件头「纪律一」）。

    这个检查不碰数据库、纯粹看常量表，所以它在 Mac（SQLite）和 NAS（PostgreSQL）上
    的结论完全一样——这正是它存在的意义：把「PostgreSQL 才会报的错」提前到本机
    第一次启动就报出来，而不是等推上 NAS 之后变成容器无限重启。
    """
    for table, column, ddl in _COLUMN_MIGRATIONS:
        # 取列定义的第一个词作为类型名："VARCHAR(16) NOT NULL DEFAULT 'user'" → "VARCHAR"
        type_name = ddl.strip().split("(")[0].split()[0].upper()
        if type_name in _ALLOWED_COLUMN_TYPES:
            continue
        raise ValueError(
            "迁移定义有误：%s.%s 用了不允许的列类型 %s。"
            "给已有表加列只能用 %s。"
            "特别注意 DATETIME 和 BOOLEAN：SQLite 收、PostgreSQL 拒，"
            "本机看着一切正常，推到服务器上会启动即崩。"
            "日期时间请写 TIMESTAMP，开关状态请用 VARCHAR 存字符串。"
            "详见 backend/app/migrations.py 文件头的说明。"
            % (table, column, type_name, " / ".join(_ALLOWED_COLUMN_TYPES))
        )


# ---------------------------------------------------------------- 进程锁

def _acquire_process_lock(conn: Connection) -> bool:
    """升级期间只允许一个进程动手；返回是否真的拿到了锁（供 finally 判断要不要释放）。

    为什么需要：这里是典型的「先查后改」——先查列在不在，再 ALTER TABLE。
    两个进程同时启动，可能都查到「列不存在」，然后第二个 ALTER 时报
    duplicate column 直接起不来。今天 Dockerfile 和 start.sh 都是裸 uvicorn、
    没有 --workers，所以只有一个进程、暂时撞不上；但「为了扛并发加个 --workers」
    是个看起来完全无害的改动，等真加上的那天，问题会以「容器起不来」的形式爆出来。

    SQLite 这边直接跳过：本机开发和单进程部署就一个进程，而且加锁反而要引入
    文件锁之类的额外机制。真正会上多进程的是 NAS 上的 PostgreSQL。
    """
    if conn.dialect.name != "postgresql":
        return False

    deadline = time.monotonic() + _LOCK_WAIT_SECONDS
    waited = False
    while True:
        got = conn.execute(
            text("SELECT pg_try_advisory_lock(:k)"), {"k": _PG_ADVISORY_LOCK_KEY}
        ).scalar()
        # 结束 SQLAlchemy 自动开启的那个隐式事务，好让后面能干净地 conn.begin()。
        # 咨询锁是「连接级」的，commit 不会把它释放掉。
        conn.commit()
        if got:
            if waited:
                logger.info("已拿到数据库升级锁，继续启动")
            return True
        if not waited:
            logger.info(
                "另一个进程正在升级数据库结构，本进程排队等待（最多 %d 秒）",
                _LOCK_WAIT_SECONDS,
            )
            waited = True
        if time.monotonic() >= deadline:
            raise RuntimeError(
                "等待其他进程完成数据库升级超过 %d 秒，本次启动放弃。"
                "服务会自动重启重试；如果一直重复出现，说明有个进程卡住了，"
                "把整个服务停掉再启动一次即可。" % _LOCK_WAIT_SECONDS
            )
        time.sleep(_LOCK_POLL_SECONDS)


def _release_process_lock(conn: Connection, acquired: bool) -> None:
    """释放升级锁。释放失败不能盖掉真正的错误，所以只记日志、不抛异常。"""
    if not acquired:
        return
    try:
        conn.execute(
            text("SELECT pg_advisory_unlock(:k)"), {"k": _PG_ADVISORY_LOCK_KEY}
        )
        conn.commit()
    except Exception:
        # 走到这儿多半是连接本身断了，而连接一断 PostgreSQL 会自动清掉它持有的锁，
        # 所以不会留下永久悬挂的锁，不用惊动使用者。
        logger.warning("释放数据库升级锁失败（连接断开时数据库会自动清理，不影响使用）")


# ---------------------------------------------------------------- 补列

def _existing_columns(conn: Connection, table: str) -> Set[str]:
    """查一张表现有的列名；表不存在时返回空集合。"""
    if conn.dialect.name == "sqlite":
        return {row[1] for row in conn.execute(text("PRAGMA table_info(%s)" % table))}
    # PostgreSQL 等：走标准 information_schema。
    # 必须带上 table_schema = current_schema()：information_schema.columns 会返回
    # 当前角色**能看到的所有 schema** 下的同名表，别的 schema 里有张同名老表就会
    # 让这里误判成「列已经有了」而静默跳过 ADD COLUMN，等应用真去读那一列时才报错，
    # 那时候现场离根因已经很远了。
    rows = conn.execute(
        text(
            "SELECT column_name FROM information_schema.columns "
            "WHERE table_name = :t AND table_schema = current_schema()"
        ),
        {"t": table},
    )
    return {row[0] for row in rows}


def _add_missing_columns(conn: Connection) -> None:
    """给老表补新列。这一段是 fail-closed 的：出错就抛出去挡死启动，别加 try/except。"""
    for table, column, ddl in _COLUMN_MIGRATIONS:
        existing = _existing_columns(conn, table)
        if not existing or column in existing:
            continue  # 表不存在（create_all 会带新列建出来）或列已存在
        conn.execute(text("ALTER TABLE %s ADD COLUMN %s %s" % (table, column, ddl)))
        logger.info("迁移：%s 表新增列 %s", table, column)


# ---------------------------------------------------------------- 危险索引

def _count_duplicate_groups(conn: Connection, item: _RiskyIndex) -> Optional[int]:
    """数一下有多少组重复值挡住了唯一索引；数不出来就返回 None（日志里省略数量）。

    判重时排除掉含 NULL 的行——唯一索引本来就把 NULL 当作各不相同，
    把它们算进来会给出一个吓人又不对的数字。
    """
    cols = ", ".join(item.columns)
    not_null = " AND ".join("%s IS NOT NULL" % c for c in item.columns)
    sql = (
        "SELECT COUNT(*) FROM ("
        "SELECT %s FROM %s WHERE %s GROUP BY %s HAVING COUNT(*) > 1"
        ") AS dup" % (cols, item.table, not_null, cols)
    )
    try:
        with conn.begin():
            return int(conn.execute(text(sql)).scalar() or 0)
    except Exception:
        return None


def _run_risky_index(conn: Connection, item: _RiskyIndex) -> None:
    """单独开一个事务建一条危险索引；失败只打人话日志，绝不挡死启动。"""
    try:
        with conn.begin():
            conn.execute(text(item.ddl))
        return
    except Exception as exc:
        detail = str(exc).strip().splitlines()[0] if str(exc).strip() else exc.__class__.__name__

    groups = _count_duplicate_groups(conn, item)
    amount = "发现 %d 组重复" % groups if groups is not None else "存在重复"
    logger.warning(
        "数据库升级提醒：%s（%s），所以这次没能加上防重复保护。"
        "其余功能一切正常，可以照常使用。"
        "处理办法：%s；处理完重启一次服务就会自动补上。"
        "（技术细节：建索引 %s 失败 —— %s）",
        item.what, amount, item.howto, item.name, detail,
    )


# ---------------------------------------------------------------- 入口

def _create_missing_tables(conn: Connection) -> None:
    """建出模型里声明、库里还没有的表（见文件头「纪律四」）。

    必须在**已经拿到锁**的那条连接上跑，所以这里收的是 Connection 而不是 Engine——
    传 Engine 的话 SQLAlchemy 会另开一条连接，而咨询锁是「连接级」的，
    等于又绕到锁外面去了。签名写成 Connection 就让这种写法根本编不出来。

    事务由调用方（run_schema_upgrade）单独开，不跟补列挤在一个事务里：建表全成
    或全不成，出错直接抛出去挡死启动（fail-closed，理由见「纪律二」）。
    """
    Base.metadata.create_all(bind=conn, checkfirst=True)


def _apply_column_and_index_migrations(conn: Connection) -> None:
    """在**已经拿到锁**的连接上补老列、补索引。收 Connection 的理由同上。"""
    # 补列 + 普通索引：同一个事务，要么全成要么全不成，失败直接抛出去
    with conn.begin():
        _add_missing_columns(conn)
        for ddl in _INDEX_MIGRATIONS_SAFE:
            conn.execute(text(ddl))
    # 危险索引：一条一个事务，失败只记日志（前一条失败不影响后一条）
    for item in _INDEX_MIGRATIONS_RISKY:
        _run_risky_index(conn, item)


def run_schema_upgrade(engine: Engine) -> None:
    """**启动时改库结构的唯一入口**：一把锁罩住「建新表 + 补老列 + 补索引」全过程。

    顺序是固定的：先建缺的表，再给老表补列——补列那一步靠 `_existing_columns`
    判断表在不在，表得先建出来它才认得。

    为什么三件事必须共用一把锁、而不是各锁各的：`create_all(checkfirst=True)`
    和补列一样是「先查后改」，两个进程同时启动会一起查到「表不存在」，
    然后第二个建表时以 `relation "xxx" already exists` 崩掉。详见文件头「纪律四」。

    调用方（main.py）故意不包 try/except：结构没补齐就该挡死启动。
    唯一「允许失败」的是 _INDEX_MIGRATIONS_RISKY，它在内部自己兜住了。
    """
    _check_column_type_whitelist()
    with engine.connect() as conn:
        acquired = _acquire_process_lock(conn)
        try:
            with conn.begin():
                _create_missing_tables(conn)
            _apply_column_and_index_migrations(conn)
        finally:
            _release_process_lock(conn, acquired)


def run_migrations(engine: Engine) -> None:
    """只给**已经存在的**表补列、补索引，不建新表。

    ⚠️ 生产启动请用上面的 `run_schema_upgrade`，别用这个——它不建表，
    而且新表和老列分成两把锁拿的话，锁只保护了一半（文件头「纪律四」）。
    保留它是因为 tests/test_migrations.py 要单独盯「老库补列」这一段：
    那些用例自己先造好老库、再调它，正好不需要建表。
    """
    _check_column_type_whitelist()
    with engine.connect() as conn:
        acquired = _acquire_process_lock(conn)
        try:
            _apply_column_and_index_migrations(conn)
        finally:
            _release_process_lock(conn, acquired)
