import pathlib
import subprocess
import sys
import tempfile
import unittest

TOOLS = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(TOOLS / "product"))
from check_product_boundary import check


class ProductBoundaryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = pathlib.Path(temporary.name)
        self.source("go.mod", "module github.com/jinyitao123/weave\n")
        self.source("cmd/weave/bootstrap.go", "package main\n")
        self.source("internal/app/bootstrap/bootstrap.go", "package bootstrap\n")

    def source(self, name, contents):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(contents, encoding="utf-8")
        return path

    def test_retired_weave_ui_cannot_return_as_untracked_nested_source(self):
        self.source("weave-app/src/pages/project/NewPage.tsx", "export {};")
        self.source("internal/app/webui/dist/index.html", "retired")
        errors = check(self.root)
        self.assertEqual(len(errors), 2, errors)
        self.assertTrue(any("weave-app" in error for error in errors))
        self.assertTrue(any("internal/app/webui" in error for error in errors))

    def test_w7_deleted_surfaces_cannot_return(self):
        for name in (
            "workbench/packages/client/index.ts",
            "Dockerfile.workbench",
            "scripts/install-weave.sh",
            "templates/code-review.yaml",
            "internal/build/teambuild/store.go",
            "internal/app/teamtemplates/catalog.go",
            "internal/app/metateam/members.go",
        ):
            self.source(name, "retired")
        errors = check(self.root)
        self.assertEqual(len(errors), 7, errors)

    def test_all_six_retired_documents_are_rejected(self):
        for name in (
            "架构/2026-08-25-Weave-产品方案-十分钟拉起一支业务团队.md",
            "架构/2026-08-26-D5-CLI-MCP-暴露面设计.md",
            "架构/2026-08-28-业务测试方案-RealReplicaBench.md",
            "架构/2026-08-29-Weave-Codex使用方向与产品合同.md",
            "架构/2026-08-29-无UI化-Codex替代Console设计.md",
            "验收/2026-08-29-Codex首次使用验收脚本.md",
        ):
            with self.subTest(document=name):
                path = self.source("docs/" + name, "retired plan")
                self.assertTrue(any(name in error for error in check(self.root)))
                path.unlink()

    def test_retired_backend_implementation_is_rejected_without_a_route(self):
        self.source("internal/app/api/conversations.go", "package api\n")
        self.source("internal/base/realtime/new.go", "package realtime\n")
        self.source("internal/kernel/registry/channels.go", "package registry\n")
        errors = check(self.root)
        self.assertEqual(len(errors), 3, errors)
        self.assertTrue(any("api/conversations.go" in error for error in errors))
        self.assertTrue(any("base/realtime" in error for error in errors))
        self.assertTrue(any("registry/channels.go" in error for error in errors))

    def test_bootstrap_registration_cannot_move_to_another_source_file(self):
        snippets = (
            'var output = map[string]string{"codex_toml": config}',
            'var output = "[mcp_servers.weave]"',
            'var command = "claude mcp add --transport stdio"',
            'type Output struct { MCP string `json:"mcp"` }',
        )
        for name in ("internal/app/bootstrap/new_client.go", "cmd/weave/client_setup.go"):
            for snippet in snippets:
                with self.subTest(source=name, snippet=snippet):
                    path = self.source(name, "package bootstrap\n" + snippet)
                    self.assertTrue(check(self.root))
                    path.unlink()

    def test_runtime_engines_and_developer_tools_are_allowed(self):
        self.source("internal/kernel/engine/codex.go", 'package engine\nconst name = "Codex"')
        self.source(".codex/config.toml", "# developer configuration")
        self.source("internal/app/bootstrap/bootstrap.go",
                    'package bootstrap\n// Codex remains a runtime engine.\n'
                    'type Result struct { APIURL string `json:"api_url"` }')
        self.source("internal/app/bootstrap/bootstrap_test.go",
                    'package bootstrap\nvar forbiddenField = "codex_toml"')
        self.assertEqual(check(self.root), [])

    def test_empty_wrong_or_incomplete_root_fails_closed(self):
        (self.root / "cmd/weave/bootstrap.go").unlink()
        with self.assertRaisesRegex(ValueError, "missing bootstrap"):
            check(self.root)
        self.source("go.mod", "module example.org/unrelated\n")
        with self.assertRaisesRegex(ValueError, "Weave Go module"):
            check(self.root)

    def test_command_fails_when_a_retired_page_returns(self):
        self.source("weave-app/src/pages/InboxPage.tsx", "export {};")
        result = subprocess.run(
            [sys.executable, str(TOOLS / "product/check_product_boundary.py"),
             "--root", str(self.root)], capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 1)
        self.assertIn("weave-app", result.stderr)


if __name__ == "__main__":
    unittest.main()
