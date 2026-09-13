package fanout

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

type isolatingCoordinator struct {
	failIntent string
	failGroup  string
	intents    []string
	groups     []string
}

func (c *isolatingCoordinator) PreparePark(context.Context, pgx.Tx, PrepareParkRequest) (ParkIntent, error) {
	return ParkIntent{}, nil
}

func (c *isolatingCoordinator) PrepareFreeCollabSynthesis(context.Context, pgx.Tx, PrepareParkRequest) (ParkIntent, error) {
	return ParkIntent{}, nil
}

func (c *isolatingCoordinator) ActivatePark(context.Context, ActivateParkRequest) (ActivationResult, error) {
	return ActivationResult{}, nil
}

func (c *isolatingCoordinator) RecordLegCompletion(context.Context, LegCompletionRequest) (LegCompletionResult, error) {
	return LegCompletionResult{}, nil
}

func (c *isolatingCoordinator) ReconcileIntent(_ context.Context, _, id string) error {
	c.intents = append(c.intents, id)
	if id == c.failIntent {
		return errors.New("bad intent")
	}
	return nil
}

func (c *isolatingCoordinator) ReconcileGroup(_ context.Context, _, id string) error {
	c.groups = append(c.groups, id)
	if id == c.failGroup {
		return errors.New("bad group")
	}
	return nil
}

func TestReconcileCandidatesIsolatesFailures(t *testing.T) {
	coordinator := &isolatingCoordinator{failIntent: "intent-bad", failGroup: "group-bad"}
	worker := &WorkflowReconcilerWorker{Coordinator: coordinator}

	processed, err := worker.reconcileCandidates(
		context.Background(), "default",
		[]string{"intent-bad", "intent-good"},
		[]string{"group-bad", "group-good"},
	)

	if processed != 2 {
		t.Fatalf("processed = %d, want 2", processed)
	}
	if err == nil {
		t.Fatal("expected aggregated reconciliation error")
	}
	if len(coordinator.intents) != 2 || coordinator.intents[1] != "intent-good" {
		t.Fatalf("intent calls = %v", coordinator.intents)
	}
	if len(coordinator.groups) != 2 || coordinator.groups[1] != "group-good" {
		t.Fatalf("group calls = %v", coordinator.groups)
	}
}
