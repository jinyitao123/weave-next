import hashlib
import os
from pathlib import Path
import subprocess
import unittest

from test_deploy_main import DeploymentHarness, SCRIPTS


class ReviewedEntryUpdateTests(DeploymentHarness):
    def setUp(self):
        super().setUp()
        self.repository = self.root / 'reviewed-source'
        script_dir = self.repository / 'scripts'
        script_dir.mkdir(parents=True)
        for name in ('update-deploy-runner.sh', 'deploy-main.sh', 'deploy-dispatch.sh'):
            (script_dir / name).write_bytes((SCRIPTS / name).read_bytes())
        self.tool = script_dir / 'update-deploy-runner.sh'
        for args in (('init', '-q'), ('config', 'user.email', 'fixture@example.invalid'),
                     ('config', 'user.name', 'Fixture'), ('add', 'scripts'), ('commit', '-q', '-m', 'Reviewed entry fixture')):
            subprocess.run(['git', '-C', str(self.repository), *args], check=True, capture_output=True)
        self.commit = subprocess.check_output(['git', '-C', str(self.repository), 'rev-parse', 'HEAD'], text=True).strip()
        self.expected_runner = (script_dir / 'deploy-main.sh').read_bytes()
        self.expected_dispatch = (script_dir / 'deploy-dispatch.sh').read_bytes()
        # Dirty local copies are deliberately different. Only the supplied Git
        # commit may be installed by the operator's one-time gate migration.
        (script_dir / 'deploy-main.sh').write_text('unreviewed local runner\n')
        (script_dir / 'deploy-dispatch.sh').write_text('unreviewed local dispatcher\n')
        self.runner = self.state / 'deploy-main.sh'
        self.dispatch = self.state / 'dispatch.sh'
        self.runner.write_text('#!/bin/sh\necho old-unconditional-web-runner\n')
        self.dispatch.write_text('#!/bin/sh\necho old-two-field-dispatcher\n')
        self.old_runner = self.runner.read_bytes()
        self.old_dispatch = self.dispatch.read_bytes()
        self._write_command('ssh', '''
            import os, pathlib, subprocess, sys
            command = sys.argv[-1].replace('/home/ubuntu/.local/share/weave-deploy', os.environ['MOCK_REMOTE_STATE'])
            if 'flock -w 1800' in command and os.environ.get('MOCK_REMOTE_CHANGED') == 'true':
                (pathlib.Path(os.environ['MOCK_REMOTE_STATE']) / 'dispatch.sh').write_text('concurrent reviewed update\\n')
            raise SystemExit(subprocess.run(['/bin/bash', '-c', command]).returncode)
        ''')
        self._write_command('scp', '''
            import os, pathlib, sys
            source = pathlib.Path(sys.argv[-2])
            destination = sys.argv[-1].split(':', 1)[1].replace('/home/ubuntu/.local/share/weave-deploy', os.environ['MOCK_REMOTE_STATE'])
            body = source.read_bytes()
            if os.environ.get('MOCK_CORRUPT_UPLOAD') == 'true':
                body += b'corrupt-transfer'
            pathlib.Path(destination).write_bytes(body)
        ''')

    def update(self, source=None, expected_runner=None, expected_dispatch=None, **overrides):
        env = dict(self.env, WEAVE_DEPLOY_BOOTSTRAP_TARGET='fixture-host', MOCK_REMOTE_STATE=str(self.state), **overrides)
        return subprocess.run(['/bin/bash', str(self.tool),
                               expected_runner or hashlib.sha256(self.old_runner).hexdigest(),
                               expected_dispatch or hashlib.sha256(self.old_dispatch).hexdigest(),
                               source or self.commit], env=env, capture_output=True, timeout=15)

    def test_installs_only_reviewed_git_bytes_for_both_entry_files(self):
        result = self.update()
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        self.assertEqual(self.runner.read_bytes(), self.expected_runner)
        self.assertEqual(self.dispatch.read_bytes(), self.expected_dispatch)
        self.assertEqual(self.runner.stat().st_mode & 0o777, 0o700)
        self.assertEqual(self.dispatch.stat().st_mode & 0o777, 0o700)
        self.assertIn(self.commit, result.stdout.decode())
        self.assert_storage_preserved()

    def test_wrong_source_commit_does_not_transfer_or_install_anything(self):
        result = self.update(source='0' * 40)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.runner.read_bytes(), self.old_runner)
        self.assertEqual(self.dispatch.read_bytes(), self.old_dispatch)
        self.assertFalse((self.state / 'deploy-main.sh.next').exists())
        self.assertFalse((self.state / 'dispatch.sh.next').exists())

    def test_preflight_hash_mismatch_preserves_both_installed_files(self):
        result = self.update(expected_dispatch='0' * 64)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.runner.read_bytes(), self.old_runner)
        self.assertEqual(self.dispatch.read_bytes(), self.old_dispatch)
        self.assertFalse((self.state / 'deploy-main.sh.next').exists())

    def test_upload_checksum_mismatch_preserves_both_installed_files(self):
        result = self.update(MOCK_CORRUPT_UPLOAD='true')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.runner.read_bytes(), self.old_runner)
        self.assertEqual(self.dispatch.read_bytes(), self.old_dispatch)

    def test_compare_and_swap_rechecks_under_lock_before_replacing_either_file(self):
        result = self.update(MOCK_REMOTE_CHANGED='true')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.runner.read_bytes(), self.old_runner)
        self.assertEqual(self.dispatch.read_text(), 'concurrent reviewed update\n')


if __name__ == '__main__':
    unittest.main()
