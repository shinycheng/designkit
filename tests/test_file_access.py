"""图片访问鉴权（/files 路由）的回归测试：不联网、不花钱、不用起服务。

守的是这一期最要紧的一个洞：在这次改动之前，/files/uploads、/files/outputs、
/files/thumbnails 是三个**一点鉴权都没有**的静态目录，知道地址就能下载任何人的
商品图和出图结果，而地址除了任务号之外全是可枚举的。

这组测试盯着四类事，任何一类被改坏都会当场变红：

  1. 路径白名单：只有形状完全正确的三种路径才放行，`..`、编码过的 `%2f`、
     大写后缀、非十六进制任务号一律 404（不是 403——403 等于告诉对方猜对了目录）；
  2. 网页端 Cookie：本人能取自己的图，别人取到 404，管理员能排障，
     改密码 / 停用账号之后旧 Cookie **立刻**失效；
  3. 对外 API 的签名链接：签名、到期时间、Key 编号三者任改一位都失效，
     停用这把 Key 会让它此前发出的所有链接立刻打不开；
  4. 没有任何凭证时按 files_signed_only 开关行事，并把次数记进计数器
     （管理员就是靠这个计数器判断敢不敢临时关掉开关的）。

**其中 RawAsgiTraversalTests 那一组不能删、也不能改写成用 httpx / requests 去请求。**
httpx 和浏览器都会在客户端先把 `../` 归一化掉，请求根本到不了服务端，
测出来是一片假绿。真正要防的是「有人用 curl --path-as-is 或裸 socket 把 `../`
原样打进来」，只有手工构造 ASGI scope 才碰得到那条路径。
"""
import asyncio
import atexit
import os
import shutil
import tempfile
import time
import unittest
import uuid

# 导入 backend.app.config 会在 import 期间就去建数据目录、改它的权限。
# 测试绝不该碰用户放着网关密钥和生产数据的那个 data/ 目录，
# 所以在导入之前先把数据目录指到一个临时位置去。
# 用 setdefault：按 tests/README.md 的标准跑法本来就会显式设这个变量，那时不覆盖它。
if not os.environ.get("DESIGNKIT_DATA_DIR"):
    _TMP_DATA_DIR = tempfile.mkdtemp(prefix="dk-file-access-")
    os.environ["DESIGNKIT_DATA_DIR"] = _TMP_DATA_DIR
    atexit.register(shutil.rmtree, _TMP_DATA_DIR, ignore_errors=True)
os.environ.setdefault("DESIGNKIT_PROVIDER", "mock")

from fastapi import FastAPI  # noqa: E402
from fastapi.testclient import TestClient  # noqa: E402

from backend.app.config import DATA_DIR  # noqa: E402
from backend.app.database import SessionLocal, engine  # noqa: E402
from backend.app.models import (  # noqa: E402
    ApiKey, Base, GenerationJob, PromptTemplate, Upload, User,
)
from backend.app.routers import files as files_router  # noqa: E402
from backend.app.services import file_access, settings_service  # noqa: E402

# 1×1 的合法 PNG。用真图而不是随便几个字节，是为了让 content-type 推断、
# ETag、304 这些走的是和线上完全一样的那条路。
PNG = bytes.fromhex(
    "89504e470d0a1a0a0000000d4948445200000001000000010802000000907753"
    "de0000000c4944415408d76360600000000400012734270a0000000049454e44ae426082"
)
MONTH = "202608"

# 整组测试共用的固定数据，在 setUpModule 里一次性建好
S = {}


