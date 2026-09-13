package workflow

import (
	"context"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
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

// WithCredentialAuthority returns an independently configured compiler adapter;
// it never mutates a builder already shared by other requests.
func (b *CandidateBuilder) WithCredentialAuthority(authority CandidateCredentialAuthority) *CandidateBuilder {
	if b == nil {
		return nil
	}
	copy := *b
	copy.credentialAuthority = authority
	return &copy
}

func (b *CandidateBuilder) credentialContext(ctx context.Context, scope CandidateCredentialScope) context.Context {
	if b.credentialAuthority == nil {
		return ctx
	}
	owner, _ := execution.RequireSubject(ctx, scope.WorkspaceID)
	return credentials.WithServiceReferenceAuthorization(ctx, func(call context.Context, subject execution.Subject, ref frozen.CredentialReference) error {
		if subject != owner {
			return execution.ErrSubjectMismatch
		}
		return b.credentialAuthority.AuthorizeCandidateCredential(call, scope, ref)
	})
}
