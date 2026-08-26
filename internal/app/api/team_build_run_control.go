package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamrestore"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

// Control-record API for TeamBuildRun (plan §6.0, ticket T08). The endpoints
// follow the T06 handler pattern in team_build_runs.go: nil-store guard →
// decodeWorkflowBody → strict validation → org-scoped store call → mapped
// error. The build execution receipt is never returned to the client; the
// workspace admin only reads the persisted control record, and write tools mint
// their own credentials server-side via ReissueReceipt.

type createTeamBuildRunRequest struct {
	Brief          teambuild.BuildBrief         `json:"brief"`
	Contract       teambuild.EvaluationContract `json:"contract"`
	ExpiresAt      time.Time                    `json:"expires_at"`
	ConversationID string                       `json:"conversation_id,omitempty"`
}

type updateTeamBuildRunDraftsRequest struct {
	Brief    teambuild.BuildBrief         `json:"brief"`
	Contract teambuild.EvaluationContract `json:"contract"`
}

// rollbackRunRequest is the only accepted rollback body: an explicit admin
// confirmation. No version, asset, or target content is ever accepted.
type rollbackRunRequest struct {
	Confirm bool `json:"confirm"`
}

type cancelBuildRunRequest struct {
	Reason string `json:"reason"`
}

// TeamBuildExecutionSubmission is the narrow API-facing projection of a
// durable execution job. The build run itself remains the source of truth for
// business status and is read through the existing GET endpoint.
type TeamBuildExecutionSubmission struct {
	WorkspaceID string `json:"workspace_id"`
	BuildRunID  string `json:"build_run_id"`
	Status      string `json:"status"`
}

type submitBuildRunResponse struct {
	WorkspaceID string `json:"workspace_id"`
	BuildRunID  string `json:"build_run_id"`
	RunStatus   string `json:"run_status"`
	QueueStatus string `json:"queue_status"`
	Authority   string `json:"authority"`
}

type TeamBuildExecutionService interface {
	Submit(
		ctx context.Context, workspaceID, buildRunID string,
	) (TeamBuildExecutionSubmission, error)
}

// teamBuildRunResponse is the client-facing JSON shape of one TeamBuildRun
// control record. It mirrors the persisted run, including the frozen hashes,
// and intentionally omits nothing from the control record while never
// exposing the BuildAuthorizationReceipt.
type teamBuildRunResponse struct {
	WorkspaceID       string                       `json:"workspace_id"`
	BuildRunID        string                       `json:"build_run_id"`
	Mode              string                       `json:"mode"`
	ExecutionStrategy string                       `json:"execution_strategy"`
	Status            string                       `json:"status"`
	ConversationID    string                       `json:"conversation_id,omitempty"`
	EvaluationTeamID  string                       `json:"evaluation_team_id,omitempty"`
	EvaluationOnly    bool                         `json:"evaluation_only"`
	Brief             teambuild.BuildBrief         `json:"brief"`
	BriefHash         string                       `json:"brief_hash"`
	Contract          teambuild.EvaluationContract `json:"contract"`
	ContractHash      string                       `json:"contract_hash"`
	AssetScope        teambuild.AssetScope         `json:"asset_scope"`
	Baseline          *teambuild.BaselineSnapshot  `json:"baseline,omitempty"`
	RoundBudget       teambuild.Budget             `json:"round_budget"`
	TotalBudget       teambuild.Budget             `json:"total_budget"`
	ExpiresAt         time.Time                    `json:"expires_at"`
	PublishEligible   bool                         `json:"publish_eligible"`
	RollbackStatus    string                       `json:"rollback_status"`
	ConfirmedBy       string                       `json:"confirmed_by,omitempty"`
	Authorization     teambuild.BuildAuthorization `json:"authorization,omitempty"`
	FinalRef          *teambuild.FinalRef          `json:"final_ref,omitempty"`
	CreatedAt         time.Time                    `json:"created_at"`
	UpdatedAt         time.Time                    `json:"updated_at"`
	DecidedAt         *time.Time                   `json:"decided_at,omitempty"`
}

type buildRunListResponse struct {
	Items  []teamBuildRunSummaryResponse `json:"items"`
	Total  int                           `json:"total"`
	Limit  int                           `json:"limit"`
	Offset int                           `json:"offset"`
}

type teamBuildRunSummaryResponse struct {
	BuildRunID            string              `json:"build_run_id"`
	Mode                  string              `json:"mode"`
	Status                string              `json:"status"`
	ConversationID        string              `json:"conversation_id,omitempty"`
	TargetTeamID          string              `json:"target_team_id,omitempty"`
	TargetTeamName        string              `json:"target_team_name,omitempty"`
	NewTeamName           string              `json:"new_team_name,omitempty"`
	CurrentRound          int                 `json:"current_round"`
	MaxRounds             int                 `json:"max_rounds"`
	LatestConclusion      string              `json:"latest_conclusion,omitempty"`
	LatestFailureCategory string              `json:"latest_failure_category,omitempty"`
	BudgetUsage           budgetUsageResponse `json:"budget_usage"`
	PublishEligible       bool                `json:"publish_eligible"`
	RollbackStatus        string              `json:"rollback_status"`
	FinalRef              *teambuild.FinalRef `json:"final_ref,omitempty"`
	ConfirmedBy           string              `json:"confirmed_by,omitempty"`
	ExpiresAt             time.Time           `json:"expires_at"`
	UpdatedAt             time.Time           `json:"updated_at"`
}

