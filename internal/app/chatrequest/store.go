package chatrequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrConflict = errors.New("client request id is already bound to different input")
	ErrNotFound = errors.New("chat request not found")
)

type Clock interface {
	Now() time.Time
}

type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

type Request struct {
	WorkspaceID        string            `json:"workspace_id"`
	UserID             string            `json:"user_id"`
	ClientRequestID    string            `json:"client_request_id"`
	RequestFingerprint string            `json:"-"`
	ProjectID          string            `json:"project_id"`
	AgentID            string            `json:"agent_id"`
	SessionID          string            `json:"session_id,omitempty"`
	ConversationID     string            `json:"conversation_id,omitempty"`
	UserMessageID      string            `json:"user_message_id,omitempty"`
	TaskID             string            `json:"task_id,omitempty"`
	RunID              string            `json:"run_id,omitempty"`
	Status             string            `json:"status"`
	Response           json.RawMessage   `json:"response,omitempty"`
	ErrorCode          string            `json:"error_code,omitempty"`
	RuntimeAssignment  json.RawMessage   `json:"runtime_assignment,omitempty"`
	WorkflowProgress   *WorkflowProgress `json:"workflow_progress,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
}

// WorkflowProgress is the durable, reconnect-safe view of a published team
// workflow. Stage completion is derived from immutable workflow deliverables,
// so a browser reload never depends on the original SSE connection.
type WorkflowProgress struct {
	Status          string    `json:"status"`
	CompletedStages int       `json:"completed_stages"`
	TotalStages     int       `json:"total_stages"`
	LatestStage     string    `json:"latest_stage,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type BeginRequest struct {
	WorkspaceID        string
	UserID             string
	ClientRequestID    string
	RequestFingerprint string
	ProjectID          string
	AgentID            string
}

type Store struct {
	pool  *pgxpool.Pool
	clock Clock
}

func New(pool *pgxpool.Pool, clock Clock) *Store {
	if clock == nil {
		clock = RealClock{}
	}
	return &Store{pool: pool, clock: clock}
}

func (s *Store) Begin(ctx context.Context, input BeginRequest) (Request, bool, error) {
	requestID, err := uuid.Parse(strings.TrimSpace(input.ClientRequestID))
	if err != nil {
		return Request{}, false, fmt.Errorf("parse client request id: %w", err)
	}
	now := s.clock.Now()
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO weave_chat_requests (
			workspace_id, user_id, client_request_id, request_fingerprint,
			project_id, agent_id, status, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,'admitting',$7,$7)
		ON CONFLICT (workspace_id, user_id, client_request_id) DO NOTHING
	`, input.WorkspaceID, input.UserID, requestID, input.RequestFingerprint,
		input.ProjectID, input.AgentID, now)
	if err != nil {
		return Request{}, false, fmt.Errorf("begin chat request: %w", err)
	}
	request, err := s.Get(ctx, input.WorkspaceID, input.UserID, requestID.String())
	if err != nil {
		return Request{}, false, err
	}
	if request.RequestFingerprint != input.RequestFingerprint ||
		request.ProjectID != input.ProjectID || request.AgentID != input.AgentID {
		return Request{}, false, ErrConflict
	}
	return request, tag.RowsAffected() == 1, nil
}

func (s *Store) Get(ctx context.Context, workspaceID, userID, clientRequestID string) (Request, error) {
	requestID, err := uuid.Parse(strings.TrimSpace(clientRequestID))
	if err != nil {
		return Request{}, ErrNotFound
	}
	request, err := scanRequest(s.pool.QueryRow(ctx, `
		SELECT workspace_id,user_id,client_request_id::text,request_fingerprint,
			project_id,agent_id,COALESCE(session_id,''),COALESCE(conversation_id,''),
			COALESCE(user_message_id,''),COALESCE(task_id,''),COALESCE(run_id,''),
			status,response,COALESCE(error_code,''),created_at,updated_at
		FROM weave_chat_requests
		WHERE workspace_id=$1 AND user_id=$2 AND client_request_id=$3
	`, workspaceID, userID, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	if err != nil {
		return Request{}, fmt.Errorf("get chat request: %w", err)
	}
	return request, nil
}

// GetWorkflowDispatch reconstructs the status record for a manual team
// workflow dispatch. Workflow dispatches predate the chat-request ledger, but
// their run and task identities are deterministic and durably persisted. This
// keeps the public client_request_id status contract valid without relying on
// process-local state.
func (s *Store) GetWorkflowDispatch(
	ctx context.Context, workspaceID, userID, clientRequestID, runID string,
) (Request, error) {
	requestID, err := uuid.Parse(strings.TrimSpace(clientRequestID))
	if err != nil {
		return Request{}, ErrNotFound
	}
	var request Request
	request.WorkspaceID = workspaceID
	request.UserID = userID
	request.ClientRequestID = requestID.String()
	request.RunID = strings.TrimSpace(runID)
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(snapshot.project_id,''), task.id,
		       snapshot.created_at, snapshot.created_at
		FROM weave_team_run_snapshots AS snapshot
		JOIN weave_task_queue AS task
		  ON task.workspace_id=snapshot.workspace_id
		 AND task.run_snapshot_id=snapshot.run_id
		WHERE snapshot.workspace_id=$1 AND snapshot.run_id=$2
		  AND snapshot.mode='fixed_workflow'
		  AND snapshot.trigger_source_v2->>'type'='manual'
		  AND snapshot.trigger_source_v2->>'source_ref'=$3
		ORDER BY task.created_at ASC, task.id ASC
		LIMIT 1
	`, workspaceID, request.RunID, userID).Scan(
		&request.ProjectID, &request.TaskID, &request.CreatedAt, &request.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	if err != nil {
		return Request{}, fmt.Errorf("get workflow dispatch: %w", err)
	}
	request.Status = "running"
	request, err = s.AttachWorkflowProgress(ctx, request)
	if err != nil {
		return Request{}, err
	}
	if request.WorkflowProgress != nil {
		request.Status = productWorkflowStatus(request.WorkflowProgress.Status)
	}
	return request, nil
}

