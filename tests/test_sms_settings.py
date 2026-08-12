"""短信设置与「试发一条」：回归测试（不联网、不发真短信、不花钱、不碰 data/）。

════════════════════════════════════════════════════════════════════════
 这一组守的是什么
════════════════════════════════════════════════════════════════════════
短信和这个系统里别的外部调用不是一回事：**请求一发出去就扣钱**，而且被刷狠了
阿里云会以「疑似恶意」为由把签名封掉——那时候不是花钱的问题，是整条手机号
注册通道停摆，重新申请签名要按工作日算。所以下面五条线，每一条都对应一种
**不会报错、手工点一遍全都正常**的事故：

1. **超限时一条都不许发出去（不是发了再拦）**
   四层限速：同一手机号的冷却、同一手机号每天、同一 IP 每小时、全站每天。
   顺序必须是「四层全部只读检查通过 → 再一起扣额度 → 最后才发」。
   坏掉的样子：额度对不上账（第四层拦下来时前三层已经白扣了），
   或者更贵的一种——两次点击同时通过检查，发出去两条。

2. **AccessKey Secret 必须加密落库、必须打码回吐、原样回传视为「没改」**
   坏掉的样子：管理员保存一次别的设置，Secret 就被一串星号覆盖掉了，
   而页面提示保存成功，直到下一个人点「获取验证码」才发现发不出去。

3. **最常犯的几种手滑要在保存这一刻就被拦住**
   把输入框里那句灰色提示文字粘进来（这个坑在「Sub2API 管理员 Key」那一栏
   真实发生过一次）、复制时多带了一行、把模板正文当成模板 CODE 填进来。
   坏掉的样子：保存成功、界面正常，然后每个人点「获取验证码」都失败，
   失败原因是一串英文错误码。

4. **闸门是双向的，而且两条注册路共用**
   邀请码注册和手机号注册并列，只要有一条开着，那三条前提就必须成立；
   手机号那条还多一条自己的（短信通道得能用）。
   坏掉的样子：管理员把邀请码注册关掉、手机号注册打开，就绕过了整道闸门。

5. **调试模式必须在界面上看得出来**
   不标的话，哪天忘了切回真实模式，管理员会一直以为短信发出去了，
   而用户那边永远收不到——双方都查不出问题出在哪，这种故障没有人能自查。

════════════════════════════════════════════════════════════════════════
 绝不调用真实的阿里云接口
════════════════════════════════════════════════════════════════════════
调一次就是花用户的钱。「试发」那几组一律把设置里的 `sms_aliyun_endpoint`
指到本机回环上一个假上游（services/sms.py 留这个设置项就是为了这件事），
由它按用例的需要返回 `Code: OK` 或者某个错误码。
测试数据里的 AccessKey 全是编出来的字符串，与任何真实凭据无关。
"""
import atexit
import json
import os
import shutil
import sys
import tempfile
import threading
import unittest
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

_HERE = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.dirname(_HERE)
if _ROOT not in sys.path:
    sys.path.insert(0, _ROOT)

# 导入 backend.app.config 会在 import 期间就去建数据目录、改权限。测试绝不该碰
# 用户放着网关 Key 和生产数据的那个 data/，所以在导入之前先把它指到临时位置。
if not os.environ.get("DESIGNKIT_DATA_DIR"):
    _TMP_DATA_DIR = tempfile.mkdtemp(prefix="dk-sms-data-")
    os.environ["DESIGNKIT_DATA_DIR"] = _TMP_DATA_DIR
    atexit.register(shutil.rmtree, _TMP_DATA_DIR, ignore_errors=True)
os.environ.setdefault("DESIGNKIT_PROVIDER", "mock")

from fastapi import FastAPI  # noqa: E402
from fastapi.testclient import TestClient  # noqa: E402
from sqlalchemy import create_engine, func, select  # noqa: E402
from sqlalchemy.orm import sessionmaker  # noqa: E402

