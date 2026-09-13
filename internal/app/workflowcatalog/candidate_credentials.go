package workflowcatalog

import (
	"context"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	workflowdef "github.com/jinyitao123/weave/internal/kernel/workflow"
)

// WithCredentialAuthority returns an independently configured compiler adapter;
// it never mutates a builder already shared by other requests.
func (b *CandidateBuilder) WithCredentialAuthority(authority workflowdef.CandidateCredentialAuthority) *CandidateBuilder {
	if b == nil {
		return nil
	}
	copy := *b
	copy.credentialAuthority = authority
	return &copy
}

func (b *CandidateBuilder) credentialContext(ctx context.Context, scope workflowdef.CandidateCredentialScope) context.Context {
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
