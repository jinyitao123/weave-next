package credentials

import (
	"context"
	"errors"
	"testing"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
)

func TestProviderStoredOwnershipCannotBeRewrittenByReference(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO weave_workspaces(id,slug,name) VALUES('workspace','workspace','workspace')`); err != nil {
		t.Fatal(err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	alice := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "alice"})
	bob := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "bob"})
	create := func(ctx context.Context, cfg llmrouter.ProviderConfig) frozen.CredentialReference {
		t.Helper()
		cfg.Name = cfg.ID
		cfg.BaseURL = "https://models.invalid/v1"
		cfg.Models = []string{"model"}
		if _, err := store.UpsertRevision(ctx, "workspace", cfg); err != nil {
			t.Fatal(err)
		}
		return providerConfigReference("workspace", cfg)
	}
	a := create(alice, llmrouter.ProviderConfig{ID: "alice-provider", APIKey: "alice-private-key", CredentialScope: frozen.CredentialScopeUser, CredentialUserID: "alice"})
	b := create(bob, llmrouter.ProviderConfig{ID: "bob-provider", APIKey: "bob-private-key", CredentialScope: frozen.CredentialScopeUser, CredentialUserID: "bob"})
	serviceCtx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", ServiceID: "shared-models"})
	shared := create(serviceCtx, llmrouter.ProviderConfig{ID: "shared-provider", APIKey: "workspace-key", CredentialScope: frozen.CredentialScopeWorkspaceService, CredentialServiceID: "shared-models"})
	resolve := func(ctx context.Context, ref frozen.CredentialReference) (string, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return "", err
		}
		defer tx.Rollback(ctx)
		return store.ResolveProviderAPIKeyTx(ctx, tx, ref)
	}
	if key, err := resolve(alice, a); err != nil || key != "alice-private-key" {
		t.Fatal("alice cannot resolve her own credential")
	}
	if key, err := resolve(bob, b); err != nil || key != "bob-private-key" {
		t.Fatal("bob cannot resolve his own credential")
	}
	forged := a
	forged.UserID = "bob"
	for _, test := range []struct {
		ctx context.Context
		ref frozen.CredentialReference
	}{{bob, a}, {bob, forged}, {context.Background(), a}, {alice, shared}} {
		if _, err := resolve(test.ctx, test.ref); err == nil {
			t.Fatal("unauthorized credential resolved")
		}
	}
	if err := store.Delete(bob, "workspace", a.ResourceID); err == nil {
		t.Fatal("another user deleted alice's credential")
	}
	if _, err := store.GetHead(bob, "workspace", a.ResourceID); err == nil {
		t.Fatal("another user read alice's credential head")
	}
	if _, err := store.UpsertRevision(bob, "workspace", llmrouter.ProviderConfig{ID: a.ResourceID, Name: "takeover", BaseURL: "https://models.invalid/v1", Models: []string{"model"}, APIKey: "attacker-key", CredentialScope: frozen.CredentialScopeUser, CredentialUserID: "bob"}); err == nil {
		t.Fatal("another user overwrote the credential")
	}
	for _, test := range []struct {
		ctx  context.Context
		want string
	}{{alice, a.ResourceID}, {bob, b.ResourceID}} {
		configs, err := store.List(test.ctx, "workspace")
		if err != nil || len(configs) != 1 || configs[0].ID != test.want {
			t.Fatalf("list crossed owners: %+v %v", configs, err)
		}
		tx, err := pool.Begin(test.ctx)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := ResolveModelRevisionTx(test.ctx, tx, "workspace", "model")
		_ = tx.Rollback(test.ctx)
		if err != nil || binding.ProviderID != test.want {
			t.Fatalf("frozen model crossed owners: %+v %v", binding, err)
		}
	}
	granted := WithServiceReferenceAuthorization(alice, func(_ context.Context, s execution.Subject, ref frozen.CredentialReference) error {
		if s.UserID == "alice" && ref == shared {
			return nil
		}
		return errors.New("not granted")
	})
	if key, err := resolve(granted, shared); err != nil || key != "workspace-key" {
		t.Fatal("explicit workspace service grant unavailable")
	}
	if key, err := resolve(serviceCtx, shared); err != nil || key != "workspace-key" {
		t.Fatal("explicit service subject unavailable")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE weave_provider_credentials SET credential_user_id='bob' WHERE workspace_id='workspace' AND id='alice-provider'`); err == nil {
		t.Fatal("credential ownership can mutate after publication")
	}
	if key, err := resolve(alice, a); err != nil || key != "alice-private-key" {
		t.Fatal("denied operations changed alice's key")
	}
}
