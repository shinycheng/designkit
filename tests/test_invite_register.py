"""邀请码 + 自助注册：回归测试（不联网、不花钱、不用起服务、不碰 data/）。

════════════════════════════════════════════════════════════════════════
 这一组守的是什么
════════════════════════════════════════════════════════════════════════
本期做的是「让陌生人不经管理员就能自己开户」。这件事一旦做错，代价和别的
功能不是一个量级——别的功能坏了是「某个人用不了」，这个坏了是
「服务器被外人使唤」或者「一张一次性码开出一堆号」。所以下面六条线
每一条都对应一种**不会报错、手工点一遍全都正常**的事故：

1. **开放注册的硬前置闸门（最贵的一条）**
   `allow_internal_targets` 默认是**开着**的（config.py，为了让现有 ERP 能取
   内网里的图）。开着它，对外 API 的 image_urls / callback_url 可以让**服务端**
   去访问内网任意 http 地址——包括同一内网里 Sub2API 的管理端口，那上面挂着
   能建号、能改余额、能读任意用户明文 API Key 的接口。
   而自助注册意味着陌生人能自己拿到一把 ERP Key。两者叠在一起 =
   「把内网的门连同钥匙一起发出去」。
   闸门必须是**双向**的：内网开着就打不开注册，注册开着就不许把内网改回来。
   坏掉的样子：设置页保存成功、界面一切正常，没有任何报错。

2. **邀请码并发扣减**
   两个人在同一瞬间用同一张只剩一次名额的码。「先查够不够、再写回去」在并发下
   两边都会通过，一张一次性码开出两个号。
   坏掉的样子：顺序调两次接口永远测不出来，只有真有人拿脚本刷的时候才发生。
   所以这里的并发测试**必须真的开线程**，把顺序调用改回去就等于删了这组测试。

3. **限速跨进程共享同一个桶**
   公网部署要开多 worker。限速原来是进程内的 `defaultdict(deque)`，
   设置里写「10 次」实际变成「10 × 进程数」，而界面上完全看不出来。
   这里**真的另起一个 python 进程**去撞，撞完回到本进程看还认不认账。

4. **客户端 IP 从 X-Forwarded-For 右边数**
   最左边那一段是客户端自己写的。照最左边算限速，攻击者每次换一个假 IP
   就等于没有限速——而且限速表还会被撑爆。

5. **注册与开通解耦**
   网关整个连不上时，注册照常成功、人照常登录进来，只是暂时不能生图。
   坏掉的样子：网关一停机，新人一个都进不来。

6. **报错不许变成探测器**
   邀请码「不存在」和「已用完」必须回同一句话，否则这个接口就是一台
   「这串字符是不是有效邀请码」的判定器，可以拿去枚举。

════════════════════════════════════════════════════════════════════════
 为什么每个用例自己起一个库，而不是用公共的那个
════════════════════════════════════════════════════════════════════════
这一组要反复改**全局设置**（self_register_enabled / allow_internal_targets /
限速阈值），而 app_settings 是全库一行的。用公共库的话，同一次
`unittest discover` 里排在后面的测试文件会莫名其妙地跑在「注册已开放」
或者「限速阈值是 3」的环境里，症状还飘忽不定。
所以这里每个用例一个独立的 SQLite 文件库 + `dependency_overrides[get_db]`，
跑完就删。用文件库而不是 `:memory:`，是因为并发和跨进程那两组要让
多个连接（甚至多个进程）看到同一份数据。
"""
import atexit
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
import uuid
from datetime import timedelta

_HERE = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.dirname(_HERE)
# 绝不写死绝对路径（同 test_provisioning.py 里那一处注释）：仓库被挪到别的目录
# 之后，写死的路径会让整组测试 import 失败，而报错完全看不出根因。
if _ROOT not in sys.path:
    sys.path.insert(0, _ROOT)
# 同目录的 fake_sub2api 要能直接 import（三组「自动开通」也是这么做的）
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)

# 导入 backend.app.config 会在 import 期间就去建数据目录、改权限。测试绝不该碰
# 用户放着网关 Key 和生产数据的那个 data/，所以在导入之前先把它指到临时位置。
# 用 setdefault：按 tests/README.md 的标准跑法本来就会显式设这个变量。
if not os.environ.get("DESIGNKIT_DATA_DIR"):
    _TMP_DATA_DIR = tempfile.mkdtemp(prefix="dk-invite-data-")
    os.environ["DESIGNKIT_DATA_DIR"] = _TMP_DATA_DIR
    atexit.register(shutil.rmtree, _TMP_DATA_DIR, ignore_errors=True)
os.environ.setdefault("DESIGNKIT_PROVIDER", "mock")

import fake_sub2api  # noqa: E402

from fastapi import FastAPI  # noqa: E402
from fastapi.testclient import TestClient  # noqa: E402
from sqlalchemy import create_engine, event, select  # noqa: E402
from sqlalchemy.orm import sessionmaker  # noqa: E402

from backend.app.deps import get_db  # noqa: E402
from backend.app.models import (  # noqa: E402
    AuthIdentity, Base, InviteCode, InviteRedemption, RateLimitState, User,
    UserGatewayAccount, utcnow,
)
from backend.app.routers import auth as auth_router  # noqa: E402
from backend.app.routers import invites as invites_router  # noqa: E402
from backend.app.routers import register as register_router  # noqa: E402
from backend.app.routers import settings_router  # noqa: E402
from backend.app.security import create_token, hash_password  # noqa: E402
from backend.app.services import provisioning, ratelimit, settings_service  # noqa: E402

# 注册用的口令：够 10 位、有字母有数字、不在弱口令名单里、不含用户名。
# 写成常量是为了让「密码规则改了」只改一处——散在各处的话，
# 规则一变就会有十几个用例一起变红，而它们要测的根本不是密码。
GOOD_PASSWORD = "ShangPin2026"
# 开放注册需要的一套合法配置（走设置接口时这三条缺一不可）。
OPEN_BASELINE = {
    "allow_internal_targets": False,
    "files_signed_only": True,
    "public_base_url": "https://designkit.example.com",
}


