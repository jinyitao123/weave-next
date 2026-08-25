package registry

import (
	"errors"
	"time"
)

var (
	// ErrTeamRosterInvalidRequest indicates that a roster command is not a
	// complete canonical control-plane request.
	ErrTeamRosterInvalidRequest = errors.New("invalid team roster request")
	// ErrTeamRosterNotFound hides cross-workspace Team identities.
	ErrTeamRosterNotFound = errors.New("team roster not found")
	// ErrTeamRosterWriteConflict indicates a stale Team CAS token.
	ErrTeamRosterWriteConflict = errors.New("team roster write conflict")
	// ErrTeamRosterIdempotencyConflict indicates reuse of a key for another command.
	ErrTeamRosterIdempotencyConflict = errors.New("team roster idempotency conflict")
	// ErrTeamRosterArchived rejects new commands against immutable archived Teams.
	ErrTeamRosterArchived = errors.New("team roster is archived")
	// ErrTeamRosterRemovalUnsupported keeps TeamWorker removal outside C-REVOKE.
	ErrTeamRosterRemovalUnsupported = errors.New("team worker removal requires a separate contract")
)

// TeamRosterCommand is the complete, idempotent roster control-plane command.
type TeamRosterCommand struct {
	WorkspaceID       string                  `json:"workspace_id"`
	TeamID            string                  `json:"team_id"`
	IdempotencyKey    string                  `json:"idempotency_key"`
	ExpectedUpdatedAt time.Time               `json:"expected_updated_at"`
	DesiredTeamStatus string                  `json:"desired_team_status"`
	LeadAgentID       string                  `json:"lead_agent_id"`
	Workers           []TeamRosterWorkerInput `json:"workers"`
	OperatorID        string                  `json:"operator_id"`
	Reason            string                  `json:"reason"`

	// allowRemove and allowArchived are internal baseline-only flags set by
	// the team-build rollback variant. They are not part of the canonical
	// command hash, so the frozen receipt identity stays the same for both
	// the normal and the baseline-only command surfaces.
	allowRemove   bool
	allowArchived bool
}

// TeamRosterAffectedWorker is the bounded write-response projection for one
// relationship whose authorization facts changed.
type TeamRosterAffectedWorker struct {
	WorkerAgentID                 string `json:"worker_agent_id"`
	AffectedPublishedVersionCount int    `json:"affected_published_version_count"`
	RevocationImpactURL           string `json:"revocation_impact_url"`
}

// TeamRosterResult is the frozen schema-one response persisted in a receipt.
type TeamRosterResult struct {
	SchemaVersion   int                        `json:"schema_version"`
	TeamID          string                     `json:"team_id"`
	TeamStatus      string                     `json:"team_status"`
	LeadAgentID     string                     `json:"lead_agent_id"`
	UpdatedAt       string                     `json:"updated_at"`
	Workers         []TeamRosterWorkerInput    `json:"workers"`
	Changed         bool                       `json:"changed"`
	AuditID         *string                    `json:"audit_id"`
	AffectedWorkers []TeamRosterAffectedWorker `json:"affected_workers"`
}

// TeamRosterAuthorizationFact is the non-sensitive relationship projection
// retained by command audit.
type TeamRosterAuthorizationFact struct {
	WorkerAgentID string   `json:"worker_agent_id"`
	AllowedKinds  []string `json:"allowed_kinds"`
	DefaultKind   string   `json:"default_kind"`
	Enabled       bool     `json:"enabled"`
}

// TeamRosterAudit is one append-only command-level roster transition.
type TeamRosterAudit struct {
	WorkspaceID    string                        `json:"workspace_id"`
	TeamID         string                        `json:"team_id"`
	AuditID        string                        `json:"audit_id"`
	IdempotencyKey string                        `json:"idempotency_key"`
	OldTeamStatus  string                        `json:"old_team_status"`
	NewTeamStatus  string                        `json:"new_team_status"`
	OldLeadAgentID *string                       `json:"old_lead_agent_id"`
	NewLeadAgentID string                        `json:"new_lead_agent_id"`
	OldWorkers     []TeamRosterAuthorizationFact `json:"old_workers"`
	NewWorkers     []TeamRosterAuthorizationFact `json:"new_workers"`
	OperatorID     string                        `json:"operator_id"`
	Reason         string                        `json:"reason"`
	CreatedAt      time.Time                     `json:"created_at"`
}
