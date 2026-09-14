import base64
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "bootstrap-server-deploy.sh"
PUBLIC_KEY = "ssh-ed25519 " + "A" * 43 + "= weave-server-test"
SERVER_ENV = """POSTGRES_PASSWORD=postgres-test
JWT_SECRET=jwt-test
WEAVE_SECRET_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
WEAVE_ADMIN_PASS=admin-test
WORKBENCH_PUBLIC_AUTHORITY=example.test:3080
WORKBENCH_DATA_PATH=/tmp/workbench-data
WORKBENCH_WORKSPACE_PATH=/tmp/workbench-workspaces
WORKBENCH_BIND_ADDRESS=0.0.0.0
OPENAI_BASE_URL=https://example.test/v1
OPENAI_API_KEY=model-test
OPENAI_MODELS=gpt-test
DEFAULT_MODEL=gpt-test
"""


class BootstrapServerDeployTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.home = Path(self.directory.name)
        self.environment = {
            **os.environ,
            "HOME": str(self.home),
            "WEAVE_SERVER_REPOSITORY_URL": "https://github.com/jinyitao123/weave-server.git",
        }

    def run_bootstrap(self, key=PUBLIC_KEY, server_env=SERVER_ENV, check=True):
        return subprocess.run(
            [str(SCRIPT)],
            input=key + "\n" + base64.b64encode(server_env.encode()).decode() + "\n",
            text=True,
            env=self.environment,
            check=check,
            capture_output=True,
        )

    def test_creates_isolated_configuration_and_restricted_key(self):
        self.run_bootstrap()
        self.run_bootstrap()
        state = self.home / ".local/share/weave-server-deploy"
        config = self.home / ".config/weave-server"
        authorized = (self.home / ".ssh/authorized_keys").read_text().splitlines()
        self.assertEqual(len(authorized), 1)
        self.assertIn("restrict,command=", authorized[0])
        self.assertTrue((state / "first-release.sh").stat().st_mode & 0o111)
        self.assertEqual((config / "server.env").read_text(), SERVER_ENV)
        self.assertEqual(
            subprocess.check_output(
                ["git", "-C", str(self.home / "weave-server-source"), "remote", "get-url", "origin"],
                text=True,
            ).strip(),
            "https://github.com/jinyitao123/weave-server.git",
        )


    def test_rejects_a_different_existing_environment(self):
        self.run_bootstrap()
        result = self.run_bootstrap(server_env=SERVER_ENV.replace("gpt-test", "other-model"), check=False)
        self.assertNotEqual(result.returncode, 0)

    def test_rejects_key_already_bound_to_another_command(self):
        ssh = self.home / ".ssh"
        ssh.mkdir()
        (ssh / "authorized_keys").write_text("command=\"old\" " + PUBLIC_KEY + "\n")
        result = self.run_bootstrap(check=False)
        self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
