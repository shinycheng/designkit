"""启动迁移（backend/app/migrations.py）的回归测试。

为什么要有这个文件
──────────────────────────────────────────────────────────────────────
`run_migrations()` 干的事情是：**给一个已经装着真实数据的老库补新列、补索引**。
它出问题的形态特别难查——本机 SQLite 一路绿灯，推到群晖的 PostgreSQL 上启动即崩；
而 docker-compose.yml 里写着 `restart: always`，运营同学只看得到「容器一直在重启」，
完全没法归因到某一行 SQL。在这个文件出现之前，这条路径从来没有被自动验证过。

**这个文件必须在 SQLite 和 PostgreSQL 上各跑一遍。只跑 SQLite 等于没跑。**
SQLite 对 DDL 里的类型名几乎来者不拒（写 `DATETIME`、`BOOLEAN DEFAULT 0` 它都收），
PostgreSQL 会当场拒绝。只跑 SQLite 不但测不出问题，反而更糟：会给人一种
「这条路已经有测试守着了」的错觉。

怎么跑
──────────────────────────────────────────────────────────────────────
默认只跑 SQLite（用临时文件建库，跑完自动删掉，不碰 data/ 目录）：

    .venv/bin/python -m unittest discover -s tests -p 'test_migrations.py' -v

加跑 PostgreSQL：先建一个**空的、名字里带 test 的**库，再把地址交给这个变量：

    DESIGNKIT_TEST_DATABASE_URL='postgresql+psycopg://designkit:密码@127.0.0.1:5432/designkit_test' \
      .venv/bin/python -m unittest discover -s tests -p 'test_migrations.py' -v

（用 docker-compose.yml 里那个 db 服务的话，进容器执行
 `psql -U designkit -c 'CREATE DATABASE designkit_test'` 建库即可，跑完可以留着复用。）

为什么不直接复用 DESIGNKIT_DATABASE_URL —— 三道保险，缺一不可
──────────────────────────────────────────────────────────────────────
这个测试会**建表、灌数据、删 schema**，跑到生产库上就是一场事故。所以：

  1. 变量名故意换成 DESIGNKIT_TEST_DATABASE_URL。DESIGNKIT_DATABASE_URL 指向的是
     真实数据库，而且 config.py 启动时会 load_dotenv()，`.env` 里填的值会被
     塞进环境变量里——两者共用一个名字，迟早有人在 NAS 上顺手跑一次测试就把
     14573 条灵感库连同历史记录全删了。
  2. 库名里必须含 "test"，否则测试直接报错、一行 SQL 都不执行。
  3. 所有表都建在一个专用 schema（dk_migration_test）里，跑完整个 schema 删掉。
     哪怕真有人把地址指到了别的库，也碰不到人家原有的表。

顺带一提：第 3 条同时还在**正面测试 P0-2 修的那个 bug**——迁移查列时必须带
`table_schema = current_schema()` 过滤，否则别的 schema 里有张同名老表就会让它
误判成「列已经有了」而静默跳过 ADD COLUMN（见下面的 ghost schema 用例）。
"""
import atexit
import logging
import os
import shutil
import tempfile
import unittest
from contextlib import contextmanager
from typing import Dict, List, Set
from unittest import mock

from sqlalchemy import create_engine, text
from sqlalchemy.engine import make_url
from sqlalchemy.exc import DatabaseError

# 导入 backend.app.models 会连带导入 config.py，而 config.py 在 import 期间就会去
# 建 data 目录、改它的权限。测试不该碰用户放着网关密钥和生产数据的那个目录，
# 所以在导入之前先把数据目录指到一个临时位置去。
# 用 setdefault：按 tests/README.md 的标准跑法本来就会显式设这个变量，那时不覆盖它。
if not os.environ.get("DESIGNKIT_DATA_DIR"):
    _TMP_DATA_DIR = tempfile.mkdtemp(prefix="dk-migration-data-")
    os.environ["DESIGNKIT_DATA_DIR"] = _TMP_DATA_DIR
    atexit.register(shutil.rmtree, _TMP_DATA_DIR, ignore_errors=True)

