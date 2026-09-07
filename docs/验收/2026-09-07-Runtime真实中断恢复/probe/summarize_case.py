#!/usr/bin/env python3
"""Summarize observed receipts, preserving unknown usage and partial delivery."""
import argparse
import collections
import datetime as dt
import hashlib
import json
import subprocess
from pathlib import Path
from usage_details import complete_fields

REPORT = Path(__file__).resolve().parents[1]
ENVIRONMENT = json.loads((REPORT / 'evidence/environment.json').read_text())
ROOT = Path(ENVIRONMENT['root'])


def summarize(case):
    directory = ROOT / 'cases' / case
    read = lambda name: json.loads((directory / name).read_text())
    status, receipt = read('status.json'), read('task-receipt.json')
    events, files = read('public-events.json'), read('final-files.json')
    result = receipt.get('result') or {}
    usage = result.get('usage_receipt') or {}
    raw_usage, summary_complete = complete_fields(usage.get('raw_summary') or '{}')
    tasks = json.loads(subprocess.check_output([
        'psql', '-d', ENVIRONMENT['database_name'], '-Atc',
        "SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT id,status,worker_id,runtime_id,created_at,started_at,completed_at FROM weave_task_queue WHERE run_snapshot_id='runtime-value-"
        + case + "' AND payload->>'node_id'='engineering' ORDER BY created_at)t"
    ], text=True))
    artifact_hashes = {
        'outputs/' + a['path']: hashlib.sha256(a['content'].encode()).hexdigest()
        for a in result.get('artifacts', [])
    }
    file_hashes = {f['path']: f['sha256'] for f in files}
    sequences = [int(e['detail']['task_seq']) for e in events]
    kinds = collections.Counter(e['detail']['event']['kind'] for e in events)
    summary = {
        'case': case, 'summarized_at': dt.datetime.now(dt.timezone.utc).isoformat(),
        'status': status, 'physical_tasks': tasks, 'physical_task_count': len(tasks),
        'logical_invocation_id': receipt['payload'].get('logical_invocation_id'),
        'queue_status': receipt['status'], 'queue_error': receipt.get('error'),
        'engine_status': result.get('status'), 'reported_models': result.get('reported_models'),
        'public_events': {'count': len(events), 'kinds': dict(kinds),
                          'sequence_unique': len(sequences) == len(set(sequences)),
                          'sequence_contiguous': sequences == list(range(1, len(sequences)+1))},
        'files': {'count': len(files), 'receipt_artifact_count': len(artifact_hashes),
                  'receipt_hash_mismatches': [p for p, h in artifact_hashes.items() if file_hashes.get(p) != h],
                  'files_without_receipt': [p for p in file_hashes if p not in artifact_hashes]},
        'usage': {
            'source': usage.get('source'), 'scope': usage.get('scope'),
            'input_tokens_including_cache': usage.get('input_tokens') if usage.get('has_tokens') else None,
            'output_tokens': usage.get('output_tokens') if usage.get('has_tokens') else None,
            'cli_reported_cost_usd': usage.get('cost_usd') if usage.get('has_cost') else None,
            'cost_is_provider_invoice': False,
            'raw_usage': raw_usage.get('usage'), 'session_id': raw_usage.get('session_id'),
            'turns': raw_usage.get('num_turns'),
            'raw_summary_complete_json': summary_complete,
            'missing_is_unknown_not_zero': True,
        },
        'scope': 'real member execution, not full workflow or Workbench recovery',
    }
    if (directory / 'fault-before.json').exists():
        before, after = read('fault-before.json'), read('fault-after.json')
        before_hashes = {f['path']: f['sha256'] for f in before['files']}
        fault_seq = before['milestone']['tool_result']['detail']['task_seq']
        summary['fault'] = {
            'before': before['at'], 'after': after.get('restarted_at', after['at']),
            'milestone_task_seq': fault_seq,
            'cli_pid_before': before['cli']['pid'],
            'cli_still_same_after_restart': after.get('cli_still_same'),
            'file_count_before': len(before_hashes),
            'missing_before_files_at_end': [p for p in before_hashes if p not in file_hashes],
            'unchanged_before_files_at_end': [p for p, h in before_hashes.items() if file_hashes.get(p) == h],
            'changed_before_files_at_end': [p for p, h in before_hashes.items() if p in file_hashes and file_hashes[p] != h],
            'public_events_after_milestone': sum(int(e['detail']['task_seq']) > int(fault_seq) for e in events),
            'changed_files_are_not_automatically_rework': True,
        }
    acceptance = directory / 'independent/acceptance.json'
    summary['independent_acceptance'] = json.loads(acceptance.read_text()) if acceptance.exists() else None
    (directory / 'summary.json').write_text(json.dumps(summary, ensure_ascii=False, indent=2)+'\n')
    return summary


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('case', choices=['N', 'C', 'M', 'M2'])
    args = parser.parse_args()
    print(json.dumps(summarize(args.case), ensure_ascii=False, indent=2))
