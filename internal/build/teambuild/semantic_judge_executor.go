package teambuild

import (
	"context"
	"encoding/json"
)

// SemanticJudgeRequest is the build-owned ABI for semantic judgment. Payload
// is the immutable evidence package; EvidenceHash binds retries and the
// persisted attempt to that exact package.
type SemanticJudgeRequest struct {
	WorkspaceID  string          `json:"workspace_id"`
	BuildRunID   string          `json:"build_run_id"`
	RevisionNo   int             `json:"revision_no"`
	Run          TeamBuildRun    `json:"-"`
	Payload      json.RawMessage `json:"payload"`
	EvidenceHash string          `json:"evidence_hash"`
}

// SemanticJudgeResult preserves the current attempt/run identities and usage
// ledger projection so M2c can replace the executor without changing callers
// or weakening audit/accounting semantics.
type SemanticJudgeResult struct {
	AttemptID   string           `json:"attempt_id"`
	RunID       string           `json:"run_id"`
	Output      string           `json:"output"`
	Usage       BudgetUsage      `json:"usage"`
	UsageSource BuildUsageSource `json:"usage_source"`
}

// SemanticJudgeExecutor executes one evidence-bound semantic judgment. M2b's
// implementation delegates to the existing metateam judge; M2c replaces only
// that implementation.
type SemanticJudgeExecutor interface {
	Execute(context.Context, SemanticJudgeRequest) (SemanticJudgeResult, error)
}