class Scaffold(unittest.TestCase):
    """一个独立的库 + 一个装了真实路由的 app + 一个管理员。"""

    def setUp(self):
        self.tmpdir = tempfile.mkdtemp(prefix="dk-invite-")
        self.db_path = os.path.join(self.tmpdir, "t.db")
        self.engine = create_engine(
            "sqlite:///" + self.db_path,
            # check_same_thread=False：并发那一组要在别的线程里用同一个引擎。
            # timeout=30：SQLite 写锁是全库的，并发写要排队，等不起就会报
            # "database is locked"，而那和被测代码没有一点关系。
            connect_args={"check_same_thread": False, "timeout": 30},
            future=True,
        )

        @event.listens_for(self.engine, "connect")
        def _pragma(dbapi_connection, _record):
            cur = dbapi_connection.cursor()
            # WAL：读不挡写。跨进程那一组要让子进程写、父进程同时读。
            cur.execute("PRAGMA journal_mode=WAL")
            cur.execute("PRAGMA busy_timeout=30000")
            cur.close()

        Base.metadata.create_all(self.engine)
        # expire_on_commit=False 与 database.py 里的 SessionLocal 保持一致。
        self.Session = sessionmaker(
            bind=self.engine, autoflush=False, expire_on_commit=False, future=True
        )

        self.app = FastAPI()
        self.app.include_router(auth_router.router)
        self.app.include_router(invites_router.router)
        self.app.include_router(register_router.router)
        self.app.include_router(settings_router.router)
        self.app.dependency_overrides[get_db] = self._db_dependency
        self.client = TestClient(self.app)

        self.db = self.Session()
        self.admin = User(
            username="inv-admin-" + uuid.uuid4().hex[:8],
            password_hash=hash_password("bosspass8888"),
            display_name="管理员", role="admin", is_active=True,
            must_change_password=False, token_version=0,
        )
        self.db.add(self.admin)
        self.db.commit()
        self.auth = {"Authorization": "Bearer " + create_token(self.admin.id, 0)}

        # 限速表的清理是按进程节流的（60 秒一次）。逐个用例之间要放开，
        # 否则第二个用例里的 cleanup 会被上一个用例的时间戳挡掉。
        ratelimit._gc_last_at = float("-inf")
        provisioning._reset_state_for_tests()

    def tearDown(self):
        self.client.close()
        self.db.close()
        self.engine.dispose()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    # ── 脚手架小工具 ──

    def _db_dependency(self):
        session = self.Session()
        try:
            yield session
        finally:
            session.close()

    def _set(self, **kwargs):
        """直接写设置表，**绕过设置接口的闸门**。

        故意提供这条后门：闸门本身要测「库里已经处在危险组合时会怎样」，
        而那种状态按设计是设置接口存不进去的（老库遗留、或者有人直接改了数据库）。
        """
        session = self.Session()
        try:
            settings_service.set_many(session, kwargs)
        finally:
            session.close()

    def _get_setting(self, key):
        session = self.Session()
        try:
            return settings_service.get(session, key)
        finally:
            session.close()

    def _open_register(self):
        """把注册开到「能用」的状态（只动 register.py 真正会看的那两项）。"""
        self._set(self_register_enabled=True, allow_internal_targets=False,
                  self_register_ip_per_hour=1000, self_register_daily_limit=1000)

    def _put_settings(self, payload):
        return self.client.put("/api/web/settings", json=payload, headers=self.auth)

    def _make_code(self, **body):
        """发一张码，返回 (归一化后的码, 整个响应体)。"""
        response = self.client.post("/api/web/invites", json=body, headers=self.auth)
        self.assertEqual(response.status_code, 200, response.text)
        payload = response.json()
        return payload["items"][0]["code"], payload

    def _register(self, code, username="zhangsan", password=GOOD_PASSWORD,
                  display_name="", headers=None):
        body = {"invite_code": code, "username": username, "password": password}
        if display_name:
            body["display_name"] = display_name
        return self.client.post("/api/web/register", json=body, headers=headers or {})

    def _fresh(self, model, **filters):
        """用一条**新连接**去读——避免读到本进程 ORM 缓存里的旧值。

        并发和跨进程那两组必须这么读：另一个线程/进程刚写完的行，
        用老 Session 查出来可能还是提交前的样子，测出来是一片假绿。
        """
        session = self.Session()
        try:
            query = session.query(model)
            for key, value in filters.items():
                query = query.filter(getattr(model, key) == value)
            return query.all()
        finally:
            session.close()


# ══════════════════════════════════════════════════════════════════════
#  一、开放注册的硬前置闸门（本期最贵的一条线）
# ══════════════════════════════════════════════════════════════════════

class SelfRegisterGateTests(Scaffold):
    """守「陌生人能不能驱动这台服务器去访问你的内网」。

    这一条比这组里其他任何一条都贵：别的坏了是某个人用不了，这个坏了是
    内网里的 Sub2API 管理端口（能建号、改余额、读明文 Key）对外网可达。
    """

    def test_default_is_the_dangerous_one(self):
        """前提确认：allow_internal_targets 默认是**开着**的。

        这一项写成断言，是因为闸门存在的全部理由就是这个默认值。
        哪天有人把默认值改成 False（看起来更安全），这条会变红，
        提醒他去确认「现有 ERP 内网取图是不是被静默打断了」——
        那是一个产品决定，不该由一次顺手的默认值调整替大家做。
        """
        self.assertTrue(self._get_setting("allow_internal_targets"))
        self.assertFalse(self._get_setting("self_register_enabled"))

    def test_cannot_open_register_while_internal_targets_on(self):
        """内网访问开着时，打不开注册。"""
        response = self._put_settings({"self_register_enabled": True})
        self.assertEqual(response.status_code, 422, response.text)
        detail = response.json()["detail"]
        self.assertIn("允许对外接口访问内网地址", detail)
        # 存不进去才叫闸门——「报了个错但还是存了」是最坏的结果。
        self.assertFalse(self._get_setting("self_register_enabled"))

    def test_cannot_turn_internal_targets_back_on_while_register_open(self):
        """反方向：注册开着时，改不回 allow_internal_targets=True。

        闸门只堵一个方向等于没堵——先关内网开注册，再把内网开回来，
        照样能到达那个危险组合。
        """
        payload = dict(OPEN_BASELINE)
        payload["self_register_enabled"] = True
        self.assertEqual(self._put_settings(payload).status_code, 200)
        self.assertTrue(self._get_setting("self_register_enabled"))

        response = self._put_settings({"allow_internal_targets": True})
        self.assertEqual(response.status_code, 422, response.text)
        # 措辞要和「还不能打开」那种区分开：这里注册已经开着了。
        self.assertIn("失去它的前提条件", response.json()["detail"])
        self.assertFalse(self._get_setting("allow_internal_targets"))

    def test_open_and_close_internal_in_one_save(self):
        """一次保存里同时「打开注册 + 关掉内网访问」是允许的（也是推荐做法）。

        闸门判断的是**合并后**的状态。判断 updates 单独一项的话，
        管理员就只能先存一次再存一次，而中间那一下他并不知道能不能存。
        """
        payload = dict(OPEN_BASELINE)
        payload["self_register_enabled"] = True
        response = self._put_settings(payload)
        self.assertEqual(response.status_code, 200, response.text)
        self.assertTrue(self._get_setting("self_register_enabled"))
        self.assertFalse(self._get_setting("allow_internal_targets"))

    def test_closing_register_is_always_allowed(self):
        """「把注册关掉」这条出路必须永远畅通，哪怕同一次保存里放开内网。"""
        payload = dict(OPEN_BASELINE)
        payload["self_register_enabled"] = True
        self.assertEqual(self._put_settings(payload).status_code, 200)

        response = self._put_settings(
            {"self_register_enabled": False, "allow_internal_targets": True})
        self.assertEqual(response.status_code, 200, response.text)
        self.assertFalse(self._get_setting("self_register_enabled"))
        self.assertTrue(self._get_setting("allow_internal_targets"))

    def test_legacy_dangerous_combo_does_not_lock_admin_out(self):
        """库里已经处在危险组合里时，改**别的**设置项不能被拦。

        这是防死锁：老库遗留、或者有人直接改了数据库，闸门如果每次保存都查，
        管理员改任何一项都存不了，连「把注册关掉」这条出路也一起堵死。
        """
        self._set(self_register_enabled=True, allow_internal_targets=True)
        response = self._put_settings({"default_n": 2})
        self.assertEqual(response.status_code, 200, response.text)
        # 但一动到闸门那四项之一，立刻要拦
        self.assertEqual(self._put_settings({"files_signed_only": True}).status_code, 422)

    def test_files_signed_only_is_a_precondition(self):
        """图片必须带凭证才能访问——谁都能注册 = 谁都能进来翻别人的图。"""
        payload = dict(OPEN_BASELINE)
        payload["files_signed_only"] = False
        payload["self_register_enabled"] = True
        response = self._put_settings(payload)
        self.assertEqual(response.status_code, 422, response.text)
        self.assertIn("图片必须带凭证", response.json()["detail"])

    def test_public_base_url_must_be_reachable_by_others(self):
        """对外访问地址不能是「服务器自己」，公网主机还必须是 https。"""
        bad_cases = ("", "http://localhost:8787", "http://127.0.0.1:8787",
                     "https://", "http://designkit.example.com")
        for bad in bad_cases:
            payload = dict(OPEN_BASELINE)
            payload["public_base_url"] = bad
            payload["self_register_enabled"] = True
            response = self._put_settings(payload)
            self.assertEqual(response.status_code, 422, "%r 应该被拦下" % bad)
            self.assertIn("对外访问地址", response.json()["detail"])
            self.assertFalse(self._get_setting("self_register_enabled"))

    def test_lan_deployment_may_stay_http(self):
        """内网地址允许 http——群晖那台临时测试机是合法用法，不该被误伤。"""
        for lan in ("http://192.168.31.235:8787", "http://nas:8787", "http://box.local:8787"):
            payload = dict(OPEN_BASELINE)
            payload["public_base_url"] = lan
            payload["self_register_enabled"] = True
            response = self._put_settings(payload)
            self.assertEqual(response.status_code, 200, "%s 应该放行：%s" % (lan, response.text))

    def test_blocker_message_names_every_failing_item(self):
        """拒绝文案必须逐条点名，管理员照着一条条改就能改完。

        只说「不满足前置条件」的话，他唯一能做的就是来问我们。
        """
        self._set(files_signed_only=False, public_base_url="")
        response = self._put_settings({"self_register_enabled": True})
        detail = response.json()["detail"]
        for marker in ("①", "②", "③"):
            self.assertIn(marker, detail)
        # 多行文本：设置页要用常驻的 inlineAlert 显示，不能用一闪而过的 toast
        self.assertGreaterEqual(len(detail.split("\n")), 4)

    def test_enabled_flag_must_be_a_real_boolean(self):
        """字符串 "true"/"false" 一律 422。

        少了这一条会出大事：Python 里 bool("false") 是 True，
        前端传字符串就等于把注册**静默打开**，而闸门读的也是这个值。
        """
        for value in ("true", "false", 1, 0):
            response = self._put_settings({"self_register_enabled": value})
            self.assertEqual(response.status_code, 422, "%r 应该被拦下" % value)
            self.assertIn("必须是 true 或 false", response.json()["detail"])

    def test_int_ranges(self):
        """几个数字设置项的范围。0 天 = 永不过期，下限特意是 0 而不是 1。"""
        ok_cases = {"self_register_ip_per_hour": 1, "self_register_daily_limit": 10000,
                    "invite_code_default_max_uses": 1000, "invite_code_default_valid_days": 0}
        for key, value in ok_cases.items():
            self.assertEqual(self._put_settings({key: value}).status_code, 200,
                             "%s=%r 应该放行" % (key, value))
        bad_cases = {"self_register_ip_per_hour": 0, "self_register_daily_limit": 10001,
                     "invite_code_default_max_uses": 0, "invite_code_default_valid_days": 366}
        for key, value in bad_cases.items():
            self.assertEqual(self._put_settings({key: value}).status_code, 422,
                             "%s=%r 应该被拦下" % (key, value))


