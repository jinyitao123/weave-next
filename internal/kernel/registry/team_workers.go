package registry

import (
	"errors"
	"time"
)

// ErrTeamWorkerNotFound indicates that a workspace-scoped team-worker relation does not exist.
var ErrTeamWorkerNotFound = errors.New("team worker not found")

// ErrActiveTeamRequiresEnabledWorker protects the active-team roster invariant.
var ErrActiveTeamRequiresEnabledWorker = errors.New("active team requires at least one enabled worker")

// TeamWorker is one team's relationship to a reusable worker agent.
type TeamWorker struct {
	WorkspaceID        string    `json:"workspace_id"`
	TeamID             string    `json:"team_id"`
	WorkerAgentID      string    `json:"worker_agent_id"`
	Duty               string    `json:"duty"`
	WhenToUse          string    `json:"when_to_use"`
	ContextInstruction string    `json:"context_instruction"`
	AllowedKinds       []string  `json:"allowed_kinds"`
	DefaultKind        string    `json:"default_kind"`
	ResultRequirement  string    `json:"result_requirement"`
	Enabled            bool      `json:"enabled"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// PublicationTeamRead is the authoritative team, lead version, and complete
// roster snapshot locked in a caller-owned publication transaction.
type PublicationTeamRead struct {
	WorkspaceID       string       `json:"workspace_id"`
	TeamID            string       `json:"team_id"`
	Status            string       `json:"status"`
	LeadAvatarID      string       `json:"lead_avatar_id"`
	LeadAvatarVersion int64        `json:"lead_avatar_version"`
	Workers           []TeamWorker `json:"workers"`
}

// TeamWorkerDispatchRules is the configured fanout policy for the active team
// represented by a lead avatar.
type TeamWorkerDispatchRules struct {
	TeamID           string
	LegTimeoutSec    int
	GroupDeadlineSec int
	Quorum           int
}
