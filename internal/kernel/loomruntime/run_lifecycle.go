package loomruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
)

type RunLifecycleQuery struct {
	WorkspaceID   string
	RunSnapshotID string
}

// RunLifecycleAgentQuery selects every admitted run attributed to one agent.
type RunLifecycleAgentQuery struct {
	WorkspaceID string
	Agent       string
}

type RunLifecycleClassification string
type RunLifecycleDiagnosticCode string
type RunLifecycleDiagnosticFact string

const (
	RunLifecycleTerminalComplete          RunLifecycleClassification = "terminal_complete"
	RunLifecycleTerminalStageYielded      RunLifecycleClassification = "terminal_stage_yielded"
	RunLifecycleTerminalPendingActive     RunLifecycleClassification = "terminal_pending_active"
	RunLifecycleTerminalReconciling       RunLifecycleClassification = "terminal_reconciling"
	RunLifecycleTerminalMissing           RunLifecycleClassification = "terminal_missing"
	RunLifecycleTerminalProjectionBlocked RunLifecycleClassification = "terminal_projection_blocked"
	RunLifecycleTerminalAssociationDefect RunLifecycleClassification = "terminal_association_defect"
	RunLifecycleTerminalLineageDefect     RunLifecycleClassification = "terminal_lineage_defect"
	RunLifecycleTerminalLivenessUnknown   RunLifecycleClassification = "terminal_liveness_unknown"
)

const (
	RunLifecycleDiagnosticPreAttribution      RunLifecycleDiagnosticCode = "terminal_pre_attribution"
	RunLifecycleDiagnosticRegistrationCorrupt RunLifecycleDiagnosticCode = "terminal_registration_corruption"
	RunLifecycleDiagnosticTerminalCorrupt     RunLifecycleDiagnosticCode = "terminal_corrupt"
	RunLifecycleDiagnosticAssociationDefect   RunLifecycleDiagnosticCode = "terminal_association_defect"
	RunLifecycleDiagnosticLineageDefect       RunLifecycleDiagnosticCode = "terminal_lineage_defect"
	RunLifecycleDiagnosticTerminalMissing     RunLifecycleDiagnosticCode = "terminal_missing"
	RunLifecycleFactReceiptAbsent             RunLifecycleDiagnosticFact = "receipt_absent"
	RunLifecycleFactLegacyAudit               RunLifecycleDiagnosticFact = "audit_legacy"
	RunLifecycleFactReceiptCorrupt            RunLifecycleDiagnosticFact = "receipt_corrupt"
	RunLifecycleFactReceiptClaimMismatch      RunLifecycleDiagnosticFact = "receipt_claim_mismatch"
	RunLifecycleFactIndexMissing              RunLifecycleDiagnosticFact = "index_missing"
	RunLifecycleFactIndexMismatch             RunLifecycleDiagnosticFact = "index_mismatch"
	RunLifecycleFactLeaseMissing              RunLifecycleDiagnosticFact = "lease_missing"
	RunLifecycleFactLeaseCorrupt              RunLifecycleDiagnosticFact = "lease_corrupt"
	RunLifecycleFactMarkerCorrupt             RunLifecycleDiagnosticFact = "marker_corrupt"
	RunLifecycleFactAuditCorrupt              RunLifecycleDiagnosticFact = "audit_corrupt"
	RunLifecycleFactAssociationMismatch       RunLifecycleDiagnosticFact = "association_mismatch"
	RunLifecycleFactUsageMarkerMismatch       RunLifecycleDiagnosticFact = "usage_marker_mismatch"
	RunLifecycleFactLineagePending            RunLifecycleDiagnosticFact = "lineage_pending"
	RunLifecycleFactLineageFailed             RunLifecycleDiagnosticFact = "lineage_failed"
	RunLifecycleFactMarkerMissing             RunLifecycleDiagnosticFact = "marker_missing"
	RunLifecycleFactAuditMissing              RunLifecycleDiagnosticFact = "audit_missing"
	RunLifecycleFactExpiredUnclaimed          RunLifecycleDiagnosticFact = "expired_unclaimed"
	RunLifecycleFactMarkerMissingValidAudit   RunLifecycleDiagnosticFact = "marker_missing_valid_audit"
)