func productWorkflowStatus(status string) string {
	switch strings.TrimSpace(status) {
	case "success", "succeeded", "completed":
		return "completed"
	case "failed", "cancelled", "abandoned":
		return "failed"
	case "yielded", "waiting", "waiting_human":
		return "yielded"
	case "queued", "pending":
		return "queued"
	default:
		return "running"
	}
}

func (s *Store) MarkAdmitted(
	ctx context.Context,
	workspaceID, userID, clientRequestID, sessionID, conversationID, userMessageID string,
) (Request, error) {
	return s.transition(ctx, workspaceID, userID, clientRequestID, `
		UPDATE weave_chat_requests
		SET session_id=$4,conversation_id=$5,user_message_id=$6,status='admitted',updated_at=$7
		WHERE workspace_id=$1 AND user_id=$2 AND client_request_id=$3 AND status='admitting'
	`, sessionID, conversationID, userMessageID)
}

func (s *Store) MarkQueued(
	ctx context.Context, workspaceID, userID, clientRequestID, taskID string, response json.RawMessage,
) (Request, error) {
	return s.transition(ctx, workspaceID, userID, clientRequestID, `
		UPDATE weave_chat_requests
		SET task_id=$4,status='queued',response=$5,updated_at=$6
		WHERE workspace_id=$1 AND user_id=$2 AND client_request_id=$3
		  AND status IN ('admitted','queued')
	`, taskID, response)
}

func (s *Store) MarkRunning(
	ctx context.Context, workspaceID, userID, clientRequestID string,
) (Request, error) {
	return s.transition(ctx, workspaceID, userID, clientRequestID, `
		UPDATE weave_chat_requests SET status='running',updated_at=$4
		WHERE workspace_id=$1 AND user_id=$2 AND client_request_id=$3
		  AND status IN ('admitted','queued','running')
	`)
}

// BindWorkflowRun records the durable workflow identity as soon as admission
// succeeds. Previously run_id was written only at terminal completion, which
// left reconnecting clients unable to recover any published-workflow progress.
func (s *Store) BindWorkflowRun(
	ctx context.Context, workspaceID, userID, clientRequestID, runID, taskID string,
) (Request, error) {
	return s.transition(ctx, workspaceID, userID, clientRequestID, `
		UPDATE weave_chat_requests
		SET run_id=NULLIF($4,''),task_id=NULLIF($5,''),status='running',updated_at=$6
		WHERE workspace_id=$1 AND user_id=$2 AND client_request_id=$3
		  AND status IN ('admitted','queued','running')
		  AND (run_id IS NULL OR run_id=NULLIF($4,''))
	`, strings.TrimSpace(runID), strings.TrimSpace(taskID))
}

