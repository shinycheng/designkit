"""自动开通（Sub2API 代客建号）最贵的六条线：端到端回归。

和同目录另外两组的分工：
- `test_provisioning.py` 盯状态机内部（每一条转移、每一种分类码）；
- `test_provision_schedule.py` 盯触发时机（建号接口、后台那一轮、锁）；
- **这一组盯的是「从 HTTP 接口进、从 HTTP 接口出」的最终表现**——
  管理员点建号会不会失败、成员登不登得进来、界面上显示的是不是人话、
  接口回出去的 JSON 里有没有夹带凭据。前两组全绿而这一组变红是完全可能的：
  状态机本身没错，但某个接口把 api_key_tail 顺手回吐了。

六条线，每一条都对应一种「不会报错、但会出事」的故障：

1. **可重入**——每一步中断后重跑，不建重复账号、不冲掉已有 Key。
   坏掉的样子：Sub2API 上多出一个没人认领的账号，或者账单上多一把
   本地已经认不出来的 Key 在持续计费。
2. **降级**——Sub2API 整个连不上时，建成员照常成功、成员照常登录进工作台，
   只是显示「需要手工处理」。**这条是产品底线**：坏掉的样子是网关一停机，
   新人一个都进不来，而他们本来只是暂时不能生图而已。
3. **不可重试的绝不重试**——两步验证、密码对不上、建 Key 撞上 429。
   最贵的是最后一个：换个后缀再撞一次，就把这个用户在网关那边锁死一小时。
4. **密码清空**——默认配置下进入 active 之后 remote_password_enc 必须为空。
   坏掉的样子不会有任何症状，只是库里长期躺着一堆可登录凭据。
5. **不泄露**——日志、接口响应、报错信息里都不出现密码和完整 Key。
6. **冒烟不过不算开通**——403「没绑分组」时状态绝不能进 active，
   否则就是「建了号但一张图也发不出去」，而界面显示一切正常。

对着同目录的 `fake_sub2api.py` 跑，**不联网、不花钱、不用起服务**，
用户那台真实的 Sub2API（192.168.31.235）全程不碰。
"""
import atexit
import json
import logging
import os
import re
import shutil
import sys
import tempfile
import unittest
import uuid

# 见 test_provisioning.py 里同一处注释：绝不写死绝对路径。
_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(_HERE))
sys.path.insert(0, _HERE)

# 导入 backend.app.config 会在 import 期间就去建数据目录。测试绝不该碰用户放着
# 网关 Key 和生产数据的那个 data/，所以在导入之前先把它指到临时位置去。
if not os.environ.get("DESIGNKIT_DATA_DIR"):
    _TMP = tempfile.mkdtemp(prefix="dk-provision-regression-")
    os.environ["DESIGNKIT_DATA_DIR"] = _TMP
    atexit.register(shutil.rmtree, _TMP, ignore_errors=True)
os.environ.setdefault("DESIGNKIT_PROVIDER", "mock")

import fake_sub2api  # noqa: E402

from fastapi import FastAPI  # noqa: E402
from fastapi.testclient import TestClient  # noqa: E402

from backend.app.database import SessionLocal, engine  # noqa: E402
from backend.app.models import Base, User, UserGatewayAccount  # noqa: E402
from backend.app.routers import auth as auth_router  # noqa: E402
from backend.app.routers import generations as generations_router  # noqa: E402
from backend.app.routers import settings_router  # noqa: E402
from backend.app.routers import users as users_router  # noqa: E402
from backend.app.security import create_token, hash_password  # noqa: E402
from backend.app.services import (  # noqa: E402
    provisioning, secrets_box, settings_service, sub2api, user_gateway,
)

# 这几个键在测试之间会被改来改去。跑完必须还原成默认值：app_settings 是**全库一行**
# 的，留着 sub2api_auto_provision=True 会让同一次 discover 里排在后面的测试
# 莫名其妙地开始给自己建号。
_TOUCHED_SETTINGS = (
    "sub2api_auto_provision", "sub2api_base_url", "sub2api_admin_key",
    "sub2api_group_id", "sub2api_email_domain", "sub2api_keep_password",
    "sub2api_max_attempts", "sub2api_timeout", "sub2api_verify_tls",
)


def _dead_port_base_url() -> str:
    """一个**保证连不上**的地址：起一台假网关再立刻关掉，端口就空出来了。

    比随便写一个端口号可靠：随便写的端口有极小概率正好被别的程序占着，
    那时测出来的是「连上了但接口不对」，和「网关关机了」根本是两回事。
    也比用超时来模拟快得多——连接被拒是毫秒级的，超时要等满 timeout。
    """
    base_url, _state, stop = fake_sub2api.start()
    stop()
    return base_url