type budgetUsageResponse struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	ToolCalls    int64   `json:"tool_calls"`
}

type teamBuildRoundResponse struct {
	BuildRunID   string    `json:"build_run_id"`
	RoundNo      int       `json:"round_no"`
	CandidateRef string    `json:"candidate_ref"`
	ReportRef    string    `json:"report_ref"`
	Conclusion   string    `json:"conclusion"`
	CreatedAt    time.Time `json:"created_at"`
}

type buildRunRoundsResponse struct {
	Items []teamBuildRoundResponse `json:"items"`
}

type roundReportResponse struct {
	BuildRunID string                     `json:"build_run_id"`
	RoundNo    int                        `json:"round_no"`
	ReportHash string                     `json:"report_hash"`
	Report     teambuild.EvaluationReport `json:"report"`
	CreatedAt  time.Time                  `json:"created_at"`
}

type buildRunUsageResponse struct {
	BuildRunID string                    `json:"build_run_id"`
	Usage      budgetUsageResponse       `json:"usage"`
	Sources    []buildRunUsageSourceView `json:"sources"`
}

type buildRunUsageSourceView struct {
	RoundNo     int       `json:"round_no"`
	SourceKind  string    `json:"source_kind"`
	SourceRole  string    `json:"source_role"`
	SourceRunID string    `json:"source_run_id"`
	CreatedAt   time.Time `json:"created_at"`
}

type buildRunProgressResponse struct {
	WorkspaceID string                          `json:"workspace_id"`
	BuildRunID  string                          `json:"build_run_id"`
	RunStatus   string                          `json:"run_status"`
	RevisionNo  int                             `json:"revision_no,omitempty"`
	Steps       []buildRunOperationStepResponse `json:"steps"`
	FinalRef    *teambuild.FinalRef             `json:"final_ref,omitempty"`
	UpdatedAt   time.Time                       `json:"updated_at"`
}

type buildRunOperationStepResponse struct {
	OperationID         string                       `json:"operation_id"`
	OperationIndex      int                          `json:"operation_index"`
	OperationType       string                       `json:"operation_type"`
	Target              string                       `json:"target,omitempty"`
	TargetName          string                       `json:"target_name,omitempty"`
	TargetRole          string                       `json:"target_role,omitempty"`
	DisplayLabel        string                       `json:"display_label,omitempty"`
	Status              string                       `json:"status"`
	DependsOn           []string                     `json:"depends_on"`
	Attempt             int                          `json:"attempt"`
	ErrorClass          string                       `json:"error_class,omitempty"`
	ErrorCode           string                       `json:"error_code,omitempty"`
	ErrorDetail         string                       `json:"error_detail,omitempty"`
	StartedAt           *time.Time                   `json:"started_at,omitempty"`
	CompletedAt         *time.Time                   `json:"completed_at,omitempty"`
	UpdatedAt           time.Time                    `json:"updated_at"`
	CandidateEvaluation *candidateEvaluationResponse `json:"candidate_evaluation,omitempty"`
}

type candidateEvaluationResponse struct {
	ScenarioInput string                    `json:"scenario_input"`
	Conclusion    string                    `json:"conclusion"`
	RubricScores  []candidateRubricResponse `json:"rubric_scores"`
	GateFailures  []string                  `json:"gate_failures"`
	ArtifactRef   string                    `json:"artifact_ref,omitempty"`
}

type candidateRubricResponse struct {
	Name  string `json:"name"`
	Score int    `json:"score"`
	Max   int    `json:"max"`
	Pass  int    `json:"pass"`
}

type buildRunOperationDisplay struct {
	Target       string `json:"target,omitempty"`
	TargetName   string `json:"target_name,omitempty"`
	TargetRole   string `json:"target_role,omitempty"`
	DisplayLabel string `json:"display_label,omitempty"`
}

type buildRunProgressChangeSet struct {
	Operations []struct {
		OperationID string          `json:"operation_id"`
		Type        string          `json:"type"`
		Target      string          `json:"target"`
		Input       json.RawMessage `json:"input"`
	} `json:"operations"`
}

type authorizeBuildRunRequest struct {
	Authority     string                            `json:"authority,omitempty"`
	RevisionToken *teambuild.BlueprintRevisionToken `json:"revision_token,omitempty"`
}

type submitBuildRunRequest = authorizeBuildRunRequest

func decodeOptionalAuthorizeBuildRunBody(c echo.Context, request *authorizeBuildRunRequest) error {
	decoder := json.NewDecoder(c.Request().Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("workflow request contains trailing JSON")
		}
		return err
	}
	return nil
}

