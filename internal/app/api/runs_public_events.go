package api

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/base/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/engine"
)

type runActivityPublicUpdate struct {
	EventID    string    `json:"event_id"`
	TaskID     string    `json:"task_id"`
	Seq        int64     `json:"seq"`
	Kind       string    `json:"kind"`
	Text       string    `json:"text"`
	OccurredAt time.Time `json:"occurred_at"`
	Truncated  bool      `json:"truncated,omitempty"`
}

type runPublicTask struct {
	ID, NodeID, AgentID, Status string
	Result                      json.RawMessage
}

func (s *Server) projectRunPublicEvents(ctx context.Context, run teamrun.TeamRun, members []runActivityMember, events []teamrun.ActivityEvent, completeness map[string]string) {
	if s.Pool == nil || run.RunSnapshotID == "" {
		return
	}
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT ON (agent_id,payload->>'node_id') id,payload->>'node_id',agent_id,status,result
 FROM weave_task_queue WHERE workspace_id=$1 AND run_snapshot_id=$2 AND kind='engine_exec' AND COALESCE(payload->>'node_id','')<>''
 ORDER BY agent_id,payload->>'node_id',created_at DESC,id DESC`, run.WorkspaceID, run.RunSnapshotID)
	if err != nil {
		completeness["public_updates"] = "unavailable"
		return
	}
	defer rows.Close()
	current := map[string]runPublicTask{}
	for rows.Next() {
		var task runPublicTask
		if err := rows.Scan(&task.ID, &task.NodeID, &task.AgentID, &task.Status, &task.Result); err != nil {
			completeness["public_updates"] = "unavailable"
			return
		}
		current[task.AgentID+":"+task.NodeID] = task
	}
	if rows.Err() != nil {
		completeness["public_updates"] = "unavailable"
		return
	}
	completeness["public_updates"] = completeness["activity_events"]
	applyRunPublicEvents(members, events, current, completeness["activity_events"] != "complete")
	for _, member := range members {
		for _, stage := range member.Stages {
			if stage.PublicUpdatesState == "partial" {
				completeness["public_updates"] = "partial"
			}
		}
	}
}

// Event arrival order is not attempt order. Select the current physical task
// from server-created queue rows, so an old journal replay stays historical.
func applyRunPublicEvents(members []runActivityMember, events []teamrun.ActivityEvent, current map[string]runPublicTask, windowPartial bool) {
	for memberIndex := range members {
		member := &members[memberIndex]
		for stageIndex := range member.Stages {
			stage := &member.Stages[stageIndex]
			task, present := current[member.AgentID+":"+stage.NodeID]
			if !present {
				continue
			}
			stage.CurrentTaskID = task.ID
			stage.PublicUpdates = nil
			stage.PublicUpdatesTruncated = windowPartial
			stage.PublicUpdatesState = "unavailable"
			tools := stage.Tools[:0]
			for _, tool := range stage.Tools {
				if strings.HasPrefix(tool.CallID, "task-") {
					tool.TaskID, _, _ = strings.Cut(tool.CallID, ":")
				}
				if tool.TaskID == "" || tool.TaskID == task.ID {
					tools = append(tools, tool)
				}
			}
			stage.Tools = tools
			messages := map[string]int{}
			for _, event := range events {
				if event.Kind != "runtime_public" || event.MemberID != member.AgentID || event.NodeID != stage.NodeID {
					continue
				}
				var detail struct {
					TaskID    string       `json:"task_id"`
					Seq       int64        `json:"task_seq"`
					Event     engine.Event `json:"event"`
					Truncated bool         `json:"truncated"`
				}
				if json.Unmarshal(event.Detail, &detail) != nil || detail.TaskID != task.ID {
					continue
				}
				stage.PublicUpdatesTruncated = stage.PublicUpdatesTruncated || detail.Truncated
				if stage.PublicUpdatesState == "unavailable" {
					stage.PublicUpdatesState = "live"
				}
				switch detail.Event.Kind {
				case "text":
					update := runActivityPublicUpdate{EventID: event.EventID, TaskID: task.ID, Seq: detail.Seq, Kind: "text", Text: detail.Event.Text, OccurredAt: event.OccurredAt, Truncated: detail.Truncated}
					if index, present := messages[detail.Event.CallID]; present && detail.Event.CallID != "" {
						stage.PublicUpdates[index] = update
					} else {
						messages[detail.Event.CallID] = len(stage.PublicUpdates)
						stage.PublicUpdates = append(stage.PublicUpdates, update)
					}
				case "tool_call", "tool_result":
					mergePublicTool(stage, task.ID, detail.Event, event.OccurredAt)
				case "stream_end":
					stage.PublicUpdatesState = "complete"
				}
			}
			sort.SliceStable(stage.PublicUpdates, func(i, j int) bool { return stage.PublicUpdates[i].Seq < stage.PublicUpdates[j].Seq })
			var result struct {
				Diagnostics []engine.Diagnostic `json:"diagnostics"`
			}
			_ = json.Unmarshal(task.Result, &result)
			for _, diagnostic := range result.Diagnostics {
				if diagnostic.Code == "public_events_unavailable" {
					stage.PublicUpdatesTruncated = true
				}
			}
			if stage.PublicUpdatesTruncated || task.Status != "running" && task.Status != "queued" && stage.PublicUpdatesState == "live" {
				stage.PublicUpdatesState = "partial"
			}
		}
	}
}

func mergePublicTool(stage *runActivityMemberStage, taskID string, event engine.Event, at time.Time) {
	callID := taskID + ":" + event.CallID
	for index := range stage.Tools {
		tool := &stage.Tools[index]
		if tool.CallID != callID {
			continue
		}
		tool.TaskID = taskID
		if event.Kind == "tool_call" {
			if tool.StartedAt == nil {
				tool.StartedAt = &at
			}
			if tool.Status == "running" {
				if event.Input != "" {
					tool.Input = event.Input
				}
				if event.Output != "" {
					tool.Output = event.Output
				}
			}
			return
		}
		tool.Status, tool.Output, tool.CompletedAt = event.Status, event.Output, &at
		if event.Input != "" {
			tool.Input = event.Input
		}
		return
	}
	tool := runActivityTool{TaskID: taskID, CallID: callID, Name: event.Tool, Status: event.Status, Input: event.Input, Output: event.Output}
	if event.Kind == "tool_call" {
		tool.StartedAt = &at
	} else {
		tool.CompletedAt = &at
	}
	stage.Tools = append(stage.Tools, tool)
}
