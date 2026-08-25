package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/jinyitao123/weave/internal/kernel/sessionexec"
)

const sessionExecutionWorkersDisableEnv = "WEAVE_SESSION_EXECUTION_WORKERS_DISABLED"

// Deployment policy pending (OQ-5). These values are process-local worker
// policy and deliberately remain outside the sessionexec ABI.
const (
	temporarySessionWorkerPollInterval = 2 * time.Second
	temporaryOutboxClaimTTL            = 30 * time.Second
	temporarySessionWorkerBatchSize    = 32
	temporaryOutboxRetryBase           = time.Second
	temporaryOutboxRetryMax            = time.Minute
)

type sessionExecutionWorkers struct {
	server *Server
	store  *sessionexec.PGStore

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newSessionExecutionWorkers(server *Server) *sessionExecutionWorkers {
	return &sessionExecutionWorkers{server: server, store: sessionexec.NewPGStore()}
}

func sessionExecutionWorkersDisabled() bool {
	value := strings.TrimSpace(os.Getenv(sessionExecutionWorkersDisableEnv))
	if value == "" {
		return false
	}
	disabled, err := strconv.ParseBool(value)
	return err == nil && disabled
}

func (workers *sessionExecutionWorkers) Start() {
	if workers == nil || workers.server == nil || workers.server.StoreExt == nil {
		return
	}
	workers.mu.Lock()
	if workers.cancel != nil {
		workers.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	workers.cancel = cancel
	workers.mu.Unlock()

	for _, loop := range []func(context.Context){
		workers.deliveryLoop,
		workers.expiryLoop,
		workers.finalizerLoop,
	} {
		workers.wg.Add(1)
		go func(run func(context.Context)) {
			defer workers.wg.Done()
			run(ctx)
		}(loop)
	}
	slog.Info("session execution background workers started")
}

func (workers *sessionExecutionWorkers) Stop() {
	if workers == nil {
		return
	}
	workers.mu.Lock()
	cancel := workers.cancel
	workers.cancel = nil
	workers.mu.Unlock()
	if cancel != nil {
		cancel()
		workers.wg.Wait()
		slog.Info("session execution background workers stopped")
	}
}

func (workers *sessionExecutionWorkers) deliveryLoop(ctx context.Context) {
	workers.poll(ctx, "outbox delivery", workers.deliverBatch)
}

func (workers *sessionExecutionWorkers) expiryLoop(ctx context.Context) {
	workers.poll(ctx, "lease expiry", workers.expireBatch)
}

func (workers *sessionExecutionWorkers) finalizerLoop(ctx context.Context) {
	workers.poll(ctx, "terminal finalizer", workers.finalizeBatch)
}

func (workers *sessionExecutionWorkers) poll(
	ctx context.Context,
	name string,
	run func(context.Context) error,
) {
	for {
		if err := run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("session execution worker iteration failed", "worker", name, "error", err)
		}
		timer := time.NewTimer(temporarySessionWorkerPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (workers *sessionExecutionWorkers) claimOutbox(
	ctx context.Context,
	owner string,
) ([]sessionexec.OutboxRecord, error) {
	tx, err := workers.server.StoreExt.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var dbNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
		return nil, fmt.Errorf("read outbox claim clock: %w", err)
	}
	records, err := workers.store.ClaimOutboxBatch(
		ctx, tx, owner, dbNow.Add(temporaryOutboxClaimTTL),
		temporarySessionWorkerBatchSize,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit session outbox claim: %w", err)
	}
	return records, nil
}

func (workers *sessionExecutionWorkers) deliverBatch(ctx context.Context) error {
	owner := "session-outbox:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	records, err := workers.claimOutbox(ctx, owner)
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := workers.deliverOne(ctx, owner, record); err != nil {
			if markErr := workers.markDeliveryFailed(ctx, owner, record, err); markErr != nil {
				slog.Error(
					"session outbox failure mark failed",
					"event_id", record.EventID,
					"error", markErr,
				)
			}
		}
	}
	return nil
}

func outboxMessage(record sessionexec.OutboxRecord) sessionexec.FinalOutboxMessage {
	return sessionexec.FinalOutboxMessage{
		EventID: record.EventID, Key: record.Key, LeaseEpoch: record.LeaseEpoch,
		ActiveRunID: record.ActiveRunID, RunSnapshotID: record.RunSnapshotID,
		Role: record.Role, Content: record.Content, Metadata: record.Metadata,
	}
}

type deliveryProjectionMetadata struct {
	ConversationID    string          `json:"conversation_id"`
	AssistantMetadata json.RawMessage `json:"assistant_metadata"`
}

func (workers *sessionExecutionWorkers) deliverOne(
	ctx context.Context,
	owner string,
	record sessionexec.OutboxRecord,
) error {
	message := outboxMessage(record)
	tx, err := workers.server.StoreExt.BeginTx(ctx)
	if err != nil {
		return err
	}
	if err := workers.store.AppendSessionMessageTx(ctx, tx, message); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("project outbox session event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit outbox session projection: %w", err)
	}

	var metadata deliveryProjectionMetadata
	if err := json.Unmarshal(record.Metadata, &metadata); err != nil {
		return fmt.Errorf("decode outbox delivery metadata: %w", err)
	}
	if metadata.ConversationID != "" {
		if workers.server.Conversations == nil {
			return fmt.Errorf("conversation projection store is unavailable")
		}
		if _, err := workers.server.Conversations.AppendMessage(ctx, conversation.Message{
			ConversationID: metadata.ConversationID,
			WorkspaceID:    record.Key.WorkspaceID,
			Role:           record.Role,
			Content:        record.Content,
			Metadata:       metadata.AssistantMetadata,
			EventID:        record.EventID,
			LeaseEpoch:     record.LeaseEpoch,
		}); err != nil {
			return fmt.Errorf("project outbox conversation event: %w", err)
		}
	}

	tx, err = workers.server.StoreExt.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var dbNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
		return fmt.Errorf("read outbox delivery clock: %w", err)
	}
	if err := workers.store.MarkOutboxDelivered(
		ctx, tx, record.Key, record.EventID, owner, dbNow,
	); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit outbox delivery mark: %w", err)
	}
	return nil
}

