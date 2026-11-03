"""Render reviewed, non-secret resource settings into an existing Apps package."""
from __future__ import annotations
import argparse
from copy import deepcopy
import ipaddress
import json
from pathlib import Path
import re
from urllib.parse import urlsplit

REQUIRED = {"app_name", "host", "endpoint", "branch", "database_resource", "database_name", "secret_scope", "secret_key", "databricks_admin_email"}
OPTIONAL = {"pg_user", "gateway_endpoint", "gateway_model", "gateway_json_mode", "description", "lakebase_observability", "databricks_allowed_email_domains"}
RESOURCE = re.compile(r"projects/[a-z0-9-]+/branches/[a-z0-9-]+")
HOST = re.compile(r"[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+", re.I)
RUNTIME_FILES = ["app.yaml", "binary.json", "binary.part-*", "config.json", "launch.py", "databricks.yml"]


def allowed_email_domains(settings: dict) -> list[str]:
    if "databricks_allowed_email_domains" not in settings:
        return []
    raw = settings["databricks_allowed_email_domains"]
    if not isinstance(raw, list) or not raw:
        raise ValueError("databricks_allowed_email_domains must be a nonempty list of exact DNS domains")
    domains = []
    for item in raw:
        domain = item.strip().lower() if isinstance(item, str) else ""
        labels = domain.split(".")
        valid = len(domain) <= 253 and len(labels) >= 2 and all(
            re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", label) for label in labels)
        try:
            ipaddress.ip_address(domain)
            valid = False
        except ValueError:
            pass
        if not valid:
            raise ValueError("databricks_allowed_email_domains must contain exact DNS domains without empty entries or wildcards")
        if domain not in domains:
            domains.append(domain)
    if settings["databricks_admin_email"].rsplit("@", 1)[-1].lower() not in domains:
        raise ValueError("databricks_admin_email must belong to databricks_allowed_email_domains")
    return domains


def telemetry_config(settings: dict) -> dict | None:
    if "lakebase_observability" not in settings:
        return None
    raw = settings["lakebase_observability"]
    bounds = {"poll_interval_seconds": (30, 3600), "query_timeout_seconds": (1, 120),
              "max_rows": (1, 10000), "max_bytes": (1024, 16777216), "queries_per_poll": (1, 1000),
              "window_seconds": (1, 604800), "retention_hours": (1, 168)}
    allowed = set(bounds) | {"enabled", "warehouse_id", "allow_payload", "allow_compute", "streams"}
    if not isinstance(raw, dict) or set(raw) - allowed:
        raise ValueError("lakebase_observability accepts bounded non-secret settings only; its resource binding is derived from the selected database")
    for field in ("enabled", "allow_payload", "allow_compute"):
        if field in raw and type(raw[field]) is not bool:
            raise ValueError("lakebase_observability." + field + " must be boolean")
    for field, (minimum, maximum) in bounds.items():
        if field in raw and (type(raw[field]) is not int or not minimum <= raw[field] <= maximum):
            raise ValueError("lakebase_observability." + field + " is outside supported bounds")
    warehouse = raw.get("warehouse_id", "")
    if not isinstance(warehouse, str) or (warehouse and not re.fullmatch(r"[a-zA-Z0-9-]{1,128}", warehouse)) or (raw.get("enabled", False) and not warehouse):
        raise ValueError("enabled lakebase_observability requires an explicitly selected existing warehouse_id")
    streams = raw.get("streams", [])
    known = {"plan_history", "postgres_logs", "active_session_history", "ddl_history", "compute_counters", "compute_gauges", "database_counters", "database_gauges", "wait_event_counters", "pg_stat_statements_counters"}
    if not isinstance(streams, list) or any(not isinstance(item, str) or item not in known for item in streams) or len(set(streams)) != len(streams):
        raise ValueError("lakebase_observability.streams contains an invalid or duplicate stream")
    if any(item.startswith("compute_") for item in streams) and not raw.get("allow_compute", False):
        raise ValueError("compute history requires explicit allow_compute authorization")
    result = deepcopy(raw)
    result.setdefault("enabled", False)
    result.setdefault("allow_payload", False)
    result.pop("allow_compute", None)
    result["bindings"] = [{"host": settings["host"], "database": settings["database_name"], "endpoint": settings["endpoint"], "allow_compute": raw.get("allow_compute", False)}]
    return result


def render(settings: dict) -> tuple[dict, dict]:
    unknown = set(settings) - REQUIRED - OPTIONAL
    if unknown:
        raise ValueError("unsupported settings keys; pass resource references, never secret values: " + ", ".join(sorted(unknown)))
    if any(not isinstance(settings.get(key), str) or not settings[key].strip() for key in REQUIRED):
        raise ValueError("all required deployment settings must be non-empty strings")
    if not re.fullmatch(r"[a-z][a-z0-9-]{0,25}", settings["app_name"]):
        raise ValueError("app_name must start with a lowercase letter and contain at most 26 lowercase letters, digits or hyphens")
    branch = settings["branch"]
    if not RESOURCE.fullmatch(branch):
        raise ValueError("branch must be projects/<project>/branches/<branch>")
    for field, suffix in (("endpoint", "/endpoints/"), ("database_resource", "/databases/")):
        if not re.fullmatch(re.escape(branch + suffix) + r"[a-z0-9-]+", settings[field]):
            raise ValueError(field + " must belong to the explicitly selected branch")
    if not HOST.fullmatch(settings["host"]) or len(settings["host"]) > 253:
        raise ValueError("host must be the endpoint hostname, without credentials, a scheme or a port")
    if not re.fullmatch(r"[^\s<>@]+@[^\s<>@]+\.[^\s<>@]+", settings["databricks_admin_email"]):
        raise ValueError("databricks_admin_email must be a plain email address")
    domains = allowed_email_domains(settings)
    gateway = settings.get("gateway_endpoint", "")
    model = settings.get("gateway_model", "")
    if bool(gateway) != bool(model):
        raise ValueError("gateway_endpoint and gateway_model must be supplied together")
    if "gateway_json_mode" in settings and (not gateway or not isinstance(settings["gateway_json_mode"], bool)):
        raise ValueError("gateway_json_mode requires a gateway and must be true or false")
    if model and not re.fullmatch(r"[a-zA-Z_][\w-]*\.[a-zA-Z_][\w-]*\.[a-zA-Z_][\w-]*", model):
        raise ValueError("gateway_model must be a fully qualified Unity Catalog model service name")
    if gateway:
        url = urlsplit(gateway)
        if url.scheme != "https" or not url.hostname or url.username or url.password or url.query or url.fragment:
            raise ValueError("gateway_endpoint must be an HTTPS base URL without credentials, query or fragment")
    telemetry = telemetry_config(settings)
    env = [
        {"name": "PGHOST", "value": settings["host"]},
        {"name": "PGDATABASE", "value": settings["database_name"]},
        {"name": "PGSSLMODE", "value": "verify-full"},
        {"name": "SAGE_LAKEBASE_ENDPOINT", "value": settings["endpoint"]},
        {"name": "SAGE_ENCRYPTION_KEY", "valueFrom": "sage-encryption-key"},
        {"name": "SAGE_AUTH_MODE", "value": "databricks_app"},
        {"name": "DATABRICKS_AUTH_TYPE", "value": "oauth-m2m"},
        {"name": "SAGE_DATABRICKS_ADMIN_EMAIL", "value": settings["databricks_admin_email"]},
        {"name": "SAGE_TRUST_LEVEL", "value": "observation"},
    ]
    if settings.get("pg_user"):
        env.append({"name": "PGUSER", "value": settings["pg_user"]})
    if domains:
        env.append({"name": "SAGE_DATABRICKS_ALLOWED_EMAIL_DOMAINS", "value": ",".join(domains)})
    if gateway:
        env.extend([
            {"name": "SAGE_LLM_AUTH", "value": "databricks"},
            {"name": "SAGE_LLM_ENDPOINT", "value": gateway.rstrip("/")},
            {"name": "SAGE_LLM_MODEL", "value": model},
        ])
    if telemetry and telemetry["enabled"]:
        env.append({"name": "DATABRICKS_WAREHOUSE_ID", "valueFrom": "lakebase-history-warehouse"})
    runtime = {"command": ["python", "launch.py"], "env": env}
    bundle_runtime = deepcopy(runtime)
    for item in bundle_runtime["env"]:
        if "valueFrom" in item:
            item["value_from"] = item.pop("valueFrom")
    bundle = {
        "bundle": {"name": settings["app_name"] + "-bundle"},
        "sync": {"include": RUNTIME_FILES},
        "resources": {"apps": {"pg_sage": {
            "name": settings["app_name"],
            "description": settings.get("description", "pg_sage Lakebase development app"),
            "source_code_path": ".", "config": bundle_runtime,
            "resources": [
                {"name": "postgres", "postgres": {"branch": branch, "database": settings["database_resource"], "permission": "CAN_CONNECT_AND_CREATE"}},
                {"name": "sage-encryption-key", "secret": {"scope": settings["secret_scope"], "key": settings["secret_key"], "permission": "READ"}},
            ],
        }}},
        "targets": {"dev": {"default": True}},
    }
    if gateway:
        bundle["resources"]["apps"]["pg_sage"]["resources"].append({
            "name": "gateway-model", "uc_securable": {"securable_full_name": model, "securable_type": "MODEL_SERVICE", "permission": "EXECUTE"},
        })
    if telemetry and telemetry["enabled"]:
        bundle["resources"]["apps"]["pg_sage"]["resources"].append({
            "name": "lakebase-history-warehouse", "sql_warehouse": {"id": telemetry["warehouse_id"], "permission": "CAN_USE"},
        })
    return runtime, bundle


def configure(package: Path, settings: dict) -> None:
    if not (package / "binary.json").is_file():
        raise ValueError("package must already contain binary.json from package.py")
    runtime, bundle = render(settings)
    # JSON syntax is valid YAML and avoids adding a deployment-time dependency.
    for name, value in (("app.yaml", runtime), ("databricks.yml", bundle)):
        path = package / name
        temporary = package / (name + ".tmp")
        temporary.write_text(json.dumps(value, indent=2) + "\n")
        temporary.replace(path)
    if "gateway_json_mode" in settings or "lakebase_observability" in settings:
        path = package / "config.json"
        config = json.loads(path.read_text()) if path.exists() else {}
        if "gateway_json_mode" in settings:
            config.setdefault("llm", {})["json_mode"] = settings["gateway_json_mode"]
        if "lakebase_observability" in settings:
            config["lakebase_observability"] = telemetry_config(settings)
        temporary = package / "config.json.tmp"
        temporary.write_text(json.dumps(config, indent=2) + "\n")
        temporary.replace(path)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--package", required=True, type=Path)
    parser.add_argument("--settings", required=True, type=Path, help="JSON file containing resource names and non-secret settings")
    args = parser.parse_args()
    settings = json.loads(args.settings.read_text())
    configure(args.package, settings)
    print("Configured " + settings["app_name"] + " in " + str(args.package))
