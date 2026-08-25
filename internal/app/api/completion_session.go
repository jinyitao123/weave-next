package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/fanout"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/sessionexec"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

type completionAdmissionRetry struct {
	disposition sessionexec.AcquireDisposition
}

func (err *completionAdmissionRetry) Error() string {
	if err == nil || err.disposition == "" {
		return "completion session admission must be retried"
	}
	return "completion session admission must be retried: " + string(err.disposition)
}

func (*completionAdmissionRetry) RetryTask() bool { return true }

func completionTaskGroupID(req taskqueue.ChatExecRequest) string {
	groupID, _ := req.Context["task_group_id"].(string)
	return strings.TrimSpace(groupID)
}

type completionRequestIdentity struct {
	LegacyGroupID   string
	WorkflowGroupID string
	CompletionID    string
}

func completionIdentity(req taskqueue.ChatExecRequest) completionRequestIdentity {
	identity := completionRequestIdentity{
		LegacyGroupID: completionTaskGroupID(req),
	}
	identity.WorkflowGroupID, _ = req.Context["workflow_fanout_group_id"].(string)
	identity.CompletionID, _ = req.Context["group_completion_id"].(string)
	identity.WorkflowGroupID = strings.TrimSpace(identity.WorkflowGroupID)
	identity.CompletionID = strings.TrimSpace(identity.CompletionID)
	if identity.WorkflowGroupID != "" {
		identity.LegacyGroupID = ""
	}
	if identity.CompletionID == "" && identity.LegacyGroupID != "" {
		identity.CompletionID = "completion:" + identity.LegacyGroupID
	}
	return identity
}

func completionSessionID(sessionKey, workspaceID, userID, agentName string) (string, error) {
	prefix := strings.Join([]string{workspaceID, userID, agentName}, ":") + ":"
	if !strings.HasPrefix(sessionKey, prefix) {
		return "", fmt.Errorf("completion conversation session key has unexpected ownership")
	}
	sessionID := strings.TrimPrefix(sessionKey, prefix)
	if sessionID == "" {
		return "", fmt.Errorf("completion conversation session key has empty session id")
	}
	return sessionID, nil
}

func completionSnapshot(
	runID string,
	groupID string,
	eventID string,
	parentRunID string,
	lead *registry.AgentRecord,
	sourceSnapshot snapshot.TeamRunSnapshot,
	decidedAt time.Time,
) snapshot.TeamRunSnapshot {
	admission, _ := json.Marshal(map[string]any{
		"schema_version":  1,
		"team_active":     true,
		"workflow_active": nil,
		"workers_enabled": true,
		"version_blocked": nil,
		"decided_at":      decidedAt.UTC().Format(time.RFC3339Nano),
	})
	associations, _ := json.Marshal(map[string]any{
		"schema_version":     1,
		"parent_run_id":      parentRunID,
		"source_snapshot_id": sourceSnapshot.RunID,
		"task_group_id":      groupID,
	})
	trigger, _ := json.Marshal(map[string]any{
		"schema_version": 1,
		"type":           "fanout_synthesis",
		"source_ref":     eventID,
	})
	workerVersions := append(json.RawMessage(nil), sourceSnapshot.WorkerVersions...)
	if len(workerVersions) == 0 {
		workerVersions = json.RawMessage(`{}`)
	}
	teamWorkers := append(json.RawMessage(nil), sourceSnapshot.TeamWorkerSnapshot...)
	if len(teamWorkers) == 0 {
		teamWorkers = json.RawMessage(`[]`)
	}
	inlineDependencies := append(
		json.RawMessage(nil), sourceSnapshot.InlineDependencies...,
	)
	if len(inlineDependencies) == 0 {
		inlineDependencies = json.RawMessage(`{}`)
	}
	return snapshot.TeamRunSnapshot{
		RunID:                 runID,
		WorkspaceID:           sourceSnapshot.WorkspaceID,
		ProjectID:             sourceSnapshot.ProjectID,
		RuntimeAssignment:     append(json.RawMessage(nil), sourceSnapshot.RuntimeAssignment...),
		TeamID:                sourceSnapshot.TeamID,
		SnapshotSchemaVersion: 2,
		Mode:                  "free_collab",
		LeadAvatarID:          lead.ID,
		LeadAvatarVersion:     lead.Version,
		WorkerVersions:        workerVersions,
		TeamWorkerSnapshot:    teamWorkers,
		AdmissionDecision:     admission,
		InlineDependencies:    inlineDependencies,
		RunAssociations:       associations,
		TriggerSourceV2:       trigger,
	}
}

