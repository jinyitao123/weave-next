package api

import "testing"

func TestCodeVerdictRequiresHostEvidenceOnTheDeliveredCommit(t *testing.T) {
	stage := func(head string, evidence *taskCodeEvidence) *taskCodeStage {
		return &taskCodeStage{Version: taskCodeVersion{SchemaVersion: 1, HeadSHA: head, TreeSHA: "tree"}, Evidence: evidence}
	}
	commands := []string{"go test ./...", "go vet ./..."}
	ok := []taskCodeCommand{{Command: "go test ./...", ExitCode: 0}, {Command: "go vet ./...", ExitCode: 0}}
	cases := []struct {
		name   string
		status string
		view   taskCodeView
		want   string
		reason string
	}{
		{"running", "running", taskCodeView{VerifyCommands: commands}, "pending", ""},
		{"no version", "succeeded", taskCodeView{VerifyCommands: commands}, "unknown", "no_code_version"},
		{"nothing declared", "succeeded", taskCodeView{Final: stage("b", nil)}, "not_declared", "no_verify_commands"},
		{"no evidence", "succeeded", taskCodeView{VerifyCommands: commands, Final: stage("b", nil)}, "unknown", "no_evidence"},
		{"evidence on another commit", "succeeded", taskCodeView{VerifyCommands: commands, Final: stage("b", &taskCodeEvidence{Commit: "a", Commands: ok})}, "unknown", "evidence_commit_mismatch"},
		{"missing command", "succeeded", taskCodeView{VerifyCommands: commands, Final: stage("b", &taskCodeEvidence{Commit: "b", Tree: "tree", TreeUnchanged: true, Commands: ok[:1]})}, "unknown", "evidence_incomplete"},
		{"different command", "succeeded", taskCodeView{VerifyCommands: commands, Final: stage("b", &taskCodeEvidence{Commit: "b", Tree: "tree", TreeUnchanged: true, Commands: []taskCodeCommand{ok[0], {Command: "true", ExitCode: 0}}})}, "unknown", "evidence_commands_mismatch"},
		{"failed command", "succeeded", taskCodeView{VerifyCommands: commands, Final: stage("b", &taskCodeEvidence{Commit: "b", Tree: "tree", TreeUnchanged: true, Commands: []taskCodeCommand{ok[0], {Command: "go vet ./...", ExitCode: 1}}})}, "failed", "command_failed"},
		{"timed out", "succeeded", taskCodeView{VerifyCommands: commands, Final: stage("b", &taskCodeEvidence{Commit: "b", Tree: "tree", TreeUnchanged: true, Commands: []taskCodeCommand{ok[0], {Command: "go vet ./...", TimedOut: true}}})}, "failed", "command_failed"},
		{"changed tracked tree", "succeeded", taskCodeView{VerifyCommands: commands, Final: stage("b", &taskCodeEvidence{Commit: "b", Tree: "tree", Commands: ok})}, "unknown", "evidence_tree_mismatch"},
		{"missing tree evidence", "succeeded", taskCodeView{VerifyCommands: commands, Final: stage("b", &taskCodeEvidence{Commit: "b", TreeUnchanged: true, Commands: ok})}, "unknown", "evidence_tree_mismatch"},
		{"all passed", "succeeded", taskCodeView{VerifyCommands: commands, Final: stage("b", &taskCodeEvidence{Commit: "b", Tree: "tree", TreeUnchanged: true, Commands: ok})}, "passed", ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := codeVerdict(test.status, test.view)
			if got.Status != test.want || got.Reason != test.reason {
				t.Fatalf("verdict = %+v, want %s/%s", got, test.want, test.reason)
			}
		})
	}
}
