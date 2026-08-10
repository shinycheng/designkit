"""多用户隔离与「按人计费」的回归测试：不联网、不花钱、不用起服务。

这组测试守的是本期最贵的一条线——**谁的图，花谁的钱，别人看不到**。
它盯着三件事：

  1. `settings_service.resolve_for_user` 的五条规则。全系统只有这一个函数
     决定「这次生图用谁的 Key」，其中最要紧的是第 5 条：**五种失败场景一律报错，
     绝不回落到管理员的全局 Key**。一旦回落，生图会照常成功、成员余额一分不扣、
     钱全从管理员账户走，日志里还没有任何痕迹能看出这次是谁花的——
     表面上一切正常，是整个改造里最难发现的一种事故。
  2. 上面那条规则在真实任务上的兑现：开了「每人一把 Key」、成员没配 Key 时，
     任务必须**永久失败**（不进重试队列）、错误文案是中文人话、**且绝不出图**。
  3. 跨用户越权：拿别人的上传 id 建任务、查别人的任务、替别人点重试，
     一律 404（不是 403——403 等于确认「这条记录存在」）。

为什么这些必须是长期回归而不是验收时跑一次：这三类问题全都**不会报错**，
只会安静地把账记错、把别人的图放出去。没有测试盯着，下一个人「顺手优化」
一下就能把它们打开，而且谁都不会当场发现。
"""
import atexit
import os
import shutil
import tempfile
import unittest
import uuid

# 导入 backend.app.config 会在 import 期间建数据目录、改它的权限。
# 测试绝不该碰用户放着网关密钥和生产数据的那个 data/ 目录，先指到临时位置去。
if not os.environ.get("DESIGNKIT_DATA_DIR"):
    _TMP_DATA_DIR = tempfile.mkdtemp(prefix="dk-multiuser-")
    os.environ["DESIGNKIT_DATA_DIR"] = _TMP_DATA_DIR
    atexit.register(shutil.rmtree, _TMP_DATA_DIR, ignore_errors=True)
os.environ.setdefault("DESIGNKIT_PROVIDER", "mock")

from fastapi import HTTPException  # noqa: E402

from backend.app.database import SessionLocal, engine  # noqa: E402
from backend.app.models import (  # noqa: E402
    Base, GenerationJob, Upload, User, UserGatewayAccount,
)
from backend.app.routers import generations as generations_router  # noqa: E402
from backend.app.services import jobs, settings_service, user_gateway, worker  # noqa: E402

# 全局那把「有效的」Key。每条失败用例里它都是配好的、能用的——
# 正因为它就摆在那里随手可得，「找不到成员的 Key 就用它顶上」才显得那么顺理成章。
GLOBAL_KEY = "sk-global-admin-key-1234"
MEMBER_KEY = "sk-member-own-key-5678"
# 成员没开通额度时给他看的那句话。改文案要连这里一起改（前端会原样展示）。
NOT_READY = "你的账号还没有开通生图额度，请联系管理员在「成员账号」页配置"


def _new_user(db, role="member", active=True):
    user = User(username="mu_" + uuid.uuid4().hex[:10], password_hash="x",
                role=role, is_active=active, token_version=0)
    db.add(user)
    db.commit()
    return user


