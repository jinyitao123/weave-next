package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/jinyitao123/weave/internal/app/designseed"
	"github.com/jinyitao123/weave/internal/app/metateam"
	"github.com/jinyitao123/weave/internal/app/projects"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/grounding"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/memory"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/sessionexec"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/labstack/echo/v4"
)

// ChatRequest is the input to the chat endpoint.
type ChatRequest struct {
	Agent                    string               `json:"agent"`
	ProjectID                string               `json:"project_id,omitempty"`
	ConversationID           string               `json:"conversation_id,omitempty"`
	Intent                   string               `json:"intent,omitempty"`
	RuntimeID                string               `json:"runtime_id,omitempty"`
	ClientRequestID          string               `json:"client_request_id,omitempty"`
	SessionID                string               `json:"session_id"`
	Message                  string               `json:"message"`
	Channel                  string               `json:"channel,omitempty"`
	Profile                  string               `json:"profile"`
	Effort                   string               `json:"effort"`
	Stream                   bool                 `json:"stream"`
	Async                    bool                 `json:"async"`   // if true, enqueue as a background task
	Context                  map[string]any       `json:"context"` // runtime context injected into system prompt
	AttachmentIDs            []string             `json:"attachment_ids,omitempty"`
	BlueprintChangeRequested bool                 `json:"blueprint_change_requested,omitempty"`
	RuntimeAssignment        *runtimes.Assignment `json:"-"`
}

// ChatResponse is the output of the chat endpoint.
type ChatResponse struct {
	Output            string               `json:"output"`
	StopReason        string               `json:"stop_reason"`
	Usage             contract.Usage       `json:"usage,omitempty"`
	SessionID         string               `json:"session_id"`
	RunID             string               `json:"run_id"`
	ProjectID         string               `json:"project_id,omitempty"`
	ConversationID    string               `json:"conversation_id,omitempty"`
	UserMessageID     string               `json:"user_message_id,omitempty"`
	YieldType         string               `json:"yield_type,omitempty"`
	GroundingWarning  string               `json:"grounding_warning,omitempty"`
	RuntimeAssignment *runtimes.Assignment `json:"runtime_assignment,omitempty"`
}

// Deployment policy pending (OQ-5). This is deliberately local to the API
// adapter and is not part of the sessionexec ABI.
const temporarySessionExecutionTTL = 30 * time.Minute

type teamSessionExecution struct {
	Lease               sessionexec.SessionExecutionLease
	Snapshot            snapshot.TeamRunSnapshot
	TerminalAttribution loomruntime.TerminalAttribution
	AcquireEventID      string
	LoomSessionKey      string
	ConversationID      string
	NextYieldGeneration int64
	Replayed            bool
	ReplayOutput        string
	// DeclaredDeliverable is the buffer the save_deliverable builtin tool
	// records into during this turn; finishTeamSessionExecution projects it.
	DeclaredDeliverable *mcphost.DeclaredDeliverable
	memoryOutboxReady   chan struct{}
	memoryOutboxOnce    sync.Once
}

func (s *Server) resolveChatRuntime(
	ctx context.Context,
	workspaceID, requestedRuntimeID string,
	rec *registry.AgentRecord,
) (*registry.AgentRecord, *runtimes.Assignment, error) {
	if rec == nil {
		return nil, nil, errors.New("agent record is required for runtime selection")
	}
	if !engine.IsCLIEngine(rec.Engine) {
		if strings.TrimSpace(requestedRuntimeID) != "" {
			return nil, nil, fmt.Errorf("runtime_override_not_supported")
		}
		engineName := strings.TrimSpace(rec.Engine)
		if engineName == "" {
			engineName = "loom"
		}
		return rec, &runtimes.Assignment{
			Mode: runtimes.SelectionAuto, ReasonCode: "in_process_engine",
			Engine: engineName, CapabilityFacts: []runtimes.CandidateFact{},
		}, nil
	}
	assignment, err := s.Runtimes.Select(
		ctx, workspaceID, rec.Engine, requestedRuntimeID, rec.RuntimeID,
	)
	if err != nil {
		return nil, nil, err
	}
	resolved := *rec
	resolved.RuntimeID = assignment.RuntimeID
	resolved.RuntimePoolID = assignment.PoolID
	if assignment.Mode == runtimes.SelectionExplicit {
		resolved.RuntimePolicyMode = "strict_pin"
	} else if assignment.PoolID != "" {
		resolved.RuntimePolicyMode = "engine_pool"
	} else {
		resolved.RuntimePolicyMode = "auto_single"
	}
	return &resolved, &assignment, nil
}

// llmForLoomNode selects only the model transport for a graph node. Team lead
// records remain Loom avatars even when their inference is delegated to an
// online CLI runtime.
func (s *Server) llmForLoomNode(
	ctx context.Context,
	workspaceID string,
	rec *registry.AgentRecord,
	teamExecution *teamSessionExecution,
) (contract.LLM, error) {
	if rec != nil && rec.Role == "avatar" && engine.IsCLIEngine(rec.Engine) {
		if teamExecution == nil || teamExecution.Snapshot.RunID == "" {
			return nil, errors.New("runtime LLM team execution snapshot is required")
		}
		return s.runtimeLLMForNode(
			workspaceID, rec, rec, execution.ScopeTeamFreeCollab,
			teamExecution.Snapshot.RunID,
		)
	}
	return s.llmFor(ctx, workspaceID)
}

func respondChatRuntimeError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, runtimes.ErrRuntimeSelectionUnavailable):
		return c.JSON(http.StatusConflict, map[string]string{"error": "runtime_unavailable"})
	case errors.Is(err, runtimes.ErrNoEligibleRuntime):
		return c.JSON(http.StatusConflict, map[string]string{"error": "no_eligible_runtime"})
	case err != nil && err.Error() == "runtime_override_not_supported":
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": "runtime_override_not_supported"})
	default:
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "runtime_selection_failed"})
	}
}

func encodeRuntimeAssignment(assignment *runtimes.Assignment) (json.RawMessage, error) {
	if assignment == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(assignment)
	if err != nil {
		return nil, fmt.Errorf("encode runtime assignment: %w", err)
	}
	return encoded, nil
}

func snapshotRuntimeAssignment(snap snapshot.TeamRunSnapshot) (*runtimes.Assignment, error) {
	if len(snap.RuntimeAssignment) == 0 {
		return nil, nil
	}
	var assignment runtimes.Assignment
	if err := json.Unmarshal(snap.RuntimeAssignment, &assignment); err != nil {
		return nil, fmt.Errorf("decode snapshot runtime assignment: %w", err)
	}
	if assignment.Mode == "" || assignment.ReasonCode == "" || assignment.Engine == "" {
		return nil, fmt.Errorf("snapshot runtime assignment is incomplete")
	}
	return &assignment, nil
}

func (s *Server) applySnapshotRuntimeAssignment(
	ctx context.Context,
	workspaceID string,
	rec *registry.AgentRecord,
	execution *teamSessionExecution,
) (*registry.AgentRecord, *runtimes.Assignment, error) {
	if execution == nil {
		return rec, nil, nil
	}
	assignment, err := snapshotRuntimeAssignment(execution.Snapshot)
	if err != nil || assignment == nil {
		return rec, assignment, err
	}
	if rec == nil {
		return nil, nil, errors.New("snapshot runtime assignment agent is unavailable")
	}
	engineName := rec.Engine
	if engineName == "" {
		engineName = "loom"
	}
	if assignment.Engine != engineName {
		// A team lead is frozen as a Loom graph identity. Its session may
		// separately freeze Codex as the node-inference transport, so resume
		// reconstructs that transient binding without changing AgentRecord.
		if rec.Role != "avatar" || assignment.Engine != engine.Codex || engineName != "loom" {
			return nil, nil, errors.New("snapshot runtime assignment engine differs from frozen agent")
		}
		resolvedIdentity := *rec
		resolvedIdentity.Engine = engine.Codex
		rec = &resolvedIdentity
	}
	if !engine.IsCLIEngine(rec.Engine) {
		if assignment.RuntimeID != "" || assignment.ReasonCode != "in_process_engine" {
			return nil, nil, errors.New("snapshot in-process runtime assignment is invalid")
		}
		return rec, assignment, nil
	}
	if assignment.RuntimeID == "" || assignment.RuntimeRevision < 1 || s.Runtimes == nil {
		return nil, nil, runtimes.ErrRuntimeSelectionUnavailable
	}
	selected, err := s.Runtimes.Get(ctx, workspaceID, assignment.RuntimeID)
	if err != nil || !selected.Online || selected.FunctionalRevision != assignment.RuntimeRevision {
		return nil, nil, runtimes.ErrRuntimeSelectionUnavailable
	}
	engineAvailable := false
	for _, candidate := range selected.Engines {
		if candidate == rec.Engine {
			engineAvailable = true
			break
		}
	}
	if !engineAvailable {
		return nil, nil, runtimes.ErrRuntimeSelectionUnavailable
	}
	resolved := *rec
	resolved.RuntimeID = assignment.RuntimeID
	resolved.RuntimePoolID = assignment.PoolID
	if assignment.Mode == runtimes.SelectionExplicit {
		resolved.RuntimePolicyMode = "strict_pin"
	} else if assignment.PoolID != "" {
		resolved.RuntimePolicyMode = "engine_pool"
	} else {
		resolved.RuntimePolicyMode = "auto_single"
	}
	return &resolved, assignment, nil
}

