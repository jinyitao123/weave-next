package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/labstack/echo/v4"
)

// handleRuntimeTaskLogs stores complete log lines of an execution claimed by
// this runtime. Lines are idempotent by stream and sequence; a task stops
// accepting lines at the protocol ceiling.
func (s *Server) handleRuntimeTaskLogs(c echo.Context) error {
	runtime, task, err := s.claimedRuntimeTask(c)
	if err != nil {
		return err
	}
	pool := s.GetPool()
	if pool == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "log store unavailable")
	}
	var request runtimeprotocol.TaskLogRequest
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 4*1024*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(new(any)) != io.EOF || request.Versioned.Validate() != nil ||
		len(request.Lines) == 0 || len(request.Lines) > runtimeprotocol.TaskLogBatchLimit {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid task log batch")
	}
	for _, line := range request.Lines {
		if runtimeprotocol.ValidateTaskLogLine(line) != nil || line.OccurredAt.After(time.Now().Add(time.Minute)) {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid task log line")
		}
	}
	streams, seqs, times, texts := make([]string, 0, len(request.Lines)), make([]int64, 0, len(request.Lines)), make([]time.Time, 0, len(request.Lines)), make([]string, 0, len(request.Lines))
	for _, line := range request.Lines {
		streams, seqs, times, texts = append(streams, line.Stream), append(seqs, line.Seq), append(times, line.OccurredAt.UTC()), append(texts, line.Text)
	}
	if _, err := pool.Exec(c.Request().Context(), `INSERT INTO weave_task_logs(workspace_id,task_id,stream,seq,occurred_at,text)
		SELECT $1,$2,stream,seq,occurred_at,text FROM unnest($3::text[],$4::bigint[],$5::timestamptz[],$6::text[]) AS line(stream,seq,occurred_at,text)
		ON CONFLICT DO NOTHING`, runtime.WorkspaceID, task.ID, streams, seqs, times, texts); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "task log store failed")
	}
	return c.NoContent(http.StatusNoContent)
}

type adminLogLine struct {
	NodeID     string    `json:"node_id"`
	Stream     string    `json:"stream"`
	Seq        int64     `json:"seq"`
	OccurredAt time.Time `json:"occurred_at"`
	Text       string    `json:"text"`
}

// handleGetAdminTaskLogs pages through the complete logs of a task's stages.
// node, stream, after (sequence) and q (case-insensitive text) narrow the read.
func (s *Server) handleGetAdminTaskLogs(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "task_logs_unavailable"})
	}
	runID := c.Param("id")
	tasks, err := s.queryAdminTasks(c, pool, adminTaskQuery{RunID: runID})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_logs_failed"})
	}
	if len(tasks) == 0 {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "task_not_found"})
	}
	if c.QueryParam("summary") == "1" {
		return s.taskLogSummary(c, pool, runID)
	}
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	after, _ := strconv.ParseInt(c.QueryParam("after"), 10, 64)
	query := strings.TrimSpace(c.QueryParam("q"))
	if len(query) > 200 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_query"})
	}
	pattern := ""
	if query != "" {
		pattern = "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query) + "%"
	}
	rows, err := pool.Query(c.Request().Context(), `
		SELECT COALESCE(q.payload->>'node_id',''), l.stream, l.seq, l.occurred_at, l.text
		FROM weave_task_logs l
		JOIN weave_task_queue q ON q.workspace_id=l.workspace_id AND q.id=l.task_id
		WHERE l.workspace_id=$1 AND q.run_snapshot_id=$2 AND q.kind='engine_exec'
		  AND ($3='' OR q.payload->>'node_id'=$3) AND ($4='' OR l.stream=$4) AND l.seq > $5
		  AND ($6='' OR l.text ILIKE $6)
		ORDER BY q.created_at, l.stream, l.seq
		LIMIT $7`, getTenant(c), runID, c.QueryParam("node"), c.QueryParam("stream"), after, pattern, limit)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_logs_failed"})
	}
	defer rows.Close()
	lines := []adminLogLine{}
	for rows.Next() {
		var line adminLogLine
		if err := rows.Scan(&line.NodeID, &line.Stream, &line.Seq, &line.OccurredAt, &line.Text); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_logs_failed"})
		}
		lines = append(lines, line)
	}
	if rows.Err() != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_logs_failed"})
	}
	return c.JSON(http.StatusOK, map[string]any{"lines": lines, "complete": len(lines) < limit})
}

