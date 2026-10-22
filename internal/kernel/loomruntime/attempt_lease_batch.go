package loomruntime

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type attemptBatchKey struct{}
type attemptRenewRequest struct {
	ctx    context.Context
	owner  AttemptLeaseOwner
	ttl    time.Duration
	result chan attemptRenewResult
}
type attemptRenewResult struct {
	renewal AttemptLeaseRenewal
	err     error
}

// AttemptLeaseBatcher coalesces physical renewals, never admission, closure or
// terminal ownership. Each reply proves a fresh committed renewal of exactly
// the requested generation, attempt, graph and original run timestamp.
type AttemptLeaseBatcher struct {
	store      expectedRunAdmissionTxStore
	queue      chan attemptRenewRequest
	stop, done chan struct{}
	once       sync.Once
}

func NewAttemptLeaseBatcher(store TerminalRecordStore) (*AttemptLeaseBatcher, error) {
	transactions, ok := store.(expectedRunAdmissionTxStore)
	if !ok {
		return nil, ErrA4AttemptLeaseHeartbeatUnsupported
	}
	b := &AttemptLeaseBatcher{store: transactions, queue: make(chan attemptRenewRequest, 4096), stop: make(chan struct{}), done: make(chan struct{})}
	go b.run()
	return b, nil
}

func WithAttemptLeaseBatcher(ctx context.Context, b *AttemptLeaseBatcher) context.Context {
	return context.WithValue(ctx, attemptBatchKey{}, b)
}
func (b *AttemptLeaseBatcher) Close() { b.once.Do(func() { close(b.stop); <-b.done }) }

// renew keeps the caller's lease duration: batching changes how renewals are
// sent, never how long a renewed lease lasts.
func (b *AttemptLeaseBatcher) renew(ctx context.Context, owner AttemptLeaseOwner, ttl time.Duration) (AttemptLeaseRenewal, error) {
	if ttl <= 0 {
		return AttemptLeaseRenewal{}, &AttemptLeaseHeartbeatError{Stage: AttemptLeaseHeartbeatRenew, WorkspaceID: owner.WorkspaceID, RunID: owner.RunID, Generation: owner.Generation, AttemptID: owner.AttemptID, Err: fmt.Errorf("attempt lease ttl must be positive")}
	}
	request := attemptRenewRequest{ctx: ctx, owner: owner, ttl: ttl, result: make(chan attemptRenewResult, 1)}
	select {
	case b.queue <- request:
	case <-ctx.Done():
		return AttemptLeaseRenewal{}, ctx.Err()
	case <-b.stop:
		return AttemptLeaseRenewal{}, context.Canceled
	}
	select {
	case result := <-request.result:
		return result.renewal, result.err
	case <-ctx.Done():
		return AttemptLeaseRenewal{}, ctx.Err()
	case <-b.stop:
		return AttemptLeaseRenewal{}, context.Canceled
	}
}

func (b *AttemptLeaseBatcher) run() {
	defer close(b.done)
	for {
		var first attemptRenewRequest
		select {
		case first = <-b.queue:
		case <-b.stop:
			return
		}
		batch := []attemptRenewRequest{first}
		timer := time.NewTimer(20 * time.Millisecond)
	collect:
		for len(batch) < 256 {
			select {
			case request := <-b.queue:
				batch = append(batch, request)
			case <-timer.C:
				break collect
			case <-b.stop:
				timer.Stop()
				return
			}
		}
		timer.Stop()
		b.flush(batch)
	}
}

func (b *AttemptLeaseBatcher) flush(batch []attemptRenewRequest) {
	var workspaces, runs, attempts, graphs, started, ttls []string
	var generations []int64
	for _, request := range batch {
		o := request.owner
		workspaces = append(workspaces, o.WorkspaceID)
		runs = append(runs, o.RunID)
		attempts = append(attempts, o.AttemptID.String())
		graphs = append(graphs, o.GraphName)
		started = append(started, o.RunStartedAt)
		generations = append(generations, o.Generation)
		ttls = append(ttls, request.ttl.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	renewals := map[string]AttemptLeaseRenewal{}
	err := func() error {
		tx, err := b.store.BeginTx(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		rows, err := tx.Query(ctx, `WITH owners AS MATERIALIZED (
		 SELECT * FROM unnest($1::text[],$2::text[],$3::bigint[],$4::text[],$5::text[],$6::text[],$7::text[])
		 AS owner(workspace,run,generation,attempt,graph,started,ttl)), db_clock AS MATERIALIZED (SELECT statement_timestamp() AS now)
		 UPDATE weave_run_attempt_leases lease SET heartbeat_at=db_clock.now,lease_expires_at=db_clock.now+owners.ttl::interval
		 FROM owners,db_clock WHERE lease.workspace_id=owners.workspace AND lease.run_id=owners.run
		 AND lease.attempt_generation=owners.generation AND lease.attempt_id=owners.attempt::uuid
		 AND lease.graph_name=owners.graph AND lease.run_started_at=owners.started
		 AND lease.state='active' AND lease.lease_expires_at>db_clock.now
		 RETURNING lease.workspace_id,lease.run_id,lease.attempt_generation,lease.attempt_id::text,lease.graph_name,lease.run_started_at,lease.heartbeat_at,lease.lease_expires_at`, workspaces, runs, generations, attempts, graphs, started, ttls)
		if err != nil {
			return err
		}
		for rows.Next() {
			var workspace, run, attempt, graph, started string
			var generation int64
			var renewal AttemptLeaseRenewal
			if err := rows.Scan(&workspace, &run, &generation, &attempt, &graph, &started, &renewal.HeartbeatAt, &renewal.LeaseExpiresAt); err != nil {
				rows.Close()
				return err
			}
			renewals[attemptRenewKey(workspace, run, generation, attempt, graph, started)] = renewal
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}()
	for _, request := range batch {
		o := request.owner
		renewal, ok := renewals[attemptRenewKey(o.WorkspaceID, o.RunID, o.Generation, o.AttemptID.String(), o.GraphName, o.RunStartedAt)]
		failure := err
		stage := AttemptLeaseHeartbeatRenew
		if failure == nil && !ok {
			failure = ErrAttemptLeaseHeartbeatOwnerLost
			stage = AttemptLeaseHeartbeatOwnerLost
		}
		if failure != nil {
			failure = &AttemptLeaseHeartbeatError{Stage: stage, WorkspaceID: o.WorkspaceID, RunID: o.RunID, Generation: o.Generation, AttemptID: o.AttemptID, Err: failure}
		}
		request.result <- attemptRenewResult{renewal: renewal, err: failure}
	}
}

func attemptRenewKey(workspace, run string, generation int64, attempt, graph, started string) string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s\x00%s", workspace, run, generation, attempt, graph, started)
}
