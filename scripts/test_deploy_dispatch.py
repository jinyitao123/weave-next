import hashlib
import io
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

from test_deploy_main import COMMIT, DeploymentHarness, SCRIPTS


SCRIPT = SCRIPTS / 'deploy-dispatch.sh'
RUNNER_SHA = hashlib.sha256((SCRIPTS / 'deploy-main.sh').read_bytes()).hexdigest()


class DeployDispatchProtocolTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.dispatch = self.root / 'dispatch.sh'
        self.dispatch.write_text(SCRIPT.read_text())
        self.dispatch.chmod(0o700)
        self.marker = self.root / 'old-runner-executed'
        runner = self.root / 'deploy-main.sh'
        runner.write_text('#!/bin/sh\ntouch "' + str(self.marker) + '"\n')
        runner.chmod(0o700)

    def run_dispatch(self, command):
        env = dict(os.environ, SSH_ORIGINAL_COMMAND=command)
        return subprocess.run(['bash', str(self.dispatch)], input=b'', capture_output=True, env=env, timeout=5)

    def test_rejects_shell_text_before_running_any_deployment_code(self):
        result = self.run_dispatch(f'deploy {COMMIT} {RUNNER_SHA}; touch {self.marker}')
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.marker.exists())

    def test_rejects_unversioned_legacy_protocol(self):
        result = self.run_dispatch('deploy ' + COMMIT)
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.marker.exists())

    def test_new_versioned_protocol_is_rejected_by_legacy_gate_before_old_runner(self):
        legacy = self.root / 'legacy-dispatch.sh'
        legacy.write_text('#!/bin/bash\nset -eu\n'
                          '[[ "${SSH_ORIGINAL_COMMAND:-}" =~ ^deploy[[:space:]]+([0-9a-f]{40})$ ]] || exit 2\n'
                          'exec "' + str(self.root / 'deploy-main.sh') + '" "${BASH_REMATCH[1]}"\n')
        result = subprocess.run(['/bin/bash', str(legacy)], capture_output=True,
                                env=dict(os.environ, SSH_ORIGINAL_COMMAND=f'deploy {COMMIT} {RUNNER_SHA}'), timeout=5)
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.marker.exists())

    def test_requires_complete_commit_and_runner_digests(self):
        for command in (f'deploy {COMMIT[:7]} {RUNNER_SHA}', f'deploy {COMMIT} {RUNNER_SHA[:7]}'):
            result = self.run_dispatch(command)
            self.assertEqual(result.returncode, 2)
            self.assertFalse(self.marker.exists())