type teamSessionExecutionContextKey struct{}

type teamSessionAdmissionError struct {
	Disposition sessionexec.AcquireDisposition
	Lease       sessionexec.SessionExecutionLease
}

func (err *teamSessionAdmissionError) Error() string {
	if err == nil {
		return "team session admission failed"
	}
	return string(err.Disposition)
}

func chatAcquireEventID(c echo.Context, clientRequestID string) string {
	if clientRequestID = strings.TrimSpace(clientRequestID); clientRequestID != "" {
		return "chat:" + clientRequestID
	}
	requestID := strings.TrimSpace(c.Request().Header.Get(echo.HeaderXRequestID))
	if requestID == "" {
		requestID = strings.TrimSpace(c.Response().Header().Get(echo.HeaderXRequestID))
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}
	return "chat:" + requestID
}

func deterministicTeamRunID(key sessionexec.SessionKey, eventID string) string {
	name := strings.Join([]string{
		key.WorkspaceID, key.UserID, key.LeadAvatarID, key.SessionID, eventID,
	}, "\x1f")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

func freeCollaborationSessionSnapshot(
	runID string,
	workspaceID string,
	projectID string,
	runtimeAssignment json.RawMessage,
	teamID string,
	rec *registry.AgentRecord,
	eventID string,
	decidedAt time.Time,
	roster ...json.RawMessage,
) snapshot.TeamRunSnapshot {
	workers := json.RawMessage(`[]`)
	workerVersions := json.RawMessage(`{}`)
	if len(roster) > 0 {
		workers = roster[0]
	}
	if len(roster) > 1 {
		workerVersions = roster[1]
	}
	admission, _ := json.Marshal(map[string]any{
		"schema_version":  1,
		"team_active":     true,
		"workflow_active": nil,
		"workers_enabled": true,
		"version_blocked": nil,
		"decided_at":      decidedAt.UTC().Format(time.RFC3339Nano),
	})
	trigger, _ := json.Marshal(map[string]any{
		"schema_version": 1,
		"type":           "conversation_explicit",
		"source_ref":     eventID,
	})
	return snapshot.TeamRunSnapshot{
		RunID:                 runID,
		WorkspaceID:           workspaceID,
		ProjectID:             projectID,
		RuntimeAssignment:     append(json.RawMessage(nil), runtimeAssignment...),
		TeamID:                teamID,
		SnapshotSchemaVersion: 2,
		Mode:                  "free_collab",
		LeadAvatarID:          rec.ID,
		LeadAvatarVersion:     rec.Version,
		WorkerVersions:        workerVersions,
		TeamWorkerSnapshot:    workers,
		AdmissionDecision:     admission,
		InlineDependencies:    json.RawMessage(`{}`),
		RunAssociations: json.RawMessage(
			`{"schema_version":1,"parent_run_id":null,` +
				`"source_snapshot_id":null,"task_group_id":null}`,
		),
		TriggerSourceV2: trigger,
	}
}

func (s *Server) freezeTeamSessionRoster(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	team *registry.PublicationTeamRead,
) (json.RawMessage, json.RawMessage, error) {
	if team == nil || s.Registry == nil {
		return nil, nil, fmt.Errorf("team admission frozen descriptor registry is unavailable")
	}
	if len(team.Workers) > 0 && s.Descriptors == nil {
		return nil, nil, fmt.Errorf("team admission frozen descriptor registry is unavailable")
	}
	workers := make([]snapshot.FrozenTeamWorker, 0, len(team.Workers))
	for _, relation := range team.Workers {
		var version int
		err := tx.QueryRow(ctx, `SELECT version FROM weave_agents
			WHERE workspace_id=$1 AND id=$2 AND role='worker' AND deleted=false
			FOR SHARE`, workspaceID, relation.WorkerAgentID).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, fmt.Errorf("team worker %q frozen descriptor is unavailable", relation.WorkerAgentID)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("lock team worker %q: %w", relation.WorkerAgentID, err)
		}
		version64 := int64(version)
		record, err := s.Registry.ResolveAgentVersionTx(
			ctx, tx, workspaceID, relation.WorkerAgentID, &version64,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve team worker %q frozen descriptor: %w", relation.WorkerAgentID, err)
		}
		if record.Role != "worker" {
			return nil, nil, fmt.Errorf("team worker %q frozen descriptor role is %q", relation.WorkerAgentID, record.Role)
		}
		proof, err := s.Descriptors.DescribeWorkerRoleProof(ctx, *record, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("describe team worker %q role proof: %w", relation.WorkerAgentID, err)
		}
		displayName := record.DisplayName
		if displayName == "" {
			displayName = record.Name
		}
		workers = append(workers, snapshot.FrozenTeamWorker{
			SchemaVersion: snapshot.TeamWorkerSnapshotSchemaVersion,
			WorkerAgentID: relation.WorkerAgentID, WorkerAgentVersion: version,
			Name: displayName, Duty: relation.Duty, WhenToUse: relation.WhenToUse,
			ContextInstruction: relation.ContextInstruction,
			AllowedKinds:       append([]string(nil), relation.AllowedKinds...),
			DefaultKind:        relation.DefaultKind, ResultRequirement: relation.ResultRequirement,
			EnabledAtSnapshot: relation.Enabled,
			RoleProof: snapshot.FrozenWorkerRoleProof{
				Role: proof.Role, AgentContentHash: proof.AgentContentHash,
				CapabilitySchema:      proof.CapabilitySchema,
				CapabilityContentHash: proof.CapabilityContentHash,
			},
		})
	}
	return snapshot.EncodeTeamWorkerSnapshot(workers)
}

