package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

// plantForgeTaskDelegation stores a Forge-issued delegation for the input that
// the run consumed, as dispatch-input registration does.
func plantForgeTaskDelegation(t *testing.T, pool *pgxpool.Pool, runID, baseURL, token string, issuedAt, expiresAt time.Time) string {
	t.Helper()
	var revision string
	if err := pool.QueryRow(t.Context(), `SELECT input_revision_id FROM weave_dispatch_input_revisions WHERE workspace_id='ws' AND consumed_run_id=$1`, runID).Scan(&revision); err != nil {
		t.Fatalf("read the run's input revision: %v", err)
	}
	key, err := secret.KeyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := secret.Seal(key, []byte(token))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(token))
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_task_business_delegations
		(workspace_id,user_id,input_revision_id,delegation_id,credential_ref,issuer,external_subject,external_organization,
		 credential_ciphertext,credential_sha256,allowed_actions,resources,workflow_id,workflow_version,issued_at,expires_at,
		 forge_base_url,forge_delegation_id)
		VALUES('ws','user',$1,gen_random_uuid(),'ref-revoke','forge:test-deployment','forge-user','ws',$2,$3,'[]'::jsonb,'[]'::jsonb,'flow',1,$4,$5,$6,'forge-delegation-1')`,
		revision, ciphertext, hex.EncodeToString(digest[:]), issuedAt, expiresAt, baseURL); err != nil {
		t.Fatalf("plant delegation: %v", err)
	}
	return revision
}

type revokedDelegationRow struct {
	revoked  bool
	reason   string
	attempts int
	retryAt  *time.Time
}

func readRevokedDelegation(t *testing.T, pool *pgxpool.Pool, revision string) revokedDelegationRow {
	t.Helper()
	var row revokedDelegationRow
	var reason *string
	if err := pool.QueryRow(t.Context(), `SELECT revoked_at IS NOT NULL,revocation_reason,revoke_attempts,revoke_next_attempt_at
		FROM weave_task_business_delegations WHERE workspace_id='ws' AND input_revision_id=$1`, revision).Scan(
		&row.revoked, &reason, &row.attempts, &row.retryAt); err != nil {
		t.Fatal(err)
	}
	if reason != nil {
		row.reason = *reason
	}
	return row
}

func TestTaskDelegationRevokerRevokesAfterTerminalRunRealPG(t *testing.T) {
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("11", 32))
	pool, runID := succeededRunForOutboxTest(t)
	var calls atomic.Int32
	failing := atomic.Bool{}
	failing.Store(true)
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/workbench/task-delegations/forge-delegation-1" ||
			r.Header.Get("Authorization") != "Bearer task-credential" || string(body) != `{"reason":"run_terminal"}` {
			t.Errorf("unexpected revoke request %s %s auth=%q body=%s", r.Method, r.URL.Path, r.Header.Get("Authorization"), body)
		}
		if failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"version":"1","delegationId":"forge-delegation-1","revoked":true}`))
	}))
	defer forge.Close()
	now := time.Now().UTC()
	revision := plantForgeTaskDelegation(t, pool, runID, forge.URL, "task-credential", now, now.Add(time.Hour))
	revoker := newTaskDelegationRevoker(pool)

	// A failed call stays open and backs off instead of being marked revoked.
	if _, err := revoker.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	row := readRevokedDelegation(t, pool, revision)
	if row.revoked || row.attempts != 1 || row.retryAt == nil || !row.retryAt.After(time.Now()) {
		t.Fatalf("failed revoke must back off and stay open: %+v", row)
	}
	if _, err := revoker.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("a backed-off delegation was retried early: %d calls", calls.Load())
	}

	// Once Forge answers, the delegation is closed for good.
	failing.Store(false)
	if _, err := pool.Exec(t.Context(), `UPDATE weave_task_business_delegations SET revoke_next_attempt_at=now()-interval '1 second'
		WHERE workspace_id='ws' AND input_revision_id=$1`, revision); err != nil {
		t.Fatal(err)
	}
	if _, err := revoker.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	row = readRevokedDelegation(t, pool, revision)
	if !row.revoked || row.reason != "run_terminal" {
		t.Fatalf("delegation after a successful revoke: %+v", row)
	}
	if _, err := revoker.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("a revoked delegation was revoked again: %d calls", calls.Load())
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_task_business_delegations SET revocation_reason='expired'
		WHERE workspace_id='ws' AND input_revision_id=$1`, revision); err == nil {
		t.Fatal("a revoked delegation's reason must be immutable")
	}
}

func TestTaskDelegationRevokerClosesExpiredDelegationsWithoutForgeRealPG(t *testing.T) {
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("11", 32))
	pool, runID := succeededRunForOutboxTest(t)
	var calls atomic.Int32
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1) }))
	defer forge.Close()
	now := time.Now().UTC()
	revision := plantForgeTaskDelegation(t, pool, runID, forge.URL, "task-credential", now.Add(-2*time.Hour), now.Add(-time.Hour))
	if _, err := newTaskDelegationRevoker(pool).Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	row := readRevokedDelegation(t, pool, revision)
	if !row.revoked || row.reason != "expired" || calls.Load() != 0 {
		t.Fatalf("expired delegation: %+v, Forge calls %d", row, calls.Load())
	}
}
