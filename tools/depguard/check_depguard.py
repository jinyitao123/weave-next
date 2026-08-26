#!/usr/bin/env python3
import argparse
import pathlib
import re
import sys

MODULE = "github.com/jinyitao123/weave"
INTERNAL = MODULE + "/internal/"

BANDS = {
    "base": {
        "streamctx",
        "frozen",
        "execution",
        "taskqueue",
        "fanout",
        "deliverable",
        "snapshot",
        "realtime",
        "storeext",
        "db",
        "testutil",
        "teamrun",
    },
    "kernel": {
        "engine",
        "grounding",
        "llmrouter",
        "otel",
        "secret",
        "embedder",
        "schedule",
        "audit",
        "config",
        "registry",
        "org",
        "credentials",
        "delivery",
        "skills",
        "memory",
        "compiler",
        "declarative",
        "teamcompiler",
        "revocation",
        "execenv",
        "runtimes",
        "runtimellm",
        "sessionexec",
        "loomruntime",
        "mcpregistry",
        "mcphost",
        "mcpprobe",
        "workflow",
        "freezer",
    },
    "build": {
        "teambuild",
        "teameval",
        "teamforge",
        "teamorch",
        "teamrestore",
    },
    "app": {
        "api",
        "daemon",
        "metateam",
        "webui",
        "chatrequest",
        "conversation",
        "projects",
        "users",
        "apikeys",
        "attachments",
        "ownermem",
        "schedules",
    },
}

TOP_TO_BAND = {pkg: band for band, pkgs in BANDS.items() for pkg in pkgs}
ORDER = {"base": 0, "kernel": 1, "build": 2, "app": 3}
BASELINE_LIMITS = {
    "base/teamrun -> kernel workflow/loomruntime baseline": 11,
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


def go_files(root: pathlib.Path):
    skip_parts = {"vendor", "node_modules", ".git", "dist"}
    for path in root.rglob("*.go"):
        if any(part in skip_parts for part in path.relative_to(root).parts):
            continue
        yield path


def import_path_for_file(root: pathlib.Path, path: pathlib.Path) -> str | None:
    rel = path.relative_to(root)
    parts = rel.parts
    if len(parts) >= 2 and parts[0] == "cmd" and parts[1] == "weave":
        return MODULE + "/cmd/weave"
    if len(parts) >= 2 and parts[0] == "internal":
        return INTERNAL + "/".join(parts[1:-1])
    return None


def package_identity(path: str) -> tuple[str | None, str | None]:
    if path == MODULE + "/cmd/weave":
        return ("app", "cmd/weave")
    if not path.startswith(INTERNAL):
        return (None, None)
    rest = path[len(INTERNAL) :]
    parts = rest.split("/")
    if not parts or parts[0] == "":
        return (None, None)
    if parts[0] in ORDER:
        band = parts[0]
        pkg = "/".join(parts[1:])
        top = parts[1] if len(parts) > 1 else ""
        if top and TOP_TO_BAND.get(top) and TOP_TO_BAND[top] != band:
            return (band, pkg)
        return (band, pkg)
    top = parts[0]
    band = TOP_TO_BAND.get(top)
    return (band, rest)


def is_allowed_exception(importer_pkg: str, imported_pkg: str) -> str | None:
    if importer_pkg == "teamrun" and imported_pkg in {
        "workflow",
        "workflow/machine",
        "loomruntime",
    }:
        return "base/teamrun -> kernel workflow/loomruntime baseline"
    return None


def main() -> int:
    parser = argparse.ArgumentParser(description="Check Weave four-band import boundaries.")
    parser.add_argument("--root", default=".", help="repository root to scan")
    args = parser.parse_args()

    root = pathlib.Path(args.root).resolve()
    violations = []
    baselines = {}
    scanned = 0

    for path in go_files(root):
        importer_path = import_path_for_file(root, path)
        if importer_path is None:
            continue
        importer_band, importer_pkg = package_identity(importer_path)
        if importer_band is None:
            continue
        scanned += 1
        imports = parse_imports(path.read_text(encoding="utf-8"))
        for imp in imports:
            if not imp.startswith(INTERNAL):
                continue
            imported_band, imported_pkg = package_identity(imp)
            if imported_band is None:
                violations.append((path, imp, "unknown internal package band"))
                continue
            baseline = is_allowed_exception(importer_pkg or "", imported_pkg or "")
            if baseline:
                baselines[baseline] = baselines.get(baseline, 0) + 1
                continue
            if ORDER[imported_band] > ORDER[importer_band]:
                violations.append(
                    (
                        path,
                        imp,
                        f"{importer_band} package cannot import {imported_band}",
                    )
                )

    for reason, limit in sorted(BASELINE_LIMITS.items()):
        count = baselines.get(reason, 0)
        print(f"depguard baseline: {reason}: {count}/{limit}")
        if count > limit:
            violations.append(
                (pathlib.Path("tools/depguard/check_depguard.py"), reason, "baseline exceeds ratchet limit")
            )
    if violations:
        for path, imp, reason in violations:
            print(f"{path}: imports {imp}: {reason}", file=sys.stderr)
        print(f"depguard violations: {len(violations)}", file=sys.stderr)
        return 1
    print(f"depguard ok: scanned {scanned} Go files, violations 0")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