class RuntimeGateTests(Scaffold):
    """注册接口**每一次请求**都重新确认一遍闸门，不是只在打开开关那一刻查一次。

    设置接口那道拦截挡的是「正常路径」。库里如果因为老版本遗留、或者有人直接
    改了数据库而处在危险组合里，那道拦截根本不会被触发——所以注册接口自己
    还要再查一遍。这是兜底，两道都要有。
    """

    def test_register_refuses_when_internal_targets_on(self):
        self._open_register()
        code, _ = self._make_code()
        # 绕过设置接口，把库改成危险组合（模拟老库遗留）
        self._set(allow_internal_targets=True)

        status = self.client.get("/api/web/register").json()
        self.assertFalse(status["enabled"])
        response = self._register(code)
        self.assertEqual(response.status_code, 403)
        self.assertEqual(response.json()["detail"], register_router.CLOSED_MESSAGE)
        self.assertEqual(self._fresh(User, username="zhangsan"), [])

    def test_refusal_does_not_explain_why(self):
        """对外只说「没有开放注册」。

        「因为管理员还没关掉内网访问」这种话，对着公网说等于免费送情报——
        它同时告诉了对方「这台机器的内网访问是开着的」。
        """
        self._open_register()
        code, _ = self._make_code()
        self._set(allow_internal_targets=True)
        for text in (self.client.get("/api/web/register").json()["message"],
                     self._register(code).json()["detail"]):
            self.assertNotIn("内网", text)
            self.assertNotIn("allow_internal", text)

    def test_closed_switch_rejects_everything(self):
        """总开关关着时：GET 说没开放，POST 直接 403，一个号都建不出来。"""
        self._open_register()
        code, _ = self._make_code()
        self._set(self_register_enabled=False)

        status = self.client.get("/api/web/register").json()
        self.assertFalse(status["enabled"])
        self.assertEqual(status["message"], register_router.CLOSED_MESSAGE)
        response = self._register(code)
        self.assertEqual(response.status_code, 403)
        self.assertEqual(self._fresh(User, username="zhangsan"), [])
        # 码也不能被悄悄扣掉
        self.assertEqual(self._fresh(InviteCode)[0].used_count, 0)

    def test_gate_reopens_without_restart(self):
        """把内网关回去之后立刻就能注册——不需要重启（设置是实时读的）。"""
        self._open_register()
        code, _ = self._make_code()
        self._set(allow_internal_targets=True)
        self.assertEqual(self._register(code).status_code, 403)
        self._set(allow_internal_targets=False)
        self.assertEqual(self._register(code).status_code, 200)

    def test_status_endpoint_needs_no_login(self):
        """GET /api/web/register 必须免登录——登录页要靠它决定显不显示注册入口。"""
        self._open_register()
        response = self.client.get("/api/web/register")
        self.assertEqual(response.status_code, 200)
        payload = response.json()
        self.assertTrue(payload["enabled"])
        # 规则文案由后端给，前端不许自己抄一份（抄岔了会出现
        # 「页面写着至少 8 位、填了 8 位却报错要 10 位」）
        self.assertTrue(payload["username_rule"])
        self.assertTrue(payload["password_rule"])
        self.assertEqual(payload["min_password_length"], register_router.MIN_PASSWORD_LENGTH)


# ══════════════════════════════════════════════════════════════════════
#  二、邀请码并发扣减
# ══════════════════════════════════════════════════════════════════════