func (s *Server) acquireTeamSession(
	ctx context.Context,
	rec *registry.AgentRecord,
	workspaceID string,
	projectID string,
	runtimeAssignment json.RawMessage,
	userID string,
	sessionID string,
	loomSessionKey string,
	eventID string,
) (*teamSessionExecution, error) {
	if s.StoreExt == nil || s.Snapshots == nil || rec == nil || rec.ID == "" {
		return nil, nil
	}
	tx, err := s.StoreExt.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var teamID string
	err = tx.QueryRow(ctx, `SELECT id FROM weave_teams
		WHERE workspace_id=$1 AND lead_avatar_id=$2 AND status='active'`,
		workspaceID, rec.ID,
	).Scan(&teamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve team session lead: %w", err)
	}
	key := sessionexec.SessionKey{
		WorkspaceID: workspaceID, UserID: userID,
		LeadAvatarID: rec.ID, SessionID: sessionID,
	}
	runID := deterministicTeamRunID(key, eventID)
	acquired, err := sessionexec.NewPGStore().AcquireTx(
		ctx,
		tx,
		sessionexec.AcquireRequest{
			Key: key, EventID: eventID, ActiveRunID: runID, RunSnapshotID: runID,
			ProjectID:      projectID,
			ControllerKind: sessionexec.ControllerLead, ControllerID: rec.ID,
			TTL: temporarySessionExecutionTTL,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("acquire team session execution lease: %w", err)
	}
	switch acquired.Disposition {
	case sessionexec.AcquireGranted:
		var dbNow time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
			return nil, fmt.Errorf("read team session admission clock: %w", err)
		}
		workers := json.RawMessage(`[]`)
		workerVersions := json.RawMessage(`{}`)
		if !teamAssemblerDisabled() {
			team, err := s.Registry.ResolvePublicationTeamTx(ctx, tx, workspaceID, teamID)
			if err != nil {
				return nil, fmt.Errorf("freeze team session roster: %w", err)
			}
			if team.LeadAvatarID != rec.ID || int(team.LeadAvatarVersion) != rec.Version {
				return nil, fmt.Errorf("team session lead changed during admission")
			}
			workers, workerVersions, err = s.freezeTeamSessionRoster(ctx, tx, workspaceID, team)
			if err != nil {
				return nil, err
			}
		}
		snap := freeCollaborationSessionSnapshot(
			runID, workspaceID, projectID, runtimeAssignment, teamID, rec, eventID, dbNow, workers, workerVersions,
		)
		if _, err := s.Snapshots.CreateTx(ctx, tx, snap); err != nil {
			return nil, fmt.Errorf("create team session snapshot: %w", err)
		}
	case sessionexec.AcquireReplay:
		// The original granted transaction already created the immutable
		// snapshot. It is loaded after this transaction commits.
	case sessionexec.AcquireBusy, sessionexec.AcquireRouteToParked:
		return nil, &teamSessionAdmissionError{
			Disposition: acquired.Disposition, Lease: acquired.Lease,
		}
	default:
		return nil, fmt.Errorf(
			"unsupported team session acquire disposition %q", acquired.Disposition,
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit team session admission: %w", err)
	}
	snap, err := s.Snapshots.GetByRunID(ctx, workspaceID, runID)
	if err != nil {
		return nil, fmt.Errorf("load team session snapshot: %w", err)
	}
	attribution, err := terminalAttributionFromSnapshot(workspaceID, *snap, nil)
	if err != nil {
		return nil, err
	}
	execution := &teamSessionExecution{
		Lease: acquired.Lease, Snapshot: *snap, TerminalAttribution: attribution,
		AcquireEventID: eventID, LoomSessionKey: loomSessionKey,
		NextYieldGeneration: 1,
		Replayed:            acquired.Disposition == sessionexec.AcquireReplay,
		memoryOutboxReady:   make(chan struct{}),
	}
	if execution.Replayed && execution.Lease.State == sessionexec.LeaseStateClosed &&
		execution.Lease.CloseReason != nil && *execution.Lease.CloseReason == sessionexec.CloseFinalCommitted {
		readTx, beginErr := s.StoreExt.BeginTx(ctx)
		if beginErr != nil {
			return nil, beginErr
		}
		defer func() { _ = readTx.Rollback(ctx) }()
		if err := readTx.QueryRow(ctx, `SELECT content FROM weave_session_outbox
			WHERE workspace_id=$1 AND user_id=$2 AND lead_avatar_id=$3 AND session_id=$4
				AND lease_epoch=$5 AND active_run_id=$6`,
			execution.Lease.Key.WorkspaceID,
			execution.Lease.Key.UserID,
			execution.Lease.Key.LeadAvatarID,
			execution.Lease.Key.SessionID,
			execution.Lease.LeaseEpoch,
			execution.Lease.ActiveRunID,
		).Scan(&execution.ReplayOutput); err != nil {
			return nil, fmt.Errorf("load replayed team session final: %w", err)
		}
	}
	return execution, nil
}

func respondTeamSessionAdmissionError(
	c echo.Context,
	err *teamSessionAdmissionError,
) error {
	payload := map[string]any{"error": "team session is not available"}
	switch err.Disposition {
	case sessionexec.AcquireBusy:
		payload["code"] = sessionexec.ErrLeaseBusy.Error()
	case sessionexec.AcquireRouteToParked:
		payload["code"] = "session_route_to_parked"
		payload["run_id"] = err.Lease.ActiveRunID
		payload["yield_type"] = err.Lease.YieldKind
	}
	return c.JSON(http.StatusConflict, payload)
}

func teamYieldKind(state loom.State) (sessionexec.YieldKind, error) {
	switch value, _ := state["yield_type"].(string); value {
	case string(sessionexec.YieldInput):
		return sessionexec.YieldInput, nil
	default:
		return "", fmt.Errorf("unsupported team session yield type %q", value)
	}
}

func teamYieldSchema(state loom.State, kind sessionexec.YieldKind) json.RawMessage {
	for _, key := range []string{"input_schema", "resume_schema"} {
		switch raw := state[key].(type) {
		case json.RawMessage:
			if len(raw) > 0 {
				return append(json.RawMessage(nil), raw...)
			}
		case []byte:
			if len(raw) > 0 {
				return append(json.RawMessage(nil), raw...)
			}
		case string:
			if json.Valid([]byte(raw)) {
				return json.RawMessage(raw)
			}
		}
	}
	// Existing free-collaboration graphs do not yet expose wait-node schemas
	// in result state. The minimal executable envelope remains object-only;
	// full frozen assembly belongs to C-ASSEMBLER.
	return json.RawMessage(`{"type":"object"}`)
}

var errTeamSessionBlankOutput = errors.New("team session terminal output is blank")

func (s *Server) finishTeamSessionExecution(
	ctx context.Context,
	execution *teamSessionExecution,
	result loomruntime.Result,
	userMessage string,
	assistantMetadata json.RawMessage,
	runErrors ...error,
) error {
	var runErr error
	if len(runErrors) > 0 {
		runErr = runErrors[0]
	}
	if execution == nil {
		return runErr
	}
	terminalErr := runErr
	if terminalErr == nil && !result.Yielded && strings.TrimSpace(result.Output) == "" {
		terminalErr = errTeamSessionBlankOutput
	}
	if terminalErr == nil && result.Yielded {
		if _, err := teamYieldKind(result.State); err != nil {
			terminalErr = err
		} else if token, _ := result.State["__yield_token"].(string); token == "" {
			terminalErr = errors.New("team session yield token is missing")
		}
	}
	if terminalErr != nil {
		ctx = context.WithoutCancel(ctx)
		defer execution.memoryOutboxOnce.Do(func() { close(execution.memoryOutboxReady) })
	}
	tx, err := s.StoreExt.BeginTx(ctx)
	if err != nil {
		if terminalErr != nil {
			return errors.Join(terminalErr, fmt.Errorf("begin failed team session close: %w", err))
		}
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	store := sessionexec.NewPGStore()
	if terminalErr != nil {
		closed, closeErr := store.FailTx(ctx, tx, sessionexec.FailRequest{
			Key: execution.Lease.Key, LeaseEpoch: execution.Lease.LeaseEpoch,
			ActiveRunID: execution.Lease.ActiveRunID, RunSnapshotID: execution.Snapshot.RunID,
		})
		if closeErr != nil {
			return errors.Join(terminalErr, fmt.Errorf("close failed team session execution: %w", closeErr))
		}
		if err := tx.Commit(ctx); err != nil {
			return errors.Join(terminalErr, fmt.Errorf("commit failed team session close: %w", err))
		}
		execution.Lease = closed
		return terminalErr
	}
	if result.Yielded {
		kind, _ := teamYieldKind(result.State)
		token, _ := result.State["__yield_token"].(string)
		generation := execution.NextYieldGeneration
		parked, err := store.Park(ctx, tx, sessionexec.ParkRequest{
			Key: execution.Lease.Key, LeaseEpoch: execution.Lease.LeaseEpoch,
			ActiveRunID: result.RunID, YieldKind: kind,
			ResumeTokenHash: sessionexec.HashResumeToken(
				execution.Lease.Key, result.RunID, generation, token,
			),
			YieldGeneration: generation,
			InputSchema:     teamYieldSchema(result.State, kind),
			ExpiresAt:       execution.Lease.ExpiresAt,
		})
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit team session park: %w", err)
		}
		execution.Lease = parked
		execution.NextYieldGeneration = generation + 1
		return nil
	}
	var assistantFields map[string]any
	if len(assistantMetadata) > 0 {
		_ = json.Unmarshal(assistantMetadata, &assistantFields)
	}
	sessionMessages := make([]contract.Message, 0, 2)
	if userMessage != "" {
		sessionMessages = append(
			sessionMessages, contract.Message{Role: "user", Content: userMessage},
		)
	}
	sessionMessages = append(
		sessionMessages, contract.Message{Role: "assistant", Content: result.Output},
	)
	metadataFields := map[string]any{
		"schema_version":     1,
		"session_key":        execution.LoomSessionKey,
		"conversation_id":    execution.ConversationID,
		"assistant_metadata": assistantFields,
		"session_messages":   sessionMessages,
	}
	if declared := execution.DeclaredDeliverable; declared != nil && strings.TrimSpace(declared.Content) != "" {
		metadataFields["declared_deliverable"] = map[string]any{
			"title":   declared.Title,
			"content": declared.Content,
		}
	}
	metadata, err := json.Marshal(metadataFields)
	if err != nil {
		return fmt.Errorf("encode team session final metadata: %w", err)
	}
	closed, err := store.CommitFinalTx(ctx, tx, sessionexec.FinalOutboxMessage{
		EventID: "final:" + execution.AcquireEventID,
		Key:     execution.Lease.Key, LeaseEpoch: execution.Lease.LeaseEpoch,
		ActiveRunID: result.RunID, RunSnapshotID: execution.Snapshot.RunID,
		Role: "assistant", Content: result.Output, Metadata: metadata,
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit team session final outbox: %w", err)
	}
	execution.Lease = closed
	execution.memoryOutboxOnce.Do(func() { close(execution.memoryOutboxReady) })
	return nil
}

func contextWithTeamSessionExecution(
	ctx context.Context,
	execution *teamSessionExecution,
) context.Context {
	if execution == nil {
		return ctx
	}
	ctx = context.WithValue(ctx, teamSessionExecutionContextKey{}, execution)
	if execution.Lease.Key.WorkspaceID == "" {
		return ctx
	}
	return memory.ContextWithSessionExecutionLease(
		ctx,
		memory.SessionExecutionLeaseHandle{
			Key: execution.Lease.Key, LeaseEpoch: execution.Lease.LeaseEpoch,
			ActiveRunID:     execution.Lease.ActiveRunID,
			OutboxCommitted: execution.memoryOutboxReady,
		},
	)
}

func teamSessionExecutionFromContext(ctx context.Context) *teamSessionExecution {
	execution, _ := ctx.Value(teamSessionExecutionContextKey{}).(*teamSessionExecution)
	return execution
}

func (s *Server) runChatSession(
	ctx context.Context,
	tenant string,
	rec *registry.AgentRecord,
	input loomruntime.Input,
	terminalAttribution loomruntime.TerminalAttribution,
	dependencies loomruntime.Dependencies,
	teamExecution *teamSessionExecution,
) (loomruntime.Result, error) {
	if teamExecution == nil {
		return loomruntime.Run(
			ctx, tenant, rec, input, terminalAttribution, dependencies,
		)
	}
	if !teamAssemblerDisabled() && dependencies.CompileAgent == nil {
		dependencies.CompileAgent = s.teamCompileFactory(
			ctx, teamExecution, false, input.UserID, dependencies.CompileOpts.MemoryService,
		)
	}
	runID := teamExecution.Lease.ActiveRunID
	if runID == "" || teamExecution.Snapshot.RunID != runID {
		return loomruntime.Result{}, fmt.Errorf(
			"team session run identity does not match immutable snapshot",
		)
	}
	prepared, err := loomruntime.Prepare(loomruntime.RunRequest{
		Tenant: tenant,
		Agent:  rec,
		Stamp: &execution.AgentExecutionStamp{
			AgentID:        rec.ID,
			AgentVersion:   rec.Version,
			ExecutionScope: execution.ScopeTeamFreeCollab,
		},
		TerminalAttribution: &terminalAttribution,
		Dependencies:        dependencies,
	})
	if err != nil {
		return loomruntime.Result{}, fmt.Errorf("agent compilation failed: %w", err)
	}
	state := loomruntime.BuildState(tenant, rec, input)
	state["__run_id"] = runID
	return prepared.Run(ctx, state)
}

func (s *Server) handleChatRequest(c echo.Context, req ChatRequest) error {
	if req.Agent == "" || req.Message == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "agent and message are required"})
	}
	if !req.Async && !req.Stream {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "stream_required"})
	}
	tenant := getTenant(c)
	userID := getUserID(c)
	req.ConversationID = strings.TrimSpace(req.ConversationID)
	req.Intent = strings.TrimSpace(req.Intent)
	req.Channel = normalizeChatChannel(req.Channel)
	ctx := withChatChannel(c.Request().Context(), req.Channel)
	roles, _ := c.Get("roles").([]string)
	ctx = contextWithTeamForgeOperator(ctx, userID, roles)
	c.SetRequest(c.Request().WithContext(ctx))
	if s.metaTeamRunDisabled(req.Agent, req.Intent) {
		return metaTeamDisabledResponse(c)
	}
	if req.Agent == metateam.BlueprintPatchPlannerName {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "platform-internal agent"})
	}
	if req.Agent == designseed.AgentName {
		designseed.EnsureDesigner(s.Registry, tenant)
	}
	rec, err := s.Registry.Get(ctx, tenant, req.Agent)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	if req.Intent != "" && (req.Intent != conversation.IntentCreateTeam ||
		rec.Name != metateam.TeamArchitectName || req.ConversationID != "") {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_conversation_intent"})
	}
	if req.Intent == conversation.IntentCreateTeam && req.Async {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "create_team_requires_stream"})
	}
	runtimeInferenceSelected := rec.Role == "avatar" && engine.IsCLIEngine(rec.Engine)
	if rec.Role == "avatar" && !runtimeInferenceSelected {
		// Any Loom team lead may use an online Codex subscription runtime instead
		// of the server's provider catalog. This only selects its inference
		// transport; team admission and graph compilation still happen below.
		if s.Runtimes != nil {
			assignment, selectErr := s.Runtimes.Select(ctx, tenant, engine.Codex, req.RuntimeID, "")
			if selectErr == nil {
				resolved := *rec
				resolved.Engine = engine.Codex
				resolved.RuntimeID = assignment.RuntimeID
				resolved.RuntimePoolID = assignment.PoolID
				if assignment.PoolID != "" {
					resolved.RuntimePolicyMode = "engine_pool"
				} else {
					resolved.RuntimePolicyMode = "auto_single"
				}
				rec = &resolved
				runtimeInferenceSelected = true
			}
		}
	}
	if runtimeInferenceSelected && req.Async {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "loom_runtime_inference_requires_stream"})
	}
	if req.Intent == conversation.IntentCreateTeam {
		// The discovery conversation compiles Blueprints against the workspace
		// capability catalog, which reads workspace credentials. Without any
		// usable provider the run is guaranteed to fail later at Blueprint
		// validation, so the entry refuses early with an actionable error
		// instead of burning a build conversation. Mirroring stays an explicit
		// admin action; nothing is auto-mirrored here.
		if !runtimeInferenceSelected && s.Credentials == nil {
			return c.JSON(http.StatusConflict, map[string]string{
				"code":  "provider_required",
				"error": "provider_required",
				"guide": "当前工作区没有可用的模型供应商。请先在控制中心 → 模型供应商中镜像系统供应商，再创建新团队。",
			})
		}
		if !runtimeInferenceSelected {
			providers, provErr := s.Credentials.ListMetadata(ctx, tenant)
			if provErr != nil {
				return c.JSON(http.StatusInternalServerError, map[string]string{"error": provErr.Error()})
			}
			if len(providers) == 0 {
				return c.JSON(http.StatusConflict, map[string]string{
					"code":  "provider_required",
					"error": "provider_required",
					"guide": "当前工作区没有可用的模型供应商，也没有在线 Codex Runtime。请先连接 Codex Runtime，或配置模型供应商。",
				})
			}
		}
	}
	project, err := s.resolveChatProject(ctx, tenant, rec.ID, req.ProjectID)
	if err != nil {
		return respondChatProjectError(c, err)
	}
	req.ProjectID = project.ID
	attachments, err := s.resolveAttachments(ctx, tenant, req.AttachmentIDs)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	execAttachments := make([]execenv.Attachment, 0, len(attachments))
	for _, attachment := range attachments {
		execAttachments = append(execAttachments, execenv.Attachment{
			Filename: attachment.Filename,
			Path:     attachment.Path,
		})
	}

	// Async mode: enqueue a background task and return immediately.
	if req.Async {
		if s.Tasks == nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "task_queue_unavailable"})
		}
		return s.enqueueTask(c, tenant, req, attachments, rec)
	}
	if req.ConversationID != "" {
		if _, err := s.resolveRequestedChatConversation(
			ctx, tenant, req.ProjectID, rec.ID, userID, req.ConversationID, req.Channel,
		); err != nil {
			return respondChatConversationError(c, err)
		}
	}

	admitted, err := s.admitOrReplayChatRequest(c, ctx, tenant, userID, rec.ID, req)
	if err != nil || !admitted {
		return err
	}
	requestFinalized := req.ClientRequestID == ""
	defer func() {
		if !requestFinalized {
			s.finishChatRequest(ctx, tenant, userID, req.ClientRequestID, "failed", "", map[string]any{
				"project_id": req.ProjectID, "error": "execution_failed",
			})
		}
	}()
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = uuid.New().String()
		req.SessionID = sessionID // ensure SSE done event returns the generated ID
	}
	sessionKey := tenant + ":" + userID + ":" + req.Agent + ":" + sessionID
	if handled, err := s.handleCreateTeamAuthorizationGuidance(
		c, ctx, tenant, userID, req, rec, sessionKey, attachments,
	); handled || err != nil {
		requestFinalized = handled && err == nil
		return err
	}
	rec, req.RuntimeAssignment, err = s.resolveChatRuntime(ctx, tenant, req.RuntimeID, rec)
	if err != nil {
		return respondChatRuntimeError(c, err)
	}
	runtimeAssignment, err := encodeRuntimeAssignment(req.RuntimeAssignment)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "runtime_assignment_encode_failed"})
	}
	publishedWorkflowID, err := s.publishedConversationWorkflowForLead(ctx, tenant, rec.ID)
	if err != nil {
		if errors.Is(err, errPublishedConversationWorkflowAmbiguous) {
			return c.JSON(http.StatusConflict, map[string]string{"error": "published_conversation_workflow_ambiguous"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "published_conversation_workflow_resolution_failed"})
	}

	// 1. Load session history.
	var teamExecution *teamSessionExecution
	if publishedWorkflowID == "" {
		teamExecution, err = s.acquireTeamSession(
			ctx, rec, tenant, req.ProjectID, runtimeAssignment, userID, sessionID, sessionKey, chatAcquireEventID(c, req.ClientRequestID),
		)
	}
	if err != nil {
		var admissionErr *teamSessionAdmissionError
		if errors.As(err, &admissionErr) {
			return respondTeamSessionAdmissionError(c, admissionErr)
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if runtimeInferenceTeamGraphUnavailable(runtimeInferenceSelected, publishedWorkflowID, teamExecution) {
		return c.JSON(http.StatusConflict, map[string]string{"error": "loom_team_graph_unavailable"})
	}
	if teamExecution != nil {
		var frozenAssignment *runtimes.Assignment
		rec, frozenAssignment, err = s.applySnapshotRuntimeAssignment(ctx, tenant, rec, teamExecution)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "runtime_assignment_snapshot_invalid"})
		}
		req.RuntimeAssignment = frozenAssignment
	}
	if teamExecution != nil && teamExecution.Replayed {
		switch teamExecution.Lease.State {
		case sessionexec.LeaseStateActive:
			return c.JSON(http.StatusConflict, map[string]any{
				"error":  "team session request is still running",
				"code":   "session_acquire_replay_in_progress",
				"run_id": teamExecution.Lease.ActiveRunID,
			})
		case sessionexec.LeaseStateParked:
			return c.JSON(http.StatusConflict, map[string]any{
				"error":      "team session is waiting for resume input",
				"code":       "session_route_to_parked",
				"run_id":     teamExecution.Lease.ActiveRunID,
				"yield_type": teamExecution.Lease.YieldKind,
			})
		case sessionexec.LeaseStateClosed:
			if teamExecution.Lease.CloseReason != nil && *teamExecution.Lease.CloseReason == sessionexec.CloseFailed {
				return c.JSON(http.StatusConflict, map[string]any{
					"error":  "team session execution failed",
					"code":   "session_execution_failed",
					"run_id": teamExecution.Lease.ActiveRunID,
				})
			}
			if teamExecution.Lease.CloseReason == nil || *teamExecution.Lease.CloseReason != sessionexec.CloseFinalCommitted {
				return c.JSON(http.StatusConflict, map[string]any{
					"error":  "team session has an invalid terminal state",
					"code":   "session_terminal_state_invalid",
					"run_id": teamExecution.Lease.ActiveRunID,
				})
			}
			resp := ChatResponse{
				Output: teamExecution.ReplayOutput, StopReason: "completed",
				SessionID: sessionID, RunID: teamExecution.Lease.ActiveRunID,
				ProjectID:         req.ProjectID,
				RuntimeAssignment: req.RuntimeAssignment,
				ConversationID:    req.ConversationID,
			}
			s.finishChatRequest(
				ctx, tenant, userID, req.ClientRequestID, "completed",
				teamExecution.Lease.ActiveRunID, resp,
			)
			requestFinalized = true
			return c.JSON(http.StatusOK, resp)
		}
	}
	ctx = contextWithTeamSessionExecution(ctx, teamExecution)
	c.SetRequest(c.Request().WithContext(ctx))
	conversationID, userMessageID, err := s.recordIncomingProductMessage(
		ctx, tenant, req.ProjectID, rec.ID, userID, sessionKey, req.Message,
		incomingProductMessageOptions{
			ConversationID: req.ConversationID,
			Intent:         req.Intent,
			TeamExecution:  teamExecution,
			Attachments:    attachments,
		},
	)
	if err != nil {
		if req.ConversationID != "" {
			return respondChatConversationError(c, err)
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if req.ClientRequestID != "" {
		if _, err := s.ChatRequests.MarkAdmitted(
			ctx, tenant, userID, req.ClientRequestID, sessionID, conversationID, userMessageID,
		); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "chat_request_admission_failed"})
		}
		if _, err := s.ChatRequests.MarkRunning(ctx, tenant, userID, req.ClientRequestID); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "chat_request_start_failed"})
		}
	}
	if teamExecution != nil {
		teamExecution.ConversationID = conversationID
	}
	var teamRunErr error

	var msgs []contract.Message
	if loaded, err := stdlib.LoadSession(s.Store, sessionKey); err != nil {
		// Session not found is normal (new session) — only log unexpected errors.
		c.Logger().Debugf("session load (key=%s): %v", sessionKey, err)
	} else {
		msgs = loaded
	}

	// 3. Append user message and persist it before running. Persisting the
	// user turn at admission decouples it from run success: a failed or
	// interrupted stream still leaves the turn in session history, so the
	// next message in the same session keeps full context.
	userMessage := contract.Message{Role: "user", Content: injectAttachmentNotice(req.Message, attachments)}
	msgs = append(msgs, userMessage)
	_ = s.appendSessionMessages(ctx, sessionKey, userMessage)

	// Save session title from first user message.

	if publishedWorkflowID != "" {
		requestFinalized = true
		return s.respondPublishedWorkflowChat(
			c, req, rec, tenant, userID, sessionKey, conversationID, userMessageID, publishedWorkflowID, msgs,
		)
	}

	var agentTool *mcphost.AgentToolDispatcher
	var fanoutTool *FanoutToolDispatcher
	var output, stopReason, runID, yieldType, errMsg string
	var usage contract.Usage
	var resultState loom.State
	var runtimeResult loomruntime.Result
	if engine.IsCLIEngine(rec.Engine) && teamExecution == nil {
		output, err = s.runEngineChat(ctx, tenant, rec, req.Message, attachments)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		stopReason = "completed"
		if teamExecution != nil {
			runID = teamExecution.Lease.ActiveRunID
			runtimeResult = loomruntime.Result{
				Output: output, StopReason: loom.StopCompleted,
				RunID: runID, State: loom.State{"output": output},
			}
		}
	} else {
		// 4. Resolve the inference transport, then compile agent → Graph. An
		// online Codex runtime may replace only contract.LLM; Loom still owns the
		// team graph and every tool dispatch.
		llm, llmErr := s.llmForLoomNode(ctx, tenant, rec, teamExecution)
		if llmErr != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": llmErr.Error()})
		}
		memSvc := s.memoryFor(ctx, tenant)
		tools, builtAgentTool, builtFanoutTool := s.buildToolDispatcherWithAgentTool(
			rec, tenant, userID, conversationID, llm, memSvc, false, execAttachments, ctx,
		)
		agentTool = builtAgentTool
		fanoutTool = builtFanoutTool
		effort := contract.EffortLevel(req.Effort)
		if effort == "" {
			effort = contract.EffortMedium
		}

		// 5. Handle streaming vs non-streaming.
		if req.Stream {
			streamErr := s.handleChatStream(
				c, tenant, rec, msgs, req, sessionKey, conversationID, userMessageID,
				tools, agentTool, fanoutTool, effort, llm, teamExecution,
			)
			requestFinalized = true
			return streamErr
		}

		compileContext := s.contextWithOwnerProfile(ctx, tenant, userID, rec, req.Context)
		compileOpts := compiler.CompileOpts{
			Profile:              req.Profile,
			Context:              compileContext,
			Effort:               effort,
			Store:                s.Store,
			SubAgentStepResolver: s.resolveSubAgent,
			AgentRunner:          s.compilerAgentRunner(tenant, userID, llm, memSvc, execAttachments),
		}
		// Enable memory + semantic skill matching if embedder is available.
		if memSvc != nil {
			compileOpts.Embedder = memSvc.Embedder() // for SemanticMatcher
			if cfg := registry.EffectiveMemoryConfig(rec); cfg.Enabled {
				compileOpts.MemoryService = memSvc
				if cfg.TopK > 0 {
					compileOpts.MemoryTopK = cfg.TopK
				}
				compileOpts.AutoRemember = cfg.AutoRemember
				compileOpts.MemoryScope = cfg.Scope
			}
		}
		configureProjectMemory(&compileOpts, tenant, rec, req.ProjectID)
		// 6-8. Compile, run, extract — via the shared runner (same core the
		// jobs path uses and the runtime daemon will use). LLM is the
		// per-workspace snapshot resolved above, not a process global.
		var terminalAttribution loomruntime.TerminalAttribution
		if teamExecution != nil {
			terminalAttribution = teamExecution.TerminalAttribution
		} else {
			terminalAttribution, err = legacyRootTerminalAttribution(tenant)
			if err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
			}
		}
		terminalSink, sinkErr := s.rootTerminalSink()
		if sinkErr != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": sinkErr.Error()})
		}
		result, runErr := s.runChatSession(ctx, tenant, rec, loomruntime.Input{
			Messages:        msgs,
			LastUserMessage: req.Message,
			SessionID:       sessionID,
			UserID:          userID,
			Profile:         req.Profile,
			Context:         compileContext,
		}, terminalAttribution, loomruntime.Dependencies{
			LLM:                llm,
			Tools:              tools,
			Store:              s.Store,
			TerminalSink:       terminalSink,
			LifecycleHook:      s.RunLifecycleHook,
			SkillVersionReader: s.Skills,
			CompileOpts:        compileOpts,
		}, teamExecution)
		teamRunErr = runErr
		if runErr != nil && !result.Ran() && teamExecution == nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": runErr.Error()})
		}
		runtimeResult = result
		output = result.Output
		usage = result.Usage
		resultState = result.State
		stopReason = string(result.StopReason)
		runID = result.RunID
		if result.Yielded {
			yieldType, _ = resultState["yield_type"].(string)
		}

		// If the graph returned an error alongside a partial result, surface it.
		if runErr != nil {
			errMsg = runErr.Error()
			c.Logger().Errorf("graph.Run error (agent=%s): %v", req.Agent, runErr)
		}
	}

	if teamExecution != nil {
		metadataCtx := contextWithAssistantAgent(ctx, req.Agent)
		metadata := s.buildAssistantMetadata(metadataCtx, tenant, output)
		metadata = mergeRuntimeAssignmentMetadata(metadata, req.RuntimeAssignment)
		if err := s.finishTeamSessionExecution(
			ctx, teamExecution, runtimeResult, injectAttachmentNotice(req.Message, attachments), metadata, teamRunErr,
		); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}

	// 9. Persist this turn. The user turn was already stored at admission, so
	// append only the assistant reply under an atomic lock so an async
	// dispatch reflow appending its report to the same session key
	// concurrently cannot be lost to a whole-blob rewrite.
	if teamExecution == nil && output != "" {
		_ = s.appendSessionMessages(ctx, sessionKey,
			contract.Message{Role: "assistant", Content: output})
	} else if teamExecution == nil {
		if resultMsgs, err := stdlib.GetMessages(resultState); err == nil && len(resultMsgs) > 0 {
			_ = stdlib.SaveSession(s.Store, sessionKey, resultMsgs)
		}
	}
	if teamExecution == nil && output != "" && conversationID != "" {
		metadataCtx := contextWithAssistantAgent(ctx, req.Agent)
		metadata := s.buildAssistantMetadata(metadataCtx, tenant, output)
		metadata = mergeRuntimeAssignmentMetadata(metadata, req.RuntimeAssignment)
		if s.TeamBuild != nil {
			if run, runErr := s.TeamBuild.GetActiveBuildRunByConversation(ctx, tenant, conversationID); runErr == nil {
				if token, ok, tokenErr := s.currentBlueprintRevisionToken(ctx, tenant, run.BuildRunID); tokenErr == nil && ok {
					fields := s.blueprintAuthorizationMetadata(ctx, tenant, run.BuildRunID, *token)
					if merged, mergeErr := mergeDiscoveryMetadata(metadata, fields); mergeErr == nil {
						metadata = merged
					}
				}
			}
		}
		if _, err := s.Conversations.AppendMessage(ctx, conversation.Message{
			ConversationID: conversationID,
			WorkspaceID:    tenant,
			Role:           "assistant",
			Content:        output,
			Metadata:       metadata,
		}); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}
	s.startOwnerMemoryFill(tenant, rec, userID, conversationID, req.Message, output, false)

	resp := ChatResponse{
		Output:            output,
		StopReason:        stopReason,
		Usage:             usage,
		SessionID:         sessionID,
		RunID:             runID,
		ProjectID:         req.ProjectID,
		ConversationID:    conversationID,
		UserMessageID:     userMessageID,
		YieldType:         yieldType,
		RuntimeAssignment: req.RuntimeAssignment,
	}
	resp.GroundingWarning = s.dispatchGroundingWarning(
		ctx, tenant, rec, output, teamExecution, agentTool, fanoutTool,
	)
	if errMsg != "" && output == "" {
		resp.Output = "Error: " + errMsg
	}
	status := "completed"
	if yieldType != "" {
		status = "yielded"
	} else if errMsg != "" {
		status = "failed"
	}
	s.finishChatRequest(ctx, tenant, userID, req.ClientRequestID, status, runID, resp)
	requestFinalized = true
	if req.Stream {
		sse, err := NewSSEWriter(c)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		if resp.Output != "" {
			_ = sse.SendEvent("chunk", map[string]any{"agent": rec.Name, "content": resp.Output})
		}
		_ = sse.SendEvent("done", resp)
		return nil
	}
	return c.JSON(http.StatusOK, resp)
}

