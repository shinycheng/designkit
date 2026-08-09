"""灵感库变量语法转换与输入图预处理的单元测试（不联网、不花钱）。"""
import io
import unittest

from PIL import Image

from backend.app.services.imaging import parse_ratio, prepare_input_image
from backend.app.services.inspiration import _sanitize_var_name, convert_prompt


class ConvertPromptTests(unittest.TestCase):
    def test_basic_argument(self):
        text, variables = convert_prompt('A {argument name="hair color" default="silver"} cat')
        self.assertEqual(text, "A {hair_color} cat")
        self.assertEqual(variables, [{
            "name": "hair_color", "label": "hair color", "type": "text",
            "default": "silver", "required": False,
        }])

    def test_same_label_reused(self):
        text, variables = convert_prompt(
            '{argument name="tone" default="warm"} and {argument name="tone"}')
        self.assertEqual(text, "{tone} and {tone}")
        self.assertEqual(len(variables), 1)

    def test_sanitized_collision_gets_suffix(self):
        text, variables = convert_prompt(
            '{argument name="Main-Color" default="a"} vs {argument name="main color" default="b"}')
        names = [v["name"] for v in variables]
        self.assertEqual(names, ["main_color", "main_color_2"])
        self.assertIn("{main_color}", text)
        self.assertIn("{main_color_2}", text)

    def test_chinese_label_kept(self):
        text, variables = convert_prompt('{argument name="背景色" default="米白"}')
        self.assertEqual(text, "{背景色}")
        self.assertEqual(variables[0]["default"], "米白")

    def test_no_arguments_passthrough(self):
        text, variables = convert_prompt("Plain prompt with {curly} braces")
        self.assertEqual(text, "Plain prompt with {curly} braces")
        self.assertEqual(variables, [])

    def test_sanitize_edge_cases(self):
        self.assertEqual(_sanitize_var_name("  Hair Color!! "), "hair_color")
        self.assertEqual(_sanitize_var_name("123abc"), "v_123abc")
        self.assertEqual(_sanitize_var_name("???"), "var")


class ImagingTests(unittest.TestCase):
    @staticmethod
    def _png(width, height, mode="RGB", color=(200, 60, 60)):
        im = Image.new(mode, (width, height), color)
        buf = io.BytesIO()
        im.save(buf, "PNG")
        return buf.getvalue()

    @staticmethod
    def _jpeg(width, height, quality=85):
        im = Image.new("RGB", (width, height), (180, 120, 90))
        buf = io.BytesIO()
        im.save(buf, "JPEG", quality=quality)
        return buf.getvalue()

    def test_parse_ratio(self):
        self.assertEqual(parse_ratio("1536x1024"), (1536, 1024))
        self.assertIsNone(parse_ratio("auto"))
        self.assertIsNone(parse_ratio("nonsense"))

    def test_pad_to_square(self):
        prepared = prepare_input_image(self._png(600, 300), "1024x1024", True, ".png")
        with Image.open(io.BytesIO(prepared.data)) as im:
            self.assertEqual(im.size, (600, 600))
        self.assertIn("补边", prepared.note)

    def test_alpha_flattened_to_white(self):
        data = self._png(100, 100, mode="RGBA", color=(0, 0, 0, 0))  # 全透明
        prepared = prepare_input_image(data, "auto", True, ".png")
        with Image.open(io.BytesIO(prepared.data)) as im:
            self.assertEqual(im.mode, "RGB")
            self.assertEqual(im.getpixel((50, 50)), (255, 255, 255))  # 不是黑块

    def test_normalize_disabled_keeps_ratio(self):
        prepared = prepare_input_image(self._png(600, 300), "1024x1024", False, ".png")
        with Image.open(io.BytesIO(prepared.data)) as im:
            self.assertEqual(im.size, (600, 300))

    def test_noop_returns_original_bytes(self):
        # 无透明、比例已合、尺寸合规的 JPEG：原样直传，不重编码（避免 PNG 膨胀数倍）
        data = self._jpeg(1024, 1024)
        prepared = prepare_input_image(data, "1024x1024", True, ".jpg")
        self.assertEqual(prepared.data, data)
        self.assertEqual(prepared.mime, "image/jpeg")
        self.assertEqual(prepared.note, "无需处理")

    def test_processed_jpeg_stays_jpeg(self):
        prepared = prepare_input_image(self._jpeg(600, 300), "1024x1024", True, ".jpg")
        self.assertEqual(prepared.mime, "image/jpeg")
        with Image.open(io.BytesIO(prepared.data)) as im:
            self.assertEqual(im.format, "JPEG")
            self.assertEqual(im.size, (600, 600))

    def test_exif_orientation_applied(self):
        # Orientation=6（顺时针转 90 度）：400x300 像素实际是竖图，摆正后应为 300x400
        im = Image.new("RGB", (400, 300), (10, 120, 200))
        exif = im.getexif()
        exif[0x0112] = 6
        buf = io.BytesIO()
        im.save(buf, "JPEG", exif=exif.tobytes())
        prepared = prepare_input_image(buf.getvalue(), "auto", True, ".jpg")
        with Image.open(io.BytesIO(prepared.data)) as out:
            self.assertEqual(out.size, (300, 400))

    def test_bad_bytes_fall_back(self):
        prepared = prepare_input_image(b"not-an-image", "1024x1024", True, ".jpg")
        self.assertEqual(prepared.data, b"not-an-image")
        self.assertEqual(prepared.mime, "image/jpeg")  # 回退时按原扩展名标 MIME
        self.assertIn("失败", prepared.note)


if __name__ == "__main__":
    unittest.main()
