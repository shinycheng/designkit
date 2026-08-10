"""跨数据库兼容性与定时同步调度的单元测试（不联网、不需要真实 PG）。"""
import atexit
import os
import shutil
import tempfile
import unittest
from datetime import datetime, timedelta

# 导入 backend.app.models 会连带导入 config.py，而 config.py 在 import 期间就会去
# 建数据目录、改它的权限。测试不该碰用户放着网关密钥和生产数据的那个 data/ 目录，
# 所以在导入之前先把数据目录指到一个临时位置去。
# 用 setdefault：按 tests/README.md 的标准跑法本来就会显式设这个变量，那时不覆盖它。
if not os.environ.get("DESIGNKIT_DATA_DIR"):
    _TMP_DATA_DIR = tempfile.mkdtemp(prefix="dk-scheduler-data-")
    os.environ["DESIGNKIT_DATA_DIR"] = _TMP_DATA_DIR
    atexit.register(shutil.rmtree, _TMP_DATA_DIR, ignore_errors=True)

from sqlalchemy import or_, select
from sqlalchemy import update as sa_update
from sqlalchemy.dialects import postgresql, sqlite
from sqlalchemy.schema import CreateTable

from backend.app.models import ApiKey, Base, GenerationJob, PromptTemplate, SyncState


class PostgresCompatibilityTests(unittest.TestCase):
    """本机没有 PG 服务器，用方言编译静态验证 SQL/DDL 的可移植性。"""

    def test_all_tables_create_on_postgres(self):
        for table in Base.metadata.sorted_tables:
            CreateTable(table).compile(dialect=postgresql.dialect())

    def test_atomic_job_claim_compiles_on_both(self):
        stmt = (
            sa_update(GenerationJob)
            .where(GenerationJob.id == "x", GenerationJob.status == "pending")
            .values(status="processing", started_at=datetime.utcnow())
        )
        for dialect in (postgresql.dialect(), sqlite.dialect()):
            stmt.compile(dialect=dialect)

    def test_scheduler_lock_compiles_on_both(self):
        now = datetime.utcnow()
        stmt = (
            sa_update(SyncState)
            .where(
                SyncState.name == "inspiration",
                (SyncState.lock_until.is_(None)) | (SyncState.lock_until < now),
            )
            .values(lock_until=now + timedelta(minutes=30))
        )
        for dialect in (postgresql.dialect(), sqlite.dialect()):
            stmt.compile(dialect=dialect)

    def test_quota_update_compiles_on_both(self):
        stmt = (
            sa_update(ApiKey)
            .where(ApiKey.id == 1, ApiKey.used_month < 100)
            .values(used_month=ApiKey.used_month + 1)
        )
        for dialect in (postgresql.dialect(), sqlite.dialect()):
            stmt.compile(dialect=dialect)

    def test_search_is_case_insensitive_on_postgres(self):
        # PG 的 LIKE 区分大小写，必须用 ILIKE；SQLite 侧走 lower() 比较。
        # 若哪天有人把 icontains 改回 contains，这条测试会失败。
        clause = PromptTemplate.name.icontains("Abc", autoescape=True)
        pg_sql = str(clause.compile(dialect=postgresql.dialect()))
        sqlite_sql = str(clause.compile(dialect=sqlite.dialect()))
        self.assertIn("ILIKE", pg_sql.upper())
        self.assertIn("LOWER", sqlite_sql.upper())

    def test_adaptable_filter_compiles(self):
        clause = or_(*[
            PromptTemplate.prompt_template.icontains(kw, autoescape=True)
            for kw in ("uploaded image", "reference image", "上传的图")
        ])
        stmt = select(PromptTemplate).where(clause)
        for dialect in (postgresql.dialect(), sqlite.dialect()):
            stmt.compile(dialect=dialect)


