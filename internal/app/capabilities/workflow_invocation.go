package capabilities

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/workflowadmission"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	kernelcredentials "github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// PublishedWorkflowResolver returns one exact, immutable published workflow.
type PublishedWorkflowResolver interface {
	ResolvePublished(context.Context, string, string, *int) (frozen.ArtifactEnvelopeV1, error)
}

// WorkflowInvokeRequest is a trusted application-authenticated request. Only
// request_id, invocation_id, lane, and input come from the HTTP body.
type WorkflowInvokeRequest struct {
	WorkspaceID             string
	ApplicationID           string
	CredentialID            string
	RequestID               string
	InvocationID            string
	CapabilityID            string
	Revision                int64
	Lane                    string
	Input                   json.RawMessage
	InvocationIDWasSupplied bool
	Priority                string
	FairnessKey             string
}

type WorkflowInvocation struct {
	ApplicationID      string                   `json:"application_id"`
	InvocationID       string                   `json:"invocation_id"`
	RequestID          string                   `json:"request_id,omitempty"`
	CapabilityID       string                   `json:"capability_id,omitempty"`
	Revision           int64                    `json:"revision,omitempty"`
	RunID              string                   `json:"run_id,omitempty"`
	WorkflowID         string                   `json:"workflow_id,omitempty"`
	WorkflowVersion    int                      `json:"workflow_version,omitempty"`
	InputHash          string                   `json:"input_hash,omitempty"`
	Status             string                   `json:"status"`
	ResultState        string                   `json:"result_state"`
	Result             json.RawMessage          `json:"result,omitempty"`
	Error              string                   `json:"error,omitempty"`
	Trace              WorkflowInvocationTrace  `json:"trace"`
	AgentID            string                   `json:"agent_id,omitempty"`
	Model              string                   `json:"model,omitempty"`
	Engine             string                   `json:"engine,omitempty"`
	PhysicalUsage      *execution.TerminalUsage `json:"physical_usage,omitempty"`
	UnreportedAttempts *int                     `json:"unreported_attempts,omitempty"`
	UsageComplete      bool                     `json:"usage_complete"`
	CreatedAt          time.Time                `json:"created_at,omitempty"`
	Replayed           bool                     `json:"replayed"`
}

type WorkflowInvocationTrace struct {
	Nodes []WorkflowInvocationTraceNode `json:"nodes"`
}

type WorkflowInvocationTraceNode struct {
	NodeID    string          `json:"node_id"`
	Label     string          `json:"label"`
	AgentID   string          `json:"agent_id,omitempty"`
	AgentName string          `json:"agent_name,omitempty"`
	Model     string          `json:"model,omitempty"`
	Kind      string          `json:"kind"`
	Status    string          `json:"status"`
	ElapsedMS int64           `json:"elapsed_ms"`
	Output    json.RawMessage `json:"output,omitempty"`
}

type WorkflowInvocationEvent struct {
	Sequence int64           `json:"sequence"`
	Type     string          `json:"type"`
	NodeID   string          `json:"node_id,omitempty"`
	AgentID  string          `json:"agent_id,omitempty"`
	Detail   json.RawMessage `json:"detail"`
	At       time.Time       `json:"at"`
}

// WorkflowInvocationService owns only the application call ledger and its
// association with the platform's published TeamRun. TeamRun remains the
// state and terminal-result authority.
type WorkflowInvocationService struct {
	pool        *pgxpool.Pool
	definitions *Service
	workflows   PublishedWorkflowResolver
	published   publication.PublishedService
	tasks       *taskqueue.Store
	canceller   *teamrun.CancelService
}

func NewWorkflowInvocationService(
	pool *pgxpool.Pool,
	definitions *Service,
	workflows PublishedWorkflowResolver,
	published publication.PublishedService,
	tasks *taskqueue.Store,
	canceller *teamrun.CancelService,
) *WorkflowInvocationService {
	if tasks == nil && pool != nil {
		tasks = taskqueue.New(pool, nil, time.Minute)
	}
	if canceller == nil && pool != nil {
		runs := teamrun.NewPGStore()
		runs.Transactions = pool
		canceller = &teamrun.CancelService{Transactions: pool, Runs: runs, Tasks: tasks}
	}
	return &WorkflowInvocationService{pool: pool, definitions: definitions, workflows: workflows,
		published: published, tasks: tasks, canceller: canceller}
}

type workflowInvocationRecord struct {
	Scheduling         *execution.SchedulingHints
	WorkspaceID        string
	ApplicationID      string
	RequestID          string
	InvocationID       string
	CredentialID       string
	CapabilityID       string
	Revision           int64
	DefinitionHash     string
	WorkflowID         string
	WorkflowVersion    int
	Input              json.RawMessage
	InputHash          string
	Lane               string
	AdmissionRequestID string
	RunID              string
	TaskID             string
	CreatedAt          time.Time
}

type workflowInvocationRecordReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *WorkflowInvocationService) ready() error {
	if s == nil || s.pool == nil || s.definitions == nil || s.definitions.drafts == nil ||
		s.workflows == nil || s.published == nil || s.tasks == nil || s.canceller == nil {
		return errors.New("published workflow invocation service is unavailable")
	}
	return nil
}

// ValidateTarget checks the exact capability revision before it is published.
// The capability revision stores only the workflow reference and contracts.
func (s *WorkflowInvocationService) ValidateTarget(ctx context.Context, workspaceID, capabilityID string, revision int64) error {
	if err := s.ready(); err != nil {
		return err
	}
	draft, err := s.definitions.drafts.GetDraft(ctx, workspaceID, capabilityID)
	if err != nil {
		return err
	}
	published, err := capability.Publish(draft, revision)
	if err != nil {
		return err
	}
	return s.ValidateDefinition(ctx, workspaceID, published.Definition)
}

// ValidateDefinition resolves the exact referenced publication and checks the
// capability contract against that source without copying its execution graph.
func (s *WorkflowInvocationService) ValidateDefinition(ctx context.Context, workspaceID string, definition capability.Definition) error {
	if err := s.ready(); err != nil {
		return err
	}
	if definition.Workflow == nil {
		return nil
	}
	if err := definition.Validate(); err != nil {
		return err
	}
	if err := capability.ValidateSchemas(definition); err != nil {
		return err
	}
	version := definition.Workflow.WorkflowVersion
	envelope, err := s.workflows.ResolvePublished(ctx, workspaceID, definition.Workflow.WorkflowID, &version)
	if err != nil {
		return err
	}
	if envelope.WorkspaceID != workspaceID || envelope.WorkflowID != definition.Workflow.WorkflowID || envelope.WorkflowVersion != version {
		return fmt.Errorf("%w: workflow publication identity differs", capability.ErrInvalidDefinition)
	}
	if err := capability.ValidateWorkflowSource(envelope); err != nil {
		return fmt.Errorf("%w: workflow source: %v", capability.ErrInvalidDefinition, err)
	}
	return validateWorkflowCapabilityContracts(envelope)
}