class Scaffold(unittest.TestCase):
    """一台假网关 + 一个管理员 + 一个装了真实路由的 app。

    刻意**不清空 users 表**：同一次 `unittest discover` 里别的测试文件也在用这个库，
    把人删光会连带影响它们（而且外键还挂着任务表）。这里改成每个用例自己造
    带随机后缀的用户名，断言也只针对自己造的那几行。
    """

    def setUp(self):
        Base.metadata.create_all(bind=engine)
        self.db = SessionLocal()
        provisioning._reset_state_for_tests()
        sub2api._reset_login_gate_for_tests()

        self._settings_backup = {
            key: settings_service.get(self.db, key) for key in _TOUCHED_SETTINGS
        }

        self.admin = User(
            username="prov-admin-" + uuid.uuid4().hex[:8],
            password_hash=hash_password("bosspass8888"),
            role="admin", is_active=True, must_change_password=False,
        )
        self.db.add(self.admin)
        self.db.commit()
        self.admin_token = create_token(self.admin.id, self.admin.token_version or 0)

        self.base_url, self.remote, self._stop = fake_sub2api.start()
        self.configure()

        app = FastAPI()
        app.include_router(auth_router.router)
        app.include_router(users_router.router)
        app.include_router(generations_router.router)
        app.include_router(settings_router.router)
        self.client = TestClient(app)

    def tearDown(self):
        try:
            settings_service.set_many(self.db, self._settings_backup)
            self.db.commit()
        finally:
            self.db.close()
            self._stop()
            provisioning._reset_state_for_tests()

    # ────────────────────── 小工具 ──────────────────────

    def configure(self, **overrides):
        """把「管理员已经在设置页填好了」这件事写进库。

        走真实的 settings_service 而不是构造一个 ProvisioningConfig 塞进去：
        这一组要验的正是「设置页填的东西能不能被开通链路读到」，
        自己造配置对象等于把这段接线跳过去了。

        overrides 的键名必须是**设置项的真名**（sub2api_base_url 这种）。
        写错一个字母的后果极其阴险：settings_service.set_many 会静默丢掉
        RUNTIME_DEFAULTS 里没有的键，于是这一句等于没写，用例照常跑完、
        断言却在验一个完全不同的场景（这个坑我自己踩过一次：本想把地址指到
        一个连不上的端口去验降级，结果它还连着那台活的假网关，测出来一片绿）。
        所以这里当场拦住。
        """
        unknown = [key for key in overrides if key not in _TOUCHED_SETTINGS]
        assert not unknown, "设置项名字写错了：%s" % unknown
        values = {
            "sub2api_auto_provision": True,
            "sub2api_base_url": self.base_url,
            "sub2api_admin_key": fake_sub2api.ADMIN_KEY,
            "sub2api_group_id": "7",
            "sub2api_email_domain": "designkit.local",
            "sub2api_keep_password": False,
            "sub2api_max_attempts": 5,
            "sub2api_timeout": 5,
            "sub2api_verify_tls": True,
        }
        values.update(overrides)
        settings_service.set_many(self.db, values)
        self.db.commit()

    def admin_headers(self):
        return {"Authorization": "Bearer " + self.admin_token}

    def create_member(self, password="init8888"):
        """管理员在「成员账号」页建一个人，走真实 HTTP 接口。"""
        name = "prov-" + uuid.uuid4().hex[:10]
        resp = self.client.post(
            "/api/web/users",
            json={"username": name, "password": password, "role": "member"},
            headers=self.admin_headers(),
        )
        return resp, name

    def fresh(self, user_id):
        """重新从库里读这一行。

        必须先 commit：Session 是 expire_on_commit=False，而接口那边用的是
        另一个 Session，不结束本地这个读事务的话读到的是它开始时的快照。
        """
        self.db.commit()
        self.db.expire_all()
        return user_gateway.get_account(self.db, user_id)

    def run_machine(self, user_id):
        """推进一轮状态机。先清掉登录节流计时，免得每个用例真等 6 秒。"""
        sub2api._reset_login_gate_for_tests()
        return provisioning.provision_user(self.db, user_id)

    def user_id_of(self, username):
        return self.db.query(User).filter(User.username == username).one().id

    def gateway_of(self, user_id):
        """从成员列表接口里取这一行的 gateway 字段（前端看到的就是它）。"""
        resp = self.client.get("/api/web/users", headers=self.admin_headers())
        self.assertEqual(resp.status_code, 200)
        for row in resp.json():
            if row["id"] == user_id:
                return row["gateway"]
        self.fail("成员列表里找不到 user_id=%s" % user_id)


# ══════════════════════════════════════════════════════════════════
#  线 2：降级——网关整个连不上时，人照样进得来
# ══════════════════════════════════════════════════════════════════

