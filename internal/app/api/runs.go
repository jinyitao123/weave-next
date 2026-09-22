package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
	"github.com/labstack/echo/v4"
)

// RunSummary is a lightweight run entry for listing.
type RunSummary struct {
	RunID          string  `json:"run_id"`
	Agent          string  `json:"agent,omitempty"`
	Tenant         string  `json:"tenant,omitempty"`
	Step           string  `json:"step,omitempty"`
	Status         string  `json:"status"`
	StopReason     string  `json:"stop_reason,omitempty"`
	DurationMs     int64   `json:"duration_ms"`
	StartedAt      string  `json:"started_at"`
	EndedAt        string  `json:"ended_at"`
	SchemaVersion  int     `json:"schema_version"`
	TokensIn       int     `json:"tokens_in"`
	TokensOut      int     `json:"tokens_out"`
	CostUSD        float64 `json:"cost_usd"`
	UsageComplete  *bool   `json:"usage_complete,omitempty"`
	Timestamp      string  `json:"timestamp,omitempty"`
	ParentRunID    string  `json:"parent_run_id,omitempty"`
	ParentSeq      int64   `json:"parent_seq,omitempty"`
	ProjectID      string  `json:"project_id,omitempty"`
	ConversationID string  `json:"conversation_id,omitempty"`
	Attribution    string  `json:"attribution"`
}

type runActivityUsage struct {
	TokensIn      int
	TokensOut     int
	CostUSD       float64
	CompleteState string
}

func decodeRunActivityUsage(data []byte) (runActivityUsage, bool) {
	var summary RunSummary
	if json.Unmarshal(data, &summary) != nil || summary.RunID == "" {
		return runActivityUsage{}, false
	}
	if summary.SchemaVersion == 3 {
		inspection := loomruntime.InspectTerminalRecord(true, data)
		if inspection.Err != nil || inspection.Entry == nil {
			return runActivityUsage{}, false
		}
		total := inspection.Entry.SubtreeTotal
		summary.TokensIn, summary.TokensOut, summary.CostUSD = total.InputTokens, total.OutputTokens, total.CostUSD
	}
	state := "complete"
	if summary.UsageComplete != nil && !*summary.UsageComplete {
		state = "partial"
	}
	return runActivityUsage{
		TokensIn: summary.TokensIn, TokensOut: summary.TokensOut,
		CostUSD: summary.CostUSD, CompleteState: state,
	}, true
}

func (s *Server) readRunActivityUsage(ctx context.Context, workspaceID, runID string) (runActivityUsage, bool) {
	if s.Store == nil {
		return runActivityUsage{}, false
	}
	data, err := s.Store.Get(ctx, "audit:"+workspaceID, runID)
	if err != nil {
		return runActivityUsage{}, false
	}
	return decodeRunActivityUsage(data)
}