class InviteConcurrencyTests(Scaffold):
    """一张码的最后一个名额，只能被一个人抢到。

    ⚠ 下面这几项**必须真的开线程**。改成「顺序调两次接口」会永远通过，
    也就永远发现不了「先查够不够、再写回去」这个错误写法——而那正是这组
    测试存在的唯一理由。
    """

    def _race(self, code, count, prefix):
        """count 个线程同时打注册接口，返回状态码列表。

        每个线程自己一个 TestClient：共用一个客户端时，starlette 的传输层
        会成为瓶颈甚至互斥，几个请求实际上是排队进去的，压根没并发上。
        """
        results = []
        lock = threading.Lock()
        barrier = threading.Barrier(count)

        def go(index):
            client = TestClient(self.app)
            try:
                # 所有线程在这里对齐，尽量让扣减那一瞬间真的重叠。
                # 没有这道栅栏时，先起的线程往往已经跑完了，测出来是假并发。
                barrier.wait(timeout=30)
                response = client.post("/api/web/register", json={
                    "invite_code": code,
                    "username": "%s%d" % (prefix, index),
                    "password": GOOD_PASSWORD,
                })
                with lock:
                    results.append(response.status_code)
            finally:
                client.close()

        threads = [threading.Thread(target=go, args=(i,)) for i in range(count)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=120)
        return results

    def test_last_slot_goes_to_exactly_one(self):
        """8 个人同时抢一张一次性码：只能成一个。"""
        self._open_register()
        code, _ = self._make_code(max_uses=1)
        results = self._race(code, 8, "race")

        self.assertEqual(len(results), 8, "有线程没跑完：%r" % (results,))
        self.assertEqual(results.count(200), 1, results)
        # 没抢到的那七个看到的是同一句「邀请码用不了」，不是 500
        self.assertEqual(set(results) - {200}, {422}, results)
        # 三份账本必须互相对得上
        self.assertEqual(self._fresh(InviteCode)[0].used_count, 1)
        self.assertEqual(len(self._fresh(User, role="member")), 1)
        self.assertEqual(len(self._fresh(InviteRedemption)), 1)

    def test_multi_slot_code_hands_out_exactly_its_quota(self):
        """一张能用 3 次的码，10 个人同时抢，正好开出 3 个号——不多也不少。

        「不少」这一半同样要测：把扣减写成悲观锁再失败就整个放弃，
        会表现成「明明还有名额，却谁也注册不上」。
        """
        self._open_register()
        code, _ = self._make_code(max_uses=3)
        results = self._race(code, 10, "many")

        self.assertEqual(results.count(200), 3, results)
        self.assertEqual(self._fresh(InviteCode)[0].used_count, 3)
        self.assertEqual(len(self._fresh(User, role="member")), 3)

    def test_username_clash_refunds_the_slot(self):
        """两个人同时用同一个用户名注册：名额要退回去。

        不退的话，一次重名就白废一张一次性码——用户看到「换个名字再试」，
        换完却发现码已经废了，而他手里根本没有第二张码。
        """
        self._open_register()
        code, _ = self._make_code(max_uses=2)
        results = []
        lock = threading.Lock()
        barrier = threading.Barrier(2)

        def go():
            client = TestClient(self.app)
            try:
                barrier.wait(timeout=30)
                response = client.post("/api/web/register", json={
                    "invite_code": code, "username": "sametwice",
                    "password": GOOD_PASSWORD,
                })
                with lock:
                    results.append(response.status_code)
            finally:
                client.close()

        threads = [threading.Thread(target=go) for _ in range(2)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=120)

        self.assertEqual(results.count(200), 1, results)
        self.assertEqual(results.count(409), 1, results)
        self.assertEqual(len(self._fresh(User, username="sametwice")), 1)
        # 关键：还剩 1 次可用（无论输的那个是在扣减前被拦住、还是扣完又退了回来）
        self.assertEqual(self._fresh(InviteCode)[0].used_count, 1)

    def test_revoked_and_expired_codes_are_dead(self):
        """作废和过期的码扣不动——并发扣减那条 SQL 的四个条件缺一不可。

        尤其是过期那一条写成 `expires_at IS NULL OR expires_at > now`：
        漏掉 IS NULL 那一半，所有**永不过期**的码会全部失效，
        而报错还是「邀请码用不了」，根本查不到这里。
        """
        self._open_register()
        revoked, payload = self._make_code(max_uses=5)
        self.client.post("/api/web/invites/%d/revoke" % payload["items"][0]["id"],
                         headers=self.auth)
        self.assertEqual(self._register(revoked, username="revoked1").status_code, 422)

        expired, payload = self._make_code(max_uses=5)
        session = self.Session()
        try:
            row = session.get(InviteCode, payload["items"][0]["id"])
            row.expires_at = utcnow() - timedelta(days=1)
            session.commit()
        finally:
            session.close()
        self.assertEqual(self._register(expired, username="expired1").status_code, 422)

        # 反面：永不过期的码（expires_at IS NULL）必须还能用
        forever, _ = self._make_code(valid_days=0)
        self.assertEqual(self._register(forever, username="forever1").status_code, 200)

    def test_taken_username_does_not_burn_a_one_shot_code(self):
        """用户名被占（顺序发生，不是并发）之后，那张一次性码必须还能用。

        这是新人最常见的一步：他随手填了 zhang 或者 test，被拒之后换个名字再来。
        名额如果在这一步就没了，他会看着「换个名字再试」，换完却发现码已经废了——
        而他手里根本没有第二张码。
        代码里有两道保险（查重排在扣减之前 + 撞车之后退回名额），
        这一项守的是**结果**，两道保险哪一道被拿掉它都还在。
        """
        self._open_register()
        code, _ = self._make_code(max_uses=1)
        # 先用管理员的用户名占位——这是最容易被随手填中的那一类名字
        occupied = self.admin.username
        self.assertEqual(self._register(code, username=occupied).status_code, 409)
        self.assertEqual(self._fresh(InviteCode)[0].used_count, 0)
        # 换个名字，同一张码照样能用
        self.assertEqual(self._register(code, username="zhangsan").status_code, 200)

    def test_no_slot_is_consumed_on_bad_password(self):
        """填错密码不能吃掉名额——字段校验必须排在扣减前面。

        排反了的话，一个手滑的新人试三次密码就把发给他的那张一次性码用光了。
        """
        self._open_register()
        code, _ = self._make_code(max_uses=1)
        self.assertEqual(self._register(code, password="short1").status_code, 422)
        self.assertEqual(self._fresh(InviteCode)[0].used_count, 0)
        self.assertEqual(self._register(code).status_code, 200)


# ══════════════════════════════════════════════════════════════════════
#  三、限速：跨进程共享 + 客户端 IP 取右边
# ══════════════════════════════════════════════════════════════════════

_CHILD_SCRIPT = '''\
"""子进程：用**另一个进程**去撞登录限速，证明桶是落库共享的而不是进程内的。"""
import json
import os
import sys

sys.path.insert(0, %(root)r)
os.environ.setdefault("DESIGNKIT_DATA_DIR", %(data_dir)r)
os.environ.setdefault("DESIGNKIT_PROVIDER", "mock")

from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine, event
from sqlalchemy.orm import sessionmaker

from backend.app.deps import get_db
from backend.app.routers import auth as auth_router

db_path, attempts, username = sys.argv[1], int(sys.argv[2]), sys.argv[3]
engine = create_engine(
    "sqlite:///" + db_path,
    connect_args={"check_same_thread": False, "timeout": 30},
    future=True,
)


@event.listens_for(engine, "connect")
def _pragma(dbapi_connection, _record):
    cur = dbapi_connection.cursor()
    cur.execute("PRAGMA journal_mode=WAL")
    cur.execute("PRAGMA busy_timeout=30000")
    cur.close()


Session = sessionmaker(bind=engine, autoflush=False, expire_on_commit=False, future=True)
app = FastAPI()
app.include_router(auth_router.router)


def _db():
    session = Session()
    try:
        yield session
    finally:
        session.close()


app.dependency_overrides[get_db] = _db
client = TestClient(app)
codes = []
for _ in range(attempts):
    codes.append(client.post("/api/web/auth/login", json={
        "username": username, "password": "definitely-not-the-password",
    }).status_code)
print(json.dumps(codes))
'''


