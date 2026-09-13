// Package runtimeagent decodes the immutable execution definition at the Host
// boundary. It contains no registry storage access.
package runtimeagent

import (
	"encoding/json"
	"errors"

	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

func Decode(claim runtimeprotocol.ExecutionClaim) (*registry.AgentRecord, error) {
	if err := claim.Validate(); err != nil {
		return nil, err
	}
	var record registry.AgentRecord
	if err := json.Unmarshal(claim.Request.FrozenAgent, &record); err != nil {
		return nil, errors.New("runtime frozen agent is invalid")
	}
	if record.WorkspaceID != claim.WorkspaceID || record.ID != claim.Agent.ID || record.Version != claim.Agent.Version || record.Name != claim.Agent.Name {
		return nil, errors.New("runtime frozen agent identity mismatch")
	}
	return &record, nil
}