func deliveryRetryDelay(attempts int32) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := temporaryOutboxRetryBase
	for attempt := int32(1); attempt < attempts && delay < temporaryOutboxRetryMax; attempt++ {
		delay *= 2
	}
	if delay > temporaryOutboxRetryMax {
		return temporaryOutboxRetryMax
	}
	return delay
}

func (workers *sessionExecutionWorkers) markDeliveryFailed(
	ctx context.Context,
	owner string,
	record sessionexec.OutboxRecord,
	deliveryErr error,
) error {
	tx, err := workers.server.StoreExt.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var dbNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
		return fmt.Errorf("read outbox retry clock: %w", err)
	}
	err = workers.store.MarkOutboxFailed(
		ctx,
		tx,
		record.Key,
		record.EventID,
		owner,
		dbNow.Add(deliveryRetryDelay(record.DeliveryAttempts)),
		deliveryErr.Error(),
	)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (workers *sessionExecutionWorkers) expireBatch(ctx context.Context) error {
	tx, err := workers.server.StoreExt.BeginTx(ctx)
	if err != nil {
		return err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("read session expiry clock: %w", err)
	}
	expired, err := workers.store.ListExpired(
		ctx, tx, now, temporarySessionWorkerBatchSize,
	)
	if err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit expired lease scan: %w", err)
	}
	for _, lease := range expired {
		tx, err := workers.server.StoreExt.BeginTx(ctx)
		if err != nil {
			return err
		}
		_, expireErr := workers.store.ExpireOneTx(
			ctx, tx, lease.Key, lease.LeaseEpoch, now,
		)
		if expireErr != nil {
			_ = tx.Rollback(ctx)
			return expireErr
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit session lease expiry: %w", err)
		}
	}
	return nil
}

type finalizerCandidate struct {
	Key              sessionexec.SessionKey
	LeaseEpoch       int64
	ActiveRunID      string
	RunSnapshotID    string
	AcquireEventID   string
	Agent            string
	ConversationID   string
	TaskGroupID      string
	TaskGroupWorkers []string
	CheckpointBytes  []byte
}

