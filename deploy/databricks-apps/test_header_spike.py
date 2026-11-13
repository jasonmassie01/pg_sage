"""Tests for the databricks-09 header-capture spike App (header-spike/app.py)."""
import base64
import importlib.util
import json
import threading
import unittest
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent / "header-spike"


def load_spike():
    spec = importlib.util.spec_from_file_location("header_spike", ROOT / "app.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


spike = load_spike()


def b64(obj):
    raw = json.dumps(obj).encode()
    return base64.urlsafe_b64encode(raw).decode().rstrip("=")


SECRET_SIG = "c2lnbmF0dXJlLXRoYXQtbXVzdC1ub3QtbGVhaw"
TOKEN = ".".join([b64({"alg": "RS256", "kid": "k1"}),
                  b64({"sub": "6a1f3e2b", "iss": "https://ws.example/oidc",
                       "client_id": "6a1f3e2b", "email": "hidden@example.com",
                       "secret_claim": "do-not-echo"}),
                  SECRET_SIG])


class CaptureTest(unittest.TestCase):
    def test_identity_headers_are_reported_with_every_value(self):
        report = spike.capture([("X-Forwarded-Email", "forged@example.invalid"),
                                ("x-forwarded-email", "real@example.com"),
                                ("X-Forwarded-User", "123@456")])
        self.assertEqual(report["x-forwarded-email"]["count"], 2)
        self.assertEqual(report["x-forwarded-email"]["values"],
                         ["forged@example.invalid", "real@example.com"])
        self.assertEqual(report["x-forwarded-user"], {"count": 1, "values": ["123@456"]})

    def test_secret_headers_never_echo_their_value(self):
        report = spike.capture([("Authorization", "Bearer " + TOKEN),
                                ("X-Forwarded-Access-Token", TOKEN),
                                ("Cookie", "session=abc123"),
                                ("X-Api-Secret", "s3cr3t")])
        text = json.dumps(report)
        for leaked in (TOKEN, SECRET_SIG, "abc123", "s3cr3t", "do-not-echo"):
            self.assertNotIn(leaked, text)
        token = report["x-forwarded-access-token"]
        self.assertEqual(token["count"], 1)
        self.assertEqual(token["values"][0]["length"], len(TOKEN))
        self.assertEqual(token["values"][0]["claims"]["sub"], "6a1f3e2b")
        self.assertEqual(token["values"][0]["claims"]["client_id"], "6a1f3e2b")
        self.assertNotIn("secret_claim", token["values"][0]["claims"])
        self.assertEqual(report["authorization"]["values"][0]["scheme"], "Bearer")
        self.assertEqual(report["authorization"]["values"][0]["claims"]["iss"],
                         "https://ws.example/oidc")
        self.assertEqual(report["cookie"]["values"][0], {"length": 14, "claims": None})

    def test_claims_keep_only_identity_fields(self):
        claims = spike.jwt_claims(TOKEN)
        self.assertEqual(set(claims), {"sub", "iss", "client_id", "email"})

    def test_malformed_tokens_report_no_claims(self):
        for raw in ("", "not-a-jwt", "a.b.c", "a.%%%.c", "a." + b64([1, 2]) + ".c",
                    "a." + base64.urlsafe_b64encode(b"\xff\xfe").decode() + ".c"):
            with self.subTest(raw=raw):
                self.assertIsNone(spike.jwt_claims(raw))

    def test_no_headers_is_an_empty_report(self):
        self.assertEqual(spike.capture([]), {})

    def test_overlong_values_are_cut(self):
        report = spike.capture([("X-Forwarded-Email", "a" * 5000)])
        self.assertEqual(len(report["x-forwarded-email"]["values"][0]), spike.MAX_VALUE)


class ServerTest(unittest.TestCase):
    def test_server_echoes_capture_as_json_without_caching(self):
        server = spike.make_server("127.0.0.1", 0)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        port = server.server_address[1]
        request = urllib.request.Request(
            f"http://127.0.0.1:{port}/mcp?probe=1", method="POST", data=b"{}",
            headers={"X-Forwarded-Email": "sp@example.com",
                     "X-Forwarded-Access-Token": TOKEN})
        with urllib.request.urlopen(request, timeout=5) as response:
            self.assertEqual(response.headers["Cache-Control"], "no-store")
            body = json.loads(response.read())
        self.assertEqual(body["method"], "POST")
        self.assertEqual(body["path"], "/mcp")
        self.assertEqual(body["headers"]["x-forwarded-email"]["values"], ["sp@example.com"])
        self.assertNotIn(TOKEN, json.dumps(body))


if __name__ == "__main__":
    unittest.main()