func (s *Server) dispatchGroundingWarning(
	ctx context.Context,
	tenant string,
	rec *registry.AgentRecord,
	output string,
	teamExecution *teamSessionExecution,
	agentTool *mcphost.AgentToolDispatcher,
	fanoutTool *FanoutToolDispatcher,
) string {
	delegateDispatches := int64(0)
	if agentTool != nil {
		delegateDispatches = agentTool.DispatchedCount()
	}
	parallelDispatches := int64(0)
	if fanoutTool != nil {
		parallelDispatches = fanoutTool.DispatchedCount()
	}
	if delegateDispatches+parallelDispatches != 0 {
		return ""
	}
	var names []string
	if teamExecution != nil && !teamAssemblerDisabled() {
		workers, err := snapshot.DecodeTeamWorkerSnapshot(teamExecution.Snapshot.TeamWorkerSnapshot)
		if err != nil {
			return ""
		}
		names = make([]string, 0, len(workers)*2)
		for _, worker := range workers {
			names = append(names, worker.WorkerAgentID, worker.Name)
		}
	} else {
		managed, err := s.Registry.ListManaged(ctx, tenant, rec.Name)
		if err != nil {
			return ""
		}
		names = make([]string, 0, len(managed)*2)
		for _, worker := range managed {
			names = append(names, worker.Name, worker.DisplayName)
		}
	}
	if !grounding.ClaimsDispatch(output, names) {
		return ""
	}
	slog.Warn("dispatch claim unbacked", "agent", rec.Name)
	return "dispatch_claim_unbacked"
}

