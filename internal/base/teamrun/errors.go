package teamrun

import "errors"

type ErrorCode string

const (
	ErrorCodeAdmissionDenied            ErrorCode = "team_run_admission_denied"
	ErrorCodeAdmissionUnavailable       ErrorCode = "team_run_admission_unavailable"
	ErrorCodeIdentityMismatch           ErrorCode = "team_run_identity_mismatch"
	ErrorCodeStateConflict              ErrorCode = "team_run_state_conflict"
	ErrorCodeResumeInvalid              ErrorCode = "team_run_resume_invalid"
	ErrorCodeResumeStale                ErrorCode = "team_run_resume_stale"
	ErrorCodeSnapshotUnavailable        ErrorCode = "team_run_snapshot_unavailable"
	ErrorCodeRuntimeIncompatible        ErrorCode = "team_run_runtime_incompatible"
	ErrorCodeUnexpectedInteractiveYield ErrorCode = "team_run_unexpected_interactive_yield"
	ErrorCodeOutputInvalid              ErrorCode = "team_run_output_invalid"
	ErrorCodeNodeOutputInvalid          ErrorCode = "team_run_node_output_invalid"
	ErrorCodeDeliveryUnavailable        ErrorCode = "team_run_delivery_unavailable"
	ErrorCodeCancelled                  ErrorCode = "team_run_cancelled"
	ErrorCodeExecutionUnrecoverable     ErrorCode = "team_run_execution_unrecoverable"
)

var (
	ErrTeamRunStateConflict          = errors.New(string(ErrorCodeStateConflict))
	ErrTeamRunIdentityMismatch       = errors.New(string(ErrorCodeIdentityMismatch))
	ErrTeamRunResumeInvalid          = errors.New(string(ErrorCodeResumeInvalid))
	ErrTeamRunResumeStale            = errors.New(string(ErrorCodeResumeStale))
	ErrTeamRunSnapshotUnavailable    = errors.New(string(ErrorCodeSnapshotUnavailable))
	ErrTeamRunCancelled              = errors.New(string(ErrorCodeCancelled))
	ErrTeamRunExecutionUnrecoverable = errors.New(string(ErrorCodeExecutionUnrecoverable))
	// ErrCandidateFanoutUsageUnsupported is retained for API compatibility.
	// Since T14B-2A revision it no longer blocks: a round-bound candidate
	// run reaching a parallel/fanout node executes normally and only its
	// measured serial/loop usage is accumulated, with the run result marked
	// usage_complete=false (unmeasured parallel legs).
	ErrCandidateFanoutUsageUnsupported = errors.New("candidate fanout usage unsupported")
	// ErrCandidateCLIUsageUnsupported is retained for API compatibility.
	// Since T14B-2A revision it no longer blocks: a round-bound candidate
	// run reaching a CLI runtime agent node executes normally and records
	// zero measured usage, with the run result marked usage_complete=false
	// (CLI node without usage receipt).
	ErrCandidateCLIUsageUnsupported = errors.New("candidate CLI runtime usage unsupported")
)

// Usage-incomplete annotation reasons. A round-bound candidate test run may
// execute parallel/fanout or CLI nodes without a usage receipt; the run
// result and EvaluationReport then carry usage_complete=false plus one of
// these reasons instead of fabricating zero usage for the unmeasured parts.
const (
	UsageIncompleteReasonParallelLegs = "unmeasured parallel legs"
	UsageIncompleteReasonCLINode      = "CLI node without usage receipt"
)

func ValidateErrorCode(code ErrorCode) bool {
	switch code {
	case ErrorCodeAdmissionDenied,
		ErrorCodeAdmissionUnavailable,
		ErrorCodeIdentityMismatch,
		ErrorCodeStateConflict,
		ErrorCodeResumeInvalid,
		ErrorCodeResumeStale,
		ErrorCodeSnapshotUnavailable,
		ErrorCodeRuntimeIncompatible,
		ErrorCodeUnexpectedInteractiveYield,
		ErrorCodeOutputInvalid,
		ErrorCodeNodeOutputInvalid,
		ErrorCodeDeliveryUnavailable,
		ErrorCodeCancelled,
		ErrorCodeExecutionUnrecoverable:
		return true
	default:
		return false
	}
}
