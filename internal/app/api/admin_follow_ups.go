package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
)

// A follow-up continues a finished code task as a new run of the same team. It
// starts from exactly the commit the previous run delivered: the previous
// run's Host-written version and cumulative patch are frozen as the seed of
// the new run's code context, and the result keeps pushing to the same branch.

type adminFollowUpRequest struct {
	Task            string `json:"task"`
	ClientRequestID string `json:"client_request_id"`
}

func runTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "abandoned":
		return true
	}
	return false
}

func (s *Server) handleSubmitFollowUp(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "follow_up_unavailable"})
	}
	var request adminFollowUpRequest
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	request.Task = strings.TrimSpace(request.Task)
	if request.Task == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "请描述这一轮要做的事"})
	}
	if _, err := uuid.Parse(request.ClientRequestID); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_client_request_id"})
	}
	ctx := c.Request().Context()
	parentID := c.Param("id")
	tasks, err := s.queryAdminTasks(c, pool, adminTaskQuery{RunID: parentID, Write: true})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "follow_up_failed"})
	}
	if len(tasks) == 0 {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "task_not_found"})
	}
	parent := tasks[0]
	if !runTerminal(parent.Status) {
		return c.JSON(http.StatusConflict, map[string]string{"error": "上一轮还没有结束"})
	}
	view, found, err := loadTaskCode(ctx, pool, getTenant(c), parentID, parent.Status)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "follow_up_failed"})
	}
	if !found {
		return c.JSON(http.StatusConflict, map[string]string{"error": "只有使用代码环境的任务可以跟进"})
	}
	if view.Final == nil {
		return c.JSON(http.StatusConflict, map[string]string{"error": "上一轮没有产出代码版本"})
	}
	if view.Final.Version.Changed && view.Patch == "" {
		return c.JSON(http.StatusConflict, map[string]string{"error": "上一轮的改动过大，无法在其基础上跟进"})
	}
	var setup string
	var commands []byte
	if err := pool.QueryRow(ctx, `SELECT setup_script, verify_commands FROM weave_run_code_contexts WHERE workspace_id=$1 AND run_id=$2`, getTenant(c), parentID).Scan(&setup, &commands); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "follow_up_failed"})
	}
	root := view.RootRunID
	if root == "" {
		root = parentID
	}
	var original []byte
	if err := pool.QueryRow(ctx, `SELECT request->'input' FROM weave_workflow_admission_requests WHERE workspace_id=$1 AND request->>'run_id'=$2 LIMIT 1`, getTenant(c), root).Scan(&original); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "follow_up_failed"})
	}
	var originalText string
	_ = json.Unmarshal(original, &originalText)

	var verifyCommands []string
	if err := json.Unmarshal(commands, &verifyCommands); err != nil {
		return workflowStoreFailure(c, err)
	}
	code := &adminRunCodeContext{EnvironmentID: view.environmentID, Repository: view.Repository, Ref: view.Final.Version.BaseSHA,
		SetupScript: setup, VerifyCommands: verifyCommands, PushBranch: view.PushBranch, ParentRunID: parentID, RootRunID: root, SeedVersion: view.Final.rawVersion, SeedPatch: view.Patch}
	c.SetParamNames("id")
	c.SetParamValues(parent.TeamID)
	if ok, err := s.ensureTeamAvailable(c, getTenant(c), parent.TeamID); !ok {
		return err
	}
	return s.dispatchAdmittedTeam(c, teamDispatchRequest{Task: followUpInput(request.Task, originalText), ClientRequestID: request.ClientRequestID, codeContext: code})
}

// followUpInput leads with the follow-up so the task list shows it as the
// title, then restates the original request for the members.
func followUpInput(followUp, original string) string {
	var input strings.Builder
	input.WriteString("跟进：")
	input.WriteString(followUp)
	input.WriteString("\n\n这是同一任务的继续，仓库已包含上一轮交付的改动。")
	if original = strings.TrimSpace(original); original != "" {
		input.WriteString("\n\n原始要求：\n")
		input.WriteString(original)
	}
	return input.String()
}

// handleGetTaskThread lists the runs of one task: the original submission and
// its follow-ups, oldest first, filtered by the caller's run visibility.
func (s *Server) handleGetTaskThread(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "task_thread_unavailable"})
	}
	ctx := c.Request().Context()
	runID := c.Param("id")
	root := runID
	var storedRoot string
	err := pool.QueryRow(ctx, `SELECT root_run_id FROM weave_run_code_contexts WHERE workspace_id=$1 AND run_id=$2`, getTenant(c), runID).Scan(&storedRoot)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_thread_failed"})
	}
	if storedRoot != "" {
		root = storedRoot
	}
	rows, err := pool.Query(ctx, `SELECT run_id FROM weave_run_code_contexts WHERE workspace_id=$1 AND (run_id=$2 OR root_run_id=$2)`, getTenant(c), root)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_thread_failed"})
	}
	ids := []string{root}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_thread_failed"})
		}
		if id != root {
			ids = append(ids, id)
		}
	}
	rows.Close()
	thread := []adminTask{}
	for _, id := range ids {
		tasks, err := s.queryAdminTasks(c, pool, adminTaskQuery{RunID: id})
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_thread_failed"})
		}
		thread = append(thread, tasks...)
	}
	if len(thread) == 0 {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "task_not_found"})
	}
	sort.SliceStable(thread, func(i, j int) bool { return thread[i].CreatedAt.Before(thread[j].CreatedAt) })
	return c.JSON(http.StatusOK, map[string]any{"runs": thread})
}
