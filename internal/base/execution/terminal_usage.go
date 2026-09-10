package execution

// TerminalUsage is the shared usage shape for exclusive, breakdown, and subtree values.
type TerminalUsage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	ToolCalls    int     `json:"tool_calls,omitempty"`
}

// TerminalChildBreakdownV3 records one descendant's exclusive usage.
type TerminalChildBreakdownV3 struct {
	RunID           string        `json:"run_id"`
	ParentRunID     string        `json:"parent_run_id"`
	ParentSeq       int64         `json:"parent_seq"`
	Agent           string        `json:"agent"`
	TeamID          *string       `json:"team_id,omitempty"`
	WorkflowID      *string       `json:"workflow_id,omitempty"`
	WorkflowVersion *int          `json:"workflow_version,omitempty"`
	RunSnapshotID   *string       `json:"run_snapshot_id,omitempty"`
	SelfExclusive   TerminalUsage `json:"self_exclusive"`
}
