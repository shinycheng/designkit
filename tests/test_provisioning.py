"""开通状态机（services/provisioning.py）的回归测试：不联网、不花钱、不用起服务。

对着 fake_sub2api.py 这台假网关跑。这组测试盯的是**可重入**——
每一步都故意中断一次再重跑，断言：
  - 不会在远端建出重复账号；
  - 不会重复发 Key，更不会把已经拿到的 Key 冲掉；
  - 不会调用任何删除/充值接口（假服务器压根没有这两条路由，调了就 404）。

为什么这些必须长期回归：可重入的破坏方式全是「看起来更简洁」的改法
（比如把 409 直接当成成功、把写 Key 的判空去掉、重跑时顺手重摇一次密码），
它们在一次性手测里全都通过，只有在断电重启之后才会以「账单上多了一笔
没人认领的钱」「某个用户永远登不上网关」的形式暴露出来。
"""
import atexit
import logging
import os
import shutil
import sys
import tempfile
import unittest

# 让 `python tests/test_provisioning.py` 这种直接跑法也能 import 到 backend 和
# 同目录的 fake_sub2api（按 README 的 `-m unittest discover -s tests` 跑法本来就行，
# 这两行是给顺手直接跑单个文件的人兜底的）。**不许写死绝对路径**：
# 换台机器、换个目录名就全断，而报出来的错是 ModuleNotFoundError，看不出根因。
_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(_HERE))
sys.path.insert(0, _HERE)

if not os.environ.get("DESIGNKIT_DATA_DIR"):
    _TMP = tempfile.mkdtemp(prefix="dk-provisioning-")
    os.environ["DESIGNKIT_DATA_DIR"] = _TMP
    atexit.register(shutil.rmtree, _TMP, ignore_errors=True)
os.environ.setdefault("DESIGNKIT_PROVIDER", "mock")

import fake_sub2api  # noqa: E402

from backend.app.database import SessionLocal, engine  # noqa: E402
from backend.app.models import Base, User, UserGatewayAccount  # noqa: E402
from backend.app.services import (  # noqa: E402
    provisioning, secrets_box, sub2api, user_gateway,
)


class Base_(unittest.TestCase):
    """公共脚手架：一台假网关 + 一个干净的库 + 一个用户。"""

    def setUp(self):
        Base.metadata.create_all(bind=engine)
        self.db = SessionLocal()
        self.db.query(UserGatewayAccount).delete()
        self.db.query(User).delete()
        self.db.commit()
        self.user = User(username="alice", password_hash="x", role="member")
        self.db.add(self.user)
        self.db.commit()

        self.base_url, self.remote, self._stop = fake_sub2api.start()
        self.cfg = provisioning.ProvisioningConfig(
            enabled=True,
            base_url=self.base_url,
            admin_key=fake_sub2api.ADMIN_KEY,
            group_id="7",
            email_domain="designkit.local",
            keep_password=False,
            max_attempts=5,
            timeout=5.0,
            verify_tls=True,
        )
        provisioning._reset_state_for_tests()

    def tearDown(self):
        self.db.close()
        self._stop()

    # —— 小工具 ——
    def run_once(self):
        """跑一轮状态机。先清掉登录节流的计时器，免得每个用例真等 6 秒。"""
        sub2api._reset_login_gate_for_tests()
        return provisioning.provision_user(self.db, self.user.id, cfg=self.cfg)

    def account(self):
        self.db.expire_all()
        return user_gateway.get_account(self.db, self.user.id)

    def register(self):
        acc = provisioning.ensure_account(self.db, self.user.id, cfg=self.cfg)
        self.db.commit()
        return acc