func teamBuildRunView(run teambuild.TeamBuildRun) teamBuildRunResponse {
	return teamBuildRunResponse{
		WorkspaceID:       run.WorkspaceID,
		BuildRunID:        run.BuildRunID,
		Mode:              run.Mode,
		ExecutionStrategy: run.EffectiveExecutionStrategy(),
		Status:            run.Status,
		ConversationID:    run.ConversationID,
		EvaluationTeamID:  run.EvaluationTeamID,
		EvaluationOnly:    run.EvaluationOnly,
		Brief:             run.Brief,
		BriefHash:         run.BriefHash,
		Contract:          run.Contract,
		ContractHash:      run.ContractHash,
		AssetScope:        run.AssetScope,
		Baseline:          run.Baseline,
		RoundBudget:       run.RoundBudget,
		TotalBudget:       run.TotalBudget,
		ExpiresAt:         run.ExpiresAt,
		PublishEligible:   run.PublishEligible,
		RollbackStatus:    run.RollbackStatus,
		ConfirmedBy:       run.ConfirmedBy,
		Authorization:     run.Authorization,
		FinalRef:          run.FinalRef,
		CreatedAt:         run.CreatedAt,
		UpdatedAt:         run.UpdatedAt,
		DecidedAt:         run.DecidedAt,
	}
}

func teamBuildRunSummaryView(summary teambuild.TeamBuildRunSummary) teamBuildRunSummaryResponse {
	return teamBuildRunSummaryResponse{
		BuildRunID:            summary.BuildRunID,
		Mode:                  summary.Mode,
		Status:                summary.Status,
		ConversationID:        summary.ConversationID,
		TargetTeamID:          summary.TargetTeamID,
		TargetTeamName:        summary.TargetTeamName,
		NewTeamName:           summary.NewTeamName,
		CurrentRound:          summary.CurrentRound,
		MaxRounds:             summary.MaxRounds,
		LatestConclusion:      summary.LatestConclusion,
		LatestFailureCategory: summary.LatestFailureCategory,
		BudgetUsage:           budgetUsageView(summary.BudgetUsage),
		PublishEligible:       summary.PublishEligible,
		RollbackStatus:        summary.RollbackStatus,
		FinalRef:              summary.FinalRef,
		ConfirmedBy:           summary.ConfirmedBy,
		ExpiresAt:             summary.ExpiresAt,
		UpdatedAt:             summary.UpdatedAt,
	}
}

func budgetUsageView(usage teambuild.BudgetUsage) budgetUsageResponse {
	return budgetUsageResponse{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		CostUSD:      usage.CostUSD,
		ToolCalls:    usage.ToolCalls,
	}
}

// handleCreateTeamBuildRun creates one planning run from the workspace admin
// confirmed brief + contract drafts and returns the complete control record
// with its frozen hashes.
func (s *Server) handleCreateTeamBuildRun(c echo.Context) error {
	if s.TeamBuild == nil {
		return teamBuildRunUnavailable(c)
	}
	var request createTeamBuildRunRequest
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	workspaceID := getTenant(c)
	run, err := s.TeamBuild.CreateBuildRun(
		c.Request().Context(),
		workspaceID,
		"br-"+uuid.NewString(),
		teambuild.CreateRunParams{
			Brief:          request.Brief,
			Contract:       request.Contract,
			ExpiresAt:      request.ExpiresAt,
			CreatedBy:      getUserID(c),
			ConversationID: request.ConversationID,
		},
	)
	if err != nil {
		return mapTeamBuildRunControlError(c, err)
	}
	return c.JSON(http.StatusCreated, teamBuildRunView(run))
}

// handleListBuildRuns returns the BuildRun list projection for back-office
// pages. It excludes the large brief/contract JSON documents by construction.
func (s *Server) handleListBuildRuns(c echo.Context) error {
	if s.TeamBuild == nil {
		return teamBuildRunUnavailable(c)
	}
	filter, err := parseBuildRunFilter(c)
	if err != nil {
		return workflowError(c, http.StatusUnprocessableEntity, "build_run_query_invalid", err.Error())
	}
	summaries, total, err := s.TeamBuild.ListBuildRuns(c.Request().Context(), getTenant(c), filter)
	if err != nil {
		if strings.Contains(err.Error(), "invalid status") || strings.Contains(err.Error(), "invalid mode") {
			return workflowError(c, http.StatusUnprocessableEntity, "build_run_query_invalid", err.Error())
		}
		return workflowStoreFailure(c, err)
	}
	items := make([]teamBuildRunSummaryResponse, 0, len(summaries))
	for _, summary := range summaries {
		items = append(items, teamBuildRunSummaryView(summary))
	}
	return c.JSON(http.StatusOK, buildRunListResponse{
		Items:  items,
		Total:  total,
		Limit:  normalizedBuildRunLimit(filter.Limit),
		Offset: filter.Offset,
	})
}

// handleUpdateTeamBuildRunDrafts revises the brief and contract drafts while
// the run is still in planning. Hashes, asset scope, and budget columns are
// recomputed from the new drafts on every write.
func (s *Server) handleUpdateTeamBuildRunDrafts(c echo.Context) error {
	if s.TeamBuild == nil {
		return teamBuildRunUnavailable(c)
	}
	var request updateTeamBuildRunDraftsRequest
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	run, err := s.TeamBuild.UpdateDraftContracts(
		c.Request().Context(),
		getTenant(c),
		c.Param("id"),
		request.Brief,
		request.Contract,
	)
	if err != nil {
		return mapTeamBuildRunControlError(c, err)
	}
	return c.JSON(http.StatusOK, teamBuildRunView(run))
}

