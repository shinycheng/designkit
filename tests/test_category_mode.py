"""「按分类生成」的单测：多分类归属、通览清单、按编号取全文。不联网、不花钱。

这条链路是生成页的主路径，出问题的表现是「选了分类却出通用图」或
「某个分类看起来几乎是空的」——都很难被用户直接看出来。
"""
import atexit
import json
import os
import shutil
import tempfile
import unittest
import urllib.request

from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

# 导入 backend 里的模块会连带导入 config.py，而 config.py 在 import 期间就会去
# 建数据目录、改它的权限。测试不该碰用户放着网关密钥和生产数据的那个 data/ 目录，
# 所以在导入之前先把数据目录指到一个临时位置去。
# 用 setdefault：按 tests/README.md 的标准跑法本来就会显式设这个变量，那时不覆盖它。
# 每个测试文件都写一遍，是因为「先导入哪个文件」取决于跑法，
# 只在其中一个里写，换个跑法就漏掉了。
if not os.environ.get("DESIGNKIT_DATA_DIR"):
    _TMP_DATA_DIR = tempfile.mkdtemp(prefix="dk-unittest-")
    os.environ["DESIGNKIT_DATA_DIR"] = _TMP_DATA_DIR
    atexit.register(shutil.rmtree, _TMP_DATA_DIR, ignore_errors=True)

from backend.app.models import Base, PromptCategory, PromptTemplate
from backend.app.services import inspiration


def _entry(db, pid, name, slugs, prompt, category_id=None):
    row = PromptTemplate(
        name=name,
        source="youmind",
        source_ref="youmind:%d" % pid,
        source_slugs=json.dumps(slugs, ensure_ascii=False),
        category_id=category_id,
        prompt_template=prompt,
        variables=[],
        default_params={},
        requires_input_image=True,
        is_enabled=False,
        sort=0,
    )
    db.add(row)
    return row