from backend.app.deps import get_db  # noqa: E402
from backend.app.models import AppSetting, Base, RateLimitState, User  # noqa: E402
from backend.app.routers import settings_router  # noqa: E402
from backend.app.security import create_token, hash_password  # noqa: E402
from backend.app.services import ratelimit, settings_service, sms  # noqa: E402

# 编出来的假凭据。**这里绝不能出现任何真实的 AccessKey**（见 services/sms.py
# 文件头第五节）：形状对得上就够了，签名算得对不对由 test_sms_signing.py 守着。
FAKE_KEY_ID = "LTAItestonly000000000001"
FAKE_KEY_SECRET = "testonlysecret0000000000000001"
FAKE_SIGN = "测试电商"
FAKE_TEMPLATE = "SMS_123456789"

# 开放注册需要的那一套合法配置（三条前提，缺一不可）。
OPEN_BASELINE = {
    "allow_internal_targets": False,
    "files_signed_only": True,
    "public_base_url": "https://designkit.example.com",
}


# ══════════════════════════════════════════════════════════════════════
#  假上游：冒充 dysmsapi.aliyuncs.com
# ══════════════════════════════════════════════════════════════════════

class _FakeAliyunHandler(BaseHTTPRequestHandler):
    def do_POST(self):  # noqa: N802 —— BaseHTTPRequestHandler 的命名规矩
        body = json.dumps(self.server.next_response).encode("utf-8")
        self.server.calls.append(self.path)
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass  # 别把 HTTP 日志刷进测试输出


class FakeAliyun(object):
    """本机回环上的假阿里云。返回什么由用例现场决定。"""

    def __init__(self):
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _FakeAliyunHandler)
        self.server.next_response = {"Code": "OK", "RequestId": "req-1", "BizId": "biz-1"}
        self.server.calls = []
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @property
    def endpoint(self):
        # http:// + 回环地址是 sms._split_endpoint 明确允许的形态（其余一律要 https）
        return "http://127.0.0.1:%d" % self.server.server_address[1]

    @property
    def calls(self):
        return self.server.calls

    def reply(self, payload):
        self.server.next_response = payload

    def close(self):
        self.server.shutdown()
        self.server.server_close()


