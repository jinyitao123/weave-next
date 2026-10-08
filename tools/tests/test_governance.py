import os
import pathlib
import re
import subprocess
import sys
import tempfile
import unittest

TOOLS = pathlib.Path(__file__).resolve().parents[1]
sys.path[:0] = [str(TOOLS), str(TOOLS / "depguard")]
from go_inventory import MODULE, go_inventory
from check_depguard import ALLOWED_EDGES, check
from check_base_dependencies import external_module
from budget.check_band_budget import budget_violations, growth_policy_violations


class BudgetGovernanceTests(unittest.TestCase):
    def test_production_cannot_grow_by_removing_tests(self):
        limit = {"lines": 100, "production_lines": 80, "allowed_dependencies": []}
        self.assertEqual(len(budget_violations("base", {"lines": 100, "production_lines": 81}, set(), limit)), 1)

    def test_dependency_cannot_be_replaced_at_same_count(self):
        limit = {"external_dependencies": 1, "allowed_dependencies": ["approved.org/module"]}
        errors = budget_violations("base", {"external_dependencies": 1}, {"other.org/module"}, limit)
        self.assertIn("not approved", errors[0])

    def test_deleted_code_must_lower_the_budget(self):
        limit = {"lines": 100, "production_lines": 80, "allowed_dependencies": []}
        errors = budget_violations("base", {"lines": 90, "production_lines": 80}, set(), limit)
        self.assertEqual(len(errors), 1)
        self.assertIn("lower it to 90", errors[0])

    def growth_budgets(self, *amendments, kernel_lines=100, recorded=0):
        budgets = {band: {"lines": 0, "production_lines": 0} for band in ("base", "kernel", "build", "app")}
        budgets["kernel"] = {"lines": kernel_lines, "production_lines": 80}
        budgets["growth_policy"] = {"recorded_amendments": recorded, "baseline_lines": 100, "baseline_production_lines": 80}
        budgets["amendments"] = list(amendments)
        return budgets

    def test_total_growth_needs_a_deletion_trigger(self):
        self.assertEqual(growth_policy_violations(self.growth_budgets()), [])
        # Moving code between bands, or deleting as much as is added, needs no amendment.
        moved = self.growth_budgets()
        moved["kernel"] = {"lines": 60, "production_lines": 50}
        moved["app"] = {"lines": 40, "production_lines": 30}
        self.assertEqual(growth_policy_violations(moved), [])
        self.assertIn("exceeds the growth ceiling", growth_policy_violations(self.growth_budgets(kernel_lines=110))[0])
        untriggered = {"date": "2026-10-09", "growth": {"lines": 10, "production_lines": 0}}
        self.assertIn("without a deletion_trigger", growth_policy_violations(self.growth_budgets(untriggered, kernel_lines=110))[0])
        triggered = dict(untriggered, deletion_trigger="delete the old exporter once W6 lands")
        self.assertEqual(growth_policy_violations(self.growth_budgets(triggered, kernel_lines=110)), [])
        # An amendment recorded before the policy, even on the same day, is not re-judged.
        earlier = {"date": "2026-10-09", "reason": "registered before the policy"}
        self.assertEqual(growth_policy_violations(self.growth_budgets(earlier, recorded=1)), [])
        self.assertIn("non-negative growth", growth_policy_violations(self.growth_budgets(earlier, {"date": "2026-10-09"}, recorded=1))[0])
        self.assertIn("non-negative growth", growth_policy_violations(self.growth_budgets({"date": "2026-10-09"}))[0])


class ImportGovernanceTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        (self.root / "go.mod").write_text(f"module {MODULE}\n\ngo 1.26.1\n")
        (self.root / "cmd").mkdir()
        (self.root / "internal/base/example").mkdir(parents=True)

    def source(self, name, contents):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(contents)

    def test_go_syntax_and_all_build_tags_are_scanned(self):
        self.source("internal/base/example/inline_windows.go", '''//go:build windows
package example
import ( renamed "github.com/jinyitao123/weave/internal/app/api" )
''')
        self.source("internal/base/example/raw_test.go", '''package example
import /* explanation */ `github.com/jinyitao123/weave/internal/kernel/engine`
''')
        self.source("internal/base/example/ignored.go", '''package example
/*
import "github.com/jinyitao123/weave/internal/app/api"
*/
var text = `
import "github.com/jinyitao123/weave/internal/app/api"
`
''')
        files = go_inventory(self.root)
        violations, baselines = check(files)
        self.assertEqual(len(files), 3)
        self.assertEqual(len(violations), 2, violations)
        self.assertFalse(baselines)

    def test_unknown_source_directory_is_rejected(self):
        violations, _ = check([{"path": "internal/legacy/main.go", "imports": []}])
        self.assertIn("outside the four bands", violations[0])

    def test_unknown_import_directory_is_rejected(self):
        violations, _ = check([{"path": "internal/app/api/main.go", "imports": [MODULE + "/internal/legacy"]}])
        self.assertIn("unknown internal package band", violations[0])

    def test_no_upward_import_exceptions_remain(self):
        self.assertEqual(ALLOWED_EDGES, set())
        violations, baselines = check([
            {"path": "internal/base/example/main.go", "imports": [MODULE + "/internal/kernel/workflow"]},
            {"path": "internal/kernel/example/main.go", "imports": [MODULE + "/internal/app/api"]},
        ])
        self.assertEqual(len(violations), 2, violations)
        self.assertFalse(baselines)

    def test_downward_and_cmd_imports_are_allowed(self):
        violations, _ = check([
            {"path": "internal/build/teamforge/main.go", "imports": [MODULE + "/internal/base/teamrun"]},
            {"path": "cmd/another/main.go", "imports": [MODULE + "/internal/app/api"]},
        ])
        self.assertFalse(violations)

    def test_malformed_import_fails_closed(self):
        self.source("internal/base/example/main.go", 'package example\nimport ( "unfinished"\n')
        with self.assertRaisesRegex(ValueError, "Go import scan failed"):
            go_inventory(self.root)

    def test_wrong_root_and_module_fail_closed(self):
        with self.assertRaisesRegex(ValueError, "no Go source"):
            go_inventory(self.root)
        (self.root / "go.mod").write_text("module example.org/other\n")
        with self.assertRaisesRegex(ValueError, "go.mod must declare"):
            go_inventory(self.root)

    def test_vendor_fails_closed(self):
        (self.root / "vendor").mkdir()
        with self.assertRaisesRegex(ValueError, "vendor/ is forbidden"):
            go_inventory(self.root)

    def test_base_whitelist_rejects_inline_external_import(self):
        self.source("internal/base/example/main.go", 'package example\nimport ( "example.org/forbidden" )\n')
        result = subprocess.run([
            sys.executable, str(TOOLS / "depguard/check_base_dependencies.py"),
            "--root", str(self.root),
        ], capture_output=True, text=True)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("example.org/forbidden", result.stderr)
        self.assertEqual(external_module("github.com/google/uuid/extra"), "github.com/google/uuid")
        self.assertEqual(external_module("github.com/google/uuid-evil"), "github.com/google/uuid-evil")


class PlatformBuildTests(unittest.TestCase):
    def test_default_server_excludes_executor_layers(self):
        dockerfile = (TOOLS.parent / "Dockerfile").read_text()
        stages = {}
        for stage in re.split(r"(?m)^FROM ", dockerfile)[1:]:
            header, body = stage.split("\n", 1)
            base, keyword, name = header.split()
            self.assertEqual(keyword, "AS")
            stages[name] = (base, body)
        self.assertEqual(name, "server", "plain docker build must select the server")

        def dependencies(target, visited=None):
            visited = set() if visited is None else visited
            if target not in stages or target in visited:
                return visited
            visited.add(target)
            base, body = stages[target]
            for source in [base] + re.findall(r"--from=([\w-]+)", body):
                dependencies(source, visited)
            return visited

        server_stages = dependencies("server")
        executor_stages = dependencies("executor")
        server = "\n".join(stages[name][1] for name in server_stages)
        executor = "\n".join(stages[name][1] for name in executor_stages)
        self.assertNotIn("executor", server_stages)
        self.assertTrue(any(stages[name][0].startswith("node:") for name in server_stages))
        self.assertNotIn("npm install", server)
        for package in ("opencode-ai@", "@openai/codex@", "@anthropic-ai/claude-code@"):
            self.assertNotIn(package, server)
            self.assertIn(package, executor)
        for path in ("/weave /usr/local/bin/weave", "/dist/runtime /dist/runtime"):
            self.assertIn("COPY --from=builder " + path, server)
        for platform in ("linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"):
            self.assertIn(platform, server)
        self.assertIn("ENV WEAVE_RUNTIME_DIST_DIR=/dist/runtime", server)
        self.assertIn('ENTRYPOINT ["weave"]', server)


