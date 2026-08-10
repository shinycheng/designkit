"""出图比例护栏的单测。不联网、不花钱。

这块代码管着「哪些尺寸能提交」和「提示词里怎么描述画幅」，
一旦放行了模型画不好的尺寸，或者画幅措辞和实际尺寸对不上，
用户拿到的是构图诡异或比例错误的图——而且要到上架时才发现。
"""
import atexit
import os
import shutil
import tempfile
import unittest

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

from backend.app.services import prompt_studio, sizing


class SizeValidationTests(unittest.TestCase):
    def test_all_presets_pass_their_own_guardrails(self):
        """预设表自己必须过得了护栏，否则界面能选、提交就 422。"""
        for preset in sizing.PRESETS:
            self.assertIsNone(
                sizing.validate_size(preset["value"]),
                "预设 %s 没通过护栏" % preset["value"],
            )

    def test_preset_declared_ratio_matches_computed(self):
        """预设声明的比例要和像素反算出来的一致，否则提示词会写错画幅。"""
        for preset in sizing.PRESETS:
            if preset["value"] == sizing.AUTO:
                continue
            self.assertEqual(
                preset["ratio"], sizing.ratio_label(preset["value"]),
                "预设 %s 声明 %s 与反算不符" % (preset["value"], preset["ratio"]),
            )

    def test_auto_is_allowed_and_unconstrained(self):
        self.assertIsNone(sizing.validate_size("auto"))
        self.assertIsNone(sizing.ratio_label("auto"))
        self.assertIsNone(sizing.orientation("auto"))

    def test_rejects_non_multiple_of_16(self):
        self.assertIn("16", sizing.validate_size("800x600") or "")
        self.assertIn("16", sizing.validate_size("1000x1000") or "")

    def test_rejects_extreme_aspect(self):
        self.assertIn("比例", sizing.validate_size("512x2048") or "")

    def test_rejects_out_of_range_edges(self):
        self.assertIn("最长边", sizing.validate_size("4096x1024") or "")
        self.assertIn("最短边", sizing.validate_size("256x256") or "")

    def test_rejects_garbage(self):
        for bad in ("", "abc", "1024", "1024xabc", "-16x-16", "0x0"):
            self.assertIsNotNone(sizing.validate_size(bad), bad)


class RatioResolutionTests(unittest.TestCase):
    def test_common_ecommerce_ratios_resolve_to_valid_sizes(self):
        for ratio in ("1:1", "3:4", "4:5", "2:3", "9:16", "16:9", "4:3", "3:2"):
            size = sizing.resolve_ratio(ratio)
            self.assertIsNotNone(size, "%s 应能算出尺寸" % ratio)
            self.assertIsNone(sizing.validate_size(size), "%s -> %s 未过护栏" % (ratio, size))
            self.assertEqual(sizing.ratio_label(size), ratio)

    def test_extreme_ratios_are_refused_not_clamped(self):
        """过于狭长的比例应返回 None，而不是硬算出一个模型画不好的尺寸。"""
        for ratio in ("21:9", "1:4", "9:21"):
            self.assertIsNone(sizing.resolve_ratio(ratio), ratio)

    def test_orientation(self):
        self.assertEqual(sizing.orientation("1024x1024"), "square")
        self.assertEqual(sizing.orientation("1536x1024"), "landscape")
        self.assertEqual(sizing.orientation("1024x1536"), "portrait")


class FramingWordingTests(unittest.TestCase):
    """画幅措辞是控制出图比例最有效的手段（实测能压过输入图比例）。"""

    def test_every_preset_gets_a_framing_phrase(self):
        for preset in sizing.PRESETS:
            label = prompt_studio.framing_label(preset["value"])
            if preset["value"] == sizing.AUTO:
                self.assertIsNone(label)
                continue
            self.assertIsNotNone(label, preset["value"])
            self.assertIn(preset["ratio"], label)
            self.assertRegex(label, r"^a (square|landscape|portrait) ")

    def test_enforce_aspect_pins_head_and_tail(self):
        out = prompt_studio.enforce_aspect("A hair dryer.", "1024x1360")
        self.assertTrue(out.startswith("Compose a portrait 3:4 image."))
        self.assertTrue(out.rstrip().endswith("The final image must be a portrait 3:4 image."))

    def test_enforce_aspect_is_idempotent(self):
        """补图任务会拿已加工过的提示词再跑一遍，不能越叠越长。"""
        once = prompt_studio.enforce_aspect("A hair dryer.", "1024x1360")
        twice = prompt_studio.enforce_aspect(once, "1024x1360")
        self.assertEqual(once, twice)

    def test_auto_adds_no_constraint(self):
        self.assertEqual(prompt_studio.enforce_aspect("A dryer.", "auto"), "A dryer.")


class TransparentBackgroundWordingTests(unittest.TestCase):
    """只发 background 参数不够——模板里的白底措辞会让模型照样画白。"""

    def test_removes_white_backdrop_wording(self):
        for text in (
            "A dryer on a pure seamless white background, studio lighting.",
            "Centered on a clean white backdrop with soft shadow.",
            "Minimalist light grey background, hero shot.",
            "Shot against a solid beige backdrop.",
        ):
            out = prompt_studio.enforce_transparent_background(text)
            body = out.split(" Output the subject")[0]
            self.assertNotRegex(body.lower(), r"(white|grey|gray|beige)\s+(background|backdrop)")
            # 替换不能把前后词粘在一起
            self.assertNotRegex(body, r"[a-zA-Z]transparent")

    def test_appends_transparency_requirement(self):
        out = prompt_studio.enforce_transparent_background("A dryer on a marble table.")
        self.assertIn("transparent background", out.lower())
        self.assertIn("alpha channel", out.lower())

    def test_is_idempotent(self):
        once = prompt_studio.enforce_transparent_background("A dryer on a white background.")
        twice = prompt_studio.enforce_transparent_background(once)
        self.assertEqual(once, twice)


if __name__ == "__main__":
    unittest.main()