class RateLimitWiringTests(Scaffold):
    """auth.py 接上 services/ratelimit.py 之后的行为。

    改造前那套是进程内的 `defaultdict(deque)`。公网部署要开多 worker，
    那套在多进程下等于「设置里写 10 次、实际 10 × 进程数」——
    而这件事在单进程的本机测试里**永远复现不出来**。
    """

    def setUp(self):
        super().setUp()
        # 阈值调小，免得每个用例都要做十几次 PBKDF2（一次约 0.1 秒 CPU）
        self._set(ratelimit_login_max_attempts=3,
                  ratelimit_login_window_minutes=15,
                  ratelimit_login_block_minutes=15)
        self.member = User(
            username="rl-" + uuid.uuid4().hex[:8],
            password_hash=hash_password("MemberPass2026"),
            role="member", is_active=True, must_change_password=False, token_version=0,
        )
        self.db.add(self.member)
        self.db.commit()

    def _login(self, password, headers=None):
        return self.client.post(
            "/api/web/auth/login",
            json={"username": self.member.username, "password": password},
            headers=headers or {},
        )

    def test_failures_land_in_the_database(self):
        """失败次数记在 rate_limit_state 表里，不是记在内存里。

        落库才有三件事：重启不丢、多进程共用、过期行有人清理。
        """
        self._login("wrong-one")
        rows = self._fresh(RateLimitState, scope="login")
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0].attempts, 1)
        # key 是「IP|小写用户名」——两段一起用，才能让「一个人反复输错自己的密码」
        # 和「一个 IP 拿一堆用户名撞库」互不影响
        self.assertTrue(rows[0].key.endswith("|" + self.member.username.lower()))

    def test_blocks_and_tells_how_long(self):
        for _ in range(3):
            self.assertEqual(self._login("wrong-one").status_code, 401)
        blocked = self._login("MemberPass2026")   # 密码是对的，照样被拦
        self.assertEqual(blocked.status_code, 429)
        # 必须说清还要等多久，否则用户只会一直点，直到确信系统坏了
        self.assertIn("分钟后再试", blocked.json()["detail"])
        # 标准头，给脚本和反代看
        self.assertGreater(int(blocked.headers["Retry-After"]), 0)

    def test_success_clears_the_bucket(self):
        for _ in range(2):
            self._login("wrong-one")
        self.assertEqual(self._login("MemberPass2026").status_code, 200)
        self.assertEqual(self._fresh(RateLimitState, scope="login"), [])

    def test_disabled_account_does_not_count_as_a_failure(self):
        """账号已停用不计一次失败：密码是对的，那不是撞库。

        计进去的话，本人连「你的账号已停用」这句话都看不到——会被 429 顶掉，
        然后他去问管理员「我登不上」，管理员看到的也是一个语焉不详的限速。
        """
        session = self.Session()
        try:
            session.get(User, self.member.id).is_active = False
            session.commit()
        finally:
            session.close()
        for _ in range(4):
            response = self._login("MemberPass2026")
            self.assertEqual(response.status_code, 403)
            self.assertEqual(response.json()["detail"], "账号已停用")
        self.assertEqual(self._fresh(RateLimitState, scope="login"), [])

    def test_bucket_is_shared_across_processes(self):
        """**真的另起一个 python 进程**去撞，撞满之后本进程也进不来。

        这是这一组里最要紧的一项：进程内字典的写法在这里会当场变红，
        而在别的任何一项里都是绿的。
        """
        script_path = os.path.join(self.tmpdir, "child_login.py")
        with open(script_path, "w", encoding="utf-8") as handle:
            handle.write(_CHILD_SCRIPT % {
                "root": _ROOT, "data_dir": os.environ["DESIGNKIT_DATA_DIR"]})

        completed = subprocess.run(
            [sys.executable, script_path, self.db_path, "3", self.member.username],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=300,
        )
        self.assertEqual(completed.returncode, 0,
                         completed.stderr.decode("utf-8", "replace"))
        child_codes = json.loads(completed.stdout.decode("utf-8").strip().splitlines()[-1])
        self.assertEqual(child_codes, [401, 401, 401], child_codes)

        # 子进程写的那一行，本进程读得到
        rows = self._fresh(RateLimitState, scope="login")
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0].attempts, 3)
        # 而且本进程认这笔账：密码正确也被拦
        self.assertEqual(self._login("MemberPass2026").status_code, 429)

    def test_forged_left_hand_xff_cannot_dodge_the_limit(self):
        """X-Forwarded-For 要从**右边**数。最左边那一段是客户端自己写的。

        照最左边算，攻击者每次换一个假 IP，限速就等于不存在
        （而且限速表还会被这些假地址撑爆）。
        """
        self._set(trusted_proxy_hops=1)
        real = "203.0.113.7"
        for index in range(3):
            headers = {"X-Forwarded-For": "10.0.0.%d, %s" % (index + 1, real)}
            self.assertEqual(self._login("wrong-one", headers).status_code, 401)
        # 再换一个从没用过的伪造前缀——还是同一个桶，照样被拦
        blocked = self._login("MemberPass2026",
                              {"X-Forwarded-For": "8.8.8.8, " + real})
        self.assertEqual(blocked.status_code, 429)

        rows = self._fresh(RateLimitState, scope="login")
        self.assertEqual(len(rows), 1, "伪造的前缀不该各开一个桶：%r" % (
            [row.key for row in rows],))
        self.assertTrue(rows[0].key.startswith(real + "|"), rows[0].key)

    def test_different_real_ips_get_separate_buckets(self):
        """反面：右边那一段不同 = 真的是不同来源，必须各算各的。

        少了这一条，「全站共用一个桶」也能让上一项通过——那样一个人被锁，
        所有人一起登不进来。
        """
        self._set(trusted_proxy_hops=1)
        for _ in range(3):
            self._login("wrong-one", {"X-Forwarded-For": "203.0.113.7"})
        self.assertEqual(
            self._login("MemberPass2026", {"X-Forwarded-For": "203.0.113.7"}).status_code, 429)
        # 另一个真实来源不受影响
        self.assertEqual(
            self._login("MemberPass2026", {"X-Forwarded-For": "198.51.100.9"}).status_code, 200)

    def test_xff_is_ignored_when_no_proxy_configured(self):
        """trusted_proxy_hops=0（默认，没有反代）时整个头一个字都不看。

        默认值取 0 是刻意的：配多了会去信一段能随便伪造的地址，限速当场作废。
        """
        self.assertEqual(self._get_setting("trusted_proxy_hops"), 0)
        for index in range(3):
            self._login("wrong-one", {"X-Forwarded-For": "10.0.0.%d" % index})
        rows = self._fresh(RateLimitState, scope="login")
        self.assertEqual(len(rows), 1)
        self.assertNotIn("10.0.0.", rows[0].key)

    def test_register_rate_limit_uses_the_right_hand_ip_too(self):
        """注册接口也走同一套：伪造左段换不来新的配额。

        注册这条路比登录更需要限速——它是全系统唯一一条不需要登录的写接口，
        而且每次都要做一次 20 万轮的 PBKDF2。
        """
        self._open_register()
        self._set(self_register_ip_per_hour=2, trusted_proxy_hops=1)
        code, _ = self._make_code(max_uses=10)
        real = "203.0.113.30"
        for index in range(2):
            response = self._register(
                code, username="rr%d" % index,
                headers={"X-Forwarded-For": "10.0.0.%d, %s" % (index, real)})
            self.assertEqual(response.status_code, 200, response.text)
        blocked = self._register(code, username="rr9",
                                 headers={"X-Forwarded-For": "9.9.9.9, " + real})
        self.assertEqual(blocked.status_code, 429)
        self.assertIn("后再试", blocked.json()["detail"])
        self.assertGreater(int(blocked.headers["Retry-After"]), 0)

    def test_failed_registrations_do_not_eat_the_daily_quota(self):
        """全站每日名额只在**成功**时才扣。

        记在前面的话，几十次手滑（密码太短、码打错）就能把当天的名额耗光，
        而那一天真正想注册的人一个都进不来。
        """
        self._open_register()
        self._set(self_register_daily_limit=1)
        code, _ = self._make_code(max_uses=5)
        for index in range(3):
            self.assertEqual(
                self._register(code, username="dq%d" % index, password="short1").status_code, 422)
        self.assertEqual(self._register(code, username="dqok").status_code, 200)
        # 名额到此用完
        response = self._register(code, username="dqlate")
        self.assertEqual(response.status_code, 429)
        self.assertIn("名额", response.json()["detail"])


# ══════════════════════════════════════════════════════════════════════
#  四、注册与开通解耦
# ══════════════════════════════════════════════════════════════════════

