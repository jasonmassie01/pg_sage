"""Header-capture App for the databricks-09 service-principal spike.

Deploy it as a separate, temporary Databricks App (never beside pg_sage) and
call it as a user and as a service principal. Each response reports which
request headers reached the App, how many values each had and, for identity
headers, the values themselves. Credential headers are never echoed: the
report gives their length and, for a JWT, a fixed set of identity claims
(never the signature or any other claim). Nothing is logged except the
method and path, and nothing is stored.
"""
import base64
import binascii
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

MAX_VALUE = 512
SECRET_MARKERS = ("authorization", "token", "cookie", "secret", "password", "key")
CLAIMS = ("sub", "iss", "aud", "azp", "client_id", "email", "scope", "exp", "iat")


def is_secret(name):
    """A header whose value is a credential: report its shape only."""
    return any(marker in name for marker in SECRET_MARKERS)


def jwt_claims(raw):
    """Return the identity claims of an unverified JWT, or None."""
    parts = raw.split(".")
    if len(parts) != 3:
        return None
    payload = parts[1] + "=" * (-len(parts[1]) % 4)
    try:
        claims = json.loads(base64.urlsafe_b64decode(payload.encode("ascii")))
    except (binascii.Error, UnicodeError, ValueError):
        return None
    if not isinstance(claims, dict):
        return None
    return {name: claims[name] for name in CLAIMS if name in claims}


def secret_shape(raw):
    """Describe a credential value without revealing it."""
    scheme, _, rest = raw.partition(" ")
    if rest and scheme.lower() in ("bearer", "basic"):
        return {"scheme": scheme, "length": len(rest), "claims": jwt_claims(rest)}
    return {"length": len(raw), "claims": jwt_claims(raw)}


def capture(items):
    """Group (name, value) pairs by lower-cased name, redacting credentials."""
    report = {}
    for name, value in items:
        key = name.lower()
        entry = report.setdefault(key, {"count": 0, "values": []})
        entry["count"] += 1
        entry["values"].append(secret_shape(value) if is_secret(key) else value[:MAX_VALUE])
    return report


class Handler(BaseHTTPRequestHandler):
    def _reply(self):
        body = json.dumps({"method": self.command, "path": urlsplit(self.path).path,
                           "headers": capture(self.headers.items())}, indent=2).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    do_GET = do_POST = do_PUT = do_DELETE = _reply

    def log_message(self, format, *args):
        # The default access log prints the request line; print only the
        # method and path so no query string or identity reaches the log.
        print(self.command, urlsplit(self.path).path, flush=True)


def make_server(host, port):
    return ThreadingHTTPServer((host, port), Handler)


if __name__ == "__main__":
    port = int(os.environ.get("DATABRICKS_APP_PORT", "8000"))
    make_server("0.0.0.0", port).serve_forever()