class GatewayDownTests(Scaffold):
    """**这一组是产品底线，不许因为「跑得慢」之类的理由删掉。**

    模拟的是最常见的一种线上状况：NAS 上的 Sub2API 容器停了 / 换了地址 /
    正在升级。此时 designkit 唯一允许的表现是「生图那一步暂时不可用」，
    绝不能是「新人建不了号」或者「建好的人登不进来」。
    """

    def setUp(self):
        super().setUp()
        # 把配置指向一个连不上的地址。注意假网关本身还开着——
        # 就是要证明「就算别处有个能通的服务，配置指向的这台停了照样得降级」。
        self.dead = _dead_port_base_url()
        self.configure(sub2api_base_url=self.dead)

    def test_create_member_succeeds_when_gateway_is_down(self):
        resp, _name = self.create_member()
        self.assertEqual(resp.status_code, 200, resp.text)
        self.assertEqual(resp.json()["gateway"]["state"], "pending")

    def test_create_member_sends_zero_requests(self):
        """建号接口一行 HTTP 都不发——降级第一层就是靠这个成立的。"""
        self.create_member()
        self.assertEqual(sum(self.remote.calls.values()), 0)

    def test_three_members_in_a_row_all_get_in(self):
        """一个人失败不影响下一个人：网关坏着的时候照样能连着建。"""
        for _ in range(3):
            resp, _name = self.create_member()
            self.assertEqual(resp.status_code, 200, resp.text)

    def test_member_can_log_in_and_open_workbench(self):
        """开通没做成，但这个人必须能登录、改密、进工作台。"""
        _resp, name = self.create_member(password="init8888")
        login = self.client.post(
            "/api/web/auth/login", json={"username": name, "password": "init8888"})
        self.assertEqual(login.status_code, 200, login.text)
        token = login.json()["token"]

        # 首登强制改密（这是既有的闸门，开通失败不该把它变成死路）
        changed = self.client.post(
            "/api/web/auth/change_password",
            json={"old_password": "init8888", "new_password": "member8888"},
            headers={"Authorization": "Bearer " + token},
        )
        self.assertEqual(changed.status_code, 200, changed.text)
        token = changed.json()["token"]

        # 进工作台：历史列表打得开，就说明人已经在系统里了
        board = self.client.get(
            "/api/web/generations", headers={"Authorization": "Bearer " + token})
        self.assertEqual(board.status_code, 200, board.text)

    def test_background_round_records_failure_without_raising(self):
        """后台那一轮碰到连不上，只能是记一笔，绝不能把异常抛出来。"""
        _resp, name = self.create_member()
        uid = self.user_id_of(name)
        result = self.run_machine(uid)  # 不抛异常本身就是断言
        self.assertEqual(result.outcome, "failed")
        self.assertEqual(result.error_code, "E_NETWORK")
        self.assertEqual(self.fresh(uid).state, "failed")

    def test_member_row_shows_plain_chinese_reason(self):
        """界面上给管理员看的必须是人话，不能是 E_NETWORK 这种分类码。"""
        _resp, name = self.create_member()
        uid = self.user_id_of(name)
        self.run_machine(uid)
        gateway = self.gateway_of(uid)
        self.assertFalse(gateway["configured"])
        self.assertNotEqual(gateway["state"], "active")
        self.assertTrue(gateway["last_error"])
        self.assertNotIn("E_", gateway["last_error"])   # 分类码单独放在 error_code 里
        self.assertEqual(gateway["error_code"], "E_NETWORK")

    def test_manual_key_channel_still_works(self):
        """管理员手工填 Key 这条路永远保留——自动开通只是省掉这一步，不是取代它。"""
        _resp, name = self.create_member()
        uid = self.user_id_of(name)
        self.run_machine(uid)
        saved = self.client.put(
            "/api/web/users/%d/gateway" % uid,
            json={"api_key": "sk-manual-1234567890"},
            headers=self.admin_headers(),
        )
        self.assertEqual(saved.status_code, 200, saved.text)
        self.assertTrue(saved.json()["gateway"]["configured"])
        self.assertEqual(self.fresh(uid).state, "active")

    def test_provision_button_never_explodes(self):
        """网关连不上时点「重新开通」，也必须是 200 + 一句人话，不是 500。"""
        _resp, name = self.create_member()
        uid = self.user_id_of(name)
        self.run_machine(uid)
        resp = self.client.post(
            "/api/web/users/%d/gateway/provision" % uid, headers=self.admin_headers())
        self.assertEqual(resp.status_code, 200, resp.text)
        body = resp.json()
        self.assertIsInstance(body["ok"], bool)
        self.assertTrue(body["message"])


# ══════════════════════════════════════════════════════════════════
#  线 1：可重入——中断后重跑，不多建、不覆盖
# ══════════════════════════════════════════════════════════════════

