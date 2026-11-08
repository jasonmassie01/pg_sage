"""Launch the packaged Go server; requires only the Apps Python standard library."""
from __future__ import annotations
from copy import deepcopy
import gzip
import hashlib
import json
import os
from pathlib import Path
import platform
import signal
import subprocess
import sys
import tempfile
import time


def unpack_binary(source: Path, destination: Path) -> Path:
    manifest = json.loads((source / "binary.json").read_text())
    machine = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    if manifest.get("format_version") != 1 or manifest.get("os") != "linux":
        raise ValueError("unsupported binary manifest")
    if platform.system() != "Linux" or manifest.get("architecture") != machine:
        raise ValueError("binary architecture does not match this Linux Apps runtime")
    chunks = manifest["chunks"]
    if not chunks or any(Path(name).name != name or not name.startswith("binary.part-") for name in chunks):
        raise ValueError("invalid binary chunk names")
    binary = gzip.decompress(b"".join((source / name).read_bytes() for name in chunks))
    if len(binary) != manifest["size"] or hashlib.sha256(binary).hexdigest() != manifest["sha256"]:
        raise ValueError("packaged binary checksum or size mismatch")
    executable = destination / "pg_sage"
    executable.write_bytes(binary)
    executable.chmod(0o700)
    return executable


def runtime_config(env: dict[str, str], base: dict | None = None) -> dict:
    if env.get("SAGE_AUTH_MODE") != "databricks_app":
        raise ValueError("Databricks Apps requires SAGE_AUTH_MODE=databricks_app")
    if not env.get("SAGE_DATABRICKS_ADMIN_EMAIL"):
        raise ValueError("provide the explicitly selected Databricks admin email")
    for name in ("DATABRICKS_TOKEN", "DATABRICKS_USERNAME", "DATABRICKS_PASSWORD", "DATABRICKS_CONFIG_PROFILE", "DATABRICKS_CONFIG_FILE"):
        if env.get(name):
            raise ValueError("Databricks Apps OAuth requires removing " + name)
    if env.get("DATABRICKS_AUTH_TYPE", "oauth-m2m") != "oauth-m2m":
        raise ValueError("Databricks Apps requires DATABRICKS_AUTH_TYPE=oauth-m2m")
    env["DATABRICKS_AUTH_TYPE"] = "oauth-m2m"
    if not all(env.get(name) for name in ("DATABRICKS_HOST", "DATABRICKS_CLIENT_ID", "DATABRICKS_CLIENT_SECRET")):
        raise ValueError("Databricks Apps requires platform workspace and service-principal OAuth credentials")
    llm = (base or {}).get("llm", {})
    if any(env.get(name) for name in ("SAGE_LLM_API_KEY", "SAGE_LLM_API_KEY_FILE", "SAGE_OPTIMIZER_LLM_API_KEY", "SAGE_OPTIMIZER_LLM_API_KEY_FILE")) or llm.get("api_key") or llm.get("optimizer_llm", {}).get("api_key"):
        raise ValueError("Databricks Apps forbids AI API keys; use platform OAuth")
    if env.get("SAGE_LLM_AUTH", "databricks") != "databricks" or llm.get("auth", "databricks") != "databricks":
        raise ValueError("Databricks Apps requires SAGE_LLM_AUTH=databricks")
    env["SAGE_LLM_AUTH"] = "databricks"
    if any(env.get(key) for key in ("SAGE_BOOTSTRAP_ADMIN_EMAIL", "SAGE_BOOTSTRAP_ADMIN_PASSWORD", "SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE")):
        raise ValueError("remove local password bootstrap settings from the Databricks Apps deployment")
    port = int(env.get("DATABRICKS_APP_PORT", "8000"))
    if not 1 <= port <= 65535:
        raise ValueError("DATABRICKS_APP_PORT must be between 1 and 65535")
    if env.get("SAGE_TLS_CERT") or env.get("SAGE_TLS_KEY"):
        raise ValueError("Databricks Apps terminates TLS; remove SAGE_TLS_CERT and SAGE_TLS_KEY")
    if not env.get("SAGE_LAKEBASE_ENDPOINT"):
        env["SAGE_LAKEBASE_ENDPOINT"] = env.get("LAKEBASE_ENDPOINT") or env.get("ENDPOINT_NAME", "")
    if not env.get("SAGE_LAKEBASE_ENDPOINT"):
        raise ValueError("set SAGE_LAKEBASE_ENDPOINT to the Lakebase endpoint resource path")
    for suffix in ("HOST", "PORT", "USER", "DATABASE", "SSLMODE"):
        target = "SAGE_PG_" + suffix
        source = "PG" + suffix
        if not env.get(target) and env.get(source):
            env[target] = env[source]
    env.setdefault("SAGE_PG_PORT", "5432")
    env.setdefault("SAGE_PG_SSLMODE", "verify-full")
    if not env.get("SAGE_PG_USER"):
        env["SAGE_PG_USER"] = env.get("DATABRICKS_CLIENT_ID", "")
    if not env.get("SAGE_DATABASE_URL") and not all(env.get("SAGE_PG_" + name) for name in ("HOST", "USER", "DATABASE")):
        raise ValueError("set PGHOST, PGDATABASE and PGUSER (or use the app service principal), or SAGE_DATABASE_URL")
    if not env.get("SAGE_ENCRYPTION_KEY") and not env.get("SAGE_ENCRYPTION_KEY_FILE"):
        raise ValueError("provide a durable SAGE_ENCRYPTION_KEY through an Apps secret resource")
    # No credentials are written to the generated configuration file.
    # Native pg_sage refreshes OAuth credentials for each physical connection.
    config = deepcopy(base or {})
    # The single-database App shares a pool for account verification, control
    # state and collectors. This adds capacity, not reserved auth connections;
    # retain an operator's explicit pool size.
    config.setdefault("postgres", {}).setdefault("max_connections", 5)
    telemetry = config.get("lakebase_observability", {})
    if telemetry.get("enabled") and env.get("DATABRICKS_WAREHOUSE_ID"):
        if telemetry.get("warehouse_id") not in (None, "", env["DATABRICKS_WAREHOUSE_ID"]):
            raise ValueError("Lakebase history warehouse must match the declared App resource")
        telemetry["warehouse_id"] = env["DATABRICKS_WAREHOUSE_ID"]
    config.setdefault("api", {})["listen_addr"] = f"0.0.0.0:{port}"
    return config