type RunLifecycleDiagnostic struct {
	Code RunLifecycleDiagnosticCode
	Fact RunLifecycleDiagnosticFact
}

type RunLifecycleItem struct {
	Expected           ExpectedRunRecordV1
	Classification     RunLifecycleClassification
	Diagnostics        []RunLifecycleDiagnostic
	AttemptGeneration  *int64
	AttemptID          *uuid.UUID
	MarkerSource       *TerminalMarkerSource
	MarkerStatus       *TerminalMarkerStatus
	EvidenceKind       *TerminalMarkerEvidenceKind
	CheckpointSeq      *int64
	RetryCount         *int64
	MarkerAuditState   *TerminalMarkerAuditState
	MarkerLineageState *TerminalMarkerLineageState
	LastErrorCode      *string
	// RunStartedAt is the run start fact from the terminal marker when present,
	// otherwise from the attempt lease; nil when neither fact exists yet.
	RunStartedAt *string
}

type RunLifecycleResult struct {
	WorkspaceID   string
	RunSnapshotID string
	ObservedAt    time.Time
	Runs          []RunLifecycleItem
}

// RunLifecycleAgentResult is the per-agent counterpart of RunLifecycleResult;
// there is no snapshot selector because the expected set is filtered by the
// registry's agent attribution.
type RunLifecycleAgentResult struct {
	WorkspaceID string
	Agent       string
	ObservedAt  time.Time
	Runs        []RunLifecycleItem
}

type RunLifecycleReader interface {
	ReadBySnapshot(context.Context, RunLifecycleQuery) (RunLifecycleResult, error)
}

// AgentRunLifecycleReader enumerates one agent's admitted runs with the same
// nine-state classification as the snapshot reader. Attribution reuses the
// expected-run registry's agent field; no new attribution logic is added.
type AgentRunLifecycleReader interface {
	ReadByAgent(context.Context, RunLifecycleAgentQuery) (RunLifecycleAgentResult, error)
}

type RunLifecycleReadStage string

const (
	RunLifecycleReadValidate RunLifecycleReadStage = "validate"
	RunLifecycleReadBegin    RunLifecycleReadStage = "begin"
	RunLifecycleReadObserve  RunLifecycleReadStage = "observe"
	RunLifecycleReadClaims   RunLifecycleReadStage = "read_claims"
	RunLifecycleReadIndexes  RunLifecycleReadStage = "read_indexes"
	RunLifecycleReadFacts    RunLifecycleReadStage = "read_facts"
	RunLifecycleReadClassify RunLifecycleReadStage = "classify"
	RunLifecycleReadCommit   RunLifecycleReadStage = "commit"
)

var (
	ErrRunLifecycleInvalidQuery       = errors.New("run lifecycle invalid query")
	ErrRunLifecycleExpectedSetCorrupt = errors.New("run lifecycle expected set corrupt")
	ErrRunLifecycleStoreUnavailable   = errors.New("run lifecycle store unavailable")
)

type RunLifecycleReadError struct {
	Stage         RunLifecycleReadStage
	WorkspaceID   string
	RunSnapshotID string
	Agent         string
	RunID         string
	Retryable     bool
	Err           error
}

func (err *RunLifecycleReadError) Error() string {
	if err == nil {
		return "run lifecycle read failed"
	}
	message := fmt.Sprintf(
		"run lifecycle read failed at %s for workspace %q",
		err.Stage,
		err.WorkspaceID,
	)
	if err.Agent != "" {
		message += fmt.Sprintf(" agent %q", err.Agent)
	} else {
		message += fmt.Sprintf(" snapshot %q", err.RunSnapshotID)
	}
	if err.RunID != "" {
		message += fmt.Sprintf(" run %q", err.RunID)
	}
	if err.Err != nil {
		message += ": " + err.Err.Error()
	}
	return message
}