class TestRegistration(Base_):
    """T1：注册登记。纯本地，绝不发请求。"""

    def test_registers_pending_without_any_http(self):
        acc = self.register()
        self.assertEqual(acc.state, "pending")
        self.assertEqual(acc.remote_email, "dk%d@designkit.local" % self.user.id)
        self.assertTrue(secrets_box.is_encrypted(acc.remote_password_enc))
        # 注册链路一行 HTTP 都不该发——降级第一层就是靠这个成立的
        self.assertEqual(sum(self.remote.calls.values()), 0)

    def test_email_has_no_plus_sign(self):
        # Sub2API 对所有域名做 stripEmailPlusSuffix，带 + 的会被并成同一个人
        acc = self.register()
        self.assertNotIn("+", acc.remote_email)

    def test_reserved_invalid_domain_falls_back(self):
        cfg = provisioning.ProvisioningConfig(enabled=True, email_domain="foo-connect.invalid")
        acc = provisioning.ensure_account(self.db, self.user.id, cfg=cfg)
        self.db.commit()
        self.assertTrue(acc.remote_email.endswith("@designkit.local"))

    def test_second_call_never_regenerates_credentials(self):
        """★ 硬保证 1：邮箱和密码一旦写入就永不重新生成。"""
        acc = self.register()
        email, pwd_enc = acc.remote_email, acc.remote_password_enc
        again = provisioning.ensure_account(self.db, self.user.id, cfg=self.cfg)
        self.db.commit()
        self.assertEqual(again.remote_email, email)
        self.assertEqual(again.remote_password_enc, pwd_enc)

    def test_disabled_auto_registers_manual_without_password(self):
        cfg = provisioning.ProvisioningConfig(enabled=False)
        acc = provisioning.ensure_account(self.db, self.user.id, cfg=cfg)
        self.db.commit()
        self.assertEqual(acc.state, "manual")
        # 不打算自动开通就不造凭据存着
        self.assertIsNone(acc.remote_password_enc)


class TestHappyPath(Base_):
    def test_full_flow_reaches_active(self):
        self.register()
        result = self.run_once()
        self.assertEqual(result.outcome, "active")
        acc = self.account()
        self.assertEqual(acc.state, "active")
        self.assertTrue(acc.api_key_enc)
        self.assertEqual(len(acc.api_key_tail), 4)
        self.assertEqual(acc.balance_usd, 12.5)
        self.assertEqual(len(self.remote.users), 1)
        self.assertEqual(len(self.remote.keys), 1)

    def test_password_cleared_after_active(self):
        """默认 sub2api_keep_password=false：进 active 立刻清密码。"""
        self.register()
        self.run_once()
        self.assertIsNone(self.account().remote_password_enc)

    def test_keep_password_setting_honoured(self):
        self.cfg.keep_password = True
        self.register()
        self.run_once()
        self.assertTrue(self.account().remote_password_enc)

    def test_explicit_concurrency_and_group_sent(self):
        """设计文档「仍未查明」1、2 条：不依赖服务端默认值。"""
        self.register()
        self.run_once()
        remote_user = self.remote.users[0]
        self.assertEqual(remote_user["concurrency"], 5)
        self.assertEqual(remote_user["rpm_limit"], 0)
        self.assertEqual(remote_user["allowed_groups"], ["7"])
        self.assertEqual(self.remote.keys[0]["group_id"], "7")

    def test_rerun_after_active_is_a_noop(self):
        self.register()
        self.run_once()
        key_before = self.account().api_key_enc
        result = self.run_once()
        self.assertEqual(result.outcome, "skipped")
        self.assertEqual(len(self.remote.users), 1)
        self.assertEqual(len(self.remote.keys), 1)
        self.assertEqual(self.account().api_key_enc, key_before)