class PlatformComposeTests(unittest.TestCase):
    def test_first_boot_and_runtime_profile(self):
        # Explicit opt-in: ordinary unit checks do not require Docker CLI.
        if os.environ.get("WEAVE_TEST_COMPOSE") != "1":
            self.skipTest("run make compose-check to validate platform configuration")
        import json
        env = dict(os.environ, JWT_SECRET="ci", WEAVE_ADMIN_PASS="ci",
                   WEAVE_SECRET_KEY="0" * 64, BUILD_COMMIT="governance-check")
        env.pop("WEAVE_RUNTIME_TOKEN", None)
        env.pop("WEAVE_PLATFORM_IMAGE", None)
        env.pop("WEAVE_RUNTIME_IMAGE", None)
        command = ["docker", "compose", "--env-file", os.devnull, "-f",
                   str(TOOLS.parent / "docker-compose.platform.yml")]
        default = subprocess.run(command + ["config", "--format", "json"],
                                 env=env, capture_output=True, text=True, check=True)
        default_services = json.loads(default.stdout)["services"]
        self.assertEqual(sorted(default_services), ["db", "weave"])
        weave_env = default_services["weave"]["environment"]
        self.assertNotIn("WEAVE_DISABLE_LOCAL_LOGIN", weave_env)
        self.assertEqual(weave_env["WEAVE_LOCAL_RUNTIME_ENABLED"], "false")
        self.assertNotIn("WEAVE_METATEAM_ENABLED", weave_env)
        self.assertNotIn("WEAVE_RETIRE_LEGACY_PLATFORM_APIS", weave_env)
        for profile in [[], ["--profile", "runtime"]]:
            result = subprocess.run(command + profile + ["config", "--format", "json"],
                                    env=env, capture_output=True, text=True, check=True)
            services = json.loads(result.stdout)["services"]
            self.assertNotIn("workbench", services)
            self.assertEqual(services["weave"]["build"]["target"], "server")
            self.assertEqual(services["weave"]["image"], "weave-platform")
            self.assertNotIn("OPENCODE_VERSION", services["weave"]["build"]["args"])
            self.assertEqual(services["weave"]["build"]["args"]["BUILD_COMMIT"], "governance-check")
            self.assertEqual(services["weave"]["build"]["args"]["WEAVE_VERSION"], "0.1.0-dev")
            self.assertEqual(services["weave"]["build"]["args"]["HTTP_PROXY"], "")
            self.assertEqual(services["weave"]["build"]["args"]["http_proxy"], "")
            if "runtime" in profile:
                self.assertEqual(services["runtime"]["build"]["target"], "executor")
                self.assertEqual(services["runtime"]["image"], "weave-executor")
                self.assertEqual(services["runtime"]["build"]["args"]["BUILD_COMMIT"], "governance-check")
                self.assertIn("OPENCODE_VERSION", services["runtime"]["build"]["args"])
                self.assertEqual(services["runtime"]["environment"]["WEAVE_RUNTIME_TOKEN"], "")
                self.assertEqual(services["runtime"]["environment"]["HTTP_PROXY"], "")
                self.assertIn("weave", services["runtime"]["environment"]["NO_PROXY"])
        env["WEAVE_RUNTIME_TOKEN"] = "rtk_ci"
        env["WEAVE_RUNTIME_IMAGE"] = "registry.example.test/weave-executor:operator-selected"
        result = subprocess.run(command + ["--profile", "runtime", "config", "--format", "json"],
                                env=env, capture_output=True, text=True, check=True)
        runtime = json.loads(result.stdout)["services"]["runtime"]
        self.assertEqual(runtime["environment"]["WEAVE_RUNTIME_TOKEN"], "rtk_ci")
        self.assertEqual(runtime["image"], env["WEAVE_RUNTIME_IMAGE"])
        self.assertNotIn("--runtime-token", runtime["command"])


if __name__ == "__main__":
    unittest.main()
