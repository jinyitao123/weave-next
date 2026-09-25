import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('deploy-dispatch.sh')
COMMIT = '9c63ea7c7c75055b5973d3f44c861668b9b597ba'


class DeployDispatchTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.state = self.root / 'state'
        self.state.mkdir()
        self.dispatch = self.state / 'dispatch.sh'
        self.dispatch.write_text(SCRIPT.read_text())
        self.dispatch.chmod(0o700)
        self.runner = self.state / 'deploy-main.sh'
        self.capture = self.root / 'runner-input.txt'
        self.runner.write_text(
            '#!/usr/bin/env python3\n'
            'import os, pathlib, sys\n'
            'pathlib.Path(os.environ["CAPTURE"]).write_text(sys.argv[1] + "\\n" + sys.stdin.read())\n'
        )
        self.runner.chmod(0o700)
        self.env = dict(os.environ, CAPTURE=str(self.capture))

    def run_dispatch(self, command, stdin='short-lived-token'):
        env = dict(self.env, SSH_ORIGINAL_COMMAND=command)
        return subprocess.run([str(self.dispatch)], input=stdin, text=True, capture_output=True, env=env)

    def test_dispatches_only_the_exact_full_sha_and_preserves_stdin(self):
        result = self.run_dispatch('deploy ' + COMMIT)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.capture.read_text(), COMMIT + '\nshort-lived-token')

    def test_rejects_shell_text_before_running_deploy_script(self):
        marker = self.root / 'should-not-exist'
        command = f'deploy {COMMIT}; touch {marker}'
        result = self.run_dispatch(command)
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.capture.exists())
        self.assertFalse(marker.exists())

    def test_rejects_an_unavailable_runner(self):
        self.runner.unlink()
        result = self.run_dispatch('deploy ' + COMMIT)
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.capture.exists())


if __name__ == '__main__':
    unittest.main()
