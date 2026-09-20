package workflowhealth

import (
	"context"
	"testing"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestObserveBatchUsesCurrentSnapshotSchemaRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store, err := New(pool, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	written, err := store.ObserveBatch(ctx, 1)
	if err != nil {
		t.Fatalf("observe current schema: %v", err)
	}
	if written != 0 {
		t.Fatalf("written = %d, want 0", written)
	}
}
