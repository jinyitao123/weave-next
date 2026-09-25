package main

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

type recoverySystemProvider struct{ config llmrouter.ProviderConfig }

func (s recoverySystemProvider) SystemProvider(id string) (llmrouter.ProviderConfig, bool) {
	return s.config, s.config.ID == id
}

func TestMirrorSystemProviderRepairsUnreadableCiphertext(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	workspaceID := "system-mirror-recovery-" + uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,$1)`, workspaceID); err != nil {
		t.Fatal(err)
	}

	currentKey := []byte(strings.Repeat("k", 32))
	store := credentials.New(pool, currentKey)
	serviceID := "system-provider:deepseek"
	serviceCtx := execution.WithSubject(ctx, execution.Subject{WorkspaceID: workspaceID, ServiceID: serviceID})
	provider := func(apiKey string) recoverySystemProvider {
		return recoverySystemProvider{config: llmrouter.ProviderConfig{
			ID: "deepseek", Name: "DeepSeek", BaseURL: "https://api.deepseek.com",
			APIKey: apiKey, Models: []string{"deepseek-flash"},
			CredentialScope:     frozen.CredentialScopeWorkspaceService,
			CredentialServiceID: serviceID,
		}}
	}

	created, err := store.MirrorSystemProvider(serviceCtx, workspaceID, "operator", "deepseek", provider("old-api-key"))
	if err != nil || created.Outcome != credentials.MirrorOutcomeCreated || created.Revision != 1 {
		t.Fatalf("initial system mirror: result=%+v err=%v", created, err)
	}

	wrongCiphertext, err := secret.Seal([]byte(strings.Repeat("x", 32)), []byte("unreadable"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_provider_credentials SET api_key_cipher=$3 WHERE workspace_id=$1 AND id=$2`, workspaceID, "system/deepseek", wrongCiphertext); err != nil {
		t.Fatal(err)
	}

	repaired, err := store.MirrorSystemProvider(serviceCtx, workspaceID, "operator", "deepseek", provider("new-api-key"))
	if err != nil || repaired.Outcome != credentials.MirrorOutcomeCredentialRotated || repaired.Revision != created.Revision {
		t.Fatalf("repair unreadable system mirror: result=%+v err=%v", repaired, err)
	}

	head, err := store.GetHead(serviceCtx, workspaceID, "system/deepseek")
	if err != nil {
		t.Fatal(err)
	}
	if head.CredentialScope != frozen.CredentialScopeWorkspaceService || head.CredentialServiceID != serviceID ||
		head.SourceKind != "system_mirror" || head.SourceProviderID == nil || *head.SourceProviderID != "deepseek" ||
		head.LatestRevision != created.Revision {
		t.Fatalf("repair changed frozen provider identity or revision: %+v", head)
	}

	var ciphertext string
	if err := pool.QueryRow(ctx, `SELECT api_key_cipher FROM weave_provider_credentials WHERE workspace_id=$1 AND id=$2`, workspaceID, "system/deepseek").Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	plaintext, err := secret.Open(currentKey, ciphertext)
	if err != nil || string(plaintext) != "new-api-key" {
		t.Fatalf("repaired secret did not open with current key: plaintext_matches=%v err=%v", string(plaintext) == "new-api-key", err)
	}

	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_system_provider_mirror_audits WHERE workspace_id=$1 AND provider_id=$2 AND outcome=$3`, workspaceID, "system/deepseek", string(credentials.MirrorOutcomeCredentialRotated)).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("expected one credential rotation audit, got %d", auditCount)
	}

	if _, err := pool.Exec(ctx, `UPDATE weave_provider_credentials SET enabled=false, revoked_at=now(), deleted_at=now() WHERE workspace_id=$1 AND id=$2`, workspaceID, "system/deepseek"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MirrorSystemProvider(serviceCtx, workspaceID, "operator", "deepseek", provider("later-api-key")); err == nil {
		t.Fatal("mirror repair revived a closed system provider credential")
	}
	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM weave_provider_credentials WHERE workspace_id=$1 AND id=$2`, workspaceID, "system/deepseek").Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("closed system provider credential became enabled")
	}
}
