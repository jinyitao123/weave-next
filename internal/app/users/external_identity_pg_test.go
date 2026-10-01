package users

import (
	"context"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestBindExternalCreatesStableAccountAndHonorsDisabledBindingRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewStore(pool)

	first, err := store.BindExternal(ctx, "http://forge.example.test", "forge-user-1", "workspace-1", "person@example.test", "Person")
	if err != nil {
		t.Fatalf("first binding: %v", err)
	}
	if first.Role != "member" || first.Disabled {
		t.Fatalf("first binding = %#v", first)
	}
	second, err := store.BindExternal(ctx, "http://forge.example.test", "forge-user-1", "workspace-1", "changed@example.test", "Changed")
	if err != nil {
		t.Fatalf("repeat binding: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("repeat binding created another user: first=%q second=%q", first.ID, second.ID)
	}

	if err := store.Update(ctx, "workspace-1", first.ID, first.DisplayName, "member", true); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, err := store.BindExternal(ctx, "http://forge.example.test", "forge-user-1", "workspace-1", "person@example.test", "Person"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled external user was not rejected: %v", err)
	}

	var usersCount, identitiesCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_users WHERE tenant_id='workspace-1'`).Scan(&usersCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_external_identities WHERE workspace_id='workspace-1'`).Scan(&identitiesCount); err != nil {
		t.Fatal(err)
	}
	if usersCount != 1 || identitiesCount != 1 {
		t.Fatalf("binding rows users=%d identities=%d", usersCount, identitiesCount)
	}
}

func TestNativeOrganizationBindingPreservesExistingIdentityAndRejectsWorkspaceMixRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	legacy, err := store.BindExternal(t.Context(), "http://native.test", "native-user", "default-workspace", "person@test", "Person")
	if err != nil {
		t.Fatal(err)
	}
	upgraded, err := store.BindExternalInOrganization(t.Context(), "http://native.test", "native-user", "default-workspace", "person@test", "Person", "real-native-org")
	if err != nil || upgraded.ID != legacy.ID || upgraded.TenantID != legacy.TenantID {
		t.Fatalf("trusted login changed existing workspace/user: %v", err)
	}
	if _, err := store.BindExternalInOrganization(t.Context(), "http://native.test", "native-user", "default-workspace", "person@test", "Person", "wrong-org"); err == nil {
		t.Fatal("existing identity switched native organization")
	}
	if _, err := store.BindExternalInOrganization(t.Context(), "http://native.test", "another-user", "default-workspace", "other@test", "Other", "wrong-org"); err == nil {
		t.Fatal("another native organization mixed into same issuer/workspace")
	}
	var actual string
	if err := pool.QueryRow(t.Context(), `SELECT native_organization FROM weave_external_identities WHERE user_id=$1`, legacy.ID).Scan(&actual); err != nil || actual != "real-native-org" {
		t.Fatal("native organization did not remain bound")
	}
}
