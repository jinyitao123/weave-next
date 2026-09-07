"""Opt-in controller: interrupt only this acceptance API after a durable tool receipt."""
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

root = Path(sys.argv[1]).resolve()
cfg = json.loads((root / "settings.json").read_text())
stage = root / "stage-d"


def query(sql):
    result = subprocess.run(
        ["psql", cfg["database_url"], "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", sql],
        check=True, text=True, capture_output=True,
    )
    return json.loads(result.stdout)


def snapshot(label):
    result = {"at": time.time()}
    for table in ["weave_team_runs", "weave_workflow_member_runs", "weave_run_attempt_leases"]:
        result[table] = query(f"select coalesce(json_agg(t),'[]'::json) from {table} t")
    result["store"] = query("select coalesce(json_agg(t),'[]'::json) from (select namespace,key,convert_from(value,'UTF8') as value from loom_store order by namespace,key) t")
    for row in result["store"]:
        try:
            row["value"] = json.loads(row["value"])
        except json.JSONDecodeError:
            pass  # Retry-entry fencing markers are opaque strings.
    work = stage / "task" if (stage / "task").is_dir() else stage
    result["files"] = [{"path": str(p.relative_to(work)), "sha256": hashlib.sha256(p.read_bytes()).hexdigest(), "bytes": p.stat().st_size} for p in sorted((work / "outputs").rglob("*")) if p.is_file() and "__pycache__" not in p.parts]
    result["tool_receipts"] = [p.name for p in sorted(stage.glob("tool-*.json"))]
    (stage / (label + ".json")).write_text(json.dumps(result, ensure_ascii=False, indent=2))
    return result


if __name__ == "__main__":
    deadline = time.monotonic() + 3600
    while not (stage / "awaiting-fault.json").exists():
        if time.monotonic() > deadline:
            raise TimeoutError("model never reached the counted test boundary")
        time.sleep(1)
    assert not (stage / "fault-completed.json").exists()
    before = snapshot("before-fault")
    sample = json.loads((stage / "active-sample.json").read_text())
    members = [member for member in before["weave_workflow_member_runs"] if member["parent_run_id"] == sample["run_id"]]
    assert len(members) == 1 and members[0]["result"] is None
    member = members[0]
    paths = [f["path"] for f in before["files"]]
    all_deliverable_categories_present = (
        sum(p.endswith(".svg") for p in paths) >= 3
        and any(p.endswith(".scad") for p in paths)
        and any(p.startswith("outputs/app/") and p.endswith(".html") for p in paths)
        and any(p.startswith("outputs/verification/") for p in paths)
    )
    assert not all_deliverable_categories_present, "full artifact set already exists; this is not the required mid-task fault"
    latest = next(v["value"] for v in before["store"] if v["namespace"].startswith("checkpoint:") and v["key"] == member["member_run_id"])
    history = next(v["value"] for v in before["store"] if v["namespace"].startswith("checkpoint:") and v["key"] == member["member_run_id"] + "/" + str(latest["seq"]).zfill(12))
    assert latest == history and latest["seq"] == member["checkpoint_seq"]
    milestone = json.loads((stage / "model-tests-milestone.json").read_text())
    confirmed = [v["value"] for v in before["store"] if v["namespace"].startswith("member-operation:") and v["key"].startswith(member["member_run_id"] + "/") and v["value"].get("kind") == "tool" and v["value"].get("response")]
    assert any(json.loads(op["response"]["content"])["stdout_stderr"] == milestone["stdout_stderr"] for op in confirmed), "successful tests not yet in durable receipt"
    process = json.loads((stage / "api-process.json").read_text())
    pid = process["pid"]
    args = subprocess.run(["ps", "-p", str(pid), "-o", "args="], text=True, capture_output=True, check=True).stdout
    parts = args.strip().split()
    assert len(parts) == 3 and parts[1] == "serve"
    assert Path(parts[0]).resolve() == root / "probe"
    assert Path(parts[2]).resolve() == root / "settings.json"
    os.kill(pid, signal.SIGKILL)
    fault = {"killed_pid": pid, "at": time.time(), "member_run_id": member["member_run_id"], "checkpoint_seq": latest["seq"], "confirmed_tools": len(confirmed)}
    (stage / "fault-completed.json").write_text(json.dumps(fault, indent=2))
    env = dict(os.environ, DATABASE_URL=cfg["database_url"], JWT_SECRET=cfg["jwt_secret"])
    log = open(stage / "serve-after-fault.log", "w")
    proc = subprocess.Popen([str(root / "probe"), "serve", str(root / "settings.json")], env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
    records = json.loads((stage / "processes.json").read_text())
    records.append({"mode": "serve-after-intended-fault", "pid": proc.pid})
    (stage / "processes.json").write_text(json.dumps(records, indent=2))
    print(json.dumps({**fault, "restarted_pid": proc.pid}), flush=True)