// handleAuthorizeBuildRun freezes the drafts, captures the optimize baseline
// server-side inside the store transaction, transitions planning → authorized,
// and mints the BuildAuthorizationReceipt. The client only confirms with an
// explicit authorization authority and optional revision token. The receipt is
// never returned to the client.
func (s *Server) handleAuthorizeBuildRun(c echo.Context) error {
	if s.TeamBuild == nil {
		return teamBuildRunUnavailable(c)
	}
	var request authorizeBuildRunRequest
	if err := decodeOptionalAuthorizeBuildRunBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	s.TeamBuild.SetBaselineSources(s.OrgStore, s.Registry, s.Workflow)
	run, _, err := s.TeamBuild.AuthorizeBuildRun(
		c.Request().Context(),
		getTenant(c),
		c.Param("id"),
		getUserID(c),
		nil,
		teambuild.AuthorizeOptions{Authority: request.Authority, RevisionToken: request.RevisionToken},
	)
	if err != nil {
		return mapTeamBuildRunControlError(c, err)
	}
	return c.JSON(http.StatusOK, teamBuildRunView(run))
}

func (s *Server) handleSubmitBuildRun(c echo.Context) error {
	if s.TeamBuild == nil || s.TeamBuildOrchestrator == nil {
		return teamBuildRunUnavailable(c)
	}
	var request submitBuildRunRequest
	if err := decodeOptionalAuthorizeBuildRunBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	run, err := s.TeamBuild.GetBuildRun(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		return mapTeamBuildRunControlError(c, err)
	}
	if run.Status == teambuild.StatusPlanning {
		_, _, err = s.TeamBuild.AuthorizeBuildRun(c.Request().Context(), getTenant(c), run.BuildRunID, getUserID(c), nil, teambuild.AuthorizeOptions{
			Authority:     strings.TrimSpace(request.Authority),
			RevisionToken: request.RevisionToken,
		})
		if err != nil {
			return mapTeamBuildRunControlError(c, err)
		}
	}
	submission, err := s.TeamBuildOrchestrator.Submit(c.Request().Context(), getTenant(c), run.BuildRunID)
	if err != nil {
		return mapTeamBuildRunControlError(c, err)
	}
	return c.JSON(http.StatusAccepted, submitBuildRunResponse{
		WorkspaceID: submission.WorkspaceID,
		BuildRunID:  submission.BuildRunID,
		RunStatus:   run.Status,
		QueueStatus: submission.Status,
		Authority:   strings.TrimSpace(request.Authority),
	})
}

// handleExecuteTeamBuildRun is the production service seam for the embedded
// meta-team controller. It durably submits one already-authorized control
// record and returns immediately; a lease-owning platform worker drives the
// resumable build independently of the HTTP request lifetime.
func (s *Server) handleExecuteTeamBuildRun(c echo.Context) error {
	if s.TeamBuildOrchestrator == nil {
		return workflowError(
			c, http.StatusServiceUnavailable,
			"team_build_orchestrator_unavailable",
			"team build orchestrator unavailable",
		)
	}
	if err := decodeEmptyWorkflowBody(c); err != nil {
		return workflowSchemaError(c)
	}
	result, err := s.TeamBuildOrchestrator.Submit(
		c.Request().Context(), getTenant(c), c.Param("id"),
	)
	if err != nil {
		return workflowError(
			c, http.StatusInternalServerError,
			"team_build_execution_failed", err.Error(),
		)
	}
	return c.JSON(http.StatusAccepted, result)
}

// handleCancelBuildRun terminates a non-terminal control record without
// mutating its frozen brief, contract, or baseline. This is the supported
// replanning path when a confirmed contract proves impossible: cancel the
// old immutable run, then create a new planning run.
func (s *Server) handleCancelBuildRun(c echo.Context) error {
	if s.TeamBuild == nil {
		return teamBuildRunUnavailable(c)
	}
	var request cancelBuildRunRequest
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	if strings.TrimSpace(request.Reason) == "" {
		return workflowError(
			c, http.StatusUnprocessableEntity,
			"build_run_cancel_reason_required", "cancel reason is required",
		)
	}
	run, err := s.TeamBuild.GetBuildRun(
		c.Request().Context(), getTenant(c), c.Param("id"),
	)
	if err != nil {
		return mapTeamBuildRunControlError(c, err)
	}
	cancelled, err := s.TeamBuild.TransitionStatus(
		c.Request().Context(), getTenant(c), c.Param("id"),
		run.Status, teambuild.StatusCancelled, getUserID(c), request.Reason,
	)
	if err != nil {
		return mapTeamBuildRunControlError(c, err)
	}
	return c.JSON(http.StatusOK, teamBuildRunView(cancelled))
}

