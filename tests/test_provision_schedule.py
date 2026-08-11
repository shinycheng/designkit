"""触发时机与定时任务的回归测试（不联网、不花钱、不用起服务）。

盯的是两条纪律：
1. **建号接口一步外部请求都不发**——网关整个挂掉，建号也必须照常成功。
2. 真正的开通全在后台那一轮里做，而且一个人失败不影响别人、一轮失败不影响线程。

对着 fake_sub2api.py 那台假网关跑，用户那台 192.168.31.235 全程不碰。
"""
import atexit
import os
import shutil
import sys
import tempfile
import unittest
from datetime import datetime, timedelta

# 见 test_provisioning.py 里同一处注释：绝不写死绝对路径。
_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(_HERE))
sys.path.insert(0, _HERE)

if not os.environ.get("DESIGNKIT_DATA_DIR"):
    _TMP = tempfile.mkdtemp(prefix="dk-provision-schedule-")
    os.environ["DESIGNKIT_DATA_DIR"] = _TMP
    atexit.register(shutil.rmtree, _TMP, ignore_errors=True)
os.environ.setdefault("DESIGNKIT_PROVIDER", "mock")

import fake_sub2api  # noqa: E402

from backend.app.database import SessionLocal, engine  # noqa: E402
from backend.app.models import Base, SyncState, User, UserGatewayAccount  # noqa: E402
from backend.app.routers import users as users_router  # noqa: E402
from backend.app.services import provisioning, scheduler, sub2api, user_gateway  # noqa: E402


class Scaffold(unittest.TestCase):
    """一台假网关 + 一个干净的库 + 一个管理员。"""

    def setUp(self):
        Base.metadata.create_all(bind=engine)
        self.db = SessionLocal()
        self.db.query(UserGatewayAccount).delete()
        self.db.query(User).delete()
        self.db.query(SyncState).delete()
        self.db.commit()
        provisioning._reset_state_for_tests()
        sub2api._reset_login_gate_for_tests()

        self.admin = User(username="boss", password_hash="x", role="admin", is_active=True)
        self.db.add(self.admin)
        self.db.commit()

        self.base_url, self.remote, self._stop = fake_sub2api.start()
        self.cfg = provisioning.ProvisioningConfig(
            enabled=True, base_url=self.base_url, admin_key=fake_sub2api.ADMIN_KEY,
            group_id="7", email_domain="designkit.local", keep_password=False,
            max_attempts=5, timeout=5.0, verify_tls=True,
        )
        # 设置项那几个键还没进 config.RUNTIME_DEFAULTS（那是设置页那个任务的活），
        # 所以这里直接把 load_config 换掉，等于「管理员已经在设置页填好了」。
        self._real_load = provisioning.load_config
        provisioning.load_config = lambda db: self.cfg

    def tearDown(self):
        provisioning.load_config = self._real_load
        self._stop()
        self.db.close()

    # —— 小工具 ——
    def _create_member(self, username="alice"):
        body = users_router.UserCreateBody(username=username, password="init8888")
        return users_router.create_user(body, admin=self.admin, db=self.db)

    def _account(self, user_id):
        self.db.expire_all()
        return user_gateway.get_account(self.db, user_id)

    def _total_calls(self):
        return sum(self.remote.calls.values())


class CreateUserIsDecoupledTests(Scaffold):
    """建号：只登记，不开通。"""

    def test_create_user_makes_zero_remote_calls(self):
        data = self._create_member()
        self.assertEqual(data["gateway"]["state"], "pending")
        self.assertEqual(self._total_calls(), 0, "建号接口发出了外部请求，等于把注册和开通绑死了")

    def test_create_user_survives_gateway_being_down(self):
        # 网关整个不可达（端口没人听）。建号必须照常成功。
        self.cfg.base_url = "http://127.0.0.1:9"
        data = self._create_member("bob")
        self.assertTrue(data["id"])
        self.assertEqual(data["gateway"]["state"], "pending")

    def test_create_user_survives_registration_failure(self):
        """登记那一步自己炸了，也绝不能让建号跟着失败。"""
        real = provisioning.ensure_account
        provisioning.ensure_account = lambda *a, **kw: (_ for _ in ()).throw(RuntimeError("boom"))
        try:
            data = self._create_member("carol")
        finally:
            provisioning.ensure_account = real
        self.assertTrue(data["id"])
        self.assertEqual(data["gateway"]["state"], "")   # 一行都没有，等后台补
        self.assertFalse(data["gateway"]["configured"])
        # 后台那一轮会把缺的行补上
        self.assertIn(data["id"], scheduler._users_without_account(self.db))

    def test_auto_off_registers_manual_row(self):
        self.cfg.enabled = False
        data = self._create_member("dave")
        self.assertEqual(data["gateway"]["state"], "manual")
        account = self._account(data["id"])
        self.assertIsNone(account.remote_password_enc, "没打算自动开通就不该造一份可登录凭据")


