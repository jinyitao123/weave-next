#!/usr/bin/env python3
import argparse
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
from go_inventory import go_inventory

MODULE = "github.com/jinyitao123/weave"
ALLOWED_EXTERNAL_MODULES = {
    "github.com/google/uuid",
    "github.com/jackc/pgx/v5",
    "github.com/jinyitao123/loom",
    "github.com/lattice-substrate/json-canon",
}


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
    violations = []
    observed = set()
    scanned = 0
    for file in go_inventory(root):
        if not file["path"].startswith("internal/base/"):
            continue
        path = pathlib.Path(file["path"])
        scanned += 1
        for imported in file["imports"]:
            module = external_module(imported)
            if module is None:
                continue
            if module not in ALLOWED_EXTERNAL_MODULES:
                violations.append((path, imported))
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
