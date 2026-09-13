package registry

import (
	"errors"
)

// Link describes a relationship between two agents.
type Link struct {
	ID          string `json:"id"`
	FromAgentID string `json:"from_agent_id"`
	ToAgentID   string `json:"to_agent_id"`
	Type        string `json:"type"`
	Instruction string `json:"instruction,omitempty"`
	Kind        string `json:"kind"`
}

// DeletedLink is a removed relationship.
type DeletedLink struct {
	Link
}

// OrphanWorker identifies a worker with no incoming manages link.
type OrphanWorker struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ErrTeamRosterWriteRequired prevents legacy manages writers from becoming a
// second source of truth for TeamWorker authorization.
var ErrTeamRosterWriteRequired = errors.New("team authorization must be changed through the team roster")

// ErrTeamNotActive prevents archived or repair-pending teams from falling
// back to legacy manages authorization.
var ErrTeamNotActive = errors.New("team is not active")

// ManagedAgent is an agent record together with guidance from its manages link.
type ManagedAgent struct {
	AgentRecord
	Instruction string `json:"instruction,omitempty"`
	Kind        string `json:"kind"`
}
