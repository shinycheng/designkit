"""登录限速落库 + 取真实客户端 IP 的单元测试（不联网、不需要起服务）。

覆盖三块：
  1. services/ratelimit.py：计数、封锁、过期、清理、上限、多线程下的原子性；
  2. deps.client_ip：X-Forwarded-For 必须从右往左数，最左边那段是可伪造的；
  3. 跨库兼容：新表的 DDL 和这里所有的 SQL 都要能在 PostgreSQL 上编译。
"""
import atexit
import os
import shutil
import tempfile
import threading
import unittest
from datetime import datetime, timedelta

# 导入 backend.app.* 会连带导入 config.py，而它在 import 期间就会去建数据目录、
# 改权限。测试绝不能碰用户放着网关密钥和生产数据的那个 data/ 目录，
# 所以在导入之前先把数据目录指到临时位置。用 setdefault：按 tests/README.md
# 的标准跑法本来就会显式设这个变量，那时不覆盖它。
if not os.environ.get("DESIGNKIT_DATA_DIR"):
    _TMP_DATA_DIR = tempfile.mkdtemp(prefix="dk-ratelimit-data-")
    os.environ["DESIGNKIT_DATA_DIR"] = _TMP_DATA_DIR
    atexit.register(shutil.rmtree, _TMP_DATA_DIR, ignore_errors=True)

from sqlalchemy import create_engine, event, select
from sqlalchemy import update as sa_update
from sqlalchemy.dialects import postgresql, sqlite
from sqlalchemy.orm import sessionmaker
from sqlalchemy.schema import CreateTable

from backend.app.deps import client_ip
from backend.app.models import AppSetting, Base, RateLimitState
from backend.app.services import ratelimit


# ────────────────────────────── 测试脚手架 ──────────────────────────────

class _FakeClient(object):
    def __init__(self, host):
        self.host = host


class _FakeRequest(object):
    """只带 client 和 headers 的假请求——client_ip 用到的就这两样。"""

    def __init__(self, host="203.0.113.9", headers=None):
        self.client = _FakeClient(host) if host is not None else None
        self.headers = {}
        for key, value in (headers or {}).items():
            self.headers[key.lower()] = value


class _DbCase(unittest.TestCase):
    """每个测试一个独立的 SQLite 文件库，互不干扰。

    用文件库而不是 :memory:，是因为多线程那一项要让几个连接看到同一份数据。
    """

    def setUp(self):
        self.tmpdir = tempfile.mkdtemp(prefix="dk-ratelimit-")
        self.engine = create_engine(
            "sqlite:///" + os.path.join(self.tmpdir, "t.db"),
            connect_args={"check_same_thread": False, "timeout": 30},
            future=True,
        )

        @event.listens_for(self.engine, "connect")
        def _pragma(dbapi_connection, _record):
            cur = dbapi_connection.cursor()
            cur.execute("PRAGMA journal_mode=WAL")
            cur.execute("PRAGMA busy_timeout=30000")
            cur.close()

        Base.metadata.create_all(self.engine)
        # expire_on_commit=False 与 database.py 里的 SessionLocal 保持一致，
        # 否则测不出「提交后 ORM 缓存是旧的」这类问题。
        self.Session = sessionmaker(
            bind=self.engine, autoflush=False, expire_on_commit=False, future=True
        )
        self.db = self.Session()
        # 清理是按进程节流的（60 秒一次），逐个测试之间要放开，否则第二个测试
        # 里的 cleanup 会被上一个测试的时间戳挡掉。负无穷 = 从没清理过。
        ratelimit._gc_last_at = float("-inf")

    def tearDown(self):
        self.db.close()
        self.engine.dispose()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def _set_setting(self, key, value):
        self.db.add(AppSetting(key=key, value={"v": value}))
        self.db.commit()

    def _row(self, scope="login", key="1.2.3.4|alice"):
        return self.db.execute(
            select(RateLimitState).where(
                RateLimitState.scope == scope, RateLimitState.key == key
            )
        ).scalar_one_or_none()


# ────────────────────────────── 计数与封锁 ──────────────────────────────

