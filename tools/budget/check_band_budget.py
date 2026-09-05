#!/usr/bin/env python3
import argparse
import json
import pathlib
import subprocess
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
from go_inventory import go_inventory

MODULE = "github.com/jinyitao123/weave"
BANDS = ("base", "kernel", "build", "app")


def module_paths(root: pathlib.Path) -> list[str]:
    result = subprocess.run(
        ["go", "list", "-m", "-f", "{{.Path}}", "all"],
        cwd=root,
        check=True,
        capture_output=True,
        text=True,
    )
    return sorted((line for line in result.stdout.splitlines() if line), key=len, reverse=True)


def imported_module(import_path: str, modules: list[str]) -> str | None:
    if import_path == MODULE or import_path.startswith(MODULE + "/"):
        return None
    if "." not in import_path.split("/", 1)[0]:
        return None
    for module in modules:
        if import_path == module or import_path.startswith(module + "/"):
            return module
    return import_path


def count_lines(files: list[pathlib.Path]) -> int:
    total = 0
    for path in files:
        data = path.read_bytes()
        total += data.count(b"\n")
        if data and not data.endswith(b"\n"):
            total += 1
    return total


def package_count(root: pathlib.Path, band: str) -> int:
    result = subprocess.run(
        ["go", "list", f"./internal/{band}/..."],
        cwd=root,
        check=True,
        capture_output=True,
        text=True,
    )
    return len([line for line in result.stdout.splitlines() if line])


def budget_violations(band: str, actual: dict, dependencies: set[str], limit: dict) -> list[str]:
    failures = [
        f"{band} {metric}: actual {value} exceeds budget {limit[metric]}"
        for metric, value in actual.items() if value > limit[metric]
    ]
    for dependency in sorted(dependencies - set(limit["allowed_dependencies"])):
        failures.append(f"{band} dependency is not approved: {dependency}")
    return failures


def main() -> int:
    parser = argparse.ArgumentParser(description="Check four-band Go size budgets.")
    parser.add_argument("--root", default=".", help="repository root to scan")
    parser.add_argument("--budgets", default="tools/budget/budgets.json", help="budget file relative to root")
    args = parser.parse_args()

    root = pathlib.Path(args.root).resolve()
    budgets = json.loads((root / args.budgets).read_text(encoding="utf-8"))
    inventory = go_inventory(root)
    modules = module_paths(root)
    failures = []

    for band in BANDS:
        files = sorted((root / "internal" / band).rglob("*.go"))
        dependencies = set()
        for file in inventory:
            if not file["path"].startswith(f"internal/{band}/"):
                continue
            for imported in file["imports"]:
                module = imported_module(imported, modules)
                if module is not None:
                    dependencies.add(module)
        actual = {
            "lines": count_lines(files),
            "production_lines": count_lines([path for path in files if not path.name.endswith("_test.go")]),
            "packages": package_count(root, band),
            "external_dependencies": len(dependencies),
        }
        limit = budgets[band]
        print(
            f"band budget {band}: "
            f"lines {actual['lines']}/{limit['lines']}, "
            f"packages {actual['packages']}/{limit['packages']}, "
            f"external_dependencies {actual['external_dependencies']}/{limit['external_dependencies']}"
        )
        print(f"  production lines {actual['production_lines']}/{limit['production_lines']}")
        failures.extend(budget_violations(band, actual, dependencies, limit))
        target = budgets["migration_targets"][band]
        debt = [f"{metric} +{actual[metric] - ceiling}" for metric, ceiling in target.items()
                if actual[metric] > ceiling]
        if debt:
            print(f"  unresolved migration debt against original target: {', '.join(debt)}")

    if failures:
        for failure in failures:
            print(f"band budget violation: {failure}", file=sys.stderr)
        return 1
    print("band budget ok")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