func (s *Server) handleGetBuildRunProgress(c echo.Context) error {
	if s.TeamBuild == nil {
		return teamBuildRunUnavailable(c)
	}
	ctx := c.Request().Context()
	workspaceID := getTenant(c)
	buildRunID := c.Param("id")
	run, err := s.TeamBuild.GetBuildRun(ctx, workspaceID, buildRunID)
	if err != nil {
		return mapTeamBuildRunControlError(c, err)
	}
	var revisionNo int
	steps := []buildRunOperationStepResponse{}
	revision, err := s.TeamBuild.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID)
	if err == nil {
		revisionNo = revision.RevisionNo
		operationDisplays := buildRunOperationDisplays(revision.ChangeSetJSON)
		operationSteps, listErr := s.TeamBuild.ListOperationSteps(ctx, workspaceID, buildRunID, revision.RevisionNo)
		if listErr != nil {
			return workflowStoreFailure(c, listErr)
		}
		var candidateEvaluation *candidateEvaluationResponse
		for _, step := range operationSteps {
			if step.OperationType != "candidate_run" ||
				(step.Status != teambuild.OperationStatusSucceeded && step.Status != teambuild.OperationStatusFailed) {
				continue
			}
			report, reportErr := s.TeamBuild.GetRoundReport(ctx, workspaceID, buildRunID, revision.RevisionNo)
			if reportErr != nil && !errors.Is(reportErr, teambuild.ErrRoundReportNotFound) {
				return workflowStoreFailure(c, reportErr)
			}
			if reportErr == nil {
				candidateEvaluation = candidateEvaluationView(run.Contract, report.Report)
			}
			break
		}
		steps = make([]buildRunOperationStepResponse, 0, len(operationSteps))
		for _, step := range operationSteps {
			view := buildRunOperationStepView(step, operationDisplays[step.OperationID])
			if step.OperationType == "candidate_run" {
				view.CandidateEvaluation = candidateEvaluation
			}
			steps = append(steps, view)
		}
	} else if !errors.Is(err, teambuild.ErrBuildRunNotFound) {
		return workflowStoreFailure(c, err)
	}
	return c.JSON(http.StatusOK, buildRunProgressResponse{
		WorkspaceID: run.WorkspaceID,
		BuildRunID:  run.BuildRunID,
		RunStatus:   run.Status,
		RevisionNo:  revisionNo,
		Steps:       steps,
		FinalRef:    run.FinalRef,
		UpdatedAt:   run.UpdatedAt,
	})
}

func candidateEvaluationView(contract teambuild.EvaluationContract, report teambuild.EvaluationReport) *candidateEvaluationResponse {
	dimensions := make(map[string]teambuild.RubricDimension, len(contract.Rubric))
	for _, dimension := range contract.Rubric {
		dimensions[dimension.ID] = dimension
	}
	rubricScores := make([]candidateRubricResponse, 0, len(report.RubricScores))
	for _, score := range report.RubricScores {
		dimension, ok := dimensions[score.DimensionID]
		name := score.DimensionID
		if ok && strings.TrimSpace(dimension.Name) != "" {
			name = dimension.Name
		}
		rubricScores = append(rubricScores, candidateRubricResponse{
			Name: name, Score: score.Score, Max: dimension.MaxScore, Pass: dimension.PassThreshold,
		})
	}
	gateFailures := make([]string, 0)
	for _, gate := range report.HardGateResults {
		if !gate.Passed {
			gateFailures = append(gateFailures, gate.GateID)
		}
	}
	scenarioInput := ""
	if len(contract.PublicScenarios) > 0 {
		scenarioInput = firstParagraph(contract.PublicScenarios[0].Input, 80)
	}
	artifactRef := ""
	for _, scenario := range report.ScenarioResults {
		if artifactRef == "" && strings.TrimSpace(scenario.ArtifactRef) != "" {
			artifactRef = scenario.ArtifactRef
		}
		if len(contract.PublicScenarios) > 0 && scenario.ScenarioID == contract.PublicScenarios[0].ID {
			artifactRef = scenario.ArtifactRef
			break
		}
	}
	return &candidateEvaluationResponse{
		ScenarioInput: scenarioInput,
		Conclusion:    report.Conclusion,
		RubricScores:  rubricScores,
		GateFailures:  gateFailures,
		ArtifactRef:   artifactRef,
	}
}

func firstParagraph(value string, maxRunes int) string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	paragraph := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if len(paragraph) > 0 {
				break
			}
			continue
		}
		paragraph = append(paragraph, line)
	}
	text := strings.Join(paragraph, " ")
	runes := []rune(text)
	if maxRunes > 0 && len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return text
}

func buildRunOperationStepView(step teambuild.OperationStep, display buildRunOperationDisplay) buildRunOperationStepResponse {
	dependsOn := append([]string(nil), step.DependsOn...)
	if dependsOn == nil {
		dependsOn = []string{}
	}
	errorDetail := ""
	if step.Status == teambuild.OperationStatusFailed {
		errorDetail = operationFailureDetail(step.EvidenceJSON)
	}
	return buildRunOperationStepResponse{
		OperationID:    step.OperationID,
		OperationIndex: step.OperationIndex,
		OperationType:  step.OperationType,
		Target:         display.Target,
		TargetName:     display.TargetName,
		TargetRole:     display.TargetRole,
		DisplayLabel:   display.DisplayLabel,
		Status:         step.Status,
		DependsOn:      dependsOn,
		Attempt:        step.Attempt,
		ErrorClass:     step.ErrorClass,
		ErrorCode:      step.ErrorCode,
		ErrorDetail:    errorDetail,
		StartedAt:      step.StartedAt,
		CompletedAt:    step.CompletedAt,
		UpdatedAt:      step.UpdatedAt,
	}
}

