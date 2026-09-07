#!/usr/bin/env python3
"""Copy completed experiment evidence and verify bytes; exclude credentials."""
import argparse
import datetime as dt
import hashlib
import json
import shutil
from pathlib import Path

REPORT = Path(__file__).resolve().parents[1]
ROOT = Path(json.loads((REPORT / 'evidence/environment.json').read_text())['root'])


def archive(case):
    source = ROOT / 'cases' / case
    status = json.loads((source / 'status.json').read_text())
    outputs = Path(status['workdir']) / 'outputs'
    destination = REPORT / 'cases' / case
    private = json.loads((ROOT / 'private-env.json').read_text())
    secrets = [(ROOT / 'runtime-token').read_bytes()]
    secrets += [str(value).encode() for key, value in private.items()
                if ('SECRET' in key or 'TOKEN' in key or key.endswith('_KEY')) and value]
    rows = []
    selections = [(p, Path('evidence') / p.relative_to(source)) for p in source.rglob('*')]
    selections += [(p, Path('outputs') / p.relative_to(outputs)) for p in outputs.rglob('*')]
    for path, relative in sorted(selections, key=lambda pair: str(pair[1])):
        if not path.is_file() or '__pycache__' in path.parts or path.suffix == '.pyc':
            continue
        if path.is_symlink():
            raise ValueError('Evidence contains unexpected symlink: ' + str(relative))
        data = path.read_bytes()
        if any(secret and secret in data for secret in secrets):
            raise ValueError('Private experiment credential detected; archive stopped: ' + str(relative))
        target = destination / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        if target.exists() and target.read_bytes() != data:
            raise ValueError('Existing archived evidence differs: ' + str(relative))
        if not target.exists():
            shutil.copy2(path, target)
        digest = hashlib.sha256(data).hexdigest()
        assert hashlib.sha256(target.read_bytes()).hexdigest() == digest
        rows.append({'path': str(relative), 'sha256': digest, 'bytes': len(data)})
    manifest = {'case': case, 'archived_at': dt.datetime.now(dt.timezone.utc).isoformat(),
                'source_workdir': status['workdir'], 'files': rows,
                'credentials_excluded': True, 'source_files_not_modified': True}
    (destination / 'archive-manifest.json').write_text(json.dumps(manifest, ensure_ascii=False, indent=2)+'\n')
    return {'case': case, 'files': len(rows), 'bytes': sum(r['bytes'] for r in rows)}


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('case', choices=['N', 'C', 'M', 'M2'])
    print(json.dumps(archive(parser.parse_args().case)))