class ResolveForUserTests(unittest.TestCase):
    """resolve_for_user 的五条规则。顺序不能改，每一条都是踩过或算过的。"""

    @classmethod
    def setUpClass(cls):
        Base.metadata.create_all(bind=engine)

    def setUp(self):
        self.db = SessionLocal()
        self.member = _new_user(self.db, "member")
        self.admin = _new_user(self.db, "admin")
        # 全局 Key 始终是配好的、有效的：所有「不许回落」的断言都在这个前提下才有意义
        settings_service.set_many(self.db, {
            "provider": "openai",
            "gateway_mode": "per_user",
            "openai_api_key": GLOBAL_KEY,
        })

    def tearDown(self):
        self.db.query(UserGatewayAccount).delete()
        self.db.commit()
        self.db.close()

    def _give_member_key(self, state="active", key=MEMBER_KEY):
        account = user_gateway.upsert_key(self.db, self.member.id, key, state=state)
        self.db.commit()
        return account

    # ---------- 规则 1：mock 模式豁免 ----------

    def test_rule1_mock_mode_needs_no_key_at_all(self):
        # 少了这条豁免，全新部署下任何成员点生成都会被拦成「你的账号还没开通额度」，
        # 而管理员自己试是不会有事的（他走规则 4），所以这个坑管理员永远复现不出来。
        settings_service.set_many(self.db, {"provider": "mock"})
        values = settings_service.resolve_for_user(self.db, self.member.id)
        self.assertEqual(values["provider"], "mock")

    # ---------- 规则 2：没开「每人一把 Key」就什么都不用管 ----------

    def test_rule2_shared_mode_uses_global_key(self):
        # 默认就是 shared：升级当天的行为与改动前逐字一致，不需要谁先去配什么
        settings_service.set_many(self.db, {"gateway_mode": "shared"})
        values = settings_service.resolve_for_user(self.db, self.member.id)
        self.assertEqual(values["openai_api_key"], GLOBAL_KEY)

    # ---------- 规则 3：用这个人自己的 Key ----------

    def test_rule3_member_uses_own_key(self):
        self._give_member_key()
        values = settings_service.resolve_for_user(self.db, self.member.id)
        self.assertEqual(values["openai_api_key"], MEMBER_KEY)

    def test_rule3_admin_personal_key_wins_over_global(self):
        account = user_gateway.upsert_key(self.db, self.admin.id, "sk-admin-personal")
        self.db.commit()
        self.assertEqual(account.state, "active")
        values = settings_service.resolve_for_user(self.db, self.admin.id)
        self.assertEqual(values["openai_api_key"], "sk-admin-personal")

    # ---------- 规则 4：只有这两种情况能用全局 Key ----------

    def test_rule4_ownerless_job_uses_global_key(self):
        # ERP 建的无主任务、以及归属人已被删除的历史任务
        values = settings_service.resolve_for_user(self.db, None)
        self.assertEqual(values["openai_api_key"], GLOBAL_KEY)

    def test_rule4_admin_without_personal_key_uses_global(self):
        # 全局 Key 本来就是管理员配的、也是他在付钱
        values = settings_service.resolve_for_user(self.db, self.admin.id)
        self.assertEqual(values["openai_api_key"], GLOBAL_KEY)

    # ---------- 规则 5：五种失败场景，一律报错，绝不回落全局 Key ----------

    def _assert_never_falls_back(self, user_id):
        """断言两件事：抛的是 GatewayNotReady，且异常消息里不含全局 Key。"""
        with self.assertRaises(settings_service.GatewayNotReady) as caught:
            settings_service.resolve_for_user(self.db, user_id)
        self.assertEqual(str(caught.exception), NOT_READY)
        self.assertNotIn(GLOBAL_KEY, str(caught.exception))

    def test_rule5_member_without_account_fails(self):
        self._assert_never_falls_back(self.member.id)

    def test_rule5_member_with_disabled_account_fails(self):
        self._give_member_key()
        user_gateway.clear_key(self.db, self.member.id)
        self.db.commit()
        self._assert_never_falls_back(self.member.id)

    def test_rule5_member_with_non_active_state_fails(self):
        # key_issued = Key 拿到了但自检还没过。只有 active 才会被采用。
        self._give_member_key(state="key_issued")
        self._assert_never_falls_back(self.member.id)

    def test_rule5_member_with_undecryptable_key_fails(self):
        # 最常见的原因不是被攻击，而是只恢复了数据库、没恢复 data/.enc_key
        account = self._give_member_key()
        account.api_key_enc = "这不是一段能解开的密文"
        self.db.commit()
        self._assert_never_falls_back(self.member.id)

    def test_rule5_missing_user_fails(self):
        # user_id 有值却查不到人：数据不一致。既然连他是不是管理员都确认不了，
        # 就不能替他花钱。
        self._assert_never_falls_back(999999)

    def test_rule5_never_returns_global_key_in_any_failure_case(self):
        """把五种失败场景放在一起再断一次：没有任何一种能拿到全局 Key。

        上面五条是分开断的，这一条是「合起来看」的兜底：将来有人给
        resolve_for_user 加第六种情况时，最容易的写法就是 `return values`，
        而那正是这一整套设计要防的事。
        """
        cases = []

        # ① 没有网关账号
        cases.append(self.member.id)
        # ② 指向不存在的用户
        cases.append(999998)
        for user_id in cases:
            with self.assertRaises(settings_service.GatewayNotReady):
                settings_service.resolve_for_user(self.db, user_id)

        # ③ 状态不是 active ④ 密文解不开 ⑤ 密文为空
        for state, enc in (("manual", None), ("active", "坏密文"), ("active", "")):
            account = self._give_member_key()
            account.state = state
            account.api_key_enc = enc
            self.db.commit()
            with self.assertRaises(settings_service.GatewayNotReady):
                settings_service.resolve_for_user(self.db, self.member.id)
            self.db.query(UserGatewayAccount).delete()
            self.db.commit()


