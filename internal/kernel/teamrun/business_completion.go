package teamrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/deliverycheck"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func ProjectBusinessCompletionReceipts(events []ActivityEvent) ([]deliverycheck.BusinessReceipt, error) {
	receipts := []deliverycheck.BusinessReceipt{}
	_, err := projectBusinessActionOutcomes(events, func(event ActivityEvent, detail businessActionActivityDetailV1, status string) {
		receipts = append(receipts, deliverycheck.BusinessReceipt{
			WorkspaceID: event.WorkspaceID, RunID: event.RunID, RunSnapshotID: detail.RunSnapshotID, SubjectID: detail.ActorID,
			InputRevisionID: detail.InputRevisionID, CapabilityID: detail.CapabilityID, ObjectName: detail.ObjectName,
			RecordID: detail.RecordID, OperationID: detail.OperationID, Status: status,
			Simulated: detail.Source == businessaction.ActionOutcomeSourceDevelopmentSimulation, OccurredAt: event.OccurredAt,
		})
	})
	if err != nil {
		return nil, err
	}
	return receipts, nil
}

func (r *WorkflowSerialRuntime) completionContext(run TeamRun, graph machine.GraphDefinition) func(context.Context) (context.Context, error) {
	return func(ctx context.Context) (context.Context, error) {
		declared, declarationErr := deliverycheck.BusinessReceiptCheck(graph.DeliveryContract)
		if declarationErr != nil {
			return ctx, executionError(ErrorCodeNodeOutputInvalid, &compiler.NodeCompletionCheckError{Reason: "business_receipt_contract_invalid"})
		}
		if declared == nil {
			return ctx, nil
		}
		candidate := deliverable.Candidate{WorkspaceID: run.WorkspaceID, RunID: run.RunID, RunSnapshotID: run.RunSnapshotID}
		if r.BusinessReceiptReader == nil {
			return ctx, executionError(ErrorCodeNodeOutputInvalid, &compiler.NodeCompletionCheckError{Reason: "business_receipt_reader_unavailable"})
		}
		frame, err := r.BusinessReceiptReader(ctx, candidate)
		if err != nil {
			return ctx, executionError(ErrorCodeNodeOutputInvalid, &compiler.NodeCompletionCheckError{Reason: "business_receipt_evidence_unavailable"})
		}
		if deliverycheck.ValidateBusinessReceiptFrame(frame, candidate) != nil {
			return ctx, executionError(ErrorCodeNodeOutputInvalid, &compiler.NodeCompletionCheckError{Reason: "business_receipt_identity_mismatch"})
		}
		check, err := deliverycheck.BusinessReceiptCheck(frame.Contract)
		if err != nil {
			return ctx, executionError(ErrorCodeNodeOutputInvalid, &compiler.NodeCompletionCheckError{Reason: "business_receipt_contract_invalid"})
		}
		if check == nil {
			return ctx, executionError(ErrorCodeNodeOutputInvalid, &compiler.NodeCompletionCheckError{Reason: "business_receipt_contract_missing"})
		}
		params, _ := deliverycheck.ParseBusinessReceiptParameters(check.Parameters)
		policy, _ := json.Marshal(struct {
			Check deliverable.CheckSpec
			Scope deliverycheck.BusinessReceiptScope
		}{*check, frame.Scope})
		hash := sha256.Sum256(policy)
		loadLatest := func(nextCtx context.Context) (deliverycheck.BusinessReceiptFrame, error) {
			latest, readErr := r.BusinessReceiptReader(nextCtx, candidate)
			if readErr != nil {
				return deliverycheck.BusinessReceiptFrame{}, &compiler.NodeCompletionCheckError{Reason: "business_receipt_evidence_unavailable"}
			}
			if deliverycheck.ValidateBusinessReceiptFrame(latest, candidate) != nil {
				return deliverycheck.BusinessReceiptFrame{}, &compiler.NodeCompletionCheckError{Reason: "business_receipt_identity_mismatch"}
			}
			latestCheck, checkErr := deliverycheck.BusinessReceiptCheck(latest.Contract)
			if checkErr != nil || latestCheck == nil {
				return deliverycheck.BusinessReceiptFrame{}, &compiler.NodeCompletionCheckError{Reason: "business_receipt_contract_changed"}
			}
			latestPolicy, _ := json.Marshal(struct {
				Check deliverable.CheckSpec
				Scope deliverycheck.BusinessReceiptScope
			}{*latestCheck, latest.Scope})
			if sha256.Sum256(latestPolicy) != hash {
				return deliverycheck.BusinessReceiptFrame{}, &compiler.NodeCompletionCheckError{Reason: "business_receipt_binding_changed"}
			}
			return latest, nil
		}
		review := func(nextCtx context.Context, candidate compiler.NodeCompletionCandidate) (compiler.NodeCompletionVerdict, error) {
			latest, readErr := loadLatest(nextCtx)
			if readErr != nil {
				return compiler.NodeCompletionVerdict{}, readErr
			}
			final, _, parseErr := machine.NormalizeWorkbenchResultV1([]byte(candidate.Content))
			if parseErr != nil {
				return compiler.NodeCompletionVerdict{}, &compiler.NodeCompletionCheckError{Reason: "business_receipt_result_invalid"}
			}
			result := deliverycheck.EvaluateBusinessReceipts(params, latest.Scope, latest.Receipts, deliverycheck.BusinessReceiptResult{
				Disposition: final.Disposition, MissingItems: final.MissingItems,
				RequireInputReview: !businessInputReviewed(candidate.PriorRejections),
			})
			if result.HardStop {
				return compiler.NodeCompletionVerdict{}, &compiler.NodeCompletionCheckError{Reason: result.Result.Reason}
			}
			verdict := compiler.NodeCompletionVerdict{Accepted: result.Accept, Feedback: result.Feedback}
			if !result.Accept {
				verdict.Reason = result.Result.Reason
			}
			return verdict, nil
		}
		// Only a rejected completion claim with no recorded attempt for a required
		// action constrains the next round. Missing-input reviews never force a
		// call, and any existing operation (even failed or unknown) is never retried.
		chooseTool := func(nextCtx context.Context, input compiler.NodeToolChoiceInput) (*contract.ToolChoice, error) {
			if !input.CompletionRejected || input.CompletionRejectionReason != deliverycheck.ReasonRequiredActionMissing {
				return nil, nil
			}
			latest, readErr := loadLatest(nextCtx)
			if readErr != nil {
				return nil, readErr
			}
			return requiredActionToolChoice(deliverycheck.UnattemptedRequiredCapabilities(params, latest.Scope, latest.Receipts), input.Tools), nil
		}

		beforeTool := func(nextCtx context.Context) error {
			latest, readErr := loadLatest(nextCtx)
			if readErr != nil {
				return readErr
			}
			// Only hard facts gate tools. Unsatisfied required actions are
			// precisely why a model may need to call an authorized tool.
			result := deliverycheck.EvaluateBusinessReceipts(params, latest.Scope, latest.Receipts, deliverycheck.BusinessReceiptResult{Disposition: "complete"})
			if result.HardStop {
				return &compiler.NodeCompletionCheckError{Reason: result.Result.Reason}
			}
			return nil
		}
		// v2 adds the one-time missing-input review and the required-action tool
		// choice; a pause taken under v1 semantics must not resume under v2.
		return compiler.WithNodeCompletionPolicy(ctx, deliverycheck.BusinessReceiptsID+"/"+deliverycheck.BusinessReceiptsVersion+":dispatch-barrier.v2:"+hex.EncodeToString(hash[:]),
			compiler.NodeCompletionPolicy{Review: review, BeforeTool: beforeTool, ChooseTool: chooseTool}), nil
	}
}

