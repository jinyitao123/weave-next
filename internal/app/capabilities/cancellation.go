package capabilities

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/teamrun"
)

const capabilityCancelGrace = 30 * time.Second

func (service *InvocationService) CancelInvocation(
	ctx context.Context,
	request CancelInvocationRequest,
) (CancellationReceipt, error) {
	if service == nil || service.Store == nil || service.Store.pool == nil ||
		service.Tasks == nil || service.Cancel == nil {
		return CancellationReceipt{}, fmt.Errorf("%w: cancellation dependencies are incomplete", ErrDisabled)
	}
	if invalidIdentity(request.Principal.WorkspaceID) || invalidIdentity(request.Principal.AppID) ||
		invalidIdentity(request.Principal.CredentialID) ||
		(request.RequestID == "" && request.InvocationID == "") ||
		request.RequestID != "" && !requestIdentityPattern.MatchString(request.RequestID) ||
		request.InvocationID != "" && invalidIdentity(request.InvocationID) ||
		request.Reason == "" || request.Reason != strings.TrimSpace(request.Reason) ||
		len(request.Reason) > 256 {
		return CancellationReceipt{}, ErrInvalid
	}
	if !request.Principal.HasScope("cancel") {
		return CancellationReceipt{}, ErrScopeDenied
	}

	// Invocation IDs are immutable, so resolving request_id before taking its
	// advisory lock cannot drift. The scoped predicate avoids cross-app probes.
	if request.RequestID == "" {
		var requestID string
		err := service.Store.pool.QueryRow(ctx, `
			SELECT request_id FROM weave_capability_invocations
			WHERE workspace_id=$1 AND app_id=$2 AND invocation_id=$3
		`, request.Principal.WorkspaceID, request.Principal.AppID,
			request.InvocationID).Scan(&requestID)
		if errors.Is(err, pgx.ErrNoRows) {
			return CancellationReceipt{}, ErrNotFound
		}
		if err != nil {
			return CancellationReceipt{}, fmt.Errorf("resolve capability request for cancellation: %w", err)
		}
		request.RequestID = requestID
	}

	tx, err := service.Store.pool.Begin(ctx)
	if err != nil {
		return CancellationReceipt{}, fmt.Errorf("begin capability cancellation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockRequestIdentity(ctx, tx, request.Principal, request.RequestID); err != nil {
		return CancellationReceipt{}, err
	}
	if _, err := service.lockActivePrincipalTx(ctx, tx, request.Principal, "cancel"); err != nil {
		return CancellationReceipt{}, err
	}
	invocation, present, err := loadInvocationByRequestTx(
		ctx, tx, request.Principal.WorkspaceID, request.Principal.AppID, request.RequestID,
	)
	if err != nil {
		return CancellationReceipt{}, err
	}
	if request.InvocationID != "" && (!present || invocation.InvocationID != request.InvocationID) {
		return CancellationReceipt{}, ErrNotFound
	}
	taskStatus := ""
	var runStatus *string
	if present {
		if err := tx.QueryRow(ctx, `
			SELECT task.status,run.status
			FROM weave_task_queue AS task
			LEFT JOIN weave_team_runs AS run
			  ON run.workspace_id=task.workspace_id AND run.run_id=$3
			WHERE task.workspace_id=$1 AND task.id=$2
		`, invocation.WorkspaceID, invocation.TaskID, invocation.RunID).Scan(
			&taskStatus, &runStatus,
		); err != nil {
			return CancellationReceipt{}, fmt.Errorf("read capability execution for cancellation: %w", err)
		}
	}
	var existingReason string
	var existingRequestedAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT reason,requested_at
		FROM weave_capability_invocation_cancellations
		WHERE workspace_id=$1 AND app_id=$2 AND request_id=$3
	`, request.Principal.WorkspaceID, request.Principal.AppID, request.RequestID).Scan(
		&existingReason, &existingRequestedAt,
	)
	alreadyExists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return CancellationReceipt{}, fmt.Errorf("read capability cancellation: %w", err)
	}
	now := service.Store.clock.Now().UTC().Truncate(time.Microsecond)
	effectiveReason, effectiveRequestedAt := request.Reason, now
	if alreadyExists {
		effectiveReason, effectiveRequestedAt = existingReason, existingRequestedAt
	}
	if !alreadyExists {
		var invocationID *string
		if present {
			invocationID = &invocation.InvocationID
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO weave_capability_invocation_cancellations(
				workspace_id,app_id,request_id,invocation_id,requested_credential_id,
				reason,requested_at
			) VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT DO NOTHING
		`, request.Principal.WorkspaceID, request.Principal.AppID, request.RequestID,
			invocationID, request.Principal.CredentialID, request.Reason, now)
		if err != nil {
			return CancellationReceipt{}, fmt.Errorf("insert capability cancellation: %w", err)
		}
		if tag.RowsAffected() == 0 {
			alreadyExists = true
			if err := tx.QueryRow(ctx, `
				SELECT reason,requested_at
				FROM weave_capability_invocation_cancellations
				WHERE workspace_id=$1 AND app_id=$2 AND request_id=$3
			`, request.Principal.WorkspaceID, request.Principal.AppID, request.RequestID).Scan(
				&effectiveReason, &effectiveRequestedAt,
			); err != nil {
				return CancellationReceipt{}, fmt.Errorf("read concurrent capability cancellation: %w", err)
			}
		}
	}
	if present {
		if _, err := service.Tasks.CancelRunTasksTx(
			ctx, tx, invocation.WorkspaceID, invocation.RunID,
		); err != nil {
			return CancellationReceipt{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return CancellationReceipt{}, fmt.Errorf("commit capability cancellation: %w", err)
	}
	if present {
		// The durable cancellation row and queue fence have already committed.
		// Reconciliation repeats this call after a crash or transient failure.
		_ = service.cancelTeamRun(ctx, invocation, effectiveReason, effectiveRequestedAt)
	}
	status := "cancelled_before_submit"
	if present {
		status = invocationCancellationStatus(true, taskStatus, runStatus)
		if status != "confirmed" && status != "not_applied" {
			status = "requested"
		}
	}
	return CancellationReceipt{
		RequestID: request.RequestID, InvocationID: invocation.InvocationID,
		Status: status, AlreadyExists: alreadyExists,
	}, nil
}

func (service *InvocationService) cancelTeamRun(
	ctx context.Context, invocation Invocation, reason string, now time.Time,
) error {
	_, err := service.Cancel.RequestCancel(ctx, teamrun.CancelRequest{
		WorkspaceID: invocation.WorkspaceID, RunID: invocation.RunID,
		CancelActor: "service-app:" + invocation.AppID, CancelReason: reason,
		GraceDeadline:  now.Add(capabilityCancelGrace),
		IdempotencyKey: "capability-cancel:" + invocation.AppID + ":" + invocation.RequestID,
	})
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, teamrun.ErrTeamRunIdentityMismatch) {
		return nil
	}
	return err
}