class ProvisioningDecoupledTests(Scaffold):
    """网关整个连不上时，注册照常成功、人照常登录进来。

    这是产品底线。坏掉的样子是：网关一停机（升级、断电、换地址、接口全改），
    新人一个都进不来——而他们本来只是暂时不能生图而已。
    """

    def setUp(self):
        super().setUp()
        self._open_register()
        # 一个**保证连不上**的地址：起一台假网关再立刻关掉，端口就空出来了
        #（写法照抄 test_provision_regression.py 的 _dead_port_base_url）。
        # 比随便写一个端口号可靠：随便写的端口有极小概率正好被别的程序占着，
        # 那时测出来的是「连上了但接口不对」，和「网关关机了」根本是两回事。
        # 也比用超时来模拟快得多——连接被拒是毫秒级的，超时要等满 timeout。
        # ⚠ 全程只碰 127.0.0.1 上这台临时假网关，绝不会连用户那台真实的 Sub2API。
        base_url, _state, stop = fake_sub2api.start()
        stop()
        self.dead_gateway = base_url
        self._set(sub2api_auto_provision=True,
                  sub2api_base_url=base_url,
                  sub2api_admin_key="fake-admin-key-for-tests-only",
                  sub2api_group_id="1",
                  sub2api_timeout=3)

    def test_register_succeeds_while_gateway_is_down(self):
        code, _ = self._make_code()
        response = self._register(code, display_name="小王")
        self.assertEqual(response.status_code, 200, response.text)
        payload = response.json()
        self.assertEqual(payload["username"], "zhangsan")
        # 注册接口**故意不发登录令牌**：注册和登录是两件事
        self.assertNotIn("token", payload)

        users = self._fresh(User, username="zhangsan")
        self.assertEqual(len(users), 1)
        user = users[0]
        # 角色写死 member、不强制改密（密码是他自己刚设的）
        self.assertEqual(user.role, "member")
        self.assertTrue(user.is_active)
        self.assertFalse(user.must_change_password)
        self.assertEqual(user.display_name, "小王")

    def test_new_user_can_log_in_while_gateway_is_down(self):
        """能注册但登不进去等于没注册。"""
        code, _ = self._make_code()
        self.assertEqual(self._register(code).status_code, 200)
        response = self.client.post("/api/web/auth/login",
                                    json={"username": "zhangsan", "password": GOOD_PASSWORD})
        self.assertEqual(response.status_code, 200, response.text)
        token = response.json()["token"]
        me = self.client.get("/api/web/auth/me",
                             headers={"Authorization": "Bearer " + token})
        self.assertEqual(me.status_code, 200)
        self.assertEqual(me.json()["role"], "member")
        self.assertFalse(me.json()["must_change_password"])

    def test_gateway_row_is_only_queued_never_awaited(self):
        """网关账号行登记成 pending 就立刻返回，注册这条路上一个网络请求都不发。

        真正去 Sub2API 建号、拿 Key 的那三次请求由后台调度器慢慢做。
        这条界限是结构性的：`http://127.0.0.1:1` 这个连不上的地址如果被
        注册接口碰过一下，这一项就会因为超时而慢得肉眼可见（或者直接失败）。
        """
        code, _ = self._make_code()
        self.assertEqual(self._register(code).status_code, 200)
        accounts = self._fresh(UserGatewayAccount)
        self.assertEqual(len(accounts), 1)
        self.assertEqual(accounts[0].state, "pending")
        # 还没拿到 Key，这是对的——「暂时不能生图」而不是「注册失败」
        self.assertFalse(accounts[0].api_key_enc)

    def test_background_round_fails_without_taking_the_user_down(self):
        """后台真的跑一轮开通、真的失败之后，这个人照样登录得进来。

        上一项证明的是「注册时不发请求」，这一项证明的是「请求发了、失败了，
        影响也只限于生图额度」——网关升级、断电、换地址时就是这个样子。
        """
        code, _ = self._make_code()
        self.assertEqual(self._register(code).status_code, 200)

        session = self.Session()
        try:
            provisioning.provision_due(session, limit=10)
        finally:
            session.close()

        account = self._fresh(UserGatewayAccount)[0]
        # 状态可以是「等着重试」也可以是「要人工处理」，但绝不能是「已开通」——
        # 一把不存在的 Key 被标成 active，表现是每一次生图都失败而界面显示正常。
        self.assertNotEqual(account.state, "active")
        self.assertFalse(account.api_key_enc)
        # 人照样进得来
        response = self.client.post("/api/web/auth/login",
                                    json={"username": "zhangsan", "password": GOOD_PASSWORD})
        self.assertEqual(response.status_code, 200, response.text)

    def test_manual_state_when_auto_provision_is_off(self):
        """自动开通没开时登记成 manual（含义是「Key 的来源是人」），同样不影响注册。"""
        self._set(sub2api_auto_provision=False)
        code, _ = self._make_code()
        self.assertEqual(self._register(code).status_code, 200)
        accounts = self._fresh(UserGatewayAccount)
        self.assertEqual(len(accounts), 1)
        self.assertEqual(accounts[0].state, "manual")

    def test_three_tables_are_written_in_one_transaction(self):
        """User + AuthIdentity + InviteRedemption 要么全成要么全不成。

        只建了 User 没建 AuthIdentity，这个人就成了「不知道从哪冒出来的账号」；
        只建了 User 没记流水，出事时查不出他是用哪张码进来的——
        而那正是 invite_redemptions 存在的唯一理由。
        """
        code, _ = self._make_code(note="给小王")
        self.assertEqual(self._register(code).status_code, 200)
        user = self._fresh(User, username="zhangsan")[0]

        identities = self._fresh(AuthIdentity, user_id=user.id)
        self.assertEqual(len(identities), 1)
        self.assertEqual(identities[0].provider, "password")
        self.assertEqual(identities[0].identifier, "zhangsan")
        # credential 恒为 NULL：密码的唯一真相在 users.password_hash。
        # 两处都存，迟早出现「改了一处、另一处还是老密码」的鬼故事。
        self.assertIsNone(identities[0].credential)

        redemptions = self._fresh(InviteRedemption, user_id=user.id)
        self.assertEqual(len(redemptions), 1)
        self.assertEqual(redemptions[0].code_snapshot, code)
        self.assertEqual(redemptions[0].username_snapshot, "zhangsan")
        self.assertTrue(redemptions[0].client_ip)

    def test_request_body_cannot_grant_admin(self):
        """请求体里写 role=admin 也只能注册出一个 member。

        这是注册接口最要紧的一行：从请求里取 role，等于任何人在注册时加一个
        "role": "admin" 就能拿到管理员权限。
        """
        code, _ = self._make_code()
        response = self.client.post("/api/web/register", json={
            "invite_code": code, "username": "sneaky", "password": GOOD_PASSWORD,
            "role": "admin", "is_active": True, "must_change_password": False,
            "monthly_quota": 999999,
        })
        self.assertEqual(response.status_code, 200, response.text)
        user = self._fresh(User, username="sneaky")[0]
        self.assertEqual(user.role, "member")
        self.assertIsNone(user.monthly_quota)


# ══════════════════════════════════════════════════════════════════════
#  五、报错不许变成探测器
# ══════════════════════════════════════════════════════════════════════

class NoOracleTests(Scaffold):
    """这个接口是全系统唯一一条免登录的写接口，它的每一句报错都会被拿去当判定器。"""

    def setUp(self):
        super().setUp()
        self._open_register()

    def test_all_invite_failures_return_one_identical_response(self):
        """不存在 / 已作废 / 已过期 / 已用完 —— 状态码和正文必须**逐字相同**。

        分开说等于给了攻击者一个「这串字符是不是有效邀请码」的判定器，
        他可以拿着它慢慢枚举，而每一次探测在日志里看起来都只是一次普通的注册失败。
        """
        seen = set()

        # ① 从不存在的码（12 位、字符集也对，形状上和真码没区别）
        response = self._register("ZZZZZZZZZZZZ", username="probe1")
        seen.add((response.status_code, response.json()["detail"]))

        # ② 已用完
        used, _ = self._make_code(max_uses=1)
        self.assertEqual(self._register(used, username="probe2").status_code, 200)
        response = self._register(used, username="probe3")
        seen.add((response.status_code, response.json()["detail"]))

        # ③ 已作废
        revoked, payload = self._make_code()
        self.client.post("/api/web/invites/%d/revoke" % payload["items"][0]["id"],
                         headers=self.auth)
        response = self._register(revoked, username="probe4")
        seen.add((response.status_code, response.json()["detail"]))

        # ④ 已过期
        expired, payload = self._make_code()
        session = self.Session()
        try:
            session.get(InviteCode, payload["items"][0]["id"]).expires_at = (
                utcnow() - timedelta(days=1))
            session.commit()
        finally:
            session.close()
        response = self._register(expired, username="probe5")
        seen.add((response.status_code, response.json()["detail"]))

        self.assertEqual(len(seen), 1, "四种失败必须无法区分，实际是：%r" % (seen,))
        status, detail = seen.pop()
        self.assertEqual(status, 422)
        self.assertEqual(detail, register_router.INVITE_INVALID_MESSAGE)

    def test_used_up_code_is_indistinguishable_from_unknown_code(self):
        """把上一项里最关键的那一对单独再钉一遍（任务点名要的就是这一对）。

        单独成项是有意的：上一项一旦因为别的原因（比如作废的措辞改了）变红，
        很容易被人「顺手」放宽成只比状态码。这一项守住最核心的那一对。
        """
        used, _ = self._make_code(max_uses=1)
        self.assertEqual(self._register(used, username="pair1").status_code, 200)
        left = self._register(used, username="pair2")
        right = self._register("ABCDEFGHJKMN", username="pair3")
        self.assertEqual(left.status_code, right.status_code)
        self.assertEqual(left.json(), right.json())

    def test_taken_username_does_not_say_it_is_taken(self):
        """用户名占用只说「不能用，换一个」。

        说「已存在」，这个接口就成了一份用户名花名册（谁在用这个系统、
        有没有 admin 这个号），而那份名单是撞库的第一步。
        """
        code, _ = self._make_code(max_uses=5)
        self.assertEqual(self._register(code, username="zhangsan").status_code, 200)
        response = self._register(code, username="zhangsan")
        self.assertEqual(response.status_code, 409)
        detail = response.json()["detail"]
        for forbidden in ("已存在", "已被占用", "已注册"):
            self.assertNotIn(forbidden, detail)
        self.assertIn("请换一个", detail)

    def test_username_check_is_case_insensitive(self):
        """ZhangSan 也算占用。

        数据库的唯一索引是区分大小写的，不在这里挡住的话，两个人会各自登进
        不同的账号，而管理员在成员列表里看到的是两行几乎一样的名字。
        """
        code, _ = self._make_code(max_uses=5)
        self.assertEqual(self._register(code, username="zhangsan").status_code, 200)
        self.assertEqual(self._register(code, username="ZhangSan").status_code, 409)

    def test_no_secret_leaks_into_responses(self):
        """注册的响应里不许出现密码、也不许出现邀请码本身。

        响应会被浏览器缓存、被截图发到群里、被前端打进 console。
        """
        code, _ = self._make_code()
        text = self._register(code, display_name="小王").text
        self.assertNotIn(GOOD_PASSWORD, text)
        self.assertNotIn(code, text)

    def test_empty_code_gets_a_different_and_helpful_message(self):
        """「一个字都没填」不是探测器，该说人话就说人话。

        全都糊成同一句的话，忘了填码的人会以为是管理员给的码有问题。
        """
        response = self._register("", username="blank1")
        self.assertEqual(response.status_code, 422)
        self.assertIn("请填写邀请码", response.json()["detail"])