from backend.app import migrations  # noqa: E402
from backend.app.migrations import _RiskyIndex, run_migrations  # noqa: E402
from backend.app.models import Base  # noqa: E402

# 加跑 PostgreSQL 的开关（见文件头「怎么跑」）
PG_URL = os.environ.get("DESIGNKIT_TEST_DATABASE_URL", "").strip()

# PostgreSQL 侧所有测试表都关在这个 schema 里，跑完整个删掉
PG_TEST_SCHEMA = "dk_migration_test"
# 专门用来复现「别的 schema 里有张同名表」的干扰 schema
PG_GHOST_SCHEMA = "dk_migration_ghost"


# ══════════════════════════════════════════════════════════════════════
# 老库长什么样：一份**冻结的历史快照**
# ══════════════════════════════════════════════════════════════════════
# 下面这些 CREATE TABLE 是「NAS 上那个已经在跑的老库」的样子，故意用手写死的
# DDL，而**不是**从 models.py 反推出来的。这一点是这个文件里最重要的设计决定：
#
#   如果从模型反推（模型的列减去 _COLUMN_MIGRATIONS 里登记过的列），那么有人
#   往模型里加了一列却忘了登记迁移时，反推出来的「老表」也会自动带上那一列，
#   测试照样绿——恰恰在最该报警的地方失效了。
#
# 冻结成快照之后，判定规则就变成了真正想要的那条：
#   **models.py 里声明的每一列、每一个索引，跑完 run_migrations 之后都必须在库里。**
# 加了列忘了登记迁移 → 老表里没有 → 测试立刻红。这正是线上会 500 的那种 bug。
#
# 所以：**改 models.py 时不要来同步改这里**。这里只有一种情况需要动——
# 当 _COLUMN_MIGRATIONS 开始给一张新的表加列时，把那张表**当时的样子**补进来
# （下面 SnapshotCoverageTests 会盯着，漏了会直接报错并告诉你怎么办）。
#
# 几条书写约定：
#   * 不写外键约束。迁移只关心列和索引，带上外键反而要操心建表顺序。
#   * 时间列一律写 TIMESTAMP，不写 DATETIME —— 后者 PostgreSQL 不认。
#   * 除主键外不写 NOT NULL / DEFAULT，省得踩 `BOOLEAN DEFAULT 0` 那类方言差异；
#     这个测试只看列在不在，不看约束。
_OLD_SCHEMA_TABLES: Dict[str, str] = {
    # 老版本的 prompt_templates：还没有 source / source_ref / source_slugs
    "prompt_templates": """
        CREATE TABLE prompt_templates (
            id INTEGER PRIMARY KEY,
            name VARCHAR(128),
            category_id INTEGER,
            description TEXT,
            prompt_template TEXT,
            variables JSON,
            default_params JSON,
            requires_input_image BOOLEAN,
            thumbnail_path VARCHAR(256),
            is_enabled BOOLEAN,
            sort INTEGER,
            created_at TIMESTAMP,
            updated_at TIMESTAMP
        )
    """,
    # 老版本的 generation_jobs：还没有 prompt_sent
    "generation_jobs": """
        CREATE TABLE generation_jobs (
            id VARCHAR(32) PRIMARY KEY,
            source VARCHAR(8),
            user_id INTEGER,
            api_key_id INTEGER,
            template_id INTEGER,
            template_name VARCHAR(128),
            prompt_final TEXT,
            params JSON,
            input_paths JSON,
            status VARCHAR(16),
            error TEXT,
            attempts INTEGER,
            next_attempt_at TIMESTAMP,
            callback_url VARCHAR(512),
            external_ref VARCHAR(128),
            webhook_status VARCHAR(64),
            created_at TIMESTAMP,
            started_at TIMESTAMP,
            finished_at TIMESTAMP
        )
    """,
    # 老版本的 sync_state：还没有 lock_owner
    "sync_state": """
        CREATE TABLE sync_state (
            name VARCHAR(64) PRIMARY KEY,
            lock_until TIMESTAMP,
            last_started_at TIMESTAMP,
            last_success_at TIMESTAMP,
            last_status VARCHAR(16),
            last_message TEXT,
            consecutive_failures INTEGER
        )
    """,
    # api_keys 今天还没有任何迁移登记，快照与模型一致。提前放进来是因为
    # 下一步（每用户网关 Key）要给它加 user_id 列——到时候这张表已经在快照里，
    # 那条迁移一写出来就自动被覆盖到，不用再回头补测试。
    "api_keys": """
        CREATE TABLE api_keys (
            id INTEGER PRIMARY KEY,
            name VARCHAR(64),
            key_prefix VARCHAR(16),
            key_hash VARCHAR(64),
            webhook_secret VARCHAR(64),
            is_active BOOLEAN,
            monthly_quota INTEGER,
            used_total INTEGER,
            used_month INTEGER,
            used_month_key VARCHAR(8),
            last_used_at TIMESTAMP,
            created_at TIMESTAMP
        )
    """,
}

