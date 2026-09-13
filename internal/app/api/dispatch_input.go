package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

type dispatchInputSourceMessage struct {
	MessageID string `json:"message_id"`
	EventSeq  *int64 `json:"event_seq"`
	SHA256    string `json:"sha256"`
}

type dispatchInputRegistration struct {
	RegistrationID     string                       `json:"registration_id"`
	WorkbenchSessionID string                       `json:"workbench_session_id"`
	ExpectedRevisionID string                       `json:"expected_revision_id"`
	SourceMessages     []dispatchInputSourceMessage `json:"source_messages"`
	Task               string                       `json:"task"`
	TeamID             string                       `json:"team_id"`
	Mode               string                       `json:"mode,omitempty"`
	WorkflowID         string                       `json:"workflow_id,omitempty"`
	WorkflowVersion    *int                         `json:"workflow_version,omitempty"`
	ProjectID          string                       `json:"project_id,omitempty"`
	RevisionContext    *dispatchRevisionContext     `json:"revision_context,omitempty"`
}

type dispatchRevisionContext struct {
	ParentInputRevisionID string `json:"parent_input_revision_id"`
	ParentRunID           string `json:"parent_run_id"`
}

type dispatchRevisionMaterial struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
}

type dispatchInputReceipt struct {
	InputRevisionID string `json:"input_revision_id"`
	ClientRequestID string `json:"client_request_id"`
	TaskSHA256      string `json:"task_sha256"`
}

type dispatchInputRevision struct {
	dispatchInputReceipt
	WorkbenchSessionID    string
	RegistrationSHA256    string
	Task                  string
	ExecutionTask         string
	TeamID                string
	Mode                  string
	WorkflowID            string
	WorkflowVersion       int
	ProjectID             string
	IsCurrent             bool
	IsClosed              bool
	ConsumedRunID         string
	ConsumedTaskID        string
	DeliveryContract      json.RawMessage
	RevisionKind          string
	RootInputRevisionID   string
	ParentInputRevisionID string
	ParentRunID           string
	ParentDeliveryDigest  string
	ParentMaterials       json.RawMessage
}

const dispatchInputColumns = `input_revision_id, client_request_id, task_sha256,
	workbench_session_id, registration_sha256, task, team_id, mode, workflow_id,
	workflow_version, project_id, is_current, closed_at IS NOT NULL, COALESCE(consumed_run_id,''), COALESCE(consumed_task_id,''), delivery_contract,
	execution_task, revision_kind, root_input_revision_id, COALESCE(parent_input_revision_id,''), COALESCE(parent_run_id,''),
	COALESCE(parent_delivery_digest,''), parent_materials`

func scanDispatchInput(row pgx.Row) (dispatchInputRevision, error) {
	var input dispatchInputRevision
	err := row.Scan(&input.InputRevisionID, &input.ClientRequestID, &input.TaskSHA256,
		&input.WorkbenchSessionID, &input.RegistrationSHA256, &input.Task, &input.TeamID, &input.Mode,
		&input.WorkflowID, &input.WorkflowVersion, &input.ProjectID, &input.IsCurrent, &input.IsClosed,
		&input.ConsumedRunID, &input.ConsumedTaskID, &input.DeliveryContract, &input.ExecutionTask,
		&input.RevisionKind, &input.RootInputRevisionID, &input.ParentInputRevisionID, &input.ParentRunID,
		&input.ParentDeliveryDigest, &input.ParentMaterials)
	return input, err
}

func (input dispatchInputRevision) dispatchRequest() teamDispatchRequest {
	return teamDispatchRequest{
		InputRevisionID: input.InputRevisionID, Task: input.ExecutionTask, Mode: input.Mode,
		WorkflowID: input.WorkflowID, WorkflowVersion: &input.WorkflowVersion,
		ClientRequestID: input.ClientRequestID, ProjectID: input.ProjectID, inputBinding: &input,
	}
}