func operationFailureDetail(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var evidence map[string]any
	if json.Unmarshal(raw, &evidence) != nil {
		return ""
	}
	for _, key := range []string{"detail", "failure_detail"} {
		if detail, ok := evidence[key].(string); ok {
			return firstParagraph(detail, 240)
		}
	}
	for _, key := range []string{"diagnosis", "Diagnosis"} {
		diagnosis, ok := evidence[key].(map[string]any)
		if !ok {
			continue
		}
		if detail, ok := diagnosis["failure_detail"].(string); ok {
			return firstParagraph(detail, 240)
		}
	}
	return ""
}

func buildRunOperationDisplays(raw json.RawMessage) map[string]buildRunOperationDisplay {
	var document buildRunProgressChangeSet
	if len(raw) == 0 || json.Unmarshal(raw, &document) != nil {
		return map[string]buildRunOperationDisplay{}
	}
	displays := make(map[string]buildRunOperationDisplay, len(document.Operations))
	for _, operation := range document.Operations {
		targetName, targetRole := operationDisplayTarget(operation.Type, operation.Target, operation.Input)
		label := operationDisplayLabel(operation.Type, targetName, targetRole)
		displays[operation.OperationID] = buildRunOperationDisplay{
			Target:       operation.Target,
			TargetName:   targetName,
			TargetRole:   targetRole,
			DisplayLabel: label,
		}
	}
	return displays
}

func operationDisplayTarget(operationType, target string, input json.RawMessage) (string, string) {
	var payload map[string]any
	_ = json.Unmarshal(input, &payload)
	switch operationType {
	case "agent_create", "agent_update":
		desired, _ := payload["desired"].(map[string]any)
		name, _ := desired["name"].(string)
		role, _ := desired["role"].(string)
		if name == "" {
			name = target
		}
		return name, role
	case "workflow_compile":
		blueprint, _ := payload["blueprint"].(map[string]any)
		template, _ := blueprint["template"].(string)
		if template != "" {
			return template, ""
		}
		return target, ""
	default:
		return target, ""
	}
}

func operationDisplayLabel(operationType, targetName, targetRole string) string {
	title := map[string]string{
		"agent_create":        "创建成员",
		"agent_update":        "更新成员",
		"team_create":         "创建团队",
		"team_update":         "更新团队",
		"roster_set":          "绑定成员",
		"agent_graph_compile": "编译成员图",
		"workflow_compile":    "编译工作流",
		"candidate_run":       "业务验收",
		"publish":             "发布团队",
	}[operationType]
	if title == "" {
		title = operationType
	}
	// candidate_run targets are stored as candidate/<team>; strip the
	// namespace and describe the trial in user terms rather than machine paths.
	targetName = strings.TrimPrefix(targetName, "candidate/")
	if operationType == "candidate_run" {
		if targetName == "" {
			return "业务验收（试运行）"
		}
		return "业务验收（试运行：" + targetName + "）"
	}
	if targetName == "" {
		return title
	}
	if operationType == "agent_create" || operationType == "agent_update" {
		if targetRole == "avatar" {
			return title + "：Lead"
		}
		return title + "：" + roleFromStableName(targetName)
	}
	return title + "：" + targetName
}

func roleFromStableName(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == ':' || r == '/' })
	if len(parts) == 0 {
		return name
	}
	role := parts[len(parts)-1]
	if role == "" {
		return name
	}
	runes := []rune(role)
	return strings.ToUpper(string(runes[:1])) + string(runes[1:])
}

// handleGetBuildRun returns one org-scoped control record for frontend
// polling.
func (s *Server) handleGetBuildRun(c echo.Context) error {
	if s.TeamBuild == nil {
		return teamBuildRunUnavailable(c)
	}
	run, err := s.TeamBuild.GetBuildRun(
		c.Request().Context(),
		getTenant(c),
		c.Param("id"),
	)
	if err != nil {
		return mapTeamBuildRunControlError(c, err)
	}
	return c.JSON(http.StatusOK, teamBuildRunView(run))
}

func (s *Server) handleListBuildRunRounds(c echo.Context) error {
	if s.TeamBuild == nil {
		return teamBuildRunUnavailable(c)
	}
	ctx := c.Request().Context()
	workspaceID := getTenant(c)
	buildRunID := c.Param("id")
	if _, err := s.TeamBuild.GetBuildRun(ctx, workspaceID, buildRunID); err != nil {
		return mapTeamBuildRunControlError(c, err)
	}
	rounds, err := s.TeamBuild.ListRounds(ctx, workspaceID, buildRunID)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	items := make([]teamBuildRoundResponse, 0, len(rounds))
	for _, round := range rounds {
		items = append(items, teamBuildRoundResponse{
			BuildRunID:   round.BuildRunID,
			RoundNo:      round.RoundNo,
			CandidateRef: round.CandidateRef,
			ReportRef:    round.ReportRef,
			Conclusion:   round.Conclusion,
			CreatedAt:    round.CreatedAt,
		})
	}
	return c.JSON(http.StatusOK, buildRunRoundsResponse{Items: items})
}