# 老库里**本来就有**的索引。老库当年是 create_all 建出来的，模型上标了 index=True
# 的列当时就带着索引，所以快照里也得有；缺了的话「模型声明的索引都要在」这条断言
# 会误报成迁移的锅。
# 注意这里没有 prompt_templates 的两条 —— 它们正是这次迁移要补的，
# 由 _INDEX_MIGRATIONS_SAFE 负责建出来。
_OLD_SCHEMA_INDEXES: List[str] = [
    "CREATE INDEX ix_generation_jobs_status ON generation_jobs (status)",
    "CREATE INDEX ix_generation_jobs_created_at ON generation_jobs (created_at)",
    "CREATE UNIQUE INDEX ix_api_keys_key_hash ON api_keys (key_hash)",
]


@contextmanager
def _capture_migration_logs(level=logging.INFO):
    """把 migrations 打的日志收下来，顺便别让它糊在测试输出里。

    没直接用 unittest 的 assertLogs：它在「一条日志都没有」时会判定失败，
    而这里恰恰有一条断言就是「第二次跑不该再打补列日志」。
    """
    logger = logging.getLogger("designkit.migrations")
    records: List[logging.LogRecord] = []

    class _Collector(logging.Handler):
        def emit(self, record):
            records.append(record)

    handler = _Collector()
    old_handlers, old_level, old_propagate = logger.handlers, logger.level, logger.propagate
    logger.handlers, logger.propagate = [handler], False
    logger.setLevel(level)
    try:
        yield records
    finally:
        logger.handlers, logger.propagate = old_handlers, old_propagate
        logger.setLevel(old_level)


def _messages(records: List[logging.LogRecord], level: int) -> List[str]:
    return [r.getMessage() for r in records if r.levelno >= level]


# ══════════════════════════════════════════════════════════════════════
# 与数据库后端无关的检查（不连库，SQLite / PostgreSQL 结论一样）
# ══════════════════════════════════════════════════════════════════════

class ColumnTypeWhitelistTests(unittest.TestCase):
    """开机自检：给老表加列只许用白名单里的类型。

    这道自检是「本机能替 NAS 提前报错」的唯一手段，所以它自己也得有测试守着：
    真出事的写法（DATETIME / BOOLEAN DEFAULT 0）必须被它拦下来。
    """

    def test_current_definitions_pass(self):
        # 仓库里现有的迁移定义必须全部合规，否则下次启动就崩了
        migrations._check_column_type_whitelist()

    def test_datetime_is_rejected(self):
        # DATETIME：SQLite 收、PostgreSQL 根本没这个类型名
        bad = [("generation_jobs", "some_time", "DATETIME")]
        with mock.patch.object(migrations, "_COLUMN_MIGRATIONS", bad):
            with self.assertRaises(ValueError) as ctx:
                migrations._check_column_type_whitelist()
        self.assertIn("TIMESTAMP", str(ctx.exception))

    def test_boolean_default_zero_is_rejected(self):
        # BOOLEAN DEFAULT 0：PostgreSQL 不会把整数 0 隐式转成布尔
        bad = [("generation_jobs", "some_flag", "BOOLEAN DEFAULT 0")]
        with mock.patch.object(migrations, "_COLUMN_MIGRATIONS", bad):
            with self.assertRaises(ValueError):
                migrations._check_column_type_whitelist()


