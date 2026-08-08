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


if __name__ == "__main__":
    unittest.main()
