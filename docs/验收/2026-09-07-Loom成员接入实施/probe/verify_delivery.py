"""Independently compare durable delivery, operation receipts, and actual files."""
import hashlib
import json
from pathlib import Path
import re
import runpy
import sys

root = Path(sys.argv[1]).resolve()
stage = root / "stage-d"
work = stage / "task" if (stage / "task").is_dir() else stage
helpers = runpy.run_path(str(Path(__file__).with_name("fault_controller.py")), run_name="snapshot_only")
after = helpers["snapshot"]("after-delivery")
query = helpers["query"]
sample = json.loads((stage / "active-sample.json").read_text())
run_id = sample["run_id"]
assert re.fullmatch(r"run-[a-f0-9-]+", run_id)
before = json.loads((stage / "before-fault.json").read_text())
fault = json.loads((stage / "fault-completed.json").read_text())
members = [row for row in after["weave_workflow_member_runs"] if row["parent_run_id"] == run_id]
assert len(members) == 1
member = members[0]
assert member["member_run_id"] == fault["member_run_id"]
assert next(row for row in after["weave_team_runs"] if row["run_id"] == run_id)["status"] == "succeeded"
assert member["result"] and not member["result"].get("error")
result = member["result"]["result"]
assert result["RunID"] == member["member_run_id"] and result["StopReason"] == "completed"
files = result["State"]["__member_artifacts_v1"]
assert files
rows = query("select coalesce(json_agg(t),'[]'::json) from (select id,title,content,metadata from weave_final_deliverables where run_id='" + run_id + "' and metadata->>'artifact_kind'='final') t")
delivered = {row["metadata"]["filename"]: row for row in rows if row["metadata"].get("filename")}
assert len(delivered) == len(files)
file_checks = []
for artifact in files:
    name = artifact["path"]
    assert delivered[name]["content"] == artifact["content"]
    data = (work / "outputs" / name).read_bytes()
    assert data == artifact["content"].encode()
    file_checks.append({"path": name, "sha256": hashlib.sha256(data).hexdigest(), "bytes": len(data), "deliverable_id": delivered[name]["id"]})

def operations(snapshot):
    return {row["key"]: row["value"] for row in snapshot["store"] if row["namespace"].startswith("member-operation:") and row["key"].startswith(member["member_run_id"] + "/")}

original = operations(before)
final = operations(after)
confirmed_preserved = []
for key, operation in original.items():
    if operation.get("response"):
        assert final[key] == operation, "confirmed operation changed after recovery"
        confirmed_preserved.append(key)
physical = [json.loads(path.read_text()) for path in stage.glob("tool-*.json")]
matched = []
for key, operation in final.items():
    if operation["kind"] != "tool" or not operation.get("response"):
        continue
    receipt = json.loads(operation["response"]["content"])
    assert sum(item == receipt for item in physical) == 1, "tool receipt not backed by one actual effect"
    assert operation["attempts"] == 1, "tool effect was retried"
    matched.append(key)
for item in json.loads((stage / "second-sample-input-hashes.json").read_text()):
    assert hashlib.sha256((stage / item["path"]).read_bytes()).hexdigest() == item["sha256"]
    assert hashlib.sha256((work / item["path"]).read_bytes()).hexdigest() == item["sha256"]
old_lease = next(row for row in before["weave_run_attempt_leases"] if row["run_id"] == member["member_run_id"])
new_lease = next(row for row in after["weave_run_attempt_leases"] if row["run_id"] == member["member_run_id"])
assert new_lease["attempt_generation"] == old_lease["attempt_generation"] + 1
report = {"status": "passed", "run_id": run_id, "member_run_id": member["member_run_id"], "attempt_before": old_lease["attempt_generation"], "attempt_after": new_lease["attempt_generation"], "confirmed_operations_preserved": confirmed_preserved, "actual_tool_operations": matched, "files": file_checks, "input_hashes_unchanged": True}
(stage / "independent-delivery-contract.json").write_text(json.dumps(report, ensure_ascii=False, indent=2))
print(json.dumps({"status": "passed", "files": len(files), "confirmed_operations_preserved": len(confirmed_preserved), "actual_tool_operations": len(matched)}))