// RunListResponse wraps paginated run results.
type RunListResponse struct {
	Runs   []RunSummary `json:"runs"`
	Total  int          `json:"total"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

func (s *Server) handleListRuns(c echo.Context) error {
	tenant := getTenant(c)
	projectID := strings.TrimSpace(c.QueryParam("project_id"))
	conversationID := strings.TrimSpace(c.QueryParam("conversation_id"))
	owningTeamID := strings.TrimSpace(c.QueryParam("owning_team_id"))

	// Parse pagination params.
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if limit <= 0 {
		limit = 50
	} else if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	selector, teamAware, selectorErr := parseTeamSelector(c.QueryParams(), "", false)
	if selectorErr != nil {
		if conversationID != "" {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "conversation_id cannot be combined with team selector parameters"})
		}
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_team_selector"})
	}
	if teamAware {
		if conversationID != "" {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "conversation_id cannot be combined with team selector parameters"})
		}
		return s.handleTeamAwareRuns(c, selector, limit, offset)
	}

	ns := "audit:" + tenant

	// Use StoreExt's paginated listing for sorted, bounded queries.
	var keys []string
	var total int
	var err error

	if projectID != "" || conversationID != "" || owningTeamID != "" {
		if s.GetPool() == nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "run_filter_unavailable"})
		}
		keys, total, err = s.listRunKeysByAttribution(
			c.Request().Context(), tenant, runAttributionFilter{
				ProjectID:      projectID,
				ConversationID: conversationID,
				OwningTeamID:   owningTeamID,
			}, limit, offset,
		)
	} else if s.StoreExt != nil {
		keys, err = s.StoreExt.ListPaginated(c.Request().Context(), ns, "", limit, offset)
		if err != nil {
			keys = nil
		}
		total, _ = s.StoreExt.CountByNamespace(c.Request().Context(), ns)
	} else {
		// Fallback: load all (non-PGStore implementations).
		keys, err = s.Store.List(c.Request().Context(), ns, "")
		if err != nil {
			return c.JSON(http.StatusOK, RunListResponse{Runs: []RunSummary{}, Total: 0, Limit: limit, Offset: offset})
		}
		total = len(keys)
		// Manual pagination.
		if offset >= len(keys) {
			keys = nil
		} else {
			end := offset + limit
			if end > len(keys) {
				end = len(keys)
			}
			keys = keys[offset:end]
		}
	}

	runs := make([]RunSummary, 0, len(keys))
	for _, key := range keys {
		data, err := s.Store.Get(c.Request().Context(), ns, key)
		if err != nil {
			continue
		}
		var run RunSummary
		if err := json.Unmarshal(data, &run); err != nil {
			continue
		}
		run.ProjectID, run.ConversationID, run.Attribution, err = s.runAttribution(
			c.Request().Context(), tenant, run.RunID,
		)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "run_attribution_failed"})
		}
		runs = append(runs, run)
	}

	return c.JSON(http.StatusOK, RunListResponse{
		Runs:   runs,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

func (s *Server) listRunKeysByProject(
	ctx context.Context,
	workspaceID, projectID string,
	limit, offset int,
) ([]string, int, error) {
	return s.listRunKeysByAttribution(ctx, workspaceID, runAttributionFilter{ProjectID: projectID}, limit, offset)
}

type runAttributionFilter struct {
	ProjectID      string
	ConversationID string
	OwningTeamID   string
}

func (s *Server) listRunKeysByAttribution(
	ctx context.Context,
	workspaceID string,
	filter runAttributionFilter,
	limit, offset int,
) ([]string, int, error) {
	pool := s.GetPool()
	if pool == nil {
		return nil, 0, errors.New("run attribution read model is unavailable")
	}
	where := []string{"stored.namespace=$2"}
	args := []any{workspaceID, "audit:" + workspaceID}
	if filter.ProjectID != "" {
		args = append(args, filter.ProjectID)
		where = append(where, "marker.project_id=$"+strconv.Itoa(len(args)))
	}
	if filter.ConversationID != "" {
		args = append(args, filter.ConversationID)
		where = append(where, "marker.conversation_id=$"+strconv.Itoa(len(args)))
	}
	if filter.OwningTeamID != "" {
		args = append(args, filter.OwningTeamID)
		where = append(where, "marker.team_id=$"+strconv.Itoa(len(args)))
	}
	args = append(args, limit, offset)
	limitParam := strconv.Itoa(len(args) - 1)
	offsetParam := strconv.Itoa(len(args))
	rows, err := pool.Query(ctx, `
		SELECT stored.key, count(*) OVER ()
		FROM loom_store AS stored
		JOIN weave_run_terminal_markers AS marker
		  ON marker.workspace_id=$1 AND marker.run_id=stored.key
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY stored.updated_at DESC, stored.key
		LIMIT $`+limitParam+` OFFSET $`+offsetParam, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	keys := make([]string, 0, limit)
	total := 0
	for rows.Next() {
		var key string
		if err := rows.Scan(&key, &total); err != nil {
			return nil, 0, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(keys) == 0 {
		countArgs := args[:len(args)-2]
		if err := pool.QueryRow(ctx, `
			SELECT count(*)
			FROM loom_store AS stored
			JOIN weave_run_terminal_markers AS marker
			  ON marker.workspace_id=$1 AND marker.run_id=stored.key
			WHERE `+strings.Join(where, " AND "), countArgs...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}
	return keys, total, nil
}

func (s *Server) runProjectAttribution(
	ctx context.Context,
	workspaceID, runID string,
) (string, string, error) {
	projectID, _, attribution, err := s.runAttribution(ctx, workspaceID, runID)
	return projectID, attribution, err
}

func (s *Server) runAttribution(
	ctx context.Context,
	workspaceID, runID string,
) (string, string, string, error) {
	pool := s.GetPool()
	if pool == nil {
		return "", "", "legacy_unattributed", nil
	}
	var projectID, conversationID *string
	err := pool.QueryRow(ctx, `
		SELECT project_id, conversation_id
		FROM weave_run_terminal_markers
		WHERE workspace_id=$1 AND run_id=$2
	`, workspaceID, runID).Scan(&projectID, &conversationID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && projectID == nil) {
		conversationValue := ""
		if conversationID != nil {
			conversationValue = *conversationID
		}
		return "", conversationValue, "legacy_unattributed", nil
	}
	if err != nil {
		return "", "", "", err
	}
	conversationValue := ""
	if conversationID != nil {
		conversationValue = *conversationID
	}
	return *projectID, conversationValue, "project_attributed", nil
}

func (s *Server) handleGetRun(c echo.Context) error {
	tenant := getTenant(c)
	runID := c.Param("id")
	selector, teamAware, selectorErr := parseTeamSelector(c.QueryParams(), runID, false)
	if selectorErr != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_team_selector"})
	}
	if teamAware {
		return s.handleTeamAwareLeg(c, selector)
	}
	ns := "audit:" + tenant

	data, err := s.Store.Get(c.Request().Context(), ns, runID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "run not found"})
	}

	var run map[string]any
	if err := json.Unmarshal(data, &run); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "corrupt run data"})
	}
	projectID, conversationID, attribution, err := s.runAttribution(c.Request().Context(), tenant, runID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "run_attribution_failed"})
	}
	if projectID != "" {
		run["project_id"] = projectID
	}
	if conversationID != "" {
		run["conversation_id"] = conversationID
	}
	run["attribution"] = attribution
	return c.JSON(http.StatusOK, run)
}

type stopRunRequest struct {
	Reason         string `json:"reason"`
	IdempotencyKey string `json:"idempotency_key"`
	GraceSeconds   int    `json:"grace_seconds,omitempty"`
}

func (s *Server) handleRetryRunStage(c echo.Context) error {
	if s.teamRunStageRetry == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "team_run_stage_retry_unavailable"})
	}
	var request struct {
		IdempotencyKey        string `json:"idempotency_key"`
		AuthorizedTotalRounds uint64 `json:"authorized_total_rounds,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil && err != io.EOF {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_stage_retry_request"})
	}
	if decoder.Decode(new(any)) != io.EOF || len(request.IdempotencyKey) > 256 || strings.TrimSpace(request.IdempotencyKey) != request.IdempotencyKey {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_stage_retry_request"})
	}
	result, err := s.teamRunStageRetry.Retry(c.Request().Context(), teamrun.StageRetryRequest{
		WorkspaceID: getTenant(c), RunID: c.Param("id"), NodeID: strings.TrimSpace(c.Param("node_id")),
		IdempotencyKey: request.IdempotencyKey, AuthorizedTotalRounds: request.AuthorizedTotalRounds, Actor: getUserID(c),
	})
	if err != nil {
		switch {
		case errors.Is(err, teamrun.ErrTeamRunIdentityMismatch):
			return c.JSON(http.StatusNotFound, map[string]string{"error": "run_not_found"})
		case errors.Is(err, teamrun.ErrTeamRunResumeInvalid):
			return c.JSON(http.StatusConflict, map[string]string{"error": "stage_resume_not_authorized"})
		case errors.Is(err, teamrun.ErrTeamRunStateConflict):
			return c.JSON(http.StatusConflict, map[string]string{"error": "stage_not_retryable"})
		default:
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "stage_retry_failed"})
		}
	}
	return c.JSON(http.StatusAccepted, result)
}

type runActivityMember struct {
	AgentID string                    `json:"agent_id"`
	Name    string                    `json:"name"`
	Duty    string                    `json:"duty,omitempty"`
	Role    string                    `json:"role"`
	Status  string                    `json:"status"`
	Runtime *runActivityMemberRuntime `json:"runtime,omitempty"`
	Stages  []runActivityMemberStage  `json:"stages"`
}

type runActivityMemberRuntime struct {
	UpdateMode          string   `json:"update_mode"`
	ConfiguredEndpoint  string   `json:"configured_endpoint,omitempty"`
	ConfiguredModel     string   `json:"configured_model,omitempty"`
	ConfigurationSource string   `json:"configuration_source,omitempty"`
	AuthMode            string   `json:"auth_mode,omitempty"`
	ReportedModels      []string `json:"reported_models,omitempty"`
	RuntimeID           string   `json:"runtime_id,omitempty"`
	Name                string   `json:"name,omitempty"`
	Engine              string   `json:"engine,omitempty"`
	Provider            string   `json:"provider,omitempty"`
	Model               string   `json:"model,omitempty"`
}