class PerUserJobFailureTests(unittest.TestCase):
    """开了「每人一把 Key」、成员没配 Key 时，任务必须永久失败且绝不出图。

    这是整组测试里最重要的一条。它把 resolve_for_user 的第 5 条规则放到
    真实的任务处理流程上验一遍：只验函数会抛异常是不够的——worker 里只要有人
    把那个 except 改成「记个日志接着往下跑」，函数照样抛、图照样出、
    钱照样从管理员账上走。
    """

    @classmethod
    def setUpClass(cls):
        Base.metadata.create_all(bind=engine)

    def setUp(self):
        self.db = SessionLocal()
        self.member = _new_user(self.db, "member")
        settings_service.set_many(self.db, {
            "provider": "openai",          # 不是 mock，所以规则 1 的豁免用不上
            "gateway_mode": "per_user",
            "openai_api_key": GLOBAL_KEY,  # 全局 Key 有效且触手可及
            "max_attempts": 3,             # 故意允许重试，看它会不会真去重试
        })
        self.job = GenerationJob(
            id=uuid.uuid4().hex, source="web", user_id=self.member.id,
            status="processing", prompt_final="一张干净的白底商品图",
            params={"n": 1, "size": "1024x1024"},
        )
        self.db.add(self.job)
        self.db.commit()

    def tearDown(self):
        self.db.query(UserGatewayAccount).delete()
        self.db.commit()
        self.db.close()

    def _run(self):
        """跑一次真实的任务处理，并且把「有没有真的去调网关」记下来。

        桩函数先记账再抛异常：记账是为了让下面的断言能直接看到「调过没有」，
        抛异常是为了万一哪天有人把桩换成真货，也不会真的把图生出来。
        """
        self.calls = []
        original = worker.provider.generate_images

        def must_not_be_called(*args, **kwargs):
            self.calls.append(kwargs or args)
            raise AssertionError(
                "成员没有网关 Key，却仍然调用了生图接口——这一刻钱已经从别人账上花出去了"
            )

        worker.provider.generate_images = must_not_be_called
        try:
            worker.GenerationWorker()._process(self.job.id)
        finally:
            worker.provider.generate_images = original
        self.db.expire_all()
        return self.db.get(GenerationJob, self.job.id)

    def test_gateway_is_never_called(self):
        """最要害的一条：生图接口**一次都不能被调到**。

        只断言「任务失败了」是不够的——先把图生出来、再因为别的原因失败，
        钱一样已经从管理员账上扣掉了，而任务状态看上去和现在一模一样。
        """
        self._run()
        self.assertEqual(self.calls, [])

    def test_job_fails_permanently(self):
        job = self._run()
        self.assertEqual(job.status, "failed")

    def test_job_does_not_go_back_to_retry_queue(self):
        # 「这个人没开通额度」不是网络抖动，重试多少次都是同一句话，
        # 只会让他多等两轮退避才看到结果。
        job = self._run()
        self.assertEqual(job.attempts, 1)
        self.assertIsNone(job.next_attempt_at)

    def test_error_message_is_plain_chinese(self):
        job = self._run()
        self.assertEqual(job.error, NOT_READY)

    def test_no_image_is_produced(self):
        job = self._run()
        self.assertEqual(list(job.images or []), [])

    def test_global_key_is_never_used(self):
        # 双保险：错误信息里不能出现全局 Key 的任何片段，
        # 且上面 _run 里那个「一调用就炸」的桩没有被触发（触发了会直接 error）。
        job = self._run()
        self.assertNotIn(GLOBAL_KEY, str(job.error))

    def test_member_with_own_key_gets_own_key_not_the_global_one(self):
        """配套的正向用例：成员配了自己的 Key，生图就必须用他自己那把。

        少了这一条，把整段逻辑改成「一律抛异常」也能让上面五条全绿。
        """
        user_gateway.upsert_key(self.db, self.member.id, MEMBER_KEY)
        self.db.commit()
        values = settings_service.resolve_for_user(self.db, self.member.id)
        self.assertEqual(values["openai_api_key"], MEMBER_KEY)
        self.assertNotEqual(values["openai_api_key"], GLOBAL_KEY)


