package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/base/fanout"
	"github.com/labstack/echo/v4"
)

const defaultTaskGroupListLimit = 20

const taskGroupResultSummaryLimit = 500

var taskGroupStatuses = map[string]struct{}{
	fanout.StatusActive:    {},
	fanout.StatusResolving: {},
	fanout.StatusResolved:  {},
}

// GroupListResponse is the paginated task-group response envelope.
type GroupListResponse struct {
	Groups []fanout.GroupListItem `json:"groups"`
	Total  int                    `json:"total"`
}

// GroupDetailResponse is the complete state of one task group.
type GroupDetailResponse struct {
	ID              string           `json:"id"`
	WorkspaceID     string           `json:"workspace_id"`
	AvatarAgent     string           `json:"avatar_agent"`
	UserID          string           `json:"user_id"`
	Status          string           `json:"status"`
	OriginalRequest string           `json:"original_request"`
	Quorum          int              `json:"quorum"`
	DeadlineAt      *time.Time       `json:"deadline_at"`
	GroupOutcome    string           `json:"group_outcome"`
	ResolvedAt      *time.Time       `json:"resolved_at"`
	ConversationID  string           `json:"conversation_id"`
	CardMessageID   string           `json:"card_message_id"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	Legs            []GroupDetailLeg `json:"legs"`
}

// GroupDetailLeg is the queue and run state associated with one group leg.
type GroupDetailLeg struct {
	Agent         string     `json:"agent"`
	Status        string     `json:"status"`
	JobID         string     `json:"job_id"`
	RunID         string     `json:"run_id"`
	Input         string     `json:"input"`
	Output        string     `json:"output"`
	ResultSummary string     `json:"result_summary"`
	Error         string     `json:"error"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func (s *Server) handleGetTaskGroup(c echo.Context) error {
	if s.Fanout == nil || s.Tasks == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "task groups are unavailable"})
	}

	ctx := c.Request().Context()
	snapshot, err := s.Fanout.Snapshot(ctx, getTenant(c), c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "task group not found"})
	}
	group := snapshot.Group
	workspaceScope := c.QueryParam("scope") == "workspace" && taskGroupsAdmin(c)
	if !workspaceScope && group.UserID != getUserID(c) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "task group not found"})
	}

	tasks, err := s.Tasks.ListByGroup(ctx, group.WorkspaceID, group.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	legs := make([]GroupDetailLeg, 0, len(tasks))
	for _, task := range tasks {
		legs = append(legs, GroupDetailLeg{
			Agent:         task.Agent,
			Status:        task.Status,
			JobID:         task.ID,
			RunID:         task.RunID,
			Input:         taskGroupLegInput(task.Payload),
			Output:        taskGroupLegOutput(task.Result),
			ResultSummary: taskGroupResultSummary(task.Result),
			Error:         task.Error,
			CreatedAt:     task.CreatedAt,
			StartedAt:     task.StartedAt,
			CompletedAt:   task.CompletedAt,
			UpdatedAt:     task.UpdatedAt,
		})
	}

	return c.JSON(http.StatusOK, GroupDetailResponse{
		ID:              group.ID,
		WorkspaceID:     group.WorkspaceID,
		AvatarAgent:     group.AvatarAgent,
		UserID:          group.UserID,
		Status:          group.Status,
		OriginalRequest: snapshot.OriginalRequest,
		Quorum:          group.Quorum,
		DeadlineAt:      group.DeadlineAt,
		GroupOutcome:    group.GroupOutcome,
		ResolvedAt:      group.ResolvedAt,
		ConversationID:  group.ConversationID,
		CardMessageID:   group.CardMessageID,
		CreatedAt:       group.CreatedAt,
		UpdatedAt:       group.UpdatedAt,
		Legs:            legs,
	})
}

func taskGroupLegInput(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var envelope struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(payload, &envelope) == nil && envelope.Message != "" {
		return envelope.Message
	}
	return string(payload)
}

func taskGroupLegOutput(result json.RawMessage) string {
	if len(result) == 0 {
		return ""
	}
	var envelope struct {
		Output string `json:"output"`
	}
	if json.Unmarshal(result, &envelope) == nil && envelope.Output != "" {
		return envelope.Output
	}
	return string(result)
}

func taskGroupResultSummary(result json.RawMessage) string {
	summary := taskGroupLegOutput(result)
	runes := []rune(summary)
	if len(runes) > taskGroupResultSummaryLimit {
		summary = string(runes[:taskGroupResultSummaryLimit])
	}
	return summary
}

func (s *Server) handleListTaskGroups(c echo.Context) error {
	statuses, ok := parseTaskGroupStatuses(c.QueryParam("status"))
	if !ok {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "unknown task group status"})
	}

	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	if limit <= 0 || limit > 100 {
		limit = defaultTaskGroupListLimit
	}
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if offset < 0 {
		offset = 0
	}

	userID := getUserID(c)
	if c.QueryParam("scope") == "workspace" && taskGroupsAdmin(c) {
		userID = ""
	}
	groups, total, err := s.Fanout.ListGroups(c.Request().Context(), fanout.GroupListFilter{
		WorkspaceID: getTenant(c),
		ProjectID:   c.QueryParam("project_id"),
		Statuses:    statuses,
		UserID:      userID,
		Limit:       limit,
		Offset:      offset,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, GroupListResponse{Groups: groups, Total: total})
}

func parseTaskGroupStatuses(raw string) ([]string, bool) {
	if raw == "" {
		return []string{fanout.StatusActive, fanout.StatusResolving}, true
	}
	statuses := strings.Split(raw, ",")
	for i, status := range statuses {
		status = strings.TrimSpace(status)
		if _, ok := taskGroupStatuses[status]; !ok {
			return nil, false
		}
		statuses[i] = status
	}
	return statuses, true
}

func taskGroupsAdmin(c echo.Context) bool {
	roles, _ := c.Get("roles").([]string)
	for _, role := range roles {
		if role == "admin" {
			return true
		}
	}
	return false
}