# ══════════════════════════════════════════════════════════════════════
#  六、邀请码管理（管理员侧）
# ══════════════════════════════════════════════════════════════════════

class InviteAdminTests(Scaffold):
    """发码、看码、作废。这三个接口只给管理员——码本身是敏感字段。"""

    def test_requires_admin(self):
        member = User(
            username="mem-" + uuid.uuid4().hex[:8],
            password_hash=hash_password("MemberPass2026"),
            role="member", is_active=True, must_change_password=False, token_version=0,
        )
        self.db.add(member)
        self.db.commit()
        headers = {"Authorization": "Bearer " + create_token(member.id, 0)}
        self.assertEqual(self.client.get("/api/web/invites", headers=headers).status_code, 403)
        self.assertEqual(
            self.client.post("/api/web/invites", json={}, headers=headers).status_code, 403)
        # 完全不带凭证也进不来
        self.assertEqual(self.client.get("/api/web/invites").status_code, 401)

    def test_codes_are_not_enumerable(self):
        """码必须是随机的，不能是自增或时间戳派生。

        可枚举的码等于没有邀请码：拿到一张就能推出别人的。
        """
        _, payload = self._make_code(count=12)
        codes = [item["code"] for item in payload["items"]]
        self.assertEqual(len(set(codes)), 12)
        for code in codes:
            self.assertEqual(len(code), 12)
            # 字符集去掉了 I L O U（免得抄错），码里不该出现它们
            for banned in "ILOU":
                self.assertNotIn(banned, code)

    def test_normalize_code_is_the_single_entry_point(self):
        """用户从聊天记录里复制过来的各种形态都要能用。

        写入和校验用两套规则是这类功能最容易出的错：管理员发出去的码在界面上
        显示得好好的，用户照着打进去却说无效，两边谁也查不出差在哪。
        """
        self._open_register()
        code, payload = self._make_code()
        display = payload["items"][0]["code_display"]
        self.assertEqual(invites_router.normalize_code(display), code)
        # 小写 + 带连字符 + 前后空格：这就是微信里复制过来的样子
        self.assertEqual(self._register("  " + display.lower() + " ").status_code, 200)

    def test_list_shows_who_used_it(self):
        """列表要能回答「这张码被谁、什么时候、从哪个 IP 用掉的」。

        出事时（码被转发到群里）这是唯一能定位到人的东西。
        """
        self._open_register()
        code, _ = self._make_code(note="给小王", max_uses=2)
        self.assertEqual(self._register(code, username="zhangsan").status_code, 200)

        item = self.client.get("/api/web/invites", headers=self.auth).json()["items"][0]
        self.assertEqual(item["code"], code)
        self.assertEqual(item["note"], "给小王")
        self.assertEqual(item["used_count"], 1)
        self.assertEqual(item["remaining"], 1)
        self.assertEqual(item["state"], "active")
        self.assertEqual(item["created_by"], self.admin.display_name)
        self.assertEqual(len(item["redemptions"]), 1)
        self.assertEqual(item["redemptions"][0]["username"], "zhangsan")
        self.assertTrue(item["redemptions"][0]["client_ip"])
        # 时间一律是带 Z 的 UTC ISO 串。少了这个 Z，浏览器会按本地时区解释，
        # 中国时区直接差 8 小时。
        self.assertTrue(item["created_at"].endswith("Z"))

    def test_notice_warns_about_the_dangerous_combo(self):
        """列表页顶部要喊出「注册已开 + 内网还开着」这个危险组合。

        正常路径上设置接口会拦住它，能走到这里说明是老库遗留状态——
        管理员这一页是他唯一可能看见这件事的地方。
        """
        self._make_code()
        # ① 总开关没开：提醒他「别人拿着码也注册不了」
        payload = self.client.get("/api/web/invites", headers=self.auth).json()
        self.assertFalse(payload["self_register_enabled"])
        self.assertIn("总开关", payload["notice"])
        # ② 危险组合
        self._set(self_register_enabled=True, allow_internal_targets=True)
        payload = self.client.get("/api/web/invites", headers=self.auth).json()
        self.assertIn("危险组合", payload["notice"])
        # ③ 永不过期的码也要提醒
        self._set(allow_internal_targets=False)
        self._make_code(valid_days=0)
        payload = self.client.get("/api/web/invites", headers=self.auth).json()
        self.assertIn("永不过期", payload["notice"])

    def test_revoke_is_idempotent_and_explains_the_blast_radius(self):
        """作废是「把还没用掉的名额收回来」，不是「把已开出的号一起停掉」。

        这两件事必须分开，返回的文案也要说清楚——那是管理员判断
        「还要不要去停号」的唯一依据。
        """
        self._open_register()
        code, payload = self._make_code(max_uses=3)
        code_id = payload["items"][0]["id"]
        self.assertEqual(self._register(code, username="zhangsan").status_code, 200)

        first = self.client.post("/api/web/invites/%d/revoke" % code_id, headers=self.auth)
        self.assertEqual(first.status_code, 200, first.text)
        self.assertIn("不会把它们停掉", first.json()["message"])
        self.assertEqual(first.json()["item"]["state"], "revoked")
        # 已经开出来的账号一个都不动
        self.assertTrue(self._fresh(User, username="zhangsan")[0].is_active)
        # 但码不能再用了
        self.assertEqual(self._register(code, username="lisi").status_code, 422)
        # 重复作废不报错（管理员多点一次不该看到红字）
        second = self.client.post("/api/web/invites/%d/revoke" % code_id, headers=self.auth)
        self.assertEqual(second.status_code, 200)
        self.assertEqual(second.json()["item"]["state"], "revoked")
        # 不存在的码是 404，不是 500
        self.assertEqual(
            self.client.post("/api/web/invites/999999/revoke", headers=self.auth).status_code, 404)

    def test_create_validation_speaks_chinese(self):
        """范围错误一律中文 422；数字字段要能接住字符串。

        前端是无构建的原生 JS，从 <input> 里读出来的是字符串 "7" 不是数字 7。
        只认 int 的话，运营在界面上填什么都会得到一个英文 422。
        """
        for body in ({"max_uses": 0}, {"max_uses": 1001}, {"valid_days": 366},
                     {"count": 21}, {"max_uses": "abc"}, {"note": "x" * 200}):
            response = self.client.post("/api/web/invites", json=body, headers=self.auth)
            self.assertEqual(response.status_code, 422, "%r 应该被拦下" % body)
            detail = response.json()["detail"]
            self.assertIsInstance(detail, str, "报错要是一句中文人话，不是 pydantic 的数组")
        # 字符串形式的合法值要能过
        response = self.client.post(
            "/api/web/invites", json={"max_uses": "2", "valid_days": "30", "count": "3"},
            headers=self.auth)
        self.assertEqual(response.status_code, 200, response.text)
        self.assertEqual(len(response.json()["items"]), 3)
        self.assertEqual(response.json()["items"][0]["max_uses"], 2)

    def test_defaults_come_from_settings(self):
        """新建表单的预填值从系统设置里读，前端不许再抄一份。

        抄了的话，管理员改了设置、界面上却还是老数字。
        """
        self._set(invite_code_default_max_uses=5, invite_code_default_valid_days=0)
        payload = self.client.get("/api/web/invites", headers=self.auth).json()
        self.assertEqual(payload["defaults"], {"max_uses": 5, "valid_days": 0})
        _, created = self._make_code()
        self.assertEqual(created["items"][0]["max_uses"], 5)
        self.assertIsNone(created["items"][0]["expires_at"])
        # 永久码要在提示语里说明代价
        self.assertIn("收不回来", created["message"])