class TestReentrancy(Base_):
    """★ 每一步都故意中断一次再重跑。"""

    def test_interrupt_after_remote_user_created(self):
        """建号：远端建好了、响应没回来。重跑不能建出第二个账号。"""
        self.register()
        self.remote.fail_admin_create_after_effect = 1
        first = self.run_once()
        self.assertEqual(first.outcome, "failed")
        self.assertEqual(len(self.remote.users), 1)          # 远端已经有一个了
        self.assertIsNone(self.account().remote_user_id)     # 本地还不知道

        second = self.run_once()                             # 重跑：409 → 反查确认
        self.assertEqual(second.outcome, "active")
        self.assertEqual(len(self.remote.users), 1)          # ★ 没建出重复账号
        self.assertEqual(self.account().remote_user_id, str(self.remote.users[0]["id"]))

    def test_interrupt_after_key_created(self):
        """建 Key：远端发好了、响应没回来。重跑必须读回同一把，不能再发一把。"""
        self.register()
        self.remote.fail_key_create_after_effect = 1
        first = self.run_once()
        self.assertEqual(first.outcome, "failed")
        self.assertEqual(len(self.remote.keys), 1)
        self.assertIsNone(self.account().api_key_enc)

        second = self.run_once()
        self.assertEqual(second.outcome, "active")
        self.assertEqual(len(self.remote.keys), 1)           # ★ 没重复发 Key
        self.assertEqual(
            secrets_box.decrypt(self.account().api_key_enc), self.remote.keys[0]["key"]
        )

    def test_interrupt_at_smoke(self):
        """冒烟失败后重跑：Key 已经在库里了，绝不能再建一把。"""
        self.register()
        self.remote.fail_usage = 1
        first = self.run_once()
        self.assertEqual(first.outcome, "failed")
        self.assertEqual(self.account().state, "failed")
        stored = self.account().api_key_enc
        self.assertTrue(stored)

        second = self.run_once()
        self.assertEqual(second.outcome, "active")
        self.assertEqual(len(self.remote.keys), 1)
        self.assertEqual(self.account().api_key_enc, stored)  # ★ 一个字节都没动

    def test_existing_key_is_never_overwritten(self):
        """★ 硬保证 2：库里已经有 Key 时，重跑绝不再建、更不覆盖。

        故意用一把**和自动推导出来的不一样**的 Key（模拟管理员手工换过一把），
        这样一旦哪天有人把「只在为 NULL 时写入」改掉，这个用例会立刻红。
        """
        self.register()
        self.run_once()
        manual_key = "sk-manual-replacement-key-9999"
        # 让假网关认这把 Key（不然冒烟会 401，测不到我们想测的东西）
        self.remote.keys.append({"id": 1, "key": manual_key, "name": "manual",
                                 "group_id": "7", "owner": "admin@example.com"})
        acc = self.account()
        acc.api_key_enc = secrets_box.encrypt(manual_key)
        acc.api_key_tail = manual_key[-4:]
        acc.state = "failed"
        acc.last_error = "E_NETWORK|临时"
        self.db.commit()
        posts_before = self.remote.count("POST", "/api/v1/keys")

        self.run_once()
        self.assertEqual(secrets_box.decrypt(self.account().api_key_enc), manual_key)
        # 一次新的建 Key 请求都没发出去
        self.assertEqual(self.remote.count("POST", "/api/v1/keys"), posts_before)

    def test_repeated_runs_do_not_multiply_anything(self):
        """连跑十轮，远端账号数和 Key 数都必须恒为 1。"""
        self.register()
        for _ in range(10):
            self.run_once()
        self.assertEqual(len(self.remote.users), 1)
        self.assertEqual(len(self.remote.keys), 1)

    def test_no_delete_or_topup_endpoints_touched(self):
        """全程不碰任何删除/充值接口（假服务器上它们只会 404）。"""
        self.register()
        self.run_once()
        for (method, path) in self.remote.calls:
            self.assertNotIn("balance", path)
            self.assertNotIn("redeem", path)
            self.assertNotEqual(method, "DELETE")

    def test_uses_correct_keys_route(self):
        """建 Key 走 /api/v1/keys；照源码注释写成 /api/v1/api-keys 会 404。"""
        self.register()
        self.run_once()
        self.assertGreaterEqual(self.remote.count("POST", "/api/v1/keys"), 1)
        self.assertEqual(self.remote.count("POST", "/api/v1/api-keys"), 0)


