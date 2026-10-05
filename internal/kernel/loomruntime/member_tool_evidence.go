package loomruntime

import (
	"context"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"github.com/jinyitao123/loom/contract"
)

const (
	MemberToolEvidenceMaxBytes = 8 * 1024
	MemberToolEvidenceMaxCount = 200
)

// MemberToolEvidence excludes model operations, checkpoint state and tool
// control-plane patches. Callers must authorize the parent run before exposing
// this bounded projection of the existing private operation journal.
type MemberToolEvidence struct {
	NodeID, MemberID, MemberRunID string
	AmbiguousInvocation           bool
	CallID, Name, Status          string
	Input, Output                 string
	InputState, OutputState       string
	InputBytes, OutputBytes       int
}

type MemberToolEvidenceReport struct {
	Tools   []MemberToolEvidence
	Partial bool
}

// ReadMemberToolEvidence selects only tool receipts from the newest invocation
// of each exact node in one parent/snapshot. A reused agent or tool call ID in a
// different node cannot change attribution. It neither executes nor rewrites
// the journal, and never reads model request/response fields into the result.
func ReadMemberToolEvidence(ctx context.Context, db memberJournalQuerier, workspaceID, parentRunID, snapshotID string) (MemberToolEvidenceReport, error) {
	if db == nil || workspaceID == "" || parentRunID == "" || snapshotID == "" {
		return MemberToolEvidenceReport{}, errors.New("member tool evidence requires an exact parent run and snapshot")
	}
	rows, err := db.Query(ctx, `WITH current_member AS (
		SELECT DISTINCT ON(node_id) node_id,member_run_id,initial_state->>'__agent_id' AS agent_id,
			count(*) OVER(PARTITION BY node_id) > 1 AS ambiguous_invocation
		FROM weave_workflow_member_runs
		WHERE workspace_id=$1 AND parent_run_id=$2 AND run_snapshot_id=$3
		ORDER BY node_id,created_at DESC,member_run_id DESC
	)
	SELECT m.node_id,m.member_run_id,COALESCE(m.agent_id,''),m.ambiguous_invocation,j.value
	FROM current_member m
	JOIN loom_store j ON j.namespace='member-operation:' || $1 AND starts_with(j.key,m.member_run_id || '/')
	WHERE convert_from(j.value,'UTF8')::jsonb->>'kind'='tool'
	ORDER BY m.node_id,j.key LIMIT $4`, workspaceID, parentRunID, snapshotID, MemberToolEvidenceMaxCount+1)
	if err != nil {
		return MemberToolEvidenceReport{}, err
	}
	defer rows.Close()
	report := MemberToolEvidenceReport{Tools: []MemberToolEvidence{}}
	for rows.Next() {
		if len(report.Tools) == MemberToolEvidenceMaxCount {
			report.Partial = true
			break
		}
		var evidence MemberToolEvidence
		var raw []byte
		if err := rows.Scan(&evidence.NodeID, &evidence.MemberRunID, &evidence.MemberID, &evidence.AmbiguousInvocation, &raw); err != nil {
			return MemberToolEvidenceReport{}, err
		}
		if !decodeMemberToolEvidence(raw, &evidence) {
			report.Partial = true
			continue
		}
		// Lifecycle events do not carry invocation identity. With more than one
		// invocation of this node, a reused call ID is insufficient to match the
		// latest journal to that lifecycle view. Do not discard history and call
		// the remaining payloads complete.
		if evidence.AmbiguousInvocation {
			report.Partial = true
		}
		report.Tools = append(report.Tools, evidence)
	}
	return report, rows.Err()
}

func decodeMemberToolEvidence(raw []byte, evidence *MemberToolEvidence) bool {
	var operation struct {
		Kind     string          `json:"kind"`
		Input    json.RawMessage `json:"input"`
		Response json.RawMessage `json:"response"`
	}
	var call contract.ToolCall
	if json.Unmarshal(raw, &operation) != nil || operation.Kind != "tool" ||
		json.Unmarshal(operation.Input, &call) != nil || call.ID == "" || call.Name == "" {
		return false
	}
	evidence.CallID, evidence.Name, evidence.Status = call.ID, call.Name, "running"
	evidence.InputState, evidence.OutputState = "missing", "missing"
	if json.Valid([]byte(call.Args)) {
		evidence.Input, evidence.InputState, evidence.InputBytes = boundedToolEvidenceValue(call.Args)
	}
	var result struct {
		CallID   string  `json:"call_id"`
		ToolName string  `json:"tool_name"`
		Content  *string `json:"content"`
		IsError  bool    `json:"is_error"`
	}
	if len(operation.Response) == 0 || json.Unmarshal(operation.Response, &result) != nil ||
		result.CallID != call.ID || result.ToolName != "" && result.ToolName != call.Name || result.Content == nil {
		return true
	}
	evidence.Status = "ok"
	if result.IsError {
		evidence.Status = "error"
	}
	evidence.Output, evidence.OutputState, evidence.OutputBytes = boundedToolEvidenceValue(*result.Content)
	return true
}

func boundedToolEvidenceValue(value string) (string, string, int) {
	bytes := len(value)
	if bytes <= MemberToolEvidenceMaxBytes {
		return value, "recorded", bytes
	}
	end := MemberToolEvidenceMaxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end], "truncated", bytes
}
