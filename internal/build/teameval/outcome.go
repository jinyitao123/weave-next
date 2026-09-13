package teameval

import (
	"sort"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

// FailureClass is the closed platform-owned outcome vocabulary. Model text
// and gate prose are never accepted as a failure class.
type FailureClass string

const (
	FailureClassPass                  FailureClass = "pass"
	FailureClassBlueprintValidation   FailureClass = "blueprint_validation_failure"
	FailureClassCompile               FailureClass = "compile_failure"
	FailureClassRuntimeInfrastructure FailureClass = "runtime_infrastructure_failure"
	FailureClassBusinessQuality       FailureClass = "business_quality_failure"
	FailureClassGovernance            FailureClass = "governance_failure"
	FailureClassBudgetExhausted       FailureClass = "budget_exhausted"
	FailureClassCancelled             FailureClass = "cancelled"
)

// RevisionAction is the only controller action a typed diagnosis may request.
type RevisionAction string

const (
	RevisionActionNone                   RevisionAction = "none"
	RevisionActionResumeSameRevision     RevisionAction = "resume_same_revision"
	RevisionActionBlockPlatformDiagnosis RevisionAction = "block_platform_diagnosis"
	RevisionActionRequestBlueprintPatch  RevisionAction = "request_blueprint_patch"
	RevisionActionBlockGovernance        RevisionAction = "block_governance"
	RevisionActionStopCancelled          RevisionAction = "stop_cancelled"
)

// OutcomeEvidence identifies the independently persisted run, snapshot,
// task, and deliverable facts used to decide whether an output failure is a
// business result. Merely having prose that mentions an artifact is not
// evidence.
type OutcomeEvidence struct {
	RunID          string   `json:"run_id,omitempty"`
	RunSnapshotID  string   `json:"run_snapshot_id,omitempty"`
	RunRef         string   `json:"run_ref,omitempty"`
	TaskRef        string   `json:"task_ref,omitempty"`
	DeliverableRef string   `json:"deliverable_ref,omitempty"`
	AdditionalRefs []string `json:"additional_refs,omitempty"`
}

// Complete reports whether all three persisted evidence layers are present
// and bound to explicit run and snapshot identities.
func (e OutcomeEvidence) Complete() bool {
	return strings.TrimSpace(e.RunID) != "" &&
		strings.TrimSpace(e.RunSnapshotID) != "" &&
		strings.TrimSpace(e.RunRef) != "" &&
		strings.TrimSpace(e.TaskRef) != "" &&
		strings.TrimSpace(e.DeliverableRef) != ""
}

func (e OutcomeEvidence) refs() []string {
	values := append([]string(nil), e.AdditionalRefs...)
	values = append(values, e.RunRef, e.TaskRef, e.DeliverableRef)
	seen := make(map[string]bool, len(values))
	refs := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		refs = append(refs, value)
	}
	sort.Strings(refs)
	return refs
}

// OutcomeInput is the deterministic input to DiagnoseOutcome. FailedGates
// must come from GateEvaluator; callers cannot provide free-form categories.
type OutcomeInput struct {
	TerminalStatus    string
	OriginalErrorCode string
	Evidence          OutcomeEvidence
	FailedGates       []GateResult
	FailedRubric      []string
	SevereDefects     []string
	Detail            string
}

// TypedDiagnosis is the controller-facing result. OriginalErrorCode remains
// unchanged so an evidence failure can never overwrite the runtime cause.
type TypedDiagnosis struct {
	Class             FailureClass   `json:"class"`
	OriginalErrorCode string         `json:"original_error_code,omitempty"`
	EvidenceRefs      []string       `json:"evidence_refs,omitempty"`
	GateCode          string         `json:"gate_code,omitempty"`
	FailureDetail     string         `json:"failure_detail,omitempty"`
	RevisionAction    RevisionAction `json:"revision_action"`
}

