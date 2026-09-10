#!/usr/bin/env python3
"""Reconstruct a declared gamma-fault variant from retained real task output.
This is NOT a replay of a complete historical checkpoint: Write event inputs
are truncated, and only the verified gamma Edit is reversed.
"""
import hashlib, json, os, shutil, subprocess, tempfile
from pathlib import Path
ROOT = Path(__file__).resolve().parents[1]
SOURCE = Path('/private/tmp/weave-corona-validation-20260906/workspaces-b/.invocations/task-3b0014b553bdaa5746c6a708d5c75ecd/default/corona-prephase-a-lab-v3-luna-digital-engineering-builder/workdir/outputs/model')
DEST = ROOT / 'probe/fixtures/t2-real-model'
EVIDENCE = ROOT / 'evidence/t2-real-model'
def digest(p): return hashlib.sha256(p.read_bytes()).hexdigest()
def main():
    events = json.loads(subprocess.check_output(['psql', '-d', 'weave_aesthetic_0905', '-Atc', "SELECT result::jsonb->'events' FROM weave_task_queue WHERE id='task-3b0014b553bdaa5746c6a708d5c75ecd'"], text=True))
    edit = json.loads(events[74]['input'])
    assert edit['file_path'].endswith('/model/tests/test_physics.py')
    assert not DEST.exists(), 'Do not overwrite frozen fixture'
    shutil.copytree(SOURCE, DEST, ignore=shutil.ignore_patterns('__pycache__', '*.pyc'))
    target = DEST / 'tests/test_physics.py'
    assert target.read_text().count(edit['new_string']) == 1
    target.write_text(target.read_text().replace(edit['new_string'], edit['old_string'], 1))
    EVIDENCE.mkdir(parents=True, exist_ok=True)
    (EVIDENCE/'source-edit.json').write_text(json.dumps({'event_index':74, 'event':events[74]}, ensure_ascii=False, indent=2)+'\n')
    rows = [{'file':str(p.relative_to(DEST)), 'source_sha256':digest(SOURCE/p.relative_to(DEST)), 'fixture_sha256':digest(p)} for p in sorted(DEST.rglob('*')) if p.is_file()]
    outcomes = []
    for mode in ['faulted', 'repaired']:
        with tempfile.TemporaryDirectory(prefix='loom-t2-audit-') as tmp:
            work = Path(tmp)/'model'; shutil.copytree(DEST, work)
            if mode == 'repaired':
                p=work/'tests/test_physics.py'; assert p.read_text().count(edit['old_string'])==1
                p.write_text(p.read_text().replace(edit['old_string'],edit['new_string'],1))
            result=subprocess.run(['python3','-B','-m','unittest','discover','-s','tests','-v'],cwd=work,capture_output=True,text=True,timeout=90,env={**os.environ,'PYTHONDONTWRITEBYTECODE':'1'})
            (EVIDENCE/f'{mode}.stdout.log').write_text(result.stdout)
            (EVIDENCE/f'{mode}.stderr.log').write_text(result.stderr)
            outcomes.append({'mode':mode,'exit_code':result.returncode,'command':'python3 -B -m unittest discover -s tests -v','run_in':'isolated temporary copy'})
            assert result.returncode == (1 if mode=='faulted' else 0), outcomes
            assert 'Ran 38 tests' in result.stderr
            if mode=='faulted': assert 'FAILED (failures=1)' in result.stderr
    data={'source_task':'task-3b0014b553bdaa5746c6a708d5c75ecd','source_path':str(SOURCE),'construction':'retained final model with only verified gamma repair reversed; not exact historical snapshot','files':rows,'outcomes':outcomes,'admission':'fixture provenance/acceptance check only; no A/B/C model run and no claim of representative 25-minute task cost'}
    (EVIDENCE/'manifest.json').write_text(json.dumps(data,ensure_ascii=False,indent=2)+'\n')
    print(json.dumps({'files':len(rows),'outcomes':outcomes},ensure_ascii=False))
if __name__=='__main__':main()