func (s *Server) acquireCompletionSession(
	ctx context.Context,
	workspaceID string,
	groupID string,
) (*registry.AgentRecord, *teamSessionExecution, error) {
	return s.acquireCompletionSessionEvent(ctx, workspaceID, completionRequestIdentity{
		LegacyGroupID: groupID,
		CompletionID:  "completion:" + groupID,
	})
}

type workflowCompletionSource struct {
	GroupID          string
	CompletionID     string
	ParentRunID      string
	SourceSnapshotID string
	ParentSeq        int64
	UserID           string
	SessionID        string
	ConversationID   string
	LoomSessionKey   string
	Record           *registry.AgentRecord
	Snapshot         snapshot.TeamRunSnapshot
}

func (s *Server) resolveWorkflowCompletionSession(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	groupID string,
	completionID string,
) (workflowCompletionSource, error) {
	var source workflowCompletionSource
	source.GroupID, source.CompletionID = groupID, completionID
	if err := tx.QueryRow(ctx, `
		SELECT intent.parent_run_id,intent.run_snapshot_id,
			intent.previous_checkpoint_sequence
		FROM weave_fanout_group AS fanout_group
		JOIN weave_fanout_intent AS intent
		  ON intent.workspace_id=fanout_group.workspace_id
		 AND intent.intent_id=fanout_group.intent_id
		WHERE fanout_group.workspace_id=$1
			AND fanout_group.group_id=$2
			AND fanout_group.group_completion_id=$3
			AND fanout_group.mode='free_collab_synthesis'
			AND fanout_group.status IN ('decided','closed')
	`, workspaceID, groupID, completionID).Scan(
		&source.ParentRunID, &source.SourceSnapshotID, &source.ParentSeq,
	); err != nil {
		return workflowCompletionSource{}, fmt.Errorf(
			"read workflow completion source: %w", err,
		)
	}
	loaded, err := s.Snapshots.GetByRunID(ctx, workspaceID, source.SourceSnapshotID)
	if err != nil {
		return workflowCompletionSource{}, fmt.Errorf(
			"load workflow completion source snapshot: %w", err,
		)
	}
	source.Snapshot = *loaded
	if loaded.LeadAvatarID == "" || loaded.LeadAvatarVersion < 1 {
		return workflowCompletionSource{}, fmt.Errorf(
			"workflow completion source snapshot has no frozen lead",
		)
	}
	source.Record, err = s.Registry.GetVersion(
		ctx, workspaceID, loaded.LeadAvatarID, loaded.LeadAvatarVersion,
	)
	if err != nil {
		return workflowCompletionSource{}, fmt.Errorf(
			"load workflow completion lead version: %w", err,
		)
	}
	if err := tx.QueryRow(ctx, `
		SELECT user_id,session_id
		FROM (
			SELECT user_id,session_id,0 AS source_order,created_at AS source_time
			FROM weave_session_outbox
			WHERE workspace_id=$1 AND lead_avatar_id=$2 AND active_run_id=$3
			UNION ALL
			SELECT user_id,session_id,1 AS source_order,updated_at AS source_time
			FROM weave_session_execution_leases
			WHERE workspace_id=$1 AND lead_avatar_id=$2 AND active_run_id=$3
		) AS source_session
		ORDER BY source_order,source_time DESC,user_id,session_id
		LIMIT 1
	`, workspaceID, source.Record.ID, source.ParentRunID).Scan(
		&source.UserID, &source.SessionID,
	); err != nil {
		return workflowCompletionSource{}, fmt.Errorf(
			"read workflow completion source session: %w", err,
		)
	}
	source.LoomSessionKey = strings.Join([]string{
		workspaceID, source.UserID, source.Record.Name, source.SessionID,
	}, ":")
	if err := tx.QueryRow(ctx, `
		SELECT id
		FROM weave_conversations
		WHERE workspace_id=$1 AND agent_id=$2 AND user_id=$3 AND session_key=$4
	`, workspaceID, source.Record.ID, source.UserID, source.LoomSessionKey).Scan(
		&source.ConversationID,
	); err != nil {
		return workflowCompletionSource{}, fmt.Errorf(
			"read workflow completion conversation: %w", err,
		)
	}
	return source, nil
}