class SchedulerDueTests(unittest.TestCase):
    """只测纯逻辑（到期判断），不碰数据库与网络。"""

    @staticmethod
    def _row(**kw):
        row = SyncState(name="inspiration")
        row.last_success_at = kw.get("last_success_at")
        row.last_started_at = kw.get("last_started_at")
        row.last_status = kw.get("last_status", "idle")
        row.consecutive_failures = kw.get("consecutive_failures", 0)
        return row

    def test_never_synced_is_due_now(self):
        from backend.app.services.scheduler import _due_at
        self.assertLessEqual(_due_at(self._row(), 12), datetime.utcnow())

    def test_recent_success_not_due(self):
        from backend.app.services.scheduler import _due_at
        row = self._row(last_success_at=datetime.utcnow(), last_status="success")
        self.assertGreater(_due_at(row, 12), datetime.utcnow() + timedelta(hours=11))

    def test_failure_uses_backoff_not_interval(self):
        from backend.app.services.scheduler import FAILURE_BACKOFF, _due_at
        started = datetime.utcnow()
        row = self._row(
            last_success_at=started - timedelta(days=5),
            last_started_at=started, last_status="failed", consecutive_failures=1,
        )
        # 失败后应按退避表（15 分钟）重试，而不是等满 12 小时
        self.assertAlmostEqual(
            (_due_at(row, 12) - started).total_seconds(),
            FAILURE_BACKOFF[0].total_seconds(), delta=2,
        )

    def test_backoff_caps_at_last_entry(self):
        from backend.app.services.scheduler import FAILURE_BACKOFF, _due_at
        started = datetime.utcnow()
        row = self._row(last_started_at=started, last_status="failed",
                        consecutive_failures=99, last_success_at=started)
        self.assertAlmostEqual(
            (_due_at(row, 12) - started).total_seconds(),
            FAILURE_BACKOFF[-1].total_seconds(), delta=2,
        )



class SyncLockTests(unittest.TestCase):
    """同步互斥：手动与自动必须共用同一把锁，否则会把上万条灵感库写重复。"""

    def setUp(self):
        import sqlalchemy as sa
        from backend.app.database import SessionLocal, engine
        from backend.app.services import scheduler as sch
        self.sa, self.sch = sa, sch
        # 先把表建出来再用。少了这一句，用一个全新的数据目录跑这组会四项一起报
        # `no such table: sync_state`——而 tests/README.md 推荐的恰恰就是
        # 「用临时数据目录跑」，等于谁照着文档做谁踩。
        # create_all 只补缺失的表，对已经有数据的目录是空操作，不会动任何现有数据。
        Base.metadata.create_all(bind=engine)
        self.db = SessionLocal()
        sch._ensure_row(self.db)
        self._reset()

    def tearDown(self):
        self._reset()
        self.db.close()

    def _reset(self):
        self.db.execute(self.sa.update(self.sch.SyncState).values(
            lock_until=None, lock_owner=None, last_status="idle",
            last_message="", consecutive_failures=0))
        self.db.commit()

    def test_manual_blocked_while_auto_holds_lock(self):
        self.assertTrue(self.sch._acquire(self.db, "auto-x"))
        self.assertFalse(self.sch._acquire(self.db, "manual-y"))

    def test_release_checks_ownership(self):
        self.sch._acquire(self.db, "owner-A")
        self.sch._release(self.db, "intruder", True, "不该生效")
        row = self.db.get(self.sch.SyncState, self.sch.TASK_NAME)
        self.db.refresh(row)
        self.assertEqual(row.lock_owner, "owner-A")
        self.assertIsNotNone(row.lock_until)

    def test_renew_only_by_owner(self):
        self.sch._acquire(self.db, "owner-A")
        self.assertTrue(self.sch._renew(self.db, "owner-A"))
        self.assertFalse(self.sch._renew(self.db, "owner-B"))

    def test_recover_stale_running(self):
        from datetime import datetime, timedelta
        self.db.execute(self.sa.update(self.sch.SyncState).values(
            last_status="running", lock_owner="dead",
            lock_until=datetime.utcnow() - timedelta(hours=1)))
        self.db.commit()
        self.sch.recover_stale(self.db)
        row = self.db.get(self.sch.SyncState, self.sch.TASK_NAME)
        self.db.refresh(row)
        self.assertEqual(row.last_status, "failed")
        self.assertIsNone(row.lock_until)

if __name__ == "__main__":
    unittest.main()
