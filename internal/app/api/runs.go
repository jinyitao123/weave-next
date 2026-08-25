package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom"
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
	Timestamp      string  `json:"timestamp,omitempty"`
	ParentRunID    string  `json:"parent_run_id,omitempty"`
	ParentSeq      int64   `json:"parent_seq,omitempty"`
	ProjectID      string  `json:"project_id,omitempty"`
	ConversationID string  `json:"conversation_id,omitempty"`
	Attribution    string  `json:"attribution"`
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
