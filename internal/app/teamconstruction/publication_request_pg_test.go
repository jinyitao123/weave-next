package teamconstruction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/publication"
)

func publicationRequestPGFixture(t *testing.T) (context.Context, *pgPublicationRequests, PublicationCommand) {
	t.Helper()
	ctx, command := publicationCommandFixture(t)
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// An independent product write makes transaction boundaries observable;
	// production asset/budget policy is wired by ProductionPhases in its batch.
	if _, err := pool.Exec(ctx, `CREATE TABLE publication_test_effects(id TEXT PRIMARY KEY,kind TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	store := &pgPublicationRequests{pool: pool}
	store.activate = func(ctx context.Context, tx pgx.Tx, r PublicationRequestRecord) error {
		_, err := tx.Exec(ctx, `INSERT INTO publication_test_effects VALUES($1,'activation')`, r.Command.Request.RequestID)
		return err
	}
	store.associateUsage = func(ctx context.Context, tx pgx.Tx, r CandidateRequestRecord) error {
		_, err := tx.Exec(ctx, `INSERT INTO publication_test_effects VALUES($1,'usage')`, r.Request.RequestID)
		return err
	}
	return ctx, store, command
}

func TestPublicationRequestStoreConcurrentActivationAndActorRealPG(t *testing.T) {
	ctx, store, command := publicationRequestPGFixture(t)
	kernel := &publicationBoundaryDouble{}
	flow := PublicationFlow{Requests: store, Kernel: kernel}
	var wg sync.WaitGroup
	failures := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := flow.Publish(ctx, command); failures <- err }()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var state string
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT state FROM weave_team_publication_requests`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM publication_test_effects`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if state != "activated" || count != 1 || len(kernel.published) != 1 {
		t.Fatal("publication repeated product effect or frozen revision")
	}
	other := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "bob"})
	if _, err := flow.Publish(other, command); !errors.Is(err, execution.ErrSubjectMismatch) {
		t.Fatalf("foreign request reused: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE weave_team_publication_requests SET state='pending',receipt=NULL`); err == nil {
		t.Fatal("persisted phase regressed")
	}
	if _, err := store.pool.Exec(ctx, `UPDATE weave_team_publication_requests SET actor_subject='{"workspace_id":"workspace","user_id":"bob"}'`); err == nil {
		t.Fatal("persisted actor changed")
	}
}

func TestPublicationRequestStoreActivationAndAssociationRollbackRealPG(t *testing.T) {
	ctx, store, command := publicationRequestPGFixture(t)
	kernel := &publicationBoundaryDouble{}
	activation := store.activate
	store.activate = func(ctx context.Context, tx pgx.Tx, r PublicationRequestRecord) error {
		if err := activation(ctx, tx, r); err != nil {
			return err
		}
		return errors.New("activation interrupted after product write")
	}
	flow := PublicationFlow{Requests: store, Kernel: kernel}
	if _, err := flow.Publish(ctx, command); err == nil {
		t.Fatal("expected interrupted activation")
	}
	var state string
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT state FROM weave_team_publication_requests`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM publication_test_effects`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if state != "revision_obtained" || count != 0 {
		t.Fatal("product activation partially committed")
	}
	store.activate = activation
	if _, err := flow.Publish(ctx, command); err != nil {
		t.Fatal(err)
	}
	if kernel.publishCalls != 2 || len(kernel.published) != 1 {
		t.Fatal("activation retry must reauthorize the same revision")
	}

	deadline := time.Now().UTC().Add(time.Hour)
	request := publication.CandidateRunRequest{Version: publication.ContractVersion, RequestID: "candidate-1", Candidate: command.Request.Candidate,
		Input: json.RawMessage(`{"input":"test"}`), InputVersion: "input-1", SourceRef: "build", Purpose: "trial", DeadlineAt: &deadline}
	target := CandidateTarget{BuildRunID: "build", RoundNo: 1, SourceRole: "trial"}
	association := store.associateUsage
	store.associateUsage = func(ctx context.Context, tx pgx.Tx, r CandidateRequestRecord) error {
		if err := association(ctx, tx, r); err != nil {
			return err
		}
		return errors.New("usage association interrupted")
	}
	admission := CandidateAdmissionFlow{Requests: store, Kernel: kernel}
	if _, err := admission.Admit(ctx, target, request); err == nil {
		t.Fatal("expected interrupted admission association")
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM publication_test_effects WHERE kind='usage'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("usage source committed without durable task association")
	}
	store.associateUsage = association
	record, err := admission.Admit(ctx, target, request)
	if err != nil {
		t.Fatal(err)
	}
	if record.Receipt == nil || len(kernel.admitted) != 1 {
		t.Fatal("admission retry lost task receipt")
	}
	target.RoundNo = 2
	if _, err = admission.Admit(ctx, target, request); !errors.Is(err, publication.ErrRequestConflict) {
		t.Fatalf("same task moved to another build round: %v", err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE weave_team_candidate_requests SET receipt=NULL`); err == nil {
		t.Fatal("persisted admission receipt was erased")
	}
}

func TestPublicationRequestDatabaseRejectsMissingActorAndInvalidDigestRealPG(t *testing.T) {
	ctx, store, _ := publicationRequestPGFixture(t)
	for _, table := range []string{"weave_team_publication_requests", "weave_team_candidate_requests"} {
		for index, actor := range []string{`{}`, `{"user_id":"alice"}`, `{"workspace_id":"workspace","user_id":"alice","service_id":"s"}`, `{"workspace_id":"other","user_id":"alice"}`} {
			extraCols, extraVals := "command,state", `'{}','pending'`
			if table == "weave_team_candidate_requests" {
				extraCols, extraVals = "target,request", `'{}','{}'`
			}
			_, err := store.pool.Exec(ctx, "INSERT INTO "+table+"(workspace_id,request_id,actor_subject,request_digest,"+extraCols+") VALUES('workspace',$1,$2::jsonb,repeat('a',64),"+extraVals+")", fmt.Sprint(index), actor)
			if err == nil {
				t.Fatalf("%s accepted malformed actor %s", table, actor)
			}
		}
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO weave_team_publication_requests(workspace_id,request_id,actor_subject,request_digest,command,state) VALUES('workspace','bad-digest','{"workspace_id":"workspace","user_id":"alice"}','invalid','{}','pending')`); err == nil {
		t.Fatal("invalid digest accepted")
	}
}
