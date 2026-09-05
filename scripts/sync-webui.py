#!/usr/bin/env python3
"""Copy the runtime maintenance build into the Go embedded UI."""
from pathlib import Path
import shutil

root = Path(__file__).resolve().parents[1]
source = root / "weave-app" / "dist"
target = root / "internal" / "app" / "webui" / "dist"
if not (source / "index.html").is_file():
    raise SystemExit("Build the runtime maintenance UI before embedding it.")
shutil.rmtree(target)
shutil.copytree(source, target)
print("Embedded runtime maintenance UI refreshed.")
