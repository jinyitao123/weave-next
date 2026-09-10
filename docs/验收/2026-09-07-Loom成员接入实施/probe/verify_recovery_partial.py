"""Check recovery evidence without treating a failed task as full delivery."""
import json
from pathlib import Path
import sys

stage = Path(sys.argv[1]) / "stage-d"
attempt = stage / "third-attempt" if (stage / "third-attempt").is_dir() else stage
before = json.loads((attempt / "before-fault.json").read_text())
after = json.loads((stage / "third-sample-final.json").read_text())
fault = json.loads((attempt / "fault-completed.json").read_text())
member_id = fault["member_run_id"]
member = next(row for row in after["weave_workflow_member_runs"] if row["member_run_id"] == member_id)
run = next(row for row in after["weave_team_runs"] if row["run_id"] == member["parent_run_id"])

def operations(snapshot):
    return {row["key"]: row["value"] for row in snapshot["store"]
            if row["namespace"].startswith("member-operation:") and row["key"].startswith(member_id + "/")}

original, final = operations(before), operations(after)
confirmed = [key for key, value in original.items() if value.get("response")]
assert all(original[key] == final[key] for key in confirmed)
physical = [json.loads(path.read_text()) for path in stage.glob("tool-*.json")]
tool_keys = []
for key, operation in final.items():
    if operation["kind"] != "tool" or not operation.get("response"):
        continue
    receipt = json.loads(operation["response"]["content"])
    assert sum(item == receipt for item in physical) == 1
    assert operation["attempts"] == 1
    tool_keys.append(key)
old_lease = next(row for row in before["weave_run_attempt_leases"] if row["run_id"] == member_id)
new_lease = next(row for row in after["weave_run_attempt_leases"] if row["run_id"] == member_id)
assert new_lease["attempt_generation"] == old_lease["attempt_generation"] + 1
report = {"recovery_receipt_checks": "passed", "full_delivery_gate": "failed",
          "run_id": run["run_id"], "member_run_id": member_id,
          "run_status": run["status"], "cause": run["cause_summary"],
          "checkpoint_before": fault["checkpoint_seq"], "checkpoint_after": member["checkpoint_seq"],
          "attempt_before": old_lease["attempt_generation"], "attempt_after": new_lease["attempt_generation"],
          "confirmed_operations_preserved": confirmed, "actual_tool_operations": tool_keys,
          "new_tool_operations_after_continue": [key for key in tool_keys if key not in original]}
(stage / "third-independent-recovery.json").write_text(json.dumps(report, ensure_ascii=False, indent=2))
print(json.dumps({k: v for k, v in report.items() if not isinstance(v, list)}, ensure_ascii=False))
