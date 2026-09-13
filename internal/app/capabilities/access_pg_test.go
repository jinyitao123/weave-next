package capabilities

import (
	"encoding/json"
	"errors"
	"github.com/jinyitao123/weave/internal/base/execution"
	"strings"
	"testing"
)

func accessFixture(t *testing.T) (*AccessStore, *PGStore, Application, string) {
	t.Helper()
	pool, store, old := executionFixture(t)
	if _, err := store.CancelInvocation(t.Context(), "ws", "app", old.InvocationID); err != nil {
		t.Fatal(err)
	}
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
	access, store, app, raw := accessFixture(t)
	p, err := access.Authenticate(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	request := InvokeRequest{WorkspaceID: "ws", ApplicationID: app.ID, CredentialID: p.CredentialID, CapabilityID: "cap", Revision: 1, RequestID: "stable", Input: json.RawMessage(`{}`)}
	service := NewService(store, store)
	invokeCtx := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "ws", ServiceID: "capability-app:" + app.ID})
	if _, _, err := service.Invoke(invokeCtx, request); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("ungranted invocation: %v", err)
	}
	grant := VersionGrant{AppID: app.ID, CapabilityID: "cap", Revision: 1, Enabled: true}
	if err := access.SetGrant(t.Context(), "ws", "author", grant); err != nil {
		t.Fatal(err)
	}
	first, _, err := service.Invoke(invokeCtx, request)
	if err != nil {
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
	request.CredentialID = second.CredentialID
	replay, replayed, err := service.Invoke(invokeCtx, request)
	if err != nil || !replayed || replay.InvocationID != first.InvocationID {
		t.Fatalf("rotation replay=%+v %v %v", replay, replayed, err)
	}
	task, claimed, err := store.ClaimTask(t.Context())
	if err != nil || !claimed {
		t.Fatalf("rotation lost queued invocation: %v %v", claimed, err)
	}
	if _, err := store.CompleteTask(t.Context(), task, json.RawMessage(`{"ok":true}`), nil); err != nil {
		t.Fatal(err)
	}
	found, err := store.GetInvocation(t.Context(), "ws", second.AppID, first.InvocationID)
	if err != nil || found.Status != "completed" {
		t.Fatalf("rotated read=%+v %v", found, err)
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

func TestApplicationRevocationStopsQueuedAndRunningWorkRealPG(t *testing.T) {
	for _, phase := range []string{"queued", "running"} {
		t.Run(phase, func(t *testing.T) {
			access, store, app, raw := accessFixture(t)
			p, err := access.Authenticate(t.Context(), raw)
			if err != nil {
				t.Fatal(err)
			}
			grant := VersionGrant{AppID: app.ID, CapabilityID: "cap", Revision: 1, Enabled: true}
			if err := access.SetGrant(t.Context(), "ws", "author", grant); err != nil {
				t.Fatal(err)
			}
			invokeCtx := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "ws", ServiceID: "capability-app:" + app.ID})
			i, _, err := NewService(store, store).Invoke(invokeCtx, InvokeRequest{WorkspaceID: "ws", ApplicationID: app.ID, CredentialID: p.CredentialID, CapabilityID: "cap", Revision: 1, RequestID: "one", Input: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			var task InvocationTask
			if phase == "running" {
				var claimed bool
				task, claimed, err = store.ClaimTask(t.Context())
				if err != nil || !claimed {
					t.Fatal(err)
				}
			}
			grant.Enabled = false
			if err := access.SetGrant(t.Context(), "ws", "author", grant); err != nil {
				t.Fatal(err)
			}
			if phase == "queued" {
				if _, claimed, err := store.ClaimTask(t.Context()); err != nil || claimed {
					t.Fatalf("revoked grant was executed: %v %v", claimed, err)
				}
			} else {
				active, err := store.TaskActive(t.Context(), task)
				if err != nil || active {
					t.Fatalf("revoked execution active=%v %v", active, err)
				}
				final, err := store.CompleteTask(t.Context(), task, json.RawMessage(`{"late":true}`), nil)
				if err != nil || final.Status != "failed" || len(final.Result) > 0 {
					t.Fatalf("revoked result=%+v %v", final, err)
				}
			}
			found, err := store.GetInvocation(t.Context(), "ws", app.ID, i.InvocationID)
			if err != nil || found.Status != "failed" {
				t.Fatalf("revoked invocation=%+v %v", found, err)
			}
		})
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
	access, store, app, raw := accessFixture(t)
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
	_, _, err = NewService(store, store).Invoke(t.Context(), InvokeRequest{WorkspaceID: "ws", ApplicationID: app.ID, CredentialID: p.CredentialID, CapabilityID: "cap", Revision: 1, RequestID: "after-revoke", Input: json.RawMessage(`{}`)})
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("stale authenticated principal admitted: %v", err)
	}
	var count int
	if err := store.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_capability_invocations WHERE workspace_id='ws' AND application_id=$1`, app.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unauthorized durable invocation: count=%d err=%v", count, err)
	}
}
