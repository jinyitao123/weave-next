package registry

import (
	"errors"
	"fmt"
)

// ErrArchivedTeamImmutable prevents roster history from being rewritten after
// a team has been archived.
var ErrArchivedTeamImmutable = errors.New("archived team roster is immutable")

// ErrTeamWorkerIncompatible indicates that an Agent cannot be referenced as a
// versioned team worker because it contains nested orchestration.
var ErrTeamWorkerIncompatible = errors.New("team worker agent is incompatible")

// TeamRosterWorkerInput is the complete mutable TeamWorker configuration used
// by the atomic roster writer.
type TeamRosterWorkerInput struct {
	WorkerAgentID      string   `json:"worker_agent_id"`
	Duty               string   `json:"duty"`
	WhenToUse          string   `json:"when_to_use"`
	ContextInstruction string   `json:"context_instruction"`
	AllowedKinds       []string `json:"allowed_kinds"`
	DefaultKind        string   `json:"default_kind"`
	ResultRequirement  string   `json:"result_requirement"`
	Enabled            bool     `json:"enabled"`
}

// ValidateTeamWorkerAgentRecord enforces the R6 boundary that a team worker's
// own graph cannot contain cross-agent orchestration.
func ValidateTeamWorkerAgentRecord(rec *AgentRecord) error {
	if rec == nil {
		return fmt.Errorf("%w: missing agent record", ErrTeamWorkerIncompatible)
	}
	if len(rec.SubAgents) > 0 {
		return fmt.Errorf("%w: agent %q defines sub_agents", ErrTeamWorkerIncompatible, rec.Name)
	}
	if rec.GraphDefinition != nil {
		for _, step := range rec.GraphDefinition.Steps {
			if step.Type == "worker" {
				return fmt.Errorf("%w: agent %q graph contains worker step %q", ErrTeamWorkerIncompatible, rec.Name, step.Name)
			}
		}
	}
	return nil
}
