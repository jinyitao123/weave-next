// Package orgspec defines product organization values shared through injected ports.
// It owns no database, directory mutation, execution loop, or scheduling state.
package orgspec

import (
	"errors"
	"time"
)

// ErrTeamNotFound indicates that a workspace-scoped team does not exist.
var ErrTeamNotFound = errors.New("team not found")

// ErrTeamArchiveBlocked means the team still owns Projects or active
// collaboration bindings and cannot be archived.
var ErrTeamArchiveBlocked = errors.New("team archive blocked by projects")

// ErrArchivedTeamImmutable prevents audit history from being rewritten after
// a team leaves the active organization directory.
var ErrArchivedTeamImmutable = errors.New("archived team is immutable")

// ErrInvalidTeamDispatchRules indicates that a dispatch rule contains a
// negative numeric value.
var ErrInvalidTeamDispatchRules = errors.New("team dispatch rule values must be non-negative")

// ErrQuorumExceedsTeamSize indicates that a rule requires more workers than
// the team currently contains.
var ErrQuorumExceedsTeamSize = errors.New("team dispatch quorum exceeds worker count")

// ErrAmbiguousTeamDispatch indicates that an avatar manages workers from more
// than one team, so no single team rule can be selected.
var ErrAmbiguousTeamDispatch = errors.New("avatar dispatch team is ambiguous")

const (
	TeamEvaluationUnevaluated = "unevaluated"
	TeamEvaluationEvaluated   = "evaluated"
)