def setUpModule():
    Base.metadata.create_all(bind=engine)
    db = SessionLocal()

    alice = User(username="fa_alice", password_hash="x", role="member", token_version=0)
    bob = User(username="fa_bob", password_hash="x", role="member", token_version=0)
    boss = User(username="fa_boss", password_hash="x", role="admin", token_version=0)
    db.add_all([alice, bob, boss])
    db.commit()

    key = ApiKey(user_id=alice.id, name="erp", key_prefix="dk_fa1",
                 key_hash="h1", webhook_secret="w")
    key2 = ApiKey(user_id=bob.id, name="erp2", key_prefix="dk_fa2",
                  key_hash="h2", webhook_secret="w")
    db.add_all([key, key2])
    db.commit()

    job_id = uuid.uuid4().hex
    erp_job_id = uuid.uuid4().hex
    db.add_all([
        GenerationJob(id=job_id, source="web", user_id=alice.id, prompt_final="p"),
        GenerationJob(id=erp_job_id, source="api", user_id=alice.id,
                      api_key_id=key.id, prompt_final="p"),
    ])
    db.commit()

    rels = {
        "out": "outputs/%s/%s/0.png" % (MONTH, job_id),
        "thumb": "thumbnails/%s/%s_0.jpg" % (MONTH, job_id),
        "erp_out": "outputs/%s/%s/0.png" % (MONTH, erp_job_id),
        "upload": "uploads/%s/%s.png" % (MONTH, uuid.uuid4().hex),
        "template": "uploads/%s/%s.jpg" % (MONTH, uuid.uuid4().hex),
        # 磁盘上有、库里查不到归属的文件：必须 404
        "orphan": "uploads/%s/%s.png" % (MONTH, uuid.uuid4().hex),
    }
    db.add(Upload(user_id=alice.id, path=rels["upload"], original_name="a.png"))
    db.add(PromptTemplate(name="封面用模板", prompt_template="x",
                          thumbnail_path=rels["template"]))
    db.commit()

    for rel in rels.values():
        path = DATA_DIR / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(PNG)
    # 假的数据库文件与密钥文件，用来确认它们绝对下载不到
    (DATA_DIR / "designkit.db").write_bytes(b"fake")
    (DATA_DIR / ".secret_key").write_bytes(b"fake")

    app = FastAPI()
    app.include_router(files_router.router)

    S.update(
        db=db, app=app, client=TestClient(app),
        alice=alice, bob=bob, boss=boss, key=key, key2=key2,
        job_id=job_id, erp_job_id=erp_job_id, **rels,
    )
    # 这组测试全程按「默认开着鉴权」跑，与线上默认值一致
    settings_service.set_many(db, {"files_signed_only": True})


def tearDownModule():
    if S.get("db") is not None:
        S["db"].close()


def cookie_for(user):
    """给某个用户签一张当前有效的取图 Cookie。"""
    return {file_access.WEB_COOKIE_NAME:
            file_access.issue_web_cookie(user.id, user.token_version)}


def get(rel_path, **kw):
    return S["client"].get("/files/" + rel_path, **kw)


class SafePathTests(unittest.TestCase):
    """路径白名单。这一层挡住的是「猜地址 + 拼 ../」，是整条链路的第一道门。"""

    def assert_404(self, rel_path):
        """管理员身份也不放行——白名单是形状校验，与身份无关。"""
        r = get(rel_path, cookies=cookie_for(S["boss"]))
        self.assertEqual(r.status_code, 404, "%s 未被拦截：HTTP %d" % (rel_path, r.status_code))

    def test_classify_recognises_three_kinds(self):
        self.assertEqual(
            [file_access.classify(S[k]) for k in ("upload", "out", "thumb")],
            ["uploads", "outputs", "thumbnails"],
        )

    def test_database_file_not_downloadable(self):
        self.assert_404("designkit.db")

    def test_secret_key_files_not_downloadable(self):
        self.assert_404(".secret_key")
        self.assert_404(".enc_key")

    def test_encoded_traversal_in_outputs_rejected(self):
        # outputs/年月/任务号/..%2f..%2f别人的任务号/0.png
        self.assert_404("outputs/%s/%s/..%%2f..%%2f%s/%s/0.png"
                        % (MONTH, S["job_id"], MONTH, S["erp_job_id"]))

    def test_encoded_traversal_to_database_rejected(self):
        # 任务书点名的这条：uploads/年月/..%2f..%2fdesignkit.db
        self.assert_404("uploads/%s/..%%2f..%%2fdesignkit.db" % MONTH)
        self.assertIsNone(file_access.classify("uploads/%s/../../designkit.db" % MONTH))

    def test_uppercase_extension_rejected(self):
        # 大写后缀必须拒。放行的话，同一个文件会有两种写法，
        # 反查归属用的字符串和真正读盘用的字符串就有可能不是同一个。
        self.assert_404("uploads/%s/%s.PNG" % (MONTH, uuid.uuid4().hex))

    def test_uppercase_hex_job_id_rejected(self):
        self.assert_404("uploads/%s/%s.png" % (MONTH, uuid.uuid4().hex.upper()))

    def test_non_hex_job_id_rejected(self):
        self.assert_404("outputs/%s/%s/0.png" % (MONTH, "z" * 32))
        self.assert_404("thumbnails/%s/%s_0.jpg" % (MONTH, "g" * 32))
        self.assert_404("uploads/%s/%s.png" % (MONTH, "x" * 32))

    def test_short_job_id_rejected(self):
        self.assert_404("outputs/%s/%s/0.png" % (MONTH, "ab12"))

    def test_trailing_newline_rejected(self):
        self.assert_404("uploads/%s/%s.png%%0a" % (MONTH, uuid.uuid4().hex))

    def test_trailing_slash_and_double_slash_rejected(self):
        self.assert_404("outputs/%s/%s/0.png/" % (MONTH, S["job_id"]))
        self.assert_404("outputs//%s/%s/0.png" % (MONTH, S["job_id"]))

    def test_unexpected_extension_rejected(self):
        # svg 能被浏览器当成脚本执行，绝不能出现在白名单里
        self.assert_404("outputs/%s/%s/0.svg" % (MONTH, S["job_id"]))

    def test_three_digit_index_rejected(self):
        self.assert_404("outputs/%s/%s/999.png" % (MONTH, S["job_id"]))

    def test_job_id_of_outputs(self):
        self.assertEqual(file_access.job_id_of("outputs", S["out"]), S["job_id"])

    def test_job_id_of_thumbnails_splits_on_last_underscore(self):
        # 缩略图是 <任务号>_<序号>.jpg，从右边切下划线。
        self.assertEqual(file_access.job_id_of("thumbnails", S["thumb"]), S["job_id"])

    def test_job_id_never_contains_underscore(self):
        # 上一条能成立的前提：白名单只认 32 位十六进制的任务号，
        # 里面不可能出现下划线，所以切分永远不会有歧义。
        # 一旦有人把正则放宽成 \w{32}，这一条会当场变红。
        self.assertIsNone(file_access.classify(
            "thumbnails/%s/%s_%s_0.jpg" % (MONTH, "a" * 16, "b" * 15)))


