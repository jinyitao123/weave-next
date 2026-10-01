import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('deployment_state', Path(__file__).with_name('deployment-state.py'))
deployment = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deployment)


class DeploymentStateTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.env = self.root / 'server.env'
        self.env.write_text('WEAVE_ADMIN_PASS=fixture-admin\nWEAVE_RUNTIME_SERVER_URL=http://example.invalid:8080\n'
                            'WORKBENCH_PUBLIC_AUTHORITY=example.invalid:3080\nWEAVE_API_KEY=\n')

    def invoke(self, *args):
        with patch('sys.argv', ['deployment-state.py', args[0], str(self.env), str(self.root), *args[1:]]):
            deployment.main()

    def test_bootstrap_persists_key_once_and_keeps_existing_settings(self):
        with patch.object(deployment.subprocess, 'check_output', return_value='{"api_key":"fixture-key"}') as command:
            with patch.object(deployment, 'get_json', return_value={'role': 'admin'}):
                self.invoke('bootstrap', '--', 'docker', 'compose')
                self.invoke('bootstrap', '--', 'docker', 'compose')
        self.assertEqual(command.call_count, 1)
        self.assertEqual(deployment.read_env(self.env)['WEAVE_ADMIN_PASS'], 'fixture-admin')
        self.assertEqual(deployment.read_env(self.env)['WEAVE_API_KEY'], 'fixture-key')
        self.assertEqual(self.env.stat().st_mode & 0o777, 0o600)
        connection = json.loads((self.root / 'connection.json').read_text())
        self.assertEqual(connection['api_key'], 'fixture-key')

    def test_empty_bootstrap_does_not_overwrite_configuration(self):
        original = self.env.read_bytes()
        with patch.object(deployment.subprocess, 'check_output', return_value='{"api_key":""}'):
            with self.assertRaises(RuntimeError):
                self.invoke('bootstrap', '--', 'docker', 'compose')
        self.assertEqual(self.env.read_bytes(), original)

    def test_workbench_storage_creation_preserves_existing_files_and_is_idempotent(self):
        workbench = self.root / 'workbench-data'
        workspaces = self.root / 'workspaces'
        workbench.mkdir()
        sentinel = workbench / 'existing-session.json'
        sentinel.write_text('{"kept":true}')
        self.env.write_text(self.env.read_text() +
                            f'WORKBENCH_DATA_PATH={workbench}\nWORKBENCH_WORKSPACE_PATH={workspaces}\n')

        prepared = deployment.prepare_workbench_storage(deployment.read_env(self.env))
        self.assertEqual([item['state'] for item in prepared], ['preserved', 'created_empty'])
        self.assertEqual(sentinel.read_text(), '{"kept":true}')
        self.assertTrue(workspaces.is_dir())
        self.assertEqual(workspaces.stat().st_mode & 0o777, 0o750)

        prepared_again = deployment.prepare_workbench_storage(deployment.read_env(self.env))
        self.assertEqual([item['state'] for item in prepared_again], ['preserved', 'preserved'])
        self.assertEqual(sentinel.read_text(), '{"kept":true}')

    def test_workbench_storage_rejects_overlapping_paths_before_creation(self):
        shared = self.root / 'shared'
        child = shared / 'workspaces'
        self.env.write_text(self.env.read_text() +
                            f'WORKBENCH_DATA_PATH={shared}\nWORKBENCH_WORKSPACE_PATH={child}\n')
        with self.assertRaisesRegex(RuntimeError, 'separate directories'):
            deployment.prepare_workbench_storage(deployment.read_env(self.env))
        self.assertFalse(shared.exists())

    def test_web_workbench_is_off_unless_server_env_opts_in(self):
        for value, expected in (('', False), ('false', False), ('0', False), ('true', True), ('1', True), ('YES', True)):
            self.assertEqual(deployment.web_workbench_enabled({'WEAVE_WEB_WORKBENCH': value}), expected, value)
        self.assertFalse(deployment.web_workbench_enabled({}))

    def test_bootstrap_without_workbench_settings_omits_its_url(self):
        self.env.write_text('WEAVE_ADMIN_PASS=fixture-admin\nWEAVE_RUNTIME_SERVER_URL=http://example.invalid:8080\nWEAVE_API_KEY=\n')
        with patch.object(deployment.subprocess, 'check_output', return_value='{"api_key":"fixture-key"}'):
            with patch.object(deployment, 'get_json', return_value={'role': 'admin'}):
                self.invoke('bootstrap', '--', 'docker', 'compose')
        connection = json.loads((self.root / 'connection.json').read_text())
        self.assertNotIn('workbench_url', connection)

    def test_storage_preparation_is_skipped_when_workbench_is_off(self):
        with patch.object(deployment, 'prepare_workbench_storage', side_effect=AssertionError('must not run')):
            self.invoke('prepare-workbench-storage')

    def test_verify_without_workbench_never_contacts_it_and_records_that(self):
        healthy = [{'build_commit': 'a' * 40}, {'status': 'ready'}, {'runtimes': []}]
        with patch.object(deployment, 'get_json', side_effect=healthy):
            with patch.object(deployment.urllib.request, 'urlopen', side_effect=AssertionError('Workbench was contacted')):
                self.invoke('verify', 'a' * 40)
        receipt = json.loads((self.root / 'last-success.json').read_text())
        self.assertIsNone(receipt['workbench_http_status'])
        self.assertFalse(receipt['web_workbench'])

    def test_verify_with_workbench_opted_in_requires_its_gateway(self):
        self.env.write_text(self.env.read_text() + 'WEAVE_WEB_WORKBENCH=true\n')
        healthy = [{'build_commit': 'a' * 40}, {'status': 'ready'}, {'runtimes': []}]
        with patch.object(deployment, 'get_json', side_effect=healthy):
            with patch.object(deployment.urllib.request, 'urlopen', side_effect=deployment.urllib.error.URLError('down')):
                with self.assertRaises(deployment.urllib.error.URLError):
                    self.invoke('verify', 'a' * 40)
        self.assertFalse((self.root / 'last-success.json').exists())

    def test_wrong_running_commit_cannot_be_recorded_as_success(self):
        with patch.object(deployment, 'get_json', return_value={'build_commit': 'a' * 40}):
            with self.assertRaises(RuntimeError):
                self.invoke('verify', 'b' * 40)
        self.assertFalse((self.root / 'last-success.json').exists())

    def test_unready_dependencies_cannot_be_recorded_as_success(self):
        with patch.object(deployment, 'get_json', side_effect=[{'build_commit': 'a' * 40}, {'status': 'not_ready'}]):
            with self.assertRaises(RuntimeError):
                self.invoke('verify', 'a' * 40)
        self.assertFalse((self.root / 'last-success.json').exists())


if __name__ == '__main__':
    unittest.main()