func (s *WorkflowInvocationService) Invoke(ctx context.Context, request WorkflowInvokeRequest) (WorkflowInvocation, bool, error) {
	if err := s.ready(); err != nil {
		return WorkflowInvocation{}, false, err
	}
	if err := validateWorkflowInvokeRequest(request); err != nil {
		return WorkflowInvocation{}, false, err
	}
	priority, err := schedulingPriority(request.Priority)
	if err != nil {
		return WorkflowInvocation{}, false, err
	}
	if len(request.FairnessKey) > 64 || strings.TrimSpace(request.FairnessKey) != request.FairnessKey || strings.ContainsAny(request.FairnessKey, "\x00\r\n") {
		return WorkflowInvocation{}, false, fmt.Errorf("%w: invalid fairness_key", capability.ErrInvalidDefinition)
	}
	subject, err := execution.RequireSubject(ctx, request.WorkspaceID)
	if err != nil || subject.UserID != "" || subject.ServiceID != "capability-app:"+request.ApplicationID {
		return WorkflowInvocation{}, false, ErrAccessDenied
	}

	revision, err := s.definitions.drafts.GetRevision(ctx, request.WorkspaceID, request.CapabilityID, request.Revision)
	if err != nil {
		return WorkflowInvocation{}, false, err
	}
	definition := revision.Definition
	if definition.Workflow == nil {
		return WorkflowInvocation{}, false, ErrWorkflowSourceRequired
	}
	if err := s.ValidateDefinition(ctx, request.WorkspaceID, definition); err != nil {
		return WorkflowInvocation{}, false, err
	}
	envelope, err := s.workflows.ResolvePublished(ctx, request.WorkspaceID, definition.Workflow.WorkflowID, &definition.Workflow.WorkflowVersion)
	if err != nil {
		return WorkflowInvocation{}, false, err
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return WorkflowInvocation{}, false, err
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) != 0 {
		return WorkflowInvocation{}, false, fmt.Errorf("%w: published workflow graph is invalid", capability.ErrInvalidDefinition)
	}
	canonicalInput, inputHash, workflowInput, err := normalizeWorkflowInput(request.Input, definition, graph)
	if err != nil {
		return WorkflowInvocation{}, false, err
	}

	invocationID := request.InvocationID
	if invocationID == "" {
		invocationID = uuid.NewString()
	}
	admissionRequestID := "capability-workflow-" + uuid.NewString()
	runID := "capability-run-" + uuid.NewString()
	taskID := "capability-task-" + uuid.NewString()
	record := workflowInvocationRecord{WorkspaceID: request.WorkspaceID, ApplicationID: request.ApplicationID,
		Scheduling: &execution.SchedulingHints{Priority: priority, FairnessKey: request.FairnessKey},
		RequestID:  request.RequestID, InvocationID: invocationID, CredentialID: request.CredentialID,
		CapabilityID: request.CapabilityID, Revision: request.Revision, DefinitionHash: revision.DefinitionHash,
		WorkflowID: definition.Workflow.WorkflowID, WorkflowVersion: definition.Workflow.WorkflowVersion,
		Input: canonicalInput, InputHash: inputHash, Lane: request.Lane, AdmissionRequestID: admissionRequestID,
		RunID: runID, TaskID: taskID}
	admission := s.admissions()
	if admission == nil {
		return WorkflowInvocation{}, false, errors.New("published workflow admission is unavailable")
	}
	stored, replayed, cancelled, err := s.reserve(ctx, request, record, admission, envelope, payload.Team.TeamID, workflowInput)
	if errors.Is(err, errQuotaSlotsFull) || errors.Is(err, errApplicationQuotaSlotsFull) {
		// Free the slots of ended work in a transaction of its own, so the
		// release survives this admission being refused, then try once more.
		if err = refreshQuotaSlots(ctx, s.pool, request.WorkspaceID, request.ApplicationID); err == nil {
			stored, replayed, cancelled, err = s.reserve(ctx, request, record, admission, envelope, payload.Team.TeamID, workflowInput)
		}
		switch {
		case errors.Is(err, errApplicationQuotaSlotsFull):
			err = ErrApplicationQuotaExceeded
		case errors.Is(err, errQuotaSlotsFull):
			err = ErrQuotaExceeded
		}
	}
	if err != nil {
		return WorkflowInvocation{}, false, err
	}
	if cancelled {
		if replayed {
			view, getErr := s.Get(ctx, stored.WorkspaceID, stored.ApplicationID, stored.InvocationID)
			if getErr != nil {
				return WorkflowInvocation{}, false, getErr
			}
			view.Replayed = true
			return view, true, nil
		}
		return WorkflowInvocation{}, false, ErrWorkflowInvocationCancelled
	}
	_, err = admission.Admit(ctx, stored.WorkspaceID, stored.AdmissionRequestID, s.associate)
	if errors.Is(err, publication.ErrAdmissionClosed) && s.hasCancelIntent(ctx, stored.WorkspaceID, stored.ApplicationID, stored.InvocationID) {
		view, getErr := s.Get(ctx, stored.WorkspaceID, stored.ApplicationID, stored.InvocationID)
		if getErr == nil {
			view.Replayed = replayed
			return view, replayed, nil
		}
	}
	if err != nil {
		return WorkflowInvocation{}, false, err
	}
	view, err := s.Get(ctx, stored.WorkspaceID, stored.ApplicationID, stored.InvocationID)
	if err != nil {
		return WorkflowInvocation{}, false, err
	}
	view.Replayed = replayed
	return view, replayed, nil
}

func validateWorkflowInvokeRequest(request WorkflowInvokeRequest) error {
	if strings.TrimSpace(request.WorkspaceID) == "" || strings.TrimSpace(request.ApplicationID) == "" ||
		strings.TrimSpace(request.CredentialID) == "" || strings.TrimSpace(request.RequestID) == "" ||
		len(request.RequestID) > 256 || strings.TrimSpace(request.RequestID) != request.RequestID ||
		strings.ContainsAny(request.RequestID, "\x00\r\n") || strings.TrimSpace(request.CapabilityID) == "" || request.Revision < 1 {
		return fmt.Errorf("%w: workspace, application, credential, request, and positive capability revision are required", capability.ErrInvalidDefinition)
	}
	if request.InvocationIDWasSupplied {
		parsed, err := uuid.Parse(request.InvocationID)
		if err != nil || parsed.String() != request.InvocationID {
			return fmt.Errorf("%w: invocation_id must be a canonical UUID", capability.ErrInvalidDefinition)
		}
	}
	if request.Lane != "" && (strings.TrimSpace(request.Lane) != request.Lane || len(request.Lane) > 256 || strings.ContainsAny(request.Lane, "\x00\r\n")) {
		return fmt.Errorf("%w: lane must contain at most 256 bytes without surrounding whitespace", capability.ErrInvalidDefinition)
	}
	if len(request.Input) == 0 {
		return fmt.Errorf("%w: input is required", capability.ErrInvalidDefinition)
	}
	return nil
}

func normalizeWorkflowInput(raw json.RawMessage, definition capability.Definition, graph machine.GraphDefinition) (json.RawMessage, string, json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, "", nil, fmt.Errorf("%w: input must be a JSON object", capability.ErrInvalidDefinition)
	}
	canonical, err := frozen.CanonicalizeJSON(raw)
	if err != nil {
		return nil, "", nil, fmt.Errorf("%w: input must be canonical JSON", capability.ErrInvalidDefinition)
	}
	if err := capability.ValidateValue(definition.InputSchema, canonical); err != nil {
		return nil, "", nil, fmt.Errorf("%w: input_schema: %v", capability.ErrInvalidDefinition, err)
	}
	hash, err := frozen.HashCanonicalJSON(canonical)
	if err != nil {
		return nil, "", nil, err
	}
	workflowInput := canonical
	switch graph.InputContract.Type {
	case machine.ValueJSON:
		if len(graph.InputContract.Schema) > 0 {
			if _, problems := machine.ValidateRuntimeInput(graph.InputContract.Schema, canonical); len(problems) != 0 {
				return nil, "", nil, fmt.Errorf("%w: input does not satisfy published workflow JSON contract", capability.ErrInvalidDefinition)
			}
		}
	case machine.ValueText:
		workflowInput, err = json.Marshal(string(canonical))
		if err != nil {
			return nil, "", nil, err
		}
	default:
		return nil, "", nil, fmt.Errorf("%w: published workflow input must be JSON or text", capability.ErrInvalidDefinition)
	}
	return canonical, hash, workflowInput, nil
}

func (s *WorkflowInvocationService) admissions() *workflowadmission.Store {
	if s == nil || s.pool == nil || s.published == nil {
		return nil
	}
	return workflowadmission.New(s.pool, s.published)
}