class ReentrancyTests(Scaffold):
    """每个用例都在某一步故意中断一次，然后重跑，看远端多出了什么。

    这几种坑的共同点是**一次性手测全都通过**：只有断电重启、容器滚动更新、
    两个 worker 同时领到同一个人的时候才会露出来，而那时的症状是账单上
    多一笔没人认领的钱，隔一个月才有人发现。
    """

    def enroll(self):
        _resp, name = self.create_member()
        return self.user_id_of(name)

    def test_interrupted_after_remote_user_created_does_not_duplicate(self):
        """远端把号建好了，但响应没回来（最经典的中断点）。重跑必须走反查。"""
        self.remote.fail_admin_create_after_effect = 1
        uid = self.enroll()
        first = self.run_machine(uid)
        self.assertEqual(first.outcome, "failed")          # 本地只看到 500
        self.assertEqual(len(self.remote.users), 1)        # 远端其实已经建好了

        second = self.run_machine(uid)
        self.assertEqual(second.outcome, "active", second.message)
        # 关键：远端仍然只有一个账号。第二次 POST 收到 409，靠反查认领了它
        self.assertEqual(len(self.remote.users), 1)
        self.assertEqual(self.remote.count("POST", "/api/v1/admin/users"), 2)
        self.assertGreaterEqual(self.remote.count("GET", "/api/v1/admin/users"), 1)
        self.assertEqual(str(self.fresh(uid).remote_user_id), str(self.remote.users[0]["id"]))

    def test_409_lookup_picks_the_exact_email_not_the_first_hit(self):
        """反查是模糊匹配。取错人的后果是把 Key 建到别人账号上，且毫无报错。

        假网关刻意把「像但不等」的那条排在第一位（搜 dk7@ 先返回 dk79@），
        直接取第一条就会翻车——这正是 sub2api.py 文档里警告的形态。
        """
        self.remote.fail_admin_create_after_effect = 1
        uid = self.enroll()
        self.run_machine(uid)
        ours = self.remote.users[0]

        # 造一个「邮箱前缀一样」的旁人：dk<uid>9@，会被模糊搜到且排在前面
        self.remote.users.append({
            "id": 999999, "email": "dk%d9@designkit.local" % uid,
            "username": "someone-else", "password": "x", "concurrency": 5,
            "rpm_limit": 0, "allowed_groups": ["7"], "deleted": False,
        })

        self.assertEqual(self.run_machine(uid).outcome, "active")
        self.assertEqual(str(self.fresh(uid).remote_user_id), str(ours["id"]))
        self.assertNotEqual(str(self.fresh(uid).remote_user_id), "999999")

    def test_interrupted_after_key_created_reuses_the_same_key(self):
        """Key 发出去了、响应没回来。重跑必须把那一把读回来，而不是再发一把。"""
        self.remote.fail_key_create_after_effect = 1
        uid = self.enroll()
        self.assertEqual(self.run_machine(uid).outcome, "failed")
        self.assertEqual(len(self.remote.keys), 1)

        self.assertEqual(self.run_machine(uid).outcome, "active")
        self.assertEqual(len(self.remote.keys), 1)   # 没有第二把
        self.assertEqual(
            user_gateway.decrypt_api_key(self.fresh(uid)), self.remote.keys[0]["key"])

    def test_existing_key_is_never_overwritten(self):
        """库里已经有 Key 了就绝不再建——这是「不冲掉已有 Key」的硬保证。

        冲掉的后果：旧 Key 还挂在 Sub2API 上继续计费，而本地已经认不出它了。
        """
        uid = self.enroll()
        self.assertEqual(self.run_machine(uid).outcome, "active")
        original = user_gateway.decrypt_api_key(self.fresh(uid))
        posts = self.remote.count("POST", "/api/v1/keys")

        # 把状态打回去，模拟「上一轮崩在半路、state 落后于事实」
        account = self.fresh(uid)
        account.state = "pending"
        self.db.commit()

        self.assertEqual(self.run_machine(uid).outcome, "active")
        self.assertEqual(user_gateway.decrypt_api_key(self.fresh(uid)), original)
        self.assertEqual(self.remote.count("POST", "/api/v1/keys"), posts)
        self.assertEqual(len(self.remote.keys), 1)

    def test_running_five_times_leaves_exactly_one_user_and_one_key(self):
        uid = self.enroll()
        for _ in range(5):
            self.run_machine(uid)
        self.assertEqual(len(self.remote.users), 1)
        self.assertEqual(len(self.remote.keys), 1)
        self.assertEqual(self.fresh(uid).state, "active")

    def test_credentials_are_never_regenerated(self):
        """远端邮箱和密码一旦写入就永不重摇——重摇等于远端账号永久失联。"""
        uid = self.enroll()
        before = self.fresh(uid)
        email, password = before.remote_email, before.remote_password_enc
        self.configure(sub2api_keep_password=True)  # 免得走到 active 被正常清掉
        for _ in range(3):
            self.run_machine(uid)
        after = self.fresh(uid)
        self.assertEqual(after.remote_email, email)
        self.assertEqual(after.remote_password_enc, password)

    def test_no_delete_or_topup_route_is_ever_called(self):
        """全程不碰任何删除 / 充值接口——「重复扣钱」这一类风险是结构性不存在的。"""
        uid = self.enroll()
        for _ in range(3):
            self.run_machine(uid)
        touched = ["%s %s" % (m, p) for (m, p) in self.remote.calls]
        for method, path in self.remote.calls:
            self.assertNotEqual(method, "DELETE", touched)
            self.assertNotIn("balance", path, touched)
            self.assertNotIn("redeem", path, touched)
        # 顺带守住那条写错的路由：照 Sub2API 源码注释写 /api/v1/api-keys 会 404
        self.assertEqual(self.remote.count("GET", "/api/v1/api-keys"), 0)


