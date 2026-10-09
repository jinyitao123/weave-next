package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
)

// Visibility follows the run readers: a run submitted from an employee's
// desktop input is visible only to that employee, like its activity; other
// runs are visible to their submitter and to administrators.

// adminTask is one team run as the admin console lists it. Status stays the
// run's own execution status; delivery verification is a separate fact read
// from the run's delivery projection, never inferred from the run status.
type adminTask struct {
	RunID           string     `json:"run_id"`
	Title           string     `json:"title"`
	TeamID          string     `json:"team_id"`
	TeamName        string     `json:"team_name"`
	WorkflowID      string     `json:"workflow_id"`
	WorkflowVersion int        `json:"workflow_version"`
	Status          string     `json:"status"`
	WaitKind        string     `json:"wait_kind,omitempty"`
	SourceKind      string     `json:"source_kind"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	TerminalAt      *time.Time `json:"terminal_at,omitempty"`
	// FailureReason classifies a failed run's cause for people; the raw cause
	// carries internal paths and identifiers and is never returned.
	FailureReason string `json:"failure_reason,omitempty"`
	// FirstOutputAt is when the first log line of any stage arrived.
	FirstOutputAt *time.Time `json:"first_output_at,omitempty"`
}

var adminFailureReasons = []struct{ needle, reason string }{
	{"code task requires git", "git_missing"},
	{"is not in the repository", "ref_not_found"},
	{"prepare code workspace: fetch repository", "repository_fetch_failed"},
	{"could not be rebuilt exactly", "handoff_mismatch"},
	{"too large to hand over", "change_too_large"},
	{"different code versions", "handoff_conflict"},
	{"prepare code workspace", "repository_checkout_failed"},
	{"OAuth token", "engine_login_failed"},
}

func adminFailureReason(cause string) string {
	for _, candidate := range adminFailureReasons {
		if strings.Contains(cause, candidate.needle) {
			return candidate.reason
		}
	}
	return ""
}

var adminTaskFilters = map[string]string{
	"":          "",
	"active":    "AND COALESCE(r.status,'queued') IN ('queued','running','parked','cancel_requested')",
	"attention": "AND (r.status IN ('failed','abandoned') OR (r.status='parked' AND r.wait_kind IN ('human','correction','runtime')))",
	"done":      "AND r.status IN ('succeeded','cancelled')",
}

const adminTaskTitleLimit = 200

func (s *Server) handleListAdminTasks(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "task_list_unavailable"})
	}
	filter, known := adminTaskFilters[c.QueryParam("filter")]
	if !known {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_filter"})
	}
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	before := time.Now().Add(time.Minute)
	if raw := strings.TrimSpace(c.QueryParam("before")); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_cursor"})
		}
		before = parsed
	}
	tasks, err := s.queryAdminTasks(c, pool, filter, before, limit, c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_list_failed"})
	}
	if c.Param("id") != "" {
		if len(tasks) == 0 {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "task_not_found"})
		}
		return c.JSON(http.StatusOK, tasks[0])
	}
	response := map[string]any{"tasks": tasks}
	if len(tasks) == limit {
		response["next_before"] = tasks[len(tasks)-1].CreatedAt.Format(time.RFC3339Nano)
	}
	return c.JSON(http.StatusOK, response)
}

// queryAdminTasks lists the tasks the caller may see, newest first.
func (s *Server) queryAdminTasks(c echo.Context, pool *pgxpool.Pool, filter string, before time.Time, limit int, runID string) ([]adminTask, error) {
	// Admission is the durable record of a submission and exists before the
	// team run is established by its consumer, so a just-submitted task is
	// listed (as queued) and readable immediately.
	rows, err := pool.Query(c.Request().Context(), `
		SELECT q.request->>'run_id', COALESCE(q.target->>'team_id',''), COALESCE(NULLIF(t.display_name,''), t.name, ''),
		       COALESCE(q.request->'revision'->>'workflow_id',''), COALESCE((q.request->'revision'->>'workflow_version')::int, 0),
		       COALESCE(r.status,'queued'), COALESCE(r.wait_kind,''), COALESCE(r.source_kind,''), q.created_at,
		       COALESCE(r.updated_at, q.created_at), r.terminal_at, q.request->'input', COALESCE(r.cause_summary,''),
		       (SELECT min(l.occurred_at) FROM weave_task_logs l JOIN weave_task_queue tq ON tq.workspace_id=l.workspace_id AND tq.id=l.task_id
		         WHERE tq.workspace_id=q.workspace_id AND tq.run_snapshot_id=q.request->>'run_id' AND tq.kind='engine_exec')
		FROM weave_workflow_admission_requests q
		LEFT JOIN weave_team_runs r ON r.workspace_id=q.workspace_id AND r.run_id=q.request->>'run_id'
		LEFT JOIN weave_teams t ON t.workspace_id=q.workspace_id AND t.id=q.target->>'team_id'
		WHERE q.workspace_id=$1 AND q.created_at < $2 AND COALESCE(q.request->>'run_id','')<>''
		  AND ($6='' OR q.request->>'run_id'=$6)
		  AND (COALESCE(q.actor_subject->>'user_id','')=$4 OR ($5 AND COALESCE(q.target->>'input_revision_id','')=''))
		  `+filter+`
		ORDER BY q.created_at DESC, q.request_id DESC
		LIMIT $3`, getTenant(c), before, limit, getUserID(c), firstRole(c) == "admin", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []adminTask{}
	for rows.Next() {
		var task adminTask
		var input []byte
		var cause string
		if err := rows.Scan(&task.RunID, &task.TeamID, &task.TeamName, &task.WorkflowID, &task.WorkflowVersion,
			&task.Status, &task.WaitKind, &task.SourceKind, &task.CreatedAt, &task.UpdatedAt, &task.TerminalAt, &input, &cause, &task.FirstOutputAt); err != nil {
			return nil, err
		}
		task.Title = adminTaskTitle(input)
		task.FailureReason = adminFailureReason(cause)
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// adminTaskTitle uses the first line of the submitted task text. Inputs that
// are structured rather than text yield "" and the page names the team instead.
func adminTaskTitle(input []byte) string {
	var text string
	if len(input) == 0 || json.Unmarshal(input, &text) != nil {
		return ""
	}
	text = strings.TrimSpace(text)
	if line, _, found := strings.Cut(text, "\n"); found {
		text = strings.TrimSpace(line)
	}
	if runes := []rune(text); len(runes) > adminTaskTitleLimit {
		text = string(runes[:adminTaskTitleLimit]) + "…"
	}
	return text
}
