package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

// taskDelegationRevoker closes Forge task delegations once the run that
// consumed their input is terminal (decision 002). It is maintenance of the
// delegation ledger, not a second execution path: it reads run terminal state
// owned by the team-run store and only calls Forge's revoke endpoint with the
// delegation's own credential. Expired delegations are closed locally.
type taskDelegationRevoker struct {
	Pool         *pgxpool.Pool
	Client       *http.Client
	PollInterval time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type claimedTaskDelegation struct {
	workspaceID       string
	inputRevisionID   string
	baseURL           string
	forgeDelegationID string
	ciphertext        string
	digest            string
	attempts          int
}

// taskDelegationRevokeLease keeps a claimed row away from other sweeps while
// one revoke call is in flight.
const taskDelegationRevokeLease = 60 * time.Second

func newTaskDelegationRevoker(pool *pgxpool.Pool) *taskDelegationRevoker {
	return &taskDelegationRevoker{Pool: pool, Client: &http.Client{Timeout: 10 * time.Second}, PollInterval: 5 * time.Second}
}

func (revoker *taskDelegationRevoker) Start() {
	if revoker == nil || revoker.Pool == nil {
		return
	}
	revoker.mu.Lock()
	if revoker.cancel != nil {
		revoker.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	revoker.cancel = cancel
	revoker.wg.Add(1)
	revoker.mu.Unlock()
	go func() {
		defer revoker.wg.Done()
		for {
			processed, err := revoker.Sweep(ctx)
			if err != nil && ctx.Err() == nil {
				slog.Error("task delegation revocation failed", "error", err)
			}
			if processed == 0 {
				timer := time.NewTimer(revoker.PollInterval)
				select {
				case <-ctx.Done():
					if !timer.Stop() {
						<-timer.C
					}
					return
				case <-timer.C:
				}
			}
		}
	}()
}

func (revoker *taskDelegationRevoker) Stop() {
	if revoker == nil {
		return
	}
	revoker.mu.Lock()
	cancel := revoker.cancel
	revoker.cancel = nil
	revoker.mu.Unlock()
	if cancel != nil {
		cancel()
		revoker.wg.Wait()
	}
}

// Sweep closes expired delegations and revokes at most one delegation whose
// consuming run is terminal. It returns how many rows it changed.
func (revoker *taskDelegationRevoker) Sweep(ctx context.Context) (int, error) {
	expired, err := revoker.Pool.Exec(ctx, `UPDATE weave_task_business_delegations
		SET revoked_at=statement_timestamp(),revocation_reason='expired',revoke_next_attempt_at=NULL
		WHERE revoked_at IS NULL AND expires_at<=statement_timestamp()`)
	if err != nil {
		return 0, fmt.Errorf("close expired task delegations: %w", err)
	}
	claimed, err := revoker.claim(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return int(expired.RowsAffected()), nil
	}
	if err != nil {
		return int(expired.RowsAffected()), err
	}
	status, revokeErr := revoker.revoke(ctx, claimed)
	if err := revoker.finish(ctx, claimed, status, revokeErr); err != nil {
		return int(expired.RowsAffected()), err
	}
	return int(expired.RowsAffected()) + 1, nil
}

func (revoker *taskDelegationRevoker) claim(ctx context.Context) (claimedTaskDelegation, error) {
	var claimed claimedTaskDelegation
	err := revoker.Pool.QueryRow(ctx, `UPDATE weave_task_business_delegations AS d
		SET revoke_next_attempt_at=statement_timestamp()+($1::text||' seconds')::interval
		FROM (
			SELECT candidate.workspace_id,candidate.input_revision_id
			FROM weave_task_business_delegations AS candidate
			JOIN weave_dispatch_input_revisions AS input
			  ON input.workspace_id=candidate.workspace_id AND input.input_revision_id=candidate.input_revision_id
			JOIN weave_team_runs AS run
			  ON run.workspace_id=input.workspace_id AND run.run_id=input.consumed_run_id
			WHERE candidate.revoked_at IS NULL
			  AND run.status IN ('succeeded','failed','cancelled','abandoned')
			  AND (candidate.revoke_next_attempt_at IS NULL OR candidate.revoke_next_attempt_at<=statement_timestamp())
			ORDER BY candidate.revoke_next_attempt_at NULLS FIRST
			LIMIT 1
			FOR UPDATE OF candidate SKIP LOCKED
		) AS picked
		WHERE d.workspace_id=picked.workspace_id AND d.input_revision_id=picked.input_revision_id
		RETURNING d.workspace_id,d.input_revision_id,d.forge_base_url,d.forge_delegation_id,
			d.credential_ciphertext,d.credential_sha256,d.revoke_attempts`,
		strconv.Itoa(int(taskDelegationRevokeLease.Seconds()))).Scan(
		&claimed.workspaceID, &claimed.inputRevisionID, &claimed.baseURL, &claimed.forgeDelegationID,
		&claimed.ciphertext, &claimed.digest, &claimed.attempts)
	return claimed, err
}

func (revoker *taskDelegationRevoker) revoke(ctx context.Context, claimed claimedTaskDelegation) (int, error) {
	base, err := url.Parse(claimed.baseURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil {
		return 0, errors.New("Forge delegation address is invalid")
	}
	key, err := secret.KeyFromEnv()
	if err != nil {
		return 0, errors.New("delegation encryption key is unavailable")
	}
	token, err := secret.Open(key, claimed.ciphertext)
	clear(key)
	if err != nil {
		return 0, errors.New("Forge task credential cannot be opened")
	}
	defer clear(token)
	digest := sha256.Sum256(token)
	if hex.EncodeToString(digest[:]) != claimed.digest {
		return 0, errors.New("Forge task credential digest mismatch")
	}
	endpoint := *base
	endpoint.Path = "/api/v1/workbench/task-delegations/" + claimed.forgeDelegationID
	endpoint.RawPath = "/api/v1/workbench/task-delegations/" + url.PathEscape(claimed.forgeDelegationID)
	endpoint.RawQuery, endpoint.Fragment = "", ""
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint.String(), bytes.NewReader([]byte(`{"reason":"run_terminal"}`)))
	if err != nil {
		return 0, err
	}
	authorization := append([]byte("Bearer "), token...)
	request.Header.Set("Authorization", string(authorization))
	clear(authorization)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := revoker.Client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("Forge revoke request failed: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	switch response.StatusCode {
	case http.StatusOK:
		return response.StatusCode, nil
	case http.StatusUnauthorized, http.StatusNotFound:
		// The credential no longer authenticates or Forge has no such
		// delegation: there is nothing left that this row could use.
		return response.StatusCode, nil
	default:
		return response.StatusCode, fmt.Errorf("Forge revoke returned status %d", response.StatusCode)
	}
}

func (revoker *taskDelegationRevoker) finish(ctx context.Context, claimed claimedTaskDelegation, status int, revokeErr error) error {
	if revokeErr == nil {
		_, err := revoker.Pool.Exec(ctx, `UPDATE weave_task_business_delegations
			SET revoked_at=statement_timestamp(),revocation_reason='run_terminal',revoke_next_attempt_at=NULL,revoke_last_error=NULL
			WHERE workspace_id=$1 AND input_revision_id=$2 AND revoked_at IS NULL`,
			claimed.workspaceID, claimed.inputRevisionID)
		return err
	}
	message := revokeErr.Error()
	if len(message) > 500 {
		message = message[:500]
	}
	slog.Warn("task delegation revoke will be retried", "workspace_id", claimed.workspaceID,
		"input_revision_id", claimed.inputRevisionID, "status", status, "error", message)
	delaySeconds := 5 << min(claimed.attempts, 8)
	_, err := revoker.Pool.Exec(ctx, `UPDATE weave_task_business_delegations
		SET revoke_attempts=revoke_attempts+1,revoke_last_error=$3,
		    revoke_next_attempt_at=statement_timestamp()+($4::text||' seconds')::interval
		WHERE workspace_id=$1 AND input_revision_id=$2 AND revoked_at IS NULL`,
		claimed.workspaceID, claimed.inputRevisionID, message, strconv.Itoa(delaySeconds))
	return err
}