class CountingTests(_DbCase):
    POLICY = ratelimit.Policy(max_attempts=3, window_seconds=600, block_seconds=900)

    def test_first_check_allows(self):
        self.assertTrue(ratelimit.check(self.db, "login", "ip|a", self.POLICY).allowed)

    def test_counts_accumulate(self):
        for expected in (1, 2):
            decision = ratelimit.record_failure(self.db, "login", "ip|a", self.POLICY)
            self.assertEqual(decision.attempts, expected)
            self.assertTrue(decision.allowed)

    def test_blocks_at_threshold(self):
        for _ in range(3):
            last = ratelimit.record_failure(self.db, "login", "ip|a", self.POLICY)
        self.assertFalse(last.allowed)
        self.assertGreater(last.retry_after, 0)
        self.assertIn("请", last.message)
        self.assertFalse(ratelimit.check(self.db, "login", "ip|a", self.POLICY).allowed)

    def test_block_message_mentions_minutes(self):
        for _ in range(3):
            ratelimit.record_failure(self.db, "login", "ip|a", self.POLICY)
        message = ratelimit.check(self.db, "login", "ip|a", self.POLICY).message
        self.assertIn("登录", message)
        self.assertIn("15 分钟", message)

    def test_other_key_unaffected(self):
        for _ in range(3):
            ratelimit.record_failure(self.db, "login", "ip|a", self.POLICY)
        self.assertTrue(ratelimit.check(self.db, "login", "ip|b", self.POLICY).allowed)

    def test_other_scope_unaffected(self):
        for _ in range(3):
            ratelimit.record_failure(self.db, "login", "ip|a", self.POLICY)
        self.assertTrue(ratelimit.check(self.db, "sms_code", "ip|a", self.POLICY).allowed)

    def test_reset_clears_row(self):
        for _ in range(3):
            ratelimit.record_failure(self.db, "login", "ip|a", self.POLICY)
        ratelimit.reset(self.db, "login", "ip|a")
        self.assertIsNone(self._row(key="ip|a"))
        self.assertTrue(ratelimit.check(self.db, "login", "ip|a", self.POLICY).allowed)

    def test_reset_of_unknown_key_is_noop(self):
        ratelimit.reset(self.db, "login", "never|seen")  # 不该抛

    def test_window_expiry_restarts_count(self):
        ratelimit.record_failure(self.db, "login", "ip|a", self.POLICY)
        ratelimit.record_failure(self.db, "login", "ip|a", self.POLICY)
        self._age_window(seconds=self.POLICY.window_seconds + 60)
        decision = ratelimit.record_failure(self.db, "login", "ip|a", self.POLICY)
        self.assertEqual(decision.attempts, 1)  # 重新从 1 开始数

    def test_block_survives_window_expiry(self):
        """封锁时间比窗口长时，窗口到期不能把封锁抹掉（否则等于偷偷缩短封锁）。"""
        policy = ratelimit.Policy(max_attempts=2, window_seconds=60, block_seconds=3600)
        for _ in range(2):
            ratelimit.record_failure(self.db, "login", "ip|a", policy)
        self._age_window(seconds=300)  # 窗口过期，但封锁还有 55 分钟
        self.assertFalse(ratelimit.check(self.db, "login", "ip|a", policy).allowed)
        self.assertFalse(ratelimit.record_failure(self.db, "login", "ip|a", policy).allowed)

    def test_unblocks_after_block_expires(self):
        policy = ratelimit.Policy(max_attempts=2, window_seconds=60, block_seconds=60)
        for _ in range(2):
            ratelimit.record_failure(self.db, "login", "ip|a", policy)
        self.db.execute(
            sa_update(RateLimitState).values(
                blocked_until=datetime.utcnow() - timedelta(seconds=1),
                window_start=datetime.utcnow() - timedelta(seconds=120),
            )
        )
        self.db.commit()
        self.assertTrue(ratelimit.check(self.db, "login", "ip|a", policy).allowed)

    def test_lowered_threshold_takes_effect_immediately(self):
        """管理员把阈值调小之后，已经攒够次数的人应该立刻被拦，而不是等下次失败。"""
        for _ in range(3):
            ratelimit.record_failure(self.db, "login", "ip|a", self.POLICY)
        ratelimit.reset(self.db, "login", "ip|a")
        loose = ratelimit.Policy(max_attempts=20, window_seconds=600, block_seconds=600)
        for _ in range(5):
            ratelimit.record_failure(self.db, "login", "ip|a", loose)
        strict = ratelimit.Policy(max_attempts=3, window_seconds=600, block_seconds=600)
        self.assertFalse(ratelimit.check(self.db, "login", "ip|a", strict).allowed)

    def _age_window(self, seconds):
        self.db.execute(
            sa_update(RateLimitState).values(
                window_start=datetime.utcnow() - timedelta(seconds=seconds)
            )
        )
        self.db.commit()


