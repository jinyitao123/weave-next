package apikeys

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestAPIKeyPersistsOwnerAndBootstrapScopesRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	workspaceID := "api-key-owner-" + uuid.NewString()
	owner, err := users.NewStore(pool).Create(ctx, workspaceID, "admin", "password", "Admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	wantScopes := []string{"admin", "org", "chat", "runs"}
	scopes := BootstrapScopes()
	if !reflect.DeepEqual(scopes, wantScopes) {
		t.Fatalf("bootstrap scopes = %v, want %v", scopes, wantScopes)
	}
	scopes[0] = "mutated"
	if !reflect.DeepEqual(BootstrapScopes(), wantScopes) {
		t.Fatal("bootstrap scopes returned shared mutable storage")
	}

	store := NewStore(pool)
	created, raw, err := store.Create(ctx, workspaceID, "codex", "admin", owner.ID, BootstrapScopes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.OwnerUserID != owner.ID || !strings.HasPrefix(raw, keyPrefix) {
		t.Fatalf("created key = %#v raw prefix valid=%v", created, strings.HasPrefix(raw, keyPrefix))
	}
	validated, err := store.Validate(ctx, raw)
	if err != nil || validated.OwnerUserID != owner.ID || !reflect.DeepEqual(validated.Scopes, wantScopes) {
		t.Fatalf("validated key = %#v error = %v", validated, err)
	}
	listed, err := store.List(ctx, workspaceID)
	if err != nil || len(listed) != 1 || listed[0].OwnerUserID != owner.ID {
		t.Fatalf("listed keys = %#v error = %v", listed, err)
	}
	if _, _, err := store.Create(ctx, workspaceID, "invalid", "admin", "missing-user", wantScopes, nil); err == nil {
		t.Fatal("key creation accepted a missing owner")
	}
}
