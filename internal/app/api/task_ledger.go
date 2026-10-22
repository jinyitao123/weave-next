package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/labstack/echo/v4"
)

var validJobStatuses = map[string]struct{}{
	taskqueue.StatusQueued:     {},
	taskqueue.StatusDispatched: {},
	taskqueue.StatusRunning:    {},
	taskqueue.StatusCompleted:  {},
	taskqueue.StatusFailed:     {},
	taskqueue.StatusCancelled:  {},
	taskqueue.StatusSuperseded: {},
	taskqueue.StatusCut:        {},
	taskqueue.StatusTimedOut:   {},
}

func parseJobStatuses(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	statuses := strings.Split(raw, ",")
	for _, status := range statuses {
		if _, ok := validJobStatuses[status]; !ok {
			return nil, fmt.Errorf("invalid status %q", status)
		}
	}
	return statuses, nil
}

// handleListJobs lists task ledger entries, newest first.
func (s *Server) handleListJobs(c echo.Context) error {
	tenant := getTenant(c)
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	statuses, err := parseJobStatuses(c.QueryParam("status"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	taskList, total, err := s.Tasks.ListFilteredByProject(
		c.Request().Context(), tenant, c.QueryParam("project_id"), statuses, limit, offset,
	)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, taskqueue.TaskListResponse{Jobs: taskList, Total: total})
}

// handleGetJob returns a single task ledger entry by ID.
func (s *Server) handleGetJob(c echo.Context) error {
	task, err := s.Tasks.Get(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, task)
}

// handleCancelJob cancels a queued or running task.
func (s *Server) handleCancelJob(c echo.Context) error {
	id := c.Param("id")
	var err error
	if s.TaskWorker != nil {
		err = s.TaskWorker.CancelTask(c.Request().Context(), getTenant(c), id)
	} else {
		err = s.Tasks.Cancel(c.Request().Context(), getTenant(c), id)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "cancelled"})
}