class TestConcurrentWriters(Base_):
    """两个 worker 同时推进同一个用户时，不能把先写进去的 Key 冲掉。

    这里不真起线程（SQLite 多写会随机报 database is locked，测试会变成偶发失败），
    而是**精确地**把竞态摆出来：在 create_key 返回之后、写库之前，
    让另一个会话抢先把一把 Key 写进这一行。这正是 db.refresh + 判空要挡的那一刻。
    """

    def test_write_is_skipped_when_another_worker_won_the_race(self):
        self.register()
        self.run_once()
        # 「另一个 worker」写进去的必须是一把**不同的** Key，否则覆盖与不覆盖
        # 结果一模一样，这个用例就废了（第一版就踩过这个坑）。
        winner_key = "sk-other-worker-key-4242"
        self.remote.keys.append({"id": 2, "key": winner_key, "name": "other",
                                 "group_id": "7", "owner": "other@example.com"})

        # 把本地这一行退回「还没有 Key」的样子，重跑
        acc = self.account()
        acc.api_key_enc = None
        acc.api_key_tail = ""
        acc.state = "user_created"
        acc.remote_password_enc = secrets_box.encrypt(self.remote.users[0]["password"])
        self.db.commit()

        from backend.app.database import SessionLocal as _Session
        original = sub2api.Sub2ApiClient.create_key

        def racing_create_key(client_self, *args, **kwargs):
            try:
                return original(client_self, *args, **kwargs)
            finally:
                # 「另一个 worker」在这一瞬间赢了：抢先把 Key 写进库
                other = _Session()
                try:
                    row = user_gateway.get_account(other, self.user.id)
                    row.api_key_enc = secrets_box.encrypt(winner_key)
                    row.api_key_tail = winner_key[-4:]
                    row.state = "key_issued"
                    other.commit()
                finally:
                    other.close()

        sub2api.Sub2ApiClient.create_key = racing_create_key
        try:
            self.run_once()
        finally:
            sub2api.Sub2ApiClient.create_key = original

        # ★ 先写进去的那把必须原封不动
        self.assertEqual(secrets_box.decrypt(self.account().api_key_enc), winner_key)


class TestWorkerEntryPoint(Base_):
    def test_provision_due_runs_everyone_and_is_idempotent(self):
        """worker 的入口：跑两轮，远端账号数和 Key 数都必须恒为 1。"""
        from backend.app import config as app_config
        from backend.app.services import settings_service
        added = []
        for key, default in provisioning.PROVISIONING_SETTING_DEFAULTS.items():
            if key not in app_config.RUNTIME_DEFAULTS:
                app_config.RUNTIME_DEFAULTS[key] = default
                added.append(key)
        try:
            settings_service.set_many(self.db, {
                "sub2api_auto_provision": True,
                "sub2api_base_url": self.base_url,
                "sub2api_admin_key": fake_sub2api.ADMIN_KEY,
                "sub2api_group_id": "7",
            })
            self.register()
            sub2api._reset_login_gate_for_tests()
            results = provisioning.provision_due(self.db)
            self.assertEqual([r.outcome for r in results], ["active"])
            sub2api._reset_login_gate_for_tests()
            self.assertEqual(provisioning.provision_due(self.db), [])
            self.assertEqual(len(self.remote.users), 1)
            self.assertEqual(len(self.remote.keys), 1)
        finally:
            for key in added:
                app_config.RUNTIME_DEFAULTS.pop(key, None)