func (s *Server) handleCreateTeamAuthorizationGuidance(
	c echo.Context,
	ctx context.Context,
	workspaceID, userID string,
	req ChatRequest,
	rec *registry.AgentRecord,
	sessionKey string,
	attachments []taskqueue.Attachment,
) (bool, error) {
	if rec == nil || rec.Name != metateam.TeamArchitectName || strings.TrimSpace(req.ConversationID) == "" {
		return false, nil
	}
	if s.Conversations == nil || s.TeamBuild == nil {
		return false, nil
	}
	intent, err := getConversationIntent(ctx, s.Conversations, workspaceID, req.ConversationID)
	if err != nil || intent != conversation.IntentCreateTeam {
		return false, nil
	}
	run, err := s.TeamBuild.GetActiveBuildRunByConversation(ctx, workspaceID, req.ConversationID)
	if err != nil {
		return false, nil
	}
	if run.Status != teambuild.StatusPlanning {
		return false, nil
	}
	if req.BlueprintChangeRequested || isCreateTeamBlueprintChangeIntent(req.Message) {
		if _, ok, tokenErr := s.currentBlueprintRevisionToken(ctx, workspaceID, run.BuildRunID); tokenErr != nil {
			return true, c.JSON(http.StatusInternalServerError, map[string]string{"error": "blueprint_revision_read_failed"})
		} else if !ok {
			return false, nil
		}
		if _, transitionErr := s.TeamBuild.TransitionStatus(
			ctx, workspaceID, run.BuildRunID,
			teambuild.StatusPlanning, teambuild.StatusBlocked,
			userID, reasonBlueprintChangeRequested,
		); transitionErr != nil {
			return true, c.JSON(http.StatusConflict, map[string]string{"error": "blueprint_change_request_failed"})
		}
		return false, nil
	}
	if !isCreateTeamContinuationIntent(req.Message) {
		return false, nil
	}
	return s.respondCreateTeamAuthorizationClarification(
		c, ctx, workspaceID, userID, req, rec, sessionKey, attachments, run,
	)
}

