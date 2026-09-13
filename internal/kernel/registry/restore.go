package registry

import (
	"errors"
)

var (
	// ErrRestoreVersionHashMismatch reports that the baseline-pinned agent
	// version's stored spec does not hash to the frozen pin content hash.
	ErrRestoreVersionHashMismatch = errors.New("agent restore version content hash mismatch")
	// ErrRestoreVersionIdentityMismatch reports that a baseline-pinned agent
	// version's frozen identity does not match its version key or the current
	// head's stable identity.
	ErrRestoreVersionIdentityMismatch = errors.New("agent restore version identity mismatch")
)

// AgentVersionRestoreResult is one agent restore outcome. Restored is false
// when the head already carried the exact baseline content (idempotent
// replay), in which case Version is the unchanged current head version.
type AgentVersionRestoreResult struct {
	AgentID  string `json:"agent_id"`
	Name     string `json:"name"`
	Version  int    `json:"version"`
	Restored bool   `json:"restored"`
}