const (
	adminStreamInterval  = 700 * time.Millisecond
	adminStreamKeepAlive = 15 * time.Second
	adminStreamMaxLife   = 30 * time.Minute
)

// taskFingerprint changes whenever anything a task page shows may have
// changed: the run, its stages, their progress events or their logs.
func (s *Server) taskFingerprint(ctx context.Context, workspaceID, runID string) (string, string, error) {
	pool := s.GetPool()
	var status, fingerprint string
	err := pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT status FROM weave_team_runs WHERE workspace_id=$1 AND run_id=$2),'queued'),
		       concat_ws('|',
		         (SELECT status||':'||updated_at::text FROM weave_team_runs WHERE workspace_id=$1 AND run_id=$2),
		         (SELECT count(*)::text||':'||COALESCE(max(COALESCE(completed_at,started_at,created_at))::text,'') FROM weave_task_queue WHERE workspace_id=$1 AND run_snapshot_id=$2),
		         (SELECT count(*)::text FROM weave_team_run_activity_events WHERE workspace_id=$1 AND run_id=$2),
		         (SELECT count(*)::text FROM weave_task_logs l JOIN weave_task_queue q ON q.workspace_id=l.workspace_id AND q.id=l.task_id WHERE l.workspace_id=$1 AND q.run_snapshot_id=$2))`,
		workspaceID, runID).Scan(&status, &fingerprint)
	return status, fingerprint, err
}

// handleAdminTaskStream pushes an "update" event within about a second of any
// change to the task, and a final "end" once the run has finished. The page
// reloads what it shows on each event.
func (s *Server) handleAdminTaskStream(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "task_stream_unavailable"})
	}
	runID := c.Param("id")
	tasks, err := s.queryAdminTasks(c, pool, adminTaskQuery{RunID: runID})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_stream_failed"})
	}
	if len(tasks) == 0 {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "task_not_found"})
	}
	response := c.Response()
	flusher, ok := response.Writer.(http.Flusher)
	if !ok {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "streaming_unsupported"})
	}
	header := response.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	ctx := c.Request().Context()
	ticker := time.NewTicker(adminStreamInterval)
	defer ticker.Stop()
	deadline := time.Now().Add(adminStreamMaxLife)
	lastWrite := time.Now()
	previous := ""
	for {
		status, fingerprint, err := s.taskFingerprint(ctx, getTenant(c), runID)
		if err != nil {
			return nil
		}
		if fingerprint != previous {
			previous = fingerprint
			if _, err := fmt.Fprintf(response, "event: update\ndata: {\"status\":%q}\n\n", status); err != nil {
				return nil
			}
			flusher.Flush()
			lastWrite = time.Now()
		}
		if runTerminal(status) {
			_, _ = fmt.Fprint(response, "event: end\ndata: {}\n\n")
			flusher.Flush()
			return nil
		}
		if time.Since(lastWrite) > adminStreamKeepAlive {
			if _, err := fmt.Fprint(response, ": keep-alive\n\n"); err != nil {
				return nil
			}
			flusher.Flush()
			lastWrite = time.Now()
		}
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

type adminLogStream struct {
	NodeID string `json:"node_id"`
	Stream string `json:"stream"`
	Lines  int64  `json:"lines"`
	Last   int64  `json:"last_seq"`
}

// taskLogSummary lists the log streams of each stage with their size.
func (s *Server) taskLogSummary(c echo.Context, pool *pgxpool.Pool, runID string) error {
	rows, err := pool.Query(c.Request().Context(), `
		SELECT COALESCE(q.payload->>'node_id',''), l.stream, count(*), max(l.seq)
		FROM weave_task_logs l
		JOIN weave_task_queue q ON q.workspace_id=l.workspace_id AND q.id=l.task_id
		WHERE l.workspace_id=$1 AND q.run_snapshot_id=$2 AND q.kind='engine_exec' AND ($3='' OR q.payload->>'node_id'=$3)
		GROUP BY 1, 2 ORDER BY 1, 2`, getTenant(c), runID, c.QueryParam("node"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_logs_failed"})
	}
	defer rows.Close()
	streams := []adminLogStream{}
	for rows.Next() {
		var stream adminLogStream
		if err := rows.Scan(&stream.NodeID, &stream.Stream, &stream.Lines, &stream.Last); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_logs_failed"})
		}
		streams = append(streams, stream)
	}
	if rows.Err() != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_logs_failed"})
	}
	return c.JSON(http.StatusOK, map[string]any{"streams": streams})
}
