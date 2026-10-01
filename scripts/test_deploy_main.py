"""Execute the deployment runner with local command/HTTP doubles, never Docker."""

import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import textwrap
import unittest


SCRIPTS = Path(__file__).resolve().parent
COMMIT = '9c63ea7c7c75055b5973d3f44c861668b9b597ba'


class DeployMainTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.state = self.root / 'state'
        self.state.mkdir()
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.calls = self.root / 'calls.jsonl'
        self.containers = self.root / 'containers.json'
        self.env_file = self.root / 'server.env'
        self.data = self.root / 'workbench-data'
        self.workspace = self.root / 'workspaces'
        self.data.mkdir()
        self.workspace.mkdir()
        (self.data / 'session.json').write_text('{"preserved":true}')
        (self.workspace / 'document.txt').write_text('preserved document')
        self._write_command('flock', 'import sys\nsys.exit(0)\n')
        self._write_command('stat', 'import os, sys\nprint(os.stat(sys.argv[-1]).st_size)\n')
        self._write_command('sha256sum', 'import hashlib, pathlib, sys\n'
                            'print(hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).hexdigest(), sys.argv[1])\n')
        self._write_command('sudo', '''
            import json, os, pathlib, sys
            args = sys.argv[1:]
            assert args[:2] == ['docker', 'compose'], args
            commands = {'config', 'build', 'ps', 'exec', 'up', 'stop'}
            index = next(i for i, value in enumerate(args) if value in commands)
            command, parameters = args[index], args[index + 1:]
            with open(os.environ['MOCK_CALLS'], 'a') as capture:
                capture.write(json.dumps({'compose': args, 'command': command, 'parameters': parameters}) + '\\n')
            state_path = pathlib.Path(os.environ['MOCK_CONTAINERS'])
            containers = json.loads(state_path.read_text())
            if command == 'ps':
                if containers.get(parameters[-1], False):
                    print('fixture-container')
            elif command == 'exec':
                assert 'pg_dump' in parameters, parameters
                print('fixture-backup')
            elif command == 'stop':
                assert '--profile' in args[:index] and 'legacy-workbench' in args[:index], args
                assert parameters == ['workbench-gateway', 'workbench'], parameters
                if os.environ.get('MOCK_STOP_FAIL') == 'true':
                    raise SystemExit(23)
                for service in parameters:
                    if service in containers:
                        containers[service] = False
            elif command == 'up':
                for service in ('db', 'weave', 'workbench', 'workbench-gateway'):
                    if service in parameters:
                        containers[service] = True
            state_path.write_text(json.dumps(containers))
        ''')
        self._write_command('python3', '''
            import importlib.util, json, os, pathlib, sys
            if sys.argv[1] == '-':
                # The runner still executes both source-SHA rechecks, without
                # contacting GitHub or exposing its fixture credential.
                print(os.environ['MOCK_MAIN_COMMIT'])
                raise SystemExit(0)
            path = pathlib.Path(sys.argv[1])
            assert path.name == 'deployment-state.py', path
            spec = importlib.util.spec_from_file_location('deployment_state', path)
            deployment = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(deployment)
            def get_json(url, api_key=None):
                if url.endswith('/v1/health'):
                    return {'build_commit': os.environ['MOCK_MAIN_COMMIT']}
                if url.endswith('/v1/ready'):
                    return {'status': 'ready'}
                return {}
            deployment.get_json = get_json
            class Gateway:
                status = 401
                def __enter__(self): return self
                def __exit__(self, *args): return False
            def urlopen(request, timeout):
                assert os.environ['MOCK_WEB_WORKBENCH'] == 'true', 'Disabled Workbench was contacted'
                return Gateway()
            deployment.urllib.request.urlopen = urlopen
            with open(os.environ['MOCK_CALLS'], 'a') as capture:
                capture.write(json.dumps({'state_action': sys.argv[2]}) + '\\n')
            sys.argv = sys.argv[1:]
            deployment.main()
        ''')
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode='w:gz') as package:
            for name in ('deploy-main.sh', 'deployment-state.py'):
                package.add(SCRIPTS / name, arcname='scripts/' + name)
            package.add(SCRIPTS.parent / 'docker-compose.platform.yml', arcname='docker-compose.platform.yml')
            package.add(SCRIPTS.parent / 'VERSION', arcname='VERSION')
        self.archive = archive.getvalue()
        self.input = b'fixture-ci-token\n' + hashlib.sha256(self.archive).hexdigest().encode() + b'\n' + self.archive
        self.env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ['PATH'],
                        WEAVE_DEPLOY_STATE_DIR=str(self.state), WEAVE_DEPLOY_ENV_FILE=str(self.env_file),
                        MOCK_CALLS=str(self.calls), MOCK_CONTAINERS=str(self.containers), MOCK_MAIN_COMMIT=COMMIT)

    def _write_command(self, name, source):
        command = self.bin / name
        command.write_text('#!' + sys.executable + '\n' + textwrap.dedent(source).lstrip())
        command.chmod(0o700)

    def deploy(self, enabled, running, stop_fails=False, workbench_settings=True):
        self.containers.write_text(json.dumps(running))
        settings = ('WEAVE_ADMIN_PASS=fixture-admin\nWEAVE_API_KEY=fixture-key\n'
                    'WEAVE_RUNTIME_SERVER_URL=http://example.invalid:8080\n'
                    f'WEAVE_WEB_WORKBENCH={str(enabled).lower()}\n')
        if workbench_settings:
            settings += ('WORKBENCH_PUBLIC_AUTHORITY=example.invalid:3080\n'
                         f'WORKBENCH_DATA_PATH={self.data}\nWORKBENCH_WORKSPACE_PATH={self.workspace}\n')
        self.env_file.write_text(settings)
        env = dict(self.env, MOCK_WEB_WORKBENCH=str(enabled).lower(), MOCK_STOP_FAIL=str(stop_fails).lower())
        return subprocess.run(['bash', str(SCRIPTS / 'deploy-main.sh'), COMMIT], input=self.input,
                              capture_output=True, env=env, timeout=20)

    def captured(self):
        return [json.loads(line) for line in self.calls.read_text().splitlines()]

    def assert_storage_preserved(self):
        self.assertEqual((self.data / 'session.json').read_text(), '{"preserved":true}')
        self.assertEqual((self.workspace / 'document.txt').read_text(), 'preserved document')

    def assert_disabled_cutover(self, calls):
        stop = next(i for i, call in enumerate(calls) if call.get('command') == 'stop')
        startup = next(i for i, call in enumerate(calls) if call.get('command') == 'up')
        self.assertLess(stop, startup)
        self.assertEqual(calls[stop]['parameters'], ['workbench-gateway', 'workbench'])
        self.assertEqual(calls[startup]['parameters'][-2:], ['db', 'weave'])
        builds = [call['parameters'] for call in calls if call.get('command') == 'build']
        self.assertEqual(builds, [['weave']])
        self.assertEqual(sum(call.get('command') == 'up' for call in calls), 1)
        self.assertFalse(any(token in call.get('compose', []) for call in calls for token in ('down', 'rm', '--volumes')))

    def test_disabled_deploy_stops_existing_web_services_and_stays_disabled_on_redeploy(self):
        running = {service: True for service in ('db', 'weave', 'workbench', 'workbench-gateway')}
        result = self.deploy(False, running)
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        self.assert_disabled_cutover(self.captured())
        stopped = json.loads(self.containers.read_text())
        self.assertTrue(stopped['weave'])
        self.assertFalse(stopped['workbench'])
        self.assertFalse(stopped['workbench-gateway'])
        self.assert_storage_preserved()
        self.calls.unlink()
        repeated = self.deploy(False, stopped)
        self.assertEqual(repeated.returncode, 0, repeated.stderr.decode())
        self.assert_disabled_cutover(self.captured())
        self.assertEqual(json.loads(self.containers.read_text()), stopped)
        self.assert_storage_preserved()
        receipt = json.loads((self.state / 'last-success.json').read_text())
        self.assertFalse(receipt['web_workbench'])
        self.assertIsNone(receipt['workbench_http_status'])

    def test_disabled_fresh_install_succeeds_without_web_containers_or_storage_settings(self):
        (self.data / 'session.json').unlink()
        self.data.rmdir()
        (self.workspace / 'document.txt').unlink()
        self.workspace.rmdir()
        result = self.deploy(False, {}, workbench_settings=False)
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        self.assert_disabled_cutover(self.captured())
        self.assertEqual(json.loads(self.containers.read_text()), {'db': True, 'weave': True})
        self.assertFalse(self.data.exists())
        self.assertFalse(self.workspace.exists())

    def test_enabled_deploy_keeps_build_and_start_behavior(self):
        result = self.deploy(True, {})
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        calls = self.captured()
        self.assertFalse(any(call.get('command') == 'stop' for call in calls))
        self.assertEqual([call['parameters'] for call in calls if call.get('command') == 'build'], [['weave', 'workbench']])
        startups = [call for call in calls if call.get('command') == 'up']
        self.assertEqual(startups[0]['parameters'][-2:], ['db', 'weave'])
        self.assertEqual(startups[1]['parameters'][-2:], ['workbench', 'workbench-gateway'])
        self.assertTrue(all('--profile' in call['compose'] for call in calls if 'compose' in call))
        receipt = json.loads((self.state / 'last-success.json').read_text())
        self.assertTrue(receipt['web_workbench'])
        self.assertEqual(receipt['workbench_http_status'], 401)
        self.assert_storage_preserved()

    def test_failed_web_shutdown_aborts_before_api_cutover_and_success_receipt(self):
        running = {'db': True, 'weave': True, 'workbench': True, 'workbench-gateway': True}
        result = self.deploy(False, running, stop_fails=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('workbench-shutdown', result.stderr.decode())
        self.assertFalse(any(call.get('command') == 'up' for call in self.captured()))
        self.assertFalse((self.state / 'last-success.json').exists())
        self.assertEqual(json.loads(self.containers.read_text()), running)
        self.assert_storage_preserved()


if __name__ == '__main__':
    unittest.main()
