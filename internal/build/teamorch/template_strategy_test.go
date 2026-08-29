package teamorch

import (
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamforge"
)

func TestWorkflowDraftContentHashUsesCompilerEnvelope(t *testing.T) {
	trigger := json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`)
	graph := json.RawMessage(`{"schema_version":1,"entry_node_id":"start","nodes":[],"edges":[]}`)
	got, err := workflowDraftContentHash(trigger, graph)
	if err != nil {
		t.Fatal(err)
	}
	want, err := frozen.HashCanonicalJSON(json.RawMessage(
		`{"trigger_config":{"schema_version":1,"type":"conversation_explicit","config":{}},` +
			`"graph_definition":{"schema_version":1,"entry_node_id":"start","nodes":[],"edges":[]}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("workflowDraftContentHash() = %q, want %q", got, want)
	}
}

func TestTemplateInstantiatedTeamID(t *testing.T) {
	evidence, err := json.Marshal(map[string]any{
		"operation_id": "op", "tool_result": map[string]any{"team": map[string]any{"id": "team-123"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := templateInstantiatedTeamID([]teambuild.OperationStep{{
		OperationType: string(teamforge.OperationTeamCreate),
		Status:        teambuild.OperationStatusSucceeded, EvidenceJSON: evidence,
	}})
	if err != nil || got != "team-123" {
		t.Fatalf("templateInstantiatedTeamID() = %q, %v", got, err)
	}
}

func TestTemplateInstantiatedTeamIDFailsClosed(t *testing.T) {
	cases := []struct {
		name  string
		steps []teambuild.OperationStep
	}{
		{name: "missing"},
		{name: "failed create", steps: []teambuild.OperationStep{{OperationType: string(teamforge.OperationTeamCreate), Status: teambuild.OperationStatusFailed}}},
		{name: "missing id", steps: []teambuild.OperationStep{{OperationType: string(teamforge.OperationTeamCreate), Status: teambuild.OperationStatusSucceeded, EvidenceJSON: json.RawMessage(`{"tool_result":{"team":{}}}`)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := templateInstantiatedTeamID(tc.steps); err == nil {
				t.Fatal("templateInstantiatedTeamID() error = nil")
			}
		})
	}
}
