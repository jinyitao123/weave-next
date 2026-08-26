package teamorch

import (
	"context"
	"fmt"
	"testing"

	"github.com/jinyitao123/weave/internal/build/teambuild"
)

func TestServiceMapsEvaluationBaselineCASFailureToBlocked(t *testing.T) {
	service := NewService(
		fakeRoundDriver{result: Result{WorkspaceID: "workspace-1", BuildRunID: "run-1", Status: teambuild.StatusPublishing}},
		fakePublisher{err: fmt.Errorf("publish: %w", teambuild.ErrEvaluationBaselineChanged)},
	)
	result, err := service.Execute(context.Background(), "workspace-1", "run-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != teambuild.StatusBlocked || result.StopReason != "evaluation_baseline_cas_failed" {
		t.Fatalf("result = %#v", result)
	}
}

type fakeRoundDriver struct {
	result Result
	err    error
}

func (f fakeRoundDriver) Run(context.Context, string, string) (Result, error) {
	return f.result, f.err
}

type fakePublisher struct{ err error }

func (f fakePublisher) PublishStep(context.Context, string, string) error { return f.err }