func loadRevisionMaterials(ctx context.Context, tx pgx.Tx, workspaceID, userID, runID string) ([]dispatchRevisionMaterial, []string, error) {
	rows, err := tx.Query(ctx, `SELECT id,title,content_type,content FROM weave_final_deliverables
		WHERE workspace_id=$1 AND user_id=$2 AND run_id=$3
			AND COALESCE(metadata->>'artifact_kind','final')='final'
		ORDER BY created_at,id`, workspaceID, userID, runID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	materials, contents := []dispatchRevisionMaterial{}, []string{}
	for rows.Next() {
		var item dispatchRevisionMaterial
		var content string
		if err := rows.Scan(&item.ID, &item.Title, &item.ContentType, &content); err != nil {
			return nil, nil, err
		}
		item.SHA256 = dispatchInputDigest([]byte(content))
		materials = append(materials, item)
		contents = append(contents, content)
	}
	return materials, contents, rows.Err()
}

func assembleRevisionTask(original, change string, materials []dispatchRevisionMaterial, contents []string) (string, string, error) {
	manifest, _ := json.Marshal(materials)
	var body strings.Builder
	body.WriteString("This is a revision of an existing task. Use all three server-authoritative sections below.\n\n[Original task]\n")
	body.WriteString(original)
	body.WriteString("\n\n[Previous final deliverables]\n")
	for index, item := range materials {
		fmt.Fprintf(&body, "\n--- %s (%s, %s) ---\n%s\n", item.Title, item.ID, item.ContentType, contents[index])
	}
	body.WriteString("\n[User revision request]\n")
	body.WriteString(change)
	body.WriteString("\n\nProduce a revised complete deliverable. Do not treat the revision request as a standalone task.")
	if body.Len() > 1<<20 {
		return "", "", errors.New("revision materials exceed the dispatch input limit")
	}
	return body.String(), dispatchInputDigest(manifest), nil
}

func dispatchInputDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func validDispatchInputSourceMessages(messages []dispatchInputSourceMessage) bool {
	if len(messages) == 0 || len(messages) > 256 {
		return false
	}
	seen := make(map[string]bool, len(messages))
	previousSeq := int64(-1)
	for _, message := range messages {
		if strings.TrimSpace(message.MessageID) == "" || len(message.MessageID) > 256 ||
			strings.ContainsRune(message.MessageID, '\x00') || seen[message.MessageID] ||
			message.EventSeq == nil || *message.EventSeq < 0 || *message.EventSeq <= previousSeq || len(message.SHA256) != 64 {
			return false
		}
		if _, err := hex.DecodeString(message.SHA256); err != nil {
			return false
		}
		seen[message.MessageID] = true
		previousSeq = *message.EventSeq
	}
	return true
}

// Only the trusted Workbench Host calls this route. The Host reads persisted
// user events and reuses its existing confirmation flow; this endpoint neither
// infers authorization from language nor proves the supplied source hashes.
// It is intentionally absent from the model-facing MCP tool catalog. Raw HTTP
// dispatch remains a compatibility path outside this input-binding guarantee.
func (s *Server) handleRegisterDispatchInput(c echo.Context) error {
	if s.GetPool() == nil || s.ScheduleTransactions == nil {
		return workflowError(c, http.StatusServiceUnavailable, "dispatch_input_unavailable", "dispatch input storage unavailable")
	}
	workspaceID, userID := getTenant(c), getUserID(c)
	if workspaceID == "" || userID == "" {
		return workflowError(c, http.StatusUnauthorized, "dispatch_input_identity_required", "dispatch input requires an authenticated user")
	}
	var request dispatchInputRegistration
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowError(c, http.StatusBadRequest, "dispatch_input_request_invalid", "dispatch input request invalid")
	}
	registrationID, err := uuid.Parse(request.RegistrationID)
	if err != nil || strings.TrimSpace(request.WorkbenchSessionID) == "" || len(request.WorkbenchSessionID) > 256 ||
		strings.ContainsRune(request.WorkbenchSessionID, '\x00') || strings.TrimSpace(request.TeamID) == "" ||
		strings.TrimSpace(request.Task) == "" || len(request.Task) > 1<<20 || strings.ContainsRune(request.Task, '\x00') ||
		!validDispatchInputSourceMessages(request.SourceMessages) || (request.WorkflowVersion != nil && *request.WorkflowVersion <= 0) {
		return workflowError(c, http.StatusBadRequest, "dispatch_input_request_invalid", "dispatch input request invalid")
	}
	request.RegistrationID = registrationID.String()
	if request.ExpectedRevisionID != "" {
		revisionID, err := uuid.Parse(request.ExpectedRevisionID)
		if err != nil {
			return workflowError(c, http.StatusBadRequest, "dispatch_input_request_invalid", "expected revision must be a UUID")
		}
		request.ExpectedRevisionID = revisionID.String()
	}
	if request.RevisionContext != nil {
		parentRevisionID, revisionErr := uuid.Parse(request.RevisionContext.ParentInputRevisionID)
		if revisionErr != nil || strings.TrimSpace(request.RevisionContext.ParentRunID) == "" {
			return workflowError(c, http.StatusBadRequest, "dispatch_revision_context_invalid", "revision source is invalid")
		}
		request.RevisionContext.ParentInputRevisionID = parentRevisionID.String()
		request.RevisionContext.ParentRunID = strings.TrimSpace(request.RevisionContext.ParentRunID)
	}
	request.TeamID = strings.TrimSpace(request.TeamID)
	request.WorkflowID = strings.TrimSpace(request.WorkflowID)
	request.ProjectID = strings.TrimSpace(request.ProjectID)
	request.Mode = strings.TrimSpace(request.Mode)
	if request.Mode == "" {
		request.Mode = teamDispatchModeWorkflow
	}
	if request.Mode != teamDispatchModeWorkflow {
		return workflowError(c, http.StatusBadRequest, "dispatch_input_mode_unsupported", "bound dispatch currently requires a fixed workflow")
	}
	encoded, _ := json.Marshal(request)
	registrationSHA256 := dispatchInputDigest(encoded)
	ctx := c.Request().Context()
	tx, err := s.ScheduleTransactions.Begin(ctx)
	if err != nil {
		return workflowStoreFailure(c, fmt.Errorf("begin dispatch input registration: %w", err))
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockDispatchInputSession(ctx, tx, workspaceID, userID, request.WorkbenchSessionID); err != nil {
		return workflowStoreFailure(c, err)
	}
	existing, err := scanDispatchInput(tx.QueryRow(ctx, `SELECT `+dispatchInputColumns+`
		FROM weave_dispatch_input_revisions
		WHERE workspace_id=$1 AND user_id=$2 AND registration_id=$3`,
		workspaceID, userID, request.RegistrationID))
	if err == nil {
		if existing.RegistrationSHA256 != registrationSHA256 {
			return workflowError(c, http.StatusConflict, "input_registration_conflict", "registration_id was already used for different input facts")
		}
		// A replay returns its original receipt without reactivating an old head,
		// and remains available if the selected team has changed since dispatch.
		return c.JSON(http.StatusOK, existing.dispatchInputReceipt)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return workflowStoreFailure(c, fmt.Errorf("read dispatch input registration: %w", err))
	}
	var currentRevisionID string
	err = tx.QueryRow(ctx, `SELECT input_revision_id FROM weave_dispatch_input_revisions
		WHERE workspace_id=$1 AND user_id=$2 AND workbench_session_id=$3 AND is_current FOR UPDATE`,
		workspaceID, userID, request.WorkbenchSessionID).Scan(&currentRevisionID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return workflowStoreFailure(c, fmt.Errorf("read dispatch input head: %w", err))
	}
	if currentRevisionID != request.ExpectedRevisionID {
		return workflowError(c, http.StatusConflict, "input_revision_conflict", "current dispatch input revision changed")
	}
	executionTask, revisionKind, rootRevisionID := request.Task, "initial", ""
	parentRevisionID, parentRunID, parentDeliveryDigest := "", "", ""
	parentMaterialsJSON := []byte(`[]`)
	var inheritedContract json.RawMessage
	if request.RevisionContext != nil {
		if currentRevisionID == "" || request.RevisionContext.ParentInputRevisionID != currentRevisionID {
			return workflowError(c, http.StatusConflict, "dispatch_revision_source_changed", "the task selected for revision is no longer current")
		}
		parent, parentErr := scanDispatchInput(tx.QueryRow(ctx, `SELECT `+dispatchInputColumns+` FROM weave_dispatch_input_revisions
			WHERE workspace_id=$1 AND user_id=$2 AND input_revision_id=$3 FOR UPDATE`, workspaceID, userID, currentRevisionID))
		if parentErr != nil {
			return workflowStoreFailure(c, fmt.Errorf("read revision parent: %w", parentErr))
		}
		if parent.ConsumedRunID == "" || parent.ConsumedRunID != request.RevisionContext.ParentRunID || parent.TeamID != request.TeamID {
			return workflowError(c, http.StatusConflict, "dispatch_revision_source_mismatch", "the selected prior run does not match this task")
		}
		materials, contents, materialErr := loadRevisionMaterials(ctx, tx, workspaceID, userID, parent.ConsumedRunID)
		if materialErr != nil {
			return workflowStoreFailure(c, fmt.Errorf("read revision materials: %w", materialErr))
		}
		if len(materials) == 0 {
			return workflowError(c, http.StatusConflict, "dispatch_revision_materials_missing", "the prior run has no final deliverable to revise")
		}
		var root dispatchInputRevision
		root, parentErr = scanDispatchInput(tx.QueryRow(ctx, `SELECT `+dispatchInputColumns+` FROM weave_dispatch_input_revisions
			WHERE workspace_id=$1 AND user_id=$2 AND input_revision_id=$3`, workspaceID, userID, parent.RootInputRevisionID))
		if parentErr != nil {
			return workflowStoreFailure(c, fmt.Errorf("read revision root: %w", parentErr))
		}
		executionTask, parentDeliveryDigest, parentErr = assembleRevisionTask(root.Task, request.Task, materials, contents)
		if parentErr != nil {
			return workflowError(c, http.StatusRequestEntityTooLarge, "dispatch_revision_materials_too_large", parentErr.Error())
		}
		parentMaterialsJSON, _ = json.Marshal(materials)
		revisionKind, rootRevisionID = "revision", parent.RootInputRevisionID
		parentRevisionID, parentRunID, inheritedContract = parent.InputRevisionID, parent.ConsumedRunID, parent.DeliveryContract
	}
	workflowID, version, handled, err := resolveRegisteredDispatchWorkflow(c, tx, request)
	if handled || err != nil {
		return err
	}
	deliveryContract, err := parseDispatchDeliveryContract(request.Task)
	if err != nil {
		return workflowError(c, http.StatusBadRequest, "dispatch_delivery_contract_invalid", err.Error())
	}
	deliveryContractJSON := []byte(`{}`)
	if deliveryContract != nil {
		deliveryContractJSON, err = json.Marshal(deliveryContract)
		if err != nil {
			return workflowError(c, http.StatusBadRequest, "dispatch_delivery_contract_invalid", "delivery contract is invalid")
		}
	} else if revisionKind == "revision" && len(inheritedContract) > 0 {
		deliveryContractJSON = inheritedContract
	}
	receipt := dispatchInputReceipt{
		InputRevisionID: uuid.NewString(), ClientRequestID: uuid.NewString(), TaskSHA256: dispatchInputDigest([]byte(request.Task)),
	}
	if rootRevisionID == "" {
		rootRevisionID = receipt.InputRevisionID
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_dispatch_input_revisions SET is_current=false
		WHERE workspace_id=$1 AND user_id=$2 AND workbench_session_id=$3 AND is_current`,
		workspaceID, userID, request.WorkbenchSessionID); err != nil {
		return workflowStoreFailure(c, fmt.Errorf("supersede dispatch input revision: %w", err))
	}
	sources, _ := json.Marshal(request.SourceMessages)
	inserted, err := tx.Exec(ctx, `INSERT INTO weave_dispatch_input_revisions
		(workspace_id,user_id,workbench_session_id,input_revision_id,registration_id,registration_sha256,
		 source_messages,task,task_sha256,team_id,mode,workflow_id,workflow_version,project_id,client_request_id,delivery_contract,
		 execution_task,revision_kind,root_input_revision_id,parent_input_revision_id,parent_run_id,parent_delivery_digest,parent_materials)
		VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10,$11,$12,$13,$14,$15,$16::jsonb,$17,$18,$19,NULLIF($20,''),NULLIF($21,''),NULLIF($22,''),$23::jsonb)
		ON CONFLICT (workspace_id,user_id,registration_id) DO NOTHING`,
		workspaceID, userID, request.WorkbenchSessionID, receipt.InputRevisionID, request.RegistrationID, registrationSHA256,
		string(sources), request.Task, receipt.TaskSHA256, request.TeamID, request.Mode, workflowID, version, request.ProjectID,
		receipt.ClientRequestID, string(deliveryContractJSON), executionTask, revisionKind, rootRevisionID,
		parentRevisionID, parentRunID, parentDeliveryDigest, string(parentMaterialsJSON))
	if err != nil {
		return workflowStoreFailure(c, fmt.Errorf("create dispatch input revision: %w", err))
	}
	if inserted.RowsAffected() != 1 {
		// The same registration ID raced from a different session. Its facts
		// necessarily differ; rolling back also restores our previous head.
		return workflowError(c, http.StatusConflict, "input_registration_conflict", "registration_id was already used for different input facts")
	}
	if err := tx.Commit(ctx); err != nil {
		return workflowStoreFailure(c, fmt.Errorf("commit dispatch input revision: %w", err))
	}
	return c.JSON(http.StatusCreated, receipt)
}

// Resolve once when the trusted Host registers the input. The original
// registration fingerprint deliberately keeps the caller's default selection,
// so an identical registration retry cannot drift to a newer published version.
func resolveRegisteredDispatchWorkflow(c echo.Context, tx pgx.Tx, request dispatchInputRegistration) (string, int, bool, error) {
	ctx, workspaceID := c.Request().Context(), getTenant(c)
	var teamStatus, defaultWorkflowID string
	err := tx.QueryRow(ctx, `SELECT status, COALESCE(default_workflow_id,'') FROM weave_teams
		WHERE workspace_id=$1 AND id=$2 FOR SHARE`, workspaceID, request.TeamID).Scan(&teamStatus, &defaultWorkflowID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, true, workflowError(c, http.StatusNotFound, "team_not_found", "team not found")
	}
	if err != nil {
		return "", 0, true, workflowStoreFailure(c, fmt.Errorf("read registered dispatch team: %w", err))
	}
	if teamStatus != "active" {
		return "", 0, true, workflowError(c, http.StatusConflict, "team_not_active", "team is not active")
	}
	workflowID := request.WorkflowID
	if workflowID == "" {
		workflowID = defaultWorkflowID
	}
	if workflowID == "" {
		return "", 0, true, workflowError(c, http.StatusConflict, "no_default_workflow", "team has no default workflow")
	}
	var workflowTeamID string
	var publishedVersion *int
	err = tx.QueryRow(ctx, `SELECT team_id,published_version FROM weave_team_workflows
		WHERE workspace_id=$1 AND id=$2 FOR SHARE`, workspaceID, workflowID).Scan(&workflowTeamID, &publishedVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, true, workflowError(c, http.StatusConflict, "default_workflow_unavailable", "selected workflow unavailable")
	}
	if err != nil {
		return "", 0, true, workflowStoreFailure(c, fmt.Errorf("read registered dispatch workflow: %w", err))
	}
	if workflowTeamID != request.TeamID {
		return "", 0, true, workflowError(c, http.StatusConflict, "workflow_team_mismatch", "selected workflow does not belong to the team")
	}
	version := request.WorkflowVersion
	if version == nil {
		version = publishedVersion
	}
	if version == nil {
		return "", 0, true, workflowError(c, http.StatusConflict, "workflow_not_published", "selected workflow has no published version")
	}
	var versionStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM weave_team_workflow_versions
		WHERE workspace_id=$1 AND workflow_id=$2 AND version=$3 FOR SHARE`, workspaceID, workflowID, *version).Scan(&versionStatus)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && versionStatus != workflow.VersionStatusPublished {
		return "", 0, true, workflowError(c, http.StatusConflict, "workflow_not_published", "selected workflow version is not published")
	}
	if err != nil {
		return "", 0, true, workflowStoreFailure(c, fmt.Errorf("read registered dispatch workflow version: %w", err))
	}
	return workflowID, *version, false, nil
}

