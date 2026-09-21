package runtimes

import (
	"context"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestManagedRegistrationReclaimsReservedNameAfterIdentityLoss(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws')`); err != nil {
		t.Fatal(err)
	}

	store := NewStore(pool)
	oldID, oldToken, err := NewRegistrationIdentity()
	if err != nil {
		t.Fatal(err)
	}
	oldRuntime, err := store.EnsureManagedRegistration(ctx, "ws", oldID, "本机运行环境", oldToken)
	if err != nil {
		t.Fatal(err)
	}
	newID, newToken, err := NewRegistrationIdentity()
	if err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.EnsureManagedRegistration(ctx, "ws", newID, "本机运行环境", newToken)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.ID != oldRuntime.ID {
		t.Fatalf("reclaimed runtime id = %q, want %q", reclaimed.ID, oldRuntime.ID)
	}
	if _, err := store.ValidateToken(ctx, oldToken); err == nil {
		t.Fatal("old managed token remained valid")
	}
	validated, err := store.ValidateToken(ctx, newToken)
	if err != nil {
		t.Fatal(err)
	}
	if validated.ID != oldRuntime.ID {
		t.Fatalf("validated runtime id = %q, want %q", validated.ID, oldRuntime.ID)
	}
}
