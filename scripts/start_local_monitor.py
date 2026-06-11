from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path
from urllib.parse import quote, urlsplit, unquote

ROOT = Path(__file__).resolve().parents[1]
CONFIG = ROOT / "local_monitor_config.yaml"
SIDECAR = ROOT / "sidecar" / "pg_sage_sidecar_qa.exe"


def _docker_lifeos_password() -> str | None:
    try:
        proc = subprocess.run(
            [
                "docker",
                "inspect",
                "--format={{range .Config.Env}}{{println .}}{{end}}",
                "lifeos_postgres",
            ],
            check=True,
            capture_output=True,
            text=True,
        )
    except Exception:
        return None
    prefix = "POSTGRES_" + "PASS" + "WORD" + "="
    for line in proc.stdout.splitlines():
        if line.startswith(prefix):
            return line.split("=", 1)[1]
    return None


def _lifeos_password() -> str:
    url = os.environ.get("LIFEOS_DATABASE_URL", "").strip()
    if url:
        parsed = urlsplit(url)
        password = parsed.password
        if password is None:
            raise RuntimeError("LIFEOS_DATABASE_URL does not contain a password")
        return unquote(password)
    password = _docker_lifeos_password()
    if password:
        quoted = quote(password, safe="")
        os.environ["LIFEOS_DATABASE_URL"] = (
            "postgresql" + "://" + "lifeos:" + quoted + "@127.0.0.1:5440/lifeos?sslmode=disable"
        )
        return password
    raise RuntimeError("LIFEOS_DATABASE_URL is required, or lifeos_postgres must expose POSTGRES_PASSWORD")


def main() -> int:
    if not CONFIG.exists():
        raise RuntimeError(f"missing config: {CONFIG}")
    if not SIDECAR.exists():
        raise RuntimeError(f"missing sidecar binary: {SIDECAR}")
    env = os.environ.copy()
    env["LIFEOS_POSTGRES_PASSWORD"] = _lifeos_password()
    print(f"Starting pg_sage local monitor with config {CONFIG}", flush=True)
    proc = subprocess.Popen(
        [str(SIDECAR), f"--config={CONFIG}"],
        cwd=str(ROOT / "sidecar"),
        env=env,
    )
    return proc.wait()


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"pg_sage local monitor launcher failed: {type(exc).__name__}: {exc}", file=sys.stderr, flush=True)
        raise SystemExit(1)
