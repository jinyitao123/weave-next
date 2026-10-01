package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	AuthorizationExpiredBeforeDispatch = "authorization_expired"
	AuthorizationDeniedBeforeDispatch  = "authorization_denied"
)

// AuthorizationRefusal is system evidence that one authorization generation
// was refused before the tool could have any external effect. It is not an
// inference from a tool result, model text or an error message.
type AuthorizationRefusal struct {
	Code            string `json:"code"`
	ReasonCode      string `json:"reason_code,omitempty"`
	InputRevisionID string `json:"input_revision_id"`
	Generation      int64  `json:"generation"`
	Legacy          bool   `json:"legacy,omitempty"`
	NoEffect        bool   `json:"no_effect"`
}

func (proof AuthorizationRefusal) Valid() bool {
	if !proof.NoEffect || proof.InputRevisionID == "" || len(proof.InputRevisionID) > 128 || strings.TrimSpace(proof.InputRevisionID) != proof.InputRevisionID {
		return false
	}
	switch proof.Code {
	case AuthorizationExpiredBeforeDispatch:
		return proof.ReasonCode == "" && ((proof.Generation > 0 && !proof.Legacy) || (proof.Generation == 0 && proof.Legacy))
	case AuthorizationDeniedBeforeDispatch:
		return !proof.Legacy && proof.Generation > 0 && validAuthorizationReasonCode(proof.ReasonCode)
	default:
		return false
	}
}

// Renewable is true only for expiry refusals that may continue after the
// original employee renews authorization for the same frozen input.
func (proof AuthorizationRefusal) Renewable() bool {
	return proof.Code == AuthorizationExpiredBeforeDispatch && proof.Valid()
}

func validAuthorizationReasonCode(code string) bool {
	if code == "" || len(code) > 128 {
		return false
	}
	for _, r := range code {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

type authorizationRefusalError struct {
	proof AuthorizationRefusal
	cause error
}

func (err *authorizationRefusalError) Error() string {
	return fmt.Sprintf("%s: %v", err.proof.Code, err.cause)
}
func (err *authorizationRefusalError) Unwrap() error { return err.cause }

// NewAuthorizationRefusal may be called only by the trusted pre-dispatch
// authority check. An expired response after a send must never use this proof.
func NewAuthorizationRefusal(inputRevisionID string, generation int64, cause error) error {
	proof := AuthorizationRefusal{Code: AuthorizationExpiredBeforeDispatch, InputRevisionID: inputRevisionID, Generation: generation, NoEffect: true}
	if cause == nil || !proof.Valid() {
		return errors.New("invalid pre-dispatch authorization refusal binding")
	}
	return &authorizationRefusalError{proof: proof, cause: cause}
}

// NewAuthorizationDenialBeforeDispatch records a trusted, non-renewable
// authorization denial that the authority confirmed had no external effect.
// The caller must have validated the authority's exact response code and phase.
func NewAuthorizationDenialBeforeDispatch(inputRevisionID string, generation int64, reasonCode string, cause error) error {
	proof := AuthorizationRefusal{Code: AuthorizationDeniedBeforeDispatch, ReasonCode: reasonCode,
		InputRevisionID: inputRevisionID, Generation: generation, NoEffect: true}
	if cause == nil || !proof.Valid() {
		return errors.New("invalid pre-dispatch authorization denial binding")
	}
	return &authorizationRefusalError{proof: proof, cause: cause}
}

func AuthorizationRefusalFromError(err error) (AuthorizationRefusal, bool) {
	var refusal *authorizationRefusalError
	if errors.As(err, &refusal) && refusal.proof.Valid() {
		return refusal.proof, true
	}
	return AuthorizationRefusal{}, false
}

type AuthorizationRetryAuthorizer func(context.Context, AuthorizationRefusal) (bool, error)
type authorizationRetryContextKey struct{}

func WithAuthorizationRetryAuthorizer(ctx context.Context, check AuthorizationRetryAuthorizer) context.Context {
	return context.WithValue(ctx, authorizationRetryContextKey{}, check)
}
func CanRetryAuthorization(ctx context.Context, proof AuthorizationRefusal) (bool, error) {
	check, _ := ctx.Value(authorizationRetryContextKey{}).(AuthorizationRetryAuthorizer)
	if check == nil || !proof.Renewable() {
		return false, nil
	}
	return check(ctx, proof)
}

func NewLegacyAuthorizationRefusal(inputRevisionID string, cause error) error {
	proof := AuthorizationRefusal{Code: AuthorizationExpiredBeforeDispatch, InputRevisionID: inputRevisionID, NoEffect: true, Legacy: true}
	if cause == nil || !proof.Valid() {
		return errors.New("invalid legacy authorization refusal binding")
	}
	return &authorizationRefusalError{proof: proof, cause: cause}
}

func AuthorizationRefusalError(proof AuthorizationRefusal, cause error) error {
	if cause == nil || !proof.Valid() {
		return errors.New("invalid authorization refusal proof")
	}
	return &authorizationRefusalError{proof: proof, cause: cause}
}
