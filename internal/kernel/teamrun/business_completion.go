package teamrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/deliverycheck"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func ProjectBusinessCompletionReceipts(events []ActivityEvent) ([]deliverycheck.BusinessReceipt, error) {
	receipts := []deliverycheck.BusinessReceipt{}
	_, err := projectBusinessActionOutcomes(events, func(event ActivityEvent, detail businessActionActivityDetailV1, status string) {
		receipts = append(receipts, deliverycheck.BusinessReceipt{WorkspaceID: event.WorkspaceID, RunID: event.RunID, InputRevisionID: detail.InputRevisionID, CapabilityID: detail.CapabilityID, ObjectName: detail.ObjectName, RecordID: detail.RecordID, OperationID: detail.OperationID, Status: status})
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
		verify := func(nextCtx context.Context, content string) (bool, string, error) {
			latest, readErr := loadLatest(nextCtx)
			if readErr != nil {
				return false, "", readErr
			}
			final, _, parseErr := machine.NormalizeWorkbenchResultV1([]byte(content))
			if parseErr != nil {
				return false, "", &compiler.NodeCompletionCheckError{Reason: "business_receipt_result_invalid"}
			}
			result := deliverycheck.EvaluateBusinessReceipts(params, latest.Scope, latest.Receipts, deliverycheck.BusinessReceiptResult{Disposition: final.Disposition, MissingItems: final.MissingItems})
			if result.HardStop {
				return false, "", &compiler.NodeCompletionCheckError{Reason: result.Result.Reason}
			}
			return result.Accept, result.Feedback, nil
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
		return compiler.WithNodeCompletionCheck(ctx, deliverycheck.BusinessReceiptsID+"/"+deliverycheck.BusinessReceiptsVersion+":dispatch-barrier.v1:"+hex.EncodeToString(hash[:]), verify, beforeTool), nil
	}
}

func isCompletionCheckError(err error) bool {
	var check *compiler.NodeCompletionCheckError
	return errors.As(err, &check)
}