func (s *WorkflowInvocationService) reserve(
	ctx context.Context,
	request WorkflowInvokeRequest,
	desired workflowInvocationRecord,
	admissions *workflowadmission.Store,
	envelope frozen.ArtifactEnvelopeV1,
	teamID string,
	workflowInput json.RawMessage,
) (workflowInvocationRecord, bool, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return workflowInvocationRecord{}, false, false, fmt.Errorf("begin capability workflow invocation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	requestLock := invocationLockKey("request", request.WorkspaceID, request.ApplicationID, request.RequestID)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, requestLock); err != nil {
		return workflowInvocationRecord{}, false, false, fmt.Errorf("lock capability workflow request: %w", err)
	}
	if existing, found, err := readWorkflowInvocationByRequest(ctx, tx, request.WorkspaceID, request.ApplicationID, request.RequestID, true); err != nil {
		return workflowInvocationRecord{}, false, false, fmt.Errorf("read capability workflow idempotency record: %w", err)
	} else if found {
		invocationLock := invocationLockKey("invocation", request.WorkspaceID, request.ApplicationID, existing.InvocationID)
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, invocationLock); err != nil {
			return workflowInvocationRecord{}, false, false, fmt.Errorf("lock capability workflow invocation: %w", err)
		}
		if !sameWorkflowInvocationRequest(existing, desired, request.InvocationIDWasSupplied) {
			return workflowInvocationRecord{}, false, false, ErrIdempotencyConflict
		}
		if err := authorizeApplicationInvocation(ctx, tx, grantIdentity{WorkspaceID: request.WorkspaceID,
			ApplicationID: request.ApplicationID, CredentialID: request.CredentialID,
			CapabilityID: request.CapabilityID, Revision: request.Revision}); err != nil {
			return workflowInvocationRecord{}, false, false, err
		}
		cancelled, err := hasWorkflowCancelIntent(ctx, tx, request.WorkspaceID, request.ApplicationID, existing.InvocationID)
		if err != nil {
			return workflowInvocationRecord{}, false, false, fmt.Errorf("read capability workflow cancellation: %w", err)
		}
		if err = tx.Commit(ctx); err != nil {
			return workflowInvocationRecord{}, false, false, err
		}
		return existing, true, cancelled, nil
	}
	invocationLock := invocationLockKey("invocation", request.WorkspaceID, request.ApplicationID, desired.InvocationID)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, invocationLock); err != nil {
		return workflowInvocationRecord{}, false, false, fmt.Errorf("lock capability workflow invocation: %w", err)
	}
	var invocationExists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_capability_workflow_invocations WHERE workspace_id=$1 AND application_id=$2 AND invocation_id=$3)`,
		request.WorkspaceID, request.ApplicationID, desired.InvocationID).Scan(&invocationExists); err != nil {
		return workflowInvocationRecord{}, false, false, fmt.Errorf("check capability workflow invocation identity: %w", err)
	}
	if invocationExists {
		return workflowInvocationRecord{}, false, false, ErrInvocationIdentityConflict
	}
	var legacyRequestExists, legacyInvocationExists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_capability_invocations
	 WHERE workspace_id=$1 AND application_id=$2 AND request_id=$3),
	 EXISTS(SELECT 1 FROM weave_capability_invocations WHERE workspace_id=$1 AND application_id=$2 AND invocation_id=$4)`,
		request.WorkspaceID, request.ApplicationID, request.RequestID, desired.InvocationID).Scan(&legacyRequestExists, &legacyInvocationExists); err != nil {
		return workflowInvocationRecord{}, false, false, fmt.Errorf("check existing capability invocation namespace: %w", err)
	}
	if legacyRequestExists {
		return workflowInvocationRecord{}, false, false, ErrIdempotencyConflict
	}
	if legacyInvocationExists {
		return workflowInvocationRecord{}, false, false, ErrInvocationIdentityConflict
	}
	// A cancellation tombstone only reserves an otherwise unused invocation ID.
	// Check every invocation namespace while holding its lock first, so a caller
	// cannot rebind an accepted invocation to a different request ID merely
	// because the original invocation has since been cancelled.
	cancelled, err := hasWorkflowCancelIntent(ctx, tx, request.WorkspaceID, request.ApplicationID, desired.InvocationID)
	if err != nil {
		return workflowInvocationRecord{}, false, false, fmt.Errorf("read capability workflow cancellation: %w", err)
	}
	if cancelled {
		if err = tx.Commit(ctx); err != nil {
			return workflowInvocationRecord{}, false, false, err
		}
		return desired, false, true, nil
	}
	if err := authorizeApplicationInvocation(ctx, tx, grantIdentity{WorkspaceID: request.WorkspaceID,
		ApplicationID: request.ApplicationID, CredentialID: request.CredentialID,
		CapabilityID: request.CapabilityID, Revision: request.Revision}); err != nil {
		return workflowInvocationRecord{}, false, false, err
	}
	if desired.Lane != "" {
		// Lane serialization is independent of priority and remains unchanged.
		laneLock := invocationLockKey("lane", request.WorkspaceID, request.ApplicationID, desired.Lane)
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, laneLock); err != nil {
			return workflowInvocationRecord{}, false, false, fmt.Errorf("lock capability workflow lane: %w", err)
		}
		active, err := workflowLaneActive(ctx, tx, request.WorkspaceID, request.ApplicationID, desired.Lane, desired.InvocationID)
		if err != nil {
			return workflowInvocationRecord{}, false, false, err
		}
		if active {
			return workflowInvocationRecord{}, false, false, ErrInvocationLaneActive
		}
	}
	runRequest := publishedRequest(desired, envelope, workflowInput)
	if _, err = admissions.ReserveTx(ctx, tx, runRequest, workflowadmission.Target{TeamID: teamID}); err != nil {
		return workflowInvocationRecord{}, false, false, fmt.Errorf("reserve published workflow admission: %w", err)
	}
	var maxPriority string
	if err := tx.QueryRow(ctx, `SELECT max_priority FROM weave_capability_apps WHERE workspace_id=$1 AND id=$2 FOR SHARE`, request.WorkspaceID, request.ApplicationID).Scan(&maxPriority); err != nil {
		return workflowInvocationRecord{}, false, false, err
	}
	ceiling, err := schedulingPriority(maxPriority)
	if err != nil || desired.Scheduling.Priority > ceiling {
		return workflowInvocationRecord{}, false, false, fmt.Errorf("%w: requested priority exceeds application max_priority", capability.ErrInvalidDefinition)
	}
	schedulingJSON, err := json.Marshal(desired.Scheduling)
	if err != nil {
		return workflowInvocationRecord{}, false, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO weave_capability_workflow_invocations(
	 workspace_id,application_id,request_id,invocation_id,credential_id,capability_id,revision,definition_hash,
	 workflow_id,workflow_version,input,input_hash,lane,admission_request_id,run_id,task_id,scheduling)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12,NULLIF($13,''),$14,$15,$16,$17::jsonb)`,
		desired.WorkspaceID, desired.ApplicationID, desired.RequestID, desired.InvocationID, desired.CredentialID,
		desired.CapabilityID, desired.Revision, desired.DefinitionHash, desired.WorkflowID, desired.WorkflowVersion,
		string(desired.Input), desired.InputHash, desired.Lane, desired.AdmissionRequestID, desired.RunID, desired.TaskID, string(schedulingJSON))
	if err != nil {
		return workflowInvocationRecord{}, false, false, fmt.Errorf("insert capability workflow invocation: %w", translateWorkflowInvocationInsertError(err))
	}
	// The quota slot is taken last, after the invocation row it is held for;
	// an exceeded quota rolls back everything above, the slot included.
	if err := acquireCapabilityQuotaSlot(ctx, tx, request.WorkspaceID,
		slotHolder{Kind: slotHolderWorkflow, ApplicationID: desired.ApplicationID, ID: desired.InvocationID}); err != nil {
		return workflowInvocationRecord{}, false, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return workflowInvocationRecord{}, false, false, fmt.Errorf("commit capability workflow invocation: %w", err)
	}
	desired.CreatedAt = time.Now().UTC()
	return desired, false, false, nil
}

func publishedRequest(record workflowInvocationRecord, envelope frozen.ArtifactEnvelopeV1, input json.RawMessage) publication.PublishedRunRequest {
	var scheduling *execution.SchedulingHints
	if record.Scheduling != nil {
		hints := *record.Scheduling
		if hints.FairnessKey == "" {
			hints.FairnessKey = record.Lane
			if hints.FairnessKey == "" {
				hints.FairnessKey = record.InvocationID
			}
		}
		hints.FairnessKey = record.ApplicationID + ":" + hints.FairnessKey
		scheduling = &hints
	}
	return publication.PublishedRunRequest{
		Scheduling: scheduling,
		Version:    publication.ContractVersion, RequestID: record.AdmissionRequestID,
		Revision: publication.RevisionRef{WorkspaceID: record.WorkspaceID, WorkflowID: record.WorkflowID,
			WorkflowVersion: record.WorkflowVersion, ContentHash: envelope.ContentHash},
		RunID: record.RunID, TaskID: record.TaskID, Input: input,
		InputVersion: "sha256:" + record.InputHash, ContextKey: record.InvocationID,
		Trigger: publication.PublishedTrigger{Type: "manual", SourceRef: record.ApplicationID},
	}
}

func (s *WorkflowInvocationService) associate(ctx context.Context, tx pgx.Tx, admission workflowadmission.Record) error {
	if admission.Receipt == nil {
		return publication.ErrInvalidReceipt
	}
	var runID, taskID, workflowID string
	var workflowVersion int
	if err := tx.QueryRow(ctx, `SELECT run_id,task_id,workflow_id,workflow_version
	 FROM weave_capability_workflow_invocations WHERE workspace_id=$1 AND admission_request_id=$2 FOR UPDATE`,
		admission.Request.Revision.WorkspaceID, admission.Request.RequestID).Scan(&runID, &taskID, &workflowID, &workflowVersion); err != nil {
		return err
	}
	if runID != admission.Receipt.RunID || taskID != admission.Receipt.TaskID ||
		workflowID != admission.Receipt.Revision.WorkflowID || workflowVersion != admission.Receipt.Revision.WorkflowVersion {
		return publication.ErrRequestConflict
	}
	return nil
}

func sameWorkflowInvocationRequest(stored, wanted workflowInvocationRecord, invocationIDSupplied bool) bool {
	storedInput, storedInputErr := frozen.CanonicalizeJSON(stored.Input)
	wantedInput, wantedInputErr := frozen.CanonicalizeJSON(wanted.Input)
	return storedInputErr == nil && wantedInputErr == nil && bytes.Equal(storedInput, wantedInput) &&
		(!invocationIDSupplied || stored.InvocationID == wanted.InvocationID) &&
		stored.CapabilityID == wanted.CapabilityID && stored.Revision == wanted.Revision &&
		stored.DefinitionHash == wanted.DefinitionHash && stored.WorkflowID == wanted.WorkflowID &&
		stored.WorkflowVersion == wanted.WorkflowVersion && stored.InputHash == wanted.InputHash && stored.Lane == wanted.Lane && sameScheduling(stored.Scheduling, wanted.Scheduling)
}

func invocationLockKey(kind, workspaceID, applicationID, identity string) string {
	digest := sha256.Sum256([]byte(kind + "\x00" + workspaceID + "\x00" + applicationID + "\x00" + identity))
	return kind + ":" + hex.EncodeToString(digest[:])
}

func hasWorkflowCancelIntent(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, workspaceID, applicationID, invocationID string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_capability_workflow_cancel_intents
	 WHERE workspace_id=$1 AND application_id=$2 AND invocation_id=$3)`, workspaceID, applicationID, invocationID).Scan(&exists)
	return exists, err
}

