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


class DeploymentHarness(unittest.TestCase):
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
        self._write_command('flock', 'import sys\nsys.exit(0)\n')
        self._write_command('stat', 'import os, sys\nprint(os.stat(sys.argv[-1]).st_size)\n')
        self._write_command('sha256sum', 'import hashlib, pathlib, sys\n'
                            'print(hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).hexdigest(), sys.argv[1])\n')
        self._write_command('sudo', '''
            import json, os, pathlib, sys
            args = sys.argv[1:]
            if args == ['docker', 'info', '--format', '{{.DockerRootDir}}']:
                print(os.environ.get('MOCK_DOCKER_ROOT', '/fixture/docker'))
                raise SystemExit(0)
            if args[:6] == ['env', 'LC_ALL=C', 'df', '-B1', '--output=avail', '--']:
                previous = pathlib.Path(os.environ['MOCK_CALLS'])
                calls = [json.loads(line) for line in previous.read_text().splitlines()] if previous.exists() else []
                value = os.environ.get('MOCK_DF_AVAILABLE', '17179869184')
                if args[-1] == '/fixture/docker':
                    value = os.environ.get('MOCK_DOCKER_AVAILABLE', value)
                if args[-1].endswith('/backups'):
                    value = os.environ.get('MOCK_BACKUP_AVAILABLE', value)
                if any(call.get('command') == 'build' for call in calls):
                    value = os.environ.get('MOCK_DF_AFTER_BUILD', value)
                if any(call.get('command') == 'exec' for call in calls):
                    value = os.environ.get('MOCK_DF_AFTER_BACKUP', value)
                with open(os.environ['MOCK_CALLS'], 'a') as capture:
                    capture.write(json.dumps({'capacity_path': args[-1]}) + '\\n')
                print('Avail')
                print(value)
                raise SystemExit(int(os.environ.get('MOCK_DF_EXIT', '0')))
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
            elif command == 'up':
                for service in ('db', 'weave'):
                    if service in parameters:
                        containers[service] = True
            state_path.write_text(json.dumps(containers))
        ''')
        self._write_command('python3', '''
            import importlib.util, json, os, pathlib, sys
            if sys.argv[1] == '-':
                code = sys.stdin.read()
                if 'api.github.com/repos/jinyitao123/weave-next/branches/main' in code:
                    with open(os.environ['MOCK_CALLS'], 'a') as capture:
                        capture.write(json.dumps({'main_check': os.environ['MOCK_MAIN_COMMIT']}) + '\\n')
                    print(os.environ['MOCK_MAIN_COMMIT'])
                    raise SystemExit(0)
                sys.argv = sys.argv[1:]
                exec(compile(code, '<deployment-input>', 'exec'))
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
            def urlopen(request, timeout):
                raise AssertionError('Deployment contacted an unexpected HTTP endpoint')
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

    def deploy(self, running):
        self.containers.write_text(json.dumps(running))
        self.env_file.write_text('WEAVE_ADMIN_PASS=fixture-admin\nWEAVE_API_KEY=fixture-key\n'
                                 'WEAVE_RUNTIME_SERVER_URL=http://example.invalid:8080\n')
        return subprocess.run(['bash', str(SCRIPTS / 'deploy-main.sh'), COMMIT], input=self.input,
                              capture_output=True, env=self.env, timeout=20)

    def captured(self):
        return [json.loads(line) for line in self.calls.read_text().splitlines()] if self.calls.exists() else []

    def assert_api_cutover(self, calls):
        self.assertFalse(any(call.get('command') == 'stop' for call in calls))
        startups = [call for call in calls if call.get('command') == 'up']
        self.assertEqual(len(startups), 1)
        self.assertEqual(startups[0]['parameters'][-2:], ['db', 'weave'])
        builds = [call['parameters'] for call in calls if call.get('command') == 'build']
        self.assertEqual(builds, [['weave']])
        self.assertFalse(any('--profile' in call.get('compose', []) for call in calls))
        self.assertFalse(any(token in call.get('compose', []) for call in calls for token in ('down', 'rm', '--volumes')))


class DeployMainTests(DeploymentHarness):

    def assert_no_cutover(self, result, build=False, backup=False):
        self.assertNotEqual(result.returncode, 0)
        commands = [call.get('command') for call in self.captured()]
        self.assertEqual('build' in commands, build)
        self.assertEqual('exec' in commands, backup)
        self.assertNotIn('up', commands)
        self.assertFalse((self.state / 'last-success.json').exists())

    def test_build_refuses_low_state_capacity(self):
        self.env['MOCK_DF_AVAILABLE'] = str(8 * 1024**3 - 1)
        self.assert_no_cutover(self.deploy({'db': True, 'weave': True}))

    def test_build_refuses_low_separate_docker_capacity(self):
        self.env['MOCK_DOCKER_AVAILABLE'] = '0'
        self.assert_no_cutover(self.deploy({'db': True, 'weave': True}))
        self.assertIn('/fixture/docker', [call.get('capacity_path') for call in self.captured()])

    def test_build_refuses_low_separate_backup_capacity(self):
        self.env['MOCK_BACKUP_AVAILABLE'] = '0'
        self.assert_no_cutover(self.deploy({'db': True, 'weave': True}))

    def test_backup_refuses_capacity_consumed_by_build(self):
        self.env['MOCK_DF_AFTER_BUILD'] = str(1024**3 - 1)
        self.assert_no_cutover(self.deploy({'db': True, 'weave': True}), build=True)

    def test_cutover_refuses_capacity_consumed_by_backup(self):
        self.env['MOCK_DF_AFTER_BACKUP'] = '0'
        self.assert_no_cutover(self.deploy({'db': True, 'weave': True}), build=True, backup=True)

    def test_unknown_or_malformed_capacity_fails_closed(self):
        for value in ('', 'unknown', '-1', '1.5', '99999999999\\n99999999999'):
            with self.subTest(value=value):
                self.env['MOCK_DF_AVAILABLE'] = value
                if self.calls.exists():
                    self.calls.unlink()
                self.assert_no_cutover(self.deploy({}))

    def test_failed_df_fails_closed(self):
        self.env['MOCK_DF_EXIT'] = '1'
        self.assert_no_cutover(self.deploy({}))

    def test_unknown_docker_root_fails_closed(self):
        self.env['MOCK_DOCKER_ROOT'] = ''
        self.assert_no_cutover(self.deploy({}))

    def test_thresholds_require_positive_integer_bytes(self):
        for name in ('WEAVE_DEPLOY_MIN_BUILD_FREE_BYTES', 'WEAVE_DEPLOY_MIN_ACTIVATE_FREE_BYTES'):
            for value in ('', '0', '-1', '1.5', 'unknown'):
                with self.subTest(name=name, value=value):
                    self.env[name] = value
                    self.assert_no_cutover(self.deploy({}))
            self.env.pop(name)

    def test_exact_thresholds_allow_original_path(self):
        self.env['MOCK_DF_AVAILABLE'] = str(8 * 1024**3)
        self.env['MOCK_DF_AFTER_BUILD'] = str(1024**3)
        result = self.deploy({'db': True, 'weave': True})
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        self.assert_api_cutover(self.captured())
        paths = [call.get('capacity_path') for call in self.captured() if 'capacity_path' in call]
        self.assertEqual(paths, [str(self.state), str(self.state / 'backups'), '/fixture/docker'] * 3)

    def test_deploy_builds_and_starts_only_the_api_and_is_repeatable(self):
        result = self.deploy({'db': True, 'weave': True})
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        self.assert_api_cutover(self.captured())
        self.calls.unlink()
        repeated = self.deploy(json.loads(self.containers.read_text()))
        self.assertEqual(repeated.returncode, 0, repeated.stderr.decode())
        self.assert_api_cutover(self.captured())
        receipt = json.loads((self.state / 'last-success.json').read_text())
        self.assertEqual(receipt['commit'], COMMIT)
        self.assertNotIn('web_workbench', receipt)
        release_env = (self.state / 'releases' / (COMMIT + '.env')).read_text()
        self.assertNotIn('WORKBENCH', release_env)

    def test_fresh_install_succeeds_without_running_containers(self):
        result = self.deploy({})
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        self.assert_api_cutover(self.captured())
        self.assertEqual(json.loads(self.containers.read_text()), {'db': True, 'weave': True})
        connection = json.loads((self.state / 'connection.json').read_text())
        self.assertNotIn('workbench_url', connection)


if __name__ == '__main__':
    unittest.main()