// AttachWorkflowProgress enriches a request whose run_id identifies an
// immutable team-run snapshot. Requests from ordinary agent chats are returned
// unchanged.
func (s *Store) AttachWorkflowProgress(ctx context.Context, request Request) (Request, error) {
	if s == nil || s.pool == nil || strings.TrimSpace(request.RunID) == "" {
		return request, nil
	}
	var progress WorkflowProgress
	err := s.pool.QueryRow(ctx, `
		SELECT
			COALESCE(run.status, 'queued'),
			snapshot.runtime_assignment,
			COALESCE(progress.completed_stages, 0)::int,
			COALESCE((
				SELECT COUNT(*)::int
				FROM jsonb_array_elements(
					CASE
						WHEN jsonb_typeof(version.graph_definition->'nodes')='array'
						THEN version.graph_definition->'nodes'
						ELSE '[]'::jsonb
					END
				) AS node
				WHERE node->>'type' IN ('lead','worker','transform','join','loop','deliver')
			), 0),
			COALESCE(progress.latest_stage, ''),
			COALESCE(progress.updated_at, run.updated_at, snapshot.created_at)
		FROM weave_team_run_snapshots AS snapshot
		JOIN weave_team_workflow_versions AS version
		  ON version.workspace_id=snapshot.workspace_id
		 AND version.workflow_id=snapshot.workflow_id
		 AND version.version=snapshot.workflow_version
		LEFT JOIN weave_team_runs AS run
		  ON run.workspace_id=snapshot.workspace_id
		 AND run.run_snapshot_id=snapshot.run_id
		LEFT JOIN LATERAL (
			SELECT
				COUNT(DISTINCT NULLIF(deliverable.metadata->>'node_id', '')) AS completed_stages,
				(
					ARRAY_AGG(deliverable.metadata->>'node_label'
					  ORDER BY deliverable.created_at DESC, deliverable.id DESC)
				)[1] AS latest_stage,
				MAX(deliverable.created_at) AS updated_at
			FROM weave_final_deliverables AS deliverable
			WHERE deliverable.workspace_id=snapshot.workspace_id
			  AND deliverable.run_snapshot_id=snapshot.run_id
			  AND deliverable.metadata->>'source'='published_workflow'
		) AS progress ON TRUE
		WHERE snapshot.workspace_id=$1 AND snapshot.run_id=$2
		  AND snapshot.mode='fixed_workflow'
	`, request.WorkspaceID, request.RunID).Scan(
		&progress.Status, &request.RuntimeAssignment,
		&progress.CompletedStages, &progress.TotalStages,
		&progress.LatestStage, &progress.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return request, nil
	}
	if err != nil {
		return request, fmt.Errorf("read chat request workflow progress: %w", err)
	}
	if len(request.RuntimeAssignment) == 0 ||
		string(request.RuntimeAssignment) == "null" ||
		string(request.RuntimeAssignment) == "{}" {
		request.RuntimeAssignment = nil
	}
	request.WorkflowProgress = &progress
	return request, nil
}

func (s *Store) MarkFinished(
	ctx context.Context,
	workspaceID, userID, clientRequestID, status, runID string,
	response json.RawMessage,
) (Request, error) {
	if status != "yielded" && status != "completed" && status != "failed" {
		return Request{}, fmt.Errorf("invalid terminal chat request status %q", status)
	}
	return s.transition(ctx, workspaceID, userID, clientRequestID, `
		UPDATE weave_chat_requests
		SET status=$4,run_id=NULLIF($5,''),response=$6,
			error_code=CASE WHEN $4='failed' THEN 'execution_failed' ELSE NULL END,
			updated_at=$7
		WHERE workspace_id=$1 AND user_id=$2 AND client_request_id=$3
		  AND status NOT IN ('completed','failed')
	`, status, runID, response)
}

// FailOrphaned finalizes chat requests stuck in 'running' whose session
// execution lease has already terminated: the lease is closed, or it expired
// without a heartbeat. These are requests whose streaming handler died before
// it could write terminal state (for example the server was restarted
// mid-run), so the request would otherwise stay 'running' forever.
func (s *Store) FailOrphaned(ctx context.Context, staleAfter time.Duration) (int64, error) {
	now := s.clock.Now()
	cutoff := now.Add(-staleAfter)
	tag, err := s.pool.Exec(ctx, `
		UPDATE weave_chat_requests AS req
		SET status='failed', error_code='execution_failed',
			response=jsonb_build_object('error','execution_interrupted'),
			updated_at=$2
		WHERE req.status='running'
		  AND req.session_id IS NOT NULL
		  AND req.updated_at < $1
		  AND EXISTS (
			SELECT 1 FROM weave_session_execution_leases AS lease
			WHERE lease.workspace_id=req.workspace_id
			  AND lease.user_id=req.user_id
			  AND lease.session_id=req.session_id
			  AND (lease.state='closed' OR lease.expires_at < $1)
		  )
	`, cutoff, now)
	if err != nil {
		return 0, fmt.Errorf("fail orphaned chat requests: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *Store) transition(
	ctx context.Context,
	workspaceID, userID, clientRequestID, query string,
	args ...any,
) (Request, error) {
	requestID, err := uuid.Parse(strings.TrimSpace(clientRequestID))
	if err != nil {
		return Request{}, ErrNotFound
	}
	params := []any{workspaceID, userID, requestID}
	params = append(params, args...)
	params = append(params, s.clock.Now())
	tag, err := s.pool.Exec(ctx, query, params...)
	if err != nil {
		return Request{}, fmt.Errorf("transition chat request: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return s.Get(ctx, workspaceID, userID, requestID.String())
	}
	return s.Get(ctx, workspaceID, userID, requestID.String())
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRequest(row rowScanner) (Request, error) {
	var request Request
	err := row.Scan(
		&request.WorkspaceID, &request.UserID, &request.ClientRequestID,
		&request.RequestFingerprint, &request.ProjectID, &request.AgentID,
		&request.SessionID, &request.ConversationID, &request.UserMessageID,
		&request.TaskID, &request.RunID, &request.Status, &request.Response,
		&request.ErrorCode, &request.CreatedAt, &request.UpdatedAt,
	)
	return request, err
}