class TestConflictHandling(Base_):
    def test_409_is_verified_by_lookup_not_swallowed(self):
        """★ 硬保证 3：409 必须反查确认，而且不能被模糊匹配骗走。"""
        # 远端先躺着一个近似账号（搜 dk<id>@ 会先命中它），以及我们自己的那个
        decoy = "dk%d0@designkit.local" % self.user.id
        self.remote.users.append({
            "id": 1, "email": decoy, "username": "someone-else", "password": "x",
            "concurrency": 5, "rpm_limit": 0, "allowed_groups": ["7"], "deleted": False,
        })
        self.register()
        acc = self.account()
        self.remote.users.append({
            "id": 2, "email": acc.remote_email, "username": "designkit",
            "password": secrets_box.decrypt(acc.remote_password_enc),
            "concurrency": 5, "rpm_limit": 0, "allowed_groups": ["7"], "deleted": False,
        })
        result = self.run_once()
        self.assertEqual(result.outcome, "active")
        # 取的是精确匹配那条，不是排在第一位的近似项
        self.assertEqual(self.account().remote_user_id, "2")

    def test_email_exists_but_not_found_becomes_ghost(self):
        """409 了却怎么也查不到 → E_LOCAL_EMAIL_GHOST，转人工，不无限重试。"""
        self.register()
        acc = self.account()
        self.remote.users.append({
            "id": 9, "email": acc.remote_email, "username": "ghost", "password": "x",
            "concurrency": 5, "rpm_limit": 0, "allowed_groups": [], "deleted": False,
        })
        self.remote.hide_from_search.add(acc.remote_email)
        result = self.run_once()
        self.assertEqual(result.error_code, "E_LOCAL_EMAIL_GHOST")
        self.assertIsNone(provisioning.retry_due_at(self.account()))  # 不自动重试

    def test_squatted_custom_key_rolls_the_salt(self):
        """custom_key 被别的租户抢注：换一轮盐，不是换个后缀硬撞。"""
        salt = provisioning._custom_key_salt()
        round0 = sub2api.derive_custom_key(salt, self.user.id, 0)
        self.remote.squatted.add(round0)
        self.register()
        result = self.run_once()
        self.assertEqual(result.outcome, "active")
        issued = self.remote.keys[0]["key"]
        self.assertNotEqual(issued, round0)
        self.assertEqual(issued, sub2api.derive_custom_key(salt, self.user.id, 1))

    def test_all_rounds_squatted_gives_up(self):
        salt = provisioning._custom_key_salt()
        for r in range(3):
            self.remote.squatted.add(sub2api.derive_custom_key(salt, self.user.id, r))
        self.register()
        result = self.run_once()
        self.assertEqual(result.error_code, "E_LOCAL_KEY_SQUATTED")


