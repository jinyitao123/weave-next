"""Shared Go-parser inventory for all repository governance checks."""

import json
import pathlib
import re
import subprocess

MODULE = "github.com/jinyitao123/weave"


def go_inventory(root: pathlib.Path) -> list[dict]:
    module_file = root / "go.mod"
    if not module_file.is_file() or not re.search(
        rf'^module\s+{re.escape(MODULE)}\s*$',
        module_file.read_text(encoding="utf-8"), re.MULTILINE,
    ):
        raise ValueError(f"go.mod must declare module {MODULE}")
    if (root / "vendor").exists():
        raise ValueError("vendor/ is forbidden; use Go modules")
    scanner = pathlib.Path(__file__).resolve().parent / "goimports" / "main.go"
    result = subprocess.run(
        ["go", "run", str(scanner), str(root)],
        cwd=root, capture_output=True, text=True,
    )
    if result.returncode:
        raise ValueError(f"Go import scan failed: {result.stderr.strip()}")
    files = json.loads(result.stdout)
    if not files:
        raise ValueError("no Go source files found; refusing an empty audit")
    return files