class SourceRunnerTransitionTests(DeploymentHarness):
    def setUp(self):
        super().setUp()
        self.dispatch = self.state / 'dispatch.sh'
        self.dispatch.write_text(SCRIPT.read_text())
        self.dispatch.chmod(0o700)
        self.marker = self.state / 'legacy-runner-executed'
        self.old_runner = self.state / 'deploy-main.sh'
        # The installed pre-cutover runner is stale local code. The new entry
        # must never execute this local file.
        self.old_runner.write_text('#!/bin/bash\nset -eu\ntouch "' + str(self.marker) + '"\n'
                                   'sudo docker compose up -d workbench workbench-gateway\n')
        self.old_runner.chmod(0o700)
        self.old_runner_bytes = self.old_runner.read_bytes()
        self._write_command('bash', '''
            import json, os, pathlib, subprocess, sys
            # Both the dispatcher and archive runner are genuinely interpreted
            # by bash; this double captures which executable bytes were used.
            target = pathlib.Path(sys.argv[1])
            with open(os.environ['MOCK_CALLS'], 'a') as capture:
                import hashlib
                capture.write(json.dumps({'executed_script': str(target), 'sha256': hashlib.sha256(target.read_bytes()).hexdigest()}) + '\\n')
            raise SystemExit(subprocess.run(['/bin/bash', *sys.argv[1:]]).returncode)
        ''')

    def deploy_source(self, payload=None, runner_sha=RUNNER_SHA, main_commit=COMMIT):
        self.containers.write_text('{"db":true,"weave":true}')
        self.env_file.write_text('WEAVE_ADMIN_PASS=fixture-admin\nWEAVE_API_KEY=fixture-key\n'
                                 'WEAVE_RUNTIME_SERVER_URL=http://example.invalid:8080\n')
        env = dict(self.env, MOCK_MAIN_COMMIT=main_commit,
                   SSH_ORIGINAL_COMMAND=f'deploy {COMMIT} {runner_sha}')
        return subprocess.run(['/bin/bash', str(self.dispatch)], input=self.input if payload is None else payload,
                              capture_output=True, env=env, timeout=20)

    def test_old_installed_runner_is_bypassed_in_first_source_cutover_and_next_deploy(self):
        first = self.deploy_source()
        self.assertEqual(first.returncode, 0, first.stderr.decode())
        self.assertFalse(self.marker.exists())
        calls = self.captured()
        self.assert_api_cutover(calls)
        executed = [call for call in calls if 'executed_script' in call]
        self.assertEqual(len(executed), 1)
        self.assertIn('.source-runner.', executed[0]['executed_script'])
        self.assertEqual(executed[0]['sha256'], RUNNER_SHA)
        self.assertIn(f'Executing verified source runner {COMMIT} ({RUNNER_SHA})', first.stdout.decode())
        self.assertEqual(sum('main_check' in call for call in calls), 3)
        self.assertEqual(self.old_runner.read_bytes(), (SCRIPTS / 'deploy-main.sh').read_bytes())
        self.assertFalse(list(self.state.glob('.source-runner.*')))
        self.calls.unlink()
        second = self.deploy_source()
        self.assertEqual(second.returncode, 0, second.stderr.decode())
        self.assert_api_cutover(self.captured())
        self.assertFalse(self.marker.exists())
        self.assertFalse(list(self.state.glob('.source-runner.*')))

    def assert_no_deployment_executed(self):
        calls = self.captured() if self.calls.exists() else []
        self.assertFalse(any('executed_script' in call or 'command' in call for call in calls))
        self.assertFalse(self.marker.exists())
        self.assertEqual(self.old_runner.read_bytes(), self.old_runner_bytes)
        self.assertFalse((self.state / 'last-success.json').exists())
        self.assertFalse(list(self.state.glob('.source-runner.*')))

    def test_bad_archive_digest_never_executes_or_replaces_any_runner(self):
        payload = b'fixture-ci-token\n' + b'0' * 64 + b'\n' + self.archive
        result = self.deploy_source(payload=payload)
        self.assertEqual(result.returncode, 2)
        self.assert_no_deployment_executed()

    def test_bad_runner_digest_never_executes_or_replaces_any_runner(self):
        result = self.deploy_source(runner_sha='0' * 64)
        self.assertEqual(result.returncode, 2)
        self.assert_no_deployment_executed()

    def test_changed_main_never_executes_source_or_installed_runner(self):
        result = self.deploy_source(main_commit='a' * 40)
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        self.assertIn('superseded before source runner execution', result.stdout.decode())
        self.assert_no_deployment_executed()

    def test_incomplete_source_archive_cannot_execute_its_valid_runner(self):
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode='w:gz') as package:
            package.add(SCRIPTS / 'deploy-main.sh', arcname='scripts/deploy-main.sh')
        raw = archive.getvalue()
        payload = b'fixture-ci-token\n' + hashlib.sha256(raw).hexdigest().encode() + b'\n' + raw
        result = self.deploy_source(payload=payload)
        self.assertNotEqual(result.returncode, 0)
        self.assert_no_deployment_executed()

    def test_runner_must_be_unique_regular_and_bounded_in_verified_archive(self):
        for scenario in ('missing', 'duplicate', 'symlink', 'oversized'):
            with self.subTest(scenario=scenario):
                archive = io.BytesIO()
                with tarfile.open(fileobj=archive, mode='w:gz') as package:
                    package.add(SCRIPTS / 'deployment-state.py', arcname='scripts/deployment-state.py')
                    package.add(SCRIPTS.parent / 'VERSION', arcname='VERSION')
                    package.add(SCRIPTS.parent / 'docker-compose.platform.yml', arcname='docker-compose.platform.yml')
                    if scenario == 'duplicate':
                        package.add(SCRIPTS / 'deploy-main.sh', arcname='scripts/deploy-main.sh')
                        package.add(SCRIPTS / 'deploy-main.sh', arcname='scripts/deploy-main.sh')
                    elif scenario == 'symlink':
                        member = tarfile.TarInfo('scripts/deploy-main.sh')
                        member.type, member.linkname = tarfile.SYMTYPE, '/tmp/untrusted-runner'
                        package.addfile(member)
                    elif scenario == 'oversized':
                        body = b'x' * (128 * 1024 + 1)
                        member = tarfile.TarInfo('scripts/deploy-main.sh')
                        member.size = len(body)
                        package.addfile(member, io.BytesIO(body))
                raw = archive.getvalue()
                payload = b'fixture-ci-token\n' + hashlib.sha256(raw).hexdigest().encode() + b'\n' + raw
                result = self.deploy_source(payload=payload)
                self.assertNotEqual(result.returncode, 0)
                self.assert_no_deployment_executed()


if __name__ == '__main__':
    unittest.main()
