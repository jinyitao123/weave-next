#!/usr/bin/env python3
import argparse
import pathlib
import re
import sys

MODULE = "github.com/jinyitao123/weave"
ALLOWED_EXTERNAL_MODULES = {
    "github.com/google/uuid",
    "github.com/jackc/pgx/v5",
    "github.com/jinyitao123/loom",
    "github.com/lattice-substrate/json-canon",
}


def parse_imports(source: str) -> list[str]:
    imports = []
    for match in re.finditer(
        r'^\s*import\s+(?:[._A-Za-z][._A-Za-z0-9]*\s+)?"([^"]+)"',
        source,
        re.MULTILINE,
    ):
        imports.append(match.group(1))
    for block in re.finditer(r"^\s*import\s*\((.*?)^\s*\)", source, re.MULTILINE | re.DOTALL):
        imports.extend(re.findall(r'"([^"]+)"', block.group(1)))
    return imports


def external_module(import_path: str) -> str | None:
    if import_path == MODULE or import_path.startswith(MODULE + "/"):
        return None
    if "." not in import_path.split("/", 1)[0]:
        return None
    matches = [
        module
        for module in ALLOWED_EXTERNAL_MODULES
        if import_path == module or import_path.startswith(module + "/")
    ]
    if matches:
        return max(matches, key=len)
    return import_path


def main() -> int:
    parser = argparse.ArgumentParser(description="Check the internal/base external dependency whitelist.")
    parser.add_argument("--root", default=".", help="repository root to scan")
    args = parser.parse_args()

    root = pathlib.Path(args.root).resolve()
    base = root / "internal" / "base"
    violations = []
    observed = set()
    scanned = 0
    for path in sorted(base.rglob("*.go")):
        scanned += 1
        for imported in parse_imports(path.read_text(encoding="utf-8")):
            module = external_module(imported)
            if module is None:
                continue
            if module not in ALLOWED_EXTERNAL_MODULES:
                violations.append((path.relative_to(root), imported))
                continue
            observed.add(module)

    if violations:
        for path, imported in violations:
            print(f"{path}: external import is not on the base whitelist: {imported}", file=sys.stderr)
        print(f"base dependency violations: {len(violations)}", file=sys.stderr)
        return 1
    modules = ", ".join(sorted(observed))
    print(f"base dependency whitelist ok: scanned {scanned} Go files, modules {len(observed)} [{modules}]")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