class CrossUserAccessTests(unittest.TestCase):
    """跨用户越权：一律 404，且文案与「不存在」逐字相同。

    为什么必须是 404 而不是 403：403 等于回答了「这条记录确实存在」。
    对方拿连续的 id 遍历一遍，就能把站内有多少人、每人有多少任务摸清楚。
    """

    @classmethod
    def setUpClass(cls):
        Base.metadata.create_all(bind=engine)

    def setUp(self):
        self.db = SessionLocal()
        self.alice = _new_user(self.db, "member")
        self.bob = _new_user(self.db, "member")
        self.admin = _new_user(self.db, "admin")
        self.upload = Upload(user_id=self.alice.id, original_name="a.png",
                             path="uploads/202608/%s.png" % uuid.uuid4().hex)
        self.db.add(self.upload)
        self.db.commit()
        self.job = GenerationJob(id=uuid.uuid4().hex, source="web",
                                 user_id=self.alice.id, status="succeeded",
                                 prompt_final="p")
        self.db.add(self.job)
        self.db.commit()

    def tearDown(self):
        self.db.close()

    def test_other_member_cannot_use_my_upload(self):
        # A 上传 → B 拿 A 的 upload_id 建任务 → 404。
        # 不挡的话，B 能把 A 的商品图当输入图使用（等于看到了那张图）。
        with self.assertRaises(HTTPException) as caught:
            jobs.resolve_upload_paths(self.db, [self.upload.id], user_id=self.bob.id)
        self.assertEqual(caught.exception.status_code, 404)

    def test_owner_can_use_own_upload(self):
        paths = jobs.resolve_upload_paths(self.db, [self.upload.id], user_id=self.alice.id)
        self.assertEqual(paths, [self.upload.path])

    def test_missing_upload_and_stolen_upload_look_identical(self):
        """「别人的」和「不存在的」必须给出一模一样的回答。

        一旦有人把越权改成 403 或换个文案，存在性探测就复活了。
        """
        with self.assertRaises(HTTPException) as stolen:
            jobs.resolve_upload_paths(self.db, [self.upload.id], user_id=self.bob.id)
        with self.assertRaises(HTTPException) as missing:
            jobs.resolve_upload_paths(self.db, [self.upload.id + 10 ** 6], user_id=self.bob.id)
        self.assertEqual(stolen.exception.status_code, missing.exception.status_code)

    def test_caller_without_identity_is_refused(self):
        # fail-closed：两个归属参数都不传 = 直接拒，不是「没归属所以全放行」
        with self.assertRaises(HTTPException) as caught:
            jobs.resolve_upload_paths(self.db, [self.upload.id])
        self.assertEqual(caught.exception.status_code, 403)

    def test_erp_key_cannot_use_web_upload(self):
        # ERP 侧按 api_key_id 判归属，拿不到网页端用户上传的图
        with self.assertRaises(HTTPException) as caught:
            jobs.resolve_upload_paths(self.db, [self.upload.id], api_key_id=12345)
        self.assertEqual(caught.exception.status_code, 404)

    def test_other_member_cannot_read_my_job(self):
        with self.assertRaises(HTTPException) as caught:
            generations_router._get_readable_job(self.job.id, self.bob, self.db)
        self.assertEqual(caught.exception.status_code, 404)
        self.assertEqual(caught.exception.detail, generations_router._JOB_NOT_FOUND)

    def test_admin_can_read_anyones_job(self):
        # 成员说「我的图出不来」时，得有人能点开那条任务看 error
        job = generations_router._get_readable_job(self.job.id, self.admin, self.db)
        self.assertEqual(job.id, self.job.id)

    def test_admin_cannot_retry_someone_elses_job(self):
        """管理员**可读不可写**。这条不对称是故意的，不是漏改。

        「重新生成」和「补图」会立刻拿**任务归属人**的网关额度去出图，
        花的是那个成员的真金白银，发出去就收不回来。
        """
        with self.assertRaises(HTTPException) as caught:
            generations_router._get_writable_job(self.job.id, self.admin, self.db)
        self.assertEqual(caught.exception.status_code, 404)

    def test_owner_can_write_own_job(self):
        job = generations_router._get_writable_job(self.job.id, self.alice, self.db)
        self.assertEqual(job.id, self.job.id)

    def test_admin_can_write_ownerless_job(self):
        # 历史上没有主人的 ERP 任务，只让管理员操作，否则它们会彻底动不了
        orphan = GenerationJob(id=uuid.uuid4().hex, source="api", user_id=None,
                               status="failed", prompt_final="p")
        self.db.add(orphan)
        self.db.commit()
        job = generations_router._get_writable_job(orphan.id, self.admin, self.db)
        self.assertEqual(job.id, orphan.id)


if __name__ == "__main__":
    unittest.main()