class BackgroundRoundTests(Scaffold):
    """后台那一轮：推进状态机 + 同步余额 + 补登记。"""

    def test_round_provisions_pending_user(self):
        uid = self._create_member()["id"]
        ran, message = scheduler.run_provisioning(trigger="test")
        self.assertTrue(ran, message)
        account = self._account(uid)
        self.assertEqual(account.state, "active")
        self.assertTrue(account.api_key_enc)
        self.assertIsNone(account.remote_password_enc, "keep_password=false 时开通后要清掉密码")
        # 管理员自己那一行也会被补登记并开通（他同样要生图），所以是 2 个人
        self.assertIn("失败 0", message)
        self.assertIn("补登记 1 人", message)
        # 冒烟走的是网关那条 /v1/usage，不是吃两档限流的统计接口
        self.assertEqual(self.remote.count("GET", "/v1/usage"), 2)  # 两个人各冒烟一次
        self.assertEqual(self.remote.count("GET", "/api/v1/usage/stats"), 0)

    def test_round_never_touches_delete_or_balance_routes(self):
        self._create_member()
        scheduler.run_provisioning(trigger="test")
        for (method, path) in self.remote.calls:
            self.assertNotEqual(method, "DELETE", "开通流程里出现了删除请求")
            self.assertNotIn("balance", path)
            self.assertNotIn("redeem", path)

    def test_balance_sync_updates_only_stale_rows(self):
        uid = self._create_member()["id"]
        scheduler.run_provisioning(trigger="test")
        before = self.remote.count("GET", "/v1/usage")

        # 刚同步过的不该再打一次
        self.assertEqual(scheduler._balance_due_user_ids(self.db), [])
        scheduler.run_provisioning(trigger="test")
        self.assertEqual(self.remote.count("GET", "/v1/usage"), before)

        # 把时间拨回去，就该轮到它了
        account = self._account(uid)
        account.balance_usd = None
        account.balance_synced_at = datetime.utcnow() - timedelta(hours=2)
        self.db.commit()
        self.assertEqual(scheduler._balance_due_user_ids(self.db), [uid])
        scheduler.run_provisioning(trigger="test")
        self.assertEqual(self.remote.count("GET", "/v1/usage"), before + 1)
        self.assertAlmostEqual(self._account(uid).balance_usd, 12.5, places=3)

    def test_balance_sync_marks_dead_key_but_keeps_it(self):
        uid = self._create_member()["id"]
        scheduler.run_provisioning(trigger="test")
        account = self._account(uid)
        enc_before = account.api_key_enc
        account.balance_synced_at = datetime.utcnow() - timedelta(hours=2)
        self.db.commit()
        self.remote.usage_status = 403  # 分组配置被改了
        ran, message = scheduler.run_provisioning(trigger="test")
        self.assertTrue(ran)
        account = self._account(uid)
        self.assertEqual(account.state, "failed")
        self.assertEqual(account.api_key_enc, enc_before, "Key 被清掉了就再也读不回来")
        self.assertIn("失效", message)

    def test_backfill_adds_missing_rows_for_active_users_only(self):
        keep = User(username="old-hand", password_hash="x", role="member", is_active=True)
        gone = User(username="left-us", password_hash="x", role="member", is_active=False)
        self.db.add_all([keep, gone])
        self.db.commit()
        self.assertIn(keep.id, scheduler._users_without_account(self.db))
        self.assertNotIn(gone.id, scheduler._users_without_account(self.db))
        scheduler.run_provisioning(trigger="test")
        self.assertEqual(self._account(keep.id).state, "active")
        self.assertIsNone(self._account(gone.id), "停用的人不该被自动建号，白占一个名额")

    def test_one_bad_user_does_not_block_the_others(self):
        good = self._create_member("good")["id"]
        bad = self._create_member("bad")["id"]
        # 让 bad 这一行的密码解不开（模拟 .enc_key 换过），good 必须照常开通
        account = self._account(bad)
        account.remote_password_enc = "v1:not-a-real-ciphertext"
        self.db.commit()
        scheduler.run_provisioning(trigger="test")
        self.assertEqual(self._account(good).state, "active")
        self.assertEqual(self._account(bad).state, "failed")

    def test_round_is_skipped_when_switch_off_or_halted(self):
        uid = self._create_member()["id"]
        self.cfg.enabled = False
        scheduler.run_provisioning(trigger="test")
        self.assertEqual(self._account(uid).state, "pending")
        self.assertEqual(self._total_calls(), 0)

        self.cfg.enabled = True
        provisioning.set_halt("后台模式开着，代登录不可用")
        ran, message = scheduler.run_provisioning(trigger="test")
        self.assertTrue(ran)
        self.assertIn("暂停", message)
        self.assertEqual(self._total_calls(), 0)
        self.assertEqual(self._account(uid).attempts, 0, "全局暂停期间不该把大家的 attempts 刷高")
        provisioning.clear_halt()

    def test_failure_does_not_raise_out_of_the_round(self):
        """一轮里出了未预期的错，run_provisioning 也要正常返回并记进状态行。"""
        self._create_member()
        real = provisioning.provision_due
        provisioning.provision_due = lambda *a, **kw: (_ for _ in ()).throw(RuntimeError("boom"))
        try:
            ran, message = scheduler.run_provisioning(trigger="test")
        finally:
            provisioning.provision_due = real
        self.assertTrue(ran)
        self.assertIn("boom", message)
        row = self.db.get(SyncState, scheduler.TASK_PROVISION)
        self.db.refresh(row)
        self.assertEqual(row.last_status, "failed")
        self.assertIsNone(row.lock_until, "失败之后锁必须放开，否则整台机器再也开通不了")


