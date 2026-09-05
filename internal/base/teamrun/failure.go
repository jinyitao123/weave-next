package teamrun

import (
	"context"
	"errors"
	"strings"
)

// FailureClass separates work-product, verification, and infrastructure
// failures so product clients can offer recovery only when replay is safe.
type FailureClass string

const (
	FailureClassWork           FailureClass = "work"
	FailureClassVerification   FailureClass = "verification"
	FailureClassInfrastructure FailureClass = "infrastructure"
	FailureClassCancelled      FailureClass = "cancelled"
)

type FailureSummary struct {
	Class     FailureClass
	Retryable bool
	Reason    string
}

// ClassifyFailure returns a stable user-facing class without exposing the
// engine's raw diagnostic text.
func ClassifyFailure(err error) FailureSummary {
	if err == nil {
		return FailureSummary{}
	}
	if errors.Is(err, context.Canceled) || executionErrorCode(err) == ErrorCodeCancelled {
		return FailureSummary{Class: FailureClassCancelled, Reason: "execution was stopped"}
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "delivery_artifact_uncollected") {
		reason := "the referenced result file was not saved as a deliverable"
		for _, match := range [][2]string{
			{"file_exceeds_256_kib", "the result file exceeds the 256 KiB delivery limit"},
			{"files_exceed_512_kib_total", "the result files exceed the 512 KiB total delivery limit"},
			{"file_not_written_by_this_invocation", "the referenced result file was not produced by this execution"},
			{"unsupported_file_type", "the result file type is not supported for delivery"},
		} {
			if strings.Contains(message, match[0]) {
				reason = match[1]
				break
			}
		}
		return FailureSummary{Class: FailureClassVerification, Reason: reason}
	}
	code := executionErrorCode(err)
	switch code {
	case ErrorCodeOutputInvalid:
		return FailureSummary{Class: FailureClassWork, Reason: "the stage did not produce a usable work result"}
	case ErrorCodeNodeOutputInvalid:
		return FailureSummary{Class: FailureClassVerification, Reason: "the stage result did not satisfy its declared output requirements"}
	case ErrorCodeDeliveryUnavailable:
		return FailureSummary{Class: FailureClassVerification, Reason: "the stage result could not be preserved as a verified deliverable"}
	}
	for _, marker := range []string{
		"timed out", "timeout", "deadline exceeded", "reconnecting", "connection reset",
		"connection refused", "broken pipe", "unexpected eof", "stream disconnected",
		"service unavailable", "temporarily unavailable", "too many requests", "rate limit",
		"status 502", "status 503", "status 504", "runtime offline", "运行时离线",
		"runtime_pool_exhausted", "runtime_pinned_unavailable", "lease lost", "failed to start",
	} {
		if strings.Contains(message, marker) {
			return FailureSummary{Class: FailureClassInfrastructure, Retryable: true,
				Reason: "the runtime connection or execution environment failed before the stage could finish"}
		}
	}
	return FailureSummary{Class: FailureClassWork, Reason: "the stage could not complete its assigned work"}
}