class RawAsgiTraversalTests(unittest.TestCase):
    """**不要把这一组改写成用 httpx / requests 去请求。**

    httpx 和浏览器都会在客户端先把 `../` 归一化掉，请求根本到不了服务端，
    测出来是一片假绿。真正要防的是「用 curl --path-as-is 或裸 socket
    把没归一化的 `../` 原样打进来」，只有手工构造 ASGI scope 才碰得到那条路径。
    """

    @staticmethod
    def raw_asgi_get(path, cookie=""):
        out = {}
        messages = [{"type": "http.request", "body": b"", "more_body": False}]

        async def receive():
            return messages.pop(0)

        async def send(msg):
            if msg["type"] == "http.response.start":
                out["status"] = msg["status"]

        headers = [(b"host", b"testserver")]
        if cookie:
            headers.append((b"cookie", cookie.encode()))
        scope = {
            "type": "http", "asgi": {"version": "3.0"}, "http_version": "1.1",
            "method": "GET", "scheme": "http", "path": path, "raw_path": path.encode(),
            "root_path": "", "query_string": b"", "headers": headers,
            "client": ("127.0.0.1", 1234), "server": ("testserver", 80),
        }
        asyncio.new_event_loop().run_until_complete(S["app"](scope, receive, send))
        return out.get("status")

    @property
    def admin_cookie(self):
        return "%s=%s" % (file_access.WEB_COOKIE_NAME,
                          file_access.issue_web_cookie(S["boss"].id, S["boss"].token_version))

    def test_unnormalized_traversal_is_404(self):
        path = "/files/outputs/%s/%s/../../%s/%s/0.png" % (
            MONTH, S["job_id"], MONTH, S["erp_job_id"])
        self.assertEqual(self.raw_asgi_get(path, self.admin_cookie), 404)

    def test_traversal_is_rejected_at_classify(self):
        path = "outputs/%s/%s/../../%s/%s/0.png" % (
            MONTH, S["job_id"], MONTH, S["erp_job_id"])
        self.assertIsNone(file_access.classify(path))

    def test_control_same_file_is_reachable_by_normal_path(self):
        # 对照组：上面那条穿越指向的文件本身是取得到的，
        # 证明前两条测到的确实是「被拦下了」，而不是「文件本来就不在」。
        path = "/files/outputs/%s/%s/0.png" % (MONTH, S["erp_job_id"])
        self.assertEqual(self.raw_asgi_get(path, self.admin_cookie), 200)