class SnapshotCoverageTests(unittest.TestCase):
    """盯着上面那份「老库快照」别漏表。"""

    def test_every_migrated_table_has_a_snapshot(self):
        missing = sorted({t for t, _c, _d in migrations._COLUMN_MIGRATIONS} - set(_OLD_SCHEMA_TABLES))
        self.assertEqual(
            missing, [],
            "这些表开始走 ALTER TABLE 加列了，但 tests/test_migrations.py 里没有它们的"
            "老库快照，等于这几条迁移没人测：%s。"
            "办法：把这些表**加列之前**的建表语句抄进 _OLD_SCHEMA_TABLES，"
            "对应的老索引抄进 _OLD_SCHEMA_INDEXES。" % missing,
        )

    def test_snapshot_tables_still_exist_in_models(self):
        # 快照里的表被删/改名了就该有人来处理这个文件，而不是留着测一张不存在的表
        unknown = sorted(set(_OLD_SCHEMA_TABLES) - set(Base.metadata.tables))
        self.assertEqual(
            unknown, [],
            "_OLD_SCHEMA_TABLES 里这几张表在 models.py 里已经找不到了：%s" % unknown,
        )


# ══════════════════════════════════════════════════════════════════════
# 真连库跑的用例（SQLite 与 PostgreSQL 各跑一遍）
# ══════════════════════════════════════════════════════════════════════