func (s *WorkflowInvocationService) hasCancelIntent(ctx context.Context, workspaceID, applicationID, invocationID string) bool {
	exists, err := hasWorkflowCancelIntent(ctx, s.pool, workspaceID, applicationID, invocationID)
	return err == nil && exists
}

func readWorkflowInvocationByRequest(ctx context.Context, q workflowInvocationRecordReader, workspaceID, applicationID, requestID string, lock bool) (workflowInvocationRecord, bool, error) {
	query := `SELECT workspace_id,application_id,request_id,invocation_id,credential_id,capability_id,revision,definition_hash,
	 workflow_id,workflow_version,input,input_hash,COALESCE(lane,''),admission_request_id,run_id,task_id,created_at,scheduling
	 FROM weave_capability_workflow_invocations WHERE workspace_id=$1 AND application_id=$2 AND request_id=$3`
	if lock {
		query += " FOR UPDATE"
	}
	record, err := scanWorkflowInvocation(q.QueryRow(ctx, query, workspaceID, applicationID, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return workflowInvocationRecord{}, false, nil
	}
	if err != nil {
		return workflowInvocationRecord{}, false, err
	}
	return record, true, nil
}

func readWorkflowInvocation(ctx context.Context, q workflowInvocationRecordReader, workspaceID, applicationID, invocationID string, lock bool) (workflowInvocationRecord, error) {
	query := `SELECT workspace_id,application_id,request_id,invocation_id,credential_id,capability_id,revision,definition_hash,
	 workflow_id,workflow_version,input,input_hash,COALESCE(lane,''),admission_request_id,run_id,task_id,created_at,scheduling
	 FROM weave_capability_workflow_invocations WHERE workspace_id=$1 AND application_id=$2 AND invocation_id=$3`
	if lock {
		query += " FOR UPDATE"
	}
	return scanWorkflowInvocation(q.QueryRow(ctx, query, workspaceID, applicationID, invocationID))
}

func scanWorkflowInvocation(row pgx.Row) (workflowInvocationRecord, error) {
	var record workflowInvocationRecord
	var scheduling []byte
	err := row.Scan(&record.WorkspaceID, &record.ApplicationID, &record.RequestID, &record.InvocationID,
		&record.CredentialID, &record.CapabilityID, &record.Revision, &record.DefinitionHash,
		&record.WorkflowID, &record.WorkflowVersion, &record.Input, &record.InputHash,
		&record.Lane, &record.AdmissionRequestID, &record.RunID, &record.TaskID, &record.CreatedAt, &scheduling)
	if err == nil && len(scheduling) > 0 {
		err = json.Unmarshal(scheduling, &record.Scheduling)
	}
	return record, err
}

func translateWorkflowInvocationInsertError(err error) error {
	var postgresError interface{ SQLState() string }
	if errors.As(err, &postgresError) && postgresError.SQLState() == "23505" {
		return ErrInvocationIdentityConflict
	}
	return err
}

func (s *WorkflowInvocationService) Get(ctx context.Context, workspaceID, applicationID, invocationID string) (WorkflowInvocation, error) {
	if err := s.ready(); err != nil {
		return WorkflowInvocation{}, err
	}
	record, err := readWorkflowInvocation(ctx, s.pool, workspaceID, applicationID, invocationID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		if s.hasCancelIntent(ctx, workspaceID, applicationID, invocationID) {
			return WorkflowInvocation{ApplicationID: applicationID, InvocationID: invocationID,
				Status: "cancelled", ResultState: "unavailable", Trace: WorkflowInvocationTrace{Nodes: []WorkflowInvocationTraceNode{}}}, nil
		}
		return WorkflowInvocation{}, ErrInvocationNotFound
	}
	if err != nil {
		return WorkflowInvocation{}, err
	}
	view := WorkflowInvocation{ApplicationID: record.ApplicationID, InvocationID: record.InvocationID,
		RequestID: record.RequestID, CapabilityID: record.CapabilityID, Revision: record.Revision,
		RunID: record.RunID, WorkflowID: record.WorkflowID, WorkflowVersion: record.WorkflowVersion,
		InputHash: record.InputHash, ResultState: "unavailable", Trace: WorkflowInvocationTrace{Nodes: []WorkflowInvocationTraceNode{}},
		CreatedAt: record.CreatedAt}
	grantActive, err := s.applicationGrantActive(ctx, record)
	if err != nil {
		return WorkflowInvocation{}, err
	}
	var runStatus, runError string
	err = s.pool.QueryRow(ctx, `SELECT status,COALESCE(error_code,'') FROM weave_team_runs WHERE workspace_id=$1 AND run_id=$2`,
		record.WorkspaceID, record.RunID).Scan(&runStatus, &runError)
	if errors.Is(err, pgx.ErrNoRows) {
		if s.publishedAdmissionClosed(ctx, record.WorkspaceID, record.AdmissionRequestID) {
			view.Status = "cancelled"
		} else {
			task, taskErr := s.tasks.Get(ctx, record.WorkspaceID, record.TaskID)
			if taskErr != nil && !errors.Is(taskErr, pgx.ErrNoRows) {
				view.Status = "pending"
			} else if task == nil {
				view.Status = "pending"
			} else {
				switch task.Status {
				case taskqueue.StatusQueued, taskqueue.StatusDispatched:
					view.Status = "queued"
				case taskqueue.StatusRunning:
					view.Status = "running"
				case taskqueue.StatusCancelRequested:
					view.Status = "cancel_requested"
				case taskqueue.StatusCancelled:
					view.Status = "cancelled"
				case taskqueue.StatusFailed, taskqueue.StatusTimedOut:
					view.Status, view.Error = "failed", task.Error
				default:
					view.Status = "pending"
				}
			}
		}
	} else if err != nil {
		return WorkflowInvocation{}, err
	} else {
		view.Status = projectTeamRunStatus(runStatus)
		if view.Status == "failed" {
			view.Error = runError
		}
		if runStatus == string(teamrun.StatusSucceeded) {
			if grantActive {
				definition, revErr := s.definitions.drafts.GetRevision(ctx, record.WorkspaceID, record.CapabilityID, record.Revision)
				if revErr != nil {
					return WorkflowInvocation{}, revErr
				}
				var result json.RawMessage
				useFinalArtifact := false
				rootTask, taskErr := s.tasks.Get(ctx, record.WorkspaceID, record.TaskID)
				if taskErr != nil {
					if !errors.Is(taskErr, pgx.ErrNoRows) {
						return WorkflowInvocation{}, taskErr
					}
					useFinalArtifact = true
				} else if len(bytes.TrimSpace(rootTask.Result)) == 0 {
					useFinalArtifact = true
				} else if isTeamRunControlEnvelope(rootTask.Result) {
					useFinalArtifact = true
				} else {
					result, err = decodeTeamRunOutput(rootTask.Result)
					if err != nil {
						view.Error, view.ResultState = "output_unavailable", "invalid"
					} else if schemaErr := capability.ValidateValue(definition.Definition.OutputSchema, result); schemaErr != nil {
						// A formal terminal output that violates the frozen capability
						// contract is the result. Do not replace it with another artifact
						// from an earlier attempt of the same run.
						view.Error, view.ResultState = "output_invalid", "invalid"
					} else {
						view.Result, view.ResultState = result, "available"
					}
				}
				if useFinalArtifact {
					result, err = s.readFinalWorkflowOutput(ctx, record)
					if err != nil {
						view.Error, view.ResultState = "output_unavailable", "invalid"
					} else if schemaErr := capability.ValidateValue(definition.Definition.OutputSchema, result); schemaErr != nil {
						view.Error, view.ResultState = "output_invalid", "invalid"
					} else {
						view.Result, view.ResultState = result, "available"
					}
				}
			}
		}
	}
	if !grantActive {
		view.Result, view.ResultState = nil, "unavailable"
		view.Error = "capability_access_revoked"
	}
	envelope, err := s.readWorkflowEnvelope(ctx, record)
	if err == nil {
		payload, decodeErr := frozen.DecodeArtifactEnvelopeV1(envelope)
		if decodeErr == nil {
			graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
			if report == nil || len(report.Issues) == 0 {
				view.Trace = s.trace(ctx, record, payload, graph, view.Status, view.Result)
				view.AgentID, view.Model, view.Engine = workflowResultProducer(payload, graph)
			}
		}
	}
	if !grantActive {
		for index := range view.Trace.Nodes {
			view.Trace.Nodes[index].Output = nil
		}
	}
	usage, err := readWorkflowInvocationUsage(ctx, s.pool, record)
	if err != nil {
		return WorkflowInvocation{}, err
	}
	view.PhysicalUsage = usage.PhysicalUsage
	view.UnreportedAttempts = usage.UnreportedAttempts
	view.UsageComplete = usage.Complete
	return view, nil
}

func (s *WorkflowInvocationService) publishedAdmissionClosed(ctx context.Context, workspaceID, requestID string) bool {
	var operation string
	err := s.pool.QueryRow(ctx, `SELECT operation FROM weave_kernel_publication_requests WHERE workspace_id=$1 AND request_id=$2`, workspaceID, requestID).Scan(&operation)
	return err == nil && operation == "published_closed"
}

func decodeTeamRunOutput(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, errors.New("team run output is unavailable")
	}
	var envelope struct {
		Output json.RawMessage `json:"output"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || len(envelope.Output) == 0 || !json.Valid(envelope.Output) {
		return nil, errors.New("team run output envelope is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("team run output envelope has trailing content")
	}
	return append(json.RawMessage(nil), envelope.Output...), nil
}

func isTeamRunControlEnvelope(raw json.RawMessage) bool {
	var control struct {
		Status string `json:"status"`
		RunID  string `json:"run_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&control); err != nil || control.RunID == "" {
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return false
	}
	switch control.Status {
	case "queued", "running", "parked", "cancel_requested":
		return true
	default:
		return false
	}
}