class Scaffold(unittest.TestCase):
    """一个独立的库 + 装了设置路由的 app + 一个管理员 + 一个假上游。

    每个用例自己起一个库：这一组要反复改**全局设置**，而 app_settings 是全库
    一行的，共用库会让同一次 discover 里排在后面的用例跑在别人改过的环境里。
    """

    def setUp(self):
        self.tmpdir = tempfile.mkdtemp(prefix="dk-sms-")
        self.engine = create_engine(
            "sqlite:///" + os.path.join(self.tmpdir, "t.db"),
            connect_args={"check_same_thread": False, "timeout": 30}, future=True,
        )
        Base.metadata.create_all(self.engine)
        self.Session = sessionmaker(
            bind=self.engine, autoflush=False, expire_on_commit=False, future=True)

        self.app = FastAPI()
        self.app.include_router(settings_router.router)
        self.app.dependency_overrides[get_db] = self._db_dependency
        self.client = TestClient(self.app)

        self.db = self.Session()
        self.admin = User(
            username="sms-admin-" + uuid.uuid4().hex[:8],
            password_hash=hash_password("bosspass8888"),
            display_name="管理员", role="admin", is_active=True,
            must_change_password=False, token_version=0,
        )
        self.db.add(self.admin)
        self.db.commit()
        self.auth = {"Authorization": "Bearer " + create_token(self.admin.id, 0)}

        self.fake = FakeAliyun()

        # 进程级状态在用例之间必须归零，否则上一个用例的残留会让下一个用例
        # 莫名其妙地被挡住（而且换个执行顺序就好了，最难查的那种）。
        ratelimit._gc_last_at = float("-inf")
        settings_router._sms_test_clear()
        settings_router._sms_test_inflight = False

    def tearDown(self):
        self.fake.close()
        self.client.close()
        self.db.close()
        self.engine.dispose()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    # ── 小工具 ──

    def _db_dependency(self):
        session = self.Session()
        try:
            yield session
        finally:
            session.close()

    def _put(self, **updates):
        return self.client.put("/api/web/settings", json=updates, headers=self.auth)

    def _put_ok(self, **updates):
        resp = self._put(**updates)
        self.assertEqual(resp.status_code, 200, resp.text)
        return resp.json()

    def _put_fail(self, **updates):
        resp = self._put(**updates)
        self.assertEqual(resp.status_code, 422, resp.text)
        return resp.json()["detail"]

    def _get(self):
        resp = self.client.get("/api/web/settings", headers=self.auth)
        self.assertEqual(resp.status_code, 200, resp.text)
        return resp.json()

    def _set_raw(self, **kwargs):
        """直接写设置表，**绕过设置接口的闸门**（用来造「老库遗留的危险组合」）。"""
        session = self.Session()
        try:
            settings_service.set_many(session, kwargs)
        finally:
            session.close()

    def _stored(self, key):
        session = self.Session()
        try:
            row = session.get(AppSetting, key)
            return row.value["v"] if row is not None else None
        finally:
            session.close()

    def _ratelimit_rows(self):
        session = self.Session()
        try:
            return {
                scope: int(count)
                for scope, count in session.execute(
                    select(RateLimitState.scope, func.count()).group_by(RateLimitState.scope)
                ).all()
            }
        finally:
            session.close()

    def _configure_aliyun(self, provider="debug"):
        """把阿里云那四项填好、把接口指向假上游。"""
        return self._put_ok(
            sms_provider=provider,
            sms_aliyun_access_key_id=FAKE_KEY_ID,
            sms_aliyun_access_key_secret=FAKE_KEY_SECRET,
            sms_aliyun_sign_name=FAKE_SIGN,
            sms_aliyun_template_code=FAKE_TEMPLATE,
            sms_aliyun_endpoint=self.fake.endpoint,
        )

    def _test_sms(self, phone="13800138000"):
        return self.client.post("/api/web/settings/test_sms", json={"phone": phone},
                                headers=self.auth)


# ══════════════════════════════════════════════════════════════════════
#  一、字段校验：专挑真实发生过的几种手滑
# ══════════════════════════════════════════════════════════════════════