class WebCookieTests(unittest.TestCase):
    """网页端凭证：登录时种下的 dk_files Cookie。"""

    def test_owner_can_get_own_output(self):
        self.assertEqual(get(S["out"], cookies=cookie_for(S["alice"])).status_code, 200)

    def test_owner_can_get_own_thumbnail(self):
        self.assertEqual(get(S["thumb"], cookies=cookie_for(S["alice"])).status_code, 200)

    def test_owner_can_get_own_upload(self):
        self.assertEqual(get(S["upload"], cookies=cookie_for(S["alice"])).status_code, 200)

    def test_other_member_gets_404_for_output(self):
        # 404 而不是 403：403 等于确认「这张图存在」，够别人拿任务号做存在性探测了
        self.assertEqual(get(S["out"], cookies=cookie_for(S["bob"])).status_code, 404)

    def test_other_member_gets_404_for_upload(self):
        self.assertEqual(get(S["upload"], cookies=cookie_for(S["bob"])).status_code, 404)

    def test_other_member_gets_404_for_thumbnail(self):
        self.assertEqual(get(S["thumb"], cookies=cookie_for(S["bob"])).status_code, 404)

    def test_admin_can_read_anyones_image(self):
        # 管理员要帮成员排障（「我这张图怎么糊的」），看不到图就没法回答
        self.assertEqual(get(S["out"], cookies=cookie_for(S["boss"])).status_code, 200)

    def test_template_cover_visible_to_every_logged_in_user(self):
        # 模板封面落在 uploads/ 下但在 uploads 表里没有行，
        # 少了这条特判，全站模板封面会对所有人 404
        self.assertEqual(get(S["template"], cookies=cookie_for(S["bob"])).status_code, 200)

    def test_orphan_file_is_404(self):
        self.assertEqual(get(S["orphan"], cookies=cookie_for(S["boss"])).status_code, 404)

    def test_response_headers_are_private_and_safe(self):
        r = get(S["out"], cookies=cookie_for(S["alice"]))
        # private：不许中间的反向代理/CDN 存一份，否则会被发给下一个没凭证的人
        self.assertEqual(r.headers.get("cache-control"), "private, max-age=300")
        self.assertEqual(r.headers.get("x-content-type-options"), "nosniff")
        self.assertEqual(r.headers.get("referrer-policy"), "no-referrer")
        self.assertTrue(r.headers.get("content-type", "").startswith("image/"))

    def test_conditional_get_returns_304(self):
        # 没有 304 的话，历史页每打开一次都要把满屏缩略图全量重传，
        # 在 NAS 的家用上行带宽下会被直接感受成「系统变卡了」
        first = get(S["out"], cookies=cookie_for(S["alice"]))
        again = get(S["out"], cookies=cookie_for(S["alice"]),
                    headers={"If-None-Match": first.headers.get("etag", "")})
        self.assertEqual(again.status_code, 304)

    def test_cookie_roundtrip(self):
        claims = file_access.parse_web_cookie(file_access.issue_web_cookie(7, 3))
        self.assertEqual(claims["user_id"], 7)
        self.assertEqual(claims["ver"], 3)

    def test_forged_cookie_signature_rejected(self):
        bad = {file_access.WEB_COOKIE_NAME:
               "%d.0.9999999999.%s" % (S["alice"].id, "0" * 32)}
        self.assertEqual(get(S["out"], cookies=bad).status_code, 403)

    def test_expired_cookie_rejected(self):
        past = int(time.time()) - 10
        value = "%d.0.%d.%s" % (
            S["alice"].id, past,
            file_access._sig(file_access._COOKIE_MSG % (S["alice"].id, 0, past)))
        self.assertEqual(
            get(S["out"], cookies={file_access.WEB_COOKIE_NAME: value}).status_code, 403)

    def test_token_version_bump_revokes_cookie(self):
        # 改密码 / 停用账号会把 token_version +1。这一条让那套撤销机制
        # 第一次覆盖到图片——以前撤销做得再认真，/files 上也是断的。
        stale = cookie_for(S["alice"])
        S["alice"].token_version = 1
        S["db"].commit()
        try:
            self.assertEqual(get(S["out"], cookies=stale).status_code, 403)
        finally:
            S["alice"].token_version = 0
            S["db"].commit()

    def test_deactivated_user_cookie_revoked(self):
        S["alice"].is_active = False
        S["db"].commit()
        try:
            self.assertEqual(get(S["out"], cookies=cookie_for(S["alice"])).status_code, 403)
        finally:
            S["alice"].is_active = True
            S["db"].commit()