func (s *Server) respondCreateTeamAuthorizationClarification(
	c echo.Context,
	ctx context.Context,
	workspaceID, userID string,
	req ChatRequest,
	rec *registry.AgentRecord,
	sessionKey string,
	attachments []taskqueue.Attachment,
	run teambuild.TeamBuildRun,
) (bool, error) {
	conversationID, userMessageID, err := s.recordIncomingProductMessage(
		ctx, workspaceID, req.ProjectID, rec.ID, userID, sessionKey, req.Message,
		incomingProductMessageOptions{
			ConversationID: req.ConversationID,
			Attachments:    attachments,
		},
	)
	if err != nil {
		return true, respondChatConversationError(c, err)
	}
	if req.ClientRequestID != "" && s.ChatRequests != nil {
		if _, err := s.ChatRequests.MarkAdmitted(ctx, workspaceID, userID, req.ClientRequestID, req.SessionID, conversationID, userMessageID); err != nil {
			return true, c.JSON(http.StatusInternalServerError, map[string]string{"error": "chat_request_admission_failed"})
		}
		if _, err := s.ChatRequests.MarkRunning(ctx, workspaceID, userID, req.ClientRequestID); err != nil {
			return true, c.JSON(http.StatusInternalServerError, map[string]string{"error": "chat_request_start_failed"})
		}
	}
	userMessage := contract.Message{Role: "user", Content: injectAttachmentNotice(req.Message, attachments)}
	_ = s.appendSessionMessages(ctx, sessionKey, userMessage)

	output := "团队方案已生成。请点击方案卡片上的「继续构建」开始；如需调整，直接说明修改意见。"
	_ = s.appendSessionMessages(ctx, sessionKey, contract.Message{Role: "assistant", Content: output})
	metadataCtx := contextWithAssistantAgent(ctx, req.Agent)
	metadata := s.buildAssistantMetadata(metadataCtx, workspaceID, output)
	metadata = mergeRuntimeAssignmentMetadata(metadata, req.RuntimeAssignment)
	if _, err := s.Conversations.AppendMessage(ctx, conversation.Message{
		ConversationID: conversationID,
		WorkspaceID:    workspaceID,
		Role:           "assistant",
		Content:        output,
		Metadata:       metadata,
	}); err != nil {
		return true, c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	s.startOwnerMemoryFill(workspaceID, rec, userID, conversationID, req.Message, output, false)

	resp := ChatResponse{
		Output:         output,
		StopReason:     "completed",
		SessionID:      req.SessionID,
		ProjectID:      req.ProjectID,
		ConversationID: conversationID,
		UserMessageID:  userMessageID,
	}
	s.finishChatRequest(ctx, workspaceID, userID, req.ClientRequestID, "completed", "", resp)
	if req.Stream {
		sse, err := NewSSEWriter(c)
		if err != nil {
			return true, err
		}
		_ = sse.SendEvent("chunk", map[string]any{"agent": rec.Name, "content": output})
		donePayload := map[string]any{
			"output":          output,
			"stop_reason":     "completed",
			"session_id":      req.SessionID,
			"project_id":      req.ProjectID,
			"conversation_id": conversationID,
			"user_message_id": userMessageID,
			"build_run_id":    run.BuildRunID,
		}
		addBusinessPhaseFields(donePayload, businessPhasePlanningCreated)
		_ = sse.SendEvent("done", donePayload)
		return true, nil
	}
	return true, c.JSON(http.StatusOK, resp)
}

func isCreateTeamContinuationIntent(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	normalized = strings.Trim(normalized, " \t\r\n。.!！?？")
	if normalized == "" {
		return false
	}
	blockers := []string{
		"不批准", "拒绝", "先别", "别执行", "不要执行", "暂停", "等一下", "等等",
		"修改", "调整", "改成", "改为", "再改", "但是", "不过", "但 ",
		"do not approve", "don't approve", "reject", "wait",
	}
	for _, blocker := range blockers {
		if strings.Contains(normalized, blocker) {
			return false
		}
	}
	exactSignals := map[string]bool{
		"授权方案": true, "授权并执行": true, "同意方案": true, "确认授权": true,
		"开始构建": true, "执行构建": true, "按方案执行": true, "按这个方案执行": true,
		"授权": true, "可以": true, "可以吗": true, "好": true, "好的": true,
		"没问题": true, "继续": true, "继续吗": true, "开始": true, "执行": true,
		"authorize": true, "authorized": true, "authorize blueprint": true,
		"ok": true, "okay": true, "go ahead": true,
	}
	if exactSignals[normalized] {
		return true
	}
	continuationSignals := []string{
		"我已阅读并批准", "批准这个", "批准该", "按这个方案执行", "按方案执行",
		"开始构建", "继续执行", "可以继续", "同意这个方案", "同意该方案",
	}
	for _, signal := range continuationSignals {
		if strings.Contains(normalized, signal) {
			return true
		}
	}
	return false
}

func isCreateTeamBlueprintChangeIntent(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return false
	}
	for _, signal := range []string{
		"请修改方案", "修改方案", "调整方案", "重新规划", "改一下方案", "改下方案",
		"revise the plan", "change the plan", "modify the plan", "replan",
	} {
		if strings.Contains(normalized, signal) {
			return true
		}
	}
	return false
}