class ValidationTests(Scaffold):

    def test_access_key_id_rejects_pasted_hint_text(self):
        """把输入框里那句灰色提示文字整段粘了进来 —— 这个坑真实发生过一次。

        报错必须**点名**这种可能。只说「格式不对」的话，管理员会盯着那串
        看起来「明明有字」的内容百思不解。
        """
        detail = self._put_fail(sms_aliyun_access_key_id="请粘贴阿里云控制台生成的 AccessKey ID")
        self.assertIn("提示文字", detail)
        self.assertIn("LTAI", detail)   # 顺手告诉他该长什么样

    def test_access_key_secret_rejects_pasted_hint_text(self):
        detail = self._put_fail(sms_aliyun_access_key_secret="在这里填写 AccessKey Secret")
        self.assertIn("提示文字", detail)

    def test_access_key_rejects_extra_copied_line(self):
        """复制的时候多带了一行（或者把前面那句说明也选进去了）。"""
        detail = self._put_fail(sms_aliyun_access_key_id="AccessKey ID\n" + FAKE_KEY_ID)
        self.assertIn("多带了一行", detail)

    def test_access_key_trailing_newline_is_trimmed_not_rejected(self):
        """两头的空白要**自动去掉**，不能报错：从网页上复制一串 Key 常带一个换行。"""
        self._put_ok(sms_aliyun_access_key_id="  " + FAKE_KEY_ID + "\n")
        self.assertEqual(self._stored("sms_aliyun_access_key_id"), FAKE_KEY_ID)

    def test_access_key_id_rejects_non_alnum(self):
        detail = self._put_fail(sms_aliyun_access_key_id="LTAI-5t/xxxx=")
        self.assertIn("字母和数字", detail)

    def test_secret_equal_to_key_id_is_rejected(self):
        """两栏填成了同一串。控制台里它们上下挨着，复制错的表现是一直签名不匹配。"""
        detail = self._put_fail(
            sms_aliyun_access_key_id=FAKE_KEY_ID,
            sms_aliyun_access_key_secret=FAKE_KEY_ID,
        )
        self.assertIn("同一串", detail)

    def test_sign_name_rejects_brackets(self):
        """签名不带【】：那对方括号是短信平台发送时自动加的。"""
        detail = self._put_fail(sms_aliyun_sign_name="【测试电商】")
        self.assertIn("方括号", detail)

    def test_sign_name_rejects_whole_sentence(self):
        """一整句话 = 多半是把提示文字粘进来了。"""
        detail = self._put_fail(sms_aliyun_sign_name="请填写在阿里云控制台审核通过的短信签名")
        self.assertIn("提示文字", detail)

    def test_template_code_rejects_template_body(self):
        """把模板的**正文**当成模板 CODE 填了进来。"""
        detail = self._put_fail(
            sms_aliyun_template_code="您的验证码是 ${code}，5 分钟内有效。")
        self.assertIn("模板 CODE", detail)
        self.assertIn("SMS_", detail)

    def test_template_code_rejects_wrong_shape(self):
        detail = self._put_fail(sms_aliyun_template_code="123456789")
        self.assertIn("SMS_", detail)

    def test_template_code_is_uppercased(self):
        """小写 sms_ 开头的照收，规整成大写——这只是复制时的大小写差异，不该拦人。"""
        self._put_ok(sms_aliyun_template_code="sms_123456789")
        self.assertEqual(self._stored("sms_aliyun_template_code"), "SMS_123456789")

    def test_endpoint_rejects_plain_http_to_public_host(self):
        """明文 http 只允许指向本机回环，否则 AccessKey 和验证码会明文过网络。"""
        detail = self._put_fail(sms_aliyun_endpoint="http://sms.example.com")
        self.assertIn("http", detail)

    def test_endpoint_blank_falls_back_to_default(self):
        self._put_ok(sms_aliyun_endpoint="   ")
        self.assertEqual(self._stored("sms_aliyun_endpoint"), sms.DEFAULT_ENDPOINT)

    def test_provider_rejects_unknown_value(self):
        detail = self._put_fail(sms_provider="tencent")
        self.assertIn("调试模式", detail)

    def test_rate_limit_thresholds_are_range_checked(self):
        """四个限速阈值是这条通道能不能上线的前提，不是随便填的调优项。"""
        for key, bad in (
            ("sms_code_resend_cooldown_seconds", 10),    # 比阿里云自己的 1 条/分钟还松
            ("sms_code_phone_daily_limit", 500),
            ("sms_code_ip_hourly_limit", 0),
            ("sms_code_global_daily_limit", 100000),     # 手滑多打几个零 = 一天几千块
            ("sms_code_ttl_seconds", 5),
            ("sms_code_max_attempts", 100),
        ):
            with self.subTest(key=key):
                resp = self._put(**{key: bad})
                self.assertEqual(resp.status_code, 422, "%s=%r 竟然存进去了" % (key, bad))

    def test_phone_register_enabled_must_be_real_bool(self):
        """前端传字符串 "false" 时 Python 会当成真值——手机号注册会被**静默打开**。"""
        resp = self._put(phone_register_enabled="false")
        self.assertEqual(resp.status_code, 422)
        self.assertIn("phone_register_enabled", resp.json()["detail"])


# ══════════════════════════════════════════════════════════════════════
#  二、AccessKey Secret：加密落库 + 打码 + 「原样回传 = 没改」
# ══════════════════════════════════════════════════════════════════════

