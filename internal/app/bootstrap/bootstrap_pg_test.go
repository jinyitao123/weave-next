package bootstrap

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestEnsureIsIdempotentAndExplicitlyResetsPasswordRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	workspaceID := "bootstrap-" + uuid.NewString()
	userStore, keyStore := users.NewStore(pool), apikeys.NewStore(pool)
	service := Service{Users: userStore, Keys: keyStore}
	options := Options{
		WorkspaceID: workspaceID, Username: "admin",
		APIURL: " http://127.0.0.1:8080/ ",
	}
	first, err := service.Ensure(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if !first.AdminCreated || first.GeneratedPassword == "" || !first.APIKeyCreated ||
		!strings.HasPrefix(first.APIKey, "wv_sk_") ||
		!reflect.DeepEqual(first.APIKeyScopes, []string{"admin", "org", "chat", "runs"}) {
		t.Fatalf("first bootstrap = %#v", first)
	}
	if _, err := userStore.Authenticate(ctx, workspaceID, "admin", first.GeneratedPassword); err != nil {
		t.Fatalf("generated password cannot authenticate: %v", err)
	}
	if first.APIURL != "http://127.0.0.1:8080" {
		t.Fatalf("Workbench service address = %q", first.APIURL)
	}
	payload, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var connection map[string]json.RawMessage
	if err := json.Unmarshal(payload, &connection); err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{"mcp", "command", "codex_toml", "claude_command"} {
		if _, exists := connection[retired]; exists {
			t.Errorf("bootstrap still emits retired client configuration %q", retired)
		}
	}

	secondOptions := options
	secondOptions.Password = "must-not-reset-without-flag"
	second, err := service.Ensure(ctx, secondOptions)
	if err != nil {
		t.Fatal(err)
	}
	if second.AdminCreated || second.PasswordReset || second.APIKeyCreated || second.APIKey != "" ||
		second.APIKeyID != first.APIKeyID || second.AdminUserID != first.AdminUserID {
		t.Fatalf("second bootstrap = %#v", second)
	}
	if _, err := userStore.Authenticate(ctx, workspaceID, "admin", first.GeneratedPassword); err != nil {
		t.Fatalf("idempotent bootstrap changed password: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE weave_users SET role='user',disabled=true WHERE id=$1;
		UPDATE weave_members SET role='member' WHERE workspace_id=$2 AND user_id=$1`, first.AdminUserID, workspaceID); err != nil {
		t.Fatal(err)
	}
	resetOptions := options
	resetOptions.Password, resetOptions.ResetPassword = "new-bootstrap-password", true
	reset, err := service.Ensure(ctx, resetOptions)
	if err != nil {
		t.Fatal(err)
	}
	if !reset.PasswordReset || reset.APIKeyCreated || reset.APIKeyID != first.APIKeyID {
		t.Fatalf("reset bootstrap = %#v", reset)
	}
	if _, err := userStore.Authenticate(ctx, workspaceID, "admin", resetOptions.Password); err != nil {
		t.Fatalf("reset password cannot authenticate: %v", err)
	}
}
