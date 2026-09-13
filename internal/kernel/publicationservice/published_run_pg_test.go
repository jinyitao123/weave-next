package publicationservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

type publishedProofAuthority struct {
	Authority
	check func(context.Context) error
}

func (a publishedProofAuthority) AuthorizePublished(ctx context.Context, _ publication.PublishedRunRequest, _ frozen.ArtifactEnvelopeV1) error {
	if a.check != nil {
		return a.check(ctx)
	}
	return nil
}

func publishedPGFixture(t *testing.T) (context.Context, *Service, *pgxpool.Pool, publication.PublishedRunRequest) {
	t.Helper()
	ctx, service, pool, revision := publicationPGFixture(t)
	if _, err := service.Publish(ctx, revision); err != nil {
		t.Fatal(err)
	}
	service.authority = publishedProofAuthority{Authority: service.authority}
	cfg := service.pool.Config()
	cfg.MaxConns = 1
	single, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	service.pool.Close()
	service.pool = single
	return ctx, service, pool, publication.PublishedRunRequest{Version: publication.ContractVersion, RequestID: "published-request", Revision: publication.CandidateRevision(revision.Candidate), RunID: "published-run", TaskID: "published-task", Input: json.RawMessage(`"original input"`), InputVersion: "input-1", Trigger: publication.PublishedTrigger{Type: "conversation_explicit", SourceRef: "conversation"}}
}

func TestPublishedAdmissionSingleConnectionAtomicAndReplayRealPG(t *testing.T) {
	ctx, service, pool, request := publishedPGFixture(t)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// A receipt write failure must roll back every physical execution fact.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_published_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.operation='published_run' THEN RAISE EXCEPTION 'lost receipt'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_published_receipt BEFORE INSERT ON weave_kernel_publication_requests FOR EACH ROW EXECUTE FUNCTION reject_published_receipt();`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AdmitPublished(ctx, request); err == nil {
		t.Fatal("receipt failure admitted execution")
	}
	for _, table := range []string{"weave_task_queue", "weave_team_run_snapshots", "weave_run_delivery_state"} {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s partially committed: %d", table, n)
		}
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_published_receipt ON weave_kernel_publication_requests`); err != nil {
		t.Fatal(err)
	}
	first, err := service.AdmitPublished(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate loss of the response after commit: the caller knows only the same
	// immutable request, and can reconstruct exactly the original receipt.
	second, err := service.AdmitPublished(ctx, request)
	if err != nil || first != second {
		t.Fatalf("unknown commit changed receipt: %+v %v", second, err)
	}
	closeReceipt, err := service.ClosePublished(ctx, request)
	if err != nil || closeReceipt == nil || *closeReceipt != first {
		t.Fatalf("accepted request closed without receipt: %v", err)
	}
	other := execution.WithSubject(ctx, execution.Subject{WorkspaceID: "workspace", UserID: "bob"})
	if _, err := service.ClosePublished(other, request); !errors.Is(err, execution.ErrSubjectMismatch) {
		t.Fatalf("foreign reconciliation: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("duplicate tasks %d %v", n, err)
	}
	service.authority = publishedProofAuthority{Authority: service.authority, check: func(context.Context) error { return errors.New("revoked") }}
	if _, err := service.AdmitPublished(ctx, request); err == nil {
		t.Fatal("receipt bypassed current authorization")
	}
}

func TestPublishedCloseBeforeAdmitAndConcurrentOrderingRealPG(t *testing.T) {
	ctx, service, pool, template := publishedPGFixture(t)
	closed := template
	closed.RequestID = "closed"
	closed.RunID = "closed-run"
	closed.TaskID = "closed-task"
	if receipt, err := service.ClosePublished(ctx, closed); err != nil || receipt != nil {
		t.Fatalf("close empty request: %+v %v", receipt, err)
	}
	if _, err := service.AdmitPublished(ctx, closed); !errors.Is(err, publication.ErrAdmissionClosed) {
		t.Fatalf("delayed admit crossed close: %v", err)
	}
	// Use separate service instances to exercise database lock ordering rather
	// than a process mutex or a single connection's local serialization.
	cfg := service.pool.Config()
	cfg.MaxConns = 4
	concurrent, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer concurrent.Close()
	other := &Service{pool: concurrent, authority: service.authority}
	for i := range 12 {
		request := template
		request.RequestID = fmt.Sprintf("race-%d", i)
		request.RunID = request.RequestID + "-run"
		request.TaskID = request.RequestID + "-task"
		var admitted publication.AdmissionReceipt
		var admitErr error
		var receipt *publication.AdmissionReceipt
		var closeErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); admitted, admitErr = service.AdmitPublished(ctx, request) }()
		go func() { defer wg.Done(); receipt, closeErr = other.ClosePublished(ctx, request) }()
		wg.Wait()
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if receipt == nil {
			if !errors.Is(admitErr, publication.ErrAdmissionClosed) {
				t.Fatalf("closure winner returned %v", admitErr)
			}
		} else if admitErr != nil || admitted != *receipt {
			t.Fatalf("admission winner lost receipt: %v", admitErr)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue WHERE id=$1`, request.TaskID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if (receipt == nil && count != 0) || (receipt != nil && count != 1) {
			t.Fatalf("closure/task disagreement: %+v %d", receipt, count)
		}
	}
}

func TestPublishedVersionBlockSerializesBeforeAdmissionRealPG(t *testing.T) {
	ctx, service, pool, request := publishedPGFixture(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE weave_workflow_version_admission_statuses SET blocked=true WHERE workspace_id='workspace'`); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	service.authority = publishedProofAuthority{Authority: service.authority, check: func(context.Context) error { close(started); return nil }}
	go func() { _, err := service.AdmitPublished(ctx, request); done <- err }()
	<-started
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	var denial *workflow.FixedWorkflowAdmissionDenial
	if !errors.As(err, &denial) || denial.ReasonCode != workflow.FixedWorkflowAdmissionVersionBlocked {
		t.Fatalf("committed block lost race: %v", err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("blocked request queued %d %v", count, err)
	}
}
