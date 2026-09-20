package runtimes

import (
	"context"
	"testing"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestManagedRegistrationReplacesLostLocalIdentityRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws')`); err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	firstID, firstToken, err := NewRegistrationIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.EnsureManagedRegistration(ctx, "ws", firstID, "local", firstToken); err != nil {
		t.Fatal(err)
	}
	secondID, secondToken, err := NewRegistrationIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.EnsureManagedRegistration(ctx, "ws", secondID, "local", secondToken); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ValidateToken(ctx, firstToken); err != ErrInvalidRuntimeToken {
		t.Fatalf("old token error = %v", err)
	}
	if _, err = store.ValidateToken(ctx, secondToken); err != nil {
		t.Fatal(err)
	}
}