# ══════════════════════════════════════════════════════════════════
#  线 3：不可重试的绝不重试
# ══════════════════════════════════════════════════════════════════

class NoRetryTests(Scaffold):
    def enroll(self):
        _resp, name = self.create_member()
        return self.user_id_of(name)

    def test_two_factor_is_recorded_and_not_rescheduled(self):
        """用户自己开了两步验证：服务端拿不到动态码，再试一万次也一样。"""
        self.remote.login_requires_2fa = True
        uid = self.enroll()
        result = self.run_machine(uid)
        self.assertEqual(result.error_code, "E_LOGIN_2FA")
        account = self.fresh(uid)
        self.assertEqual(account.state, "failed")
        # 不排期 = 不会被后台自动领走，只能等管理员在界面上处理
        self.assertIsNone(provisioning.retry_due_at(account))
        self.assertNotIn(uid, provisioning.due_user_ids(self.db))

    def test_password_mismatch_logs_in_exactly_once(self):
        """用户自己改了 Sub2API 密码：不可自愈，且绝不反复去撞登录限流。"""
        self.remote.login_status = 401
        uid = self.enroll()
        for _ in range(3):
            self.run_machine(uid)   # 手动连推三轮，模拟管理员连点三次
        account = self.fresh(uid)
        self.assertEqual(provisioning.parse_last_error(account.last_error)[0],
                         "E_PASSWORD_MISMATCH")
        self.assertIsNone(provisioning.retry_due_at(account))
        # 每一轮最多打一次登录：三轮就是三次，不能出现「一轮里重试好几次」
        self.assertLessEqual(self.remote.count("POST", "/api/v1/auth/login"), 3)

    def test_key_rate_limit_posts_keys_exactly_once(self):
        """**最贵的一条**：建 Key 撞上 429 必须立刻停手。

        再撞一次就把这个用户在网关那边锁死一小时（自定义 key 冲突计数
        20 次/小时/用户）。所以这里不是「少重试几次」，是「一次都不许再来」。
        """
        self.remote.key_create_status = 429
        self.remote.key_create_body = {"error": "rate limit exceeded"}
        uid = self.enroll()
        result = self.run_machine(uid)
        self.assertEqual(result.error_code, "E_KEY_RATE_LIMITED")
        self.assertEqual(self.remote.count("POST", "/api/v1/keys"), 1)

    def test_key_rate_limit_does_not_try_another_suffix(self):
        """撞 429 之后不许换个 custom_key 再来——次数一样、值不同就是换后缀了。"""
        self.remote.key_create_status = 429
        self.remote.key_create_body = {"error": "rate limit exceeded"}
        uid = self.enroll()
        self.run_machine(uid)
        self.run_machine(uid)   # 下一轮重来，允许再试，但必须是同一个 custom_key
        submitted = {body.get("custom_key") for body in self.remote.create_key_bodies}
        self.assertEqual(len(submitted), 1, self.remote.create_key_bodies)

    def test_non_retryable_failures_are_not_picked_up_by_the_worker(self):
        """不可重试的人不能一直躺在后台队列里被反复捞出来。"""
        self.remote.login_requires_2fa = True
        uid = self.enroll()
        self.run_machine(uid)
        self.assertNotIn(uid, provisioning.due_user_ids(self.db))


# ══════════════════════════════════════════════════════════════════
#  线 4：密码清空
# ══════════════════════════════════════════════════════════════════