func (err *RunLifecycleReadError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

type RunLifecycleObserver interface {
	ObserveRunLifecycle(context.Context, RunLifecycleObservation)
}

type RunLifecycleObservation struct {
	WorkspaceID        string
	RunSnapshotID      string
	RunID              string
	ObservedAt         time.Time
	Classification     RunLifecycleClassification
	DiagnosticCodes    []RunLifecycleDiagnosticCode
	AttemptGeneration  *int64
	MarkerSource       *TerminalMarkerSource
	MarkerStatus       *TerminalMarkerStatus
	CheckpointSeq      *int64
	RetryCount         *int64
	MarkerAuditState   *TerminalMarkerAuditState
	MarkerLineageState *TerminalMarkerLineageState
	LastErrorCode      *string
}

type runLifecycleReceiptState uint8

const (
	runLifecycleReceiptAbsent runLifecycleReceiptState = iota
	runLifecycleReceiptValid
	runLifecycleReceiptCorrupt
	runLifecycleReceiptClaimMismatch
)

type runLifecycleIndexState uint8

const (
	runLifecycleIndexValid runLifecycleIndexState = iota
	runLifecycleIndexMissing
	runLifecycleIndexMismatch
)

type runLifecycleFacts struct {
	expected      ExpectedRunRecordV1
	claimBytes    []byte
	indexState    runLifecycleIndexState
	receiptState  runLifecycleReceiptState
	lease         *RunAttemptLease
	leaseCorrupt  bool
	marker        *TerminalMarkerV1
	markerCorrupt bool
	audit         TerminalRecordInspection
}

func classifyRunLifecycle(
	facts runLifecycleFacts,
	observedAt time.Time,
) RunLifecycleItem {
	item := RunLifecycleItem{
		Expected:    facts.expected,
		Diagnostics: make([]RunLifecycleDiagnostic, 0),
	}
	appendDiagnostic := func(
		code RunLifecycleDiagnosticCode,
		fact RunLifecycleDiagnosticFact,
	) {
		item.Diagnostics = append(
			item.Diagnostics,
			RunLifecycleDiagnostic{Code: code, Fact: fact},
		)
	}

	switch facts.indexState {
	case runLifecycleIndexMissing:
		appendDiagnostic(
			RunLifecycleDiagnosticRegistrationCorrupt,
			RunLifecycleFactIndexMissing,
		)
	case runLifecycleIndexMismatch:
		appendDiagnostic(
			RunLifecycleDiagnosticRegistrationCorrupt,
			RunLifecycleFactIndexMismatch,
		)
	}
	switch facts.receiptState {
	case runLifecycleReceiptAbsent:
		appendDiagnostic(
			RunLifecycleDiagnosticPreAttribution,
			RunLifecycleFactReceiptAbsent,
		)
	case runLifecycleReceiptCorrupt:
		appendDiagnostic(
			RunLifecycleDiagnosticRegistrationCorrupt,
			RunLifecycleFactReceiptCorrupt,
		)
	case runLifecycleReceiptClaimMismatch:
		appendDiagnostic(
			RunLifecycleDiagnosticRegistrationCorrupt,
			RunLifecycleFactReceiptClaimMismatch,
		)
	}
	if facts.receiptState == runLifecycleReceiptValid {
		switch {
		case facts.leaseCorrupt:
			appendDiagnostic(
				RunLifecycleDiagnosticRegistrationCorrupt,
				RunLifecycleFactLeaseCorrupt,
			)
		case facts.lease == nil:
			appendDiagnostic(
				RunLifecycleDiagnosticRegistrationCorrupt,
				RunLifecycleFactLeaseMissing,
			)
		}
	}

	if facts.lease != nil {
		generation := facts.lease.AttemptGeneration
		attemptID := facts.lease.AttemptID
		retryCount := facts.lease.RetryCount
		item.AttemptGeneration = &generation
		item.AttemptID = &attemptID
		item.RetryCount = &retryCount
		startedAt := facts.lease.RunStartedAt
		item.RunStartedAt = &startedAt
	}
	if facts.marker != nil {
		if item.AttemptGeneration == nil {
			generation := facts.marker.AttemptGeneration
			attemptID := facts.marker.AttemptID
			item.AttemptGeneration = &generation
			item.AttemptID = &attemptID
		}
		source := facts.marker.Source
		status := facts.marker.Status
		evidenceKind := facts.marker.EvidenceKind
		auditState := facts.marker.AuditState
		lineageState := facts.marker.LineageState
		item.MarkerSource = &source
		item.MarkerStatus = &status
		item.EvidenceKind = &evidenceKind
		item.CheckpointSeq = cloneLifecycleInt64(facts.marker.CheckpointSeq)
		item.MarkerAuditState = &auditState
		item.MarkerLineageState = &lineageState
		startedAt := facts.marker.RunStartedAt
		item.RunStartedAt = &startedAt
	}

	associationDefect := false
	if facts.markerCorrupt {
		appendDiagnostic(
			RunLifecycleDiagnosticTerminalCorrupt,
			RunLifecycleFactMarkerCorrupt,
		)
		associationDefect = true
	}
	switch facts.audit.Classification {
	case TerminalRecordCorrupt:
		appendDiagnostic(
			RunLifecycleDiagnosticTerminalCorrupt,
			RunLifecycleFactAuditCorrupt,
		)
		associationDefect = true
	case TerminalRecordAssociationDefect:
		appendDiagnostic(
			RunLifecycleDiagnosticAssociationDefect,
			RunLifecycleFactAssociationMismatch,
		)
		associationDefect = true
	case TerminalRecordPreAttribution:
		appendDiagnostic(
			RunLifecycleDiagnosticPreAttribution,
			RunLifecycleFactLegacyAudit,
		)
	}

	if facts.marker != nil {
		if err := compareExpectedRunIdentity(
			expectedRunRecordFromTerminalMarker(*facts.marker),
			facts.expected,
		); err != nil {
			appendDiagnostic(
				RunLifecycleDiagnosticAssociationDefect,
				RunLifecycleFactAssociationMismatch,
			)
			associationDefect = true
		}
	}
	if facts.audit.Classification == TerminalRecordValid &&
		facts.audit.Entry != nil {
		if err := compareExpectedRunIdentity(
			expectedRunRecordFromNormalTerminal(*facts.audit.Entry),
			facts.expected,
		); err != nil {
			appendDiagnostic(
				RunLifecycleDiagnosticAssociationDefect,
				RunLifecycleFactAssociationMismatch,
			)
			associationDefect = true
		}
	}
	if facts.marker != nil &&
		facts.audit.Classification == TerminalRecordValid &&
		facts.audit.Entry != nil &&
		!lifecycleMarkerMatchesAudit(*facts.marker, *facts.audit.Entry) {
		appendDiagnostic(
			RunLifecycleDiagnosticAssociationDefect,
			RunLifecycleFactUsageMarkerMismatch,
		)
		associationDefect = true
	}

	indexOrLeaseDefect := facts.indexState != runLifecycleIndexValid ||
		(facts.receiptState == runLifecycleReceiptValid &&
			(facts.lease == nil || facts.leaseCorrupt))
	validAudit := facts.audit.Classification == TerminalRecordValid &&
		facts.audit.Entry != nil
	validFinal := facts.marker != nil &&
		facts.marker.Phase == TerminalMarkerPhaseFinal &&
		facts.marker.AuditState == TerminalMarkerAuditMaterialized &&
		facts.marker.LineageState == TerminalMarkerLineageComplete &&
		validAudit

	switch {
	case associationDefect:
		item.Classification = RunLifecycleTerminalAssociationDefect
	case indexOrLeaseDefect:
		item.Classification = RunLifecycleTerminalLivenessUnknown
	case validFinal:
		item.Classification = RunLifecycleTerminalComplete
	case facts.receiptState == runLifecycleReceiptCorrupt ||
		facts.receiptState == runLifecycleReceiptClaimMismatch:
		item.Classification = RunLifecycleTerminalLivenessUnknown
	case lifecycleHasCurrentRecoveryClaim(facts.lease, observedAt):
		item.Classification = RunLifecycleTerminalReconciling
	case lifecycleHasCurrentActiveLease(facts.lease, observedAt):
		item.Classification = RunLifecycleTerminalPendingActive
	case lifecycleHasValidYieldedProjection(facts):
		item.Classification = RunLifecycleTerminalStageYielded
	case lifecycleHasBlockedProjection(facts):
		item.Classification = RunLifecycleTerminalProjectionBlocked
	case lifecycleHasLineageDefect(facts):
		item.Classification = RunLifecycleTerminalLineageDefect
		switch facts.marker.LineageState {
		case TerminalMarkerLineagePending:
			appendDiagnostic(
				RunLifecycleDiagnosticLineageDefect,
				RunLifecycleFactLineagePending,
			)
		case TerminalMarkerLineageFailed:
			appendDiagnostic(
				RunLifecycleDiagnosticLineageDefect,
				RunLifecycleFactLineageFailed,
			)
		}
	case facts.lease != nil:
		item.Classification = RunLifecycleTerminalMissing
		if facts.marker == nil {
			appendDiagnostic(
				RunLifecycleDiagnosticTerminalMissing,
				RunLifecycleFactMarkerMissing,
			)
			if validAudit {
				appendDiagnostic(
					RunLifecycleDiagnosticTerminalMissing,
					RunLifecycleFactMarkerMissingValidAudit,
				)
			}
		}
		if !validAudit {
			appendDiagnostic(
				RunLifecycleDiagnosticTerminalMissing,
				RunLifecycleFactAuditMissing,
			)
		}
		if facts.lease.State == AttemptLeaseActive &&
			!facts.lease.LeaseExpiresAt.After(observedAt) &&
			!lifecycleHasCurrentRecoveryClaim(facts.lease, observedAt) {
			appendDiagnostic(
				RunLifecycleDiagnosticTerminalMissing,
				RunLifecycleFactExpiredUnclaimed,
			)
		}
	default:
		item.Classification = RunLifecycleTerminalLivenessUnknown
	}
	switch item.Classification {
	case RunLifecycleTerminalReconciling:
		if facts.lease != nil {
			item.LastErrorCode = cloneLifecycleString(
				facts.lease.LastErrorCode,
			)
		}
	case RunLifecycleTerminalProjectionBlocked:
		if facts.marker != nil {
			item.LastErrorCode = cloneLifecycleString(
				facts.marker.LastErrorCode,
			)
		}
	}

	item.Diagnostics = normalizeLifecycleDiagnostics(item.Diagnostics)
	return item
}

func validateA4AdmissionReceiptClaimBinding(
	receipt A4AdmissionReceiptV1,
	namespace string,
	key string,
	claim ExpectedRunRecordV1,
	canonicalClaim []byte,
	domainKey string,
) error {
	if err := validateA4AdmissionReceiptV1(receipt); err != nil {
		return a4AdmissionReceiptConflict("invalid embedded receipt: %v", err)
	}
	encodedClaim, err := encodeExpectedRunRecordV1(claim)
	if err != nil {
		return a4AdmissionReceiptConflict("invalid expected run claim: %v", err)
	}
	if !bytes.Equal(canonicalClaim, encodedClaim) {
		return a4AdmissionReceiptConflict(
			"canonical claim bytes do not match expected run claim",
		)
	}
	if namespace != a4AdmissionReceiptNamespace(claim.WorkspaceID) ||
		key != a4AdmissionReceiptKey(claim.RunID) {
		return a4AdmissionReceiptConflict("physical receipt key mismatch")
	}
	if receipt.WorkspaceID != claim.WorkspaceID ||
		receipt.RunID != claim.RunID ||
		receipt.ClaimKey != expectedRunClaimKey(claim.RunID) {
		return a4AdmissionReceiptConflict("embedded receipt identity mismatch")
	}
	wantDomainKey := expectedRunDomainKey(claim)
	if domainKey != wantDomainKey || receipt.DomainKey != wantDomainKey {
		return a4AdmissionReceiptConflict("receipt domain key mismatch")
	}
	digest := sha256.Sum256(canonicalClaim)
	if receipt.CanonicalClaimSHA256 != hex.EncodeToString(digest[:]) {
		return a4AdmissionReceiptConflict("canonical claim digest mismatch")
	}
	return nil
}

func lifecycleMarkerMatchesAudit(
	marker TerminalMarkerV1,
	audit TerminalEntryV3,
) bool {
	return marker.RunID == audit.RunID &&
		marker.WorkspaceID == audit.Tenant &&
		string(marker.Status) == audit.Status &&
		marker.UsageInputTokens == int64(audit.SelfExclusive.InputTokens) &&
		marker.UsageOutputTokens == int64(audit.SelfExclusive.OutputTokens) &&
		marker.UsageToolCalls == int64(audit.SelfExclusive.ToolCalls) &&
		math.Float64bits(marker.UsageCostUSD) ==
			math.Float64bits(audit.SelfExclusive.CostUSD)
}

func lifecycleHasCurrentRecoveryClaim(
	lease *RunAttemptLease,
	observedAt time.Time,
) bool {
	return lease != nil &&
		lease.State == AttemptLeaseReconciling &&
		lease.ClaimID != nil &&
		lease.ClaimExpiresAt != nil &&
		lease.ClaimExpiresAt.After(observedAt)
}

func lifecycleHasCurrentActiveLease(
	lease *RunAttemptLease,
	observedAt time.Time,
) bool {
	return lease != nil &&
		lease.State == AttemptLeaseActive &&
		lease.LeaseExpiresAt.After(observedAt)
}

func lifecycleHasValidYieldedProjection(facts runLifecycleFacts) bool {
	return facts.marker != nil &&
		facts.marker.Phase == TerminalMarkerPhaseYielded &&
		facts.marker.Status == TerminalMarkerStatusYielded &&
		facts.marker.AuditState == TerminalMarkerAuditMaterialized &&
		facts.marker.LineageState == TerminalMarkerLineageComplete &&
		facts.audit.Classification == TerminalRecordValid &&
		facts.audit.Entry != nil &&
		facts.lease != nil &&
		facts.lease.State == AttemptLeaseYielded &&
		facts.lease.AttemptGeneration == facts.marker.AttemptGeneration &&
		facts.lease.AttemptID == facts.marker.AttemptID
}

func lifecycleHasBlockedProjection(facts runLifecycleFacts) bool {
	return facts.marker != nil &&
		facts.marker.AuditState == TerminalMarkerAuditBlocked
}

func lifecycleHasLineageDefect(facts runLifecycleFacts) bool {
	return facts.marker != nil &&
		facts.marker.AuditState == TerminalMarkerAuditMaterialized &&
		facts.audit.Classification == TerminalRecordValid &&
		facts.audit.Entry != nil &&
		(facts.marker.LineageState == TerminalMarkerLineagePending ||
			facts.marker.LineageState == TerminalMarkerLineageFailed)
}

func normalizeLifecycleDiagnostics(
	diagnostics []RunLifecycleDiagnostic,
) []RunLifecycleDiagnostic {
	if diagnostics == nil {
		return []RunLifecycleDiagnostic{}
	}
	sort.Slice(diagnostics, func(i, j int) bool {
		if diagnostics[i].Code != diagnostics[j].Code {
			return diagnostics[i].Code < diagnostics[j].Code
		}
		return diagnostics[i].Fact < diagnostics[j].Fact
	})
	out := diagnostics[:0]
	for _, diagnostic := range diagnostics {
		if len(out) == 0 || out[len(out)-1] != diagnostic {
			out = append(out, diagnostic)
		}
	}
	return out
}

func cloneLifecycleString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneLifecycleInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
