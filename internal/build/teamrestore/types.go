// Package teamrestore implements the manual stepwise baseline rollback for
// team build runs (plan §7.2 / §10.4, ticket TEAM-FORGE-T15B). The service
// restores only from the server-frozen BaselineSnapshot of one optimize run:
// the inputs are workspace + build run + operator, nothing else is accepted.
// Each restore step runs in its own transaction with an idempotency key (or
// content-idempotent replay), records a best-effort audit entry, and a failed
// step stops the sequence while earlier steps stay restored.
package teamrestore

import (
	"errors"

	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// StepName identifies one restore step of the fixed sequence.
type StepName string

const (
	StepAgents     StepName = "agents"
	StepTeamRoster StepName = "team_roster"
	StepWorkflows  StepName = "workflows"
	StepComplete   StepName = "complete"
)

// StepStatus is the durable per-step verdict of one restore attempt.
type StepStatus string

const (
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
	StepPending StepStatus = "pending"
	StepSkipped StepStatus = "skipped"
)

// ErrRollbackBaselineMissing reports a control record whose claimed baseline
// disappeared; ClaimRollback normally prevents this state.
var ErrRollbackBaselineMissing = errors.New("rollback baseline missing")

// StepResult is one step's status with its reason or summary.
type StepResult struct {
	Step   StepName   `json:"step"`
	Status StepStatus `json:"status"`
	Detail string     `json:"detail,omitempty"`
}

// AgentRestoreResult is one restored agent pin: the new head version, or the
// unchanged version when the head already carried the baseline content.
type AgentRestoreResult struct {
	AgentID  string `json:"agent_id"`
	Name     string `json:"name"`
	Version  int    `json:"version"`
	Restored bool   `json:"restored"`
}

// WorkflowRestoreResult is one restored workflow: the new published version,
// a no-op replay when the published content already equals the baseline, or
// a skipped workflow without published baseline content.
type WorkflowRestoreResult struct {
	WorkflowID string `json:"workflow_id"`
	Version    int    `json:"version,omitempty"`
	Restored   bool   `json:"restored"`
	Skipped    bool   `json:"skipped,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// RollbackResult is the complete report of one rollback attempt. It is
// written into the best-effort audit detail and returned to the caller; the
// run's rollback_status is rolled_back only after every step succeeded.
type RollbackResult struct {
	BuildRunID      string                     `json:"build_run_id"`
	RollbackStatus  string                     `json:"rollback_status"`
	Steps           []StepResult               `json:"steps"`
	AgentVersions   []AgentRestoreResult       `json:"agent_versions,omitempty"`
	Roster          *registry.TeamRosterResult `json:"roster,omitempty"`
	WorkflowResults []WorkflowRestoreResult    `json:"workflow_versions,omitempty"`
	AuditWarnings   []string                   `json:"audit_warnings,omitempty"`
}

func initialSteps() []StepResult {
	return []StepResult{
		{Step: StepAgents, Status: StepPending},
		{Step: StepTeamRoster, Status: StepPending},
		{Step: StepWorkflows, Status: StepPending},
		{Step: StepComplete, Status: StepPending},
	}
}

func setStep(result RollbackResult, step StepName, status StepStatus, detail string) RollbackResult {
	for index := range result.Steps {
		if result.Steps[index].Step == step {
			result.Steps[index].Status = status
			result.Steps[index].Detail = detail
			return result
		}
	}
	return result
}

func appendAuditWarning(result RollbackResult, warning string) RollbackResult {
	result.AuditWarnings = append(result.AuditWarnings, warning)
	return result
}