class LockTests(Scaffold):
    """两路任务各锁各的，别互相挡。"""

    def test_inspiration_lock_does_not_block_provisioning(self):
        self.assertTrue(scheduler._acquire(self.db, "sync-1", name=scheduler.TASK_NAME))
        self.assertTrue(scheduler._acquire(self.db, "prov-1", name=scheduler.TASK_PROVISION,
                                           ttl=scheduler.PROVISION_LOCK_TTL))

    def test_second_round_is_refused_while_first_holds_lock(self):
        self.assertTrue(scheduler._acquire(self.db, "prov-1", name=scheduler.TASK_PROVISION,
                                           ttl=scheduler.PROVISION_LOCK_TTL))
        ran, message = scheduler.run_provisioning(trigger="test")
        self.assertFalse(ran)
        self.assertIn("已有开通任务", message)

    def test_release_checks_ownership(self):
        scheduler._acquire(self.db, "prov-1", name=scheduler.TASK_PROVISION)
        scheduler._release(self.db, "someone-else", True, "不该生效", name=scheduler.TASK_PROVISION)
        row = self.db.get(SyncState, scheduler.TASK_PROVISION)
        self.db.refresh(row)
        self.assertEqual(row.lock_owner, "prov-1")


class AdminRetryTests(Scaffold):
    """管理员那个「重新开通」按钮。"""

    def _fail_once(self, username="alice"):
        uid = self._create_member(username)["id"]
        self.remote.login_requires_2fa = True   # 不可重试类失败
        scheduler.run_provisioning(trigger="test")
        self.remote.login_requires_2fa = False
        return uid

    def test_retry_requeues_failed_user(self):
        uid = self._fail_once()
        account = self._account(uid)
        self.assertEqual(account.state, "failed")
        self.assertEqual(account.attempts, 1)
        result = users_router.provision_gateway(uid, admin=self.admin, db=self.db)
        self.assertTrue(result["ok"], result["message"])
        account = self._account(uid)
        self.assertEqual(account.state, "pending")
        self.assertEqual(account.attempts, 0, "管理员多半是刚把问题修好了，不清零等于只剩一次机会")
        # 排完队后台跑一轮就好了
        scheduler.run_provisioning(trigger="test")
        self.assertEqual(self._account(uid).state, "active")

    def test_retry_returns_immediately(self):
        """接口自己不打任何外部请求——开通里那次代登录最坏要排两分钟队。"""
        uid = self._fail_once()
        before = self._total_calls()
        users_router.provision_gateway(uid, admin=self.admin, db=self.db)
        self.assertEqual(self._total_calls(), before)

    def test_retry_refuses_when_already_active(self):
        uid = self._create_member()["id"]
        scheduler.run_provisioning(trigger="test")
        result = users_router.provision_gateway(uid, admin=self.admin, db=self.db)
        self.assertFalse(result["ok"])
        self.assertIn("已经开通好了", result["message"])

    def test_retry_tells_admin_when_switch_is_off(self):
        uid = self._create_member()["id"]
        self.cfg.enabled = False
        result = users_router.provision_gateway(uid, admin=self.admin, db=self.db)
        self.assertFalse(result["ok"])
        self.assertIn("系统设置", result["message"])

    def test_retry_tells_admin_what_is_missing(self):
        uid = self._create_member()["id"]
        self.cfg.group_id = ""
        result = users_router.provision_gateway(uid, admin=self.admin, db=self.db)
        self.assertFalse(result["ok"])
        self.assertIn("分组", result["message"])

    def test_retry_refuses_stopped_user(self):
        uid = self._create_member()["id"]
        user = self.db.get(User, uid)
        user.is_active = False
        self.db.commit()
        result = users_router.provision_gateway(uid, admin=self.admin, db=self.db)
        self.assertFalse(result["ok"])
        self.assertIn("停用", result["message"])

    def test_retry_refuses_when_password_already_wiped(self):
        """开通成功后密码被清掉了，就绝不能再自动开一次（重摇密码 = 远端永久失联）。"""
        uid = self._create_member()["id"]
        scheduler.run_provisioning(trigger="test")
        account = self._account(uid)
        # 管理员把这把 Key 清掉了（clear_key），而密码在开通成功那一刻就按
        # keep_password=false 清掉了——这一行从此只能手工发 Key，
        # 绝不能重新摇一个密码（远端那个账号会永久失联）。
        account.state = "manual"
        account.api_key_enc = None
        account.api_key_tail = ""
        self.db.commit()
        result = users_router.provision_gateway(uid, admin=self.admin, db=self.db)
        self.assertFalse(result["ok"])
        self.assertIn("配置生图 Key", result["message"])
        self.assertEqual(self._account(uid).state, "manual")

    def test_retry_refuses_when_key_was_filled_by_hand(self):
        uid = self._create_member("hand")["id"]
        account = self._account(uid)
        account.remote_email = None
        account.remote_password_enc = None
        self.db.commit()
        user_gateway.upsert_key(self.db, uid, "sk-typed-by-admin", state="manual")
        self.db.commit()
        result = users_router.provision_gateway(uid, admin=self.admin, db=self.db)
        self.assertFalse(result["ok"])
        self.assertIn("清除", result["message"])

    def test_retry_backfills_missing_row(self):
        user = User(username="legacy", password_hash="x", role="member", is_active=True)
        self.db.add(user)
        self.db.commit()
        result = users_router.provision_gateway(user.id, admin=self.admin, db=self.db)
        self.assertTrue(result["ok"], result["message"])
        self.assertEqual(self._account(user.id).state, "pending")