func (s *Server) handleGetBuildRunRoundReport(c echo.Context) error {
	if s.TeamBuild == nil {
		return teamBuildRunUnavailable(c)
	}
	roundNo, err := parsePositiveIntParam(c.Param("n"), "round_no")
	if err != nil {
		return workflowError(c, http.StatusUnprocessableEntity, "build_run_query_invalid", err.Error())
	}
	report, err := s.TeamBuild.GetRoundReport(
		c.Request().Context(), getTenant(c), c.Param("id"), roundNo,
	)
	if err != nil {
		if errors.Is(err, teambuild.ErrRoundReportNotFound) {
			return workflowError(c, http.StatusNotFound, "round_report_not_found", "round report not found")
		}
		return workflowStoreFailure(c, err)
	}
	return c.JSON(http.StatusOK, roundReportResponse{
		BuildRunID: report.BuildRunID,
		RoundNo:    report.RoundNo,
		ReportHash: report.ReportHash,
		Report:     report.Report,
		CreatedAt:  report.CreatedAt,
	})
}

func (s *Server) handleGetBuildRunUsage(c echo.Context) error {
	if s.TeamBuild == nil {
		return teamBuildRunUnavailable(c)
	}
	ctx := c.Request().Context()
	workspaceID := getTenant(c)
	buildRunID := c.Param("id")
	if _, err := s.TeamBuild.GetBuildRun(ctx, workspaceID, buildRunID); err != nil {
		return mapTeamBuildRunControlError(c, err)
	}
	usage, err := s.TeamBuild.GetBudgetUsage(ctx, workspaceID, buildRunID)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	rounds, err := s.TeamBuild.ListRounds(ctx, workspaceID, buildRunID)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	sources := make([]buildRunUsageSourceView, 0)
	for _, round := range rounds {
		roundSources, err := s.TeamBuild.ListUsageSources(ctx, workspaceID, buildRunID, round.RoundNo)
		if err != nil {
			return workflowStoreFailure(c, err)
		}
		for _, source := range roundSources {
			sources = append(sources, buildRunUsageSourceView{
				RoundNo:     source.RoundNo,
				SourceKind:  source.SourceKind,
				SourceRole:  source.SourceRole,
				SourceRunID: source.SourceRunID,
				CreatedAt:   source.CreatedAt,
			})
		}
	}
	return c.JSON(http.StatusOK, buildRunUsageResponse{
		BuildRunID: buildRunID,
		Usage:      budgetUsageView(usage),
		Sources:    sources,
	})
}

// handleRollbackBuildRun performs the admin-confirmed stepwise baseline
// rollback of one optimize build run. The service accepts only
// (workspace, run, operator); everything restored comes from the run's
// frozen baseline. Precondition failures map to 409 (non-optimize /
// non-terminal / already rolled back) or 422 (no confirm / invalid baseline);
// a failed restore step returns 500 with the done/failed/pending report.
func (s *Server) handleRollbackBuildRun(c echo.Context) error {
	if s.Pool == nil || s.TeamBuild == nil || s.OrgStore == nil ||
		s.Registry == nil || s.Workflow == nil || s.Audit == nil ||
		s.DeliveryTargets == nil || s.Skills == nil || s.Credentials == nil ||
		s.AgentSchedules == nil || s.Descriptors == nil {
		return workflowError(
			c,
			http.StatusServiceUnavailable,
			"build_run_rollback_unavailable",
			"build run rollback service unavailable",
		)
	}
	var request rollbackRunRequest
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	if !request.Confirm {
		return workflowError(
			c,
			http.StatusUnprocessableEntity,
			"rollback_not_confirmed",
			"rollback requires confirm: true",
		)
	}
	builder := workflow.NewCandidateBuilder(
		s.Workflow,
		s.Registry,
		s.DeliveryTargets,
		s.Skills,
		s.Credentials,
		s.AgentSchedules,
		s.Descriptors,
	)
	service := teamrestore.New(
		s.Pool,
		s.TeamBuild,
		s.OrgStore,
		s.Registry,
		s.Registry,
		s.Workflow,
		builder,
		s.Audit,
	)
	result, err := service.Rollback(
		c.Request().Context(),
		getTenant(c),
		c.Param("id"),
		getUserID(c),
	)
	if err != nil {
		return mapTeamBuildRunRollbackError(c, err, result)
	}
	return c.JSON(http.StatusOK, result)
}

func teamBuildRunUnavailable(c echo.Context) error {
	return workflowError(
		c,
		http.StatusServiceUnavailable,
		"team_build_unavailable",
		"team build store unavailable",
	)
}

func parseBuildRunFilter(c echo.Context) (teambuild.BuildRunFilter, error) {
	filter := teambuild.BuildRunFilter{
		Status: strings.TrimSpace(c.QueryParam("status")),
		Mode:   strings.TrimSpace(c.QueryParam("mode")),
	}
	var err error
	filter.Limit, err = parseOptionalNonNegativeInt(c.QueryParam("limit"), "limit")
	if err != nil {
		return teambuild.BuildRunFilter{}, err
	}
	filter.Offset, err = parseOptionalNonNegativeInt(c.QueryParam("offset"), "offset")
	if err != nil {
		return teambuild.BuildRunFilter{}, err
	}
	return filter, nil
}