class TestFailureRouting(Base_):
    def test_2fa_is_not_retried(self):
        self.remote.login_requires_2fa = True
        self.register()
        result = self.run_once()
        self.assertEqual(result.error_code, "E_LOGIN_2FA")
        self.assertEqual(self.account().state, "failed")
        self.assertIsNone(provisioning.retry_due_at(self.account()))

    def test_password_mismatch_is_not_retried(self):
        self.remote.login_status = 401
        self.register()
        result = self.run_once()
        self.assertEqual(result.error_code, "E_PASSWORD_MISMATCH")
        self.assertIsNone(provisioning.retry_due_at(self.account()))

    def test_backend_mode_halts_everything(self):
        self.remote.login_status = 403
        self.remote.login_body = {"code": "BACKEND_MODE_ADMIN_ONLY",
                                  "message": "Backend mode enabled", "data": None}
        self.register()
        result = self.run_once()
        self.assertEqual(result.error_code, "E_BACKEND_MODE")
        self.assertTrue(result.halt_all)
        self.assertTrue(provisioning.is_halted())
        # 全局暂停后连试都不试了
        self.assertEqual(self.run_once().outcome, "skipped")

    def test_captcha_halts_everything(self):
        self.remote.login_status = 400
        self.remote.login_body = {"code": "TURNSTILE_NOT_CONFIGURED",
                                  "message": "captcha misconfigured", "data": None}
        self.register()
        result = self.run_once()
        self.assertEqual(result.error_code, "E_CAPTCHA_ON")
        self.assertTrue(provisioning.is_halted())

    def test_compliance_423_string_code(self):
        self.remote.admin_create_status = 423
        self.register()
        result = self.run_once()
        self.assertEqual(result.error_code, "E_COMPLIANCE")
        self.assertTrue(provisioning.is_halted())

    def test_key_rate_limit_stops_immediately(self):
        """★ 建 Key 撞 429：必须立刻停手，绝不换后缀重试（否则锁死一小时）。"""
        self.remote.key_create_status = 429
        self.register()
        result = self.run_once()
        self.assertEqual(result.error_code, "E_KEY_RATE_LIMITED")
        self.assertEqual(self.remote.count("POST", "/api/v1/keys"), 1)  # 只发了一次
        self.assertIsNone(provisioning.retry_due_at(self.account()))

    def test_no_group_on_smoke(self):
        self.remote.usage_status = 403
        self.remote.usage_body = {"error": "API Key is not assigned to any group"}
        self.register()
        result = self.run_once()
        self.assertEqual(result.error_code, "E_NO_GROUP")
        self.assertEqual(self.account().state, "failed")

    def test_network_error_is_retryable_with_backoff(self):
        self.register()
        self._stop()  # 把假网关关掉，模拟网关挂了
        result = self.run_once()
        self.assertEqual(result.error_code, "E_NETWORK")
        acc = self.account()
        self.assertEqual(acc.attempts, 1)
        self.assertIsNotNone(provisioning.retry_due_at(acc))
        self.assertEqual(provisioning.backoff_seconds(1), 60)
        self.assertEqual(provisioning.backoff_seconds(5), 43200)

    def test_missing_config_does_not_burn_attempts(self):
        """管理员忘填分组 id 的那五分钟，不能把全站用户刷成 manual。"""
        self.cfg.group_id = ""
        self.register()
        for _ in range(8):
            result = self.run_once()
        self.assertEqual(result.error_code, "E_LOCAL_CONFIG")
        acc = self.account()
        self.assertEqual(acc.attempts, 0)
        self.assertEqual(acc.state, "pending")  # 状态没被打成 failed

    def test_max_attempts_downgrades_to_manual(self):
        self.cfg.max_attempts = 3
        self.register()
        self._stop()
        for _ in range(3):
            self.run_once()
        acc = self.account()
        self.assertEqual(acc.state, "manual")
        code, message = provisioning.parse_last_error(acc.last_error)
        self.assertEqual(code, "E_LOCAL_MAX_ATTEMPTS")
        self.assertIn("手工", message)

    def test_last_error_is_code_pipe_humanspeak(self):
        self.remote.login_requires_2fa = True
        self.register()
        self.run_once()
        raw = self.account().last_error
        self.assertTrue(raw.startswith("E_LOGIN_2FA|"))
        code, message = provisioning.parse_last_error(raw)
        self.assertIn(code, provisioning.ALL_ERROR_CODES)
        self.assertNotIn("HTTP", message)  # 给运营看的是人话，不是状态码


class TestScheduling(Base_):
    def test_due_ids_include_in_flight_and_skip_manual(self):
        self.register()
        self.assertIn(self.user.id, provisioning.due_user_ids(self.db))
        provisioning.demote_to_manual(self.db, self.user.id)
        self.assertNotIn(self.user.id, provisioning.due_user_ids(self.db))

    def test_non_retryable_failed_is_not_due(self):
        self.remote.login_requires_2fa = True
        self.register()
        self.run_once()
        self.assertNotIn(self.user.id, provisioning.due_user_ids(self.db))

    def test_retryable_failed_is_due_after_backoff(self):
        from datetime import timedelta
        self.register()
        self._stop()
        self.run_once()
        acc = self.account()
        self.assertNotIn(self.user.id, provisioning.due_user_ids(self.db))
        acc.updated_at = acc.updated_at - timedelta(seconds=120)
        self.db.commit()
        self.assertIn(self.user.id, provisioning.due_user_ids(self.db))

    def test_active_is_not_due(self):
        self.register()
        self.run_once()
        self.assertNotIn(self.user.id, provisioning.due_user_ids(self.db))


