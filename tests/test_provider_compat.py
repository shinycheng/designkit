"""Provider compatibility regressions that do not call a real upstream API."""
import base64
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import httpx

from backend.app.services import provider


class _FakeClient:
    calls = []
    responses = []

    def __init__(self, *args, **kwargs):
        pass

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, traceback):
        return False

    def post(self, url, *, headers=None, data=None, files=None, json=None):
        payload = data if data is not None else json
        self.__class__.calls.append({"url": url, "payload": dict(payload or {})})
        status, body = self.__class__.responses.pop(0)
        return httpx.Response(
            status,
            json=body,
            request=httpx.Request("POST", url),
        )


class ProviderCompatibilityTests(unittest.TestCase):
    def setUp(self):
        _FakeClient.calls = []
        _FakeClient.responses = []

    def test_multi_image_falls_back_when_gateway_rejects_n(self):
        """A gateway exposing Images API may reject multi-image `n` internally."""
        _FakeClient.responses = [
            (400, {"error": {"message": "Unknown parameter: 'tools[0].n'."}}),
            (200, {"data": [{"b64_json": base64.b64encode(b"first").decode()}]}),
            (200, {"data": [{"b64_json": base64.b64encode(b"second").decode()}]}),
        ]
        settings = {
            "provider": "openai",
            "openai_base_url": "https://images.example.test",
            "openai_api_key": "test-key",
            "image_model": "gpt-image-1",
            "request_timeout": 5,
        }

        with tempfile.TemporaryDirectory() as directory:
            image_path = Path(directory) / "input.png"
            image_path.write_bytes(b"test-image")
            with patch.object(provider.httpx, "Client", _FakeClient), patch.object(
                provider, "abs_path", return_value=image_path
            ):
                images = provider.generate_images(
                    settings,
                    "white background product photo",
                    ["uploads/input.png"],
                    2,
                    "1024x1024",
                    "high",
                )

        self.assertEqual(images, [b"first", b"second"])
        self.assertEqual(len(_FakeClient.calls), 3)
        self.assertTrue(
            all(call["url"].endswith("/v1/images/edits") for call in _FakeClient.calls)
        )
        self.assertEqual(_FakeClient.calls[0]["payload"]["n"], "2")
        self.assertEqual(_FakeClient.calls[1]["payload"]["n"], "1")
        self.assertEqual(_FakeClient.calls[2]["payload"]["n"], "1")

    def test_unrelated_bad_request_does_not_trigger_multi_image_fallback(self):
        _FakeClient.responses = [
            (400, {"error": {"message": "Invalid value for quality."}}),
        ]
        settings = {
            "provider": "openai",
            "openai_base_url": "https://images.example.test",
            "openai_api_key": "test-key",
            "image_model": "gpt-image-1",
            "request_timeout": 5,
        }

        with patch.object(provider.httpx, "Client", _FakeClient):
            with self.assertRaisesRegex(provider.ProviderError, "Invalid value for quality"):
                provider.generate_images(
                    settings,
                    "white background product photo",
                    [],
                    2,
                    "1024x1024",
                    "high",
                )

        self.assertEqual(len(_FakeClient.calls), 1)

    def test_single_image_keeps_standard_images_api_request(self):
        _FakeClient.responses = [
            (200, {"data": [{"b64_json": base64.b64encode(b"only").decode()}]}),
        ]
        settings = {
            "provider": "openai",
            "openai_base_url": "https://images.example.test",
            "openai_api_key": "test-key",
            "image_model": "gpt-image-1",
            "request_timeout": 5,
        }

        with patch.object(provider.httpx, "Client", _FakeClient):
            images = provider.generate_images(
                settings,
                "white background product photo",
                [],
                1,
                "1024x1024",
                "high",
            )

        self.assertEqual(images, [b"only"])
        self.assertEqual(len(_FakeClient.calls), 1)
        self.assertEqual(_FakeClient.calls[0]["payload"]["n"], 1)

    # ---------------------------------------------------------------- 费用相关

    def _openai_settings(self, model="gpt-image-1", timeout=5):
        return {
            "provider": "openai",
            "openai_base_url": "https://images.example.test",
            "openai_api_key": "test-key",
            "image_model": model,
            "request_timeout": timeout,
        }

    def test_response_format_negotiation_is_reused_across_single_requests(self):
        """协商掉 response_format 后，逐张请求不得再重复踩同一个 400。

        回归的是「每张图先发一次必然失败的请求」导致的请求数翻倍（费用与耗时）。
        """
        _FakeClient.responses = [
            (400, {"error": {"message": "Unknown parameter: response_format"}}),  # 协商
            (400, {"error": {"message": "Unknown parameter: 'tools[0].n'."}}),    # 拒绝多图
            (200, {"data": [{"b64_json": base64.b64encode(b"a").decode()}]}),
            (200, {"data": [{"b64_json": base64.b64encode(b"b").decode()}]}),
            (200, {"data": [{"b64_json": base64.b64encode(b"c").decode()}]}),
        ]
        with patch.object(provider.httpx, "Client", _FakeClient):
            images = provider.generate_images(
                self._openai_settings(model="dall-e-3"), "prompt", [], 3, "1024x1024", None
            )

        self.assertEqual(images, [b"a", b"b", b"c"])
        # 2 次探测 + 3 张实图，一次都不能多
        self.assertEqual(len(_FakeClient.calls), 5)
        with_rf = [c for c in _FakeClient.calls if "response_format" in c["payload"]]
        self.assertEqual(len(with_rf), 1, "response_format 协商结果没有被复用")

    def test_partial_success_is_returned_instead_of_discarded(self):
        """逐张生成中途失败时，已生成（已计费）的图必须返回而不是整批丢弃。"""
        _FakeClient.responses = [
            (400, {"error": {"message": "Unknown parameter: 'tools[0].n'."}}),
            (200, {"data": [{"b64_json": base64.b64encode(b"kept1").decode()}]}),
            (200, {"data": [{"b64_json": base64.b64encode(b"kept2").decode()}]}),
            (500, {"error": {"message": "upstream busy"}}),
        ]
        with patch.object(provider.httpx, "Client", _FakeClient):
            images = provider.generate_images(
                self._openai_settings(), "prompt", [], 3, "1024x1024", None
            )

        self.assertEqual(images, [b"kept1", b"kept2"])

    def test_all_single_requests_failing_still_raises(self):
        """一张都没拿到时必须报错，不能悄悄返回空结果。"""
        _FakeClient.responses = [
            (400, {"error": {"message": "Unknown parameter: 'tools[0].n'."}}),
            (500, {"error": {"message": "gateway down"}}),
        ]
        with patch.object(provider.httpx, "Client", _FakeClient):
            with self.assertRaisesRegex(provider.ProviderError, "gateway down"):
                provider.generate_images(
                    self._openai_settings(), "prompt", [], 3, "1024x1024", None
                )

    def test_stops_as_soon_as_enough_images_collected(self):
        """网关一次返回多张时应立即停手，不再多发（多发即多付费）。"""
        _FakeClient.responses = [
            (400, {"error": {"message": "Unknown parameter: 'tools[0].n'."}}),
            (200, {"data": [
                {"b64_json": base64.b64encode(b"x").decode()},
                {"b64_json": base64.b64encode(b"y").decode()},
            ]}),
        ]
        with patch.object(provider.httpx, "Client", _FakeClient):
            images = provider.generate_images(
                self._openai_settings(), "prompt", [], 2, "1024x1024", None
            )

        self.assertEqual(images, [b"x", b"y"])
        self.assertEqual(len(_FakeClient.calls), 2, "拿够张数后仍继续发请求")

    def test_multi_image_rejection_matcher(self):
        """错误匹配：网关的多种措辞都要能识别，无关的 400 不能误触发。"""
        for message in (
            "Unknown parameter: 'tools[0].n'.",
            "Unsupported parameter: \"n\" is not supported with this model.",
            "Invalid parameter: n",
        ):
            self.assertTrue(provider._rejects_multi_image(message), message)
        for message in (
            "Invalid value for quality.",
            "Unknown parameter: response_format",
            "Your prompt contains banned words",
            "Unsupported parameter: 'size'.",
        ):
            self.assertFalse(provider._rejects_multi_image(message), message)


if __name__ == "__main__":
    unittest.main()