class _MigrationCaseMixin(object):
    """两个后端共用的用例。

    子类负责提供 self.engine（每个用例一个干净的空库）以及
    _columns() / _indexes() 两个查询方法——这两个方法故意**不复用**
    migrations.py 里的实现，否则被测代码查错了地方，测试也跟着查错地方，
    永远自洽、永远绿。
    """

    # ---------------------------------------------------------- 工具

    def _build_old_database(self) -> None:
        """造出一个「老库」：只有老列老索引，然后按真实启动顺序跑 create_all。

        create_all 只补缺失的**表**（users、uploads 这些新表会建出来），
        已经存在的老表它一列都不会加——这正是 run_migrations 存在的理由。
        """
        with self.engine.begin() as conn:
            for ddl in _OLD_SCHEMA_TABLES.values():
                conn.execute(text(ddl))
            for ddl in _OLD_SCHEMA_INDEXES:
                conn.execute(text(ddl))
        Base.metadata.create_all(bind=self.engine)

    def _insert_template(self, row_id: int, name: str) -> None:
        with self.engine.begin() as conn:
            conn.execute(
                text(
                    "INSERT INTO prompt_templates (id, name, prompt_template) "
                    "VALUES (:i, :n, :p)"
                ),
                {"i": row_id, "n": name, "p": "随便一句提示词"},
            )

    def _registered_columns(self):
        return [(t, c) for t, c, _ddl in migrations._COLUMN_MIGRATIONS]

    # ---------------------------------------------------------- 用例

    def test_old_database_gets_every_registered_column(self):
        self._build_old_database()

        # 先证明这个用例本身有效：升级之前，这些列确实一个都不在。
        # 少了这一步，哪天 create_all 或快照被人改成「老库其实已经有新列了」，
        # 后面的断言会全部空转通过。
        for table, column in self._registered_columns():
            self.assertNotIn(
                column, self._columns(table),
                "老库快照里不该有 %s.%s，否则这条迁移等于没被测到" % (table, column),
            )

        run_migrations(self.engine)

        for table, column in self._registered_columns():
            self.assertIn(
                column, self._columns(table),
                "升级后 %s 表仍然缺列 %s" % (table, column),
            )

    def test_every_model_column_exists_after_upgrade(self):
        """models.py 里声明的列，升级完必须在库里 —— 这条是真正的护栏。

        典型事故：有人给模型加了一列，忘了往 _COLUMN_MIGRATIONS 登记。
        本机 `rm -rf data` 重来一遍是 create_all 建的新表，带着新列，一切正常；
        NAS 上是老表，列压根没加上，等到某个接口去读它才 500。
        """
        self._build_old_database()
        run_migrations(self.engine)

        for table in sorted(_OLD_SCHEMA_TABLES):
            actual = self._columns(table)
            for column in Base.metadata.tables[table].columns:
                self.assertIn(
                    column.name, actual,
                    "models.py 的 %s 表声明了 %s 列，但升级完的老库里没有。"
                    "多半是加了模型字段却忘了往 backend/app/migrations.py 的 "
                    "_COLUMN_MIGRATIONS 里登记。" % (table, column.name),
                )

    def test_every_model_index_exists_after_upgrade(self):
        """索引同理：模型上标了 index=True，升级完老库里也得有。

        索引缺了不会报错，只会让灵感库那种上万条的表越查越慢，
        而且慢得毫无征兆——所以只能靠测试盯着。
        """
        self._build_old_database()
        run_migrations(self.engine)

        actual = self._indexes()
        for table in sorted(_OLD_SCHEMA_TABLES):
            for index in Base.metadata.tables[table].indexes:
                self.assertIn(
                    index.name, actual,
                    "models.py 的 %s 表声明了索引 %s，但升级完的老库里没有。"
                    "给老表新加的索引要往 _INDEX_MIGRATIONS_SAFE 里登记。"
                    % (table, index.name),
                )

        # 「可能撞上存量重复值」的那一组，在干净库上应当照常建出来
        for item in migrations._INDEX_MIGRATIONS_RISKY:
            self.assertIn(item.name, actual, "干净库上 %s 也没建出来" % item.name)

    def test_running_twice_is_idempotent(self):
        """重复启动不能报错，也不该重复干活。

        容器重启是家常便饭，run_migrations 每次启动都会跑一遍。
        """
        self._build_old_database()
        run_migrations(self.engine)
        before_cols = {t: self._columns(t) for t in _OLD_SCHEMA_TABLES}
        before_idx = self._indexes()

        with _capture_migration_logs() as records:
            run_migrations(self.engine)  # 第二遍，不许抛异常

        self.assertEqual(before_cols, {t: self._columns(t) for t in _OLD_SCHEMA_TABLES})
        self.assertEqual(before_idx, self._indexes())
        # 第二遍不该再打「新增列」的日志，否则说明「列已存在」的判断没生效
        added = [m for m in _messages(records, logging.INFO) if "新增列" in m]
        self.assertEqual(added, [], "第二次启动又去补了一遍列：%s" % added)

    def test_risky_index_failure_does_not_block_the_columns(self):
        """唯一索引撞上存量重复值时：不许抛异常，更不许把补列一起带走。

        这是 P0-2 拆事务的验收点。不拆的话，一条建不出来的唯一索引会让整批 DDL
        回滚 → 启动被挡死 → docker 的 restart: always 无限重启。
        """
        self._build_old_database()
        # 造两条重名记录，让下面这条唯一索引必然建不出来
        self._insert_template(1, "撞名的模板")
        self._insert_template(2, "撞名的模板")

        doomed = _RiskyIndex(
            name="uq_test_dup_name",
            ddl="CREATE UNIQUE INDEX IF NOT EXISTS uq_test_dup_name ON prompt_templates (name)",
            table="prompt_templates",
            columns=("name",),
            what="测试用：模板名有重复",
            howto="测试用：无需处理",
        )
        with mock.patch.object(migrations, "_INDEX_MIGRATIONS_RISKY", [doomed]):
            with _capture_migration_logs(logging.WARNING) as records:
                run_migrations(self.engine)  # 不许抛

        # 补列照常生效（证明它跑在独立事务里，没被索引失败连累）
        for table, column in self._registered_columns():
            self.assertIn(column, self._columns(table))
        # 普通索引也照常建上
        self.assertIn("ix_prompt_templates_source", self._indexes())
        # 那条注定失败的索引确实没建上
        self.assertNotIn("uq_test_dup_name", self._indexes())

        # 日志得是人话，并且数得出到底有几组重复
        warnings = _messages(records, logging.WARNING)
        self.assertEqual(len(warnings), 1, "应当只打一条提醒，实际：%s" % warnings)
        self.assertIn("测试用：模板名有重复", warnings[0])
        self.assertIn("发现 1 组重复", warnings[0])
        self.assertIn("照常使用", warnings[0])

    def test_risky_index_is_created_once_duplicates_are_cleaned(self):
        """上面那条警告向使用者承诺了「清理完重启一次就会自动补上」，这里验证它是真的。"""
        self._build_old_database()
        self._insert_template(1, "撞名的模板")
        self._insert_template(2, "撞名的模板")

        doomed = _RiskyIndex(
            name="uq_test_dup_name",
            ddl="CREATE UNIQUE INDEX IF NOT EXISTS uq_test_dup_name ON prompt_templates (name)",
            table="prompt_templates",
            columns=("name",),
        )
        with mock.patch.object(migrations, "_INDEX_MIGRATIONS_RISKY", [doomed]):
            with _capture_migration_logs(logging.WARNING):
                run_migrations(self.engine)
            self.assertNotIn("uq_test_dup_name", self._indexes())

            # 清掉重复行，再启动一次
            with self.engine.begin() as conn:
                conn.execute(text("DELETE FROM prompt_templates WHERE id = 2"))
            with _capture_migration_logs(logging.WARNING) as records:
                run_migrations(self.engine)

        self.assertIn("uq_test_dup_name", self._indexes())
        self.assertEqual(_messages(records, logging.WARNING), [])

    def test_bad_add_column_blocks_startup(self):
        """补列失败必须抛出去挡死启动（fail-closed），不许吞掉。

        吞掉的话，问题会在几天后以「某个接口莫名 500」的形式爆出来，离根因十万八千里。
        这里用「给有数据的表加 NOT NULL 无默认值的列」来制造失败——
        SQLite 和 PostgreSQL 都会拒绝，而且它能通过类型白名单自检（类型是 VARCHAR），
        所以拦住它的确实是数据库本身，测的是真链路。
        """
        self._build_old_database()
        self._insert_template(1, "随便一条")

        bad = list(migrations._COLUMN_MIGRATIONS) + [
            ("prompt_templates", "must_fail", "VARCHAR(16) NOT NULL")
        ]
        with mock.patch.object(migrations, "_COLUMN_MIGRATIONS", bad):
            with self.assertRaises(DatabaseError):
                run_migrations(self.engine)

        # 把坏定义拿掉再启动一次要能自愈：崩掉的那次可能已经加上了一部分列
        # （pysqlite 不把 DDL 包进事务，PostgreSQL 会整体回滚，两边行为并不一致），
        # 靠的就是补列本身幂等 + 容器会自动重启。
        run_migrations(self.engine)
        for table, column in self._registered_columns():
            self.assertIn(column, self._columns(table))
        self.assertNotIn("must_fail", self._columns("prompt_templates"))


