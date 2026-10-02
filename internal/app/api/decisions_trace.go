package api

import (
	"encoding/json"
	"time"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

const maxDecisionNodeOutput = 8 << 10

// decisionTraceNode is one model node of a decision workflow as recorded by
// the run ledger: who it is, where it stands and, once finished, what it said.
type decisionTraceNode struct {
	NodeID     string          `json:"node_id"`
	Type       string          `json:"type"`
	Label      string          `json:"label,omitempty"`
	AgentID    string          `json:"agent_id,omitempty"`
	AgentName  string          `json:"agent_name,omitempty"`
	Model      string          `json:"model,omitempty"`
	Status     string          `json:"status"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	DurationMs int64           `json:"duration_ms,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
}

// decisionTrace projects existing run evidence (published members, activity
// events and stage deliverables) onto the graph's model nodes, in graph order.
// It reads only this run's records and adds no state of its own.
func decisionTrace(payload frozen.ArtifactPayloadV1, graph machine.GraphDefinition, run teamrun.TeamRun, items []deliverable.FinalDeliverable, events []teamrun.ActivityEvent) map[string]any {
	refs, _ := runActivityDeliverables(items)
	members, _ := runActivityPublishedMembers(payload, graph, run.Status, refs)
	applyRunActivityEvents(members, events)
	type located struct {
		member runActivityMember
		stage  runActivityMemberStage
	}
	stages := map[string]located{}
	for _, member := range members {
		for _, stage := range member.Stages {
			stages[stage.NodeID] = located{member, stage}
		}
	}
	outputs := map[string]json.RawMessage{}
	for _, item := range items {
		var meta struct {
			NodeID string `json:"node_id"`
		}
		_ = json.Unmarshal(item.Metadata, &meta)
		if meta.NodeID == "" || len(item.Content) > maxDecisionNodeOutput || item.RunSnapshotID != run.RunSnapshotID {
			continue
		}
		if json.Valid([]byte(item.Content)) {
			outputs[meta.NodeID] = json.RawMessage(item.Content)
		} else if encoded, err := json.Marshal(item.Content); err == nil {
			outputs[meta.NodeID] = encoded
		}
	}
	nodes := []decisionTraceNode{}
	for _, node := range graph.Nodes {
		if node.Type != machine.NodeWorker && node.Type != machine.NodeLead {
			continue
		}
		entry := decisionTraceNode{NodeID: node.ID, Type: string(node.Type), Label: node.Label, Status: "pending"}
		if found, ok := stages[node.ID]; ok {
			entry.AgentID, entry.AgentName = found.member.AgentID, found.member.Name
			if found.member.Runtime != nil {
				entry.Model = found.member.Runtime.Model
			}
			entry.StartedAt, entry.FinishedAt, entry.DurationMs = found.stage.StartedAt, found.stage.CompletedAt, found.stage.DurationMs
			switch found.stage.Status {
			case "running", "failed":
				entry.Status = found.stage.Status
			case "completed":
				entry.Status = "succeeded"
			case "not_recorded":
				entry.Status = "skipped"
			}
		}
		if entry.Status != "succeeded" && (run.Status == teamrun.StatusCancelled || run.Status == teamrun.StatusAbandoned) {
			entry.Status = "cancelled"
		}
		if entry.Status == "succeeded" {
			entry.Output = outputs[node.ID]
		}
		nodes = append(nodes, entry)
	}
	return map[string]any{"nodes": nodes}
}
