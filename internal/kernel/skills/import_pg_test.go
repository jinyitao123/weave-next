package skills

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

type importFixtureReader struct{}

func (importFixtureReader) Get(context.Context, string, string) ([]byte, error) {
	return []byte(`{"id":"skill","name":"Report","body":"Summarize the supplied input."}`), nil
}

// Only Kernel-owned tables exist. Import must not query a product workspace
// as a prerequisite to serializing its own receipt and immutable version.
func importFixture(t *testing.T) (*pgxpool.Pool, *Importer) {
	t.Helper()
	seed := testutil.PostgresPool(t)
	_, err := seed.Exec(t.Context(), `
 CREATE TABLE weave_skills(workspace_id text, id text, name text, latest_version bigint, updated_at timestamptz, PRIMARY KEY(workspace_id,id));
 CREATE TABLE weave_skill_versions(workspace_id text, skill_id text, version bigint, name text, description text, body text, always_active bool, resources jsonb, content_hash text, created_at timestamptz DEFAULT now(), PRIMARY KEY(workspace_id,skill_id,version));
 CREATE TABLE weave_skill_import_receipts(workspace_id text, idempotency_key text, request_hash text, response jsonb, PRIMARY KEY(workspace_id,idempotency_key));
 CREATE TABLE weave_skill_import_audits(workspace_id text, audit_id text, idempotency_key text, skill_id text, operator_id text, reason text, request_hash text, source_hash text, version bigint, changed bool, PRIMARY KEY(workspace_id,audit_id));
 `)
	if err != nil {
		t.Fatal(err)
	}
	cfg := seed.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, NewImporter(pool, New(pool), importFixtureReader{})
}

func TestImportConcurrentReplayCreatesOneVersionReceiptAndAudit(t *testing.T) {
	pool, importer := importFixture(t)
	request := ImportRequest{SchemaVersion: 1, IdempotencyKey: "once", Reason: "Import report skill"}
	const callers = 12
	results := make(chan *ImportResponse, callers)
	failures := make(chan error, callers)
	var workers sync.WaitGroup
	for range callers {
		workers.Go(func() {
			result, err := importer.Import(t.Context(), "workspace", "skill", "user", request)
			results <- result
			failures <- err
		})
	}
	workers.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	for response := range results {
		if response == nil || response.Version != 1 || !response.Changed {
			t.Fatalf("concurrent replay changed response: %+v", response)
		}
	}
	var versions, receipts, audits int
	if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM weave_skill_versions),(SELECT count(*) FROM weave_skill_import_receipts),(SELECT count(*) FROM weave_skill_import_audits)`).Scan(&versions, &receipts, &audits); err != nil || versions != 1 || receipts != 1 || audits != 1 {
		t.Fatalf("duplicate import effects: %d/%d/%d %v", versions, receipts, audits, err)
	}
	request.IdempotencyKey = "unchanged"
	response, err := importer.Import(t.Context(), "workspace", "skill", "user", request)
	if err != nil || response.Version != 1 || response.Changed {
		t.Fatalf("unchanged source created a new version: %+v %v", response, err)
	}
	request.Reason = "Different request"
	if _, err := importer.Import(t.Context(), "workspace", "skill", "user", request); !errors.Is(err, ErrSkillImportIdempotencyConflict) {
		t.Fatalf("idempotency conflict accepted: %v", err)
	}
}

func TestImportNamespaceLockIsScopedAndReleasedOnCancellation(t *testing.T) {
	pool, importer := importFixture(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := lockImportNamespace(t.Context(), tx, "blocked"); err != nil {
		t.Fatal(err)
	}
	request := ImportRequest{SchemaVersion: 1, IdempotencyKey: "once", Reason: "Import report skill"}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	blocked := make(chan error, 1)
	go func() {
		_, err := importer.Import(ctx, "blocked", "skill", "user", request)
		blocked <- err
	}()
	independent, stopIndependent := context.WithTimeout(t.Context(), 2*time.Second)
	defer stopIndependent()
	if _, err := importer.Import(independent, "independent", "skill", "user", request); err != nil {
		t.Fatalf("unrelated namespace was blocked: %v", err)
	}
	if err := <-blocked; !errors.Is(err, ErrSkillImportFailed) || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("locked import ignored cancellation: %v (context: %v)", err, ctx.Err())
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := importer.Import(t.Context(), "blocked", "skill", "user", request); err != nil {
		t.Fatalf("cancelled import retained lock or partial write: %v", err)
	}
}
