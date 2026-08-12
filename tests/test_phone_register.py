"""手机号注册 / 登录：回归测试（不联网、不发短信、不花钱、不碰 data/）。

════════════════════════════════════════════════════════════════════════
 这一组守的是什么
════════════════════════════════════════════════════════════════════════
每一条都对应一种**不会报错、手工点一遍全都正常**的事故：

1. **关着的时候要真的关死**（总开关 + 共用的内网闸门）。
2. **三层限速：超限时一条短信都不许发出去**（不是发了再拦，那时钱已经花了）。
   而且配额必须在**真正调用上游之前**扣掉——否则「让每次调用都超时」就能
   无限次花钱而配额一次都不减。
3. **不许泄露某个手机号注册没注册**：发码接口对已注册/未注册的响应逐字一样。
4. **验证码只存哈希**：拿到数据库也不能反查出码。
5. **试错次数有上限**，用完当场作废（6 位数是猜得完的）。
6. **注册与开通解耦**：网关行只登记成 pending，注册接口不发任何外部请求。
7. **邀请码原子扣减**：一张一次性码不能开出两个号。
"""
import atexit
import os
import shutil
import sys
import tempfile
import unittest
import uuid

_HERE = os.path.dirname(os.path.abspath(__file__))
_ROOT = "/Users/monica/Desktop/designkit"
if _ROOT not in sys.path:
    sys.path.insert(0, _ROOT)

if not os.environ.get("DESIGNKIT_DATA_DIR"):
    _TMP_DATA_DIR = tempfile.mkdtemp(prefix="dk-phone-data-")
    os.environ["DESIGNKIT_DATA_DIR"] = _TMP_DATA_DIR
    atexit.register(shutil.rmtree, _TMP_DATA_DIR, ignore_errors=True)
os.environ.setdefault("DESIGNKIT_PROVIDER", "mock")

from fastapi import FastAPI  # noqa: E402
from fastapi.testclient import TestClient  # noqa: E402
from sqlalchemy import create_engine, event, select  # noqa: E402
from sqlalchemy.orm import sessionmaker  # noqa: E402

from backend.app.deps import get_db  # noqa: E402
from backend.app.models import (  # noqa: E402
    AppSetting, AuthIdentity, Base, InviteCode, InviteRedemption,
    PhoneVerificationCode, User, UserGatewayAccount, utcnow,
)
from backend.app.routers import invites as invites_router  # noqa: E402
from backend.app.routers import phone as phone_router  # noqa: E402
from backend.app.services import provisioning, ratelimit, settings_service  # noqa: E402

GOOD_PASSWORD = "ShangPin2026"
OPEN_BASELINE = {
    "phone_register_enabled": True,
    "allow_internal_targets": False,
    "files_signed_only": True,
    "public_base_url": "https://designkit.example.com",
    "sms_provider": "debug",
}