class SecretHandlingTests(Scaffold):

    def test_secret_is_encrypted_at_rest_and_masked_in_responses(self):
        payload = self._put_ok(sms_aliyun_access_key_secret=FAKE_KEY_SECRET)
        stored = self._stored("sms_aliyun_access_key_secret")
        # 库里必须是密文（v1: 前缀是 secrets_box 的约定），不能是明文
        self.assertTrue(stored.startswith("v1:"), stored[:20])
        self.assertNotIn(FAKE_KEY_SECRET, stored)
        # 接口回吐的必须是整串星号（连末 4 位都不给）
        self.assertEqual(set(payload["sms_aliyun_access_key_secret"]), {"*"})
        self.assertEqual(set(self._get()["sms_aliyun_access_key_secret"]), {"*"})
        # 而 AccessKey ID **故意不打码**：管理员要能看出自己填的是哪一把
        self._put_ok(sms_aliyun_access_key_id=FAKE_KEY_ID)
        self.assertEqual(self._get()["sms_aliyun_access_key_id"], FAKE_KEY_ID)

    def test_resubmitting_the_mask_does_not_overwrite_the_secret(self):
        """前端把掩码原样回传表示「没改」。当成新值存进去的话，
        管理员保存一次别的设置就把 Secret 冲成了一串星号，而页面提示保存成功。"""
        self._put_ok(sms_aliyun_access_key_secret=FAKE_KEY_SECRET)
        before = self._stored("sms_aliyun_access_key_secret")
        masked = self._get()["sms_aliyun_access_key_secret"]
        self._put_ok(sms_aliyun_access_key_secret=masked, worker_concurrency=2)
        self.assertEqual(self._stored("sms_aliyun_access_key_secret"), before)
        # 而且解出来还是原来那一把
        session = self.Session()
        try:
            self.assertEqual(sms.load_config(session).access_key_secret, FAKE_KEY_SECRET)
        finally:
            session.close()

    def test_secret_can_be_cleared_with_an_empty_string(self):
        self._put_ok(sms_aliyun_access_key_secret=FAKE_KEY_SECRET)
        self._put_ok(sms_aliyun_access_key_secret="")
        self.assertEqual(self._stored("sms_aliyun_access_key_secret"), "")


# ══════════════════════════════════════════════════════════════════════
#  三、闸门：切阿里云 / 打开手机号注册
# ══════════════════════════════════════════════════════════════════════

