package frozen

import (
	"context"
	"errors"

	"github.com/jinyitao123/weave/internal/base/execution"
)

var ErrCredentialSubjectDenied = errors.New("credential subject is not authorized")

// ServiceReferenceAuthorization is supplied only by the server after checking
// the user's grant for this published reference. Host identity and body fields
// cannot supply it. Implementations may recheck revocation on every resolution.
type ServiceReferenceAuthorization func(context.Context, execution.Subject, CredentialReference) error

type serviceReferenceAuthorizationKey struct{}

func WithServiceReferenceAuthorization(ctx context.Context, authorize ServiceReferenceAuthorization) context.Context {
	return context.WithValue(ctx, serviceReferenceAuthorizationKey{}, authorize)
}

func HasServiceReferenceAuthorization(ctx context.Context) bool {
	fn, _ := ctx.Value(serviceReferenceAuthorizationKey{}).(ServiceReferenceAuthorization)
	return fn != nil
}

// ServiceReferenceAuthorizationFromContext returns the server-installed
// authorization hook so trusted layers can compose a narrower policy around
// one frozen execution without replacing the existing owner check.
func ServiceReferenceAuthorizationFromContext(ctx context.Context) ServiceReferenceAuthorization {
	fn, _ := ctx.Value(serviceReferenceAuthorizationKey{}).(ServiceReferenceAuthorization)
	return fn
}

func AuthorizeCredentialReference(ctx context.Context, ref CredentialReference) error {
	if ValidateCredentialReference(ref) != nil {
		return ErrCredentialSubjectDenied
	}
	subject, err := execution.RequireSubject(ctx, ref.WorkspaceID)
	if err != nil {
		return ErrCredentialSubjectDenied
	}
	if ref.Scope == CredentialScopeUser {
		if subject.UserID != ref.UserID || subject.ServiceID != "" {
			return ErrCredentialSubjectDenied
		}
		return nil
	}
	if subject.ServiceID != "" {
		if ref.Scope == CredentialScopeWorkspaceService && subject.ServiceID == ref.ServiceID {
			return nil
		}
		fn := ServiceReferenceAuthorizationFromContext(ctx)
		if fn == nil || fn(ctx, subject, ref) != nil {
			return ErrCredentialSubjectDenied
		}
		return nil
	}
	if ref.Scope == CredentialScopeUser {
		if subject.UserID != ref.UserID {
			return ErrCredentialSubjectDenied
		}
		return nil
	}
	fn := ServiceReferenceAuthorizationFromContext(ctx)
	if fn == nil || fn(ctx, subject, ref) != nil {
		return ErrCredentialSubjectDenied
	}
	return nil
}
