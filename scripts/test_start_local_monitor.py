"""Launcher regression coverage; subprocesses and credentials remain local mocks."""
import importlib.util
import os
import io
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch, Mock

spec = importlib.util.spec_from_file_location(
    "launcher", Path(__file__).with_name("start_local_monitor.py")
)
launcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launcher)


class LauncherTests(unittest.TestCase):
    # No concurrent launcher tests: the supervisor owns one child synchronously;
    # all subprocesses are mocked so tests never start or stop user services.
    def test_discovered_environment_and_output_reach_child(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = root / "config.yaml"
            binary = root / "sidecar.exe"
            config.touch()
            binary.touch()
            child = Mock()
            child.wait.return_value = 0
            def password():
                os.environ["LIFEOS_DATABASE_URL"] = "postgresql://mock"
                return "mock-password"
            with patch.multiple(launcher, ROOT=root, CONFIG=config, SIDECAR=binary), \
                    patch.dict(os.environ, {}, clear=True), \
                    patch.object(launcher, "_lifeos_password", side_effect=password), \
                    patch.object(launcher.subprocess, "Popen", return_value=child) as popen:
                self.assertEqual(launcher.main(), 0)
                kwargs = popen.call_args.kwargs
                self.assertEqual(kwargs["env"].get("LIFEOS_DATABASE_URL"), "postgresql://mock")
                self.assertEqual(kwargs["env"]["LIFEOS_POSTGRES_PASSWORD"], "mock-password")
                self.assertIsNotNone(kwargs.get("stdout"))
                self.assertIsNotNone(kwargs.get("stderr"))

    def test_fleet_credentials_use_container_values_and_preserve_override(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / 'config.yaml'
            config.write_text('${FLEET_PG1_PASSWORD} ${FLEET_PG2_PASSWORD}')
            env = {'FLEET_PG1_PASSWORD': 'explicit-override'}
            with patch.object(launcher, 'CONFIG', config), \
                    patch.object(launcher, '_docker_postgres_password',
                                 return_value='container-password') as lookup:
                launcher._load_local_fleet_credentials(env)
            self.assertEqual(env['FLEET_PG1_PASSWORD'], 'explicit-override')
            self.assertEqual(env['FLEET_PG2_PASSWORD'], 'container-password')
            lookup.assert_called_once_with('pg_sage-pg-target-2-1')

    def test_missing_fleet_credential_fails_before_launch(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / 'config.yaml'
            config.write_text('${FLEET_PG1_PASSWORD}')
            with patch.object(launcher, 'CONFIG', config), \
                    patch.object(launcher, '_docker_postgres_password', return_value=None):
                with self.assertRaisesRegex(RuntimeError, 'FLEET_PG1_PASSWORD'):
                    launcher._load_local_fleet_credentials({})

    def test_docker_timeout_is_bounded_and_does_not_expose_output(self):
        timeout = subprocess.TimeoutExpired('docker inspect', 10, output='mock-secret')
        with patch.object(launcher.subprocess, 'run', side_effect=timeout) as run, \
                patch('sys.stderr', new_callable=io.StringIO) as stderr:
            self.assertIsNone(launcher._docker_postgres_password('test-container'))
        self.assertEqual(run.call_args.kwargs['timeout'], 10)
        self.assertIn('TimeoutExpired', stderr.getvalue())
        self.assertNotIn('mock-secret', stderr.getvalue())

    def test_restart_code_relaunches_then_propagates_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config, binary = root / 'config.yaml', root / 'sidecar.exe'
            config.touch()
            binary.touch()
            first, second = Mock(), Mock()
            first.wait.return_value, second.wait.return_value = 42, 7
            with patch.multiple(launcher, ROOT=root, CONFIG=config, SIDECAR=binary), \
                    patch.object(launcher, '_lifeos_password', return_value='test-only'), \
                    patch.object(launcher.subprocess, 'Popen',
                                 side_effect=[first, second]) as popen:
                self.assertEqual(launcher.main(), 7)
            self.assertEqual(popen.call_count, 2)
            for call in popen.call_args_list:
                self.assertEqual(call.args[0], [str(binary), f'--config={config}'])
                self.assertEqual(call.kwargs['cwd'], str(root / 'sidecar'))
                self.assertTrue(call.kwargs['stdout'].closed)
                self.assertTrue(call.kwargs['stderr'].closed)
            self.assertTrue((root / 'logs' / 'sidecar.runtime.out.log').exists())

    def test_missing_inputs_fail_before_password_lookup_or_child(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config, binary = root / 'missing.yaml', root / 'missing.exe'
            with patch.multiple(launcher, CONFIG=config, SIDECAR=binary), \
                    patch.object(launcher, '_lifeos_password') as password, \
                    patch.object(launcher.subprocess, 'Popen') as popen:
                with self.assertRaisesRegex(RuntimeError, 'missing config'):
                    launcher.main()
                config.touch()
                with self.assertRaisesRegex(RuntimeError, 'missing sidecar binary'):
                    launcher.main()
                password.assert_not_called()
                popen.assert_not_called()

    def test_lifeos_url_password_decodes_reserved_characters(self):
        url = 'postgresql://fixture:mock%40pass%3Aword@localhost/example'
        with patch.dict(os.environ, {'LIFEOS_DATABASE_URL': url}, clear=True), \
                patch.object(launcher, '_docker_postgres_password') as lookup:
            self.assertEqual(launcher._lifeos_password(), 'mock@pass:word')
            lookup.assert_not_called()

    def test_lifeos_url_missing_password_does_not_fallback(self):
        with patch.dict(os.environ, {'LIFEOS_DATABASE_URL': 'postgresql://localhost/x'},
                        clear=True), \
                patch.object(launcher, '_docker_postgres_password') as lookup:
            with self.assertRaisesRegex(RuntimeError, 'does not contain a password'):
                launcher._lifeos_password()
            lookup.assert_not_called()

    def test_lifeos_docker_password_is_escaped_into_child_url(self):
        with patch.dict(os.environ, {}, clear=True), \
                patch.object(launcher, '_docker_postgres_password',
                             return_value='mock@pass:word') as lookup:
            self.assertEqual(launcher._lifeos_password(), 'mock@pass:word')
            self.assertIn('mock%40pass%3Aword@', os.environ['LIFEOS_DATABASE_URL'])
            lookup.assert_called_once_with('lifeos_postgres')

    def test_docker_env_parser_preserves_equals_and_ignores_other_values(self):
        proc = subprocess.CompletedProcess([], 0, 'OTHER=hidden\nPOSTGRES_PASSWORD=a=b=c\n')
        with patch.object(launcher.subprocess, 'run', return_value=proc):
            self.assertEqual(launcher._docker_postgres_password('fixture'), 'a=b=c')
        proc.stdout = 'OTHER=hidden\n'
        with patch.object(launcher.subprocess, 'run', return_value=proc):
            self.assertIsNone(launcher._docker_postgres_password('fixture'))

    def test_docker_command_failures_do_not_expose_captured_secrets(self):
        errors = [OSError('mock-secret'),
                  subprocess.CalledProcessError(1, 'docker', output='mock-secret')]
        for error in errors:
            with self.subTest(error=type(error).__name__), \
                    patch.object(launcher.subprocess, 'run', side_effect=error), \
                    patch('sys.stderr', new_callable=io.StringIO) as stderr:
                self.assertIsNone(launcher._docker_postgres_password('fixture'))
                self.assertIn(type(error).__name__, stderr.getvalue())
                self.assertNotIn('mock-secret', stderr.getvalue())

    def test_config_without_fleet_placeholders_never_inspects_docker(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / 'config.yaml'
            config.write_text('databases: []', encoding='utf-8')
            with patch.object(launcher, 'CONFIG', config), \
                    patch.object(launcher, '_docker_postgres_password') as lookup:
                env = {}
                launcher._load_local_fleet_credentials(env)
                self.assertEqual(env, {})
                lookup.assert_not_called()


if __name__ == "__main__":
    unittest.main()