func (s *WorkflowInvocationService) readFinalWorkflowOutput(
	ctx context.Context,
	record workflowInvocationRecord,
) (json.RawMessage, error) {
	envelope, err := s.readWorkflowEnvelope(ctx, record)
	if err != nil {
		return nil, err
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return nil, err
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) != 0 {
		return nil, errors.New("published workflow graph is invalid")
	}
	deliverNodeID := ""
	for _, node := range graph.Nodes {
		if node.Type != machine.NodeDeliver {
			continue
		}
		if deliverNodeID != "" {
			return nil, errors.New("published workflow has ambiguous final delivery nodes")
		}
		deliverNodeID = node.ID
	}
	if deliverNodeID == "" {
		return nil, errors.New("published workflow has no final delivery node")
	}
	rows, err := s.pool.Query(ctx, `SELECT d.content,d.content_type
	 FROM weave_final_deliverables d
	 JOIN weave_team_run_snapshots snapshot ON snapshot.workspace_id=d.workspace_id AND snapshot.run_id=d.run_snapshot_id
	 WHERE d.workspace_id=$1 AND d.run_id=$2 AND d.run_snapshot_id=$3
	   AND snapshot.mode='fixed_workflow' AND snapshot.workflow_id=$4 AND snapshot.workflow_version=$5
	   AND snapshot.artifact_workflow_id=$4 AND snapshot.artifact_workflow_version=$5
	   AND snapshot.trigger_source_v2->>'type'='manual'
	   AND snapshot.trigger_source_v2->>'source_ref'=$6
	   AND d.metadata->>'source'='published_workflow' AND d.metadata->>'artifact_kind'='final'
	   AND d.metadata->>'node_type'='deliver' AND d.metadata->>'node_id'=$7
	 ORDER BY d.created_at DESC,d.id DESC LIMIT 2`, record.WorkspaceID, record.RunID, record.RunID,
		record.WorkflowID, record.WorkflowVersion, record.ApplicationID, deliverNodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result json.RawMessage
	for rows.Next() {
		var content, contentType string
		if err := rows.Scan(&content, &contentType); err != nil {
			return nil, err
		}
		if result != nil {
			return nil, errors.New("published workflow final output is ambiguous")
		}
		if contentType != "application/json" || !json.Valid([]byte(content)) {
			return nil, errors.New("published workflow final output is not valid JSON")
		}
		result = append(json.RawMessage(nil), []byte(content)...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, errors.New("published workflow final output is unavailable")
	}
	return result, nil
}

func (s *WorkflowInvocationService) Events(ctx context.Context, workspaceID, applicationID, invocationID string) ([]WorkflowInvocationEvent, error) {
	view, err := s.Get(ctx, workspaceID, applicationID, invocationID)
	if err != nil {
		return nil, err
	}
	if view.Error == "capability_access_revoked" {
		return nil, ErrAccessDenied
	}
	if view.RunID == "" {
		return []WorkflowInvocationEvent{}, nil
	}
	events, err := (&teamrun.PGActivityStore{Transactions: s.pool}).List(ctx, workspaceID, view.RunID, 1000)
	if err != nil {
		return nil, err
	}
	out := make([]WorkflowInvocationEvent, 0, len(events))
	for _, event := range events {
		out = append(out, WorkflowInvocationEvent{Sequence: event.Seq, Type: event.Kind, NodeID: event.NodeID,
			AgentID: event.MemberID, Detail: append(json.RawMessage(nil), event.Detail...), At: event.OccurredAt})
	}
	return out, nil
}

func (s *WorkflowInvocationService) List(ctx context.Context, workspaceID, applicationID string) ([]WorkflowInvocation, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT invocation_id FROM weave_capability_workflow_invocations
	 WHERE workspace_id=$1 AND application_id=$2 ORDER BY created_at DESC LIMIT 100`, workspaceID, applicationID)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	out := make([]WorkflowInvocation, 0, len(ids))
	for _, id := range ids {
		view, err := s.Get(ctx, workspaceID, applicationID, id)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	return out, nil
}

func (s *WorkflowInvocationService) Cancel(ctx context.Context, workspaceID, applicationID, credentialID, invocationID string) (WorkflowInvocation, error) {
	if err := s.ready(); err != nil {
		return WorkflowInvocation{}, err
	}
	parsed, err := uuid.Parse(invocationID)
	if err != nil || parsed.String() != invocationID || workspaceID == "" || applicationID == "" || credentialID == "" {
		return WorkflowInvocation{}, ErrInvocationNotFound
	}
	subject, err := execution.RequireSubject(ctx, workspaceID)
	if err != nil || subject.UserID != "" || subject.ServiceID != "capability-app:"+applicationID {
		return WorkflowInvocation{}, ErrAccessDenied
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WorkflowInvocation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lock := invocationLockKey("invocation", workspaceID, applicationID, invocationID)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
		return WorkflowInvocation{}, err
	}
	var allowed bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(
	 SELECT 1 FROM weave_capability_apps a JOIN weave_capability_credentials c
	 ON c.workspace_id=a.workspace_id AND c.app_id=a.id
	 WHERE a.workspace_id=$1 AND a.id=$2 AND a.enabled AND c.id=$3 AND c.revoked_at IS NULL AND 'cancel'=ANY(c.scopes)
	)`, workspaceID, applicationID, credentialID).Scan(&allowed)
	if err != nil {
		return WorkflowInvocation{}, err
	}
	if !allowed {
		return WorkflowInvocation{}, ErrAccessDenied
	}
	if _, err := tx.Exec(ctx, `INSERT INTO weave_capability_workflow_cancel_intents(workspace_id,application_id,invocation_id,requested_by_credential_id)
	 VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, workspaceID, applicationID, invocationID, credentialID); err != nil {
		return WorkflowInvocation{}, err
	}
	record, err := readWorkflowInvocation(ctx, tx, workspaceID, applicationID, invocationID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return WorkflowInvocation{}, err
		}
		return WorkflowInvocation{ApplicationID: applicationID, InvocationID: invocationID,
			Status: "cancelled", ResultState: "unavailable", Trace: WorkflowInvocationTrace{Nodes: []WorkflowInvocationTraceNode{}}}, nil
	}
	if err != nil {
		return WorkflowInvocation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return WorkflowInvocation{}, err
	}
	if status, found, err := s.cancelAssociatedRun(ctx, record); found || err != nil {
		if err != nil {
			return WorkflowInvocation{}, err
		}
		view, err := s.Get(ctx, workspaceID, applicationID, invocationID)
		if err != nil {
			return WorkflowInvocation{}, err
		}
		if view.Status == "pending" {
			view.Status = status
		}
		return view, nil
	}
	// A durable invocation association precedes kernel admission. Reconcile uses
	// the kernel's own close-versus-admit receipt, fencing a cancellation that
	// wins before publication admission.
	_, closed, reconcileErr := s.admissions().Reconcile(ctx, workspaceID, record.AdmissionRequestID, s.associate)
	if reconcileErr == nil && closed {
		return WorkflowInvocation{ApplicationID: applicationID, InvocationID: invocationID, RequestID: record.RequestID,
			CapabilityID: record.CapabilityID, Revision: record.Revision, WorkflowID: record.WorkflowID,
			WorkflowVersion: record.WorkflowVersion, InputHash: record.InputHash,
			Status: "cancelled", ResultState: "unavailable", Trace: WorkflowInvocationTrace{Nodes: []WorkflowInvocationTraceNode{}}}, nil
	}
	if status, found, cancelErr := s.cancelAssociatedRun(ctx, record); found || cancelErr != nil {
		if cancelErr != nil {
			return WorkflowInvocation{}, cancelErr
		}
		view, getErr := s.Get(ctx, workspaceID, applicationID, invocationID)
		if getErr != nil {
			return WorkflowInvocation{}, getErr
		}
		if view.Status == "pending" {
			view.Status = status
		}
		return view, nil
	}
	if reconcileErr != nil {
		return WorkflowInvocation{}, reconcileErr
	}
	return WorkflowInvocation{ApplicationID: applicationID, InvocationID: invocationID, RequestID: record.RequestID,
		CapabilityID: record.CapabilityID, Revision: record.Revision, WorkflowID: record.WorkflowID,
		WorkflowVersion: record.WorkflowVersion, InputHash: record.InputHash,
		Status: "cancelled", ResultState: "unavailable", Trace: WorkflowInvocationTrace{Nodes: []WorkflowInvocationTraceNode{}}}, nil
}

// RevokeGrant closes pending admissions and asks the existing TeamRun owner to
// cancel any accepted invocation for one exact application grant.
func (s *WorkflowInvocationService) RevokeGrant(ctx context.Context, workspaceID, applicationID, capabilityID string, revision int64) error {
	if err := s.ready(); err != nil {
		return err
	}
	if workspaceID == "" || applicationID == "" || (capabilityID == "") != (revision == 0) || revision < 0 {
		return errors.New("workflow grant revocation identity is invalid")
	}
	rows, err := s.pool.Query(ctx, `SELECT i.workspace_id,i.application_id,i.request_id,i.invocation_id,i.credential_id,
	 i.capability_id,i.revision,i.definition_hash,i.workflow_id,i.workflow_version,i.input,i.input_hash,COALESCE(i.lane,''),
	 i.admission_request_id,i.run_id,i.task_id,i.created_at,i.scheduling
	 FROM weave_capability_workflow_invocations i
	 LEFT JOIN weave_workflow_admission_requests a ON a.workspace_id=i.workspace_id AND a.request_id=i.admission_request_id
	 LEFT JOIN weave_kernel_publication_requests k ON k.workspace_id=i.workspace_id AND k.request_id=i.admission_request_id
	 LEFT JOIN weave_team_runs r ON r.workspace_id=i.workspace_id AND r.run_id=i.run_id
	 LEFT JOIN weave_task_queue q ON q.workspace_id=i.workspace_id AND q.id=i.task_id
	 WHERE i.workspace_id=$1 AND i.application_id=$2 AND ($3='' OR (i.capability_id=$3 AND i.revision=$4))
	 AND k.operation IS DISTINCT FROM 'published_closed'
	 AND ((a.receipt IS NULL AND r.run_id IS NULL AND q.id IS NULL) OR r.status IN ('queued','running','parked','cancel_requested')
	   OR q.status IN ('queued','dispatched','running','cancel_requested'))`,
		workspaceID, applicationID, capabilityID, revision)
	if err != nil {
		return err
	}
	records := []workflowInvocationRecord{}
	for rows.Next() {
		record, scanErr := scanWorkflowInvocation(rows)
		if scanErr != nil {
			rows.Close()
			return scanErr
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	appCtx := execution.WithSubject(ctx, execution.Subject{WorkspaceID: workspaceID, ServiceID: "capability-app:" + applicationID})
	admissions := s.admissions()
	for _, record := range records {
		_, closed, reconcileErr := admissions.Reconcile(appCtx, workspaceID, record.AdmissionRequestID, s.associate)
		if reconcileErr == nil && closed {
			continue
		}
		if _, _, cancelErr := s.cancelAssociatedRun(appCtx, record); cancelErr != nil {
			return cancelErr
		}
		if reconcileErr != nil {
			// A previously accepted receipt is still fenced by cancellation via
			// the stored run/task identity when live access has been revoked.
			var operation string
			if stateErr := s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(operation),'') FROM weave_kernel_publication_requests
			 WHERE workspace_id=$1 AND request_id=$2`, workspaceID, record.AdmissionRequestID).Scan(&operation); stateErr != nil {
				return errors.Join(reconcileErr, stateErr)
			} else if operation == "" {
				return reconcileErr
			}
		}
	}
	return nil
}

