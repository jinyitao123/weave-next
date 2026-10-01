package execution

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestAuthorizationRefusalRequiresTypedBoundNoEffectEvidence(t *testing.T) {
	cause := errors.New("authorization lapsed")
	proof, found := AuthorizationRefusalFromError(fmt.Errorf("wrapped: %w", NewAuthorizationRefusal("input-revision", 3, cause)))
	if !found || !proof.Valid() || proof.Generation != 3 || proof.InputRevisionID != "input-revision" || !proof.NoEffect {
		t.Fatalf("trusted refusal lost binding: %+v found=%v", proof, found)
	}
	if !errors.Is(NewAuthorizationRefusal("input-revision", 3, cause), cause) {
		t.Fatal("typed refusal lost original expiry cause")
	}
	for _, err := range []error{errors.New(AuthorizationExpiredBeforeDispatch), cause, NewAuthorizationRefusal("", 3, cause), NewAuthorizationRefusal("input-revision", 0, cause), NewAuthorizationRefusal("input-revision", 3, nil)} {
		if proof, found := AuthorizationRefusalFromError(err); found {
			t.Fatalf("unbound refusal became no-effect evidence: %+v", proof)
		}
	}
}

func TestAuthorizationDenialIsNoEffectEvidenceButCannotRenew(t *testing.T) {
	cause := errors.New("organization qualification is inactive")
	err := NewAuthorizationDenialBeforeDispatch("input-revision", 4, "FORGE_TASK_ORGANIZATION_FORBIDDEN", cause)
	proof, found := AuthorizationRefusalFromError(fmt.Errorf("wrapped: %w", err))
	if !found || !proof.Valid() || proof.Renewable() || proof.Code != AuthorizationDeniedBeforeDispatch ||
		proof.ReasonCode != "FORGE_TASK_ORGANIZATION_FORBIDDEN" || proof.Generation != 4 || !proof.NoEffect {
		t.Fatalf("trusted denial proof lost its binding: %+v found=%v", proof, found)
	}
	if !errors.Is(err, cause) {
		t.Fatal("typed denial lost original authority cause")
	}
	called := false
	ctx := WithAuthorizationRetryAuthorizer(context.Background(), func(context.Context, AuthorizationRefusal) (bool, error) {
		called = true
		return true, nil
	})
	if allowed, retryErr := CanRetryAuthorization(ctx, proof); retryErr != nil || allowed || called {
		t.Fatalf("non-renewable denial reached renewal authorizer: allowed=%v called=%v err=%v", allowed, called, retryErr)
	}
	replayed := AuthorizationRefusalError(proof, errors.New("authorization still denied"))
	replayedProof, replayedFound := AuthorizationRefusalFromError(replayed)
	if !replayedFound || replayedProof != proof || replayedProof.Renewable() {
		t.Fatalf("journal replay changed denial into a renewable proof: %+v found=%v", replayedProof, replayedFound)
	}
	for _, reason := range []string{"", "FORGE-TASK-ORGANIZATION-FORBIDDEN", "forge_task_organization_forbidden"} {
		denial := NewAuthorizationDenialBeforeDispatch("input-revision", 4, reason, cause)
		if invalid, found := AuthorizationRefusalFromError(denial); found {
			t.Fatalf("invalid native authorization reason became trusted: %q proof=%+v", reason, invalid)
		}
	}
}
