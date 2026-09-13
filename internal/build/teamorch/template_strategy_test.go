package teamorch

import (
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamforge"
)

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