// SweepControls persists deadline stop intents and replays every pending
// cancellation into the existing queue and TeamRun cancellation mechanisms.
// It is safe to run concurrently in multiple server processes.
func (service *InvocationService) SweepControls(ctx context.Context, limit int) (int, error) {
	if service == nil || service.Store == nil || service.Store.pool == nil ||
		service.Tasks == nil || service.Cancel == nil {
		return 0, ErrDisabled
	}
	if limit < 1 {
		limit = 32
	}
	now := service.Store.clock.Now().UTC().Truncate(time.Microsecond)
	tx, err := service.Store.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin capability control sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		WITH expired AS (
			SELECT invocation.workspace_id,invocation.app_id,invocation.request_id,
			       invocation.invocation_id,invocation.credential_id
			FROM weave_capability_invocations AS invocation
			JOIN weave_task_queue AS task
			  ON task.workspace_id=invocation.workspace_id AND task.id=invocation.task_id
			LEFT JOIN weave_team_runs AS run
			  ON run.workspace_id=invocation.workspace_id AND run.run_id=invocation.run_id
			JOIN weave_capability_releases AS release
			  ON release.workspace_id=invocation.workspace_id AND release.id=invocation.release_id
			LEFT JOIN weave_capability_invocation_cancellations AS cancellation
			  ON cancellation.workspace_id=invocation.workspace_id
			 AND cancellation.app_id=invocation.app_id
			 AND cancellation.request_id=invocation.request_id
			WHERE cancellation.request_id IS NULL
			  AND CASE WHEN run.run_id IS NULL
			    THEN task.status NOT IN ('completed','failed','cancelled','superseded','cut','timed_out')
			    ELSE run.status NOT IN ('succeeded','failed','cancelled','abandoned')
			  END
			  AND CASE WHEN run.run_id IS NULL OR run.status='queued'
			    THEN invocation.accepted_at + make_interval(
			      secs => (release.execution_limits->>'queue_timeout_seconds')::int
			    ) <= $1
			    ELSE LEAST(
			      invocation.deadline_at,
			      COALESCE(task.started_at,invocation.accepted_at) + make_interval(
			        secs => (release.execution_limits->>'execution_timeout_seconds')::int
			      )
			    ) <= $1
			  END
			ORDER BY invocation.deadline_at,invocation.workspace_id,invocation.invocation_id
			FOR UPDATE OF invocation SKIP LOCKED
			LIMIT $2
		)
		INSERT INTO weave_capability_invocation_cancellations(
			workspace_id,app_id,request_id,invocation_id,requested_credential_id,
			reason,requested_at
		)
		SELECT workspace_id,app_id,request_id,invocation_id,credential_id,
		       'deadline_exceeded',$1
		FROM expired
		ON CONFLICT DO NOTHING
	`, now, limit); err != nil {
		return 0, fmt.Errorf("persist capability deadline cancellations: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT `+qualifiedInvocationColumns("invocation")+`,cancellation.reason,cancellation.requested_at
		FROM weave_capability_invocation_cancellations AS cancellation
		JOIN weave_capability_invocations AS invocation
		  ON invocation.workspace_id=cancellation.workspace_id
		 AND invocation.app_id=cancellation.app_id
		 AND invocation.request_id=cancellation.request_id
		JOIN weave_task_queue AS task
		  ON task.workspace_id=invocation.workspace_id AND task.id=invocation.task_id
		LEFT JOIN weave_team_runs AS run
		  ON run.workspace_id=invocation.workspace_id AND run.run_id=invocation.run_id
		WHERE CASE WHEN run.run_id IS NULL
		  THEN task.status NOT IN ('completed','failed','cancelled','superseded','cut','timed_out')
		  ELSE run.status NOT IN ('succeeded','failed','cancelled','abandoned')
		END
		ORDER BY cancellation.requested_at,invocation.workspace_id,invocation.invocation_id
		FOR UPDATE OF invocation SKIP LOCKED
		LIMIT $1
	`, limit)
	if err != nil {
		return 0, fmt.Errorf("list pending capability cancellations: %w", err)
	}
	type pendingControl struct {
		invocation  Invocation
		reason      string
		requestedAt time.Time
	}
	pending := make([]pendingControl, 0, limit)
	for rows.Next() {
		var item pendingControl
		if err := rows.Scan(
			&item.invocation.WorkspaceID, &item.invocation.AppID, &item.invocation.InvocationID,
			&item.invocation.RequestID, &item.invocation.CapabilityID, &item.invocation.ReleaseVersion,
			&item.invocation.ReleaseID, &item.invocation.GrantID, &item.invocation.CredentialID,
			&item.invocation.ArtifactContentHash, &item.invocation.Normalization,
			&item.invocation.InputHash, &item.invocation.RequestFingerprint,
			&item.invocation.ConcurrencyKey, &item.invocation.RunID, &item.invocation.TaskID,
			&item.invocation.AcceptedAt, &item.invocation.DeadlineAt,
			&item.reason, &item.requestedAt,
		); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan pending capability cancellation: %w", err)
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("read pending capability cancellation rows: %w", err)
	}
	rows.Close()
	for _, item := range pending {
		if _, err := service.Tasks.CancelRunTasksTx(
			ctx, tx, item.invocation.WorkspaceID, item.invocation.RunID,
		); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit capability control sweep: %w", err)
	}
	for _, item := range pending {
		if err := service.cancelTeamRun(ctx, item.invocation, item.reason, item.requestedAt); err != nil {
			return len(pending), fmt.Errorf("request capability TeamRun cancellation: %w", err)
		}
	}
	return len(pending), nil
}