func parseOptionalNonNegativeInt(raw, name string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, errors.New(name + " must be a non-negative integer")
	}
	return value, nil
}

func parsePositiveIntParam(raw, name string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, errors.New(name + " must be a positive integer")
	}
	return value, nil
}

func normalizedBuildRunLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}

// mapTeamBuildRunControlError maps the control-record store surface to
// stable HTTP errors: not found → 404, wrong run state (planning required /
// duplicate authorize) → 409, missing or invalid baseline / invalid draft
// content → 422, everything else → 500.
func mapTeamBuildRunControlError(c echo.Context, err error) error {
	var floorViolation *teambuild.ContractFloorViolation
	if errors.As(err, &floorViolation) {
		field := "contract.hard_gates"
		if len(floorViolation.InvalidWaivers) > 0 &&
			len(floorViolation.MissingGateIDs) == 0 &&
			len(floorViolation.WeakenedGateIDs) == 0 &&
			len(floorViolation.InvalidFloorGateIDs) == 0 {
			field = "brief.waived_gates"
		}
		return c.JSON(http.StatusUnprocessableEntity, map[string]any{
			"error": "build run evaluation contract is below the L1 floor",
			"code":  "build_run_schema_invalid",
			"detail": map[string]any{
				"field":                  field,
				"missing_gate_ids":       floorViolation.MissingGateIDs,
				"weakened_gate_ids":      floorViolation.WeakenedGateIDs,
				"invalid_floor_gate_ids": floorViolation.InvalidFloorGateIDs,
				"invalid_waivers":        floorViolation.InvalidWaivers,
			},
		})
	}
	switch {
	case errors.Is(err, teambuild.ErrBuildRunNotFound):
		return workflowError(c, http.StatusNotFound, "build_run_not_found", "build run not found")
	case errors.Is(err, teambuild.ErrBuildRunNotPlanning):
		return workflowError(
			c,
			http.StatusConflict,
			"build_run_not_planning",
			"build run is not in planning",
		)
	case errors.Is(err, teambuild.ErrCompilerRevisionRequired):
		return workflowError(
			c,
			http.StatusConflict,
			"blueprint_required",
			"complete blueprint planning before authorizing this build run",
		)
	case errors.Is(err, teambuild.ErrBlueprintRevisionMismatch):
		return workflowError(
			c,
			http.StatusConflict,
			"blueprint_revision_mismatch",
			"blueprint revision has changed; confirm the latest plan",
		)
	case errors.Is(err, teambuild.ErrInvalidTransition):
		return workflowError(
			c,
			http.StatusConflict,
			"build_run_transition_conflict",
			"build run cannot make the requested status transition",
		)
	case errors.Is(err, teambuild.ErrOptimizeBaselineRequired),
		errors.Is(err, teambuild.ErrBaselineNotAllowed),
		errors.Is(err, teambuild.ErrBuiltinAssetInBrief),
		errors.Is(err, teambuild.ErrBuildRunDraftInvalid),
		errors.Is(err, teambuild.ErrBaselineTargetNotFound),
		errors.Is(err, teambuild.ErrBaselineSnapshotInvalid):
		return workflowError(
			c,
			http.StatusUnprocessableEntity,
			"build_run_schema_invalid",
			"build run draft or baseline is invalid",
		)
	case errors.Is(err, teambuild.ErrBaselineScopeNotClosed):
		return workflowError(
			c,
			http.StatusConflict,
			"build_run_scope_not_closed",
			"baseline assets are not exactly covered by the asset scope",
		)
	case errors.Is(err, teambuild.ErrBaselineSourceUnavailable):
		return workflowError(
			c,
			http.StatusServiceUnavailable,
			"team_build_baseline_unavailable",
			"baseline snapshot sources unavailable",
		)
	default:
		return workflowStoreFailure(c, err)
	}
}

// mapTeamBuildRunRollbackError maps the rollback service surface to stable
// HTTP errors: 404 for an unknown run, 409 for a run that cannot be rolled
// back (non-optimize / non-terminal / already claimed or completed), 422 for
// a tampered or invalid baseline, and 500 with the step report for an
// execution failure.
func mapTeamBuildRunRollbackError(
	c echo.Context,
	err error,
	result teamrestore.RollbackResult,
) error {
	switch {
	case errors.Is(err, teambuild.ErrBuildRunNotFound):
		return workflowError(
			c,
			http.StatusNotFound,
			"build_run_not_found",
			"build run not found",
		)
	case errors.Is(err, teambuild.ErrBuildRunNotOptimize),
		errors.Is(err, teambuild.ErrBuildRunNotTerminal),
		errors.Is(err, teambuild.ErrBuildRunRollbackClaimed):
		return workflowError(
			c,
			http.StatusConflict,
			"build_run_rollback_conflict",
			"build run cannot be rolled back",
		)
	case errors.Is(err, teambuild.ErrBaselineSnapshotInvalid):
		return workflowError(
			c,
			http.StatusUnprocessableEntity,
			"build_run_rollback_baseline_invalid",
			"build run baseline is invalid",
		)
	default:
		return c.JSON(http.StatusInternalServerError, map[string]any{
			"error":    "team build rollback failed",
			"code":     "build_run_rollback_failed",
			"rollback": result,
		})
	}
}