class Scaffold(unittest.TestCase):
    def setUp(self):
        self.tmpdir = tempfile.mkdtemp(prefix="dk-phone-")
        self.db_path = os.path.join(self.tmpdir, "t.db")
        self.engine = create_engine(
            "sqlite:///" + self.db_path,
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
        self.Session = sessionmaker(
            bind=self.engine, autoflush=False, expire_on_commit=False, future=True
        )
        self.app = FastAPI()
        self.app.include_router(phone_router.router)
        self.app.dependency_overrides[get_db] = self._db_dependency
        self.client = TestClient(self.app)
        self.db = self.Session()
        ratelimit._gc_last_at = float("-inf")
        phone_router._cleanup_last_at = float("-inf")
        provisioning._reset_state_for_tests()

    def tearDown(self):
        self.client.close()
        self.db.close()
        self.engine.dispose()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def _db_dependency(self):
        session = self.Session()
        try:
            yield session
        finally:
            session.close()

    def _set(self, **kwargs):
        """直接写设置表。

        **不走 settings_service.set_many**：那个函数会丢掉 RUNTIME_DEFAULTS 里
        没有的键，而 phone_register_require_invite 今天正好还没加进 config.py
        （见 blockers）。直接写行既能覆盖已有的键，也能模拟「config 加上之后」的样子。
        """
        for key, value in kwargs.items():
            row = self.db.get(AppSetting, key)
            if row is None:
                self.db.add(AppSetting(key=key, value={"v": value}))
            else:
                row.value = {"v": value}
        self.db.commit()

    def _open(self, **extra):
        values = dict(OPEN_BASELINE)
        values.update(extra)
        self._set(**values)

    def _invite(self, max_uses=1):
        # 必须存**归一化之后**的码：normalize_code 会把易混字符纠正掉
        # （I→1、O→0…），存一个没归一化的码会让所有校验都对不上。
        code = invites_router.normalize_code("TEST" + uuid.uuid4().hex[:8])
        self.db.add(InviteCode(
            code=code, max_uses=max_uses, used_count=0, created_by_user_id=None,
            note="test", expires_at=None, revoked_at=None,
        ))
        self.db.commit()
        return code

    def _send(self, phone, scope="register"):
        return self.client.post("/api/web/phone/code", json={"phone": phone, "scope": scope})

    def _send_ok(self, phone, scope="register"):
        resp = self._send(phone, scope)
        self.assertEqual(resp.status_code, 200, resp.text)
        data = resp.json()
        self.assertTrue(data["debug"])
        self.assertTrue(data["code"])
        return data["code"]

    def _clear_cooldown(self, phone):
        """把这个号的冷却桶清掉（测试要连着发好几条，不想真等 60 秒）。

        注意 key 必须过一遍 phone_bucket_key——限速表里存的是**带密钥哈希**，
        不是明文号码（见 PhonePrivacyTests 里的理由）。这里图省事传明文的话，
        清的是一个根本不存在的桶，表现是「第二次发码莫名其妙 429」。
        """
        ratelimit.reset(
            self.db, phone_router.SCOPE_SMS_COOLDOWN, phone_router.phone_bucket_key(phone))


class GateTests(Scaffold):
    def test_closed_by_default(self):
        """总开关默认关：发码和注册都要被挡住，而且不解释为什么。"""
        resp = self._send("13800138000")
        self.assertEqual(resp.status_code, 403)
        self.assertIn("联系管理员", resp.json()["detail"])
        resp = self.client.post("/api/web/phone/register", json={
            "phone": "13800138000", "code": "123456", "invite_code": "X"})
        self.assertEqual(resp.status_code, 403)
        # 一行验证码记录都不许写进去
        self.assertEqual(self.db.query(PhoneVerificationCode).count(), 0)

    def test_internal_targets_gate_shared_with_invite_path(self):
        """共用的那道硬闸门：allow_internal_targets 开着时，手机号这条路也走不通。"""
        self._open(allow_internal_targets=True)
        resp = self._send("13800138000")
        self.assertEqual(resp.status_code, 403)
        self.assertEqual(self.db.query(PhoneVerificationCode).count(), 0)
        # 关掉之后立刻恢复（闸门是每次请求都重算的，不是只在开开关那一刻查）
        self._set(allow_internal_targets=False)
        self.assertEqual(self._send("13800138000").status_code, 200)

    def test_login_code_faucet_closed_when_nobody_uses_phone(self):
        """从没人用手机号注册过时，「发登录验证码」是关死的（否则是个白送短信的水龙头）。"""
        self._open()
        resp = self._send("13800138000", scope="login")
        self.assertEqual(resp.status_code, 403)
        self.assertEqual(self.db.query(PhoneVerificationCode).count(), 0)


class SendCodeTests(Scaffold):
    def setUp(self):
        super().setUp()
        self._open()

    def test_debug_mode_returns_code_and_notice(self):
        """调试模式：把码回给前端，并且带上「没有真的发出去」那句话。"""
        resp = self._send("13800138000")
        self.assertEqual(resp.status_code, 200, resp.text)
        data = resp.json()
        self.assertTrue(data["debug"])
        self.assertRegex(data["code"], r"^\d{6}$")
        self.assertIn("没有真的发送", data["debug_notice"])
        self.assertEqual(data["channel"], "debug")
        # 手机号只回脱敏形式
        self.assertEqual(data["phone"], "138****8000")
        self.assertNotIn("13800138000", resp.text)

    def test_code_is_stored_hashed_only(self):
        """库里绝不能出现验证码明文。"""
        code = self._send_ok("13800138000")
        row = self.db.query(PhoneVerificationCode).one()
        self.assertNotIn(code, row.code_hash)
        self.assertEqual(len(row.code_hash), 64)
        self.assertEqual(row.code_hash, phone_router.code_hash("13800138000", "register", code))
        self.assertEqual(row.phone, "13800138000")
        self.assertEqual(row.channel, "debug")

    def test_identical_response_for_registered_and_unregistered(self):
        """已注册和未注册的号，发码响应必须逐字一样（除了随机的码本身）。"""
        registered = "13800138001"
        self._register(registered)
        self._clear_cooldown(registered)

        unregistered = "13900139002"
        a = self._send(registered)
        b = self._send(unregistered)
        self.assertEqual(a.status_code, b.status_code)

        def _normalize(resp, phone):
            data = resp.json()
            # 这两项**必然**不一样（码是随机的、号是不同的号），把它们抹平之后
            # 剩下的每一个字都必须一模一样——包括 message 的句式。
            code = data.pop("code")
            data["message"] = data["message"].replace(code, "<CODE>")
            self.assertEqual(data.pop("phone"), phone[:3] + "****" + phone[-4:])
            return data

        self.assertEqual(_normalize(a, registered), _normalize(b, unregistered))

    def test_cooldown_blocks_second_send(self):
        """同一个号连着点两次「获取验证码」：第二次一条都不许发出去。"""
        self._send_ok("13800138000")
        resp = self._send("13800138000")
        self.assertEqual(resp.status_code, 429)
        self.assertIn("秒", resp.json()["detail"])
        self.assertIn("Retry-After", resp.headers)
        # 被拦下时**没有**写第二行（也就是没发第二条）
        self.assertEqual(self.db.query(PhoneVerificationCode).count(), 1)

    def test_phone_daily_limit(self):
        self._set(sms_code_phone_daily_limit=2)
        for _ in range(2):
            self._send_ok("13800138000")
            self._clear_cooldown("13800138000")
        resp = self._send("13800138000")
        self.assertEqual(resp.status_code, 429)
        self.assertIn("今天", resp.json()["detail"])
        self.assertEqual(self.db.query(PhoneVerificationCode).count(), 2)

    def test_global_daily_limit_stops_everyone(self):
        """全站每日总量是最后一道兜底：打满之后换个号也发不出去。"""
        self._set(sms_code_global_daily_limit=2)
        self._send_ok("13800138001")
        self._send_ok("13800138002")
        resp = self._send("13800138003")
        self.assertEqual(resp.status_code, 429)
        self.assertEqual(self.db.query(PhoneVerificationCode).count(), 2)

    def test_ip_hourly_limit(self):
        self._set(sms_code_ip_hourly_limit=2)
        self._send_ok("13800138001")
        self._send_ok("13800138002")
        resp = self._send("13800138003")
        self.assertEqual(resp.status_code, 429)
        self.assertEqual(self.db.query(PhoneVerificationCode).count(), 2)

    def test_quota_consumed_even_when_upstream_fails(self):
        """上游失败也要扣配额：超时可能意味着「已经发了、也扣了钱」。"""
        import backend.app.services.sms as sms_module
        original = sms_module.send_code

        def _boom(db, phone, code, out_id="", config=None):
            return sms_module.SmsResult(
                ok=False, debug=False, channel="aliyun", category="network",
                message="验证码发送超时了", admin_hint="假的上游失败")

        sms_module.send_code = _boom
        try:
            resp = self._send("13800138000")
            self.assertEqual(resp.status_code, 502)
        finally:
            sms_module.send_code = original
        # 冷却桶已经被扣掉了 → 立刻再点一次会撞冷却
        self.assertEqual(self._send("13800138000").status_code, 429)
        # 那条没送出去的码已经被作废
        row = self.db.query(PhoneVerificationCode).one()
        self.assertIsNotNone(row.consumed_at)

    def test_bad_phone_rejected_before_anything(self):
        for bad in ("", "12345", "23800138000", "1380013800a"):
            resp = self._send(bad)
            self.assertEqual(resp.status_code, 422, bad)
        self.assertEqual(self.db.query(PhoneVerificationCode).count(), 0)

    def test_phone_is_normalized(self):
        """+86 / 空格 / 连字符都是同一个号。"""
        self._send_ok("+86 138 0013-8000")
        row = self.db.query(PhoneVerificationCode).one()
        self.assertEqual(row.phone, "13800138000")

    # ── 给别的用例用的注册小工具 ──
    def _register(self, phone):
        code = self._send_ok(phone)
        resp = self.client.post("/api/web/phone/register", json={
            "phone": phone, "code": code, "invite_code": self._invite()})
        self.assertEqual(resp.status_code, 200, resp.text)
        return resp.json()


class RegisterTests(Scaffold):
    def setUp(self):
        super().setUp()
        self._open()

    def _register(self, phone, **extra):
        code = self._send_ok(phone)
        body = {"phone": phone, "code": code, "invite_code": self._invite()}
        body.update(extra)
        return self.client.post("/api/web/phone/register", json=body)

    def test_happy_path(self):
        resp = self._register("13800138000", display_name="小王")
        self.assertEqual(resp.status_code, 200, resp.text)
        data = resp.json()
        # 注册完直接给令牌（不让他为了登录再花一条短信）
        self.assertTrue(data["token"])
        self.assertEqual(data["user"]["role"], "member")
        user = self.db.query(User).filter(User.username == data["username"]).one()
        # 用户名里不许含手机号的任何一位
        self.assertNotIn("13800138000", user.username)
        self.assertNotIn("8000", user.username)
        self.assertEqual(user.display_name, "小王")
        # 手机号身份建好了，而且是归一化后的形式
        identity = self.db.query(AuthIdentity).filter(
            AuthIdentity.provider == "phone").one()
        self.assertEqual(identity.identifier, "13800138000")
        self.assertEqual(identity.user_id, user.id)
        self.assertIsNotNone(identity.verified_at)
        # 没设密码 → password_hash 是空串，而且没有 password 身份行
        self.assertEqual(user.password_hash, "")
        self.assertEqual(self.db.query(AuthIdentity).filter(
            AuthIdentity.provider == "password").count(), 0)

    def test_registration_does_not_wait_for_provisioning(self):
        """注册链路与开通链路解耦：只登记一行 pending/manual，不发任何外部请求。"""
        resp = self._register("13800138000")
        self.assertEqual(resp.status_code, 200)
        user = self.db.query(User).one()
        account = self.db.query(UserGatewayAccount).filter(
            UserGatewayAccount.user_id == user.id).one()
        self.assertIn(account.state, ("pending", "manual"))

    def test_wrong_code_rejected_and_attempts_capped(self):
        self._set(sms_code_max_attempts=3)
        phone = "13800138000"
        real = self._send_ok(phone)
        wrong = "000000" if real != "000000" else "111111"
        invite = self._invite()
        for _ in range(3):
            resp = self.client.post("/api/web/phone/register", json={
                "phone": phone, "code": wrong, "invite_code": invite})
            self.assertEqual(resp.status_code, 422)
            self.assertIn("验证码不正确或已过期", resp.json()["detail"])
        # 次数用完 → 这条码当场作废，连真的码也不认了
        resp = self.client.post("/api/web/phone/register", json={
            "phone": phone, "code": real, "invite_code": invite})
        self.assertEqual(resp.status_code, 422)
        self.assertEqual(self.db.query(User).count(), 0)
        # 邀请码一次都没被扣
        self.assertEqual(self.db.query(InviteCode).one().used_count, 0)

    def test_code_is_single_use(self):
        phone = "13800138000"
        code = self._send_ok(phone)
        resp = self.client.post("/api/web/phone/register", json={
            "phone": phone, "code": code, "invite_code": self._invite()})
        self.assertEqual(resp.status_code, 200)
        resp = self.client.post("/api/web/phone/register", json={
            "phone": "13900139000", "code": code, "invite_code": self._invite()})
        self.assertEqual(resp.status_code, 422)

    def test_code_bound_to_scope(self):
        """注册用的码不能拿去当登录用的码。"""
        phone = "13800138000"
        self._register(phone)
        self._clear_cooldown(phone)
        code = self._send_ok(phone, scope="register")
        resp = self.client.post("/api/web/phone/login", json={"phone": phone, "code": code})
        self.assertEqual(resp.status_code, 401)

    def test_invite_required_by_default(self):
        phone = "13800138000"
        code = self._send_ok(phone)
        resp = self.client.post("/api/web/phone/register", json={"phone": phone, "code": code})
        self.assertEqual(resp.status_code, 422)
        self.assertIn("邀请码", resp.json()["detail"])

    def test_invite_can_be_turned_off(self):
        self._set(phone_register_require_invite=False)
        phone = "13800138000"
        code = self._send_ok(phone)
        resp = self.client.post("/api/web/phone/register", json={"phone": phone, "code": code})
        self.assertEqual(resp.status_code, 200, resp.text)
        # 没用邀请码就不该有兑换流水
        self.assertEqual(self.db.query(InviteRedemption).count(), 0)

    def test_invite_consumed_once(self):
        invite = self._invite(max_uses=1)
        phone = "13800138000"
        code = self._send_ok(phone)
        resp = self.client.post("/api/web/phone/register", json={
            "phone": phone, "code": code, "invite_code": invite})
        self.assertEqual(resp.status_code, 200)
        self.assertEqual(self.db.query(InviteCode).filter(
            InviteCode.code == invite).one().used_count, 1)
        self.assertEqual(self.db.query(InviteRedemption).count(), 1)
        # 用完了就换不到第二个号
        self._clear_cooldown("13900139000")
        code2 = self._send_ok("13900139000")
        resp = self.client.post("/api/web/phone/register", json={
            "phone": "13900139000", "code": code2, "invite_code": invite})
        self.assertEqual(resp.status_code, 422)

    def test_duplicate_phone_told_to_login(self):
        """验过码之后才会走到这一句——对能收到这个号短信的人来说，这不是秘密。"""
        phone = "13800138000"
        self.assertEqual(self._register(phone).status_code, 200)
        self._clear_cooldown(phone)
        resp = self._register(phone)
        self.assertEqual(resp.status_code, 409)
        self.assertIn("已经注册过", resp.json()["detail"])
        self.assertEqual(self.db.query(User).count(), 1)
        # 邀请码名额不能被这次失败吃掉
        codes = self.db.query(InviteCode).order_by(InviteCode.id.desc()).all()
        self.assertEqual(codes[0].used_count, 0)

    def test_weak_password_rejected(self):
        phone = "13800138000"
        code = self._send_ok(phone)
        resp = self.client.post("/api/web/phone/register", json={
            "phone": phone, "code": code, "invite_code": self._invite(), "password": "123"})
        self.assertEqual(resp.status_code, 422)
        self.assertIn("密码", resp.json()["detail"])

    def test_password_cannot_contain_phone(self):
        phone = "13800138000"
        code = self._send_ok(phone)
        resp = self.client.post("/api/web/phone/register", json={
            "phone": phone, "code": code, "invite_code": self._invite(),
            "password": "ab13800138000"})
        self.assertEqual(resp.status_code, 422)
        self.assertIn("手机号", resp.json()["detail"])

    def test_role_cannot_be_forged(self):
        phone = "13800138000"
        code = self._send_ok(phone)
        resp = self.client.post("/api/web/phone/register", json={
            "phone": phone, "code": code, "invite_code": self._invite(),
            "role": "admin", "is_active": True})
        self.assertEqual(resp.status_code, 200)
        self.assertEqual(self.db.query(User).one().role, "member")


class LoginTests(Scaffold):
    def setUp(self):
        super().setUp()
        self._open()
        self.phone = "13800138000"
        code = self._send_ok(self.phone)
        resp = self.client.post("/api/web/phone/register", json={
            "phone": self.phone, "code": code, "invite_code": self._invite(),
            "password": GOOD_PASSWORD})
        self.assertEqual(resp.status_code, 200, resp.text)
        self.registered = resp.json()
        self._clear_cooldown(self.phone)

    def test_login_with_code(self):
        code = self._send_ok(self.phone, scope="login")
        resp = self.client.post("/api/web/phone/login", json={"phone": self.phone, "code": code})
        self.assertEqual(resp.status_code, 200, resp.text)
        self.assertTrue(resp.json()["token"])
        identity = self.db.query(AuthIdentity).filter(
            AuthIdentity.provider == "phone").one()
        self.db.refresh(identity)
        self.assertIsNotNone(identity.last_login_at)

    def test_login_with_password(self):
        resp = self.client.post("/api/web/phone/login", json={
            "phone": self.phone, "password": GOOD_PASSWORD})
        self.assertEqual(resp.status_code, 200, resp.text)
        self.assertTrue(resp.json()["token"])

    def test_wrong_password_and_unknown_phone_look_the_same(self):
        a = self.client.post("/api/web/phone/login", json={
            "phone": self.phone, "password": "WrongPass2026"})
        b = self.client.post("/api/web/phone/login", json={
            "phone": "13900139999", "password": "WrongPass2026"})
        self.assertEqual(a.status_code, b.status_code)
        self.assertEqual(a.json(), b.json())

    def test_wrong_code_and_unknown_phone_look_the_same(self):
        self._send_ok(self.phone, scope="login")
        a = self.client.post("/api/web/phone/login", json={
            "phone": self.phone, "code": "000000"})
        self._clear_cooldown("13900139999")
        self._send_ok("13900139999", scope="login")
        b = self.client.post("/api/web/phone/login", json={
            "phone": "13900139999", "code": "000000"})
        self.assertEqual(a.status_code, b.status_code)
        self.assertEqual(a.json(), b.json())

    def test_disabled_account(self):
        user = self.db.query(User).one()
        user.is_active = False
        self.db.commit()
        resp = self.client.post("/api/web/phone/login", json={
            "phone": self.phone, "password": GOOD_PASSWORD})
        self.assertEqual(resp.status_code, 403)
        self.assertIn("停用", resp.json()["detail"])

    def test_login_still_works_after_registration_closed(self):
        """管理员拉完人就关掉注册，已经注册的人当然还要能登录。"""
        self._set(phone_register_enabled=False)
        code = self._send_ok(self.phone, scope="login")
        resp = self.client.post("/api/web/phone/login", json={"phone": self.phone, "code": code})
        self.assertEqual(resp.status_code, 200, resp.text)

    def test_login_rate_limited(self):
        self._set(ratelimit_login_max_attempts=3)
        for _ in range(3):
            resp = self.client.post("/api/web/phone/login", json={
                "phone": self.phone, "password": "WrongPass2026"})
            self.assertEqual(resp.status_code, 401)
        resp = self.client.post("/api/web/phone/login", json={
            "phone": self.phone, "password": GOOD_PASSWORD})
        self.assertEqual(resp.status_code, 429)


class StatusTests(Scaffold):
    def test_status_does_not_explain_why_closed(self):
        resp = self.client.get("/api/web/phone/status")
        self.assertEqual(resp.status_code, 200)
        data = resp.json()
        self.assertFalse(data["enabled"])
        self.assertTrue(data["require_invite"])  # 默认要求邀请码
        self.assertTrue(data["debug_mode"])
        self.assertIn("没有真的发送", data["debug_notice"])
        self.assertNotIn("allow_internal_targets", resp.text)
        self.assertNotIn("phone_register_enabled", resp.text)

    def test_status_when_open(self):
        self._open()
        data = self.client.get("/api/web/phone/status").json()
        self.assertTrue(data["enabled"])
        self.assertEqual(data["code_ttl_seconds"], 300)
        self.assertEqual(data["resend_after_seconds"], 60)


class CleanupTests(Scaffold):
    def test_expired_rows_are_deleted(self):
        self._open()
        self._send_ok("13800138000")
        row = self.db.query(PhoneVerificationCode).one()
        row.expires_at = utcnow() - __import__("datetime").timedelta(days=3)
        self.db.commit()
        self.assertEqual(phone_router.cleanup_expired(self.db), 1)
        self.assertEqual(self.db.query(PhoneVerificationCode).count(), 0)


if __name__ == "__main__":
    unittest.main(verbosity=2)


class PhonePrivacyTests(unittest.TestCase):
    """手机号绝不能以明文进限速表或日志。

    这是实测发现的问题，不是洁癖：
    - rate_limit_state 是**长期留存**的表，而且会为「每一个曾经请求过验证码的号码」
      留一行——包括打错的、试探的、发完再没回来的。也就是说它会慢慢攒出一份
      「所有来过的手机号」名单，而这些人绝大多数从没注册成功，
      系统本来就没有任何理由保存他们的号码。
    - services/ratelimit.py 会把 key 打进日志。日志是会被打包发给别人排错的，
      等于把这份名单也发了出去。（那边现在也做了脱敏，是第二道保险。）
    """

    def test_bucket_key_is_not_the_raw_phone(self):
        from backend.app.routers import phone as phone_router
        key = phone_router.phone_bucket_key("13800138000")
        self.assertNotIn("13800138000", key)
        self.assertNotIn("8000", key)

    def test_bucket_key_is_stable_and_distinct(self):
        """同号必须算出同一个桶（否则限速形同虚设），不同号必须分开。"""
        from backend.app.routers import phone as phone_router
        self.assertEqual(
            phone_router.phone_bucket_key("13800138000"),
            phone_router.phone_bucket_key("13800138000"))
        self.assertNotEqual(
            phone_router.phone_bucket_key("13800138000"),
            phone_router.phone_bucket_key("13800138001"))

    def test_bucket_key_is_not_a_bare_sha256(self):
        """必须带密钥。中国大陆手机号不到百亿种，裸 sha256 几分钟就能反查回明文。"""
        import hashlib
        from backend.app.routers import phone as phone_router
        bare = hashlib.sha256(b"13800138000").hexdigest()
        self.assertNotIn(phone_router.phone_bucket_key("13800138000"), bare)

    def test_ratelimit_masks_long_digit_runs_in_logs(self):
        """第二道保险：即使有人把明文号码传进来，日志里也不该出现完整号码。"""
        from backend.app.services import ratelimit
        self.assertEqual(ratelimit.mask_key("13800138000"), "138****8000")
        # IP 不该被误伤——排错要靠它
        self.assertEqual(ratelimit.mask_key("192.168.31.235"), "192.168.31.235")
        self.assertEqual(ratelimit.mask_key("127.0.0.1|zhangsan"), "127.0.0.1|zhangsan")
