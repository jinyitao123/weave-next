"""Copy actual acceptance evidence, redact credentials, and index archived bytes."""
import hashlib
import json
from pathlib import Path
import re
import sys

root = Path(sys.argv[1]).resolve()
report = Path(__file__).resolve().parents[1]
source = root / "stage-d"
target = report / "cases/stage-d"
target.mkdir(parents=True, exist_ok=True)
settings = json.loads((root / "settings.json").read_text())
secrets = [settings[k] for k in ("secret_key", "jwt_secret") if settings.get(k)]
secrets.append((root / "api-key").read_text().strip())
for path in source.glob("workbench*.log"):
    secrets.extend(re.findall(r"[?&]token=([^\s]+)", path.read_text()))

for path in sorted(source.rglob("*")):
    rel = path.relative_to(source)
    if not path.is_file() or path.is_symlink() or any(part in {"inference", "__pycache__", "node_modules"} for part in rel.parts):
        continue
    if path.suffix == ".pyc":
        continue
    data = path.read_bytes()
    for secret in secrets:
        if secret:
            data = data.replace(secret.encode(), b"[REDACTED_SECRET]")
    destination = target / rel
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_bytes(data)

index = []
for path in sorted((report / "cases").rglob("*")):
    if path.is_file():
        data = path.read_bytes()
        index.append({"path": str(path.relative_to(report)), "sha256": hashlib.sha256(data).hexdigest(), "bytes": len(data)})
(report / "archive-index.json").write_text(json.dumps(index, ensure_ascii=False, indent=2) + "\n")
print(json.dumps({"archived_files": len(index), "bytes": sum(item["bytes"] for item in index)}))
