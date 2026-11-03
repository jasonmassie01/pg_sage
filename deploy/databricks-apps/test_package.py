import importlib.util
import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path
import struct
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent


def load_module(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


packager = load_module("package")
launcher = load_module("launch")
configurer = load_module("configure")


def elf(payload=b"", architecture=62, dynamic=False):
    binary = bytearray(128)
    binary[:6] = b"\x7fELF\x02\x01"
    struct.pack_into("<H", binary, 18, architecture)
    struct.pack_into("<Q", binary, 32, 64)
    struct.pack_into("<HH", binary, 54, 56, 1)
    struct.pack_into("<I", binary, 64, 3 if dynamic else 1)
    return bytes(binary) + payload


class PackageTests(unittest.TestCase):
    def test_large_binary_roundtrip_stays_under_upload_limit(self):
        with tempfile.TemporaryDirectory() as temp:
            temp = Path(temp)
            binary = elf(os.urandom(9_000_000))
            source = temp / "source"
            source.write_bytes(binary)
            output = temp / "app"
            manifest = packager.package(source, output)
            self.assertGreater(len(manifest["chunks"]), 1)
            self.assertTrue(all(file.stat().st_size < 10_000_000 for file in output.iterdir()))
            runtime = temp / "runtime"
            runtime.mkdir()
            with patch.object(launcher.platform, "system", return_value="Linux"), patch.object(launcher.platform, "machine", return_value="x86_64"):
                restored = launcher.unpack_binary(output, runtime)
            self.assertEqual(restored.read_bytes(), binary)
            self.assertEqual(restored.stat().st_mode & 0o777, 0o700)
            manifest["sha256"] = "0" * 64
            (output / "binary.json").write_text(json.dumps(manifest))
            with patch.object(launcher.platform, "system", return_value="Linux"), patch.object(launcher.platform, "machine", return_value="x86_64"):
                with self.assertRaisesRegex(ValueError, "checksum"):
                    launcher.unpack_binary(output, runtime)

    def test_rejects_dynamic_and_wrong_platform_binary(self):
        for binary in (b"not an ELF file", elf(dynamic=True), elf(architecture=0)):
            with self.subTest(binary=binary[:20]):
                with self.assertRaises(ValueError):
                    packager.binary_architecture(binary)

    def test_rejects_runtime_architecture_mismatch(self):
        with tempfile.TemporaryDirectory() as temp:
            temp = Path(temp)
            source = temp / "source"
            source.write_bytes(elf())
            output = temp / "app"
            packager.package(source, output)
            with patch.object(launcher.platform, "system", return_value="Linux"), patch.object(launcher.platform, "machine", return_value="aarch64"):
                with self.assertRaisesRegex(ValueError, "architecture"):
                    launcher.unpack_binary(output, temp)


class LauncherTests(unittest.TestCase):
    def env(self):
        return {"DATABRICKS_APP_PORT": "8123", "PGHOST": "example.databricks.com", "PGDATABASE": "sage_test",
                "DATABRICKS_HOST": "https://workspace.cloud.databricks.com", "DATABRICKS_CLIENT_SECRET": "test-oauth-secret",
                "DATABRICKS_CLIENT_ID": "app-principal", "ENDPOINT_NAME": "projects/test/branches/dev/endpoints/primary",
                "SAGE_ENCRYPTION_KEY": "test-secret", "SAGE_LLM_AUTH": "databricks",
                "SAGE_AUTH_MODE": "databricks_app", "SAGE_DATABRICKS_ADMIN_EMAIL": "owner@example.com"}

    def test_platform_integration_and_no_secrets_in_config(self):
        env = self.env()
        config = launcher.runtime_config(env)
        self.assertEqual(config["api"]["listen_addr"], "0.0.0.0:8123")
        self.assertEqual(config["postgres"]["max_connections"], 5)
        self.assertEqual(env["SAGE_PG_USER"], "app-principal")
        self.assertEqual(env["SAGE_PG_DATABASE"], "sage_test")
        self.assertEqual(env["SAGE_PG_SSLMODE"], "verify-full")
        self.assertEqual(env["SAGE_LAKEBASE_ENDPOINT"], env["ENDPOINT_NAME"])
        self.assertNotIn("test-secret", json.dumps(config))
        self.assertEqual(env["SAGE_LLM_AUTH"], "databricks")
        self.assertEqual(env["SAGE_AUTH_MODE"], "databricks_app")
        self.assertEqual(env["DATABRICKS_AUTH_TYPE"], "oauth-m2m")
        self.assertNotIn("test-oauth-secret", json.dumps(config))

    def test_alternate_auth_sources_and_ai_keys_are_refused(self):
        for change in ({"DATABRICKS_TOKEN": "test-pat"}, {"DATABRICKS_CONFIG_PROFILE": "other-user"},
                       {"DATABRICKS_AUTH_TYPE": "pat"}, {"DATABRICKS_CLIENT_SECRET": ""},
                       {"SAGE_LLM_API_KEY": "test-key"}, {"SAGE_LLM_AUTH": "api_key"}):
            with self.subTest(change=change):
                with self.assertRaises(ValueError):
                    launcher.runtime_config(self.env() | change)
        for llm in ({"api_key": "test-key"}, {"auth": "api_key"}, {"optimizer_llm": {"api_key": "test-key"}}):
            with self.subTest(llm=llm):
                with self.assertRaises(ValueError):
                    launcher.runtime_config(self.env(), {"llm": llm})

    def test_custom_config_preserved_but_platform_port_wins(self):
        config = launcher.runtime_config(self.env(), {"api": {"listen_addr": "127.0.0.1:9999"}, "llm": {"json_mode": False}})
        self.assertEqual(config["api"]["listen_addr"], "0.0.0.0:8123")
        self.assertIs(config["llm"]["json_mode"], False)

    def test_shared_postgres_pool_default_preserves_explicit_size_and_base(self):
        for postgres, expected in (({"port": 5432}, 5), ({"port": 5432, "max_connections": 2}, 2)):
            with self.subTest(postgres=postgres):
                base = {"postgres": dict(postgres), "api": {"listen_addr": "127.0.0.1:9999"}}
                config = launcher.runtime_config(self.env(), base)
                self.assertEqual(config["postgres"]["max_connections"], expected)
                self.assertEqual(config["postgres"]["port"], 5432)
                self.assertEqual(base, {"postgres": postgres, "api": {"listen_addr": "127.0.0.1:9999"}})
                self.assertIsNot(config["postgres"], base["postgres"])

    def test_telemetry_warehouse_matches_declared_app_resource(self):
        env = self.env() | {"DATABRICKS_WAREHOUSE_ID": "declared-warehouse"}
        config = launcher.runtime_config(env, {"lakebase_observability": {"enabled": True, "allow_payload": False}})
        self.assertEqual(config["lakebase_observability"]["warehouse_id"], "declared-warehouse")
        self.assertFalse(config["lakebase_observability"]["allow_payload"])
        with self.assertRaises(ValueError):
            launcher.runtime_config(env, {"lakebase_observability": {"enabled": True, "warehouse_id": "different-warehouse"}})

    def test_existing_explicit_connection_values_win(self):
        env = self.env()
        env["SAGE_PG_USER"] = "custom-role"
        env["SAGE_LAKEBASE_ENDPOINT"] = "projects/explicit/branches/dev/endpoints/primary"
        launcher.runtime_config(env)
        self.assertEqual(env["SAGE_PG_USER"], "custom-role")
        self.assertIn("explicit", env["SAGE_LAKEBASE_ENDPOINT"])

    def test_launch_supervises_native_server_and_keeps_credentials_in_environment(self):
        with tempfile.TemporaryDirectory() as temp:
            temp = Path(temp)
            binary = temp / "pg_sage"
            with patch.dict(launcher.os.environ, self.env(), clear=True), patch.object(launcher.tempfile, "TemporaryDirectory") as temporary, patch.object(launcher, "unpack_binary", return_value=binary), patch.object(launcher, "supervise", return_value=0) as supervise:
                temporary.return_value.__enter__.return_value = str(temp)
                self.assertEqual(launcher.main(), 0)
            args, env = supervise.call_args.args
            self.assertEqual(args, [str(binary), "--config", str(temp / "config.json")])
            config = json.loads((temp / "config.json").read_text())
            self.assertEqual(config["api"]["listen_addr"], "0.0.0.0:8123")
            self.assertNotIn("test-secret", json.dumps(config))
            self.assertEqual(env["SAGE_ENCRYPTION_KEY"], "test-secret")

    def test_missing_configuration_fails_before_launch(self):
        for change in ({"DATABRICKS_APP_PORT": "65536"}, {"DATABRICKS_APP_PORT": "not-a-port"},
                       {"SAGE_TLS_CERT": "/cert.pem"}, {"ENDPOINT_NAME": ""},
                       {"SAGE_ENCRYPTION_KEY": ""}, {"PGHOST": ""}):
            with self.subTest(change=change):
                env = self.env() | change
                with self.assertRaises(ValueError):
                    launcher.runtime_config(env)

    def test_local_password_auth_or_missing_bound_admin_fails_before_launch(self):
        for change in ({"SAGE_AUTH_MODE": ""}, {"SAGE_AUTH_MODE": "local"},
                       {"SAGE_DATABRICKS_ADMIN_EMAIL": ""},
                       {"SAGE_BOOTSTRAP_ADMIN_EMAIL": "owner@example.com"},
                       {"SAGE_BOOTSTRAP_ADMIN_PASSWORD": "unused-password"},
                       {"SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE": "/unused/password"}):
            with self.subTest(change=change):
                with self.assertRaises(ValueError):
                    launcher.runtime_config(self.env() | change)


class ConfigureTests(unittest.TestCase):
    def settings(self):
        branch = "projects/search/branches/sage-dev"
        return {"app_name": "pg-sage-dev", "host": "ep-dev.database.us-west-2.cloud.databricks.com",
                "branch": branch, "endpoint": branch + "/endpoints/primary",
                "database_resource": branch + "/databases/sage-dev", "database_name": "sage_dev",
                "secret_scope": "sage-app", "secret_key": "encryption-key",
                "databricks_admin_email": "owner@example.com",
                "gateway_endpoint": "https://example.cloud.databricks.com/ai-gateway/mlflow/v1",
                "gateway_model": "system.ai.test", "gateway_json_mode": True}

    def test_runtime_and_bundle_settings_match_with_correct_reference_spelling(self):
        runtime, bundle = configurer.render(self.settings())
        app = bundle["resources"]["apps"]["pg_sage"]
        self.assertEqual(app["resources"][0]["postgres"]["branch"], self.settings()["branch"])
        normalized = []
        for item in app["config"]["env"]:
            item = dict(item)
            if "value_from" in item:
                item["valueFrom"] = item.pop("value_from")
            normalized.append(item)
        self.assertEqual(runtime["env"], normalized)
        self.assertEqual(app["resources"][1]["secret"]["permission"], "READ")
        self.assertEqual(app["resources"][2]["uc_securable"], {"securable_full_name": "system.ai.test", "securable_type": "MODEL_SERVICE", "permission": "EXECUTE"})
        self.assertEqual([r["name"] for r in app["resources"]], ["postgres", "sage-encryption-key", "gateway-model"])
        self.assertIn({"name": "SAGE_AUTH_MODE", "value": "databricks_app"}, runtime["env"])
        self.assertIn({"name": "SAGE_DATABRICKS_ADMIN_EMAIL", "value": "owner@example.com"}, runtime["env"])
        self.assertFalse(any(item["name"].startswith("SAGE_BOOTSTRAP_") for item in runtime["env"]))
        self.assertNotIn("PGUSER", [item["name"] for item in runtime["env"]])
        self.assertEqual(bundle["targets"], {"dev": {"default": True}})

    def test_rejects_mismatched_resources_and_inline_credentials(self):
        cases = [{"endpoint": "projects/search/branches/production/endpoints/primary"},
                 {"database_resource": "projects/search/branches/production/databases/db"},
                 {"host": "user:password@example.com"}, {"encryption_key": "do-not-write-secrets"},
                 {"gateway_endpoint": "https://user:password@example.com/v1"},
                 {"gateway_endpoint": "https://example.com/v1?token=secret"},
                 {"databricks_admin_email": "Owner <owner@example.com>"},
                 {"databricks_admin_user_id": "123456789"},
                 {"bootstrap_admin_email": "owner@example.com"},
                 {"bootstrap_admin_secret_key": "bootstrap-admin-password"},
                 {"bootstrap_admin_password": "do-not-write-secrets"},
                 {"gateway_model": "not-a-UC-model"},
                 {"gateway_json_mode": "true"},
                 {"gateway_model": ""}]
        for change in cases:
            with self.subTest(change=change):
                with self.assertRaises(ValueError):
                    configurer.render(self.settings() | change)

    def test_gateway_resource_is_optional(self):
        settings = self.settings()
        del settings["gateway_endpoint"], settings["gateway_model"], settings["gateway_json_mode"]
        runtime, bundle = configurer.render(settings)
        self.assertEqual(len(bundle["resources"]["apps"]["pg_sage"]["resources"]), 2)
        self.assertNotIn("SAGE_LLM_MODEL", [item["name"] for item in runtime["env"]])

    def test_optional_email_audience_matches_runtime_and_bundle(self):
        runtime, bundle = configurer.render(self.settings())
        self.assertNotIn("SAGE_DATABRICKS_ALLOWED_EMAIL_DOMAINS", [item["name"] for item in runtime["env"]])
        settings = self.settings() | {"databricks_allowed_email_domains": [" EXAMPLE.COM ", "other.example.com", "example.com"]}
        runtime, bundle = configurer.render(settings)
        expected = {"name": "SAGE_DATABRICKS_ALLOWED_EMAIL_DOMAINS", "value": "example.com,other.example.com"}
        self.assertIn(expected, runtime["env"])
        self.assertIn(expected, bundle["resources"]["apps"]["pg_sage"]["config"]["env"])
        env = LauncherTests().env()
        env["SAGE_DATABRICKS_ALLOWED_EMAIL_DOMAINS"] = expected["value"]
        launcher.runtime_config(env)
        self.assertEqual(env["SAGE_DATABRICKS_ALLOWED_EMAIL_DOMAINS"], expected["value"])

    def test_email_audience_rejects_malformed_domains_and_admin_lockout(self):
        for domains in ([], "", "example.com", None, [None], [42], [""], [" "], ["*.example.com"],
                        ["example.com,"], ["https://example.com"], ["example.com/path"], ["example.com:443"],
                        ["user@example.com"], [".example.com"], ["example.com."], ["example..com"],
                        ["-bad.example.com"], ["bad-.example.com"], ["bad_domain.example.com"], ["exam ple.com"],
                        ["localhost"], ["127.0.0.1"], ["[::1]"], ["exämple.com"], ["a" * 64 + ".com"],
                        ["a." * 126 + "com"], ["other.example.com"], ["sub.example.com"], ["notexample.com"],
                        ["example.com.evil.test"]):
            with self.subTest(domains=domains):
                with self.assertRaises(ValueError):
                    configurer.render(self.settings() | {"databricks_allowed_email_domains": domains})

    def test_telemetry_is_metadata_only_and_bound_to_reviewed_database(self):
        settings = self.settings() | {"lakebase_observability": {"enabled": True, "warehouse_id": "existing-warehouse", "max_rows": 50, "queries_per_poll": 2}}
        rendered = configurer.telemetry_config(settings)
        self.assertFalse(rendered["allow_payload"])
        self.assertEqual(rendered["bindings"], [{"host": settings["host"], "database": settings["database_name"], "endpoint": settings["endpoint"], "allow_compute": False}])
        _, bundle = configurer.render(settings)
        resources = bundle["resources"]["apps"]["pg_sage"]["resources"]
        self.assertEqual(len(resources), 4)
        self.assertEqual(resources[-1], {"name": "lakebase-history-warehouse", "sql_warehouse": {"id": "existing-warehouse", "permission": "CAN_USE"}})
        self.assertFalse(any(item.get("uc_securable", {}).get("securable_type") == "SCHEMA" for item in resources), "telemetry must not silently grant UC or PII access")
        with tempfile.TemporaryDirectory() as temp:
            package = Path(temp)
            (package / "binary.json").write_text("{}")
            (package / "config.json").write_text(json.dumps({"ask": {"enabled": False}}))
            configurer.configure(package, settings)
            config = json.loads((package / "config.json").read_text())
            self.assertEqual(config["lakebase_observability"], rendered)
            self.assertFalse(config["ask"]["enabled"])

    def test_telemetry_rejects_unbounded_settings_credentials_and_unreviewed_scope(self):
        for telemetry in ({"enabled": True}, {"enabled": "true"}, {"warehouse_id": "id?token=secret"},
                          {"allow_payload": "true"}, {"max_rows": 10001}, {"max_rows": True},
                          {"retention_hours": 169}, {"query_timeout_seconds": 121}, {"queries_per_poll": 0},
                          {"token": "do-not-write"}, {"bindings": [{"database": "other"}]},
                          {"streams": ["arbitrary_table"]}, {"streams": ["plan_history", "plan_history"]},
                          {"streams": ["compute_gauges"]}):
            with self.subTest(telemetry=telemetry):
                with self.assertRaises(ValueError):
                    configurer.render(self.settings() | {"lakebase_observability": telemetry})

    def test_configure_writes_yaml_compatible_json_without_touching_binary(self):
        with tempfile.TemporaryDirectory() as temp:
            package = Path(temp)
            (package / "binary.json").write_text("{}")
            configurer.configure(package, self.settings())
            self.assertEqual(json.loads((package / "app.yaml").read_text())["command"], ["python", "launch.py"])
            self.assertEqual(json.loads((package / "databricks.yml").read_text())["resources"]["apps"]["pg_sage"]["name"], "pg-sage-dev")
            self.assertEqual((package / "binary.json").read_text(), "{}")
            self.assertIs(json.loads((package / "config.json").read_text())["llm"]["json_mode"], True)


@unittest.skipUnless(os.name == "posix", "Apps supervision requires POSIX process groups")
class SupervisorTests(unittest.TestCase):
    def run_supervisor(self, script, temp, timeout=12.0):
        command = [sys.executable, "-c", script, str(temp)]
        runner = "import os,runpy,sys; m=runpy.run_path(sys.argv[1]); sys.exit(m['supervise'](sys.argv[3:],dict(os.environ),float(sys.argv[2])))"
        return subprocess.Popen([sys.executable, "-c", runner, str(ROOT / "launch.py"), str(timeout), *command], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

    def test_requested_restart_relaunches_and_declares_supervisor(self):
        script = "import os,sys; from pathlib import Path; p=Path(sys.argv[1])/'count'; n=int(p.read_text())+1 if p.exists() else 1; p.write_text(str(n)); assert os.environ['SAGE_SUPERVISED']=='1'; sys.exit(42 if n==1 else 0)"
        with tempfile.TemporaryDirectory() as temp:
            process = self.run_supervisor(script, temp)
            stdout, stderr = process.communicate(timeout=5)
            self.assertEqual(process.returncode, 0, stderr)
            self.assertEqual((Path(temp) / "count").read_text(), "2")
            self.assertIn("restart requested", stdout)

    def test_actual_failure_is_not_restarted(self):
        with tempfile.TemporaryDirectory() as temp:
            process = self.run_supervisor("import sys; sys.exit(7)", temp)
            process.communicate(timeout=5)
            self.assertEqual(process.returncode, 7)

    def test_restart_loop_is_bounded(self):
        with tempfile.TemporaryDirectory() as temp:
            process = self.run_supervisor("import sys; sys.exit(42)", temp)
            _, stderr = process.communicate(timeout=5)
            self.assertEqual(process.returncode, 1)
            self.assertIn("too many restart requests", stderr)

    def test_platform_shutdown_reaches_child_and_does_not_restart(self):
        script = "import signal,sys,time; from pathlib import Path; p=Path(sys.argv[1]); signal.signal(signal.SIGTERM,lambda s,f: (p.joinpath('stopped').write_text('yes'),sys.exit(0))); p.joinpath('ready').write_text('yes'); time.sleep(30)"
        with tempfile.TemporaryDirectory() as temp:
            process = self.run_supervisor(script, temp)
            deadline = time.monotonic() + 5
            while not (Path(temp) / "ready").exists() and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertTrue((Path(temp) / "ready").exists())
            process.send_signal(signal.SIGTERM)
            _, stderr = process.communicate(timeout=5)
            self.assertEqual(process.returncode, 0, stderr)
            self.assertEqual((Path(temp) / "stopped").read_text(), "yes")

    def test_unresponsive_child_is_killed_within_shutdown_budget(self):
        script = "import signal,sys,time; from pathlib import Path; signal.signal(signal.SIGTERM,signal.SIG_IGN); Path(sys.argv[1]).joinpath('ready').write_text('yes'); time.sleep(30)"
        with tempfile.TemporaryDirectory() as temp:
            process = self.run_supervisor(script, temp, timeout=0.1)
            deadline = time.monotonic() + 5
            while not (Path(temp) / "ready").exists() and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertTrue((Path(temp) / "ready").exists())
            process.send_signal(signal.SIGTERM)
            process.communicate(timeout=3)
            self.assertEqual(process.returncode, 128 + signal.SIGTERM)


if __name__ == "__main__":
    unittest.main()