# ─────────────────────────── key 的上限与清理 ───────────────────────────

class BoundsTests(_DbCase):
    POLICY = ratelimit.Policy(max_attempts=3, window_seconds=600, block_seconds=600)

    def test_long_key_is_truncated_to_column_width(self):
        """key 由攻击者可控的内容拼成，超长必须先截断——PG 上超长会直接报错。"""
        ratelimit.record_failure(self.db, "login", "1.2.3.4|" + "x" * 5000, self.POLICY)
        stored = self.db.execute(select(RateLimitState.key)).scalars().all()
        self.assertEqual(len(stored), 1)
        self.assertLessEqual(len(stored[0]), ratelimit.MAX_KEY_LENGTH)

    def test_scope_is_normalized_and_bounded(self):
        ratelimit.record_failure(self.db, "LOGIN" + "z" * 100, "ip|a", self.POLICY)
        stored = self.db.execute(select(RateLimitState.scope)).scalars().all()
        self.assertLessEqual(len(stored[0]), 32)
        self.assertEqual(stored[0], stored[0].lower())

    def test_cleanup_removes_expired_rows(self):
        ratelimit.record_failure(self.db, "login", "ip|a", self.POLICY)
        self.db.execute(
            sa_update(RateLimitState).values(
                updated_at=datetime.utcnow() - timedelta(days=3)
            )
        )
        self.db.commit()
        removed = ratelimit.cleanup(self.db, "login", self.POLICY)
        self.assertEqual(removed, 1)
        self.assertIsNone(self._row(key="ip|a"))

    def test_cleanup_keeps_fresh_rows(self):
        ratelimit.record_failure(self.db, "login", "ip|a", self.POLICY)
        self.assertEqual(ratelimit.cleanup(self.db, "login", self.POLICY), 0)
        self.assertIsNotNone(self._row(key="ip|a"))

    def test_cleanup_caps_row_count(self):
        """光按时间删挡不住撞库：一分钟塞十万个新用户名进来，行行都没过期。"""
        original_cap = ratelimit.MAX_ROWS_PER_SCOPE
        ratelimit.MAX_ROWS_PER_SCOPE = 10
        try:
            now = datetime.utcnow()
            for i in range(25):
                self.db.add(RateLimitState(
                    scope="login", key="1.2.3.4|user%03d" % i,
                    window_start=now, attempts=1, blocked_until=None,
                    # 序号越小越旧，超出上限时该被删掉的正是这些
                    updated_at=now - timedelta(seconds=25 - i),
                ))
            self.db.commit()
            ratelimit.cleanup(self.db, "login", self.POLICY)
            left = self.db.execute(select(RateLimitState.key)).scalars().all()
            self.assertEqual(len(left), 10)
            self.assertIn("1.2.3.4|user024", left)   # 最近活动的留下
            self.assertNotIn("1.2.3.4|user000", left)  # 最久没动的被删
        finally:
            ratelimit.MAX_ROWS_PER_SCOPE = original_cap

    def test_table_stays_bounded_under_flood(self):
        """端到端地验一遍：狂灌不同 key，表不会无限涨。"""
        original_cap = ratelimit.MAX_ROWS_PER_SCOPE
        ratelimit.MAX_ROWS_PER_SCOPE = 20
        try:
            for i in range(200):
                ratelimit._gc_last_at = float("-inf")  # 关掉进程内节流，让每次都真的清
                ratelimit.record_failure(self.db, "login", "1.2.3.4|u%d" % i, self.POLICY)
            total = len(self.db.execute(select(RateLimitState.key)).scalars().all())
            self.assertLessEqual(total, 21)  # 上限 + 当前这一行
        finally:
            ratelimit.MAX_ROWS_PER_SCOPE = original_cap


