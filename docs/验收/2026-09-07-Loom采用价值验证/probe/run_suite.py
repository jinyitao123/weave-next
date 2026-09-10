#!/usr/bin/env python3
import argparse
import datetime as dt
import json
import os
from pathlib import Path
import random
import shutil
import subprocess
import sys
import time


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_suffix(path.suffix + ".tmp")
    temp.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n")
    temp.replace(path)


def append_jsonl(path, value):
    with path.open("a") as handle:
        handle.write(json.dumps(value, ensure_ascii=False) + "\n")


def prepare_workspace(case_dir, fixture, archive_existing=False):
    workspace = case_dir / "workspace"
    if workspace.exists():
        if archive_existing:
            archived = case_dir / "workspace-before-restart"
            if archived.exists():
                raise RuntimeError(f"archive already exists: {archived}")
            workspace.rename(archived)
        else:
            shutil.rmtree(workspace)
    shutil.copytree(fixture, workspace)


def run_process(binary, case_dir, candidate, task, scenario, phase, sequence, env):
    started = dt.datetime.now(dt.timezone.utc)
    command = [str(binary), str(case_dir), candidate, task, scenario, phase]
    try:
        completed = subprocess.run(command, capture_output=True, text=True, env=env, timeout=720)
        exit_code = completed.returncode
        stdout = completed.stdout
        stderr = completed.stderr
    except subprocess.TimeoutExpired as exc:
        exit_code = 124
        stdout = exc.stdout or ""
        stderr = (exc.stderr or "") + "\nprocess timeout after 720 seconds"
    ended = dt.datetime.now(dt.timezone.utc)
    (case_dir / f"process-{sequence:02d}-{phase}.stdout.log").write_text(stdout)
    (case_dir / f"process-{sequence:02d}-{phase}.stderr.log").write_text(stderr)
    record = {
        "sequence": sequence,
        "phase": phase,
        "command": command,
        "pid_observed_in_events": True,
        "started_at": started.isoformat(),
        "ended_at": ended.isoformat(),
        "elapsed_seconds": (ended - started).total_seconds(),
        "exit_code": exit_code,
    }
    append_jsonl(case_dir / "processes.jsonl", record)
    return exit_code


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--fixtures", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--seed", type=int, default=20260907)
    args = parser.parse_args()

    cases = [(task, candidate, scenario) for task in ("t1", "t2") for candidate in ("A", "B", "C") for scenario in ("s0", "s1", "s2")]
    random.Random(args.seed).shuffle(cases)
    args.output.mkdir(parents=True, exist_ok=False)
    write_json(args.output / "order.json", {"seed": args.seed, "cases": [{"task": t, "candidate": c, "scenario": s} for t, c, s in cases]})

    env = os.environ.copy()
    env.setdefault("LOOM_VALUE_CLI_VERSION", "2.1.245")
    env.setdefault("LOOM_VALUE_DATABASE_URL", "postgres://jinyitao@127.0.0.1:5432/weave_next?sslmode=disable")
    outcomes = []
    for index, (task, candidate, scenario) in enumerate(cases, 1):
        case_dir = args.output / f"{index:02d}-{task}-{candidate}-{scenario}"
        case_dir.mkdir()
        prepare_workspace(case_dir, args.fixtures / task)
        print(f"[{index:02d}/18] {task} {candidate} {scenario}", flush=True)
        exit_code = run_process(args.binary, case_dir, candidate, task, scenario, "start", 1, env)
        process_sequence = 1
        if exit_code == 86:
            if candidate == "A":
                prepare_workspace(case_dir, args.fixtures / task, archive_existing=True)
            process_sequence += 1
            exit_code = run_process(args.binary, case_dir, candidate, task, scenario, "resume", process_sequence, env)
        status = "completed" if exit_code == 0 else "failed"
        outcomes.append({"index": index, "task": task, "candidate": candidate, "scenario": scenario, "status": status, "exit_code": exit_code})
        write_json(args.output / "suite-status.json", {"updated_at": dt.datetime.now(dt.timezone.utc).isoformat(), "completed_cases": index, "total_cases": len(cases), "outcomes": outcomes})
        print(f"          {status} exit={exit_code}", flush=True)
        time.sleep(0.2)
    return 0 if all(item["status"] == "completed" for item in outcomes) else 1


if __name__ == "__main__":
    sys.exit(main())