class SqliteMigrationTests(_MigrationCaseMixin, unittest.TestCase):
    """SQLite（monica 的 Mac / 单机部署）。每个用例一个全新的临时库文件。"""

    def setUp(self):
        self._dir = tempfile.mkdtemp(prefix="dk-migration-sqlite-")
        self.engine = create_engine("sqlite:///" + os.path.join(self._dir, "t.db"), future=True)

    def tearDown(self):
        self.engine.dispose()
        shutil.rmtree(self._dir, ignore_errors=True)

    def _columns(self, table: str) -> Set[str]:
        with self.engine.connect() as conn:
            return {row[1] for row in conn.execute(text("PRAGMA table_info(%s)" % table))}

    def _indexes(self) -> Set[str]:
        with self.engine.connect() as conn:
            rows = conn.execute(
                text("SELECT name FROM sqlite_master WHERE type = 'index' AND name IS NOT NULL")
            )
            # sqlite_autoindex_* 是 UNIQUE 约束自动生成的，不是我们建的，排除掉
            return {r[0] for r in rows if not r[0].startswith("sqlite_autoindex_")}


@unittest.skipUnless(
    PG_URL,
    "没设 DESIGNKIT_TEST_DATABASE_URL，跳过 PostgreSQL 那一半。"
    "注意：只跑 SQLite 是测不出 PostgreSQL 才会拒绝的 DDL 的，"
    "推送前请按 tests/README.md 把这半边也跑一遍。",
)
class PostgresMigrationTests(_MigrationCaseMixin, unittest.TestCase):
    """PostgreSQL（群晖 NAS 生产环境）。

    每个用例把整个 dk_migration_test schema 删掉重建，等于一个全新空库；
    连接固定 search_path 到这个 schema，所有建表删表都关在里面。
    """

    @classmethod
    def setUpClass(cls):
        url = make_url(PG_URL)
        # 允许直接粘 postgresql://... —— 项目里装的驱动是 psycopg(3)，补上驱动名
        if url.drivername == "postgresql":
            url = url.set(drivername="postgresql+psycopg")
        if not url.database or "test" not in url.database.lower():
            raise RuntimeError(
                "DESIGNKIT_TEST_DATABASE_URL 指向的库名是 %r，里面没有 test 字样，"
                "本测试拒绝执行。这个测试会建表、灌数据、删 schema，"
                "指错库就是一场事故。请单独建一个空库（例如 designkit_test）再跑。"
                % (url.database,)
            )
        cls._url = url
        # 管理连接：不设 search_path，专门用来建/删测试 schema
        cls._admin = create_engine(url, future=True)

    @classmethod
    def tearDownClass(cls):
        with cls._admin.begin() as conn:
            conn.execute(text("DROP SCHEMA IF EXISTS %s CASCADE" % PG_TEST_SCHEMA))
            conn.execute(text("DROP SCHEMA IF EXISTS %s CASCADE" % PG_GHOST_SCHEMA))
        cls._admin.dispose()

    def setUp(self):
        with self._admin.begin() as conn:
            conn.execute(text("DROP SCHEMA IF EXISTS %s CASCADE" % PG_TEST_SCHEMA))
            conn.execute(text("DROP SCHEMA IF EXISTS %s CASCADE" % PG_GHOST_SCHEMA))
            conn.execute(text("CREATE SCHEMA %s" % PG_TEST_SCHEMA))
        # -c search_path=... 是 libpq 的连接参数，psycopg2/psycopg3 都吃这一套；
        # 池里每条连接都会带上，等于把整个测试关进这个 schema。
        # 这里**故意不用 NullPool**：连接一断，PostgreSQL 会自动清掉它持有的咨询锁，
        # 那样「锁到底有没有被正确释放」就测不出来了。
        self.engine = create_engine(
            self._url,
            connect_args={"options": "-csearch_path=%s" % PG_TEST_SCHEMA},
            future=True,
        )

    def tearDown(self):
        self.engine.dispose()
        with self._admin.begin() as conn:
            conn.execute(text("DROP SCHEMA IF EXISTS %s CASCADE" % PG_TEST_SCHEMA))
            conn.execute(text("DROP SCHEMA IF EXISTS %s CASCADE" % PG_GHOST_SCHEMA))

    def _columns(self, table: str) -> Set[str]:
        # 写死 schema 名去查，不用 current_schema()——被测代码正是靠
        # current_schema() 定位的，这里再用一遍就成了自证清白
        with self.engine.connect() as conn:
            rows = conn.execute(
                text(
                    "SELECT column_name FROM information_schema.columns "
                    "WHERE table_name = :t AND table_schema = :s"
                ),
                {"t": table, "s": PG_TEST_SCHEMA},
            )
            return {r[0] for r in rows}

    def _indexes(self) -> Set[str]:
        with self.engine.connect() as conn:
            rows = conn.execute(
                text("SELECT indexname FROM pg_indexes WHERE schemaname = :s"),
                {"s": PG_TEST_SCHEMA},
            )
            # xxx_pkey 是主键自带的索引，不是我们建的
            return {r[0] for r in rows if not r[0].endswith("_pkey")}

    # ------------------------------------------------ 只有 PostgreSQL 才有的坑

    def test_same_table_in_another_schema_does_not_hide_a_missing_column(self):
        """别的 schema 里有张同名表时，不许把 ADD COLUMN 静默跳过。

        information_schema.columns 会返回当前角色**能看到的所有 schema** 下的同名表。
        查列时不带 table_schema = current_schema() 过滤，就会误判成「列已经有了」，
        然后应用真去读那一列时才报错——那时候现场离根因已经很远了。
        群晖上一个库里放多个 schema（或者迁移脚本留下个半成品）都会踩到。
        """
        self._build_old_database()
        with self._admin.begin() as conn:
            conn.execute(text("CREATE SCHEMA %s" % PG_GHOST_SCHEMA))
            # 这张幌子表**已经有** prompt_sent 列
            conn.execute(text(
                "CREATE TABLE %s.generation_jobs "
                "(id VARCHAR(32) PRIMARY KEY, prompt_sent TEXT)" % PG_GHOST_SCHEMA
            ))

        run_migrations(self.engine)

        self.assertIn(
            "prompt_sent", self._columns("generation_jobs"),
            "被别的 schema 里的同名表骗过去了：真正在用的表没补上列",
        )

    def test_second_process_waits_and_then_fails_readably(self):
        """两个进程同时启动时，后来的那个要排队；等太久要报中文人话并且什么都别改。

        补列是典型的「先查后改」：两个进程都查到「列不存在」，第二个 ALTER 时会报
        duplicate column 直接起不来。今天是单进程 uvicorn 撞不上，但哪天有人为了扛并发
        加个 --workers，问题就会以「容器起不来」的形式爆出来。
        """
        self._build_old_database()
        key = migrations._PG_ADVISORY_LOCK_KEY

        blocker = self.engine.connect()
        blocker.execute(text("SELECT pg_advisory_lock(:k)"), {"k": key})
        blocker.commit()
        try:
            # 把等待时间调短，不然这条用例要跑满两分钟
            with mock.patch.object(migrations, "_LOCK_WAIT_SECONDS", 1), \
                    mock.patch.object(migrations, "_LOCK_POLL_SECONDS", 0.1):
                with _capture_migration_logs():
                    with self.assertRaises(RuntimeError) as ctx:
                        run_migrations(self.engine)
            message = str(ctx.exception)
            self.assertIn("数据库升级", message)
            self.assertIn("重启", message)  # 得告诉使用者怎么办
            # 拿不到锁就一列都不许动
            self.assertNotIn("prompt_sent", self._columns("generation_jobs"))
        finally:
            blocker.execute(text("SELECT pg_advisory_unlock(:k)"), {"k": key})
            blocker.commit()
            blocker.close()

        # 前面那个进程升级完、锁放开之后，本进程重启一次要能正常升上去
        run_migrations(self.engine)
        self.assertIn("prompt_sent", self._columns("generation_jobs"))

    def test_lock_is_released_after_migration(self):
        """升级完必须把锁还回去，否则同一条连接留在池里，后面的进程会一直排队到超时。"""
        self._build_old_database()
        run_migrations(self.engine)

        with self.engine.connect() as conn:
            held = conn.execute(
                text(
                    "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' "
                    "AND ((classid::bigint << 32) | objid::bigint) = :k"
                ),
                {"k": migrations._PG_ADVISORY_LOCK_KEY},
            ).scalar()
        self.assertEqual(held, 0, "升级完之后还有 %s 把锁没释放" % held)


if __name__ == "__main__":
    unittest.main()
