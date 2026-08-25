package teamtemplates

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/build/teambuild"
)

func TestDeclarativeTemplatePersistsFrozenSecondRevision(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	workspaceID := "template-declarative-" + uuid.NewString()
	builds := teambuild.New(pool, teambuild.RealClock{})
	service := New(NewPGIdempotencyStore(pool), builds, noopSubmitter{}, Options{
		Policy: testPolicy(), ReadyTimeout: 2 * time.Millisecond, PollInterval: time.Millisecond,
	})
	spec := declarativeTestSpec(t)
	outcome, err := service.Instantiate(ctx, workspaceID, "user-1", Request{
		YAML: validTemplateYAML, DeclarativeSpec: &spec, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("Instantiate() error = %v", err)
	}
	if outcome.Status != "building" {
		t.Fatalf("outcome = %#v, want asynchronous building", outcome)
	}
	revision, err := builds.GetLatestBlueprintRevision(ctx, workspaceID, outcome.BuildRunID)
	if err != nil {
		t.Fatalf("GetLatestBlueprintRevision() error = %v", err)
	}
	if revision.RevisionNo != 2 || revision.WorkflowMode != teambuild.BlueprintWorkflowDeclarativeV1 {
		t.Fatalf("revision = %#v", revision)
	}
	run, err := builds.GetBuildRun(ctx, workspaceID, outcome.BuildRunID)
	if err != nil {
		t.Fatalf("GetBuildRun() error = %v", err)
	}
	if run.Authorization.RevisionToken == nil || run.Authorization.RevisionToken.RevisionNo != 2 ||
		run.ExecutionStrategy != teambuild.ExecutionStrategyTemplateInstantiate {
		t.Fatalf("authorized run = %#v", run)
	}
}

type noopSubmitter struct{}

func (noopSubmitter) Submit(context.Context, string, string) error { return nil }
