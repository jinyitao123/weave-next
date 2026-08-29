package api

import "testing"

func TestDeterministicWorkflowDispatchIDs(t *testing.T) {
	runID, taskID := deterministicWorkflowDispatchIDs("workspace", "user", "request")
	replayedRunID, replayedTaskID := deterministicWorkflowDispatchIDs("workspace", "user", "request")
	if runID != replayedRunID || taskID != replayedTaskID {
		t.Fatalf("retry IDs changed: (%q, %q) != (%q, %q)", runID, taskID, replayedRunID, replayedTaskID)
	}
	otherRunID, otherTaskID := deterministicWorkflowDispatchIDs("workspace", "other-user", "request")
	if runID == otherRunID || taskID == otherTaskID {
		t.Fatal("dispatch IDs must be scoped to the caller identity")
	}
}

func TestWorkflowDispatchFingerprintCoversDispatchFacts(t *testing.T) {
	version := 2
	base := teamDispatchRequest{
		Task:            "ship it",
		Mode:            teamDispatchModeWorkflow,
		WorkflowID:      "workflow-1",
		WorkflowVersion: &version,
		ClientRequestID: "request-1",
		ProjectID:       "project-1",
		ConversationID:  "conversation-1",
	}
	want := workflowDispatchFingerprint("team-1", base)
	if got := workflowDispatchFingerprint("team-1", base); got != want {
		t.Fatalf("same facts produced different fingerprints: %q != %q", got, want)
	}

	mutations := []struct {
		name string
		edit func(*teamDispatchRequest)
	}{
		{"task", func(request *teamDispatchRequest) { request.Task = "changed" }},
		{"mode", func(request *teamDispatchRequest) { request.Mode = teamDispatchModeFreeCollab }},
		{"workflow", func(request *teamDispatchRequest) { request.WorkflowID = "workflow-2" }},
		{"version", func(request *teamDispatchRequest) { changed := 3; request.WorkflowVersion = &changed }},
		{"project", func(request *teamDispatchRequest) { request.ProjectID = "project-2" }},
		{"conversation", func(request *teamDispatchRequest) { request.ConversationID = "conversation-2" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := base
			mutation.edit(&changed)
			if got := workflowDispatchFingerprint("team-1", changed); got == want {
				t.Fatalf("%s change did not affect dispatch fingerprint", mutation.name)
			}
		})
	}
	if got := workflowDispatchFingerprint("team-2", base); got == want {
		t.Fatal("team change did not affect dispatch fingerprint")
	}
}
