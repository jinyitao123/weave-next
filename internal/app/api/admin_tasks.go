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

// Visibility: administrators and owners read every team run of the workspace,
// including the ones employees started from the desktop or Feishu; everyone
// else reads the runs they submitted. Reading is all an administrator gets on
// someone else's run: follow-ups and pull requests stay with the submitter.

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
	// Source is where the run was started: console, desktop, feishu or schedule.
	Source string `json:"source"`
	// Actor is the display name of whoever started the run.
	Actor string `json:"actor"`
	Mine  bool   `json:"mine"`
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

var adminTaskSources = map[string]bool{"console": true, "desktop": true, "feishu": true, "schedule": true}

// adminTaskQuery selects the runs a console request may see. Write is set by
// the endpoints that act on a run: they keep the narrower rule.
type adminTaskQuery struct {
	Filter string
	Before time.Time
	Limit  int
	RunID  string
	TeamID string
	Source string
	Write  bool
}

// adminRunReader reports whether the caller reads every run of the workspace.
func adminRunReader(c echo.Context) bool {
	role := firstRole(c)
	return role == "admin" || role == "owner"
}

// auditRunView records that an administrator opened someone else's run. One
// row per viewer, run and surface an hour keeps a polling page from flooding it.
func (s *Server) auditRunView(c echo.Context, runID, surface string) {
	pool := s.GetPool()
	if pool == nil || runID == "" {
		return
	}
	_, _ = pool.Exec(c.Request().Context(), `INSERT INTO weave_run_view_audit(workspace_id,run_id,viewer_id,viewer_role,surface)
		SELECT $1,$2,$3,$4,$5 WHERE NOT EXISTS (SELECT 1 FROM weave_run_view_audit
		 WHERE workspace_id=$1 AND run_id=$2 AND viewer_id=$3 AND surface=$5 AND viewed_at > now() - interval '1 hour')`,
		getTenant(c), runID, getUserID(c), firstRole(c), surface)
}

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
	source := c.QueryParam("source")
	if source != "" && !adminTaskSources[source] {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_source"})
	}
	tasks, err := s.queryAdminTasks(c, pool, adminTaskQuery{Filter: filter, Before: before, Limit: limit, RunID: c.Param("id"), TeamID: strings.TrimSpace(c.QueryParam("team")), Source: source})
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
func (s *Server) queryAdminTasks(c echo.Context, pool *pgxpool.Pool, query adminTaskQuery) ([]adminTask, error) {
	if query.Before.IsZero() {
		query.Before = time.Now().Add(time.Minute)
	}
	if query.Limit <= 0 {
		query.Limit = 1
	}
	const source = `CASE WHEN EXISTS (SELECT 1 FROM weave_feishu_messages f WHERE f.workspace_id=q.workspace_id AND f.run_id=q.request->>'run_id') THEN 'feishu'
		WHEN COALESCE(q.target->>'input_revision_id','')<>'' THEN 'desktop'
		WHEN r.source_kind='schedule' THEN 'schedule' ELSE 'console' END`
	// Admission is the durable record of a submission and exists before the
	// team run is established by its consumer, so a just-submitted task is
	// listed (as queued) and readable immediately.
	rows, err := pool.Query(c.Request().Context(), `
		SELECT q.request->>'run_id', COALESCE(q.target->>'team_id',''), COALESCE(NULLIF(t.display_name,''), t.name, ''),
		       COALESCE(q.request->'revision'->>'workflow_id',''), COALESCE((q.request->'revision'->>'workflow_version')::int, 0),
		       COALESCE(r.status,'queued'), COALESCE(r.wait_kind,''), COALESCE(r.source_kind,''), q.created_at,
		       COALESCE(r.updated_at, q.created_at), r.terminal_at, q.request->'input', COALESCE(r.cause_summary,''),
		       (SELECT min(l.occurred_at) FROM weave_task_logs l JOIN weave_task_queue tq ON tq.workspace_id=l.workspace_id AND tq.id=l.task_id
		         WHERE tq.workspace_id=q.workspace_id AND tq.run_snapshot_id=q.request->>'run_id' AND tq.kind='engine_exec'),
		       `+source+`, COALESCE(NULLIF(u.display_name,''),u.username,''), COALESCE(q.actor_subject->>'user_id','')=$4
		FROM weave_workflow_admission_requests q
		LEFT JOIN weave_team_runs r ON r.workspace_id=q.workspace_id AND r.run_id=q.request->>'run_id'
		LEFT JOIN weave_teams t ON t.workspace_id=q.workspace_id AND t.id=q.target->>'team_id'
		LEFT JOIN weave_users u ON u.id=q.actor_subject->>'user_id'
		WHERE q.workspace_id=$1 AND q.created_at < $2 AND COALESCE(q.request->>'run_id','')<>''
		  AND ($6='' OR q.request->>'run_id'=$6)
		  AND (COALESCE(q.actor_subject->>'user_id','')=$4 OR $5 OR ($7 AND COALESCE(q.target->>'input_revision_id','')=''))
		  AND ($8='' OR q.target->>'team_id'=$8)
		  AND ($9='' OR `+source+`=$9)
		  `+query.Filter+`
		ORDER BY q.created_at DESC, q.request_id DESC
		LIMIT $3`, getTenant(c), query.Before, query.Limit, getUserID(c), adminRunReader(c) && !query.Write, query.RunID,
		firstRole(c) == "admin", query.TeamID, query.Source)
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
			&task.Status, &task.WaitKind, &task.SourceKind, &task.CreatedAt, &task.UpdatedAt, &task.TerminalAt, &input, &cause, &task.FirstOutputAt,
			&task.Source, &task.Actor, &task.Mine); err != nil {
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
