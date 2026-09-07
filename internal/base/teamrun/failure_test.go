package teamrun

import (
	"context"
	"errors"
	"testing"
)

func TestClassifyFailureSeparatesRecoveryAuthority(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		class     FailureClass
		retryable bool
	}{
		{name: "runtime timeout", err: executionError(ErrorCodeExecutionUnrecoverable, errors.New("Reconnecting... 2/5 (request timed out)")), class: FailureClassInfrastructure, retryable: true},
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
