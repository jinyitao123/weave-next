package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/labstack/echo/v4"
)

// adminTaskWait explains why a task is not running yet.
type adminTaskWait struct {
	Waiting  bool   `json:"waiting"`
	Reason   string `json:"reason,omitempty"` // team_pickup | node_busy | node_unavailable code
	Position int    `json:"position,omitempty"`
	NodeName string `json:"node_name,omitempty"`
	Engine   string `json:"engine,omitempty"`
}

func (s *Server) handleGetAdminTaskQueue(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "task_queue_unavailable"})
	}
	runID := c.Param("id")
	tasks, err := s.queryAdminTasks(c, pool, "", time.Now().Add(time.Minute), 1, runID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_queue_failed"})
	}
	if len(tasks) == 0 {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "task_not_found"})
	}
	if runTerminal(tasks[0].Status) {
		return c.JSON(http.StatusOK, adminTaskWait{})
	}
	ctx := c.Request().Context()
	var taskID, runtimeID string
	var payload []byte
	var createdAt time.Time
	err = pool.QueryRow(ctx, `SELECT id, COALESCE(runtime_id,''), payload, created_at FROM weave_task_queue
		WHERE workspace_id=$1 AND run_snapshot_id=$2 AND kind='engine_exec' AND status='queued' ORDER BY created_at, id LIMIT 1`, getTenant(c), runID).
		Scan(&taskID, &runtimeID, &payload, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var active bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_task_queue WHERE workspace_id=$1 AND run_snapshot_id=$2 AND kind='engine_exec' AND status IN ('dispatched','running'))`, getTenant(c), runID).Scan(&active); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_queue_failed"})
		}
		if active || tasks[0].Status == "running" || tasks[0].Status == "parked" {
			return c.JSON(http.StatusOK, adminTaskWait{})
		}
		return c.JSON(http.StatusOK, adminTaskWait{Waiting: true, Reason: "team_pickup"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_queue_failed"})
	}
	var request struct {
		Engine string `json:"engine"`
	}
	_ = json.Unmarshal(payload, &request)
	wait := adminTaskWait{Waiting: true, Engine: request.Engine, Reason: "node_busy"}
	if err := pool.QueryRow(ctx, `SELECT count(*)+1 FROM weave_task_queue
		WHERE workspace_id=$1 AND kind='engine_exec' AND status='queued' AND COALESCE(runtime_id,'')=$2 AND (created_at, id) < ($3, $4)`,
		getTenant(c), runtimeID, createdAt, taskID).Scan(&wait.Position); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_queue_failed"})
	}
	if s.Runtimes != nil && runtimeID != "" {
		if stored, err := s.Runtimes.List(ctx, getTenant(c)); err == nil {
			for _, runtime := range stored {
				if runtime.ID != runtimeID {
					continue
				}
				wait.NodeName = runtime.Name
				if reason := runtimes.UnavailableReason(runtime, request.Engine); reason != "" {
					wait.Reason = reason
				}
			}
		}
	}
	return c.JSON(http.StatusOK, wait)
}

// handleGetAdminMetrics reports the experience measurements the console
// targets: the median time from submission to the first output line.
func (s *Server) handleGetAdminMetrics(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "metrics_unavailable"})
	}
	var median *float64
	var sample int
	err := pool.QueryRow(c.Request().Context(), `
		WITH recent AS (
			SELECT q.created_at,
			       (SELECT min(l.occurred_at) FROM weave_task_logs l JOIN weave_task_queue tq ON tq.workspace_id=l.workspace_id AND tq.id=l.task_id
			         WHERE tq.workspace_id=q.workspace_id AND tq.run_snapshot_id=q.request->>'run_id' AND tq.kind='engine_exec') AS first_output
			FROM weave_workflow_admission_requests q
			WHERE q.workspace_id=$1 AND COALESCE(q.request->>'run_id','')<>''
			ORDER BY q.created_at DESC LIMIT 50
		)
		SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM first_output - created_at)), count(first_output)
		FROM recent WHERE first_output IS NOT NULL`, getTenant(c)).Scan(&median, &sample)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "metrics_failed"})
	}
	return c.JSON(http.StatusOK, map[string]any{"first_output_median_seconds": median, "sample": sample})
}
