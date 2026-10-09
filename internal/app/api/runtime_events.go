package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/labstack/echo/v4"
)

func (s *Server) handleRuntimeTaskEvents(c echo.Context) error {
	runtime, task, err := s.runtimeTask(c)
	if err != nil {
		return err
	}
	if s.Pool == nil || s.teamRunActivities == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "activity store unavailable")
	}
	var payload runtimes.EngineExecRequest
	if json.Unmarshal(task.Payload, &payload) != nil || !engine.PublishesPublicEvents(payload.Engine) || payload.NodeID == "" || task.RunSnapshotID == "" {
		return echo.NewHTTPError(http.StatusConflict, "task does not support public progress")
	}
	var request runtimeprotocol.PublicEventsRequest
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 256*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid public event batch")
	}
	if decoder.Decode(new(any)) != io.EOF || request.Versioned.Validate() != nil || len(request.Events) == 0 || len(request.Events) > runtimeprotocol.PublicEventBatchLimit {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid public event batch")
	}
	for index, event := range request.Events {
		if runtimeprotocol.ValidatePublicEvent(event) != nil || event.OccurredAt.Before(task.CreatedAt.Add(-time.Minute)) || event.OccurredAt.After(time.Now().Add(time.Minute)) ||
			index > 0 && event.Seq != request.Events[index-1].Seq+1 {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid public event")
		}
	}
	ctx := c.Request().Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var runID string
	// Match cancellation's run -> task lock order, including the activity FK.
	if err := tx.QueryRow(ctx, `SELECT run_id FROM weave_team_runs WHERE workspace_id=$1 AND run_snapshot_id=$2 FOR KEY SHARE`, runtime.WorkspaceID, task.RunSnapshotID).Scan(&runID); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "task run not found")
	}
	// Serialize batches for this physical task. This lock never nests a run
	// mutation: late events can be retained, but cannot revive a finished run.
	var status, workerID string
	var started bool
	err = tx.QueryRow(ctx, `SELECT status,COALESCE(worker_id,''),started_at IS NOT NULL FROM weave_task_queue
 WHERE workspace_id=$1 AND id=$2 AND runtime_id=$3 FOR UPDATE`, runtime.WorkspaceID, task.ID, runtime.ID).Scan(&status, &workerID, &started)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "task not found")
	}
	if !started || status == string(taskqueue.StatusQueued) || workerID != "" && workerID != runtimes.RuntimeWorkerID(runtime.WorkspaceID, runtime.ID) {
		return echo.NewHTTPError(http.StatusConflict, "task has not executed on this runtime")
	}
	var last int64
	var ended bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX((detail->>'task_seq')::bigint),0),COALESCE(BOOL_OR(detail->'event'->>'kind'='stream_end'),false) FROM weave_team_run_activity_events
 WHERE workspace_id=$1 AND run_id=$2 AND kind='runtime_public' AND detail->>'task_id'=$3`, runtime.WorkspaceID, runID, task.ID).Scan(&last, &ended); err != nil {
		return err
	}
	for _, event := range request.Events {
		id := fmt.Sprintf("runtime:%s:%d", task.ID, event.Seq)
		detail, _ := json.Marshal(map[string]any{"task_id": task.ID, "task_seq": event.Seq, "event": event.Event, "truncated": event.Truncated})
		if event.Seq <= last {
			var identical bool
			if err := tx.QueryRow(ctx, `SELECT detail=$4::jsonb AND occurred_at=$5 FROM weave_team_run_activity_events
 WHERE workspace_id=$1 AND run_id=$2 AND event_id=$3`, runtime.WorkspaceID, runID, id, string(detail), event.OccurredAt).Scan(&identical); err != nil || !identical {
				return echo.NewHTTPError(http.StatusConflict, "public event sequence was already used")
			}
			continue
		}
		if event.Seq != last+1 {
			return echo.NewHTTPError(http.StatusConflict, "public event sequence has a gap")
		}
		if ended {
			return echo.NewHTTPError(http.StatusConflict, "public event stream already ended")
		}
		if err := s.teamRunActivities.RecordTx(ctx, tx, teamrun.ActivityEvent{
			WorkspaceID: runtime.WorkspaceID, RunID: runID, EventID: id, Kind: "runtime_public",
			NodeID: payload.NodeID, MemberID: task.AgentID, MemberVersion: int64(task.AgentVersion), Detail: detail, OccurredAt: event.OccurredAt,
		}); err != nil {
			return err
		}
		last = event.Seq
		ended = event.Event.Kind == "stream_end"
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, runtimeprotocol.PublicEventsResponse{Versioned: runtimeprotocol.NewVersioned(), AckSeq: request.Events[len(request.Events)-1].Seq})
}
