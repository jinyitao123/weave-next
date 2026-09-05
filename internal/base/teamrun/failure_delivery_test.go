package teamrun

import (
	"errors"
	"strings"
	"testing"
)

func TestDeliveryCaptureFailureRequiresProductRepairWithoutRuntimeRetry(t *testing.T) {
	for marker, expected := range map[string]string{
		"file_exceeds_256_kib": "256 KiB", "files_exceed_512_kib_total": "512 KiB",
		"file_not_written_by_this_invocation": "not produced by this execution", "unsupported_file_type": "not supported",
		"file_read_failed": "not saved",
	} {
		t.Run(marker, func(t *testing.T) {
			failure := ClassifyFailure(errors.New("delivery_artifact_uncollected: " + marker + " /private/customer/connection reset.txt"))
			if failure.Class != FailureClassVerification || failure.Retryable || !strings.Contains(failure.Reason, expected) || strings.Contains(failure.Reason, "/private/") {
				t.Fatalf("delivery gap became retryable or leaked path: %+v", failure)
			}
		})
	}
}
