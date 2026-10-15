package api

import (
	"net/http"
	"testing"

	"github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func TestCapabilityWorkflowAppliesFullOutputSchemaAfterTeamRunRealPG(t *testing.T) {
	f := newCapabilityWorkflowContractFixture(t)
	definition := capability.Definition{
		SchemaVersion: 1, CapabilityID: "strict-output", Name: "Strict output capability",
		InputSchema:  []byte(`{"type":"object","additionalProperties":false,"properties":{"document":{"type":"string"}},"required":["document"]}`),
		OutputSchema: []byte(`{"type":"object","additionalProperties":false,"properties":{"document":{"type":"string","minLength":10}},"required":["document"]}`),
		Workflow:     &capability.WorkflowReference{WorkflowID: "flow", WorkflowVersion: 1},
	}
	if err := f.server.Capabilities.SaveDraft(t.Context(), capabilities.DraftRequest{WorkspaceID: "ws", Definition: definition}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.Capabilities.Publish(t.Context(), "ws", definition.CapabilityID, 1); err != nil {
		t.Fatal(err)
	}
	app, key := f.application(t, "strict output app")
	f.grant(t, app.ID, definition.CapabilityID, true)
	invoked := f.call(t, key, http.MethodPost, workflowInvocationPath(definition.CapabilityID, 1), map[string]any{
		"request_id": "strict-output-request", "input": map[string]any{"document": "Short"},
	})
	if invoked.status != http.StatusAccepted {
		t.Fatalf("workflow invocation status=%d body=%+v", invoked.status, invoked.body)
	}
	invocationID := invoked.stringField(t, "invocation_id")
	processOneCapabilityWorkflowRun(t, f)
	runID := invoked.stringField(t, "run_id")
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO weave_final_deliverables(
	 id,workspace_id,user_id,lead_avatar_id,session_id,event_id,run_id,run_snapshot_id,title,content,content_type,metadata)
	 SELECT 'conflicting-valid-final','ws',$1,t.lead_avatar_id,'workflow:'||snapshot.run_id,'conflicting-final',
	 snapshot.run_id,snapshot.run_id,'Final','{"document":"This artifact passes minLength."}','application/json',
	 '{"source":"published_workflow","artifact_kind":"final","node_id":"deliver","node_type":"deliver"}'
	 FROM weave_team_run_snapshots snapshot JOIN weave_teams t ON t.workspace_id=snapshot.workspace_id AND t.id=snapshot.team_id
	 WHERE snapshot.workspace_id='ws' AND snapshot.run_id=$2 AND snapshot.mode='fixed_workflow'`, app.ID, runID); err != nil {
		t.Fatalf("seed conflicting but schema-valid final artifact: %v", err)
	}
	view := f.call(t, key, http.MethodGet, "/v1/invocations/"+invocationID, nil)
	if view.status != http.StatusOK || view.stringField(t, "status") != "completed" ||
		view.stringField(t, "result_state") != "invalid" || view.stringField(t, "error") != "output_invalid" {
		t.Fatalf("capability output schema was not applied to TeamRun result: status=%d body=%+v", view.status, view.body)
	}
	if _, ok := view.body["result"]; ok {
		t.Fatalf("schema-invalid workflow result was exposed: %s", view.body["result"])
	}
}

func processOneCapabilityWorkflowRun(t *testing.T, f *capabilityWorkflowContractFixture) {
	t.Helper()
	runs := teamrun.NewPGStore()
	runs.Transactions = f.pool
	checkpoints := teamrun.NewPGCheckpointStore()
	runtime := &teamrun.WorkflowSerialRuntime{
		Artifacts: f.server.WorkflowArtifacts, Loader: &workflow.RuntimeLoader{},
		HostFactory: workflow.NewRuntimeHostFactory(),
		CredentialResolvers: func(string) (workflow.RuntimeCredentialResolver, error) {
			return capabilityWorkflowEmptyCredentialResolver{}, nil
		},
		Transactions: f.pool, Runs: runs, Checkpoints: checkpoints, Tasks: f.server.Tasks,
		Snapshots: f.server.Snapshots,
	}
	consumer := &teamrun.Consumer{Transactions: f.pool, Snapshots: f.server.Snapshots, Runs: runs, Tasks: f.server.Tasks}
	executor := &teamrun.Executor{Tasks: f.server.Tasks, Transactions: f.pool, Runs: runs,
		Checkpoints: checkpoints, Runtime: runtime, Consumer: consumer}
	worked, err := executor.ProcessNext(t.Context(), "capability-output-test-worker")
	if err != nil || !worked {
		t.Fatalf("process published capability workflow: worked=%v err=%v", worked, err)
	}
}