class CategoryFilterTests(unittest.TestCase):
    """一条提示词常同属多个分类，只认第一个会让「电商主图」416 条只剩 18 条。"""

    def setUp(self):
        engine = create_engine("sqlite://", future=True)
        Base.metadata.create_all(engine)
        self.db = sessionmaker(bind=engine, future=True)()
        cat = PromptCategory(name="产品营销", sort=1)
        self.db.add(cat)
        self.db.flush()
        # 主分类都是「产品营销」，但其中两条也带 ecommerce-main-image 标签
        _entry(self.db, 1, "Studio Can Shot",
               ["product-marketing", "ecommerce-main-image"],
               "product photo of a can on seamless background", cat.id)
        _entry(self.db, 2, "Perfume Packshot",
               ["product-marketing", "ecommerce-main-image"],
               "packshot of a perfume bottle, studio lighting", cat.id)
        _entry(self.db, 3, "Anime Poster",
               ["product-marketing"], "an anime character poster", cat.id)
        self.db.commit()

    def tearDown(self):
        self.db.close()

    def test_filter_uses_all_slugs_not_just_the_first(self):
        rows = (
            self.db.query(PromptTemplate)
            .filter(inspiration.slug_filter("ecommerce-main-image"))
            .all()
        )
        self.assertEqual({r.name for r in rows}, {"Studio Can Shot", "Perfume Packshot"})

    def test_slug_filter_does_not_match_prefixes(self):
        """product-marketing 不能命中 product-marketing-extra 这类前缀。"""
        _entry(self.db, 4, "Other", ["product-marketing-extra"], "x")
        self.db.commit()
        rows = (
            self.db.query(PromptTemplate)
            .filter(inspiration.slug_filter("product-marketing"))
            .all()
        )
        self.assertNotIn("Other", {r.name for r in rows})

    def test_digest_returns_id_and_title_only(self):
        """通览清单只发标题：正文全给会让单次调用涨到几十万 token。"""
        digest = inspiration.category_digest(self.db, "product-marketing")
        self.assertEqual(len(digest), 3)
        for item in digest:
            self.assertEqual(set(item), {"id", "title"})

    def test_fetch_references_keeps_model_order_and_caps_count(self):
        rows = self.db.query(PromptTemplate).order_by(PromptTemplate.id).all()
        ids = [rows[2].id, rows[0].id]           # 故意倒序
        refs = inspiration.fetch_references(self.db, ids)
        self.assertEqual([r["title"] for r in refs], ["Anime Poster", "Studio Can Shot"])

    def test_fetch_references_ignores_unknown_ids(self):
        refs = inspiration.fetch_references(self.db, [99999])
        self.assertEqual(refs, [])

    def test_fetch_references_empty_input(self):
        self.assertEqual(inspiration.fetch_references(self.db, []), [])

    def test_digest_can_reach_newly_synced_entries(self):
        """超额分类必须能选到新同步进来的条目。

        上游每天更新两次，新条目 id 总是最大。以前按 id 升序取前 N 条，
        「产品营销」这种超额分类里新条目永远进不了候选——同步了也白同步。
        """
        for pid in range(10, 20):
            _entry(self.db, pid, "Packshot %d" % pid,
                   ["product-marketing"], "packshot on seamless background")
        self.db.commit()
        newest = self.db.query(PromptTemplate).order_by(PromptTemplate.id.desc()).first()

        original = inspiration.DIGEST_LIMIT
        inspiration.DIGEST_LIMIT = 3
        try:
            seen = set()
            for _ in range(50):
                digest = inspiration.category_digest(self.db, "product-marketing")
                self.assertEqual(len(digest), 3)          # 上限仍然生效
                seen.update(d["id"] for d in digest)
            self.assertIn(newest.id, seen)                # 最新的一条够得着
            self.assertGreater(len(seen), 3)              # 也不再是固定的同一批
        finally:
            inspiration.DIGEST_LIMIT = original

    def test_digest_prefilters_oversized_categories(self):
        """上万条的分类要先按「像不像商品摄影」预筛，否则一次调用几十万 token。"""
        original = inspiration.DIGEST_LIMIT
        inspiration.DIGEST_LIMIT = 2
        try:
            digest = inspiration.category_digest(self.db, "product-marketing")
            self.assertEqual(len(digest), 2)
            # 预筛应留下商品摄影那两条，而不是动漫海报
            titles = {d["title"] for d in digest}
            self.assertNotIn("Anime Poster", titles)
        finally:
            inspiration.DIGEST_LIMIT = original


class SyncProxyTests(unittest.TestCase):
    """同步代理只能作用于灵感库下载，绝不能影响生图网关。"""

    def test_no_proxy_does_not_inject_our_proxy(self):
        """留空时不能注入代理——否则内网环境下同步会被莫名其妙地导去代理。"""
        plain = inspiration._build_opener("")
        configured = inspiration._build_opener("http://127.0.0.1:7890")

        def injected(opener):
            return {
                h.proxies.get("https")
                for h in opener.handlers
                if isinstance(h, urllib.request.ProxyHandler)
            }

        self.assertNotIn("http://127.0.0.1:7890", injected(plain))
        self.assertIn("http://127.0.0.1:7890", injected(configured))

    def test_proxy_is_applied_to_both_schemes(self):
        opener = inspiration._build_opener("http://127.0.0.1:7890")
        proxies = [h.proxies for h in opener.handlers
                   if isinstance(h, urllib.request.ProxyHandler)]
        self.assertTrue(any(p.get("https") == "http://127.0.0.1:7890" for p in proxies))
        self.assertTrue(any(p.get("http") == "http://127.0.0.1:7890" for p in proxies))

    def test_blank_proxy_variants_are_treated_as_direct(self):
        for value in ("", "   ", None):
            opener = inspiration._build_opener(value)
            injected = [h.proxies for h in opener.handlers
                        if isinstance(h, urllib.request.ProxyHandler)
                        and h.proxies.get("https") == value]
            self.assertEqual(injected, [])


if __name__ == "__main__":
    unittest.main()
