from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path
from urllib.parse import quote, urlsplit, unquote

ROOT = Path(__file__).resolve().parents[1]
CONFIG = ROOT / "local_monitor_config.yaml"
SIDECAR = ROOT / "sidecar" / "pg_sage_sidecar_qa.exe"


def _docker_postgres_password(container: str) -> str | None:
    try:
        proc = subprocess.run(
            [
                "docker",
                "inspect",
                "--format={{range .Config.Env}}{{println .}}{{end}}",
                container,
            ],
            check=True,
            capture_output=True,
            text=True,
            timeout=10,
        )
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as exc:
        print(f"Docker password lookup failed: {type(exc).__name__}", file=sys.stderr)
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
    password = _docker_postgres_password("lifeos_postgres")
    if password:
        quoted = quote(password, safe="")
        os.environ["LIFEOS_DATABASE_URL"] = (
            "postgresql" + "://" + "lifeos:" + quoted + "@127.0.0.1:5440/lifeos?sslmode=disable"
        )
        return password
    raise RuntimeError("LIFEOS_DATABASE_URL is required, or lifeos_postgres must expose POSTGRES_PASSWORD")


def _load_local_fleet_credentials(env: dict[str, str]) -> None:
    configured = CONFIG.read_text(encoding="utf-8")
    targets = {
        "FLEET_PG1_PASSWORD": "pg_sage-pg-target-1",
        "FLEET_PG2_PASSWORD": "pg_sage-pg-target-2-1",
    }
    for key, container in targets.items():
        if "${" + key + "}" not in configured or env.get(key):
            continue
        password = _docker_postgres_password(container)
        if not password:
            raise RuntimeError(f"{key} is required or {container} must expose POSTGRES_PASSWORD")
        env[key] = password


def main() -> int:
    if not CONFIG.exists():
        raise RuntimeError(f"missing config: {CONFIG}")
    if not SIDECAR.exists():
        raise RuntimeError(f"missing sidecar binary: {SIDECAR}")
    password = _lifeos_password()
    env = os.environ.copy()
    env["LIFEOS_POSTGRES_PASSWORD"] = password
    _load_local_fleet_credentials(env)
    log_dir = ROOT / "logs"
    log_dir.mkdir(exist_ok=True)
    # Supervisor loop: the sidecar exits with RESTART_EXIT_CODE (42) when the
    # /api/v1/restart endpoint is used (so startup-only settings take effect).
    # Relaunch on that code; any other exit code stops the launcher.
    RESTART_EXIT_CODE = 42
    while True:
        print(
            f"Starting pg_sage local monitor with config {CONFIG}", flush=True
        )
        with (log_dir / "sidecar.runtime.out.log").open("ab") as stdout, \
                (log_dir / "sidecar.runtime.err.log").open("ab") as stderr:
            proc = subprocess.Popen(
                [str(SIDECAR), f"--config={CONFIG}"],
                cwd=str(ROOT / "sidecar"),
                env=env,
                stdout=stdout,
                stderr=stderr,
            )
            code = proc.wait()
        if code != RESTART_EXIT_CODE:
            return code
        print("Restart requested via UI — relaunching sidecar…", flush=True)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError, ValueError) as exc:
        print(f"pg_sage local monitor launcher failed: {type(exc).__name__}: {exc}", file=sys.stderr, flush=True)
        raise SystemExit(1)
