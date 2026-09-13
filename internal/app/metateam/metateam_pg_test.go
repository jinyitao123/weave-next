package metateam

import (
	"context"
	"testing"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"

	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestMetaTeamSeedDisabledSkipsAndRetainsPlatformAssets(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	workspaceID := "metateam-toggle-" + uuid.NewString()
	reg := agentcatalog.New(pool)
	orgStore := orgstore.NewStore(pool)

	if err := EnsureMetaTeamIfEnabled(ctx, reg, orgStore, workspaceID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Get(ctx, workspaceID, TeamArchitectName); err == nil {
		t.Fatal("disabled seed created the architect")
	}

	if err := EnsureMetaTeamIfEnabled(ctx, reg, orgStore, workspaceID, true); err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{ConfigEngineerName, GraphDesignerName} {
		if _, err := reg.Get(ctx, workspaceID, retired); err == nil {
			t.Fatalf("clean seed registered build-controlled role %q", retired)
		}
	}
	before := make(map[string]int, 6)
	for _, builtin := range metaTeamAgents() {
		record, err := reg.Get(ctx, workspaceID, builtin.name)
		if err != nil {
			t.Fatalf("read %q: %v", builtin.name, err)
		}
		if record.Visibility != registry.VisibilityPlatform {
			t.Fatalf("%q visibility = %q", builtin.name, record.Visibility)
		}
		before[builtin.name] = record.Version
	}
	teams, err := orgStore.ListTeams(ctx, workspaceID)
	if err != nil || len(teams) != 1 || teams[0].Name != TeamName {
		t.Fatalf("seeded teams = %#v error = %v", teams, err)
	}
	workers, err := reg.ListTeamWorkers(ctx, workspaceID)
	if err != nil || len(workers) != 1 {
		t.Fatalf("seeded workers = %#v error = %v", workers, err)
	}
	evaluator, err := reg.Get(ctx, workspaceID, EvalDebuggerName)
	if err != nil || workers[0].WorkerAgentID != evaluator.ID {
		t.Fatalf("remaining meta worker = %#v evaluator = %#v error = %v", workers[0], evaluator, err)
	}

	if err := EnsureMetaTeamIfEnabled(ctx, reg, orgStore, workspaceID, false); err != nil {
		t.Fatal(err)
	}
	for name, version := range before {
		record, err := reg.Get(ctx, workspaceID, name)
		if err != nil {
			t.Fatalf("retained %q: %v", name, err)
		}
		if record.Version != version {
			t.Fatalf("retained %q version = %d, want %d", name, record.Version, version)
		}
	}
}