var (
	errChatConversationsUnavailable    = errors.New("chat conversations unavailable")
	errChatConversationNotFound        = errors.New("chat conversation not found")
	errChatConversationReadOnly        = errors.New("chat conversation read only")
	errChatConversationAgentMismatch   = errors.New("chat conversation agent mismatch")
	errChatConversationProjectMismatch = errors.New("chat conversation project mismatch")
	errChatConversationChannelMismatch = errors.New("chat conversation channel mismatch")
)

type incomingProductMessageOptions struct {
	ConversationID string
	Intent         string
	TeamExecution  *teamSessionExecution
	Attachments    []taskqueue.Attachment
}

func normalizeChatChannel(channel string) string {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return "default"
	}
	return channel
}

func (s *Server) resolveRequestedChatConversation(
	ctx context.Context,
	workspaceID, projectID, agentID, userID, conversationID, channel string,
) (conversation.Conversation, error) {
	if s.Conversations == nil {
		return conversation.Conversation{}, errChatConversationsUnavailable
	}
	conv, err := s.Conversations.GetConversation(ctx, workspaceID, conversationID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return conversation.Conversation{}, fmt.Errorf("%w: %s", errChatConversationNotFound, conversationID)
		}
		return conversation.Conversation{}, err
	}
	if err := validateChatConversationParticipant(conv, workspaceID, agentID, userID); err != nil {
		if conv.WorkspaceID != workspaceID {
			return conversation.Conversation{}, fmt.Errorf("%w: %s", errChatConversationNotFound, conversationID)
		}
		if conv.UserID != userID {
			return conversation.Conversation{}, fmt.Errorf("%w: %s", errChatConversationReadOnly, conversationID)
		}
		return conversation.Conversation{}, fmt.Errorf("%w: %s", errChatConversationAgentMismatch, conversationID)
	}
	if conv.ProjectID != projectID {
		return conversation.Conversation{}, fmt.Errorf("%w: %s", errChatConversationProjectMismatch, conversationID)
	}
	if normalizeChatChannel(conv.Channel) != normalizeChatChannel(channel) {
		return conversation.Conversation{}, fmt.Errorf("%w: %s", errChatConversationChannelMismatch, conversationID)
	}
	return conv, nil
}

func respondChatConversationError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, errChatConversationsUnavailable):
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "conversations_unavailable"})
	case errors.Is(err, errChatConversationNotFound):
		return c.JSON(http.StatusNotFound, map[string]string{"error": "conversation_not_found"})
	case errors.Is(err, errChatConversationReadOnly):
		return c.JSON(http.StatusForbidden, map[string]string{"error": "conversation_read_only"})
	case errors.Is(err, errChatConversationAgentMismatch):
		return c.JSON(http.StatusConflict, map[string]string{"error": "conversation_agent_mismatch"})
	case errors.Is(err, errChatConversationProjectMismatch):
		return c.JSON(http.StatusConflict, map[string]string{"error": "conversation_project_mismatch"})
	case errors.Is(err, errChatConversationChannelMismatch):
		return c.JSON(http.StatusConflict, map[string]string{"error": "conversation_channel_mismatch"})
	default:
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "conversation_resolution_failed"})
	}
}

func (s *Server) recordIncomingProductMessage(
	ctx context.Context,
	workspaceID, projectID, agentID, userID, sessionKey, content string,
	options ...incomingProductMessageOptions,
) (string, string, error) {
	if s.Conversations == nil {
		return "", "", nil
	}
	var option incomingProductMessageOptions
	if len(options) > 0 {
		option = options[0]
	}
	var conv conversation.Conversation
	var err error
	if option.ConversationID != "" {
		conv, err = s.resolveRequestedChatConversation(
			ctx, workspaceID, projectID, agentID, userID, option.ConversationID, chatChannelFromContext(ctx),
		)
	} else if projectID == "" {
		conv, err = s.Conversations.EnsureConversationChannel(
			ctx, workspaceID, agentID, userID, chatChannelFromContext(ctx),
		)
	} else {
		conv, err = s.Conversations.CreateProjectConversationChannel(
			ctx, workspaceID, projectID, agentID, userID, chatChannelFromContext(ctx), sessionKey,
			option.Intent,
		)
	}
	if err != nil {
		return "", "", err
	}
	if conv.SessionKey == "" {
		if _, err := s.Conversations.BindSessionKey(ctx, workspaceID, conv.ID, sessionKey); err != nil {
			return "", "", err
		}
	}
	metadata, err := encodeUserAttachmentMetadata(option.Attachments)
	if err != nil {
		return "", "", fmt.Errorf("encode user attachment metadata: %w", err)
	}
	message := conversation.Message{
		ConversationID: conv.ID,
		WorkspaceID:    workspaceID,
		Role:           "user",
		Content:        content,
		Metadata:       metadata,
	}
	if option.TeamExecution != nil {
		message.EventID = "user:" + option.TeamExecution.AcquireEventID
		message.LeaseEpoch = option.TeamExecution.Lease.LeaseEpoch
	}
	created, err := s.Conversations.AppendMessage(ctx, message)
	if err != nil {
		return "", "", err
	}
	return conv.ID, created.ID, nil
}

var errChatProjectsUnavailable = errors.New("projects are not configured")

func (s *Server) resolveChatProject(
	ctx context.Context,
	workspaceID, agentID, projectID string,
) (projects.Project, error) {
	if s.Projects == nil {
		return projects.Project{}, errChatProjectsUnavailable
	}
	projectID = strings.TrimSpace(projectID)
	var project projects.Project
	var err error
	if projectID == "" {
		project, err = s.Projects.EnsureUnclassified(ctx, workspaceID, agentID)
	} else {
		project, err = s.Projects.GetActive(ctx, workspaceID, projectID)
	}
	if err != nil {
		return projects.Project{}, err
	}
	if project.AvatarID != agentID {
		return projects.Project{}, conversation.ErrProjectAgentMismatch
	}
	return project, nil
}

