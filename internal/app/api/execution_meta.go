package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
)

const assistantExecutionSchemaVersion = 1

type executionAgentContextKey struct{}

func contextWithExecutionAgent(ctx context.Context, agent string) context.Context {
	return context.WithValue(ctx, executionAgentContextKey{}, agent)
}

func executionAgentFromContext(ctx context.Context, fallback string) string {
	agent, _ := ctx.Value(executionAgentContextKey{}).(string)
	if agent == "" {
		return fallback
	}
	return agent
}

type assistantExecutionToolCall struct {
	CallID      string     `json:"call_id,omitempty"`
	Agent       string     `json:"agent,omitempty"`
	Name        string     `json:"name"`
	Args        string     `json:"args,omitempty"`
	Result      string     `json:"result,omitempty"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type assistantExecutionSegment struct {
	ID        string                      `json:"id"`
	Type      string                      `json:"type"`
	Agent     string                      `json:"agent"`
	Content   string                      `json:"content,omitempty"`
	Tool      *assistantExecutionToolCall `json:"tool,omitempty"`
	CreatedAt time.Time                   `json:"created_at"`
}

type assistantExecutionMetadata struct {
	SchemaVersion int                          `json:"schema_version"`
	RunID         string                       `json:"run_id,omitempty"`
	Agent         string                       `json:"agent"`
	Status        string                       `json:"status"`
	StopReason    string                       `json:"stop_reason,omitempty"`
	Input         string                       `json:"input"`
	Output        string                       `json:"output"`
	StartedAt     time.Time                    `json:"started_at"`
	CompletedAt   *time.Time                   `json:"completed_at,omitempty"`
	ToolCalls     []assistantExecutionToolCall `json:"tool_calls,omitempty"`
}

// assistantExecutionRecorder records only observed runtime facts. It never
// estimates progress or invents edges that are absent from tool events.
type assistantExecutionRecorder struct {
	mu        sync.Mutex
	execution assistantExecutionMetadata
	segments  []assistantExecutionSegment
}

func newAssistantExecutionRecorder(agent, input string, startedAt time.Time) *assistantExecutionRecorder {
	return &assistantExecutionRecorder{execution: assistantExecutionMetadata{
		SchemaVersion: assistantExecutionSchemaVersion,
		Agent:         agent,
		Status:        "running",
		Input:         input,
		StartedAt:     startedAt.UTC(),
	}}
}

func (r *assistantExecutionRecorder) text(ctx context.Context, fallbackAgent, content string) {
	if r == nil || content == "" {
		return
	}
	agent := executionAgentFromContext(ctx, fallbackAgent)
	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.segments) > 0 {
		last := &r.segments[len(r.segments)-1]
		if last.Type == "text" && last.Agent == agent {
			last.Content += content
			return
		}
	}
	r.segments = append(r.segments, assistantExecutionSegment{
		ID:        "text:" + agent + ":" + now.Format(time.RFC3339Nano),
		Type:      "text",
		Agent:     agent,
		Content:   content,
		CreatedAt: now,
	})
}

func (r *assistantExecutionRecorder) toolStarted(ctx context.Context, fallbackAgent string, call contract.ToolCall) {
	if r == nil {
		return
	}
	now := time.Now().UTC()
	record := assistantExecutionToolCall{
		CallID: call.ID, Agent: executionAgentFromContext(ctx, fallbackAgent),
		Name: call.Name, Args: call.Args, Status: "running", StartedAt: now,
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.execution.ToolCalls = append(r.execution.ToolCalls, record)
	segmentRecord := record
	r.segments = append(r.segments, assistantExecutionSegment{
		ID:        "tool:" + toolSegmentID(record, len(r.segments)),
		Type:      "tool",
		Agent:     record.Agent,
		Tool:      &segmentRecord,
		CreatedAt: now,
	})
}

func (r *assistantExecutionRecorder) toolFinished(ctx context.Context, fallbackAgent string, call contract.ToolCall, result *contract.ToolResult) {
	if r == nil || result == nil {
		return
	}
	now := time.Now().UTC()
	status := "success"
	if result.IsError {
		status = "error"
	}
	agent := executionAgentFromContext(ctx, fallbackAgent)
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := len(r.execution.ToolCalls) - 1; index >= 0; index-- {
		record := &r.execution.ToolCalls[index]
		if record.Status == "running" && record.Name == call.Name &&
			(call.ID == "" || record.CallID == call.ID) && record.Agent == agent {
			record.Result = result.Content
			record.Status = status
			record.CompletedAt = &now
			r.finishToolSegment(agent, call, result.Content, status, now)
			return
		}
	}
	record := assistantExecutionToolCall{
		CallID: call.ID, Agent: agent, Name: call.Name, Args: call.Args,
		Result: result.Content, Status: status, StartedAt: now, CompletedAt: &now,
	}
	r.execution.ToolCalls = append(r.execution.ToolCalls, record)
	segmentRecord := record
	r.segments = append(r.segments, assistantExecutionSegment{
		ID:        "tool:" + toolSegmentID(record, len(r.segments)),
		Type:      "tool",
		Agent:     agent,
		Tool:      &segmentRecord,
		CreatedAt: now,
	})
}

func (r *assistantExecutionRecorder) finish(result loomruntime.Result, runErr error) {
	if r == nil {
		return
	}
	now := time.Now().UTC()
	status := "completed"
	if runErr != nil {
		status = "failed"
	} else if result.Yielded {
		status = "yielded"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.execution.RunID = result.RunID
	r.execution.Output = result.Output
	r.execution.StopReason = string(result.StopReason)
	r.execution.Status = status
	r.execution.CompletedAt = &now
	for index := range r.execution.ToolCalls {
		call := &r.execution.ToolCalls[index]
		if call.Status != "running" {
			continue
		}
		if runErr != nil {
			call.Status = "error"
		} else {
			call.Status = "stopped"
		}
		call.CompletedAt = &now
		r.finishToolSegment(call.Agent, contract.ToolCall{ID: call.CallID, Name: call.Name}, call.Result, call.Status, now)
	}
}

func (r *assistantExecutionRecorder) snapshot() assistantExecutionMetadata {
	if r == nil {
		return assistantExecutionMetadata{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot := r.execution
	snapshot.ToolCalls = append([]assistantExecutionToolCall(nil), r.execution.ToolCalls...)
	return snapshot
}

func (r *assistantExecutionRecorder) snapshotSegments() []assistantExecutionSegment {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	segments := make([]assistantExecutionSegment, len(r.segments))
	for index, segment := range r.segments {
		segments[index] = segment
		if segment.Tool != nil {
			tool := *segment.Tool
			segments[index].Tool = &tool
		}
	}
	return segments
}

func (r *assistantExecutionRecorder) lastToolCallFailed(names ...string) bool {
	if r == nil {
		return false
	}
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := len(r.execution.ToolCalls) - 1; index >= 0; index-- {
		call := r.execution.ToolCalls[index]
		if _, ok := wanted[call.Name]; ok {
			return call.Status == "error"
		}
	}
	return false
}

func (r *assistantExecutionRecorder) lastSuccessfulToolResult(name string) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := len(r.execution.ToolCalls) - 1; index >= 0; index-- {
		call := r.execution.ToolCalls[index]
		if call.Name == name && call.Status == "success" && call.Result != "" {
			return call.Result, true
		}
	}
	return "", false
}

func (r *assistantExecutionRecorder) finishToolSegment(agent string, call contract.ToolCall, result, status string, completedAt time.Time) {
	for index := len(r.segments) - 1; index >= 0; index-- {
		segment := &r.segments[index]
		if segment.Type != "tool" || segment.Tool == nil || segment.Agent != agent || segment.Tool.Name != call.Name {
			continue
		}
		if call.ID != "" && segment.Tool.CallID != call.ID {
			continue
		}
		segment.Tool.Result = result
		segment.Tool.Status = status
		segment.Tool.CompletedAt = &completedAt
		return
	}
}

func toolSegmentID(call assistantExecutionToolCall, index int) string {
	if call.CallID != "" {
		return call.CallID
	}
	return fmt.Sprintf("%s:%d", call.Name, index)
}

func mergeAssistantExecutionMetadata(metadata json.RawMessage, execution assistantExecutionMetadata) json.RawMessage {
	if execution.SchemaVersion == 0 || execution.Agent == "" {
		return metadata
	}
	fields := map[string]any{}
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &fields); err != nil {
			slog.Warn("assistant execution metadata could not be merged", "error", err)
			return metadata
		}
	}
	fields["execution"] = execution
	encoded, err := json.Marshal(fields)
	if err != nil {
		slog.Warn("assistant execution metadata could not be encoded", "error", err)
		return metadata
	}
	return encoded
}

func mergeAssistantExecutionSegments(metadata json.RawMessage, segments []assistantExecutionSegment) json.RawMessage {
	if len(segments) == 0 {
		return metadata
	}
	fields := map[string]any{}
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &fields); err != nil {
			slog.Warn("assistant execution segments could not be merged", "error", err)
			return metadata
		}
	}
	if _, exists := fields["execution_segments"]; !exists {
		fields["execution_segments"] = segments
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		slog.Warn("assistant execution segments could not be encoded", "error", err)
		return metadata
	}
	return encoded
}

// mergeTeamTemplateDraftMetadata promotes the last successfully rendered
// template draft out of the execution trace. Message timelines intentionally
// compact tool payloads, while Workbench review needs the complete
// normalized YAML after a refresh.
func mergeTeamTemplateDraftMetadata(metadata json.RawMessage, execution assistantExecutionMetadata) json.RawMessage {
	for index := len(execution.ToolCalls) - 1; index >= 0; index-- {
		call := execution.ToolCalls[index]
		if call.Name != "tf_render_template_draft" || call.Status != "success" || call.Result == "" {
			continue
		}
		var draft struct {
			SchemaVersion int             `json:"schema_version"`
			Status        string          `json:"status"`
			YAML          string          `json:"yaml"`
			Preview       json.RawMessage `json:"preview"`
			NextAction    string          `json:"next_action"`
		}
		if err := json.Unmarshal([]byte(call.Result), &draft); err != nil ||
			draft.SchemaVersion != 1 || draft.Status != "ready_for_review" ||
			draft.YAML == "" || len(draft.Preview) == 0 {
			return metadata
		}
		fields := map[string]any{}
		if len(metadata) > 0 {
			if err := json.Unmarshal(metadata, &fields); err != nil {
				return metadata
			}
		}
		fields["team_template_draft"] = json.RawMessage(call.Result)
		encoded, err := json.Marshal(fields)
		if err != nil {
			return metadata
		}
		return encoded
	}
	return metadata
}