type runActivityMemberInputRef struct {
	Name         string `json:"name"`
	ExpectedType string `json:"expected_type"`
	Source       string `json:"source"`
	NodeID       string `json:"node_id,omitempty"`
	Path         string `json:"path,omitempty"`
	Iteration    string `json:"iteration,omitempty"`
	Summary      string `json:"summary,omitempty"`
}

type runActivityMemberBudgetPause struct {
	Reason                string `json:"reason"`
	RoundsUsed            uint64 `json:"rounds_used"`
	AuthorizedTotalRounds uint64 `json:"authorized_total_rounds"`
}

type runActivityMemberStage struct {
	BudgetPause            *runActivityMemberBudgetPause `json:"budget_pause,omitempty"`
	MemberRunID            string                        `json:"member_run_id,omitempty"`
	CheckpointSavedAt      *time.Time                    `json:"checkpoint_saved_at,omitempty"`
	CurrentTaskID          string                        `json:"current_task_id,omitempty"`
	PublicUpdates          []runActivityPublicUpdate     `json:"public_updates,omitempty"`
	PublicUpdatesTruncated bool                          `json:"public_updates_truncated,omitempty"`
	PublicUpdatesState     string                        `json:"public_updates_state,omitempty"`
	NodeID                 string                        `json:"node_id"`
	Name                   string                        `json:"name"`
	Status                 string                        `json:"status"`
	Inputs                 []runActivityMemberInputRef   `json:"inputs"`
	OutputRefs             []string                      `json:"output_refs"`
	StartedAt              *time.Time                    `json:"started_at,omitempty"`
	CompletedAt            *time.Time                    `json:"completed_at,omitempty"`
	DurationMs             int64                         `json:"duration_ms,omitempty"`
	ToolCalls              int                           `json:"tool_calls,omitempty"`
	Tools                  []runActivityTool             `json:"tools"`
	FailureClass           string                        `json:"failure_class,omitempty"`
	FailureReason          string                        `json:"failure_reason,omitempty"`
	Retryable              bool                          `json:"retryable,omitempty"`
}

type runActivityTool struct {
	TaskID      string     `json:"task_id,omitempty"`
	CallID      string     `json:"call_id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Input       string     `json:"input,omitempty"`
	Output      string     `json:"output,omitempty"`
}

type runActivityRuntime struct {
	RuntimeID string `json:"runtime_id,omitempty"`
	Name      string `json:"name"`
	Engine    string `json:"engine,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Status    string `json:"status"`
}