class PasswordWipeTests(Scaffold):
    def enroll(self):
        _resp, name = self.create_member()
        return self.user_id_of(name)

    def test_password_is_wiped_after_active(self):
        """默认配置下开通成功即清空密码：长期保管的只是一堆 Key，不是可登录凭据。"""
        uid = self.enroll()
        self.assertTrue(self.fresh(uid).remote_password_enc)  # 开通前确实有
        self.assertEqual(self.run_machine(uid).outcome, "active")
        account = self.fresh(uid)
        self.assertEqual(account.state, "active")
        self.assertFalse(account.remote_password_enc)
        self.assertTrue(account.api_key_enc)   # Key 还在，人照样能生图

    def test_password_kept_when_admin_asked_to_keep_it(self):
        """对照组：开关打开就得留着。两边都测，才能证明是配置在起作用。"""
        self.configure(sub2api_keep_password=True)
        uid = self.enroll()
        self.assertEqual(self.run_machine(uid).outcome, "active")
        self.assertTrue(self.fresh(uid).remote_password_enc)

    def test_password_survives_a_failed_smoke(self):
        """清空必须**严格发生在 active 之后**。

        提前清掉的后果：这一行永远建不成 Key（建 Key 那一步要代登录），
        而且不可逆——只能改走手工发 Key。
        """
        self.remote.usage_status = 403
        uid = self.enroll()
        self.assertEqual(self.run_machine(uid).outcome, "failed")
        self.assertTrue(self.fresh(uid).remote_password_enc)

    def test_wiped_row_is_not_reshuffled_on_the_next_round(self):
        """密码清了之后再跑一轮，不许「顺手重摇一个新密码」。"""
        uid = self.enroll()
        self.run_machine(uid)
        email = self.fresh(uid).remote_email
        self.run_machine(uid)
        account = self.fresh(uid)
        self.assertFalse(account.remote_password_enc)
        self.assertEqual(account.remote_email, email)
        self.assertEqual(account.state, "active")


# ══════════════════════════════════════════════════════════════════
#  线 6：冒烟不过就不算开通
# ══════════════════════════════════════════════════════════════════

class SmokeGateTests(Scaffold):
    """「建了号但一张图也发不出去」正是这一步要防的形态。

    前面每一步都只能证明 HTTP 200，证明不了这把 Key 真能发出请求去。
    少了这道闸，用户会在点「生成」的时候才发现，而那时的报错是从生图链路里
    冒出来的，谁也想不到是开通没做完。
    """

    def enroll(self):
        _resp, name = self.create_member()
        return self.user_id_of(name)

    def test_403_no_group_never_becomes_active(self):
        self.remote.usage_status = 403
        self.remote.usage_body = {"error": "API Key is not assigned to any group"}
        uid = self.enroll()
        result = self.run_machine(uid)
        self.assertEqual(result.error_code, "E_NO_GROUP")
        self.assertNotEqual(self.fresh(uid).state, "active")

    def test_403_reason_tells_the_admin_where_to_click(self):
        """报错要写清「该去哪里点什么」，不是一句 403。"""
        self.remote.usage_status = 403
        uid = self.enroll()
        self.run_machine(uid)
        message = self.gateway_of(uid)["last_error"]
        self.assertIn("分组", message)

    def test_401_rejected_key_never_becomes_active(self):
        self.remote.usage_status = 401
        uid = self.enroll()
        self.assertEqual(self.run_machine(uid).outcome, "failed")
        self.assertNotEqual(self.fresh(uid).state, "active")

    def test_key_is_kept_after_a_failed_smoke(self):
        """冒烟没过不等于 Key 没发出去：下一轮必须复用它，不能再发一把。"""
        self.remote.usage_status = 403
        uid = self.enroll()
        self.run_machine(uid)
        self.assertTrue(self.fresh(uid).api_key_enc)
        self.run_machine(uid)
        self.assertEqual(len(self.remote.keys), 1)
        self.assertEqual(self.remote.count("POST", "/api/v1/keys"), 1)

    def test_recovers_once_the_group_is_fixed(self):
        """管理员把分组配对之后，下一轮自己就好了，不需要人再点什么。"""
        self.remote.usage_status = 403
        uid = self.enroll()
        self.run_machine(uid)
        self.remote.usage_status = 0
        self.assertEqual(self.run_machine(uid).outcome, "active")
        self.assertEqual(self.fresh(uid).state, "active")


# ══════════════════════════════════════════════════════════════════
#  线 5：不泄露
# ══════════════════════════════════════════════════════════════════

class _LogTrap(logging.Handler):
    """把所有日志（含 DEBUG）攒起来，跑完全文搜一遍。"""

    def __init__(self):
        logging.Handler.__init__(self, level=logging.DEBUG)
        self.lines = []

    def emit(self, record):
        try:
            self.lines.append(record.getMessage())
        except Exception:      # 格式化失败也不能把测试带崩
            self.lines.append(repr(record.args))

    def text(self):
        return "\n".join(self.lines)


