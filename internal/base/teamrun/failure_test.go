package teamrun

import (
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