class GateTests(Scaffold):

    def test_cannot_switch_to_aliyun_before_config_is_complete(self):
        detail = self._put_fail(sms_provider="aliyun")
        self.assertIn("AccessKey", detail)
        self.assertIn("调试模式", detail)   # 要给出「先切回调试模式」这条退路
        self.assertEqual(self._stored("sms_provider"), None)   # 一个字都没存进去

    def test_can_switch_to_aliyun_when_everything_is_filled_in_one_save(self):
        """一次保存里把四项和通道一起提交是允许的（也是最自然的做法）。"""
        self._configure_aliyun(provider="aliyun")
        self.assertEqual(self._stored("sms_provider"), "aliyun")

    def test_gate_is_two_way_cannot_break_a_live_aliyun_config(self):
        """已经在阿里云模式下，把签名清空同样要被拦住（闸门是双向的）。"""
        self._configure_aliyun(provider="aliyun")
        detail = self._put_fail(sms_aliyun_sign_name="")
        self.assertIn("短信签名", detail)

    def test_switching_back_to_debug_is_always_allowed(self):
        """「切回调试模式」这条退路必须永远畅通，否则库里一旦坏掉就没法收场。"""
        self._set_raw(sms_provider="aliyun")   # 造一个「通道开着但没配」的老库状态
        self._put_ok(sms_provider="debug")
        self.assertEqual(self._stored("sms_provider"), "debug")

    def test_phone_register_needs_the_shared_three_conditions(self):
        """手机号注册和邀请码注册**共用**同一套硬前置闸门。"""
        detail = self._put_fail(phone_register_enabled=True)
        self.assertIn("①", detail)   # 内网访问
        self.assertIn("②", detail)   # 对外访问地址
        self.assertFalse(self._stored("phone_register_enabled"))

    def test_phone_register_opens_in_debug_mode(self):
        """调试模式不花钱，是内网试跑整条流程的正常做法，不该拦。"""
        self._put_ok(phone_register_enabled=True, **OPEN_BASELINE)
        self.assertTrue(self._stored("phone_register_enabled"))

    def test_phone_register_blocked_when_aliyun_is_half_configured(self):
        """通道选了阿里云却没配齐，就不许打开手机号注册——
        否则每个人点「获取验证码」都失败，而失败原因是一串英文错误码。"""
        self._set_raw(sms_provider="aliyun", sms_aliyun_access_key_id=FAKE_KEY_ID)
        detail = self._put_fail(phone_register_enabled=True, **OPEN_BASELINE)
        self.assertIn("④", detail)
        self.assertIn("试发一条", detail)

    def test_phone_register_on_blocks_reopening_internal_targets(self):
        """反方向：手机号注册开着时，把内网访问改回来同样要被拦下。

        只做「打开时检查一次」等于没做——这正是最危险的组合，而界面上什么都看不出来。
        """
        self._put_ok(phone_register_enabled=True, **OPEN_BASELINE)
        detail = self._put_fail(allow_internal_targets=True)
        self.assertIn("手机号注册", detail)
        self.assertFalse(self._stored("allow_internal_targets"))

    def test_turning_phone_register_off_is_always_allowed(self):
        self._put_ok(phone_register_enabled=True, **OPEN_BASELINE)
        self._set_raw(files_signed_only=False)   # 造一个已经不合法的组合
        self._put_ok(phone_register_enabled=False)
        self.assertFalse(self._stored("phone_register_enabled"))


# ══════════════════════════════════════════════════════════════════════
#  四、当前状态：调试模式必须一眼看得出来
# ══════════════════════════════════════════════════════════════════════

class StatusTests(Scaffold):

    def test_debug_mode_is_labelled_in_the_settings_payload(self):
        status = self._get()["sms_status"]
        self.assertTrue(status["debug"])
        self.assertIn("调试模式", status["mode_text"])
        self.assertEqual(status["notice"], sms.DEBUG_MODE_NOTICE)
        self.assertFalse(status["ready"])
        self.assertTrue(status["problems"])

    def test_real_mode_says_it_costs_money(self):
        self._configure_aliyun(provider="aliyun")
        status = self._get()["sms_status"]
        self.assertFalse(status["debug"])
        self.assertIn("费用", status["mode_text"])
        self.assertTrue(status["ready"])

    def test_phone_register_on_while_still_in_debug_raises_a_warning(self):
        """这是最危险的一种组合：验证码直接显示在注册页上，等于没有验证。"""
        self._put_ok(phone_register_enabled=True, **OPEN_BASELINE)
        status = self._get()["sms_status"]
        self.assertTrue(status["warning"])
        self.assertIn("等于没有验证", status["warning"])

    def test_status_endpoint_matches_the_settings_payload(self):
        resp = self.client.get("/api/web/settings/sms", headers=self.auth)
        self.assertEqual(resp.status_code, 200, resp.text)
        self.assertEqual(resp.json()["provider"], self._get()["sms_status"]["provider"])

    def test_status_never_leaks_the_credentials(self):
        self._configure_aliyun(provider="aliyun")
        blob = json.dumps(self._get(), ensure_ascii=False)
        self.assertNotIn(FAKE_KEY_SECRET, blob)

    def test_limits_report_the_effective_values(self):
        """界面显示的必须是**实际生效**的值（已经夹过范围），
        否则管理员会照着一个根本没生效的数字去排查。"""
        self._set_raw(sms_code_global_daily_limit=999999)   # 老库遗留的越界值
        self.assertEqual(self._get()["sms_status"]["limits"]["global_daily_limit"], 2000)