func lockDispatchInputSession(ctx context.Context, tx pgx.Tx, workspaceID, userID, sessionID string) error {
	// The same lock also protects an empty session (there is no row to lock yet).
	// JSON encoding keeps identity boundaries unambiguous even for arbitrary IDs.
	identity, _ := json.Marshal([]string{"workbench-dispatch-input", workspaceID, userID, sessionID})
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || $1, 0))`, string(identity))
	if err != nil {
		return fmt.Errorf("lock dispatch input session: %w", err)
	}
	return nil
}

func (s *Server) loadDispatchInput(ctx context.Context, workspaceID, userID, revisionID string) (dispatchInputRevision, error) {
	return scanDispatchInput(s.GetPool().QueryRow(ctx, `SELECT `+dispatchInputColumns+`
		FROM weave_dispatch_input_revisions WHERE workspace_id=$1 AND user_id=$2 AND input_revision_id=$3`,
		workspaceID, userID, revisionID))
}

// Check the head and reserve the fixed input inside the existing snapshot/queue
// transaction. Registration takes this same lock, closing the check/enqueue race.
func (s *Server) lockBoundDispatchInput(c echo.Context, tx pgx.Tx, request teamDispatchRequest) (bool, error) {
	if request.inputBinding == nil {
		return false, nil
	}
	ctx, workspaceID, userID := c.Request().Context(), getTenant(c), getUserID(c)
	if err := lockDispatchInputSession(ctx, tx, workspaceID, userID, request.inputBinding.WorkbenchSessionID); err != nil {
		return true, workflowStoreFailure(c, err)
	}
	input, err := scanDispatchInput(tx.QueryRow(ctx, `SELECT `+dispatchInputColumns+`
		FROM weave_dispatch_input_revisions WHERE workspace_id=$1 AND user_id=$2 AND input_revision_id=$3 FOR UPDATE`,
		workspaceID, userID, request.InputRevisionID))
	if err != nil {
		return true, workflowStoreFailure(c, fmt.Errorf("lock bound dispatch input: %w", err))
	}
	if input.ConsumedRunID != "" {
		// Another identical request may have committed after the initial read.
		// Release this transaction before reading the existing durable receipt.
		_ = tx.Rollback(ctx)
		return s.replayBoundDispatchInput(c, input, request)
	}
	if input.IsClosed {
		return true, workflowError(c, http.StatusConflict, "dispatch_input_closed", "dispatch input was closed before admission")
	}
	if !input.IsCurrent {
		return true, workflowError(c, http.StatusConflict, "dispatch_input_superseded", "dispatch input revision is no longer current")
	}
	return false, nil
}

func consumeDispatchInputTx(ctx context.Context, tx pgx.Tx, workspaceID, userID, revisionID, runID, taskID string) error {
	if revisionID == "" {
		return nil
	}
	result, err := tx.Exec(ctx, `UPDATE weave_dispatch_input_revisions
		SET consumed_run_id=$4, consumed_task_id=$5, consumed_at=statement_timestamp()
		WHERE workspace_id=$1 AND user_id=$2 AND input_revision_id=$3 AND consumed_run_id IS NULL AND closed_at IS NULL`,
		workspaceID, userID, revisionID, runID, taskID)
	if err != nil {
		return fmt.Errorf("consume dispatch input revision: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("dispatch input revision changed before queue commit")
	}
	return nil
}

func (s *Server) replayBoundDispatchInput(c echo.Context, input dispatchInputRevision, request teamDispatchRequest) (bool, error) {
	if input.ConsumedRunID == "" {
		return false, nil
	}
	if _, err := s.boundDispatchInputResult(c.Request().Context(), getTenant(c), input, request); err != nil {
		return true, workflowStoreFailure(c, err)
	}
	return true, s.finishWorkflowAdmission(c, input.ConsumedRunID, request.ClientRequestID, http.StatusOK)
}

func (s *Server) boundDispatchInputResult(ctx context.Context, workspaceID string, input dispatchInputRevision, request teamDispatchRequest) (workflowManualRunResponse, error) {
	existing, teamID, payload, fingerprint, found, err := s.loadWorkflowDispatchReplay(ctx, workspaceID, input.ConsumedRunID, input.ConsumedTaskID)
	if err != nil {
		return workflowManualRunResponse{}, fmt.Errorf("read consumed dispatch input: %w", err)
	}
	var task string
	if !found || teamID != input.TeamID || json.Unmarshal([]byte(payload), &task) != nil || task != input.ExecutionTask ||
		fingerprint != workflowDispatchFingerprint(input.TeamID, request) {
		return workflowManualRunResponse{}, errors.New("consumed dispatch input has no matching durable workflow receipt")
	}
	existing.RunID = input.ConsumedRunID
	existing.InputRevisionID = input.InputRevisionID
	existing.ClientRequestID = input.ClientRequestID
	return existing, nil
}

type dispatchInputReconciliation struct {
	State   string                     `json:"state"`
	Receipt dispatchInputReceipt       `json:"receipt"`
	Result  *workflowManualRunResponse `json:"result,omitempty"`
}

// Reconcile an uncertain response without submitting an old task again. A
// read-only "not found" is insufficient: a delayed dispatch could still queue
// later. Closing an unconsumed revision under the admission lock makes that
// outcome stable. An already accepted run is returned and never cancelled.
func (s *Server) handleReconcileDispatchInput(c echo.Context) error {
	if s.GetPool() == nil || s.ScheduleTransactions == nil {
		return workflowError(c, http.StatusServiceUnavailable, "dispatch_input_unavailable", "dispatch input storage unavailable")
	}
	if err := decodeEmptyWorkflowBody(c); err != nil {
		return workflowError(c, http.StatusBadRequest, "dispatch_input_request_invalid", "reconciliation requires an empty object")
	}
	revisionID, err := uuid.Parse(c.Param("input_revision_id"))
	if err != nil {
		return workflowError(c, http.StatusBadRequest, "dispatch_input_revision_invalid", "input_revision_id must be a UUID")
	}
	ctx, workspaceID, userID := c.Request().Context(), getTenant(c), getUserID(c)
	input, err := s.loadDispatchInput(ctx, workspaceID, userID, revisionID.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return workflowError(c, http.StatusNotFound, "dispatch_input_not_found", "dispatch input not found for the current user")
	}
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	tx, err := s.ScheduleTransactions.Begin(ctx)
	if err != nil {
		return workflowStoreFailure(c, fmt.Errorf("begin dispatch input reconciliation: %w", err))
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockDispatchInputSession(ctx, tx, workspaceID, userID, input.WorkbenchSessionID); err != nil {
		return workflowStoreFailure(c, err)
	}
	input, err = scanDispatchInput(tx.QueryRow(ctx, `SELECT `+dispatchInputColumns+`
		FROM weave_dispatch_input_revisions WHERE workspace_id=$1 AND user_id=$2 AND input_revision_id=$3 FOR UPDATE`,
		workspaceID, userID, input.InputRevisionID))
	if err != nil {
		return workflowStoreFailure(c, fmt.Errorf("read dispatch input for reconciliation: %w", err))
	}
	if input.ConsumedRunID != "" {
		_ = tx.Rollback(ctx)
		result, err := s.boundDispatchInputResult(ctx, workspaceID, input, input.dispatchRequest())
		if err != nil {
			return workflowStoreFailure(c, err)
		}
		return c.JSON(http.StatusOK, dispatchInputReconciliation{State: "accepted", Receipt: input.dispatchInputReceipt, Result: &result})
	}
	runID, _ := deterministicWorkflowDispatchIDs(workspaceID, userID, input.ClientRequestID)
	var prepared bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_workflow_admission_requests WHERE workspace_id=$1 AND request_id=$2)`, workspaceID, runID).Scan(&prepared); err != nil {
		return workflowStoreFailure(c, err)
	}
	if prepared {
		_ = tx.Rollback(ctx)
		store := s.workflowAdmissions()
		if store == nil {
			return workflowStoreFailure(c, errors.New("admission reconciliation unavailable"))
		}
		record, closed, err := store.Reconcile(ctx, workspaceID, runID, s.associateWorkflowAdmission)
		if err != nil {
			return workflowStoreFailure(c, err)
		}
		if !closed {
			result := workflowAdmissionResponse(record, input.ClientRequestID)
			return c.JSON(http.StatusOK, dispatchInputReconciliation{State: "accepted", Receipt: input.dispatchInputReceipt, Result: &result})
		}
		tx, err = s.ScheduleTransactions.Begin(ctx)
		if err != nil {
			return workflowStoreFailure(c, err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err = lockDispatchInputSession(ctx, tx, workspaceID, userID, input.WorkbenchSessionID); err != nil {
			return workflowStoreFailure(c, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_dispatch_input_revisions SET closed_at=COALESCE(closed_at,statement_timestamp())
		WHERE workspace_id=$1 AND user_id=$2 AND input_revision_id=$3`, workspaceID, userID, input.InputRevisionID); err != nil {
		return workflowStoreFailure(c, fmt.Errorf("close unconsumed dispatch input: %w", err))
	}
	if err := tx.Commit(ctx); err != nil {
		return workflowStoreFailure(c, fmt.Errorf("commit dispatch input reconciliation: %w", err))
	}
	return c.JSON(http.StatusOK, dispatchInputReconciliation{State: "closed", Receipt: input.dispatchInputReceipt})
}
