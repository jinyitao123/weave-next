package capabilities

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func accessFixture(t *testing.T) (*AccessStore, *PGStore, Application, string) {
	t.Helper()
	pool, store := publishedCapabilityFixture(t)
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws') ON CONFLICT DO NOTHING;
 INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('author','ws','author','unused','developer')`); err != nil {
		t.Fatal(err)
	}
	access := NewAccessStore(pool)
	app, err := access.CreateApplication(t.Context(), "ws", "author", "consumer")
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := access.IssueCredential(t.Context(), "ws", "author", app.ID, "first", []string{"invoke", "read", "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	return access, store, app, raw
}

func TestApplicationCredentialRotationPreservesIdentityRealPG(t *testing.T) {
	access, _, app, raw := accessFixture(t)
	p, err := access.Authenticate(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := access.AuthorizeInvocation(t.Context(), p, "cap", 1); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("ungranted invocation: %v", err)
	}
	grant := VersionGrant{AppID: app.ID, CapabilityID: "cap", Revision: 1, Enabled: true}
	if err := access.SetGrant(t.Context(), "ws", "author", grant); err != nil {
		t.Fatal(err)
	}
	if err := access.AuthorizeInvocation(t.Context(), p, "cap", 1); err != nil {
		t.Fatal(err)
	}
	_, secondKey, err := access.IssueCredential(t.Context(), "ws", "author", app.ID, "second", []string{"invoke", "read", "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := access.Authenticate(t.Context(), secondKey)
	if err != nil {
		t.Fatal(err)
	}
	if second.AppID != p.AppID || second.CredentialID == p.CredentialID {
		t.Fatal("rotation changed application identity")
	}
	if err := access.RevokeCredential(t.Context(), "ws", "author", app.ID, p.CredentialID); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Authenticate(t.Context(), raw); !errors.Is(err, ErrAccessDenied) {
		t.Fatal("revoked credential still works")
	}
	if err := access.AuthorizeInvocation(t.Context(), second, "cap", 1); err != nil {
		t.Fatalf("rotated credential lost the application's grant: %v", err)
	}
	snapshot, err := access.Snapshot(t.Context(), "ws", "author")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), raw) || strings.Contains(string(encoded), secondKey) || strings.Contains(string(encoded), "key_hash") {
		t.Fatal("snapshot leaked a credential")
	}
}

func TestApplicationCredentialBoundsAndActorChecksRealPG(t *testing.T) {
	access, store, app, raw := accessFixture(t)
	if _, _, err := access.IssueCredential(t.Context(), "ws", "author", app.ID, "bad", []string{"manage"}); err == nil {
		t.Fatal("application got authoring scope")
	}
	if _, err := access.CreateApplication(t.Context(), "other", "author", "cross-workspace"); !errors.Is(err, ErrAccessDenied) {
		t.Fatal("cross workspace actor allowed")
	}
	p, err := access.Authenticate(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), `UPDATE weave_users SET disabled=true WHERE tenant_id='ws' AND id='author'`); err != nil {
		t.Fatal(err)
	}
	if err := access.RevokeCredential(t.Context(), "ws", "author", app.ID, p.CredentialID); !errors.Is(err, ErrAccessDenied) {
		t.Fatal("disabled manager allowed")
	}
	if _, err := store.pool.Exec(t.Context(), `UPDATE weave_users SET disabled=false WHERE tenant_id='ws' AND id='author'`); err != nil {
		t.Fatal(err)
	}
	if err := access.SetApplicationEnabled(t.Context(), "ws", "author", app.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Authenticate(t.Context(), raw); !errors.Is(err, ErrAccessDenied) {
		t.Fatal("disabled application allowed")
	}
}

func TestApplicationAdmissionRechecksRevocationAfterAuthenticationRealPG(t *testing.T) {
	access, _, app, raw := accessFixture(t)
	p, err := access.Authenticate(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := access.SetGrant(t.Context(), "ws", "author", VersionGrant{AppID: app.ID, CapabilityID: "cap", Revision: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := access.AuthorizeInvocation(t.Context(), p, "cap", 1); err != nil {
		t.Fatal(err)
	}
	if err := access.RevokeCredential(t.Context(), "ws", "author", app.ID, p.CredentialID); err != nil {
		t.Fatal(err)
	}
	if err := access.AuthorizeInvocation(t.Context(), p, "cap", 1); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("stale authenticated principal admitted: %v", err)
	}
}