class MemberListTests(Scaffold):
    """成员列表要能看出开通状态和失败原因（只给人话那半截）。"""

    def test_list_shows_state_and_plain_reason(self):
        uid = self._create_member()["id"]
        self.remote.login_requires_2fa = True
        scheduler.run_provisioning(trigger="test")
        rows = users_router.list_users(_=self.admin, db=self.db)
        row = [r for r in rows if r["id"] == uid][0]
        gateway = row["gateway"]
        self.assertEqual(gateway["state"], "failed")
        self.assertTrue(gateway["last_error"])
        self.assertNotIn("|", gateway["last_error"], "分类码不该出现在给运营看的那句话里")
        self.assertTrue(gateway["error_code"].startswith("E_"))
        self.assertEqual(gateway["attempts"], 1)

    def test_list_never_leaks_credentials(self):
        uid = self._create_member()["id"]
        scheduler.run_provisioning(trigger="test")
        account = self._account(uid)
        rows = users_router.list_users(_=self.admin, db=self.db)
        gateway = [r for r in rows if r["id"] == uid][0]["gateway"]
        blob = repr(gateway)
        for banned in ("api_key_tail", "remote_email", "remote_password"):
            self.assertNotIn(banned, blob)
        self.assertNotIn(account.api_key_tail, blob)
        self.assertNotIn(account.remote_email or "@@@", blob)

    def test_timestamps_carry_utc_marker(self):
        uid = self._create_member()["id"]
        scheduler.run_provisioning(trigger="test")
        rows = users_router.list_users(_=self.admin, db=self.db)
        gateway = [r for r in rows if r["id"] == uid][0]["gateway"]
        self.assertTrue(gateway["balance_synced_at"].endswith("Z"),
                        "不带 Z 浏览器会当本地时间，中国时区直接差 8 小时")


if __name__ == "__main__":
    unittest.main(verbosity=2)
