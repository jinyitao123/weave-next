package deliverycheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

const BusinessReceiptsID = "weave.business-action-receipts"
const BusinessReceiptsVersion = "v1"

type BusinessReceiptParameters struct {
	RequiredCapabilityIDs []string `json:"required_capability_ids"`
	WhenAuthorized        bool     `json:"when_authorized"`
	AllowNeedsInput       bool     `json:"allow_needs_input"`
}

func ParseBusinessReceiptParameters(raw json.RawMessage) (BusinessReceiptParameters, error) {
	if len(raw) > 8192 {
		return BusinessReceiptParameters{}, errors.New("business receipt parameters are invalid")
	}
	canonical, canonicalErr := frozen.CanonicalizeJSON(raw)
	if canonicalErr != nil {
		return BusinessReceiptParameters{}, errors.New("business receipt parameters are invalid")
	}
	raw = canonical
	var wire struct {
		RequiredCapabilityIDs []string `json:"required_capability_ids"`
		WhenAuthorized        *bool    `json:"when_authorized"`
		AllowNeedsInput       *bool    `json:"allow_needs_input"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if len(raw) > 8192 || d.Decode(&wire) != nil || d.Decode(&struct{}{}) != io.EOF || wire.WhenAuthorized == nil || !*wire.WhenAuthorized || wire.AllowNeedsInput == nil || len(wire.RequiredCapabilityIDs) < 1 || len(wire.RequiredCapabilityIDs) > 16 {
		return BusinessReceiptParameters{}, errors.New("business receipt parameters are invalid")
	}
	seen := map[string]bool{}
	for _, id := range wire.RequiredCapabilityIDs {
		if len(id) > 160 || id != strings.TrimSpace(id) || !strings.HasPrefix(id, "forge:action:") || !strings.Contains(strings.TrimPrefix(id, "forge:action:"), ".") || seen[id] {
			return BusinessReceiptParameters{}, errors.New("business receipt capabilities are invalid")
		}
		seen[id] = true
	}
	sort.Strings(wire.RequiredCapabilityIDs)
	return BusinessReceiptParameters{wire.RequiredCapabilityIDs, true, *wire.AllowNeedsInput}, nil
}

// BusinessReceiptCheck selects the existing delivery-contract extension, not a
// second goal protocol. Unsupported versions and ambiguous declarations fail.
func BusinessReceiptCheck(c *deliverable.DeliveryContract) (*deliverable.CheckSpec, error) {
	if c == nil {
		return nil, nil
	}
	var found *deliverable.CheckSpec
	for _, check := range c.RequiredChecks {
		if check.VerifierID != BusinessReceiptsID {
			continue
		}
		if found != nil || check.VerifierVersion != BusinessReceiptsVersion || c.ExternalEffectsCheckID != check.ID {
			return nil, errors.New("business receipt check binding is invalid")
		}
		if _, err := ParseBusinessReceiptParameters(check.Parameters); err != nil {
			return nil, err
		}
		copy := check
		found = &copy
	}
	return found, nil
}

func ValidateBusinessReceiptCapabilities(c *deliverable.DeliveryContract, available []string) error {
	check, err := BusinessReceiptCheck(c)
	if err != nil || check == nil {
		return err
	}
	params, _ := ParseBusinessReceiptParameters(check.Parameters)
	for _, id := range params.RequiredCapabilityIDs {
		present := false
		for _, bound := range available {
			present = present || bound == id
		}
		if !present {
			return errors.New("business receipt requirement is not bound to a workflow member")
		}
	}
	return nil
}

// These are trusted storage projections. No model-provided identity or receipt
// is accepted. A nil AllowedCapabilityIDs means unavailable, not read-only.
type BusinessReceiptScope struct {
	WorkspaceID, RunID, RunSnapshotID, InputRevisionID, SubjectID string
	AllowedCapabilityIDs                                          []string
	ObjectName, RecordID                                          string
	Closed                                                        bool
}
type BusinessReceipt struct {
	WorkspaceID, RunID, InputRevisionID, CapabilityID, ObjectName, RecordID, OperationID, Status string
}
type BusinessReceiptFrame struct {
	Contract *deliverable.DeliveryContract
	Scope    BusinessReceiptScope
	Receipts []BusinessReceipt
}
type BusinessReceiptReader func(context.Context, deliverable.Candidate) (BusinessReceiptFrame, error)

func ValidateBusinessReceiptFrame(frame BusinessReceiptFrame, c deliverable.Candidate) error {
	if frame.Scope.WorkspaceID != c.WorkspaceID || frame.Scope.RunID != c.RunID || frame.Scope.RunSnapshotID != c.RunSnapshotID {
		return errors.New("business receipt frame identity mismatch")
	}
	return nil
}

type BusinessReceiptEvaluation struct {
	Accept   bool
	HardStop bool
	Feedback string
	Result   deliverable.CheckResult
}

func EvaluateBusinessReceipts(params BusinessReceiptParameters, scope BusinessReceiptScope, receipts []BusinessReceipt, final BusinessReceiptResult) BusinessReceiptEvaluation {
	evidence := struct {
		Version          string   `json:"version"`
		ParametersSHA256 string   `json:"parameters_sha256"`
		RunID            string   `json:"run_id"`
		InputRevisionID  string   `json:"input_revision_id"`
		Matched          []string `json:"matched_capability_ids"`
	}{Version: BusinessReceiptsVersion, RunID: scope.RunID, InputRevisionID: scope.InputRevisionID, Matched: []string{}}
	encoded, _ := json.Marshal(params)
	digest := sha256.Sum256(encoded)
	evidence.ParametersSHA256 = hex.EncodeToString(digest[:])
	result := func(accept, hard bool, status deliverable.VerificationStatus, reason, feedback string) BusinessReceiptEvaluation {
		raw, _ := json.Marshal(evidence)
		return BusinessReceiptEvaluation{accept, hard, feedback, deliverable.CheckResult{VerifierID: BusinessReceiptsID, VerifierVersion: BusinessReceiptsVersion, Status: status, Reason: reason, Evidence: raw}}
	}
	hard := func(reason string) BusinessReceiptEvaluation {
		return result(false, true, deliverable.VerificationUnknown, reason, "")
	}
	if scope.WorkspaceID == "" || scope.RunID == "" || scope.RunSnapshotID == "" || scope.SubjectID == "" || scope.Closed || scope.AllowedCapabilityIDs == nil || receipts == nil {
		return hard("business_receipt_evidence_unavailable")
	}
	allowed := map[string]bool{}
	for _, id := range scope.AllowedCapabilityIDs {
		allowed[id] = true
	}
	required := map[string]bool{}
	for _, id := range params.RequiredCapabilityIDs {
		if allowed[id] {
			required[id] = true
		}
	}
	if len(required) > 0 && (scope.InputRevisionID == "" || scope.ObjectName == "" || scope.RecordID == "") {
		return hard("business_receipt_record_binding_missing")
	}
	for id := range required {
		if !strings.HasPrefix(id, "forge:action:"+scope.ObjectName+".") {
			return hard("business_receipt_record_binding_mismatch")
		}
	}
	operations := map[string]BusinessReceipt{}
	succeeded := map[string]bool{}
	failed, unknown := false, false
	for _, receipt := range receipts {
		if receipt.WorkspaceID != scope.WorkspaceID || receipt.RunID != scope.RunID || receipt.InputRevisionID != scope.InputRevisionID || receipt.OperationID == "" || !allowed[receipt.CapabilityID] || receipt.ObjectName != scope.ObjectName || receipt.RecordID != scope.RecordID {
			return hard("business_receipt_identity_mismatch")
		}
		if previous, ok := operations[receipt.OperationID]; ok {
			if previous != receipt {
				return hard("business_receipt_operation_conflict")
			}
			continue
		}
		operations[receipt.OperationID] = receipt
		if receipt.Status == "failed" {
			failed = true
		} else if receipt.Status != "succeeded" {
			unknown = true
		} else {
			succeeded[receipt.CapabilityID] = true
		}
	}
	if failed {
		return result(false, true, deliverable.VerificationFailed, "business_action_failed", "")
	}
	if unknown {
		return hard("business_action_unknown")
	}
	for id := range required {
		if succeeded[id] {
			evidence.Matched = append(evidence.Matched, id)
		}
	}
	sort.Strings(evidence.Matched)
	if len(required) == 0 {
		return result(true, false, deliverable.VerificationPassed, "business_actions_not_requested", "")
	}
	if final.Disposition == "needs_input" {
		if params.AllowNeedsInput && len(final.MissingItems) > 0 && len(operations) == 0 {
			return result(true, false, deliverable.VerificationUnknown, "business_actions_deferred_for_input", "")
		}
		return hard("business_actions_cannot_be_deferred")
	}
	if final.Disposition != "complete" {
		return hard("business_receipt_result_invalid")
	}
	missing := []string{}
	for id := range required {
		if !succeeded[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return result(false, false, deliverable.VerificationFailed, "required_business_action_missing", "The frozen delivery contract requires successful platform receipts for: "+strings.Join(missing, ", ")+". No matching call is recorded. Use only the authorized tools if prerequisites are met, or report genuine missing input. Do not repeat actions that already succeeded. Do not claim a tool was called without its receipt.")
	}
	return result(true, false, deliverable.VerificationPassed, "required_business_actions_recorded", "")
}

type BusinessReceiptResult struct {
	Disposition  string
	MissingItems []string
}