// Workspace represents an isolated workspace.
type Workspace struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Member represents a user's membership in a workspace.
type Member struct {
	WorkspaceID string    `json:"workspace_id"`
	UserID      string    `json:"user_id"`
	Role        string    `json:"role"`
	CreatedAt   time.Time `json:"created_at"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Deleted     bool      `json:"deleted"`
}

// Team represents a named team within a workspace.
type Team struct {
	ID                     string     `json:"id"`
	WorkspaceID            string     `json:"workspace_id"`
	Name                   string     `json:"name"`
	DisplayName            string     `json:"display_name"`
	Objective              string     `json:"objective"`
	PrimaryScenario        string     `json:"primary_scenario"`
	SuccessCriteria        string     `json:"success_criteria"`
	LeadAvatarID           string     `json:"lead_avatar_id"`
	Status                 string     `json:"status"`
	DefaultWorkflowID      string     `json:"default_workflow_id,omitempty"`
	Evaluation             string     `json:"evaluation"`
	EvaluationBuildRunID   string     `json:"evaluation_build_run_id,omitempty"`
	EvaluationContractHash string     `json:"evaluation_contract_hash,omitempty"`
	EvaluatedAt            *time.Time `json:"evaluated_at,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

// UpdateTeamDesignInput contains the design-level fields that an optimize
// compiler is allowed to refresh for an existing team. It deliberately excludes
// name, lead, status, and roster membership: those remain separate governance
// surfaces.
type UpdateTeamDesignInput struct {
	Objective       string `json:"objective"`
	PrimaryScenario string `json:"primary_scenario"`
	SuccessCriteria string `json:"success_criteria"`
}

// TeamDispatchRules controls parallel dispatch for one team.
type TeamDispatchRules struct {
	TeamID           string `json:"team_id"`
	LegTimeoutSec    int    `json:"leg_timeout_sec"`
	GroupDeadlineSec int    `json:"group_deadline_sec"`
	Quorum           int    `json:"quorum"`
}

// RestoreTeamState is the baseline-only team field set restored by one
// rollback transaction: the mutable identity/scenario fields, the lifecycle
// status, and the parallel dispatch rules. Lead and workers are restored by
// the roster command in the same transaction.
type RestoreTeamState struct {
	Name            string
	Objective       string
	PrimaryScenario string
	SuccessCriteria string
	Status          string
	DispatchRules   TeamDispatchRules
}

var (
	// ErrInvalidTeamCreationInput indicates that required aggregate fields are
	// missing or that the initial worker list contains duplicate identities.
	ErrInvalidTeamCreationInput = errors.New("invalid active team creation input")
	// ErrInvalidTeamWorkerKinds indicates that a worker's allowed/default kind
	// configuration does not form a valid consult/dispatch/handoff set.
	ErrInvalidTeamWorkerKinds = errors.New("invalid team worker kinds")
	// ErrTeamLeadUnavailable indicates that the requested lead is not a live
	// avatar in the target workspace.
	ErrTeamLeadUnavailable = errors.New("team lead avatar is unavailable")
	// ErrTeamWorkerUnavailable indicates that a requested member is not a live
	// worker in the target workspace.
	ErrTeamWorkerUnavailable = errors.New("team worker is unavailable")
	// ErrTeamLeadConflict indicates that the avatar already leads an active team.
	ErrTeamLeadConflict = errors.New("team lead avatar already leads an active team")
	// ErrTeamNameConflict indicates that the workspace already contains a Team
	// with the requested name.
	ErrTeamNameConflict = errors.New("team name already exists in workspace")
)

// InitialTeamWorker describes one enabled worker created with a Team.
type InitialTeamWorker struct {
	WorkerAgentID      string   `json:"worker_agent_id"`
	Duty               string   `json:"duty"`
	WhenToUse          string   `json:"when_to_use"`
	ContextInstruction string   `json:"context_instruction"`
	AllowedKinds       []string `json:"allowed_kinds"`
	DefaultKind        string   `json:"default_kind"`
	ResultRequirement  string   `json:"result_requirement"`
}

// CreateActiveTeamInput contains the complete aggregate required to create an
// immediately usable Team.
type CreateActiveTeamInput struct {
	Name            string              `json:"name"`
	DisplayName     string              `json:"display_name,omitempty"`
	Objective       string              `json:"objective"`
	PrimaryScenario string              `json:"primary_scenario"`
	SuccessCriteria string              `json:"success_criteria"`
	LeadAvatarID    string              `json:"lead_avatar_id"`
	Workers         []InitialTeamWorker `json:"workers"`
	DesiredStatus   string              `json:"-"`
	Evaluation      string              `json:"-"`
}

// TeamWorker is one workspace-scoped Team membership and its call policy.
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

// CreateActiveTeamResult is the Team aggregate committed by CreateActiveTeam.
type CreateActiveTeamResult struct {
	Team    Team         `json:"team"`
	Workers []TeamWorker `json:"workers"`
}

// Ineligibility reasons reported by the team-creation-options read model.
// They are stable API values; each maps to the CreateActiveTeam rejection
// enforced by the shared predicates in team_eligibility.go.
const (
	// TeamCreationReasonAlreadyLeadsActiveTeam marks a lead candidate that
	// already leads an active team (teamLeadConflict / ErrTeamLeadConflict).
	TeamCreationReasonAlreadyLeadsActiveTeam = "already_leads_active_team"
	// TeamCreationReasonNestedOrchestration marks a worker whose agent record
	// fails teamWorkerRecordCompatible (ErrTeamWorkerUnavailable), whether the
	// record declares sub_agents or its graph contains a worker step.
	TeamCreationReasonNestedOrchestration = "nested_orchestration"
)

// TeamCreationLeadCandidate is one live workspace avatar and its eligibility
// to lead a new team.
type TeamCreationLeadCandidate struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	DisplayName      string `json:"display_name"`
	Eligible         bool   `json:"eligible"`
	IneligibleReason string `json:"ineligible_reason"`
}

// TeamCreationTeamReference names one team holding a worker membership row.
type TeamCreationTeamReference struct {
	TeamID   string `json:"team_id"`
	TeamName string `json:"team_name"`
}

// TeamCreationWorkerOption is one live workspace worker and its eligibility
// to join a new team. Being referenced by other teams never affects
// eligibility; the references are reported factually.
type TeamCreationWorkerOption struct {
	ID                string                      `json:"id"`
	Name              string                      `json:"name"`
	DisplayName       string                      `json:"display_name"`
	Engine            string                      `json:"engine"`
	Eligible          bool                        `json:"eligible"`
	IneligibleReason  string                      `json:"ineligible_reason"`
	ReferencedByTeams []TeamCreationTeamReference `json:"referenced_by_teams"`
}

// TeamCreationOptions is the workspace-scoped creation candidate panorama.
type TeamCreationOptions struct {
	LeadCandidates []TeamCreationLeadCandidate `json:"lead_candidates"`
	WorkerPool     []TeamCreationWorkerOption  `json:"worker_pool"`
}
