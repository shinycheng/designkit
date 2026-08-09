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
            # 各家网关拒绝多图的真实措辞，缺一条降级路径就对该模型形同虚设
            "You must provide n=1 for this model.",
            "Only n=1 is supported for gpt-image-1",
            "n must be 1 for this model",
        ):
            self.assertTrue(provider._rejects_multi_image(message), message)
        for message in (
            "Invalid value for quality.",
            "Unknown parameter: response_format",
            "Your prompt contains banned words",
            "Unsupported parameter: 'size'.",
            "Rate limit reached, retry in 10 seconds",
            "Image must be at least 1024 pixels",
        ):
            self.assertFalse(provider._rejects_multi_image(message), message)

    def test_error_param_triggers_fallback_even_with_unfamiliar_wording(self):
        """网关文案不认识时，OpenAI 风格的 error.param 仍应触发降级。"""
        _FakeClient.responses = [
            (400, {"error": {"message": "该模型一次只能出一张图", "param": "n"}}),
            (200, {"data": [{"b64_json": base64.b64encode(b"p1").decode()}]}),
            (200, {"data": [{"b64_json": base64.b64encode(b"p2").decode()}]}),
        ]
        with patch.object(provider.httpx, "Client", _FakeClient):
            images = provider.generate_images(
                self._openai_settings(), "prompt", [], 2, "1024x1024", None
            )
        self.assertEqual(images, [b"p1", b"p2"])


class GatewayErrorReadabilityTests(unittest.TestCase):
    """网关报错要能让非技术用户看懂该怎么办，而不是「没有返回任何图片」。"""

    def _resp(self, status, *, json_body=None, text=None):
        if json_body is not None:
            return httpx.Response(status, json=json_body,
                                  request=httpx.Request("POST", "https://x.test"))
        return httpx.Response(status, text=text or "",
                              request=httpx.Request("POST", "https://x.test"))

    def test_business_envelope_surfaces_real_reason(self):
        """HTTP 200 + {code,msg} 是国内中转网关的常见形态，必须透出 msg。"""
        resp = self._resp(200, json_body={"code": 500, "msg": "余额不足"})
        with self.assertRaises(provider.ProviderError) as ctx:
            provider._decode_response(resp, 5)
        self.assertIn("余额不足", str(ctx.exception))

    def test_success_envelope_with_code_is_not_treated_as_failure(self):
        """成功响应也可能带 code=200/'0'，不能因为有 code 就把图丢掉。"""
        payload = base64.b64encode(b"img").decode()
        for code in (0, 200, "0", "success"):
            resp = self._resp(200, json_body={"code": code, "data": [{"b64_json": payload}]})
            self.assertEqual(provider._decode_response(resp, 5), [b"img"])

    def test_alternate_image_field_names(self):
        """部分网关把图放在 images / results 而不是 data。"""
        payload = base64.b64encode(b"img").decode()
        for key in ("images", "results", "output"):
            resp = self._resp(200, json_body={key: [{"b64_json": payload}]})
            self.assertEqual(provider._decode_response(resp, 5), [b"img"])

    def test_unknown_shape_lists_top_level_fields(self):
        """异步任务式网关返回 task_id：要说清是「形态不对」而非「没图」。"""
        resp = self._resp(200, json_body={"task_id": "abc", "status": "pending"})
        with self.assertRaises(provider.ProviderError) as ctx:
            provider._decode_response(resp, 5)
        message = str(ctx.exception)
        self.assertIn("task_id", message)
        self.assertIn("status", message)

    def test_html_error_page_is_summarised(self):
        """网关挂掉时会返回整页 HTML，不能把标签原样糊给用户。"""
        html = "<!DOCTYPE html><html><head><title>502 Bad Gateway</title></head><body>" + "x" * 500
        resp = self._resp(502, text=html)
        message = provider._friendly_error(resp)
        self.assertNotIn("<html", message.lower())
        self.assertIn("502", message)

    def test_status_hint_is_actionable(self):
        """401/429 要给出「去哪儿做什么」，不是只报状态码。"""
        self.assertIn("API Key", provider._friendly_error(self._resp(401, text="")))
        self.assertIn("额度", provider._friendly_error(self._resp(429, text="")))

    def test_body_reason_outranks_status_hint(self):
        """有业务原因时以它为准——「余额不足」比「网关内部错误」有用得多。"""
        resp = self._resp(500, json_body={"error": {"message": "insufficient balance"}})
        self.assertIn("insufficient balance", provider._friendly_error(resp))

    def test_plain_string_urls_are_accepted(self):
        """少数网关直接给 URL 字符串数组。"""
        resp = self._resp(200, json_body={"data": [base64.b64encode(b"img").decode()]})
        self.assertEqual(provider._decode_response(resp, 5), [b"img"])