class NoLeakTests(Scaffold):
    """密码和完整 Key 一个字都不许出现在日志、接口响应和报错信息里。

    为什么必须由测试守着而不是靠代码评审：泄露的写法看起来都很无辜——
    `logger.debug("请求体 %s", payload)` 里就带着密码，
    `return account.__dict__` 里就带着密文和末 4 位。它们不会报错，
    只会安静地把凭据抄进日志文件，而日志文件是会被打包发给别人排错的。
    """

    def enroll(self):
        _resp, name = self.create_member()
        return self.user_id_of(name)

    def secrets_in_play(self, uid):
        """这一轮里所有「绝不能出现在外面」的明文。"""
        found = [fake_sub2api.ADMIN_KEY]
        found.extend(u["password"] for u in self.remote.users if u.get("password"))
        found.extend(k["key"] for k in self.remote.keys)
        found.extend(self.remote.tokens.keys())          # JWT
        account = self.fresh(uid)
        if account is not None and account.remote_password_enc:
            found.append(secrets_box.decrypt(account.remote_password_enc))
        return [s for s in found if s]

    def test_full_flow_logs_contain_no_secret(self):
        """把整条链路（建号→代登录→建 Key→冒烟）的 DEBUG 全量日志抓下来全文搜。"""
        trap = _LogTrap()
        root = logging.getLogger()
        old_level = root.level
        root.addHandler(trap)
        root.setLevel(logging.DEBUG)
        try:
            self.configure(sub2api_keep_password=True)  # 留着密码，才搜得出「有没有被打出去」
            uid = self.enroll()
            self.assertEqual(self.run_machine(uid).outcome, "active")
        finally:
            root.removeHandler(trap)
            root.setLevel(old_level)

        text = trap.text()
        self.assertTrue(text.strip(), "一条日志都没抓到，这个用例等于没跑")
        for secret in self.secrets_in_play(uid):
            self.assertNotIn(secret, text, "日志里出现了明文凭据（末 4 位 %s）" % secret[-4:])

    def test_failure_message_never_carries_the_password(self):
        """报错信息也算「外面」：last_error 会原样显示给管理员看。"""
        self.remote.login_status = 401
        self.configure(sub2api_keep_password=True)
        uid = self.enroll()
        self.run_machine(uid)
        account = self.fresh(uid)
        password = secrets_box.decrypt(account.remote_password_enc)
        self.assertNotIn(password, account.last_error)
        self.assertNotIn(fake_sub2api.ADMIN_KEY, account.last_error)

    def test_member_list_api_has_no_secret(self):
        uid = self.enroll()
        self.run_machine(uid)
        resp = self.client.get("/api/web/users", headers=self.admin_headers())
        for secret in self.secrets_in_play(uid):
            self.assertNotIn(secret, resp.text)

    def test_member_list_has_no_key_tail_and_no_remote_email(self):
        """连末 4 位和远端邮箱都不给前端。

        末 4 位单独看没用，但 designkit、Sub2API 后台、上游账单三处各有一份，
        凑在一起就能把人、Key、花的钱三边对上号（规矩立在 user_gateway.py）。
        """
        uid = self.enroll()
        self.run_machine(uid)
        gateway = self.gateway_of(uid)
        self.assertNotIn("api_key_tail", gateway)
        self.assertNotIn("remote_email", gateway)
        self.assertNotIn("remote_password_enc", gateway)
        account = self.fresh(uid)
        self.assertNotIn(account.api_key_tail, json.dumps(gateway, ensure_ascii=False))

    def test_provision_button_response_has_no_secret(self):
        uid = self.enroll()
        self.run_machine(uid)
        resp = self.client.post(
            "/api/web/users/%d/gateway/provision" % uid, headers=self.admin_headers())
        self.assertEqual(resp.status_code, 200, resp.text)
        for secret in self.secrets_in_play(uid):
            self.assertNotIn(secret, resp.text)

    def test_settings_api_masks_the_admin_key(self):
        """管理员 Key 是整台 Sub2API 的最高权限，界面上只需表达「填了没有」。"""
        resp = self.client.get("/api/web/settings", headers=self.admin_headers())
        self.assertEqual(resp.status_code, 200, resp.text)
        self.assertNotIn(fake_sub2api.ADMIN_KEY, resp.text)
        value = resp.json().get("sub2api_admin_key")
        self.assertTrue(value)                      # 「填了」这件事要看得出来
        self.assertNotIn(fake_sub2api.ADMIN_KEY[-4:], str(value))

    def test_selfcheck_response_has_no_secret(self):
        """自检会真去读远端，报的东西最多，最容易顺手把凭据带出来。"""
        uid = self.enroll()
        self.assertEqual(self.run_machine(uid).outcome, "active")
        resp = self.client.post(
            "/api/web/settings/test_provisioning", headers=self.admin_headers())
        self.assertEqual(resp.status_code, 200, resp.text)   # 自检永远 200，成败看 level
        for secret in self.secrets_in_play(uid):
            self.assertNotIn(secret, resp.text)
        # 探针 detail 是别的模块产出的字典，原样回吐就会把「抽查的那把 Key 的
        # 末 4 位」带出来。字段名也一并守住：出现这几个名字就说明有人把整行
        # 序列化了，而不是按白名单挑字段。
        # 注意不能直接搜 "password"——响应里本来就有一个 keep_password 开关，
        # 那是设置项名字，不是凭据。
        self.assertNotIn("key_tail", resp.text)
        self.assertNotIn("remote_password", resp.text)
        self.assertNotIn("api_key_enc", resp.text)