# ══════════════════════════════════════════════════════════════════════
#  五、试发一条：真链路（打到假上游）+ 四层限速
# ══════════════════════════════════════════════════════════════════════

class TestSendTests(Scaffold):

    def test_bad_phone_is_rejected_before_anything_happens(self):
        resp = self._test_sms(phone="1380013800")   # 少一位
        self.assertEqual(resp.status_code, 422)
        self.assertIn("11 位", resp.json()["detail"])
        self.assertEqual(self.fake.calls, [])

    def test_incomplete_config_does_not_burn_any_quota(self):
        """没配齐就先别发。这一步没花钱，也不该白白吃掉一次全站名额。"""
        resp = self._test_sms()
        self.assertEqual(resp.status_code, 200, resp.text)
        payload = resp.json()
        self.assertFalse(payload["ok"])
        self.assertIn("没填齐", payload["message"])
        self.assertEqual(self.fake.calls, [])
        self.assertEqual(self._ratelimit_rows(), {})

    def test_successful_send_reports_the_receipt_and_charges_all_four_buckets(self):
        self._configure_aliyun(provider="debug")   # 调试模式下试发也照发不误
        resp = self._test_sms()
        self.assertEqual(resp.status_code, 200, resp.text)
        payload = resp.json()
        self.assertTrue(payload["ok"], payload)
        self.assertEqual(payload["provider_msg_id"], "biz-1")
        self.assertEqual(payload["phone"], "138****8000")   # 脱敏
        self.assertIn("受理", payload["note"])              # 受理 ≠ 送达
        self.assertEqual(len(self.fake.calls), 1)
        # 四层限速各记一笔——这条短信和注册页那边花的是同一张账单
        rows = self._ratelimit_rows()
        self.assertEqual(sorted(rows), sorted([
            settings_router.SMS_SCOPE_COOLDOWN,
            settings_router.SMS_SCOPE_PHONE_DAILY,
            settings_router.SMS_SCOPE_IP_HOURLY,
            settings_router.SMS_SCOPE_GLOBAL_DAILY,
        ]))
        # 结果里不能有验证码，也不能有凭据
        blob = json.dumps(payload, ensure_ascii=False)
        self.assertNotIn(FAKE_KEY_SECRET, blob)
        self.assertNotIn("13800138000", blob)

    def test_phone_number_never_lands_in_the_ratelimit_table(self):
        """限速的 key 会**写进数据库**、触发时还会**原样打进日志**
        （services/ratelimit.py 的 _maybe_block）。所以那里放的必须是不可逆的桶名，
        不是手机号本身——否则凭空多出第三处存全量号码的地方，而日志一打包发给
        别人排查，一串真实号码就跟着出去了，还不会有任何报错。
        """
        self._configure_aliyun()
        self.assertTrue(self._test_sms(phone="13800138000").json()["ok"])
        session = self.Session()
        try:
            keys = list(session.execute(select(RateLimitState.key)).scalars().all())
        finally:
            session.close()
        self.assertTrue(keys)
        for key in keys:
            self.assertNotIn("13800138000", key)
            self.assertNotIn("138", key[:3])   # 连号段都不该露出来
        # 同一个号码必须稳定落进同一个桶，否则限速等于没限
        self.assertEqual(
            settings_router.phone_bucket_key("13800138000"),
            settings_router.phone_bucket_key("13800138000"),
        )
        self.assertNotEqual(
            settings_router.phone_bucket_key("13800138000"),
            settings_router.phone_bucket_key("13900139000"),
        )

    def test_second_click_is_blocked_by_the_cooldown(self):
        """防连点：第二次必须被挡住，而且要说清「等多久」。"""
        self._configure_aliyun()
        self.assertTrue(self._test_sms().json()["ok"])
        resp = self._test_sms()
        self.assertEqual(resp.status_code, 429)
        self.assertIn("Retry-After", resp.headers)
        self.assertIn("秒", resp.json()["detail"])
        self.assertEqual(len(self.fake.calls), 1)   # 第二条**一条都没发出去**

    def test_global_daily_cap_stops_even_a_different_phone(self):
        """全站每日名额是最后一道兜底：换个号码也不行。"""
        self._configure_aliyun()
        self._put_ok(sms_code_global_daily_limit=1)
        self.assertTrue(self._test_sms(phone="13800138000").json()["ok"])
        resp = self._test_sms(phone="13900139000")
        self.assertEqual(resp.status_code, 429)
        self.assertIn("全站", resp.json()["detail"])
        self.assertEqual(len(self.fake.calls), 1)

    def test_over_limit_never_burns_the_other_buckets(self):
        """第四层拦下来的时候，前三层的额度**一次都不许扣**。

        扣了的话就会出现「明明没发几条，额度却见底了」——而这种账对不上，
        管理员既查不出来也不敢再调阈值。
        """
        self._configure_aliyun()
        self._put_ok(sms_code_global_daily_limit=1)
        self.assertTrue(self._test_sms(phone="13800138000").json()["ok"])
        before = self._ratelimit_rows()
        resp = self._test_sms(phone="13900139000")   # 换号码 → 只会撞上全站那一层
        self.assertEqual(resp.status_code, 429)
        # 新号码没有留下任何计数行（前三层一次都没扣）
        self.assertEqual(self._ratelimit_rows(), before)

    def test_balance_failure_tells_the_admin_to_top_up(self):
        """余额不足 → 说清「去充值」，而不是把给注册用户看的那句笼统的话丢给管理员。"""
        self._configure_aliyun()
        self.fake.reply({"Code": "isv.AMOUNT_NOT_ENOUGH", "Message": "amount not enough",
                         "RequestId": "req-2"})
        payload = self._test_sms().json()
        self.assertFalse(payload["ok"])
        self.assertEqual(payload["category"], sms.CATEGORY_BALANCE)
        self.assertEqual(payload["error_code"], "isv.AMOUNT_NOT_ENOUGH")
        self.assertIn("充值", payload["next_step"])
        self.assertIn("余额", payload["message"])
        # 给公网注册用户看的那句话不该出现在管理员的结果里（它对他毫无用处）
        self.assertNotIn("联系管理员帮你开通账号", payload["message"])

    def test_unapproved_signature_points_at_the_console_review_status(self):
        self._configure_aliyun()
        self.fake.reply({"Code": "isv.SMS_SIGNATURE_ILLEGAL", "Message": "illegal",
                         "RequestId": "req-3"})
        payload = self._test_sms().json()
        self.assertEqual(payload["category"], sms.CATEGORY_CONFIG)
        self.assertIn("审核", payload["next_step"])

    def test_flow_control_tells_the_admin_to_wait(self):
        self._configure_aliyun()
        self.fake.reply({"Code": "isv.BUSINESS_LIMIT_CONTROL", "Message": "limited",
                         "RequestId": "req-4"})
        payload = self._test_sms().json()
        self.assertEqual(payload["category"], sms.CATEGORY_FLOW_CONTROL)
        self.assertIn("等", payload["next_step"])

    def test_last_test_result_is_remembered_for_the_settings_page(self):
        self._configure_aliyun()
        self._test_sms()
        last = self._get()["sms_status"]["last_test"]
        self.assertIsNotNone(last)
        self.assertEqual(last["phone"], "138****8000")
        # 改了短信配置之后，上一次的结论就不作数了，必须清掉
        self._put_ok(sms_aliyun_sign_name="另一个签名")
        self.assertIsNone(self._get()["sms_status"]["last_test"])


if __name__ == "__main__":
    unittest.main()
