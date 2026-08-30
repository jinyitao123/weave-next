package api

import (
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
)

func TestTeamProductRunStatus(t *testing.T) {
	tests := []struct {
		name           string
		classification loomruntime.RunLifecycleClassification
		terminal       *loomruntime.TerminalEntryV3
		want           string
	}{
		{name: "active", classification: loomruntime.RunLifecycleTerminalPendingActive, want: "running"},
		{name: "queued continuation with expired lease", classification: loomruntime.RunLifecycleTerminalMissing, want: "running"},
		{name: "human wait", classification: loomruntime.RunLifecycleTerminalStageYielded, want: "yielded"},
		{name: "success", classification: loomruntime.RunLifecycleTerminalComplete, terminal: &loomruntime.TerminalEntryV3{Status: "success"}, want: "completed"},
		{name: "business failure", classification: loomruntime.RunLifecycleTerminalComplete, terminal: &loomruntime.TerminalEntryV3{Status: "failed"}, want: "failed"},
		{name: "projection cannot finish", classification: loomruntime.RunLifecycleTerminalProjectionBlocked, want: "failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := teamProductRunStatus(string(test.classification), test.terminal); got != test.want {
				t.Fatalf("status = %q, want %q", got, test.want)
			}
		})
	}
}
