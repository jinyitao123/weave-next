#!/usr/bin/env python3
import argparse
import csv
import datetime as dt
import json
from collections import Counter
from pathlib import Path


def read_jsonl(path):
    if not path.exists():
        return []
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def parse_time(value):
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("batch", type=Path)
    parser.add_argument("--csv", required=True, type=Path)
    parser.add_argument("--json", required=True, type=Path)
    args = parser.parse_args()
    rows = []
    order = json.loads((args.batch / "order.json").read_text())["cases"]
    for index, spec in enumerate(order, 1):
        case_dir = args.batch / f"{index:02d}-{spec['task']}-{spec['candidate']}-{spec['scenario']}"
        events = read_jsonl(case_dir / "events.jsonl")
        processes = read_jsonl(case_dir / "processes.jsonl")
        result_path = case_dir / "result.json"
        result = json.loads(result_path.read_text()) if result_path.exists() else {}
        llm_events = [e for e in events if e.get("kind") == "llm_completed"]
        tools = [e for e in events if e.get("kind") == "tool_dispatch"]
        actual = [e for e in tools if e.get("actual_execution")]
        semantic_counts = Counter(e.get("semantic_key") for e in actual)
        repeated = sum(max(0, count - 1) for count in semantic_counts.values())
        starts = [parse_time(e["at"]) for e in events if e.get("kind") == "process_start"]
        completes = [parse_time(e["at"]) for e in events if e.get("kind") == "process_complete"]
        elapsed = (max(completes) - min(starts)).total_seconds() if starts and completes else None
        resume_records = [p for p in processes if p.get("phase") == "resume"]
        recovery = resume_records[-1]["elapsed_seconds"] if resume_records else None
        status = ("completed" if result.get("done") and processes and processes[-1].get("exit_code") == 0 else "failed") if result else ("interrupted_unknown" if events else "not_started")
        row = {
            "case": case_dir.name,
            "task": spec["task"],
            "candidate": spec["candidate"],
            "scenario": spec["scenario"],
            "status": status,
            "completed": bool(result.get("done")) if result else None,
            "probe_self_check": result.get("independent_correct"),
            "adoption_comparable": False,
            "process_exit_records": len(processes),
            "observed_process_starts": len(starts),
            "elapsed_seconds": round(elapsed, 3) if elapsed is not None else "",
            "recovery_process_seconds": round(recovery, 3) if recovery is not None else None,
            "llm_calls": len(llm_events),
            "input_tokens": sum(e.get("input_tokens", 0) for e in llm_events),
            "output_tokens": sum(e.get("output_tokens", 0) for e in llm_events),
            "tool_dispatches": len(tools),
            "actual_tool_executions": len(actual),
            "deduplicated_tool_receipts": sum(1 for e in tools if e.get("deduplicated")),
            "repeated_semantic_tool_executions": repeated,
            "effect_applications": sum(1 for e in actual if e.get("effect_applied")),
            "loom_history_count": result.get("loom_history_count", 0),
            "crash_window": next((e.get("window") for e in events if e.get("kind") == "injected_crash"), ""),
        }
        rows.append(row)
    args.csv.parent.mkdir(parents=True, exist_ok=True)
    with args.csv.open("w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=list(rows[0]) if rows else [])
        if rows:
            writer.writeheader()
            writer.writerows(rows)
    args.json.write_text(json.dumps({"generated_at": dt.datetime.now(dt.timezone.utc).isoformat(), "rows": rows}, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps({"cases": len(rows), "statuses": dict(Counter(r["status"] for r in rows)), "csv": str(args.csv)}, ensure_ascii=False))


if __name__ == "__main__":
    main()