func (s *WorkflowInvocationService) cancelAssociatedRun(ctx context.Context, record workflowInvocationRecord) (string, bool, error) {
	var status string
	err := s.pool.QueryRow(ctx, `SELECT status FROM weave_team_runs WHERE workspace_id=$1 AND run_id=$2`, record.WorkspaceID, record.RunID).Scan(&status)
	if err == nil {
		if teamrun.Status(status).Terminal() {
			return projectTeamRunStatus(status), true, nil
		}
		cancelled, err := s.canceller.RequestCancel(ctx, teamrun.CancelRequest{
			WorkspaceID: record.WorkspaceID, RunID: record.RunID, CancelActor: "capability-app:" + record.ApplicationID,
			CancelReason: "application_requested", GraceDeadline: time.Now().UTC().Add(30 * time.Second),
			IdempotencyKey: "capability-invocation-cancel-" + record.InvocationID,
		})
		if err != nil {
			return "", true, err
		}
		return projectTeamRunStatus(string(cancelled.Status)), true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	task, taskErr := s.tasks.Get(ctx, record.WorkspaceID, record.TaskID)
	if taskErr != nil {
		if errors.Is(taskErr, pgx.ErrNoRows) || strings.Contains(taskErr.Error(), "not found") {
			return "", false, nil
		}
		return "", false, taskErr
	}
	if task.Status == taskqueue.StatusQueued || task.Status == taskqueue.StatusDispatched || task.Status == taskqueue.StatusRunning {
		if err := s.tasks.Cancel(ctx, record.WorkspaceID, record.TaskID); err != nil {
			return "", false, err
		}
		task, taskErr = s.tasks.Get(ctx, record.WorkspaceID, record.TaskID)
		if taskErr != nil {
			return "", false, taskErr
		}
	}
	switch task.Status {
	case taskqueue.StatusCancelled:
		return "cancelled", true, nil
	case taskqueue.StatusCancelRequested:
		return "cancel_requested", true, nil
	case taskqueue.StatusCompleted:
		return "completed", true, nil
	case taskqueue.StatusFailed, taskqueue.StatusTimedOut:
		return "failed", true, nil
	default:
		return "", false, nil
	}
}

func (s *WorkflowInvocationService) applicationGrantActive(ctx context.Context, record workflowInvocationRecord) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
	 SELECT 1 FROM weave_capability_apps a JOIN weave_capability_grants g
	   ON g.workspace_id=a.workspace_id AND g.app_id=a.id
	 WHERE a.workspace_id=$1 AND a.id=$2 AND a.enabled AND g.capability_id=$3 AND g.revision=$4 AND g.enabled
	)`, record.WorkspaceID, record.ApplicationID, record.CapabilityID, record.Revision).Scan(&active)
	return active, err
}

// AuthorizeRuntimeCredential rechecks the invocation's exact application grant
// each time TeamRun resolves a frozen provider reference for a new attempt.
func (s *WorkflowInvocationService) AuthorizeRuntimeCredential(
	ctx context.Context,
	run teamrun.TeamRun,
	task *taskqueue.Task,
	subject execution.Subject,
	ref frozen.CredentialReference,
) error {
	if err := s.ready(); err != nil {
		return err
	}
	if queries := kernelcredentials.ReferenceAuthorizationQueriesFromContext(ctx); queries != nil {
		return s.authorizeRuntimeCredentialWithQueries(ctx, queries, run, task, subject, ref)
	}
	// Direct callers without a credential resolver transaction keep the same
	// live checks, but runtime credential resolution reuses its existing tx via
	// ReferenceAuthorizationQueriesFromContext above.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	return s.authorizeRuntimeCredentialWithQueries(ctx, tx, run, task, subject, ref)
}

type workflowTaskAuthorizationIdentity struct {
	ID              string
	WorkspaceID     string
	RunSnapshotID   string
	WorkflowID      string
	Kind            string
	WorkflowVersion int
	IdentitySchema  int
	IdentityKind    taskqueue.IdentityKind
	Subject         execution.Subject
}

// taskAuthorizationColumns selects the identity of the task joined as alias,
// led by whether the task exists.
func taskAuthorizationColumns(alias string) string {
	return alias + `.id IS NOT NULL,COALESCE(` + alias + `.id,''),COALESCE(` + alias + `.workspace_id,''),
	 COALESCE(` + alias + `.run_snapshot_id,''),COALESCE(` + alias + `.workflow_id,''),COALESCE(` + alias + `.workflow_version,0),
	 COALESCE(` + alias + `.kind,''),COALESCE(` + alias + `.identity_schema_version,0),COALESCE(` + alias + `.identity_kind,''),` + alias + `.actor_subject`
}

type scannedTaskAuthorizationIdentity struct {
	present      bool
	identity     workflowTaskAuthorizationIdentity
	kind         string
	actorSubject []byte
}

func (s *scannedTaskAuthorizationIdentity) targets() []any {
	return []any{&s.present, &s.identity.ID, &s.identity.WorkspaceID, &s.identity.RunSnapshotID, &s.identity.WorkflowID,
		&s.identity.WorkflowVersion, &s.identity.Kind, &s.identity.IdentitySchema, &s.kind, &s.actorSubject}
}

// result is the task's identity, or an error when the task does not exist.
func (s *scannedTaskAuthorizationIdentity) result() (workflowTaskAuthorizationIdentity, error) {
	if !s.present {
		return workflowTaskAuthorizationIdentity{}, pgx.ErrNoRows
	}
	identity := s.identity
	identity.IdentityKind = taskqueue.IdentityKind(s.kind)
	if err := json.Unmarshal(s.actorSubject, &identity.Subject); err != nil {
		return workflowTaskAuthorizationIdentity{}, err
	}
	return identity, nil
}

func (s *WorkflowInvocationService) authorizeRuntimeCredentialWithQueries(
	ctx context.Context,
	queries kernelcredentials.ReferenceAuthorizationQueries,
	run teamrun.TeamRun,
	task *taskqueue.Task,
	subject execution.Subject,
	ref frozen.CredentialReference,
) error {
	if err := s.ready(); err != nil {
		return err
	}
	if task == nil || run.WorkspaceID == "" || run.RunID == "" || run.SourceTaskID == "" || task.WorkspaceID != run.WorkspaceID ||
		task.RunSnapshotID != run.RunSnapshotID || subject.WorkspaceID != run.WorkspaceID || subject.UserID != "" ||
		!strings.HasPrefix(subject.ServiceID, "capability-app:") || ref.Kind != frozen.CredentialProviderAPIKey ||
		ref.Scope != frozen.CredentialScopeWorkspaceService || ref.WorkspaceID != run.WorkspaceID {
		return frozen.ErrCredentialSubjectDenied
	}
	applicationID := strings.TrimPrefix(subject.ServiceID, "capability-app:")
	if queries == nil {
		return frozen.ErrCredentialSubjectDenied
	}
	// One read takes the invocation, share-locked, with the current and
	// source task identities and the frozen definition it is checked against.
	var record workflowInvocationRecord
	var scheduling, definitionRaw []byte
	var current, source scannedTaskAuthorizationIdentity
	targets := []any{&record.WorkspaceID, &record.ApplicationID, &record.RequestID, &record.InvocationID,
		&record.CredentialID, &record.CapabilityID, &record.Revision, &record.DefinitionHash,
		&record.WorkflowID, &record.WorkflowVersion, &record.Input, &record.InputHash,
		&record.Lane, &record.AdmissionRequestID, &record.RunID, &record.TaskID, &record.CreatedAt, &scheduling}
	targets = append(append(append(targets, current.targets()...), source.targets()...), &definitionRaw)
	err := queries.QueryRow(ctx, `SELECT i.workspace_id,i.application_id,i.request_id,i.invocation_id,i.credential_id,
	 i.capability_id,i.revision,i.definition_hash,i.workflow_id,i.workflow_version,i.input,i.input_hash,COALESCE(i.lane,''),
	 i.admission_request_id,i.run_id,i.task_id,i.created_at,i.scheduling,
	 `+taskAuthorizationColumns("qc")+`,`+taskAuthorizationColumns("qs")+`,rev.definition
	 FROM weave_capability_workflow_invocations i
	 LEFT JOIN weave_task_queue qc ON qc.workspace_id=$4 AND qc.id=$5
	 LEFT JOIN weave_task_queue qs ON qs.workspace_id=i.workspace_id AND qs.id=$6
	 LEFT JOIN weave_capability_revisions rev ON rev.workspace_id=i.workspace_id AND rev.capability_id=i.capability_id
	   AND rev.revision=i.revision AND rev.definition_hash=i.definition_hash
	 WHERE i.workspace_id=$1 AND i.application_id=$2 AND i.run_id=$3 FOR SHARE OF i`,
		run.WorkspaceID, applicationID, run.RunID, task.WorkspaceID, task.ID, run.SourceTaskID).Scan(targets...)
	if err == nil && len(scheduling) > 0 {
		err = json.Unmarshal(scheduling, &record.Scheduling)
	}
	if err != nil {
		return frozen.ErrCredentialSubjectDenied
	}
	if record.TaskID != run.SourceTaskID || record.WorkflowID != run.WorkflowID || record.WorkflowVersion != run.WorkflowVersion {
		return frozen.ErrCredentialSubjectDenied
	}
	currentTask, err := current.result()
	if err != nil || currentTask.ID != task.ID || currentTask.WorkspaceID != run.WorkspaceID ||
		currentTask.RunSnapshotID != run.RunSnapshotID || currentTask.WorkflowID != run.WorkflowID ||
		currentTask.WorkflowVersion != run.WorkflowVersion || currentTask.Kind != task.Kind ||
		currentTask.IdentityKind != task.IdentityKind || currentTask.IdentitySchema != task.IdentitySchemaVersion ||
		currentTask.Subject != subject || task.Subject != subject {
		return frozen.ErrCredentialSubjectDenied
	}
	sourceTask, err := source.result()
	if err != nil || sourceTask.ID != record.TaskID || sourceTask.WorkspaceID != run.WorkspaceID ||
		sourceTask.RunSnapshotID != run.RunSnapshotID || sourceTask.WorkflowID != run.WorkflowID ||
		sourceTask.WorkflowVersion != run.WorkflowVersion || sourceTask.Kind != "team_workflow" ||
		sourceTask.IdentityKind != taskqueue.IdentityTeamWorkflow || sourceTask.IdentitySchema != 2 ||
		sourceTask.Subject != subject {
		return frozen.ErrCredentialSubjectDenied
	}
	if err := authorizeApplicationGrant(ctx, queries, grantIdentity{WorkspaceID: record.WorkspaceID,
		ApplicationID: record.ApplicationID, CapabilityID: record.CapabilityID, Revision: record.Revision}); err != nil {
		return ErrAccessDenied
	}
	if definitionRaw == nil {
		return ErrAccessDenied
	}
	var definition capability.Definition
	if json.Unmarshal(definitionRaw, &definition) != nil || definition.Workflow == nil ||
		definition.Workflow.WorkflowID != record.WorkflowID || definition.Workflow.WorkflowVersion != record.WorkflowVersion {
		return frozen.ErrCredentialSubjectDenied
	}
	return nil
}

func projectTeamRunStatus(status string) string {
	switch teamrun.Status(status) {
	case teamrun.StatusSucceeded:
		return "completed"
	case teamrun.StatusFailed, teamrun.StatusAbandoned:
		return "failed"
	case teamrun.StatusCancelled:
		return "cancelled"
	default:
		return status
	}
}

func (s *WorkflowInvocationService) readWorkflowEnvelope(ctx context.Context, record workflowInvocationRecord) (frozen.ArtifactEnvelopeV1, error) {
	artifact, err := workflow.NewArtifactStore(s.pool, nil).GetArtifact(ctx, record.WorkspaceID, record.WorkflowID, record.WorkflowVersion)
	if err != nil {
		return frozen.ArtifactEnvelopeV1{}, err
	}
	return frozen.ArtifactEnvelopeV1{WorkspaceID: artifact.WorkspaceID, WorkflowID: artifact.WorkflowID,
		WorkflowVersion: artifact.WorkflowVersion, ArtifactSchemaVersion: artifact.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm, CanonicalizationVersion: artifact.CanonicalizationVersion,
		HashAlgorithm: artifact.HashAlgorithm, ContentHash: artifact.ContentHash, Payload: artifact.Payload}, nil
}

func workflowResultProducer(payload frozen.ArtifactPayloadV1, graph machine.GraphDefinition) (string, string, string) {
	nodes := make(map[string]machine.Node, len(graph.Nodes))
	var delivery *machine.Node
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
		if node.Type == machine.NodeDeliver {
			copy := node
			delivery = &copy
		}
	}
	if delivery == nil {
		return "", "", ""
	}
	config, ok := delivery.Config.(machine.DeliverConfig)
	if !ok || config.Result.Source != machine.ValueNodeOutput {
		return "", "", ""
	}
	producers := map[machine.AgentVersionKey]bool{}
	visiting := map[string]bool{}
	hasOtherSource := false
	var collectNode func(string)
	var collectRef func(machine.ValueRef)
	collectNode = func(nodeID string) {
		if visiting[nodeID] {
			hasOtherSource = true
			return
		}
		node, exists := nodes[nodeID]
		if !exists {
			hasOtherSource = true
			return
		}
		switch node.Type {
		case machine.NodeLead, machine.NodeWorker:
			agentID, version := nodeAgentIdentity(node, payload)
			if agentID == "" || version < 1 {
				hasOtherSource = true
				return
			}
			producers[machine.AgentVersionKey{AgentID: agentID, AgentVersion: version}] = true
		case machine.NodeTransform:
			transform, valid := node.Config.(machine.TransformConfig)
			if !valid {
				hasOtherSource = true
				return
			}
			visiting[nodeID] = true
			switch transform.Operation {
			case machine.TransformIdentity:
				if transform.Value == nil {
					hasOtherSource = true
				} else {
					collectRef(*transform.Value)
				}
			case machine.TransformObject:
				if len(transform.Fields) == 0 {
					hasOtherSource = true
				}
				for _, reference := range transform.Fields {
					collectRef(reference)
				}
			case machine.TransformArray:
				if len(transform.Items) == 0 {
					hasOtherSource = true
				}
				for _, reference := range transform.Items {
					collectRef(reference)
				}
			default:
				hasOtherSource = true
			}
			delete(visiting, nodeID)
		default:
			hasOtherSource = true
		}
	}
	collectRef = func(ref machine.ValueRef) {
		if ref.Source == machine.ValueNodeOutput {
			collectNode(ref.NodeID)
		} else {
			hasOtherSource = true
		}
		if ref.Default != nil {
			hasOtherSource = true
			collectRef(*ref.Default)
		}
	}
	collectRef(config.Result)
	if hasOtherSource || len(producers) != 1 {
		return "", "", ""
	}
	var producer machine.AgentVersionKey
	for producer = range producers {
		break
	}
	for _, bundle := range payload.Bundles {
		if bundle.Agent.AgentID != producer.AgentID || bundle.Agent.AgentVersion != producer.AgentVersion {
			continue
		}
		model := bundle.Agent.Model
		if model == "" {
			model = bundle.PrimaryModel.ModelID
		}
		return bundle.Agent.AgentID, model, bundle.Agent.Engine
	}
	return "", "", ""
}

func (s *WorkflowInvocationService) trace(
	ctx context.Context,
	record workflowInvocationRecord,
	payload frozen.ArtifactPayloadV1,
	graph machine.GraphDefinition,
	runStatus string,
	result json.RawMessage,
) WorkflowInvocationTrace {
	completed := map[string]json.RawMessage{}
	var checkpointRaw []byte
	if err := s.pool.QueryRow(ctx, `SELECT value FROM loom_store WHERE namespace=$1 AND key=$2`,
		teamrun.CheckpointNamespace(record.WorkspaceID), teamrun.CheckpointKey(record.RunID)).Scan(&checkpointRaw); err == nil {
		if checkpoint, decodeErr := teamrun.DecodeWorkflowCheckpointV1(checkpointRaw); decodeErr == nil {
			completed = checkpoint.CompletedOutputs
		}
	}
	stageOutputs := s.readWorkflowStageOutputs(ctx, record, graph)
	activityStatus := map[string]string{}
	activityElapsed := map[string]int64{}
	if events, err := (&teamrun.PGActivityStore{Transactions: s.pool}).List(ctx, record.WorkspaceID, record.RunID, 1000); err == nil {
		for _, event := range events {
			if event.NodeID == "" {
				continue
			}
			switch event.Kind {
			case "member_started":
				activityStatus[event.NodeID] = "running"
			case "member_completed":
				activityStatus[event.NodeID] = "completed"
				var detail struct {
					DurationMS int64 `json:"duration_ms"`
				}
				_ = json.Unmarshal(event.Detail, &detail)
				activityElapsed[event.NodeID] = detail.DurationMS
			case "member_failed":
				activityStatus[event.NodeID] = "failed"
			case "member_paused":
				activityStatus[event.NodeID] = "parked"
			}
		}
	}
	bundles := make(map[string]frozen.FrozenExecutionBundle, len(payload.Bundles))
	for _, bundle := range payload.Bundles {
		bundles[agentBundleKey(bundle.Agent.AgentID, bundle.Agent.AgentVersion)] = bundle
	}
	trace := WorkflowInvocationTrace{Nodes: make([]WorkflowInvocationTraceNode, 0, len(graph.Nodes))}
	for _, node := range graph.Nodes {
		label := strings.TrimSpace(node.Label)
		if label == "" {
			label = node.ID
		}
		item := WorkflowInvocationTraceNode{NodeID: node.ID, Label: label, Kind: string(node.Type), Status: "pending"}
		agentID, agentVersion := nodeAgentIdentity(node, payload)
		if agentID != "" {
			item.AgentID = agentID
			if bundle, ok := bundles[agentBundleKey(agentID, agentVersion)]; ok {
				item.AgentName = bundle.Agent.DisplayName
				if item.AgentName == "" {
					item.AgentName = bundle.Agent.Name
				}
				item.Model = bundle.Agent.Model
				if item.Model == "" {
					item.Model = bundle.PrimaryModel.ModelID
				}
			}
		}
		if output, ok := completed[node.ID]; ok {
			item.Status = "completed"
			item.Output = append(json.RawMessage(nil), output...)
		} else if output, ok := stageOutputs[node.ID]; ok {
			item.Output = append(json.RawMessage(nil), output...)
		}
		if status, ok := activityStatus[node.ID]; ok {
			item.Status = status
			item.ElapsedMS = activityElapsed[node.ID]
		}
		if node.Type == machine.NodeDeliver && runStatus == "completed" && len(result) > 0 {
			item.Status = "completed"
			item.Output = append(json.RawMessage(nil), result...)
		}
		trace.Nodes = append(trace.Nodes, item)
	}
	return trace
}

// readWorkflowStageOutputs projects immutable stage deliverables created by
// the published TeamRun itself. The app/run/snapshot join is the invocation
// binding; deliverable owner columns are not used as application authority.
func (s *WorkflowInvocationService) readWorkflowStageOutputs(
	ctx context.Context,
	record workflowInvocationRecord,
	graph machine.GraphDefinition,
) map[string]json.RawMessage {
	nodeTypes := make(map[string]string, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodeTypes[node.ID] = string(node.Type)
	}
	rows, err := s.pool.Query(ctx, `SELECT d.metadata->>'node_id',d.metadata->>'node_type',d.content_type,d.content
	 FROM weave_final_deliverables d
	 JOIN weave_team_run_snapshots snapshot ON snapshot.workspace_id=d.workspace_id AND snapshot.run_id=d.run_snapshot_id
	 WHERE d.workspace_id=$1 AND d.run_id=$2 AND d.run_snapshot_id=$3
	   AND snapshot.mode='fixed_workflow' AND snapshot.workflow_id=$4 AND snapshot.workflow_version=$5
	   AND snapshot.artifact_workflow_id=$4 AND snapshot.artifact_workflow_version=$5
	   AND snapshot.trigger_source_v2->>'type'='manual'
	   AND snapshot.trigger_source_v2->>'source_ref'=$6
	   AND d.metadata->>'source'='published_workflow' AND d.metadata->>'artifact_kind'='stage'
	   AND COALESCE(d.metadata->>'filename','')=''`, record.WorkspaceID, record.RunID, record.RunID,
		record.WorkflowID, record.WorkflowVersion, record.ApplicationID)
	if err != nil {
		return map[string]json.RawMessage{}
	}
	defer rows.Close()
	outputs := map[string]json.RawMessage{}
	ambiguous := map[string]bool{}
	for rows.Next() {
		var nodeID, nodeType, contentType, content string
		if rows.Scan(&nodeID, &nodeType, &contentType, &content) != nil || nodeID == "" || nodeTypes[nodeID] != nodeType || ambiguous[nodeID] {
			continue
		}
		encoded, err := workflowDeliverableJSON(contentType, content)
		if err != nil {
			continue
		}
		if previous, exists := outputs[nodeID]; exists && !bytes.Equal(previous, encoded) {
			delete(outputs, nodeID)
			ambiguous[nodeID] = true
			continue
		}
		outputs[nodeID] = encoded
	}
	return outputs
}

func workflowDeliverableJSON(contentType, content string) (json.RawMessage, error) {
	if contentType == "application/json" {
		value := json.RawMessage(bytes.TrimSpace([]byte(content)))
		if len(value) == 0 || !json.Valid(value) {
			return nil, errors.New("stage deliverable contains invalid JSON")
		}
		return append(json.RawMessage(nil), value...), nil
	}
	encoded, err := json.Marshal(content)
	return json.RawMessage(encoded), err
}

func nodeAgentIdentity(node machine.Node, payload frozen.ArtifactPayloadV1) (string, int64) {
	switch config := node.Config.(type) {
	case machine.LeadConfig:
		return payload.Team.LeadAgentID, payload.Team.LeadAgentVersion
	case machine.WorkerConfig:
		return config.AgentID, config.AgentVersion
	default:
		return "", 0
	}
}

func agentBundleKey(agentID string, version int64) string {
	return fmt.Sprintf("%s/%d", agentID, version)
}

func validateWorkflowCapabilityContracts(envelope frozen.ArtifactEnvelopeV1) error {
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return err
	}
	trigger, report := machine.DecodeTriggerConfigV1(payload.TriggerConfig)
	if report != nil && len(report.Issues) != 0 {
		return fmt.Errorf("%w: workflow trigger is invalid", capability.ErrInvalidDefinition)
	}
	if trigger.Type != machine.TriggerConversationExplicit || trigger.Delivery != nil {
		return fmt.Errorf("%w: workflow invocation requires a conversation_explicit trigger without delivery", capability.ErrInvalidDefinition)
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) != 0 {
		return fmt.Errorf("%w: workflow graph is invalid", capability.ErrInvalidDefinition)
	}
	if graph.OutputContract.Type != machine.ValueJSON || len(graph.OutputContract.Schema) == 0 {
		return fmt.Errorf("%w: workflow output must declare a JSON schema", capability.ErrInvalidDefinition)
	}
	// The frozen machine contract remains within the executable schema subset;
	// the capability contract may be stricter JSON Schema and is enforced over
	// the final TeamRun result in Get. Avoid redefining or narrowing that schema
	// algebra here.
	if graph.InputContract.Type != machine.ValueJSON && graph.InputContract.Type != machine.ValueText {
		return fmt.Errorf("%w: workflow input must accept JSON or text", capability.ErrInvalidDefinition)
	}
	if graph.InputContract.Type == machine.ValueJSON && len(graph.InputContract.Schema) > 0 {
		if problems := machine.ValidateRuntimeInputSchema(graph.InputContract.Schema); len(problems) != 0 {
			return fmt.Errorf("%w: workflow input schema is invalid", capability.ErrInvalidDefinition)
		}
	}
	return nil
}

// Keep the compile-time dependency on the catalog package visible: this is the
// published workflow catalog's application boundary, not an arbitrary URL.
var _ PublishedWorkflowResolver = (*workflowcatalog.Store)(nil)
