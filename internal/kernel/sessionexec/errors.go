package sessionexec

import (
	"errors"
	"fmt"

	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

var (
	ErrLeaseNotFound           = errors.New("session_lease_not_found")
	ErrLeaseBusy               = errors.New("session_lease_busy")
	ErrLeaseEpochFenced        = errors.New("session_lease_epoch_fenced")
	ErrLeaseRunMismatch        = errors.New("session_lease_run_mismatch")
	ErrLeaseStateConflict      = errors.New("session_lease_state_conflict")
	ErrYieldKindMismatch       = errors.New("session_yield_kind_mismatch")
	ErrResumeTokenInvalid      = errors.New("session_resume_token_invalid")
	ErrYieldGenerationConflict = errors.New("session_yield_generation_conflict")
	ErrResumeInputInvalid      = errors.New("session_resume_input_invalid")
	ErrOutboxConflict          = errors.New("session_outbox_conflict")
	ErrOutboxAlreadyCommitted  = errors.New("session_outbox_already_committed")
	ErrLeaseExpired            = errors.New("session_lease_expired")
)

// ResumeInputValidationError preserves field-level diagnostics while keeping
// errors.Is(err, ErrResumeInputInvalid) stable for transport adapters.
type ResumeInputValidationError struct {
	Problems []machine.RuntimeSchemaProblem
}

func (err *ResumeInputValidationError) Error() string {
	if err == nil || len(err.Problems) == 0 {
		return ErrResumeInputInvalid.Error()
	}
	first := err.Problems[0]
	return fmt.Sprintf("%s: %s: %s", ErrResumeInputInvalid, first.Path, first.Message)
}

func (*ResumeInputValidationError) Unwrap() error { return ErrResumeInputInvalid }
