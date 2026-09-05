#!/usr/bin/env python3
import argparse
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
from go_inventory import MODULE, go_inventory

INTERNAL = MODULE + "/internal/"
ORDER = {"base": 0, "kernel": 1, "build": 2, "app": 3}

# Migration debt is pinned to exact source/import pairs. Removing one edge
# does not create permission to add an upward import somewhere else.
BASELINE = {
    "executor_fanout.go": {"workflow", "workflow/machine"},
    "executor_terminal.go": {"loomruntime"},
    "executor_transition.go": {"loomruntime"},
    "fanout_seams.go": {"loomruntime"},
    "workflow_interpreter.go": {"loomruntime", "workflow", "workflow/machine"},
    "workflow_runtime.go": {"loomruntime", "workflow", "workflow/machine"},
}
ALLOWED_EDGES = {
    ("internal/base/teamrun/" + file, INTERNAL + "kernel/" + package)
    for file, packages in BASELINE.items() for package in packages
}


def band_for_import(path: str) -> str | None:
    if not path.startswith(INTERNAL):
        return None
    parts = path[len(INTERNAL):].split("/")
    return parts[0] if len(parts) >= 2 and parts[0] in ORDER and parts[1] else None


def check(files: list[dict]) -> tuple[list[str], set]:
    violations, baselines = [], set()
    for file in files:
        path = file["path"]
        importer = "app" if path.startswith("cmd/") else band_for_import(
            MODULE + "/" + str(pathlib.PurePosixPath(path).parent)
        )
        if importer is None:
            violations.append(f"{path}: source is outside the four bands")
            continue
        for imported in file["imports"]:
            if not imported.startswith(INTERNAL):
                continue
            target = band_for_import(imported)
            if target is None:
                violations.append(f"{path}: unknown internal package band: {imported}")
            elif ORDER[target] > ORDER[importer]:
                edge = (path, imported)
                if edge in ALLOWED_EDGES:
                    baselines.add(edge)
                else:
                    violations.append(f"{path}: {importer} cannot import {target}: {imported}")
    return violations, baselines


def main() -> int:
    parser = argparse.ArgumentParser(description="Check Weave four-band import boundaries.")
    parser.add_argument("--root", default=".", help="repository root to scan")
    args = parser.parse_args()
    try:
        files = go_inventory(pathlib.Path(args.root).resolve())
    except (ValueError, OSError) as err:
        print(f"depguard: {err}", file=sys.stderr)
        return 1
    violations, baselines = check(files)
    print(f"depguard baseline: exact upward edges {len(baselines)}/{len(ALLOWED_EDGES)}")
    for violation in violations:
        print(violation, file=sys.stderr)
    if violations:
        return 1
    print(f"depguard ok: scanned {len(files)} Go files, violations 0")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