// businessInputReviewed reports whether this loop already gave the one-time
// missing-input review, from Loom's structured rejection history rather than
// from any message text.
func businessInputReviewed(priorRejections []string) bool {
	for _, reason := range priorRejections {
		if reason == deliverycheck.ReasonInputReview {
			return true
		}
	}
	return false
}

// requiredActionToolChoice names the single missing action tool when it is the
// only tool offered; otherwise it requires some call so prerequisite reads stay
// possible. Missing actions whose tools this node does not offer are not forced.
func requiredActionToolChoice(missing []string, tools []contract.ToolDef) *contract.ToolChoice {
	offered := map[string]bool{}
	for _, tool := range tools {
		offered[tool.Name] = true
	}
	names := []string{}
	for _, id := range missing {
		if name, err := businessaction.CapabilityToolName(id); err == nil && offered[name] {
			names = append(names, name)
		}
	}
	switch {
	case len(names) == 0:
		return nil
	case len(names) == 1 && len(tools) == 1:
		return &contract.ToolChoice{Mode: contract.ToolChoiceTool, Name: names[0]}
	default:
		return &contract.ToolChoice{Mode: contract.ToolChoiceRequired}
	}
}

func isCompletionCheckError(err error) bool {
	var check *compiler.NodeCompletionCheckError
	return errors.As(err, &check)
}