def supervise(command: list[str], env: dict[str, str], shutdown_timeout: float = 12.0) -> int:
    """Relaunch explicit pg_sage restarts; forward platform shutdown signals."""
    child = None
    stopping = None
    shutdown_deadline = None
    restarts = []
    child_env = dict(env, SAGE_SUPERVISED="1")

    def forward(signum, _frame):
        nonlocal stopping, shutdown_deadline
        if stopping is None:
            shutdown_deadline = time.monotonic() + shutdown_timeout
        stopping = signum
        if child is not None and child.poll() is None:
            try:
                os.killpg(child.pid, signum)
            except ProcessLookupError:
                pass

    previous = {sig: signal.signal(sig, forward) for sig in (signal.SIGTERM, signal.SIGINT)}
    try:
        while stopping is None:
            child = subprocess.Popen(command, env=child_env, start_new_session=True)
            if stopping is not None:
                forward(stopping, None)
            while True:
                try:
                    status = child.wait(timeout=0.25)
                    break
                except subprocess.TimeoutExpired:
                    if stopping is not None and time.monotonic() >= shutdown_deadline:
                        try:
                            os.killpg(child.pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                        status = child.wait()
                        break
            child = None
            if stopping is not None:
                return 0 if status == 0 else 128 + stopping
            if status != 42:
                return status if status >= 0 else 128 - status
            now = time.monotonic()
            restarts = [started for started in restarts if now - started < 60]
            restarts.append(now)
            if len(restarts) > 5:
                print("[apps-launcher] too many restart requests; exiting", file=sys.stderr, flush=True)
                return 1
            print("[apps-launcher] restart requested; relaunching pg_sage", flush=True)
        return 0
    finally:
        for sig, handler in previous.items():
            signal.signal(sig, handler)


def main() -> int:
    env = dict(os.environ)
    source = Path(__file__).resolve().parent
    config = runtime_config(env, json.loads((source / "config.json").read_text()))
    workspace_host = env.get("DATABRICKS_HOST", "")
    if not workspace_host:
        host_format = "missing"
    elif workspace_host.startswith("https://"):
        host_format = "https-url"
    elif workspace_host.startswith("http://"):
        host_format = "http-url"
    elif "://" not in workspace_host and not any(char in workspace_host for char in "/@?# \r\n\t"):
        host_format = "bare-hostname"
    else:
        host_format = "other"
    # The format helps diagnose platform configuration without logging its
    # value or any other injected environment variable.
    print("[apps-launcher] workspace host input format: " + host_format, flush=True)
    with tempfile.TemporaryDirectory(prefix="pg-sage-app-") as directory:
        runtime = Path(directory)
        executable = unpack_binary(source, runtime)
        config_path = runtime / "config.json"
        config_path.write_text(json.dumps(config))
        config_path.chmod(0o600)
        return supervise([str(executable), "--config", str(config_path)], env)


if __name__ == "__main__":
    sys.exit(main())