func respondChatProjectError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, errChatProjectsUnavailable):
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "projects_unavailable"})
	case errors.Is(err, projects.ErrNotFound):
		return c.JSON(http.StatusNotFound, map[string]string{"error": "project_not_found"})
	case errors.Is(err, projects.ErrArchived):
		return c.JSON(http.StatusConflict, map[string]string{"error": "project_archived"})
	case errors.Is(err, conversation.ErrProjectAgentMismatch):
		return c.JSON(http.StatusConflict, map[string]string{"error": "project_avatar_mismatch"})
	case errors.Is(err, projects.ErrInvalidAvatar):
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": "invalid_project_avatar"})
	default:
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "project_resolution_failed"})
	}
}

type chatChannelContextKey struct{}

func withChatChannel(ctx context.Context, channel string) context.Context {
	return context.WithValue(ctx, chatChannelContextKey{}, channel)
}

func chatChannelFromContext(ctx context.Context) string {
	channel, _ := ctx.Value(chatChannelContextKey{}).(string)
	return channel
}

// buildToolDispatcher creates a ToolDispatcher from the agent's MCP server config
// plus agent-as-tool capabilities. Managed agents are exposed as callable tools
// (depth-limited to 1 — inner agents don't get agent-as-tool).
func (s *Server) buildToolDispatcher(
	rec *registry.AgentRecord,
	tenant, userID, conversationID string,
	llm contract.LLM,
	memSvc *memory.Service,
	noDispatch bool,
	contexts ...context.Context,
) contract.ToolDispatcher {
	dispatcher, _, _ := s.buildToolDispatcherWithAgentTool(
		rec, tenant, userID, conversationID, llm, memSvc, noDispatch, nil, contexts...,
	)
	return dispatcher
}

func (s *Server) compilerAgentRunner(
	tenant, userID string,
	llm contract.LLM,
	memSvc *memory.Service,
	attachments []execenv.Attachment,
) compiler.AgentRunner {
	var workspacesRoot, oneapiBase, boundaryBase, oneapiKey string
	if s.Config != nil {
		workspacesRoot = s.Config.WorkspacesRoot
		oneapiBase = s.Config.OneAPIBase
		boundaryBase = s.Config.MCPBoundaryBase
		oneapiKey = s.Config.OneAPIKey
	}
	broker := mcphost.NewToolBroker(s.mcpAccessFactory())
	runner := mcphost.NewAgentRunner(
		s.Registry, tenant, llm, s.Store, memSvc,
		workspacesRoot, oneapiBase, boundaryBase, oneapiKey, attachments,
	)
	runner.Broker = broker
	runner.LocalExec = s.engineExecutorFor(false)
	runner.RemoteExec = s.engineExecutorFor(true)
	runner.RunLifecycleHook = s.RunLifecycleHook
	runner.SkillVersionReader = s.Skills
	if s.Fanout != nil && s.Tasks != nil {
		runner.InnerPlatformTools = func(innerRec *registry.AgentRecord) []contract.ToolDispatcher {
			return []contract.ToolDispatcher{s.subAgentTaskStatusDispatcher(tenant, innerRec.Name, userID)}
		}
	}
	return mcphost.NewCompilerAgentRunner(runner)
}

func (s *Server) buildToolDispatcherWithAgentTool(
	rec *registry.AgentRecord,
	tenant, userID, conversationID string,
	llm contract.LLM,
	memSvc *memory.Service,
	noDispatch bool,
	attachments []execenv.Attachment,
	contexts ...context.Context,
) (contract.ToolDispatcher, *mcphost.AgentToolDispatcher, *FanoutToolDispatcher) {
	var agentTool *mcphost.AgentToolDispatcher
	var fanoutTool *FanoutToolDispatcher
	broker := mcphost.NewToolBroker(s.mcpAccessFactory())

	buildCtx := context.Background()
	if len(contexts) > 0 && contexts[0] != nil {
		buildCtx = contexts[0]
	}
	teamExecution := teamSessionExecutionFromContext(buildCtx)
	// declareTool lazily creates the turn's deliverable declaration buffer on
	// the execution so finishTeamSessionExecution can project it after the run.
	declareTool := func() *mcphost.DeliverableDeclareDispatcher {
		if teamExecution.DeclaredDeliverable == nil {
			teamExecution.DeclaredDeliverable = &mcphost.DeclaredDeliverable{}
		}
		return mcphost.NewDeliverableDeclareDispatcher(teamExecution.DeclaredDeliverable)
	}
	dispatcher := broker.Build(buildCtx, mcphost.ToolBrokerRequest{
		WorkspaceID:    tenant,
		Agent:          rec,
		ConversationID: conversationID,
		LLM:            llm,
		Memory:         memSvc,
		PlatformTools: func(runtimeLLM contract.LLM, runtimeMemory *memory.Service) []contract.ToolDispatcher {
			if teamExecution != nil && !teamAssemblerDisabled() {
				workers, err := snapshotTeamWorkers(teamExecution.Snapshot)
				if err != nil {
					return []contract.ToolDispatcher{mcphost.NewRejectedMCPDispatcher(err)}
				}
				dispatchers := []contract.ToolDispatcher{&teamCatalogDownstream{runner: &apiLockedWorkerRunner{
					server: s, snapshot: teamExecution.Snapshot, workers: workers,
					llm: runtimeLLM, memory: runtimeMemory, userID: userID,
					conversationID: conversationID,
				}}, declareTool()}
				// The meta-team lead (团队架构师) keeps its teamforge read
				// tools on the legacy team-session topology as well, so the
				// discovery phase still works when the assembler is enabled.
				dispatchers = append(
					dispatchers,
					s.teamForgePlatformTools(buildCtx, tenant, conversationID, rec.Name)...,
				)
				return dispatchers
			}
			if noDispatch {
				return nil
			}
			// Agent-as-tool: expose managed agents as callable tools.
			var workspacesRoot, oneapiBase, boundaryBase, oneapiKey string
			if s.Config != nil {
				workspacesRoot = s.Config.WorkspacesRoot
				oneapiBase = s.Config.OneAPIBase
				boundaryBase = s.Config.MCPBoundaryBase
				oneapiKey = s.Config.OneAPIKey
			}
			agentTool = mcphost.NewAgentToolDispatcher(
				s.Registry, tenant, rec.Name, runtimeLLM, s.Store, runtimeMemory,
				s.dispatchRecorder(),
				workspacesRoot, oneapiBase, boundaryBase, oneapiKey,
				attachments,
			)
			agentTool.Broker = broker
			agentTool.RunLifecycleHook = s.RunLifecycleHook
			agentTool.LocalExec = s.engineExecutorFor(false)
			agentTool.RemoteExec = s.engineExecutorFor(true)
			if s.Fanout != nil && s.Tasks != nil {
				agentTool.InnerPlatformTools = func(innerRec *registry.AgentRecord) []contract.ToolDispatcher {
					return []contract.ToolDispatcher{s.subAgentTaskStatusDispatcher(tenant, innerRec.Name, userID)}
				}
			}
			platform := []contract.ToolDispatcher{agentTool}
			// The lead agent always has the declare tool in team sessions, also
			// on the legacy (assembler-disabled) topology.
			if teamExecution != nil {
				platform = append(platform, declareTool())
			}
			// Meta-team employees receive their role- and run-state-scoped
			// teamforge tools (T08). Non-meta-team agents get nothing here, so
			// existing PlatformTools behavior is unchanged.
			platform = append(
				platform,
				s.teamForgePlatformTools(buildCtx, tenant, conversationID, rec.Name)...,
			)
			// All chat/SSE/resume/jobs entry points share this builder. Keep the
			// transfer tool on the same final topology predicate as compilation.
			if compiler.HasSubAgents(rec, true) {
				if routes, err := compiler.BuildSubAgentRouteTable(rec.SubAgents); err == nil {
					platform = append(platform, mcphost.NewTransferDispatcher(rec.Name, routes))
				}
			}
			if s.Fanout != nil && s.Tasks != nil {
				fanoutTool = NewFanoutToolDispatcher(
					s.Registry, s.Fanout, s.Tasks, s.Conversations,
					tenant, rec.Name, userID, conversationID, nil,
				)
				fanoutTool.rules = s.OrgStore
				platform = append(platform, fanoutTool)
				platform = append(platform, s.gatedTaskStatusDispatcher(tenant, rec.Name, userID, conversationID))
			}
			return platform
		},
	})
	return dispatcher, agentTool, fanoutTool
}

func (s *Server) buildAuditedMCPDispatcher(
	rec *registry.AgentRecord,
	tenant, conversationID string,
) contract.ToolDispatcher {
	dispatcher, err := s.mcpAccessFactory().Build(
		context.Background(), tenant, rec, conversationID,
	)
	if err != nil {
		return mcphost.NewRejectedMCPDispatcher(err)
	}
	return dispatcher
}

func (s *Server) mcpAccessFactory() *mcphost.MCPAccessFactory {
	var auditRecorder mcphost.GovernanceAuditRecorder
	if s.Audit != nil {
		auditRecorder = s.Audit
	}
	return mcphost.NewMCPAccessFactory(
		s.agentMCPResolver(), auditRecorder,
	)
}

func (s *Server) dispatchRecorder() mcphost.DispatchRecorder {
	if s.Tasks == nil {
		return nil
	}
	return s.Tasks
}
