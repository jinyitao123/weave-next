package metateam

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestMetaTeamSeedDisabledSkipsAndRetainsPlatformAssets(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	workspaceID := "metateam-toggle-" + uuid.NewString()
	reg := registry.New(pool)
	orgStore := org.NewStore(pool)

	if err := EnsureMetaTeamIfEnabled(ctx, reg, orgStore, workspaceID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Get(ctx, workspaceID, TeamArchitectName); err == nil {
		t.Fatal("disabled seed created the architect")
	}

	if err := EnsureMetaTeamIfEnabled(ctx, reg, orgStore, workspaceID, true); err != nil {
		t.Fatal(err)
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
