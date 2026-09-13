package workflowdispatch

import (
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/snapshot"
)

func TestValidateSnapshotRequiresExactFrozenIdentityAndTrigger(t *testing.T) {
	valid := snapshot.TeamRunSnapshot{
		RunID: "run", WorkspaceID: "workspace", TeamID: "team",
		SnapshotSchemaVersion: 2, Mode: "fixed_workflow",
		WorkflowID: "workflow", WorkflowVersion: 3,
		ArtifactWorkflowID: "workflow", ArtifactWorkflowVersion: 3,
		TriggerSourceV2: json.RawMessage(`{"schema_version":1,"type":"api","source_ref":"invocation"}`),
	}
	if err := ValidateSnapshot(valid, "workspace", "workflow", "invocation", "api"); err != nil {
		t.Fatal(err)
	}

	wrongVersion := valid
	wrongVersion.ArtifactWorkflowVersion = 4
	if err := ValidateSnapshot(wrongVersion, "workspace", "workflow", "invocation", "api"); err == nil {
		t.Fatal("mismatched artifact version was accepted")
	}

	unknownTriggerField := valid
	unknownTriggerField.TriggerSourceV2 = json.RawMessage(`{"schema_version":1,"type":"api","source_ref":"invocation","user_id":"owner"}`)
	if err := ValidateSnapshot(unknownTriggerField, "workspace", "workflow", "invocation", "api"); err == nil {
		t.Fatal("unexpected trigger attribution was accepted")
	}

	trailingTrigger := valid
	trailingTrigger.TriggerSourceV2 = json.RawMessage(`{"schema_version":1,"type":"api","source_ref":"invocation"}{}`)
	if err := ValidateSnapshot(trailingTrigger, "workspace", "workflow", "invocation", "api"); err == nil {
		t.Fatal("trailing trigger value was accepted")
	}
}