class TransparentBackgroundCompatTests(unittest.TestCase):
    """透明底的兼容判定：判错会把已付费的图丢光，判漏会让用户以为拿到了透明底。"""

    def _resp(self, status, body):
        return httpx.Response(status, json=body,
                              request=httpx.Request("POST", "https://x.test"))

    def test_prompt_echo_is_not_mistaken_for_param_rejection(self):
        """透明模式的提示词必然含 "transparent background"。网关把提示词回显进
        错误体时，不能因此判定成「网关不支持 background 参数」。"""
        resp = self._resp(400, {"error": {
            "message": "Your prompt was rejected: 'Compose a square 1:1 image. "
                       "A dryer on a fully transparent background ...' violates policy",
        }})
        self.assertFalse(provider._rejects_background(resp))

    def test_real_param_rejection_is_detected(self):
        for message in (
            "Unknown parameter: 'background'.",
            "Unsupported parameter: background",
            "Invalid value for parameter background",
        ):
            resp = self._resp(400, {"error": {"message": message}})
            self.assertTrue(provider._rejects_background(resp), message)

    def test_error_param_field_is_authoritative(self):
        resp = self._resp(400, {"error": {"message": "bad request", "param": "background"}})
        self.assertTrue(provider._rejects_background(resp))

    def test_partial_results_survive_a_background_rejection_mid_stream(self):
        """逐张降级途中抛出的兼容性错误，绝不能把已经出好、已经付过钱的图丢掉。"""
        payload = base64.b64encode(b"paid").decode()
        _FakeClient.responses = [
            (400, {"error": {"message": "Unknown parameter: 'tools[0].n'."}}),  # 触发逐张降级
            (200, {"data": [{"b64_json": payload}]}),                            # 第 1 张，已计费
            (200, {"data": [{"b64_json": payload}]}),                            # 第 2 张，已计费
            (400, {"error": {"message": "Unknown parameter: 'background'."}}),   # 第 3 张才发现不支持
        ]
        settings = {
            "provider": "openai",
            "openai_base_url": "https://images.example.test",
            "openai_api_key": "test-key",
            "image_model": "gpt-image-1",
            "request_timeout": 5,
            "image_background": "transparent",
        }
        with patch.object(provider.httpx, "Client", _FakeClient):
            images = provider.generate_images(settings, "prompt", [], 3, "1024x1024", None)
        self.assertEqual(images, [b"paid", b"paid"], "已付费的图被丢弃了")


class ApiBaseTests(unittest.TestCase):
    """地址拼接：填错一个结尾就是 404，而报错只说「连不上」。"""

    def _base(self, url):
        return provider._api_base({"openai_base_url": url})

    def test_appends_v1_when_missing(self):
        self.assertEqual(self._base("https://gw.test"), "https://gw.test/v1")
        self.assertEqual(self._base("https://gw.test/"), "https://gw.test/v1")

    def test_keeps_existing_v1(self):
        self.assertEqual(self._base("https://gw.test/v1"), "https://gw.test/v1")

    def test_does_not_double_up_other_version_prefixes(self):
        """火山方舟这类自带 /api/v3 的地址，以前会被拼成 /api/v3/v1/... 直接 404。"""
        self.assertEqual(self._base("https://ark.test/api/v3"), "https://ark.test/api/v3")
        self.assertEqual(self._base("https://ark.test/api/plan/v3"), "https://ark.test/api/plan/v3")

    def test_missing_base_raises_actionable_error(self):
        with self.assertRaises(provider.ProviderError) as ctx:
            self._base("")
        self.assertIn("系统设置", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