func (workers *sessionExecutionWorkers) finalizerCandidates(
	ctx context.Context,
) ([]finalizerCandidate, error) {
	tx, err := workers.server.StoreExt.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT lease.workspace_id,lease.user_id,lease.lead_avatar_id,lease.session_id,
			lease.lease_epoch,lease.active_run_id,lease.run_snapshot_id,
			lease.acquire_event_id,marker.agent,COALESCE(conversation.id,''),
			COALESCE(marker.task_group_id,''),
			COALESCE((
				SELECT array_agg(task.agent ORDER BY task.created_at,task.id)
				FROM weave_task_queue AS task
				WHERE task.workspace_id=marker.workspace_id
					AND task.task_group_id=marker.task_group_id
			),ARRAY[]::text[]),
			checkpoint.value
		FROM weave_run_terminal_markers AS marker
		JOIN weave_session_execution_leases AS lease
			ON lease.workspace_id=marker.workspace_id
			AND lease.active_run_id=marker.run_id
			AND lease.run_snapshot_id=marker.run_snapshot_id
		JOIN loom_store AS checkpoint
			ON checkpoint.namespace='checkpoint:' || marker.workspace_id || ':' || marker.agent
			AND checkpoint.key=marker.run_id
		LEFT JOIN weave_conversations AS conversation
			ON conversation.workspace_id=lease.workspace_id
			AND conversation.agent_id=lease.lead_avatar_id
			AND conversation.user_id=lease.user_id
			AND conversation.session_key=
				lease.workspace_id || ':' || lease.user_id || ':' ||
				marker.agent || ':' || lease.session_id
		WHERE marker.phase='final' AND marker.status='success'
			AND lease.state='active'
			AND NOT EXISTS (
				SELECT 1 FROM weave_session_outbox AS outbox
				WHERE outbox.workspace_id=lease.workspace_id
					AND outbox.user_id=lease.user_id
					AND outbox.lead_avatar_id=lease.lead_avatar_id
					AND outbox.session_id=lease.session_id
					AND outbox.active_run_id=lease.active_run_id
			)
		ORDER BY marker.terminal_at,marker.workspace_id,marker.run_id
		LIMIT $1
	`, temporarySessionWorkerBatchSize)
	if err != nil {
		return nil, fmt.Errorf("scan terminal finalizer candidates: %w", err)
	}
	defer rows.Close()
	candidates := make([]finalizerCandidate, 0)
	for rows.Next() {
		var candidate finalizerCandidate
		if err := rows.Scan(
			&candidate.Key.WorkspaceID,
			&candidate.Key.UserID,
			&candidate.Key.LeadAvatarID,
			&candidate.Key.SessionID,
			&candidate.LeaseEpoch,
			&candidate.ActiveRunID,
			&candidate.RunSnapshotID,
			&candidate.AcquireEventID,
			&candidate.Agent,
			&candidate.ConversationID,
			&candidate.TaskGroupID,
			&candidate.TaskGroupWorkers,
			&candidate.CheckpointBytes,
		); err != nil {
			return nil, fmt.Errorf("decode terminal finalizer candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate terminal finalizer candidates: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit terminal finalizer scan: %w", err)
	}
	return candidates, nil
}

type finalizerCheckpoint struct {
	State map[string]any `json:"state"`
}

func (workers *sessionExecutionWorkers) finalizerMessage(
	ctx context.Context,
	candidate finalizerCandidate,
) (sessionexec.FinalOutboxMessage, error) {
	var checkpoint finalizerCheckpoint
	if err := json.Unmarshal(candidate.CheckpointBytes, &checkpoint); err != nil {
		return sessionexec.FinalOutboxMessage{}, fmt.Errorf("decode finalizer checkpoint: %w", err)
	}
	if checkpoint.State == nil {
		return sessionexec.FinalOutboxMessage{}, fmt.Errorf("finalizer checkpoint state is missing")
	}
	output, ok := checkpoint.State["output"].(string)
	if !ok {
		return sessionexec.FinalOutboxMessage{}, fmt.Errorf("finalizer checkpoint output is missing")
	}
	assistantMetadata := workers.server.buildAssistantMetadata(
		contextWithAssistantAgent(ctx, candidate.Agent),
		candidate.Key.WorkspaceID,
		output,
	)
	userMessage, _ := checkpoint.State["last_user_message"].(string)
	sessionMessages := make([]contract.Message, 0, 2)
	if candidate.TaskGroupID != "" {
		sessionMessages = append(sessionMessages, contract.Message{
			Role: "assistant",
			Content: withTaskGroupProvenance(output, map[string]any{
				"task_group_id":      candidate.TaskGroupID,
				"task_group_workers": candidate.TaskGroupWorkers,
			}, candidate.ActiveRunID),
		})
	} else {
		if userMessage != "" {
			sessionMessages = append(
				sessionMessages, contract.Message{Role: "user", Content: userMessage},
			)
		}
		sessionMessages = append(
			sessionMessages, contract.Message{Role: "assistant", Content: output},
		)
	}
	var assistantFields map[string]any
	if len(assistantMetadata) > 0 {
		_ = json.Unmarshal(assistantMetadata, &assistantFields)
	}
	metadata, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"session_key": strings.Join([]string{
			candidate.Key.WorkspaceID,
			candidate.Key.UserID,
			candidate.Agent,
			candidate.Key.SessionID,
		}, ":"),
		"conversation_id":    candidate.ConversationID,
		"assistant_metadata": assistantFields,
		"session_messages":   sessionMessages,
	})
	if err != nil {
		return sessionexec.FinalOutboxMessage{}, err
	}
	return sessionexec.FinalOutboxMessage{
		EventID: "final:" + candidate.AcquireEventID,
		Key:     candidate.Key, LeaseEpoch: candidate.LeaseEpoch,
		ActiveRunID: candidate.ActiveRunID, RunSnapshotID: candidate.RunSnapshotID,
		Role: "assistant", Content: output, Metadata: metadata,
	}, nil
}

func (workers *sessionExecutionWorkers) finalizeBatch(ctx context.Context) error {
	candidates, err := workers.finalizerCandidates(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		message, err := workers.finalizerMessage(ctx, candidate)
		if err != nil {
			slog.Error(
				"terminal finalizer checkpoint rejected",
				"run_id", candidate.ActiveRunID,
				"error", err,
			)
			continue
		}
		tx, err := workers.server.StoreExt.BeginTx(ctx)
		if err != nil {
			return err
		}
		_, commitErr := workers.store.CommitFinalTx(ctx, tx, message)
		if commitErr == nil {
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("commit terminal finalizer replay: %w", err)
			}
			continue
		}
		_ = tx.Rollback(ctx)
		if errors.Is(commitErr, sessionexec.ErrOutboxConflict) {
			if err := workers.auditFinalizerConflict(ctx, candidate, message); err != nil {
				slog.Error(
					"terminal finalizer conflict audit failed",
					"run_id", candidate.ActiveRunID,
					"error", err,
				)
			}
			continue
		}
		slog.Error(
			"terminal finalizer replay failed",
			"run_id", candidate.ActiveRunID,
			"error", commitErr,
		)
	}
	return nil
}

func (workers *sessionExecutionWorkers) auditFinalizerConflict(
	ctx context.Context,
	candidate finalizerCandidate,
	message sessionexec.FinalOutboxMessage,
) error {
	digest := sha256.Sum256(
		append(append([]byte(nil), []byte(message.Content)...), message.Metadata...),
	)
	detail := json.RawMessage(`{"schema_version":1,"side_effect":"finalizer_replay"}`)
	tx, err := workers.server.StoreExt.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lease, err := workers.store.Get(ctx, tx, candidate.Key)
	if err != nil {
		return err
	}
	err = workers.store.IsolatedAppendAuditTx(
		ctx,
		tx,
		sessionexec.IsolatedSessionAuditEvent{
			ID:  "isolated:finalizer_replay:" + candidate.ActiveRunID,
			Key: candidate.Key, ObservedEpoch: candidate.LeaseEpoch,
			CurrentEpoch: lease.LeaseEpoch, ActiveRunID: candidate.ActiveRunID,
			EventKind: "late_write_isolated", IsolationReason: "outbox_conflict",
			PayloadDigest: digest[:], Detail: detail,
		},
	)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
