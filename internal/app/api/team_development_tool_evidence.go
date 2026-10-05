package api

import (
	"context"
	"encoding/json"

	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

// Development-only evidence comes from already durable tool receipts. The
// caller has verified the trial actor; ordinary production activity never
// invokes this reader or copies private tool payloads to activity events.
func (s *Server) projectDevelopmentTrialToolEvidence(ctx context.Context, run teamrun.TeamRun, members []runActivityMember, completeness map[string]string) {
	completeness["member_tool_payloads"] = "unavailable"
	if s.GetPool() == nil {
		return
	}
	report, err := loomruntime.ReadMemberToolEvidence(ctx, s.GetPool(), run.WorkspaceID, run.RunID, run.RunSnapshotID)
	if err != nil {
		return
	}
	byNode := make(map[string][]loomruntime.MemberToolEvidence)
	for _, evidence := range report.Tools {
		byNode[evidence.NodeID] = append(byNode[evidence.NodeID], evidence)
	}
	complete := !report.Partial && completeness["member_tool_activity"] == "complete"
	for memberIndex := range members {
		member := &members[memberIndex]
		for stageIndex := range member.Stages {
			stage := &member.Stages[stageIndex]
			for toolIndex := range stage.Tools {
				stage.Tools[toolIndex].InputState, stage.Tools[toolIndex].OutputState = "missing", "missing"
			}
			if stage.Status != "running" && stage.Status != "completed" && stage.Status != "failed" {
				complete = complete && len(stage.Tools) == 0 && len(byNode[stage.NodeID]) == 0
				continue
			}
			byCall := make(map[string][]loomruntime.MemberToolEvidence)
			for _, evidence := range byNode[stage.NodeID] {
				if !evidence.AmbiguousInvocation && evidence.MemberID == member.AgentID && (stage.MemberRunID == "" || stage.MemberRunID == evidence.MemberRunID) {
					byCall[evidence.CallID] = append(byCall[evidence.CallID], evidence)
				}
			}
			for toolIndex := range stage.Tools {
				tool := &stage.Tools[toolIndex]
				// Repeated model call IDs within one invocation cannot identify an
				// exact operation. Preserve lifecycle facts but do not guess payloads.
				candidates := byCall[tool.CallID]
				if len(candidates) == 1 && candidates[0].Name == tool.Name {
					evidence := candidates[0]
					tool.Input, tool.Output = evidence.Input, evidence.Output
					tool.InputState, tool.OutputState = evidence.InputState, evidence.OutputState
					tool.InputBytes, tool.OutputBytes = evidence.InputBytes, evidence.OutputBytes
				}
				if tool.InputState != "recorded" || tool.OutputState != "recorded" {
					complete = false
				}
			}
			// A recorded tool not represented in the lifecycle list is a gap,
			// even if another source reports zero measured calls.
			if len(byCall) > len(stage.Tools) {
				complete = false
			}
		}
	}
	completeness["member_tool_payloads"] = "partial"
	if complete {
		completeness["member_tool_payloads"] = "complete"
	}
}

// Journal-backed empty values are recorded facts. Keep them present on the
// wire, while preserving ordinary activity's existing omitted empty payloads.
// A missing state cannot serialize residual content from another projection.
func (tool runActivityTool) MarshalJSON() ([]byte, error) {
	type wireTool runActivityTool
	wire := struct {
		wireTool
		Input       *string `json:"input,omitempty"`
		Output      *string `json:"output,omitempty"`
		InputBytes  *int    `json:"input_bytes,omitempty"`
		OutputBytes *int    `json:"output_bytes,omitempty"`
	}{wireTool: wireTool(tool)}
	if tool.InputState == "recorded" || tool.InputState == "truncated" {
		wire.Input, wire.InputBytes = &tool.Input, &tool.InputBytes
	} else if tool.InputState == "" && tool.Input != "" {
		wire.Input = &tool.Input
	}
	if tool.OutputState == "recorded" || tool.OutputState == "truncated" {
		wire.Output, wire.OutputBytes = &tool.Output, &tool.OutputBytes
	} else if tool.OutputState == "" && tool.Output != "" {
		wire.Output = &tool.Output
	}
	return json.Marshal(wire)
}