// DiagnoseOutcome maps trusted terminal and gate facts onto the closed
// failure vocabulary. Unknown error codes and incomplete evidence fail
// closed as platform diagnosis; they never become business revision.
func DiagnoseOutcome(input OutcomeInput) TypedDiagnosis {
	code := strings.TrimSpace(input.OriginalErrorCode)
	diagnosis := TypedDiagnosis{
		OriginalErrorCode: code,
		EvidenceRefs:      input.Evidence.refs(),
		FailureDetail:     strings.TrimSpace(input.Detail),
	}
	if strings.EqualFold(strings.TrimSpace(input.TerminalStatus), "cancelled") ||
		code == string(teamrun.ErrorCodeCancelled) {
		diagnosis.Class = FailureClassCancelled
		diagnosis.RevisionAction = RevisionActionStopCancelled
		return diagnosis
	}

	if code != "" {
		return diagnoseErrorCode(diagnosis, code, input.Evidence.Complete(), diagnosis.FailureDetail)
	}
	if len(input.SevereDefects) > 0 {
		diagnosis.Class = FailureClassBusinessQuality
		diagnosis.FailureDetail = "severe defect: " + strings.Join(input.SevereDefects, "; ")
		if input.Evidence.Complete() {
			diagnosis.RevisionAction = RevisionActionRequestBlueprintPatch
		} else {
			diagnosis.RevisionAction = RevisionActionBlockPlatformDiagnosis
		}
		return diagnosis
	}

	for _, gate := range input.FailedGates {
		if gate.Status != StatusFail {
			continue
		}
		diagnosis.GateCode = gate.Code
		if diagnosis.FailureDetail == "" {
			diagnosis.FailureDetail = gate.Detail
		}
		switch gate.Code {
		case GateNoGovernanceViolation, GateScopeCompliance:
			diagnosis.Class = FailureClassGovernance
			diagnosis.RevisionAction = RevisionActionBlockGovernance
		case GateRunTerminalConsistent:
			diagnosis.Class = FailureClassRuntimeInfrastructure
			diagnosis.RevisionAction = RevisionActionBlockPlatformDiagnosis
		case GateGraphSchema, GateDepsFreezable, GateDepsPinned, GateEngineGraphMatch:
			diagnosis.Class = FailureClassCompile
			diagnosis.RevisionAction = RevisionActionBlockPlatformDiagnosis
		default:
			if !input.Evidence.Complete() {
				diagnosis.Class = FailureClassRuntimeInfrastructure
				diagnosis.RevisionAction = RevisionActionBlockPlatformDiagnosis
			} else {
				diagnosis.Class = FailureClassBusinessQuality
				diagnosis.RevisionAction = RevisionActionRequestBlueprintPatch
			}
		}
		return diagnosis
	}

	if isSuccessfulTerminal(input.TerminalStatus) {
		if len(input.FailedRubric) > 0 {
			diagnosis.Class = FailureClassBusinessQuality
			diagnosis.RevisionAction = RevisionActionRequestBlueprintPatch
			diagnosis.FailureDetail = "rubric below threshold: " + strings.Join(input.FailedRubric, ", ")
			return diagnosis
		}
		diagnosis.Class = FailureClassPass
		diagnosis.RevisionAction = RevisionActionNone
		return diagnosis
	}
	// A non-success terminal without an exact error code is not a quality
	// signal. Keep the same revision and ask the platform to diagnose it.
	diagnosis.Class = FailureClassRuntimeInfrastructure
	diagnosis.RevisionAction = RevisionActionBlockPlatformDiagnosis
	return diagnosis
}

func diagnoseErrorCode(base TypedDiagnosis, code string, evidenceComplete bool, detail string) TypedDiagnosis {
	switch teamrun.ErrorCode(code) {
	case teamrun.ErrorCodeCancelled:
		base.Class = FailureClassCancelled
		base.RevisionAction = RevisionActionStopCancelled
	case teamrun.ErrorCodeAdmissionDenied:
		base.Class = FailureClassGovernance
		base.RevisionAction = RevisionActionBlockGovernance
	case teamrun.ErrorCodeAdmissionUnavailable,
		teamrun.ErrorCodeSnapshotUnavailable,
		teamrun.ErrorCodeStateConflict,
		teamrun.ErrorCodeResumeInvalid,
		teamrun.ErrorCodeResumeStale:
		base.Class = FailureClassRuntimeInfrastructure
		base.RevisionAction = RevisionActionResumeSameRevision
	case teamrun.ErrorCodeIdentityMismatch,
		teamrun.ErrorCodeRuntimeIncompatible,
		teamrun.ErrorCodeUnexpectedInteractiveYield,
		teamrun.ErrorCodeDeliveryUnavailable:
		base.Class = FailureClassRuntimeInfrastructure
		base.RevisionAction = RevisionActionBlockPlatformDiagnosis
	case teamrun.ErrorCodeExecutionUnrecoverable:
		base.Class = FailureClassRuntimeInfrastructure
		if isRetryableCandidateRuntimeDetail(detail) {
			base.RevisionAction = RevisionActionResumeSameRevision
		} else {
			base.RevisionAction = RevisionActionBlockPlatformDiagnosis
		}
	case teamrun.ErrorCodeOutputInvalid, teamrun.ErrorCodeNodeOutputInvalid:
		if evidenceComplete {
			base.Class = FailureClassBusinessQuality
			base.RevisionAction = RevisionActionRequestBlueprintPatch
		} else {
			base.Class = FailureClassRuntimeInfrastructure
			base.RevisionAction = RevisionActionBlockPlatformDiagnosis
		}
	default:
		base.Class = FailureClassRuntimeInfrastructure
		base.RevisionAction = RevisionActionBlockPlatformDiagnosis
	}
	return base
}

func isRetryableCandidateRuntimeDetail(detail string) bool {
	if isAgentNodeTimeoutDetail(detail) {
		return true
	}
	detail = strings.ToLower(strings.TrimSpace(detail))
	return strings.Contains(detail, "stream disconnected before completion") ||
		strings.Contains(detail, "idle timeout waiting for sse") ||
		strings.Contains(detail, "tls handshake eof") ||
		strings.Contains(detail, "error sending request for url") ||
		strings.Contains(detail, "connection reset") ||
		strings.Contains(detail, "unexpected eof") ||
		strings.Contains(detail, "unexpected status 502") ||
		strings.Contains(detail, "unexpected status 503") ||
		strings.Contains(detail, "unexpected status 504") ||
		strings.Contains(detail, "service temporarily unavailable") ||
		strings.Contains(detail, "员工所在运行时离线")
}

func isAgentNodeTimeoutDetail(detail string) bool {
	const marker = `" timed out after `
	detail = strings.TrimSpace(detail)
	prefix := string(teamrun.ErrorCodeExecutionUnrecoverable) + `: agent node "`
	if !strings.HasPrefix(detail, prefix) {
		return false
	}
	remainder := strings.TrimPrefix(detail, prefix)
	markerIndex := strings.Index(remainder, marker)
	if markerIndex <= 0 {
		return false
	}
	durationText := remainder[markerIndex+len(marker):]
	duration, err := time.ParseDuration(durationText)
	return err == nil && duration > 0
}

func isSuccessfulTerminal(status string) bool {
	status = strings.TrimSpace(status)
	return status == "success" || status == "succeeded"
}
