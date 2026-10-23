package agentcatalog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// Several exact versions resolve in one statement, in the order asked, with
// the checks a single resolution makes.
func TestResolveAgentVersionsReadsExactVersionsInOrderRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws')`); err != nil {
		t.Fatal(err)
	}
	catalog := agentcatalog.New(pool)
	lead := registry.AgentRecord{Name: "lead", Role: "avatar"}
	worker := registry.AgentRecord{Name: "worker", Role: "worker"}
	for _, rec := range []*registry.AgentRecord{&lead, &worker} {
		if err := catalog.Put(ctx, "ws", rec); err != nil {
			t.Fatal(err)
		}
	}
	first := worker.Version
	worker.DisplayName = "second"
	if err := catalog.Put(ctx, "ws", &worker); err != nil || worker.Version == first {
		t.Fatalf("second worker version: %d %v", worker.Version, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	keys := []agentcatalog.AgentVersionKey{{AgentID: worker.ID, Version: int64(first)}, {AgentID: lead.ID, Version: int64(lead.Version)}, {AgentID: worker.ID, Version: int64(worker.Version)}}
	records, err := catalog.ResolveAgentVersionsTx(ctx, tx, "ws", keys)
	if err != nil || len(records) != 3 {
		t.Fatalf("records=%d err=%v", len(records), err)
	}
	for i, key := range keys {
		if records[i].ID != key.AgentID || int64(records[i].Version) != key.Version {
			t.Fatalf("record %d = %s v%d, want %s v%d", i, records[i].ID, records[i].Version, key.AgentID, key.Version)
		}
	}
	var resolverErr *registry.ResolverError
	if _, err := catalog.ResolveAgentVersionsTx(ctx, tx, "ws", []agentcatalog.AgentVersionKey{{AgentID: lead.ID, Version: 99}}); !errors.As(err, &resolverErr) || resolverErr.Code() != registry.CodeDependencyVersionRequired {
		t.Fatalf("missing version error = %v", err)
	}
	if _, err := catalog.ResolveAgentVersionsTx(ctx, tx, "other", keys[:1]); !errors.As(err, &resolverErr) {
		t.Fatalf("another workspace's version resolved: %v", err)
	}
}
