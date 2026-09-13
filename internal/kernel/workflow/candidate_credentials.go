package workflow

import (
	"context"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// CandidateCredentialScope is derived from the authoritative product draft and
// selected exact agent versions; it is never accepted from a request payload.
type CandidateCredentialScope struct {
	WorkspaceID      string
	TeamID           string
	Lead             machine.AgentVersionKey
	Agents           []machine.AgentVersionKey
	DeliveryTargetID string
}

type CandidateCredentialAuthority interface {
	AuthorizeCandidateCredential(context.Context, CandidateCredentialScope, frozen.CredentialReference) error
}
