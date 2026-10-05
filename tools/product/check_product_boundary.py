"""Keep retired product surfaces out of the Weave execution backend."""

import argparse
import pathlib
import re
import sys


RETIRED_PATHS = (
    "deploy/docker-compose.yml",
    "deploy/Dockerfile.weave-prebuilt",
    "deploy/.env.example",
    "internal/app/schedules",
    "internal/app/api/schedules.go",
    "internal/app/api/agentsched.go",
    "console",
    "console-v2",
    "workbench",
    "Dockerfile.workbench",
    "scripts/install-weave.sh",
    "scripts/capability-browser-acceptance.mjs",
    "templates",
    "internal/build",
    "internal/app/teamtemplates",
    "internal/app/teamevaluations",
    "internal/app/teamrestore",
    "internal/app/teamassets",
    "internal/app/metateam",
    "internal/app/designseed",
    *(
        f"internal/app/teamconstruction/{name}.go"
        for name in (
            "candidate_evidence", "compiler_operations", "dispatch", "phases",
            "publication_build_effect",
        )
    ),
    "weave-app",
    "internal/app/webui",
    "scripts/sync-webui.py",
    *(
        f"internal/app/api/{name}.go"
        for name in (
            "conversations", "threads", "flags", "mcp_proxy", "projects",
            "project_resources", "project_memories", "sessions", "channels",
            "events", "fork", "sources", "task_groups",
            "retired_routes", "team_assembler", "team_blueprint_planning",
            "team_build_authorization_token", "team_build_run_control",
            "team_build_runs", "team_declarative_workflow_planning",
            "team_evaluations", "team_templates", "teamforge_wiring",
            "terminal_outcome",
        )
    ),
    "internal/base/realtime",
    "internal/app/projects/resources.go",
    "internal/app/projects/collaborators.go",
    "internal/kernel/registry/channels.go",
    "docs/架构/2026-08-25-Weave-产品方案-十分钟拉起一支业务团队.md",
    "docs/架构/2026-08-26-D5-CLI-MCP-暴露面设计.md",
    "docs/架构/2026-08-28-业务测试方案-RealReplicaBench.md",
    "docs/架构/2026-08-29-Weave-Codex使用方向与产品合同.md",
    "docs/架构/2026-08-29-无UI化-Codex替代Console设计.md",
    "docs/验收/2026-08-29-Codex首次使用验收脚本.md",
)

# Match registration contracts in bootstrap source, not engine or developer names.
REGISTRATION_PATTERNS = (
    ("independent client registration fields", r'\b(?:codex_toml|claude_command)\b'),
    ("Codex MCP registration", r'\[mcp_servers\.weave\]'),
    ("Claude MCP registration", r'\bclaude\s+mcp\s+add\b'),
    ("Codex client configuration", r'\.codex/config\.toml'),
    ("bootstrap MCP registration output", r'json:\s*"mcp(?:,|\")'),
    ("retired bootstrap registration helper", r'\bregistrationSnippets\s*\('),
)


def check(root: pathlib.Path) -> list[str]:
    module = root / "go.mod"
    if not module.is_file() or not re.search(
        r"^module\s+github\.com/jinyitao123/weave\s*$",
        module.read_text(encoding="utf-8"), re.MULTILINE,
    ):
        raise ValueError("root must be the Weave Go module")
    for required in ("cmd/weave/bootstrap.go", "internal/app/bootstrap/bootstrap.go"):
        if not (root / required).is_file():
            raise ValueError(f"missing bootstrap source: {required}")

    def has_source(path: pathlib.Path) -> bool:
        # Git cannot retain empty directories; a deleted checkout can leave
        # those behind without restoring any product source.
        return path.is_symlink() or path.is_file() or (
            path.is_dir() and any(p.is_file() or p.is_symlink() for p in path.rglob("*"))
        )

    errors = [f"retired product path exists: {name}" for name in RETIRED_PATHS
              if has_source(root / name)]
    bootstrap_sources = set((root / "internal/app/bootstrap").rglob("*.go"))
    bootstrap_sources.update((root / "cmd/weave").rglob("*.go"))
    for source in sorted(bootstrap_sources):
        if source.name.endswith("_test.go"):
            continue
        contents = source.read_text(encoding="utf-8")
        for label, pattern in REGISTRATION_PATTERNS:
            if re.search(pattern, contents):
                errors.append(f"{source.relative_to(root)}: {label}")
    return errors


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, type=pathlib.Path)
    args = parser.parse_args()
    try:
        errors = check(args.root.resolve())
    except (OSError, UnicodeError, ValueError) as error:
        print(f"product boundary: {error}", file=sys.stderr)
        return 1
    if errors:
        for error in errors:
            print(error, file=sys.stderr)
        return 1
    print("product boundary: passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