class BadInputTests(Scaffold):
    """第七条线：**「测试能不能自动开通」这个按钮，无论填了什么都不许崩**。

    这一组来自一次真实事故。用户把输入框里那句灰色提示文字
    「粘贴网关后台生成的管理员 Key」当成内容粘进了管理员 Key 栏，保存成功了；
    点自检时 httpx 要把它塞进 x-api-key 请求头，而 HTTP 头只能装 latin-1，
    于是抛 UnicodeEncodeError，界面上原样显示成：

        自检过程中出现意外错误（UnicodeEncodeError）。请确认 Sub2API 地址填得对不对

    两处都错得很严重：一是把 Python 的异常类名甩给了一个非技术用户，
    二是那句「请确认地址」把她引向了完全错误的方向——真因在 Key 那一栏。

    为什么必须长期跑：这个按钮是她遇到问题时**唯一的自助工具**。
    它自己崩掉，她就彻底没有下一步了——既看不出哪里错，也没有别的地方可查。
    """

    #: 保存时就该被拦下的输入，以及提示里必须出现的关键词（点出真因，不是泛泛而谈）
    REJECTED = (
        ("含中文（把提示文字粘进来了）", {"sub2api_admin_key": "粘贴网关后台生成的管理员 Key"}, "中文"),
        ("超长（整段文字都粘进来了）", {"sub2api_admin_key": "a" * 5000}, "太长"),
        ("夹着换行/制表符", {"sub2api_admin_key": "admin-abc\n\tdef"}, "换行"),
        ("地址里带用户名密码", {"sub2api_base_url": "http://user:pass@127.0.0.1:9"}, "用户名"),
        ("端口不是数字", {"sub2api_base_url": "http://127.0.0.1:abc"}, "端口"),
    )

    def test_bad_values_are_rejected_on_save_with_a_real_reason(self):
        for name, patch, keyword in self.REJECTED:
            with self.subTest(name):
                resp = self.client.put(
                    "/api/web/settings", json=patch, headers=self.admin_headers())
                self.assertEqual(resp.status_code, 422, name)
                detail = str(resp.json().get("detail") or "")
                self.assertIn(keyword, detail,
                              "提示没点出真因，用户会照着它去改错的地方：%s" % detail)

    def test_selfcheck_never_leaks_a_python_exception_name(self):
        """脏数据已经躺在库里的情况——保存时的校验只挡新值，挡不住升级前存进去的。

        所以绕过接口直接写库（用户的实例当时就是这个状态），再点自检。
        """
        settings_service.set_many(self.db, {
            "sub2api_admin_key": "粘贴网关后台生成的管理员 Key",
            "sub2api_base_url": _dead_port_base_url(),
        })
        self.db.commit()

        resp = self.client.post(
            "/api/web/settings/test_provisioning", headers=self.admin_headers())
        self.assertEqual(resp.status_code, 200, "自检本身不许 500")

        # 整个响应里不许出现任何 XxxError —— 那是把内部实现甩给用户看
        leaked = sorted(set(re.findall(r"[A-Za-z]+Error", resp.text)))
        self.assertEqual(leaked, [], "自检把 Python 异常名泄露给用户了：%s" % leaked)

        # 而且必须真的指出是 Key 那一栏的问题，不能含糊成「请确认地址」
        self.assertIn("中文", resp.text)

    def test_selfcheck_survives_every_bad_value_that_is_already_in_the_db(self):
        """把每一种脏值都直接写进库跑一遍，断言自检永远给出结构化结果。"""
        for name, patch, _ in self.REJECTED:
            with self.subTest(name):
                settings_service.set_many(self.db, patch)
                self.db.commit()
                resp = self.client.post(
                    "/api/web/settings/test_provisioning", headers=self.admin_headers())
                self.assertEqual(resp.status_code, 200, name)
                body = resp.json()
                self.assertIn(body.get("level"), ("red", "yellow", "green"), name)
                leaked = sorted(set(re.findall(r"[A-Za-z]+Error", resp.text)))
                self.assertEqual(leaked, [], "%s：泄露了 %s" % (name, leaked))

    def test_a_valid_admin_key_still_saves(self):
        """别把校验写得太紧——真实的 Key 是 admin- 加 64 位十六进制，必须能存进去。"""
        good = "admin-" + "3f9c1a2b" * 8
        resp = self.client.put(
            "/api/web/settings", json={"sub2api_admin_key": good},
            headers=self.admin_headers())
        self.assertEqual(resp.status_code, 200, resp.text)
        self.assertEqual(settings_service.get(self.db, "sub2api_admin_key"), good)


if __name__ == "__main__":
    unittest.main()