type runActivityStage struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type runActivityDeliverableRef struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	ContentType string    `json:"content_type"`
	Kind        string    `json:"kind"`
	NodeID      string    `json:"node_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func runActivityMembers(raw json.RawMessage, runStatus teamrun.Status) ([]runActivityMember, string) {
	if len(raw) == 0 {
		return []runActivityMember{}, "unavailable"
	}
	workers, err := snapshot.DecodeTeamWorkerSnapshot(raw)
	if err != nil {
		return []runActivityMember{}, "unavailable"
	}
	members := make([]runActivityMember, 0, len(workers))
	for _, worker := range workers {
		status := "known"
		if runStatus.Terminal() {
			status = "finished"
		}
		members = append(members, runActivityMember{
			AgentID: worker.WorkerAgentID,
			Name:    worker.Name,
			Duty:    worker.Duty,
			Role:    "worker",
			Status:  status,
			Stages:  []runActivityMemberStage{},
		})
	}
	return members, "complete"
}

func runActivityMemberInputs(inputs map[string]machine.InputBinding) []runActivityMemberInputRef {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	refs := make([]runActivityMemberInputRef, 0, len(names))
	for _, name := range names {
		binding := inputs[name]
		refs = append(refs, runActivityMemberInputRef{
			Name: name, ExpectedType: string(binding.ExpectedType),
			Source: string(binding.Value.Source), NodeID: binding.Value.NodeID,
			Path: binding.Value.Path, Iteration: string(binding.Value.Iteration),
		})
	}
	return refs
}

func runActivityBundleRuntime(bundle frozen.FrozenExecutionBundle) *runActivityMemberRuntime {
	runtime := &runActivityMemberRuntime{
		UpdateMode: "on_completion",
		RuntimeID:  bundle.Agent.RuntimeID, Engine: bundle.Agent.Engine,
		Provider: bundle.PrimaryModel.ProviderID, Model: bundle.PrimaryModel.ModelID,
	}
	if engine.IsCLIEngine(bundle.Agent.Engine) {
		runtime.Model = bundle.Agent.Model
	}
	if bundle.Runtime != nil {
		runtime.RuntimeID = bundle.Runtime.RuntimeID
		runtime.Engine = bundle.Runtime.Engine
	}
	if runtime.RuntimeID == "" && runtime.Engine == "" && runtime.Provider == "" && runtime.Model == "" {
		return nil
	}
	return runtime
}

func runActivityPublishedMembers(
	payload frozen.ArtifactPayloadV1,
	graph machine.GraphDefinition,
	runStatus teamrun.Status,
	deliverables []runActivityDeliverableRef,
) ([]runActivityMember, []runActivityRuntime) {
	bundles := make(map[string]frozen.FrozenExecutionBundle, len(payload.Bundles))
	for _, bundle := range payload.Bundles {
		bundles[bundle.Agent.AgentID] = bundle
	}
	newMember := func(agentID, duty, role string) runActivityMember {
		name := agentID
		var runtime *runActivityMemberRuntime
		if bundle, ok := bundles[agentID]; ok {
			if strings.TrimSpace(bundle.Agent.DisplayName) != "" {
				name = bundle.Agent.DisplayName
			} else if strings.TrimSpace(bundle.Agent.Name) != "" {
				name = bundle.Agent.Name
			}
			runtime = runActivityBundleRuntime(bundle)
		}
		return runActivityMember{
			AgentID: agentID, Name: name, Duty: duty, Role: role,
			Status: "pending", Runtime: runtime, Stages: []runActivityMemberStage{},
		}
	}
	members := make([]runActivityMember, 0, len(payload.Team.Workers)+1)
	memberIndex := make(map[string]int, len(payload.Team.Workers)+1)
	leadParticipates := false
	for _, node := range graph.Nodes {
		if node.Type == machine.NodeLead {
			leadParticipates = true
			break
		}
	}
	if _, frozenLead := bundles[payload.Team.LeadAgentID]; frozenLead {
		leadParticipates = true
	}
	if payload.Team.LeadAgentID != "" && leadParticipates {
		memberIndex[payload.Team.LeadAgentID] = len(members)
		members = append(members, newMember(payload.Team.LeadAgentID, "", "lead"))
	}
	for _, worker := range payload.Team.Workers {
		if _, exists := memberIndex[worker.WorkerAgentID]; exists {
			continue
		}
		memberIndex[worker.WorkerAgentID] = len(members)
		members = append(members, newMember(worker.WorkerAgentID, worker.Duty, "worker"))
	}
	outputsByNode := make(map[string][]string)
	for _, item := range deliverables {
		if item.NodeID != "" {
			outputsByNode[item.NodeID] = append(outputsByNode[item.NodeID], item.ID)
		}
	}
	for _, node := range graph.Nodes {
		agentID := ""
		switch config := node.Config.(type) {
		case machine.LeadConfig:
			agentID = payload.Team.LeadAgentID
		case machine.WorkerConfig:
			agentID = config.AgentID
		default:
			continue
		}
		index, ok := memberIndex[agentID]
		if !ok {
			continue
		}
		name := strings.TrimSpace(node.Label)
		if name == "" {
			name = node.ID
		}
		status := "pending"
		if len(outputsByNode[node.ID]) > 0 {
			status = "completed"
		} else if runStatus.Terminal() {
			status = "not_recorded"
		}
		members[index].Stages = append(members[index].Stages, runActivityMemberStage{
			NodeID: node.ID, Name: name, Status: status,
			Inputs:     runActivityMemberInputs(node.Inputs),
			OutputRefs: append([]string(nil), outputsByNode[node.ID]...),
			Tools:      []runActivityTool{},
		})
	}
	for index := range members {
		completed := 0
		for _, stage := range members[index].Stages {
			if stage.Status == "completed" {
				completed++
			}
		}
		switch {
		case len(members[index].Stages) > 0 && completed == len(members[index].Stages):
			members[index].Status = "completed"
		case runStatus == teamrun.StatusFailed:
			members[index].Status = "failed"
		case runStatus == teamrun.StatusCancelled || runStatus == teamrun.StatusAbandoned:
			members[index].Status = "stopped"
		case runStatus.Terminal():
			members[index].Status = "not_recorded"
		case completed > 0:
			members[index].Status = "partially_completed"
		default:
			members[index].Status = "pending"
		}
	}
	runtimes := make([]runActivityRuntime, 0, len(members))
	seenRuntimes := make(map[string]struct{}, len(members))
	for _, member := range members {
		if member.Runtime == nil {
			continue
		}
		key := strings.Join([]string{member.Runtime.RuntimeID, member.Runtime.Engine, member.Runtime.Provider, member.Runtime.Model}, "\x1f")
		if _, exists := seenRuntimes[key]; exists {
			continue
		}
		seenRuntimes[key] = struct{}{}
		name := member.Runtime.RuntimeID
		if name == "" {
			name = member.Runtime.Engine
		}
		if name == "" {
			name = strings.Trim(strings.Join([]string{member.Runtime.Provider, member.Runtime.Model}, "/"), "/")
		}
		runtimes = append(runtimes, runActivityRuntime{
			RuntimeID: member.Runtime.RuntimeID, Name: name,
			Engine: member.Runtime.Engine, Provider: member.Runtime.Provider,
			Model: member.Runtime.Model, Status: string(runStatus),
		})
	}
	return members, runtimes
}

func runActivityRuntimes(raw json.RawMessage, currentExecutorID *string, status teamrun.Status) ([]runActivityRuntime, string) {
	type assignment struct {
		RuntimeID string `json:"runtime_id"`
		Mode      string `json:"mode"`
		Engine    string `json:"engine"`
	}
	var frozen assignment
	assignmentKnown := len(raw) > 0 && json.Unmarshal(raw, &frozen) == nil
	runtimes := make([]runActivityRuntime, 0, 1)
	if frozen.RuntimeID != "" || frozen.Engine != "" || frozen.Mode != "" {
		name := frozen.RuntimeID
		if name == "" {
			name = frozen.Engine
		}
		runtimes = append(runtimes, runActivityRuntime{
			RuntimeID: frozen.RuntimeID,
			Name:      name,
			Engine:    frozen.Engine,
			Mode:      frozen.Mode,
			Status:    string(status),
		})
	}
	if currentExecutorID != nil && strings.TrimSpace(*currentExecutorID) != "" {
		executor := strings.TrimSpace(*currentExecutorID)
		matched := false
		for index := range runtimes {
			if runtimes[index].RuntimeID == executor || runtimes[index].Name == executor {
				runtimes[index].Status = string(status)
				matched = true
			}
		}
		if !matched {
			runtimes = append(runtimes, runActivityRuntime{Name: executor, Status: string(status)})
		}
	}
	if !assignmentKnown && len(runtimes) == 0 {
		return runtimes, "unavailable"
	}
	return runtimes, "complete"
}

func runActivityDeliverables(items []deliverable.FinalDeliverable) ([]runActivityDeliverableRef, []runActivityStage) {
	refs := make([]runActivityDeliverableRef, 0, len(items))
	stages := make([]runActivityStage, 0, len(items))
	seenStages := make(map[string]struct{}, len(items))
	for _, item := range items {
		var metadata struct {
			ArtifactKind string `json:"artifact_kind"`
			Source       string `json:"source"`
			NodeType     string `json:"node_type"`
			NodeID       string `json:"node_id"`
			NodeLabel    string `json:"node_label"`
		}
		_ = json.Unmarshal(item.Metadata, &metadata)
		kind := metadata.ArtifactKind
		// Older serial executions labeled every successful node's files final.
		// Only the workflow's delivery node establishes final delivery; keep
		// those immutable historical files visible as stage material.
		if metadata.Source == "published_workflow" && metadata.NodeType != "" && metadata.NodeType != "deliver" {
			kind = "stage"
		}
		if kind == "" {
			kind = "final"
		}
		refs = append(refs, runActivityDeliverableRef{
			ID: item.ID, Title: item.Title, ContentType: item.ContentType,
			Kind: kind, NodeID: metadata.NodeID, CreatedAt: item.CreatedAt,
		})
		if metadata.NodeID == "" {
			continue
		}
		if _, present := seenStages[metadata.NodeID]; present {
			continue
		}
		seenStages[metadata.NodeID] = struct{}{}
		name := strings.TrimSpace(metadata.NodeLabel)
		if name == "" {
			name = metadata.NodeID
		}
		stages = append(stages, runActivityStage{NodeID: metadata.NodeID, Name: name, Status: "completed"})
	}
	return refs, stages
}

func applyRunActivityEvents(members []runActivityMember, events []teamrun.ActivityEvent) {
	memberIndex := make(map[string]int, len(members))
	for index := range members {
		memberIndex[members[index].AgentID] = index
	}
	for _, event := range events {
		index, present := memberIndex[event.MemberID]
		if !present {
			continue
		}
		stageIndex := -1
		for candidate := range members[index].Stages {
			if members[index].Stages[candidate].NodeID == event.NodeID {
				stageIndex = candidate
				break
			}
		}
		if stageIndex < 0 {
			continue
		}
		stage := &members[index].Stages[stageIndex]
		var detail struct {
			DurationMs    int64             `json:"duration_ms"`
			ToolCalls     int               `json:"tool_calls"`
			ToolName      string            `json:"tool_name"`
			ToolCallID    string            `json:"tool_call_id"`
			Status        string            `json:"status"`
			Input         string            `json:"input"`
			Output        string            `json:"output"`
			InputSummary  map[string]string `json:"input_summary"`
			FailureClass  string            `json:"failure_class"`
			FailureReason string            `json:"failure_reason"`
			Retryable     bool              `json:"retryable"`
		}
		_ = json.Unmarshal(event.Detail, &detail)
		// A long tool trace can outlive the window containing member_started.
		// These events still prove current work, even when an older output exists.
		if event.Kind == "tool_started" || event.Kind == "tool_completed" {
			if stage.Status != "running" {
				stage.StartedAt = nil
				stage.DurationMs, stage.ToolCalls = 0, 0
			}
			stage.CompletedAt = nil
			stage.FailureClass, stage.FailureReason = "", ""
			stage.Retryable = false
			stage.Status, members[index].Status = "running", "running"
		}
		switch event.Kind {
		case "member_started":
			occurred := event.OccurredAt
			stage.StartedAt = &occurred
			stage.CompletedAt = nil
			stage.DurationMs = 0
			stage.ToolCalls = 0
			stage.Tools = nil
			stage.FailureClass = ""
			stage.FailureReason = ""
			stage.Retryable = false
			stage.Status = "running"
			members[index].Status = "running"
			for inputIndex := range stage.Inputs {
				stage.Inputs[inputIndex].Summary = detail.InputSummary[stage.Inputs[inputIndex].Name]
			}
		case "member_completed":
			occurred := event.OccurredAt
			stage.CompletedAt = &occurred
			stage.DurationMs = detail.DurationMs
			stage.ToolCalls = detail.ToolCalls
			stage.Status = "completed"
			stage.FailureClass, stage.FailureReason = "", ""
			stage.Retryable = false
		case "member_failed":
			occurred := event.OccurredAt
			stage.CompletedAt = &occurred
			stage.DurationMs = detail.DurationMs
			stage.Status = "failed"
			stage.FailureClass = detail.FailureClass
			stage.FailureReason = detail.FailureReason
			stage.Retryable = detail.Retryable
			members[index].Status = "failed"
		case "tool_started":
			occurred := event.OccurredAt
			stage.Tools = append(stage.Tools, runActivityTool{CallID: detail.ToolCallID, Name: detail.ToolName,
				Status: "running", StartedAt: &occurred, Input: detail.Input})
		case "tool_completed":
			occurred := event.OccurredAt
			found := false
			for toolIndex := range stage.Tools {
				if stage.Tools[toolIndex].CallID == detail.ToolCallID {
					stage.Tools[toolIndex].Status = detail.Status
					stage.Tools[toolIndex].CompletedAt = &occurred
					if detail.Input != "" {
						stage.Tools[toolIndex].Input = detail.Input
					}
					stage.Tools[toolIndex].Output = detail.Output
					found = true
					break
				}
			}
			if !found {
				stage.Tools = append(stage.Tools, runActivityTool{CallID: detail.ToolCallID, Name: detail.ToolName,
					Status: detail.Status, CompletedAt: &occurred, Input: detail.Input, Output: detail.Output})
			}
		}
	}
	summarizeRunActivityMembers(members)
}

func summarizeRunActivityMembers(members []runActivityMember) {
	for index := range members {
		member := &members[index]
		if len(member.Stages) == 0 {
			continue
		}
		counts := make(map[string]int)
		for _, stage := range member.Stages {
			counts[stage.Status]++
		}
		switch {
		case counts["failed"] > 0:
			member.Status = "failed"
		case counts["running"] > 0:
			member.Status = "running"
		case counts["completed"] == len(member.Stages):
			member.Status = "completed"
		case counts["cancelled"] > 0:
			member.Status = "cancelled"
		case counts["completed"] > 0:
			member.Status = "partially_completed"
		case counts["pending"] == len(member.Stages):
			member.Status = "pending"
		case counts["not_recorded"] > 0:
			member.Status = "not_recorded"
		}
	}
}

func enrichRunActivityRuntimes(
	ctx context.Context,
	store *runtimes.Store,
	workspaceID string,
	members []runActivityMember,
	activityRuntimes []runActivityRuntime,
) {
	if store == nil {
		return
	}
	stored, err := store.List(ctx, workspaceID)
	if err != nil {
		return
	}
	byID := make(map[string]runtimes.Runtime, len(stored))
	for _, runtime := range stored {
		byID[runtime.ID] = runtime
	}
	for memberIndex := range members {
		if members[memberIndex].Runtime == nil {
			continue
		}
		if runtime, ok := byID[members[memberIndex].Runtime.RuntimeID]; ok {
			members[memberIndex].Runtime.Name = runtime.Name
			members[memberIndex].Runtime.UpdateMode = "on_completion"
			if capability, exists := runtime.EngineCapabilities[members[memberIndex].Runtime.Engine]; exists {
				members[memberIndex].Runtime.ConfiguredEndpoint = capability.ConfiguredEndpoint
				members[memberIndex].Runtime.ConfiguredModel = capability.ConfiguredModel
				members[memberIndex].Runtime.ConfigurationSource = capability.ConfigurationSource
				members[memberIndex].Runtime.AuthMode = capability.AuthMode
			}
			if capability, exists := runtime.EngineCapabilities[members[memberIndex].Runtime.Engine]; exists && (capability.Engine == "codex" || capability.Engine == "claude") && capability.PublicEvents {
				members[memberIndex].Runtime.UpdateMode = "live"
			}
		}
	}
	for index := range activityRuntimes {
		if runtime, ok := byID[activityRuntimes[index].RuntimeID]; ok {
			activityRuntimes[index].Name = runtime.Name
		}
	}
}

// Current member execution overrides historical outputs for the same node.
// Keep recorded non-member nodes such as transforms, delivery and human waits.
func runActivityCurrentStages(members []runActivityMember, recorded []runActivityStage) []runActivityStage {
	stages := append([]runActivityStage{}, recorded...)
	byNode := make(map[string]int, len(stages))
	for index, stage := range stages {
		byNode[stage.NodeID] = index
	}
	for _, member := range members {
		for _, stage := range member.Stages {
			current := runActivityStage{NodeID: stage.NodeID, Name: stage.Name, Status: stage.Status}
			if index, exists := byNode[stage.NodeID]; exists {
				stages[index] = current
			} else {
				byNode[stage.NodeID] = len(stages)
				stages = append(stages, current)
			}
		}
	}
	return stages
}

func runActivityStageProgress(stages []runActivityStage) (int, int) {
	completed := 0
	for _, stage := range stages {
		if stage.Status == "completed" {
			completed++
		}
	}
	return completed, len(stages)
}

func refineRunActivityCompleteness(
	completeness map[string]string,
	members []runActivityMember,
	runStatus teamrun.Status,
) {
	if completeness["members"] != "complete" || completeness["activity_events"] != "complete" {
		return
	}
	executedStages := 0
	inputsComplete := true
	outputsComplete := completeness["deliverables"] == "complete"
	toolsComplete := true
	for _, member := range members {
		for _, stage := range member.Stages {
			switch stage.Status {
			case "running", "completed", "failed":
				executedStages++
			case "pending":
				if runStatus.Terminal() {
					inputsComplete = false
					outputsComplete = false
					toolsComplete = false
				}
				continue
			case "not_recorded":
				inputsComplete = false
				outputsComplete = false
				toolsComplete = false
				continue
			default:
				continue
			}
			if stage.StartedAt == nil {
				inputsComplete = false
				toolsComplete = false
			}
			if stage.Status == "completed" && len(stage.OutputRefs) == 0 {
				outputsComplete = false
			}
			completedTools := 0
			for _, tool := range stage.Tools {
				if tool.Status == "ok" || tool.Status == "error" {
					completedTools++
				}
				if runStatus.Terminal() && tool.Status == "running" {
					toolsComplete = false
				}
			}
			if stage.Status == "completed" || stage.Status == "failed" {
				if stage.ToolCalls != completedTools {
					toolsComplete = false
				}
			}
		}
	}
	if executedStages == 0 {
		return
	}
	if inputsComplete {
		completeness["member_inputs"] = "complete"
	}
	if outputsComplete {
		completeness["member_outputs"] = "complete"
	}
	if toolsComplete {
		completeness["member_tool_activity"] = "complete"
	}
	if completeness["member_inputs"] == "complete" &&
		completeness["member_outputs"] == "complete" &&
		completeness["member_tool_activity"] == "complete" {
		completeness["stages"] = "complete"
	}
}

func latestRunActivityStage(members []runActivityMember, fallback []runActivityStage) string {
	var latestName string
	var latestAt time.Time
	for _, member := range members {
		for _, stage := range member.Stages {
			occurredAt := stage.StartedAt
			if stage.CompletedAt != nil {
				occurredAt = stage.CompletedAt
			}
			if occurredAt != nil && (latestName == "" || occurredAt.After(latestAt)) {
				latestName = stage.Name
				latestAt = *occurredAt
			}
		}
	}
	if latestName != "" {
		return latestName
	}
	for index := len(fallback) - 1; index >= 0; index-- {
		if fallback[index].Name != "" && fallback[index].Status != "pending" && fallback[index].Status != "not_recorded" {
			return fallback[index].Name
		}
	}
	return ""
}

// handleGetRunActivity is the small exact-run read contract used by
// Workbench. It intentionally returns persisted execution facts only.
func (s *Server) handleGetRunActivity(c echo.Context) error {
	if s.teamRunCancel == nil || s.teamRunCancel.Runs == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "team_run_unavailable"})
	}
	run, err := s.teamRunCancel.Runs.Get(c.Request().Context(), getTenant(c), c.Param("id"))
	if errors.Is(err, teamrun.ErrTeamRunIdentityMismatch) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "run_not_found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "run_read_failed"})
	}
	completeness := map[string]string{
		"run": "complete", "stages": "unavailable", "members": "unavailable",
		"runtimes": "unavailable", "human_tasks": "complete", "deliverables": "unavailable",
		"member_inputs": "unavailable", "member_outputs": "unavailable", "member_tool_activity": "unavailable",
		"activity_events": "unavailable", "corrections": "unavailable", "usage": "unavailable",
	}
	members := []runActivityMember{}
	runtimes := []runActivityRuntime{}
	candidateContentHash := ""
	if s.Snapshots != nil {
		if frozen, snapshotErr := s.Snapshots.GetByRunID(c.Request().Context(), getTenant(c), run.RunSnapshotID); snapshotErr == nil {
			members, completeness["members"] = runActivityMembers(frozen.TeamWorkerSnapshot, run.Status)
			runtimes, completeness["runtimes"] = runActivityRuntimes(frozen.RuntimeAssignment, run.CurrentExecutorID, run.Status)
			candidateContentHash = frozen.CandidateContentHash
		}
	}
	if len(runtimes) == 0 && run.CurrentExecutorID != nil {
		runtimes, completeness["runtimes"] = runActivityRuntimes(nil, run.CurrentExecutorID, run.Status)
	}
	humanTasks := []map[string]any{}
	if run.Status == teamrun.StatusParked && run.WaitKind != nil && *run.WaitKind == teamrun.WaitHuman {
		completeness["human_tasks"] = "unavailable"
		if s.teamRunHumanTasks != nil {
			if item, humanErr := s.teamRunHumanTasks.Get(c.Request().Context(), getTenant(c), run.RunID); humanErr == nil {
				humanTasks = append(humanTasks, map[string]any{
					"run_id": run.RunID, "node_id": item.Detail.NodeID,
					"title": item.Detail.Task.Title, "instructions": item.Detail.Task.Instructions,
					"deadline_at": item.Detail.DeadlineAt,
				})
				completeness["human_tasks"] = "complete"
			}
		}
	}
	deliverableRefs := []runActivityDeliverableRef{}
	stages := []runActivityStage{}
	if s.Deliverables != nil {
		if items, deliverableErr := s.Deliverables.List(c.Request().Context(), getTenant(c), deliverable.ListFilter{RunID: run.RunID, Limit: 101}); deliverableErr == nil {
			completeness["deliverables"] = "complete"
			if len(items) > 100 {
				items = items[:100]
				completeness["deliverables"] = "partial"
			}
			deliverableRefs, stages = runActivityDeliverables(items)
			// Workflow outputs prove completed nodes, but do not prove the
			// unpublished remainder of the workflow graph.
			if len(stages) > 0 {
				completeness["stages"] = "partial"
			}
		}
	}
	if len(humanTasks) > 0 {
		human := humanTasks[0]
		nodeID, _ := human["node_id"].(string)
		name, _ := human["title"].(string)
		stages = append(stages, runActivityStage{NodeID: nodeID, Name: name, Status: "waiting"})
		completeness["stages"] = "partial"
	}
	if s.Workflow != nil && run.WorkflowID != "" && run.WorkflowVersion > 0 {
		artifact, artifactErr := s.WorkflowArtifacts.GetArtifact(
			c.Request().Context(), getTenant(c), run.WorkflowID, run.WorkflowVersion,
		)
		if artifactErr != nil && candidateContentHash != "" {
			artifact, artifactErr = s.WorkflowArtifacts.GetCandidateArtifact(
				c.Request().Context(), getTenant(c), run.WorkflowID, run.WorkflowVersion, candidateContentHash,
			)
		}
		if artifactErr == nil {
			payload, payloadErr := frozen.DecodeArtifactEnvelopeV1(frozen.ArtifactEnvelopeV1{
				WorkspaceID: artifact.WorkspaceID, WorkflowID: artifact.WorkflowID,
				WorkflowVersion:           artifact.WorkflowVersion,
				ArtifactSchemaVersion:     artifact.ArtifactSchemaVersion,
				CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm,
				CanonicalizationVersion:   artifact.CanonicalizationVersion,
				HashAlgorithm:             artifact.HashAlgorithm, ContentHash: artifact.ContentHash,
				Payload: artifact.Payload,
			})
			if payloadErr == nil {
				if graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition); report == nil || len(report.Issues) == 0 {
					publishedMembers, publishedRuntimes := runActivityPublishedMembers(payload, graph, run.Status, deliverableRefs)
					members = publishedMembers
					completeness["members"] = "complete"
					completeness["member_inputs"] = "partial"
					if completeness["deliverables"] == "complete" {
						completeness["member_outputs"] = "partial"
					}
					if len(publishedRuntimes) > 0 {
						runtimes = publishedRuntimes
						completeness["runtimes"] = "complete"
					}
				}
			}
		}
	}
	activityEvents := []teamrun.ActivityEvent{}
	if s.teamRunActivities != nil {
		if items, activityErr := s.teamRunActivities.List(c.Request().Context(), getTenant(c), run.RunID, 501); activityErr == nil {
			completeness["activity_events"] = "complete"
			if len(items) > 500 {
				items = items[len(items)-500:]
				completeness["activity_events"] = "partial"
			}
			activityEvents = items
			applyRunActivityEvents(members, activityEvents)
			completeness["member_tool_activity"] = "partial"
		}
	}
	s.projectMemberCheckpoints(c.Request().Context(), run, members)
	waitNodeID := s.reconcileRunActivityRecovery(c.Request().Context(), run, members)
	// A claimed fanout leg may still be waiting for its physical CLI slot.
	// Apply the current engine task after recovery projects the logical leg.
	s.projectRunPublicEvents(c.Request().Context(), run, members, activityEvents, completeness)
	summarizeRunActivityMembers(members)
	if completeness["activity_events"] != "unavailable" {
		refineRunActivityCompleteness(completeness, members, run.Status)
	}
	corrections := []teamrun.Correction{}
	if s.teamRunCorrections != nil {
		if items, correctionErr := s.teamRunCorrections.List(c.Request().Context(), getTenant(c), run.RunID, 20); correctionErr == nil {
			corrections = items
			completeness["corrections"] = "complete"
		}
	}
	usage, usageKnown := s.readRunActivityUsage(c.Request().Context(), getTenant(c), run.RunID)
	if usageKnown {
		completeness["usage"] = usage.CompleteState
	}
	observedAt := time.Now().UTC()
	enrichRunActivityRuntimes(c.Request().Context(), s.Runtimes, getTenant(c), members, runtimes)
	stages = runActivityCurrentStages(members, stages)
	completedStages, totalStages := runActivityStageProgress(stages)
	return c.JSON(http.StatusOK, map[string]any{
		"schema_version":           3,
		"run_id":                   run.RunID,
		"project_id":               run.ProjectID,
		"status":                   run.Status,
		"team_id":                  run.TeamID,
		"workflow_id":              run.WorkflowID,
		"workflow_version":         run.WorkflowVersion,
		"run_snapshot_id":          run.RunSnapshotID,
		"current_executor_id":      run.CurrentExecutorID,
		"wait_kind":                run.WaitKind,
		"wait_node_id":             waitNodeID,
		"checkpoint_ref":           run.CheckpointRef,
		"cancel_requested_at":      run.CancelRequestedAt,
		"cancel_grace_deadline_at": run.CancelGraceDeadlineAt,
		"stop_unconfirmed":         s.runActivityStopUnconfirmed(c.Request().Context(), run),
		"created_at":               run.CreatedAt,
		"started_at":               run.CreatedAt,
		"updated_at":               run.UpdatedAt,
		"terminal_at":              run.TerminalAt,
		"finished_at":              run.TerminalAt,
		"tokens_in":                usage.TokensIn,
		"tokens_out":               usage.TokensOut,
		"cost_usd":                 usage.CostUSD,
		"observed_at":              observedAt,
		"revision": map[string]any{
			"team_run_generation":   run.Generation,
			"execution_lease_epoch": run.ExecutionLeaseEpoch,
			"resume_generation":     run.ResumeGeneration,
		},
		"members":          members,
		"runtimes":         runtimes,
		"stages":           stages,
		"latest_stage":     latestRunActivityStage(members, stages),
		"completed_stages": completedStages,
		"total_stages":     totalStages,
		"human_tasks":      humanTasks,
		"deliverables":     deliverableRefs,
		"delivery":         s.runDelivery(c.Request().Context(), run),
		"activity_events":  activityEvents,
		"corrections":      corrections,
		"completeness":     completeness,
	})
}

// handleStopRun exposes Weave's existing exact TeamRun cancellation state
// machine as one authenticated, idempotent product command.
func (s *Server) handleStopRun(c echo.Context) error {
	if s.teamRunCancel == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "team_run_cancel_unavailable"})
	}
	var request stopRunRequest
	if c.Request().Body != nil {
		if err := json.NewDecoder(c.Request().Body).Decode(&request); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		}
	}
	request.Reason = strings.TrimSpace(request.Reason)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.Reason == "" {
		request.Reason = "user_requested"
	}
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 256 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "idempotency_key_required"})
	}
	grace := 30 * time.Second
	if request.GraceSeconds > 0 {
		if request.GraceSeconds > 3600 {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "grace_seconds_out_of_range"})
		}
		grace = time.Duration(request.GraceSeconds) * time.Second
	}
	run, err := s.teamRunCancel.RequestCancel(c.Request().Context(), teamrun.CancelRequest{
		WorkspaceID: getTenant(c), RunID: c.Param("id"),
		CancelActor: getUserID(c), CancelReason: request.Reason,
		GraceDeadline: time.Now().UTC().Add(grace), IdempotencyKey: request.IdempotencyKey,
	})
	if errors.Is(err, teamrun.ErrTeamRunIdentityMismatch) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "run_not_found"})
	}
	if errors.Is(err, teamrun.ErrTeamRunStateConflict) {
		return c.JSON(http.StatusConflict, map[string]string{"error": "stop_conflict"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "run_stop_failed"})
	}
	return c.JSON(http.StatusAccepted, map[string]any{
		"run_id": run.RunID, "status": run.Status,
		"idempotency_key": request.IdempotencyKey,
	})
}

// internalStateKeys are checkpoint state entries never exposed via the
// read-only state endpoint (runtime plumbing and conversation payloads).
var internalStateKeys = map[string]bool{
	"messages": true, "usage": true, "context": true,
	"tenant": true, "user_id": true, "session_id": true, "agent_name": true,
}

var errRunAgentMismatch = errors.New("run agent does not match terminal marker")

func (s *Server) runCheckpointGraph(
	ctx context.Context,
	workspaceID, runID, agent string,
) (string, error) {
	legacyGraph := workspaceID + ":" + agent
	pool := s.GetPool()
	if pool == nil {
		return legacyGraph, nil
	}

	var markerAgent, attributionScope string
	var checkpointGraph, teamID, runSnapshotID *string
	err := pool.QueryRow(ctx, `
		SELECT agent, attribution_scope, checkpoint_graph, team_id, run_snapshot_id
		FROM weave_run_terminal_markers
		WHERE workspace_id=$1 AND run_id=$2
	`, workspaceID, runID).Scan(
		&markerAgent, &attributionScope, &checkpointGraph, &teamID, &runSnapshotID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return legacyGraph, nil
	}
	if err != nil {
		return "", err
	}
	if markerAgent != agent {
		return "", errRunAgentMismatch
	}
	if checkpointGraph != nil && strings.TrimSpace(*checkpointGraph) != "" {
		return *checkpointGraph, nil
	}
	if (attributionScope == "team_free_collab" || attributionScope == "fixed_workflow") &&
		teamID != nil && runSnapshotID != nil {
		return "team:" + workspaceID + ":" + *teamID + ":" + *runSnapshotID, nil
	}
	return legacyGraph, nil
}

// handleGetRunState returns business-facing state from a run's checkpoint
// (read-only; requires ?agent= to locate the graph's checkpoint namespace).
func (s *Server) handleGetRunState(c echo.Context) error {
	tenant := getTenant(c)
	runID := c.Param("id")
	agent := c.QueryParam("agent")
	if agent == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "agent query param is required"})
	}

	graphName, err := s.runCheckpointGraph(c.Request().Context(), tenant, runID, agent)
	if errors.Is(err, errRunAgentMismatch) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "run state not found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to resolve run checkpoint"})
	}
	ns := "checkpoint:" + graphName
	data, err := s.Store.Get(c.Request().Context(), ns, runID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "run state not found"})
	}

	var cp struct {
		LastStep   string         `json:"last_step"`
		YieldPhase string         `json:"yield_phase"`
		SavedAt    string         `json:"saved_at"`
		State      map[string]any `json:"state"`
	}
	if err := json.Unmarshal(data, &cp); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "corrupt checkpoint data"})
	}

	state := make(map[string]any, len(cp.State))
	for k, v := range cp.State {
		if strings.HasPrefix(k, "__") || internalStateKeys[k] {
			continue
		}
		state[k] = v
	}

	return c.JSON(http.StatusOK, map[string]any{
		"run_id":      runID,
		"agent":       agent,
		"last_step":   cp.LastStep,
		"yield_phase": cp.YieldPhase,
		"saved_at":    cp.SavedAt,
		"state":       state,
	})
}

// handleGetRunCheckpoints returns the retained per-step checkpoint metadata
// for a run. Graph names include the authenticated tenant, so history lookup
// cannot cross workspace checkpoint namespaces.
func (s *Server) handleGetRunCheckpoints(c echo.Context) error {
	tenant := getTenant(c)
	runID := c.Param("id")
	agent := c.QueryParam("agent")
	if agent == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "agent query param is required"})
	}

	graphName, err := s.runCheckpointGraph(c.Request().Context(), tenant, runID, agent)
	if errors.Is(err, errRunAgentMismatch) {
		return c.JSON(http.StatusOK, []loom.CheckpointInfo{})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to resolve run checkpoint"})
	}
	graph := loom.NewGraph(graphName, "")
	history, err := graph.History(c.Request().Context(), s.Store, runID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to list run checkpoints"})
	}
	return c.JSON(http.StatusOK, history)
}

func (s *Server) handleGetRunTrace(c echo.Context) error {
	tenant := getTenant(c)
	runID := c.Param("id")
	traceNS := "trace:" + tenant + ":" + runID

	keys, err := s.Store.List(c.Request().Context(), traceNS, "")
	if err != nil {
		return c.JSON(http.StatusOK, []json.RawMessage{})
	}

	entries := make([]json.RawMessage, 0, len(keys))
	for _, key := range keys {
		data, err := s.Store.Get(c.Request().Context(), traceNS, key)
		if err != nil {
			continue
		}
		entries = append(entries, json.RawMessage(data))
	}

	return c.JSON(http.StatusOK, entries)
}
