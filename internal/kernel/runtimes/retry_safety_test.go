package runtimes

import (
	"errors"
	"testing"
)

func TestRuntimeRetryRequiresNoPossiblyAdmittedInvocation(t *testing.T) {
	for _, tt := range []struct {
		taskID, message string
		retry           bool
	}{
		{"", "runtime offline", true},
		{"", "connection refused before admission", true},
		{"task-maybe-committed", "commit remote engine task: connection reset", false},
		{"task-started", "stream disconnected", false},
		{"task-started", "deadline exceeded", false},
		{"task-started", "runtime_process_interrupted: signal killed", false},
		{"task-started", "runtime_credentials_missing: ONEAPI_API_KEY", false},
		{"", "invalid execution identity", false},
	} {
		if got := canRetryRuntimeAttempt(tt.taskID, errors.New(tt.message)); got != tt.retry {
			t.Errorf("task=%q error=%q: retry=%v want=%v", tt.taskID, tt.message, got, tt.retry)
		}
	}
}