class NoCredentialTests(unittest.TestCase):
    """没有任何凭证时的行为，以及给管理员看的那个计数器。"""

    def tearDown(self):
        settings_service.set_many(S["db"], {"files_signed_only": True})

    def test_blocked_when_signed_only_on(self):
        settings_service.set_many(S["db"], {"files_signed_only": True})
        self.assertEqual(get(S["out"]).status_code, 403)

    def test_allowed_when_switch_turned_off(self):
        # 保留这个开关是给将来真接了 ERP 对接方时临时放宽用的
        settings_service.set_many(S["db"], {"files_signed_only": False})
        self.assertEqual(get(S["out"]).status_code, 200)

    def test_counter_records_blocked_access(self):
        file_access.reset_unsigned_stats()
        settings_service.set_many(S["db"], {"files_signed_only": True})
        get(S["out"])
        stats = file_access.unsigned_access_stats()
        self.assertEqual(stats["total"], 1)
        self.assertEqual(stats["blocked"], 1)

    def test_counter_path_does_not_leak_job_id(self):
        # 计数器会被展示在设置页上，只记目录前缀、不记完整路径
        file_access.reset_unsigned_stats()
        get(S["out"])
        self.assertNotIn(S["job_id"], str(file_access.unsigned_access_stats()["last_path"]))


class ErpSignatureTests(unittest.TestCase):
    """对外 API 的签名链接 ?k=<Key编号>&e=<到期秒>&t=<签名>。"""

    def setUp(self):
        self.exp = file_access.erp_exp(168)
        self.query = file_access.signed_query(S["erp_out"], S["key"].id, self.exp)

    def test_valid_signature_downloads_without_cookie(self):
        self.assertEqual(get(S["erp_out"] + "?" + self.query).status_code, 200)

    def test_signature_valid_but_image_belongs_to_another_key(self):
        # 签名是真的，但这张图不是这把 Key 的 → 404，不确认它存在
        q = file_access.signed_query(S["out"], S["key"].id, self.exp)
        self.assertEqual(get(S["out"] + "?" + q).status_code, 404)

    def test_tampered_signature_403(self):
        r = get(S["erp_out"] + "?k=%d&e=%d&t=%s" % (S["key"].id, self.exp, "0" * 32))
        self.assertEqual(r.status_code, 403)

    def test_tampered_exp_403(self):
        # 只改到期时间、签名照抄 → 必须失效，否则链接等于永不过期
        r = get(S["erp_out"] + "?k=%d&e=%d&t=%s"
                % (S["key"].id, self.exp + 86400,
                   file_access.sign_erp(S["erp_out"], S["key"].id, self.exp)))
        self.assertEqual(r.status_code, 403)

    def test_tampered_key_id_403(self):
        r = get(S["erp_out"] + "?k=%d&e=%d&t=%s"
                % (S["key2"].id, self.exp,
                   file_access.sign_erp(S["erp_out"], S["key"].id, self.exp)))
        self.assertEqual(r.status_code, 403)

    def test_signature_not_transferable_to_another_path(self):
        sig = file_access.sign_erp(S["erp_out"], S["key"].id, self.exp)
        r = get(S["out"] + "?k=%d&e=%d&t=%s" % (S["key"].id, self.exp, sig))
        self.assertEqual(r.status_code, 403)

    def test_other_key_resigned_gets_404(self):
        # 换一把 Key 重新签一个合法签名，也换不到别人的图
        q = file_access.signed_query(S["erp_out"], S["key2"].id, self.exp)
        self.assertEqual(get(S["erp_out"] + "?" + q).status_code, 404)

    def test_expired_link_403(self):
        past = int(time.time()) - 10
        q = file_access.signed_query(S["erp_out"], S["key"].id, past)
        self.assertEqual(get(S["erp_out"] + "?" + q).status_code, 403)

    def test_disabled_key_kills_all_its_links(self):
        # 停用一把 Key，它此前发出去的所有图片链接立刻打不开，不用等签名过期
        S["key"].is_active = False
        S["db"].commit()
        try:
            self.assertEqual(get(S["erp_out"] + "?" + self.query).status_code, 403)
        finally:
            S["key"].is_active = True
            S["db"].commit()

    def test_non_numeric_params_are_403_not_422(self):
        # 422 会把参数的含义暴露出来，所以坏参数一律当成签名无效
        self.assertEqual(get(S["erp_out"] + "?k=abc&e=xx&t=zz").status_code, 403)

    def test_signed_query_and_append_signature_format(self):
        self.assertEqual(
            file_access.append_signature("http://x/files/a", S["upload"], 1, self.exp).count("?"), 1)
        self.assertEqual(
            file_access.append_signature("http://x/files/a?z=1", S["upload"], 1, self.exp).count("?"), 1)


if __name__ == "__main__":
    unittest.main()
