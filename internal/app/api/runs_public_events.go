package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
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
	Engine, Model               string
	CreatedAt                   time.Time
	Result                      json.RawMessage
	StartedAt                   *time.Time
	CompletedAt                 *time.Time
	Invalidated                 bool
}

func (s *Server) projectRunPublicEvents(ctx context.Context, run teamrun.TeamRun, members []runActivityMember, events []teamrun.ActivityEvent, completeness map[string]string) {
	if s.Pool == nil || run.RunSnapshotID == "" {
		return
	}
	rows, err := s.Pool.Query(ctx, `SELECT q.id,q.payload->>'node_id',q.agent_id,q.status,q.result,q.started_at,COALESCE(q.payload->>'engine',''),COALESCE(q.payload->>'model',''),q.created_at,q.completed_at,
 EXISTS (SELECT 1 FROM weave_team_run_corrections c
 WHERE c.workspace_id=q.workspace_id AND c.run_id=$3 AND c.status='applied'
 AND c.applied_at>q.created_at AND c.affected_node_ids ? (q.payload->>'node_id'))
 FROM weave_task_queue q WHERE q.workspace_id=$1 AND q.run_snapshot_id=$2 AND q.kind='engine_exec' AND COALESCE(q.payload->>'node_id','')<>''
 ORDER BY q.agent_id,q.payload->>'node_id',q.created_at DESC,q.id DESC`, run.WorkspaceID, run.RunSnapshotID, run.RunID)
	if err != nil {
		completeness["public_updates"] = "unavailable"
		return
	}
	defer rows.Close()
	current := map[string]runPublicTask{}
	history := map[string][]runPublicTask{}
	for rows.Next() {
		var task runPublicTask
		if err := rows.Scan(&task.ID, &task.NodeID, &task.AgentID, &task.Status, &task.Result, &task.StartedAt, &task.Engine, &task.Model, &task.CreatedAt, &task.CompletedAt, &task.Invalidated); err != nil {
			completeness["public_updates"] = "unavailable"
			return
		}
		key := task.AgentID + ":" + task.NodeID
		if _, exists := current[key]; !exists {
			current[key] = task
		}
		history[key] = append(history[key], task)
	}
	if rows.Err() != nil {
		completeness["public_updates"] = "unavailable"
		return
	}
	completeness["public_updates"] = completeness["activity_events"]
	applyRunPublicEvents(members, events, current, completeness["activity_events"] != "complete")
	for memberIndex := range members {
		member := &members[memberIndex]
		for stageIndex := range member.Stages {
			stage := &member.Stages[stageIndex]
			attempts := history[member.AgentID+":"+stage.NodeID]
			if len(attempts) < 2 {
				continue
			}
			for index := len(attempts) - 1; index >= 0; index-- {
				task := attempts[index]
				var result struct {
					Status string `json:"status"`
					Safe   bool   `json:"retry_safe_before_execution"`
				}
				_ = json.Unmarshal(task.Result, &result)
				status := task.Status
				if result.Status != "" {
					status = result.Status
				}
				label := map[string]string{"queued": "等待执行", "running": "执行中", "completed": "已完成", "failed": "未完成", "timeout": "超时", "cancelled": "已停止", "cancel_requested": "停止中"}[status]
				if label == "" {
					label = "状态待确认"
				}
				model := task.Model
				if model == "" {
					model = "节点默认模型"
				}
				detail := fmt.Sprintf("平台执行记录 · 第 %d 次 · %s · %s · %s", len(attempts)-index, task.Engine, model, label)
				if result.Safe {
					detail += "；已确认尚未执行工具"
				}
				stage.PublicUpdates = append(stage.PublicUpdates, runActivityPublicUpdate{EventID: "attempt:" + task.ID, TaskID: task.ID, Seq: 1, Kind: "attempt", Text: detail, OccurredAt: task.CreatedAt})
			}
		}
	}
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
			if task.Invalidated {
				// An applied correction supersedes this attempt, even if its old
				// completion event or artifact is still in the activity window.
				stage.CurrentTaskID = ""
				stage.StartedAt, stage.CompletedAt = nil, nil
				stage.Tools, stage.PublicUpdates = nil, nil
				stage.DurationMs, stage.ToolCalls = 0, 0
				if stage.Status != "cancelled" && stage.Status != "failed" {
					stage.Status = "pending"
				}
				continue
			}
			stage.CurrentTaskID = task.ID
			if task.Status == "completed" && (stage.Status == "pending" || stage.Status == "not_recorded" || stage.Status == "completed") {
				var result struct {
					Status string `json:"status"`
				}
				// Bounded event and artifact windows are not the completion
				// ledger. Restore only an explicit successful current receipt;
				// a replay's result-fetch duration is not the CLI execution time.
				if json.Unmarshal(task.Result, &result) == nil && result.Status == "completed" {
					stage.Status = "completed"
					stage.StartedAt, stage.CompletedAt = task.StartedAt, task.CompletedAt
					if task.StartedAt != nil && task.CompletedAt != nil {
						stage.DurationMs = task.CompletedAt.Sub(*task.StartedAt).Milliseconds()
					}
				}
			}
			if stage.Status == "running" {
				if task.Status == "queued" {
					stage.Status, stage.StartedAt = "pending", nil
				} else if task.Status == "running" && task.StartedAt != nil {
					stage.StartedAt = task.StartedAt
				}
			}
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
				Diagnostics    []engine.Diagnostic `json:"diagnostics"`
				ReportedModels []string            `json:"reported_models"`
			}
			_ = json.Unmarshal(task.Result, &result)
			if member.Runtime != nil && len(result.ReportedModels) > 0 {
				member.Runtime.ReportedModels = append([]string(nil), result.ReportedModels...)
			}
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
