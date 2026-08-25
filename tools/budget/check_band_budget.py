#!/usr/bin/env python3
import argparse
import json
import pathlib
import re
import subprocess
import sys

MODULE = "github.com/jinyitao123/weave"
BANDS = ("base", "kernel", "build", "app")


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


def main() -> int:
    parser = argparse.ArgumentParser(description="Check four-band Go size budgets.")
    parser.add_argument("--root", default=".", help="repository root to scan")
    parser.add_argument("--budgets", default="tools/budget/budgets.json", help="budget file relative to root")
    args = parser.parse_args()

    root = pathlib.Path(args.root).resolve()
    budgets = json.loads((root / args.budgets).read_text(encoding="utf-8"))
    modules = module_paths(root)
    failures = []

    for band in BANDS:
        files = sorted((root / "internal" / band).rglob("*.go"))
        dependencies = set()
        for path in files:
            for imported in parse_imports(path.read_text(encoding="utf-8")):
                module = imported_module(imported, modules)
                if module is not None:
                    dependencies.add(module)
        actual = {
            "lines": count_lines(files),
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
        for metric, value in actual.items():
            if value > limit[metric]:
                failures.append(f"{band} {metric}: actual {value} exceeds budget {limit[metric]}")

    if failures:
        for failure in failures:
            print(f"band budget violation: {failure}", file=sys.stderr)
        return 1
    print("band budget ok")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