# ══════════════════════════════════════════════════════════════════════
#  七、字段校验（自助注册的密码比管理员建号更严，这是有意的）
# ══════════════════════════════════════════════════════════════════════

class FieldValidationTests(Scaffold):
    """自助注册设的密码是要一直用下去的，还是在公网上用——所以规则比 8 位那条严。

    管理员建号时设的初始密码寿命只有几分钟（must_change_password 强制本人立刻改掉），
    而这里没有强制改密这一步。
    """

    def setUp(self):
        super().setUp()
        self._open_register()

    def test_username_rule_text_is_what_the_user_sees(self):
        """报错正文和 GET 接口给前端的规则文案**逐字相同**。

        两处写岔的话，用户会照着页面上的规则填、然后被报错说「不对」，
        谁也说不清该信哪个。
        """
        code, _ = self._make_code(max_uses=9)
        rule = self.client.get("/api/web/register").json()["username_rule"]
        for bad in ("ab", "有中文", "with space", "x" * 33, "bad!name", ""):
            response = self._register(code, username=bad)
            self.assertEqual(response.status_code, 422, "%r 应该被拦下" % bad)
            self.assertEqual(response.json()["detail"], rule)
        self.assertEqual(self._fresh(InviteCode)[0].used_count, 0)

    def test_password_rules(self):
        code, _ = self._make_code(max_uses=9)
        bad_cases = (
            ("Short1", "至少"),                    # 太短
            ("abcdefghijkl", "字母和数字"),         # 纯字母
            ("123456789012", "字母和数字"),         # 纯数字
            ("password123", "太常见"),              # 弱口令名单
            ("a1a1a1a1a1a1", "重复"),               # 只有两种字符
            (" ShangPin2026", "空格"),              # 复制粘贴带进来的空格
            ("zhangsan2026x", "包含用户名"),        # 含用户名
        )
        for password, marker in bad_cases:
            response = self._register(code, password=password)
            self.assertEqual(response.status_code, 422, "%r 应该被拦下" % password)
            self.assertIn(marker, response.json()["detail"])
        self.assertEqual(self._fresh(InviteCode)[0].used_count, 0)

    def test_display_name_length_is_capped_before_the_database(self):
        """显示名列宽是 64，超长在 PostgreSQL 上会直接 500。

        本机 SQLite 照单全收，所以这条不测就永远发现不了——
        直到有个陌生人在注册页上看到一个 500。
        """
        code, _ = self._make_code(max_uses=9)
        response = self._register(code, display_name="名" * 33)
        self.assertEqual(response.status_code, 422)
        self.assertIn("显示名", response.json()["detail"])
        # 控制字符会把成员列表和日志排版搞乱
        self.assertEqual(self._register(code, display_name="小\n王").status_code, 422)
        # 不填就用用户名
        self.assertEqual(self._register(code).status_code, 200)
        self.assertEqual(self._fresh(User, username="zhangsan")[0].display_name, "")

    def test_oversized_fields_do_not_crash_the_endpoint(self):
        """超长输入一律 4xx，不能是 500。

        这条免登录接口面向公网，塞一段 10KB 的正文进来是必然会发生的。
        """
        code, _ = self._make_code(max_uses=9)
        self.assertLess(self._register("X" * 10000, username="huge1").status_code, 500)
        self.assertLess(self._register(code, username="u" * 5000).status_code, 500)
        self.assertLess(self._register(code, password="P1" * 5000).status_code, 500)
        self.assertLess(
            self._register(code, username="huge2", display_name="名" * 5000).status_code, 500)

    def test_rate_limit_key_survives_a_huge_username(self):
        """超长用户名不能把限速表写崩（key 进库前会被截断到 128）。

        不截断的话，PostgreSQL 会直接报错，整个登录接口 500——
        而这是攻击者一行代码就能触发的。
        """
        self.client.post("/api/web/auth/login",
                         json={"username": "n" * 10000, "password": "whatever12"})
        rows = self._fresh(RateLimitState, scope="login")
        self.assertEqual(len(rows), 1)
        self.assertLessEqual(len(rows[0].key), ratelimit.MAX_KEY_LENGTH)


# ══════════════════════════════════════════════════════════════════════
#  八、跨库兼容：这两条 SQL 在 PostgreSQL 上也要能编译
# ══════════════════════════════════════════════════════════════════════

class CrossDatabaseTests(unittest.TestCase):
    """本机是 SQLite、生产是 PostgreSQL，两边都要能跑。

    只跑 SQLite 比不跑更糟——它会让人以为这条路已经有测试守着了，
    而实际上 PostgreSQL 那边启动就崩（test_migrations.py 的文件头讲的是同一件事）。
    """

    def test_new_tables_compile_on_postgresql(self):
        from sqlalchemy.dialects import postgresql
        from sqlalchemy.schema import CreateTable
        for model in (AuthIdentity, InviteCode, InviteRedemption, RateLimitState):
            ddl = str(CreateTable(model.__table__).compile(dialect=postgresql.dialect()))
            self.assertIn(model.__tablename__, ddl)
            # SQLite 对类型名几乎来者不拒，PostgreSQL 不认 DATETIME
            self.assertNotIn("DATETIME", ddl.upper().replace("TIMESTAMP", ""))

    def test_atomic_invite_sql_compiles_on_both(self):
        from sqlalchemy import update as sa_update
        from sqlalchemy.dialects import postgresql, sqlite
        statements = (
            # 抢名额：判断和修改压在同一条 SQL 里
            sa_update(InviteCode)
            .where(
                InviteCode.code == "X",
                InviteCode.revoked_at.is_(None),
                (InviteCode.expires_at.is_(None)) | (InviteCode.expires_at > utcnow()),
                InviteCode.used_count < InviteCode.max_uses,
            )
            .values(used_count=InviteCode.used_count + 1),
            # 退名额：used_count > 0 防负数
            sa_update(InviteCode)
            .where(InviteCode.id == 1, InviteCode.used_count > 0)
            .values(used_count=InviteCode.used_count - 1),
        )
        for statement in statements:
            for dialect in (postgresql.dialect(), sqlite.dialect()):
                self.assertTrue(str(statement.compile(dialect=dialect)))


if __name__ == "__main__":
    unittest.main(verbosity=2)