func (s *Server) acquireCompletionSessionEvent(
	ctx context.Context,
	workspaceID string,
	identity completionRequestIdentity,
) (*registry.AgentRecord, *teamSessionExecution, error) {
	if s.StoreExt == nil || s.Snapshots == nil || s.Registry == nil {
		return nil, nil, fmt.Errorf("completion session dependencies are unavailable")
	}
	if identity.CompletionID == "" ||
		(identity.LegacyGroupID == "") == (identity.WorkflowGroupID == "") {
		return nil, nil, fmt.Errorf("completion event identity is invalid")
	}
	tx, err := s.StoreExt.BeginTx(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var agentName, userID, conversationID, parentRunID, sourceSnapshotID string
	var parentSeq int64
	var workflowSource *workflowCompletionSource
	groupID := identity.LegacyGroupID
	if identity.WorkflowGroupID != "" {
		source, err := s.resolveWorkflowCompletionSession(
			ctx, tx, workspaceID, identity.WorkflowGroupID, identity.CompletionID,
		)
		if err != nil {
			return nil, nil, err
		}
		workflowSource = &source
		groupID = source.GroupID
		agentName, userID, conversationID = source.Record.Name, source.UserID, source.ConversationID
		parentRunID, parentSeq, sourceSnapshotID = source.ParentRunID, source.ParentSeq, source.SourceSnapshotID
	} else {
		if err := tx.QueryRow(ctx, `
			SELECT avatar_agent,user_id,COALESCE(conversation_id,'')
			FROM weave_task_group
			WHERE workspace_id=$1 AND id=$2
		`, workspaceID, groupID).Scan(&agentName, &userID, &conversationID); err != nil {
			return nil, nil, fmt.Errorf("read completion task group: %w", err)
		}
		if err := tx.QueryRow(ctx, `
			SELECT aggregation_parent_run_id,parent_seq,run_snapshot_id
			FROM weave_run_terminal_markers
			WHERE workspace_id=$1 AND task_group_id=$2
				AND phase='final'
				AND aggregation_parent_run_id IS NOT NULL
				AND parent_seq IS NOT NULL
				AND run_snapshot_id IS NOT NULL
			ORDER BY created_at,run_id
			LIMIT 1
		`, workspaceID, groupID).Scan(
			&parentRunID, &parentSeq, &sourceSnapshotID,
		); err != nil {
			return nil, nil, fmt.Errorf("read completion parent attribution: %w", err)
		}
	}
	sourceSnapshot, err := s.Snapshots.GetByRunID(ctx, workspaceID, sourceSnapshotID)
	if err != nil {
		return nil, nil, fmt.Errorf("load completion source snapshot: %w", err)
	}
	var rec *registry.AgentRecord
	if workflowSource != nil {
		rec = workflowSource.Record
	} else if sourceSnapshot.LeadAvatarID != "" && sourceSnapshot.LeadAvatarVersion > 0 {
		rec, err = s.Registry.GetVersion(
			ctx, workspaceID, sourceSnapshot.LeadAvatarID, sourceSnapshot.LeadAvatarVersion,
		)
	} else {
		rec, err = s.Registry.Get(ctx, workspaceID, agentName)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("load completion lead version: %w", err)
	}
	if rec.Name != agentName {
		return nil, nil, fmt.Errorf("completion lead version does not match task group avatar")
	}
	var teamActive bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM weave_teams
			WHERE workspace_id=$1 AND id=$2 AND lead_avatar_id=$3 AND status='active'
		)
	`, workspaceID, sourceSnapshot.TeamID, rec.ID).Scan(&teamActive); err != nil {
		return nil, nil, fmt.Errorf("check completion team admission: %w", err)
	}
	if !teamActive {
		return nil, nil, fmt.Errorf("completion source team is not active for the frozen lead")
	}

	var loomSessionKey string
	if workflowSource != nil {
		loomSessionKey = workflowSource.LoomSessionKey
	} else if conversationID != "" {
		err = tx.QueryRow(ctx, `
			SELECT session_key
			FROM weave_conversations
			WHERE workspace_id=$1 AND id=$2 AND agent_id=$3 AND user_id=$4
		`, workspaceID, conversationID, rec.ID, userID).Scan(&loomSessionKey)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT id,session_key
			FROM weave_conversations
			WHERE workspace_id=$1 AND agent_id=$2 AND user_id=$3
				AND channel='default' AND parent_message_id IS NULL
		`, workspaceID, rec.ID, userID).Scan(&conversationID, &loomSessionKey)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read completion conversation session: %w", err)
	}
	sessionID, err := completionSessionID(loomSessionKey, workspaceID, userID, agentName)
	if err != nil {
		return nil, nil, err
	}
	key := sessionexec.SessionKey{
		WorkspaceID: workspaceID, UserID: userID,
		LeadAvatarID: rec.ID, SessionID: sessionID,
	}
	eventID := identity.CompletionID
	runID := deterministicTeamRunID(key, eventID)

	var existingSnapshot bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM weave_team_run_snapshots
			WHERE workspace_id=$1 AND run_id=$2
		)
	`, workspaceID, runID).Scan(&existingSnapshot); err != nil {
		return nil, nil, fmt.Errorf("check completion snapshot replay: %w", err)
	}
	if !existingSnapshot {
		var dbNow time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
			return nil, nil, fmt.Errorf("read completion admission clock: %w", err)
		}
		if _, err := s.Snapshots.CreateTx(
			ctx,
			tx,
			completionSnapshot(runID, groupID, eventID, parentRunID, rec, *sourceSnapshot, dbNow),
		); err != nil {
			return nil, nil, fmt.Errorf("create completion snapshot: %w", err)
		}
	}

	acquired, err := sessionexec.NewPGStore().AcquireTx(ctx, tx, sessionexec.AcquireRequest{
		Key: key, EventID: eventID, ActiveRunID: runID, RunSnapshotID: runID,
		ProjectID:      sourceSnapshot.ProjectID,
		ControllerKind: sessionexec.ControllerLead, ControllerID: rec.ID,
		TTL: temporarySessionExecutionTTL,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("acquire completion session execution lease: %w", err)
	}
	switch acquired.Disposition {
	case sessionexec.AcquireGranted, sessionexec.AcquireReplay:
	case sessionexec.AcquireBusy, sessionexec.AcquireRouteToParked:
		return nil, nil, &completionAdmissionRetry{disposition: acquired.Disposition}
	default:
		return nil, nil, fmt.Errorf(
			"unsupported completion acquire disposition %q", acquired.Disposition,
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit completion session admission: %w", err)
	}
	createdSnapshot, err := s.Snapshots.GetByRunID(ctx, workspaceID, runID)
	if err != nil {
		return nil, nil, fmt.Errorf("load completion snapshot: %w", err)
	}
	attribution, err := terminalAttributionFromSnapshot(
		workspaceID, *createdSnapshot, &parentSeq,
	)
	if err != nil {
		return nil, nil, err
	}
	return rec, &teamSessionExecution{
		Lease: acquired.Lease, Snapshot: *createdSnapshot,
		TerminalAttribution: attribution, AcquireEventID: eventID,
		LoomSessionKey: loomSessionKey, ConversationID: conversationID,
		NextYieldGeneration: 1, memoryOutboxReady: make(chan struct{}),
	}, nil
}

type completionTaskBuilder struct {
	Server *Server
}

func (b completionTaskBuilder) BuildLateSynthesisTask(
	ctx context.Context,
	tx pgx.Tx,
	completion fanout.GroupCompletion,
) (*taskqueue.Task, error) {
	if b.Server == nil || b.Server.Snapshots == nil || b.Server.Registry == nil {
		return nil, fanout.NewWorkflowError(
			fanout.ErrorStoreUnavailable, "completion task builder dependencies are unavailable",
		)
	}
	source, err := b.Server.resolveWorkflowCompletionSession(
		ctx, tx, completion.WorkspaceID, completion.GroupID, completion.GroupCompletionID,
	)
	if err != nil {
		return nil, err
	}
	message, workers, err := workflowCompletionPrompt(completion)
	if err != nil {
		return nil, err
	}
	var runtimeAssignment *taskqueue.RuntimeAssignment
	if len(source.Snapshot.RuntimeAssignment) > 0 {
		runtimeAssignment = &taskqueue.RuntimeAssignment{}
		if err := json.Unmarshal(source.Snapshot.RuntimeAssignment, runtimeAssignment); err != nil {
			return nil, fanout.NewWorkflowError(
				fanout.ErrorInvalidRequest, "decode frozen runtime assignment: %v", err,
			)
		}
	}
	payload, err := json.Marshal(taskqueue.ChatExecRequest{
		Agent: source.Record.Name, AgentID: source.Record.ID,
		AgentVersion: source.Record.Version, UserID: source.UserID,
		ProjectID: source.Snapshot.ProjectID, RuntimeAssignment: runtimeAssignment,
		ConversationID: source.ConversationID, Message: message, NoDispatch: true,
		Context: map[string]any{
			"task_group_id":            completion.GroupID,
			"workflow_fanout_group_id": completion.GroupID,
			"group_completion_id":      completion.GroupCompletionID,
			"task_group_workers":       workers,
		},
	})
	if err != nil {
		return nil, fanout.NewWorkflowError(
			fanout.ErrorInvalidRequest, "encode late synthesis task: %v", err,
		)
	}
	return &taskqueue.Task{
		ID: completion.GroupCompletionID, WorkspaceID: completion.WorkspaceID,
		ProjectID: source.Snapshot.ProjectID,
		Agent:     source.Record.Name, AgentID: source.Record.ID,
		AgentVersion: source.Record.Version, IdentityKind: taskqueue.IdentityAgent,
		IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeTeamFreeCollab,
		RunSnapshotID:     source.Snapshot.RunID,
		RuntimeAssignment: append(json.RawMessage(nil), source.Snapshot.RuntimeAssignment...),
		ContextKey:        completion.GroupCompletionID, Source: "fanout_completion",
		Kind: "chat", Status: taskqueue.StatusQueued, Payload: payload,
		RuntimeID: func() string {
			if runtimeAssignment == nil {
				return ""
			}
			return runtimeAssignment.RuntimeID
		}(),
	}, nil
}

func workflowCompletionPrompt(
	completion fanout.GroupCompletion,
) (string, []string, error) {
	var result fanout.JoinResultV1
	if err := json.Unmarshal(completion.JoinResult, &result); err != nil {
		return "", nil, fanout.NewWorkflowError(
			fanout.ErrorInvalidRequest, "decode late synthesis join result: %v", err,
		)
	}
	if result.GroupID != completion.GroupID ||
		result.GroupCompletionID != completion.GroupCompletionID ||
		result.Generation != completion.Generation {
		return "", nil, fanout.NewWorkflowError(
			fanout.ErrorResumeConflict, "late synthesis join result identity differs",
		)
	}
	if _, err := fanout.ProjectJoinResult(result); err != nil {
		return "", nil, err
	}
	workers := make([]string, 0, len(result.Legs))
	var prompt strings.Builder
	prompt.WriteString("你派出的团队已完成，以下是冻结的决议结果，请综合后向用户汇报。\n\n")
	fmt.Fprintf(
		&prompt, "任务组：%s\n决议：%s\n诊断：%s\n\n各成员结果：\n",
		result.GroupID, result.Decision, result.Diagnostic,
	)
	for _, leg := range result.Legs {
		workers = append(workers, leg.BranchID)
		summary := completionResultText(leg.Result)
		if summary == "" && leg.ErrorCode != nil {
			summary = *leg.ErrorCode
		}
		if summary == "" {
			summary = "无结果"
		}
		fmt.Fprintf(
			&prompt, "- 员工：%s\n  状态：%s\n  结果摘要：%s\n",
			leg.BranchID, leg.DecisionState, summary,
		)
	}
	return prompt.String(), workers, nil
}

func completionResultText(result json.RawMessage) string {
	if len(result) == 0 || string(result) == "null" {
		return ""
	}
	var value string
	if json.Unmarshal(result, &value) == nil {
		return value
	}
	return string(result)
}

func completionExecutionStamp(rec *registry.AgentRecord) *execution.AgentExecutionStamp {
	return &execution.AgentExecutionStamp{
		AgentID: rec.ID, AgentVersion: rec.Version,
		ExecutionScope: execution.ScopeTeamFreeCollab,
	}
}

func (s *Server) finishCompletionSession(
	ctx context.Context,
	execution *teamSessionExecution,
	result loomruntime.Result,
	assistantMetadata json.RawMessage,
	contextValues map[string]any,
) error {
	if execution == nil {
		return errors.New("completion session execution is required")
	}
	if result.Yielded {
		return fmt.Errorf("completion synthesis cannot yield")
	}
	content := result.Output
	sessionContent := withTaskGroupProvenance(content, contextValues, result.RunID)
	var assistantFields map[string]any
	if len(assistantMetadata) > 0 {
		_ = json.Unmarshal(assistantMetadata, &assistantFields)
	}
	metadata, err := json.Marshal(map[string]any{
		"schema_version":     1,
		"session_key":        execution.LoomSessionKey,
		"conversation_id":    execution.ConversationID,
		"assistant_metadata": assistantFields,
		"session_messages": []contract.Message{
			{Role: "assistant", Content: sessionContent},
		},
	})
	if err != nil {
		return fmt.Errorf("encode completion final metadata: %w", err)
	}
	tx, err := s.StoreExt.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	closed, err := sessionexec.NewPGStore().CommitFinalTx(
		ctx,
		tx,
		sessionexec.FinalOutboxMessage{
			EventID: "final:" + execution.AcquireEventID,
			Key:     execution.Lease.Key, LeaseEpoch: execution.Lease.LeaseEpoch,
			ActiveRunID: result.RunID, RunSnapshotID: execution.Snapshot.RunID,
			Role: "assistant", Content: content, Metadata: metadata,
		},
	)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit completion final outbox: %w", err)
	}
	execution.Lease = closed
	execution.memoryOutboxOnce.Do(func() { close(execution.memoryOutboxReady) })
	return nil
}
