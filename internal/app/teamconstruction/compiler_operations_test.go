package teamconstruction

import (
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/frozen"
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