# ────────────────────────────── 设置项 ──────────────────────────────

class PolicyTests(_DbCase):
    def test_defaults_match_previous_hardcoded_values(self):
        policy = ratelimit.policy_for(self.db, "login")
        self.assertEqual(policy.max_attempts, 10)
        self.assertEqual(policy.window_seconds, 15 * 60)
        self.assertEqual(policy.block_seconds, 15 * 60)

    def test_settings_override(self):
        self._set_setting("ratelimit_login_max_attempts", 3)
        self._set_setting("ratelimit_login_window_minutes", 2)
        self._set_setting("ratelimit_login_block_minutes", 30)
        policy = ratelimit.policy_for(self.db, "login")
        self.assertEqual((policy.max_attempts, policy.window_seconds, policy.block_seconds),
                         (3, 120, 1800))

    def test_unknown_scope_falls_back_to_default_policy(self):
        self.assertEqual(ratelimit.policy_for(self.db, "sms_code"), ratelimit.DEFAULT_POLICY)

    def test_garbage_setting_falls_back(self):
        self._set_setting("ratelimit_login_max_attempts", "十次")
        self.assertEqual(ratelimit.policy_for(self.db, "login").max_attempts, 10)

    def test_zero_is_clamped_so_nobody_gets_locked_out(self):
        self._set_setting("ratelimit_login_max_attempts", 0)
        self._set_setting("ratelimit_login_window_minutes", 0)
        policy = ratelimit.policy_for(self.db, "login")
        self.assertGreaterEqual(policy.max_attempts, 1)
        self.assertGreaterEqual(policy.window_seconds, 10)

    def test_trusted_proxy_hops_default_is_zero(self):
        self.assertEqual(ratelimit.trusted_proxy_hops(self.db), 0)

    def test_trusted_proxy_hops_from_settings(self):
        self._set_setting("trusted_proxy_hops", 2)
        self.assertEqual(ratelimit.trusted_proxy_hops(self.db), 2)

    def test_trusted_proxy_hops_rejects_negative(self):
        self._set_setting("trusted_proxy_hops", -5)
        self.assertEqual(ratelimit.trusted_proxy_hops(self.db), 0)


# ────────────────────────── 原子性（多线程） ──────────────────────────

class AtomicityTests(_DbCase):
    def test_concurrent_failures_are_not_lost(self):
        """并发下计数不能丢。

        「先 SELECT 再写回 +1」在这里会丢计数——攻击者并发撞库就能让计数永远追不上。
        条件 UPDATE + rowcount 才不会。
        """
        policy = ratelimit.Policy(max_attempts=1000, window_seconds=600, block_seconds=600)
        threads = []
        errors = []

        def worker():
            session = self.Session()
            try:
                for _ in range(10):
                    ratelimit.record_failure(session, "login", "1.2.3.4|alice", policy)
            except Exception as exc:  # noqa: BLE001
                errors.append(exc)
            finally:
                session.close()

        for _ in range(6):
            threads.append(threading.Thread(target=worker))
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()

        self.assertEqual(errors, [])
        row = self._row(key="1.2.3.4|alice")
        self.assertIsNotNone(row)
        self.assertEqual(row.attempts, 60)

    def test_survives_state_shared_across_sessions(self):
        """两个 Session（模拟两个进程）看到的是同一份计数，不是各存一份。"""
        policy = ratelimit.Policy(max_attempts=2, window_seconds=600, block_seconds=600)
        other = self.Session()
        try:
            ratelimit.record_failure(self.db, "login", "ip|a", policy)
            ratelimit.record_failure(other, "login", "ip|a", policy)
            self.assertFalse(ratelimit.check(other, "login", "ip|a", policy).allowed)
        finally:
            other.close()


# ─────────────────────────── 取真实客户端 IP ───────────────────────────

