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
