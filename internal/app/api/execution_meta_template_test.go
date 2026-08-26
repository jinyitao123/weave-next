package api

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMergeTeamTemplateDraftMetadataPersistsCompleteDraft(t *testing.T) {
	result := `{"schema_version":1,"status":"ready_for_review","idempotency_key":"44b79220-51d6-4628-b896-052f2b5a2ea2","yaml":"schema: team-template/v1\nname: demo\n","preview":{"name":"demo"},"next_action":"review_and_submit_from_console"}`
	metadata := mergeTeamTemplateDraftMetadata(nil, assistantExecutionMetadata{
		SchemaVersion: 1,
		Agent:         "__team_architect",
		ToolCalls: []assistantExecutionToolCall{{
			Name: "tf_render_template_draft", Result: result, Status: "success", StartedAt: time.Now(),
		}},
	})
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &fields); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if string(fields["team_template_draft"]) != result {
		t.Fatalf("draft metadata = %s", fields["team_template_draft"])
	}
	compacted := compactTimelineMetadata(metadata)
	if err := json.Unmarshal(compacted, &fields); err != nil {
		t.Fatalf("decode compacted metadata: %v", err)
	}
	var draft struct {
		YAML           string `json:"yaml"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.Unmarshal(fields["team_template_draft"], &draft); err != nil {
		t.Fatalf("decode compacted draft metadata: %v", err)
	}
	if draft.YAML != "schema: team-template/v1\nname: demo\n" || draft.IdempotencyKey != "44b79220-51d6-4628-b896-052f2b5a2ea2" {
		t.Fatalf("compacted draft metadata = %#v", draft)
	}
}
