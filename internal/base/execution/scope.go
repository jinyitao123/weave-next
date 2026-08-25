// Package execution defines durable execution identity shared by queue and runtime code.
package execution

// Scope controls which orchestration capabilities a version-locked Agent run has.
type Scope string

const (
	ScopeLegacyOrchestrator Scope = "legacy_orchestrator"
	ScopeTeamWorkerLeaf     Scope = "team_worker_leaf"
	ScopeTeamFreeCollab     Scope = "team_free_collab"
)

// AgentExecutionStamp is the durable agent identity and orchestration scope
// captured in every newly stamped checkpoint. A nil stamp denotes a genuinely
// unstamped legacy checkpoint; LegacyScope marks the historical two-key form
// whose effective scope is ScopeLegacyOrchestrator.
type AgentExecutionStamp struct {
	AgentID        string
	AgentVersion   int
	ExecutionScope Scope
	RunSnapshotID  string
	LegacyScope    bool
}

// Valid reports whether s is one of the durable execution scopes.
func (s Scope) Valid() bool {
	switch s {
	case ScopeLegacyOrchestrator, ScopeTeamWorkerLeaf, ScopeTeamFreeCollab:
		return true
	default:
		return false
	}
}