class TestBalanceSync(Base_):
    def test_sync_updates_balance(self):
        self.register()
        self.run_once()
        acc = self.account()
        acc.balance_usd = None
        self.db.commit()
        result = provisioning.sync_balance(self.db, self.user.id, cfg=self.cfg)
        self.assertEqual(result.outcome, "active")
        self.assertEqual(self.account().balance_usd, 12.5)

    def test_network_error_keeps_active(self):
        self.register()
        self.run_once()
        self._stop()
        result = provisioning.sync_balance(self.db, self.user.id, cfg=self.cfg)
        self.assertEqual(result.outcome, "skipped")
        self.assertEqual(self.account().state, "active")  # 不因为网络抖动掉出 active

    def test_key_revoked_flags_failed_but_keeps_key(self):
        self.register()
        self.run_once()
        enc = self.account().api_key_enc
        self.remote.usage_status = 401
        self.remote.usage_body = {"error": "invalid api key"}
        result = provisioning.sync_balance(self.db, self.user.id, cfg=self.cfg)
        self.assertEqual(result.error_code, "E_KEY_REJECTED")
        self.assertEqual(self.account().state, "failed")
        self.assertEqual(self.account().api_key_enc, enc)  # 绝不清掉


class TestAdminActions(Base_):
    def test_retry_puts_failed_back_to_pending(self):
        self.remote.login_requires_2fa = True
        self.register()
        self.run_once()
        self.assertTrue(provisioning.request_retry(self.db, self.user.id, reset_attempts=True))
        acc = self.account()
        self.assertEqual(acc.state, "pending")
        self.assertEqual(acc.attempts, 0)
        self.assertEqual(acc.last_error, "")

    def test_resume_auto_refuses_when_password_was_cleared(self):
        """密码清掉之后不能重新自动开通——绝不重摇密码，只能走手工。"""
        self.register()
        self.run_once()
        acc = self.account()
        acc.api_key_enc = None
        acc.api_key_tail = ""
        acc.state = "manual"
        self.db.commit()
        self.assertFalse(provisioning.resume_auto(self.db, self.user.id, cfg=self.cfg))
        self.assertEqual(self.account().state, "manual")

    def test_resume_auto_fills_credentials_for_fresh_manual_row(self):
        cfg_off = provisioning.ProvisioningConfig(enabled=False)
        provisioning.ensure_account(self.db, self.user.id, cfg=cfg_off)
        self.db.commit()
        self.assertTrue(provisioning.resume_auto(self.db, self.user.id, cfg=self.cfg))
        acc = self.account()
        self.assertEqual(acc.state, "pending")
        self.assertTrue(acc.remote_password_enc)

    def test_resume_auto_refuses_rows_with_hand_filled_key(self):
        """手工填 Key 的行切自动，会在网关上凭空建一个永远用不上的账号。"""
        user_gateway.upsert_key(self.db, self.user.id, "sk-filled-by-admin-0001",
                                state="manual")
        self.db.commit()
        self.assertFalse(provisioning.resume_auto(self.db, self.user.id, cfg=self.cfg))
        self.assertEqual(self.account().state, "manual")

    def test_demote_reason_is_not_labelled_as_an_error_code(self):
        self.register()
        provisioning.demote_to_manual(self.db, self.user.id, "管理员改为手工发 Key")
        code, message = provisioning.parse_last_error(self.account().last_error)
        self.assertEqual(code, "")
        self.assertEqual(message, "管理员改为手工发 Key")

    def test_manual_rows_are_never_touched(self):
        self.register()
        provisioning.demote_to_manual(self.db, self.user.id, "管理员手工发 Key")
        result = self.run_once()
        self.assertEqual(result.outcome, "skipped")
        self.assertEqual(sum(self.remote.calls.values()), 0)


