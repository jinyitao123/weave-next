package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func validWorkbenchContextInputRow() workbenchContextInputRow {
	inputRevisionID := uuid.NewString()
	sourceMessages, _ := json.Marshal([]dispatchInputSourceMessage{{MessageID: "message-a", EventSeq: int64Pointer(3), SHA256: strings.Repeat("a", 64)}})
	inputSHA := dispatchInputDigest([]byte("fixed task"))
	resources, _ := json.Marshal([]workbenchContextDelegatedResource{
		{Type: "dispatch-input", ID: inputRevisionID, SHA256: inputSHA},
		{Type: "forge-file", ID: "file-a", Name: "source.txt", Bytes: 10, SHA256: strings.Repeat("b", 64)},
	})
	return workbenchContextInputRow{
		InputRevisionID: inputRevisionID, RunID: "run-a", WorkbenchSessionID: "session-a",
		SourceMessages: sourceMessages, Task: "fixed task", TaskSHA256: inputSHA,
		TeamID: "team-a", WorkflowID: "workflow-a", WorkflowVersion: 2,
		RootInputRevisionID: inputRevisionID, RevisionKind: "initial", DelegatedResources: resources,
	}
}

func int64Pointer(value int64) *int64 { return &value }

func TestWorkbenchContextResultEmitsEmptyMissingItemsForComplete(t *testing.T) {
	missing := []string{}
	encoded, err := json.Marshal(workbenchContextFinalDeliverable{
		ID: "deliverable", Title: "检查意见", ContentType: "application/json", Content: "{}",
		Disposition: "complete", Summary: "本轮检查已完成", MissingItems: &missing,
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["missing_items"]) != "[]" || len(fields["disposition"]) == 0 || len(fields["summary"]) == 0 {
		t.Fatalf("complete result omits protocol fields: %s", encoded)
	}
}

func TestProjectWorkbenchRunContextUsesPersistedInputAndReferences(t *testing.T) {
	input := validWorkbenchContextInputRow()
	response, err := projectWorkbenchRunContext(input, workbenchRunRecord{RunID: "run-a", Status: "parked"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Version != "1" || response.Source.InputRevisionID != input.InputRevisionID ||
		response.Source.RunID != input.RunID || response.Source.WorkbenchSessionID != input.WorkbenchSessionID {
		t.Fatalf("source references changed: %+v", response.Source)
	}
	if response.Input.Task != input.Task || response.Input.TaskSHA256 != input.TaskSHA256 || response.Input.TeamID != input.TeamID ||
		response.Input.WorkflowID != input.WorkflowID || response.Input.WorkflowVersion != input.WorkflowVersion ||
		len(response.Input.SourceMessages) != 1 || response.Input.SourceMessages[0].MessageID != "message-a" ||
		len(response.Input.Materials) != 1 || response.Input.Materials[0].ID != "file-a" {
		t.Fatalf("input references changed: %+v", response.Input)
	}
	if response.Input.Parent == nil || response.Input.Parent.RootInputRevisionID != input.InputRevisionID || response.Run.Status != "parked" {
		t.Fatalf("lineage or run status changed: parent=%+v run=%+v", response.Input.Parent, response.Run)
	}
}

func TestProjectWorkbenchRunContextRejectsMismatchedStoredFacts(t *testing.T) {
	for name, mutate := range map[string]func(*workbenchContextInputRow, *workbenchRunRecord){
		"wrong run":    func(_ *workbenchContextInputRow, run *workbenchRunRecord) { run.RunID = "other-run" },
		"changed task": func(input *workbenchContextInputRow, _ *workbenchRunRecord) { input.Task = "changed task" },
		"bad root": func(input *workbenchContextInputRow, _ *workbenchRunRecord) {
			input.RootInputRevisionID = uuid.NewString()
		},
		"unknown revision kind": func(input *workbenchContextInputRow, _ *workbenchRunRecord) { input.RevisionKind = "unknown" },
		"invalid status":        func(_ *workbenchContextInputRow, run *workbenchRunRecord) { run.Status = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			input := validWorkbenchContextInputRow()
			run := workbenchRunRecord{RunID: "run-a", Status: "succeeded"}
			mutate(&input, &run)
			if _, err := projectWorkbenchRunContext(input, run); err == nil {
				t.Fatal("mismatched stored facts were accepted")
			}
		})
	}
}

func TestProjectWorkbenchRunContextPreservesUppercaseSourceDigest(t *testing.T) {
	input := validWorkbenchContextInputRow()
	var messages []dispatchInputSourceMessage
	if err := json.Unmarshal(input.SourceMessages, &messages); err != nil {
		t.Fatal(err)
	}
	messages[0].SHA256 = strings.Repeat("A", 64)
	input.SourceMessages, _ = json.Marshal(messages)
	response, err := projectWorkbenchRunContext(input, workbenchRunRecord{RunID: "run-a", Status: "succeeded"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Input.SourceMessages[0].SHA256 != messages[0].SHA256 {
		t.Fatalf("source digest was rewritten: got=%s want=%s", response.Input.SourceMessages[0].SHA256, messages[0].SHA256)
	}
}

func TestProjectWorkbenchContextResourcesFailsClosedOnBrokenBinding(t *testing.T) {
	input := validWorkbenchContextInputRow()
	var resources []workbenchContextDelegatedResource
	if err := json.Unmarshal(input.DelegatedResources, &resources); err != nil {
		t.Fatal(err)
	}
	resources[0].ID = "other-input"
	raw, _ := json.Marshal(resources)
	if _, _, err := projectWorkbenchContextResources(raw, input.InputRevisionID, input.TaskSHA256); err == nil {
		t.Fatal("mismatched dispatch-input sentinel was accepted")
	}
	resources[0].ID = input.InputRevisionID
	resources[1].SHA256 = strings.Repeat("z", 64)
	raw, _ = json.Marshal(resources)
	if _, _, err := projectWorkbenchContextResources(raw, input.InputRevisionID, input.TaskSHA256); err == nil {
		t.Fatal("malformed Forge material digest was accepted")
	}
}

func TestProjectWorkbenchContextPreservesFrozenOriginalSourceKind(t *testing.T) {
	input := validWorkbenchContextInputRow()
	var resources []workbenchContextDelegatedResource
	if err := json.Unmarshal(input.DelegatedResources, &resources); err != nil {
		t.Fatal(err)
	}
	resources[1] = workbenchContextDelegatedResource{
		Type: "forge-file", SourceKind: "approval", RequestID: "approval-request-a",
		MaterialID: "aaaaaaaaaaaaaaaaaaaaaaaa", ID: "file-a", Name: "审批材料.pdf",
		MediaType: "application/pdf", Bytes: 20, SHA256: strings.Repeat("b", 64),
	}
	raw, _ := json.Marshal(resources)
	materials, _, err := projectWorkbenchContextResources(raw, input.InputRevisionID, input.TaskSHA256)
	if err != nil || len(materials) != 1 || materials[0].SourceKind != "approval" ||
		materials[0].RequestID != "approval-request-a" || materials[0].MaterialID != "aaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("frozen source route was lost: materials=%+v err=%v", materials, err)
	}
	resources[1].RequestID = ""
	raw, _ = json.Marshal(resources)
	if _, _, err := projectWorkbenchContextResources(raw, input.InputRevisionID, input.TaskSHA256); err == nil {
		t.Fatal("accepted an approval source without its request ID")
	}
}
