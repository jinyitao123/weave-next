package api

import (
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/deliverable"
)

func TestWorkflowArtifactFilenameKeepsOnlyTheArtifactBaseName(t *testing.T) {
	tests := []struct {
		name     string
		metadata string
		want     string
	}{
		{name: "file", metadata: `{"filename":"ledger.jsonl"}`, want: "ledger.jsonl"},
		{name: "nested", metadata: `{"filename":"reports/source-map.html"}`, want: "source-map.html"},
		{name: "windows path", metadata: `{"filename":"reports\\evidence.csv"}`, want: "evidence.csv"},
		{name: "workflow summary", metadata: `{"filename":""}`, want: ""},
		{name: "invalid metadata", metadata: `{`, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := deliverable.FinalDeliverable{Metadata: json.RawMessage(test.metadata)}
			if got := workflowArtifactFilename(item); got != test.want {
				t.Fatalf("workflowArtifactFilename()=%q want=%q", got, test.want)
			}
		})
	}
}