class TestSerialization(Base_):
    def test_view_never_leaks_credentials_or_tail(self):
        self.register()
        self.run_once()
        view = provisioning.account_view(self.account())
        blob = repr(view)
        acc = self.account()
        self.assertNotIn("remote_email", view)
        self.assertNotIn("remote_password_enc", view)
        self.assertNotIn("api_key_enc", view)
        self.assertNotIn("api_key_tail", view)
        self.assertNotIn(acc.api_key_tail, blob)
        self.assertNotIn(self.remote.keys[0]["key"], blob)
        self.assertTrue(view["configured"])

    def test_summary_lists_stuck_users(self):
        self.remote.login_requires_2fa = True
        self.register()
        self.run_once()
        data = provisioning.summary(self.db)
        self.assertEqual(data["counts"].get("failed"), 1)
        self.assertEqual(data["stuck"][0]["username"], "alice")
        self.assertIn("两步验证", data["stuck"][0]["error_message"])


class TestNoSecretsInLogs(Base_):
    """★ 跑完整流程，断言所有日志里既没有密码明文也没有 Key 明文。"""

    def test_full_flow_logs_are_clean(self):
        self.register()
        acc = self.account()
        password = secrets_box.decrypt(acc.remote_password_enc)

        records = []

        class Sink(logging.Handler):
            def emit(self, record):
                try:
                    records.append(record.getMessage())
                except Exception:
                    records.append(str(record.msg))

        sink = Sink()
        root = logging.getLogger()
        old_level = root.level
        root.setLevel(logging.DEBUG)   # 连 DEBUG 的请求体日志一起收
        root.addHandler(sink)
        try:
            self.run_once()
        finally:
            root.removeHandler(sink)
            root.setLevel(old_level)

        issued_key = self.remote.keys[0]["key"]
        blob = "\n".join(records)
        self.assertTrue(records)
        self.assertNotIn(password, blob)
        self.assertNotIn(issued_key, blob)
        self.assertNotIn(fake_sub2api.ADMIN_KEY, blob)
        for token in self.remote.tokens:
            self.assertNotIn(token, blob)
        # 末 4 位是唯一允许出现的部分
        self.assertIn(issued_key[-4:], blob)


class TestConfigLoading(Base_):
    def test_reads_settings_once_config_keys_exist(self):
        """设置页任务把这些键加进 RUNTIME_DEFAULTS 之后，load_config 要能读到。"""
        from backend.app import config as app_config
        from backend.app.services import settings_service
        added = []
        for key, default in provisioning.PROVISIONING_SETTING_DEFAULTS.items():
            if key not in app_config.RUNTIME_DEFAULTS:
                app_config.RUNTIME_DEFAULTS[key] = default
                added.append(key)
        try:
            settings_service.set_many(self.db, {
                "sub2api_auto_provision": True,
                "sub2api_base_url": "http://example.invalid:3000",
                "sub2api_group_id": "42",
                "sub2api_email_domain": "gw.example.com",
            })
            cfg = provisioning.load_config(self.db)
            self.assertTrue(cfg.enabled)
            self.assertEqual(cfg.group_id, "42")
            self.assertEqual(cfg.email_domain, "gw.example.com")
        finally:
            for key in added:
                app_config.RUNTIME_DEFAULTS.pop(key, None)

    def test_missing_config_keys_degrade_to_disabled(self):
        """config 还没加这些键时，读不到就当没开，绝不抛异常带崩注册。"""
        cfg = provisioning.load_config(self.db)
        self.assertFalse(cfg.enabled)
        self.assertIn("还没填", cfg.missing())

    def test_admin_key_not_in_repr(self):
        self.assertNotIn(fake_sub2api.ADMIN_KEY, repr(self.cfg))

    def test_error_codes_table_is_complete(self):
        for code in provisioning.RETRYABLE_ERROR_CODES:
            self.assertIn(code, provisioning.ALL_ERROR_CODES)
        for code in provisioning.NON_COUNTING_ERROR_CODES:
            self.assertIn(code, provisioning.ALL_ERROR_CODES)


if __name__ == "__main__":
    unittest.main(verbosity=2)
