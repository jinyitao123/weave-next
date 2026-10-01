package teamrun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

func TestClassifyFailureSeparatesRecoveryAuthority(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		class     FailureClass
		retryable bool
	}{
		{name: "runtime timeout", err: executionError(ErrorCodeExecutionUnrecoverable, errors.New("Reconnecting... 2/5 (request timed out)")), class: FailureClassInfrastructure, retryable: true},
		{name: "provider connection lost mid-response", err: executionError(ErrorCodeExecutionUnrecoverable, errors.New("API Error: Connection lost mid-response. The response above may be incomplete.")), class: FailureClassInfrastructure, retryable: true},
		{name: "invalid work", err: executionError(ErrorCodeOutputInvalid, errors.New("missing result")), class: FailureClassWork},
		{name: "schema verification", err: executionError(ErrorCodeNodeOutputInvalid, errors.New("schema mismatch")), class: FailureClassVerification},
		{name: "unsupported runtime model", err: errors.New(`unexpected status 404 Not Found: Model "gpt-6-astra" is not supported`), class: FailureClassInfrastructure},
		{name: "preflight before transient wrapper", err: errors.New("failed to start: runtime_credentials_missing: ONEAPI_API_KEY"), class: FailureClassInfrastructure},
		{name: "unsupported before reconnect wrapper", err: errors.New(`Reconnecting: Model "missing" is not supported`), class: FailureClassInfrastructure},
		{name: "missing runtime credential", err: errors.New("Missing environment variable: `ONEAPI_API_KEY`"), class: FailureClassInfrastructure},
		{name: "native process interruption", err: errors.New("runtime_process_interrupted: signal killed"), class: FailureClassInfrastructure, retryable: true},
		{name: "signal words without native proof", err: errors.New("signal: killed"), class: FailureClassWork},
		{name: "explicit stop wins over signal", err: errors.Join(context.Canceled, errors.New("runtime_process_interrupted: signal terminated")), class: FailureClassCancelled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ClassifyFailure(test.err)
			if got.Class != test.class || got.Retryable != test.retryable || got.Reason == "" {
				t.Fatalf("ClassifyFailure() = %#v, want class=%q retryable=%v", got, test.class, test.retryable)
			}
		})
	}
}

func TestClassifyFailureNamesAnExpiredDelegationAndDoesNotOfferRetry(t *testing.T) {
	for name, err := range map[string]error{
		"member build":  fmt.Errorf("%w: %w", mcphost.ErrFailClosed, businessaction.ErrDelegationExpired),
		"per call":      fmt.Errorf("business delegation no longer authorizes this action: %w", businessaction.ErrDelegationExpired),
		"wrapped again": fmt.Errorf("member stage failed: %w", fmt.Errorf("tool: %w", businessaction.ErrDelegationExpired)),
	} {
		summary := ClassifyFailure(err)
		if summary.Class != FailureClassInfrastructure || summary.Retryable || !strings.Contains(summary.Reason, "authorization") ||
			!strings.Contains(summary.Reason, "resubmit") {
			t.Errorf("%s: summary=%+v, want a non-retryable infrastructure failure telling the employee to resubmit", name, summary)
		}
	}
	if summary := ClassifyFailure(errors.New("business delegation no longer authorizes this action")); strings.Contains(summary.Reason, "expired") {
		t.Errorf("a scope error was reported as an expiry: %+v", summary)
	}
}

func TestClassifyFailureKeepsOrganizationDenialNonRenewable(t *testing.T) {
	err := execution.NewAuthorizationDenialBeforeDispatch("input", 2, "FORGE_TASK_ORGANIZATION_FORBIDDEN", errors.New("Forge refused current task"))
	proof, found := execution.AuthorizationRefusalFromError(err)
	if !found || proof.Renewable() {
		t.Fatalf("fixture did not create a non-renewable no-effect refusal: %+v found=%v", proof, found)
	}
	summary := ClassifyFailure(err)
	if summary.Class != FailureClassInfrastructure || summary.Retryable || summary.AuthorizationRequired != nil || !summary.AuthorizationDenied || !strings.Contains(summary.Reason, "Forge authorization") {
		t.Fatalf("organization denial was offered renewal or retry: %+v", summary)
	}
}
