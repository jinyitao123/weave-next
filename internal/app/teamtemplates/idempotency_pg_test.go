package teamtemplates

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestPGIdempotencyStoreClaimKeepsFirstFingerprint(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	workspaceID := "template-idempotency-" + uuid.NewString()
	key := uuid.New()
	store := NewPGIdempotencyStore(pool)
	store.now = func() time.Time { return time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC) }
	first := IdempotencyRecord{
		WorkspaceID: workspaceID, Key: key,
		Fingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BuildRunID:  "run-1", CreatedBy: "user-1",
	}
	claimed, err := store.Claim(ctx, first)
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	changed := first
	changed.Fingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	changed.BuildRunID = "run-2"
	replayed, err := store.Claim(ctx, changed)
	if err != nil {
		t.Fatalf("replay Claim() error = %v", err)
	}
	if replayed != claimed || replayed.Fingerprint != first.Fingerprint || replayed.BuildRunID != first.BuildRunID {
		t.Fatalf("replayed claim = %#v, want %#v", replayed, claimed)
	}
}
