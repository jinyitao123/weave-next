package teamtemplates

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/app/metateam"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestDeclarativeTemplatePersistsFrozenSecondRevision(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	workspaceID := "template-declarative-" + uuid.NewString()
	reg := registry.New(pool)
	if err := metateam.EnsureMetaTeam(ctx, reg, org.NewStore(pool), workspaceID); err != nil {
		t.Fatalf("seed clean workspace: %v", err)
	}
	for _, name := range []string{metateam.ConfigEngineerName, metateam.GraphDesignerName} {
		if _, err := reg.Get(ctx, workspaceID, name); err == nil {
			t.Fatalf("clean workspace contains retired registry role %q", name)
		}
	}
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

func TestDeclarativeTemplateIgnoresRetainedLegacyConstructionAgents(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	workspaceID := "template-upgraded-" + uuid.NewString()
	reg := registry.New(pool)
	if err := metateam.EnsureMetaTeam(ctx, reg, org.NewStore(pool), workspaceID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{metateam.ConfigEngineerName, metateam.GraphDesignerName} {
		if err := reg.Put(ctx, workspaceID, &registry.AgentRecord{
			Name: name, Role: "worker", Visibility: registry.VisibilityPlatform,
			Spec: stdlib.AgentSpec{Identity: stdlib.IdentitySpec{Core: "retained legacy seed"}},
		}); err != nil {
			t.Fatalf("seed retained %q: %v", name, err)
		}
	}
	builds := teambuild.New(pool, teambuild.RealClock{})
	service := New(NewPGIdempotencyStore(pool), builds, noopSubmitter{}, Options{
		Policy: testPolicy(), ReadyTimeout: 2 * time.Millisecond, PollInterval: time.Millisecond,
	})
	spec := declarativeTestSpec(t)
	outcome, err := service.Instantiate(ctx, workspaceID, "user-1", Request{
		YAML: validTemplateYAML, DeclarativeSpec: &spec, IdempotencyKey: uuid.NewString(),
	})
	if err != nil || outcome.Status != "building" {
		t.Fatalf("upgraded template outcome = %#v error = %v", outcome, err)
	}
	for _, name := range []string{metateam.ConfigEngineerName, metateam.GraphDesignerName} {
		if record, err := reg.Get(ctx, workspaceID, name); err != nil || record.Spec.Identity.Core != "retained legacy seed" {
			t.Fatalf("legacy role %q was not retained read-only: record=%#v err=%v", name, record, err)
		}
	}
}

type noopSubmitter struct{}

func (noopSubmitter) Submit(context.Context, string, string) error { return nil }