class ClientIpTests(unittest.TestCase):
    def test_no_proxy_uses_peer_address(self):
        request = _FakeRequest("203.0.113.9", {"X-Forwarded-For": "1.1.1.1"})
        self.assertEqual(client_ip(request, 0), "203.0.113.9")

    def test_one_hop_takes_rightmost(self):
        request = _FakeRequest("10.0.0.1", {"X-Forwarded-For": "1.1.1.1, 198.51.100.7"})
        self.assertEqual(client_ip(request, 1), "198.51.100.7")

    def test_spoofed_leftmost_is_ignored(self):
        """最左边那段是客户端自己写的，取它等于限速形同虚设。"""
        request = _FakeRequest("10.0.0.1", {
            "X-Forwarded-For": "9.9.9.9, 8.8.8.8, 198.51.100.7"
        })
        self.assertEqual(client_ip(request, 1), "198.51.100.7")

    def test_two_hops(self):
        request = _FakeRequest("10.0.0.1", {
            "X-Forwarded-For": "9.9.9.9, 198.51.100.7, 172.16.0.5"
        })
        self.assertEqual(client_ip(request, 2), "198.51.100.7")

    def test_header_shorter_than_hops_falls_back_to_peer(self):
        request = _FakeRequest("10.0.0.1", {"X-Forwarded-For": "198.51.100.7"})
        self.assertEqual(client_ip(request, 3), "10.0.0.1")

    def test_missing_header_falls_back_to_peer(self):
        self.assertEqual(client_ip(_FakeRequest("10.0.0.1"), 1), "10.0.0.1")

    def test_empty_header_falls_back_to_peer(self):
        request = _FakeRequest("10.0.0.1", {"X-Forwarded-For": "  ,  "})
        self.assertEqual(client_ip(request, 1), "10.0.0.1")

    def test_strips_port_from_ipv4(self):
        request = _FakeRequest("10.0.0.1", {"X-Forwarded-For": "198.51.100.7:51234"})
        self.assertEqual(client_ip(request, 1), "198.51.100.7")

    def test_strips_brackets_and_port_from_ipv6(self):
        request = _FakeRequest("10.0.0.1", {"X-Forwarded-For": "[2001:db8::1]:443"})
        self.assertEqual(client_ip(request, 1), "2001:db8::1")

    def test_keeps_bare_ipv6(self):
        request = _FakeRequest("10.0.0.1", {"X-Forwarded-For": "2001:db8::1"})
        self.assertEqual(client_ip(request, 1), "2001:db8::1")

    def test_missing_client_never_raises(self):
        self.assertEqual(client_ip(_FakeRequest(None), 0), "-")

    def test_absurdly_long_value_is_truncated(self):
        request = _FakeRequest("10.0.0.1", {"X-Forwarded-For": "x" * 9000})
        self.assertLessEqual(len(client_ip(request, 1)), 45)


# ─────────────────────────── 跨库兼容（PG） ───────────────────────────

class PostgresCompatibilityTests(unittest.TestCase):
    """本机没有 PG，用方言编译静态验证 DDL 与 SQL 的可移植性。
    （群晖生产跑的是 PostgreSQL，这里写错的话表现是容器无限重启。）"""

    def test_new_table_ddl_compiles_on_postgres(self):
        CreateTable(RateLimitState.__table__).compile(dialect=postgresql.dialect())

    def test_no_datetime_or_boolean_default_zero_in_ddl(self):
        ddl = str(CreateTable(RateLimitState.__table__).compile(dialect=postgresql.dialect()))
        self.assertNotIn("DATETIME", ddl.upper())
        self.assertNotIn("BOOLEAN DEFAULT 0", ddl.upper())

    def test_conditional_update_compiles_on_both(self):
        now = datetime.utcnow()
        stmt = (
            sa_update(RateLimitState)
            .where(
                RateLimitState.scope == "login",
                RateLimitState.key == "ip|a",
                RateLimitState.window_start > now,
                (RateLimitState.blocked_until.is_(None))
                | (RateLimitState.blocked_until <= now),
            )
            .values(attempts=RateLimitState.attempts + 1, updated_at=now)
        )
        for dialect in (postgresql.dialect(), sqlite.dialect()):
            stmt.compile(dialect=dialect)

    def test_cleanup_select_compiles_on_both(self):
        stmt = (
            select(RateLimitState.key)
            .where(RateLimitState.scope == "login")
            .order_by(RateLimitState.updated_at.desc())
            .offset(5000)
            .limit(1000)
        )
        for dialect in (postgresql.dialect(), sqlite.dialect()):
            stmt.compile(dialect=dialect)


if __name__ == "__main__":
    unittest.main()
