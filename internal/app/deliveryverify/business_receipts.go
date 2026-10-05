package deliveryverify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/deliverycheck"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// BusinessReceiptReader reads only immutable scope/contract bindings and the
// current authoritative activity ledger. It never reads credentials or invokes
// Forge. Terminal grant revocation does not erase historical successful effects.
func BusinessReceiptReader(pool *pgxpool.Pool) deliverycheck.BusinessReceiptReader {
	return func(ctx context.Context, c deliverable.Candidate) (deliverycheck.BusinessReceiptFrame, error) {
		frame := deliverycheck.BusinessReceiptFrame{}
		if pool == nil || c.WorkspaceID == "" || c.RunID == "" || c.RunSnapshotID == "" {
			return frame, errors.New("business receipt identity unavailable")
		}
		var raw []byte
		var inputID, status, workflowID string
		var workflowVersion int
		var terminalAt pgtype.Timestamptz
		err := pool.QueryRow(ctx, `SELECT b.contract,b.input_revision_id,r.status,r.workflow_id,r.workflow_version,r.terminal_at
 FROM weave_team_runs r JOIN weave_run_delivery_state b ON b.workspace_id=r.workspace_id AND b.run_snapshot_id=r.run_snapshot_id
		WHERE r.workspace_id=$1 AND r.run_id=$2 AND r.run_snapshot_id=$3 AND (b.run_id IS NULL OR b.run_id=r.run_id) AND b.workflow_id=r.workflow_id AND b.workflow_version=r.workflow_version`, c.WorkspaceID, c.RunID, c.RunSnapshotID).Scan(&raw, &inputID, &status, &workflowID, &workflowVersion, &terminalAt)
		if err != nil {
			return frame, err
		}
		frame.Contract, err = deliverable.DecodeDeliveryContract(raw)
		if err != nil {
			return frame, err
		}
		check, err := deliverycheck.BusinessReceiptCheck(frame.Contract)
		if err != nil || check == nil {
			return frame, err
		}
		frame.Scope = deliverycheck.BusinessReceiptScope{WorkspaceID: c.WorkspaceID, RunID: c.RunID, RunSnapshotID: c.RunSnapshotID, InputRevisionID: inputID, Closed: status == "cancelled" || status == "abandoned" || status == "cancel_requested"}
		if terminalAt.Valid {
			value := terminalAt.Time.UTC()
			frame.Scope.TerminalAt = &value
		}
		// The kernel receipt and task commit atomically. The product's trial
		// receipt is written afterwards and may still be NULL when work starts.
		var trialActor, trialTeam, trialRequestID string
		var trialRequest, admissionReceipt, trialActions []byte
		var trialRequestDigest string
		trialErr := pool.QueryRow(ctx, `SELECT t.actor_id,t.team_id,q.context_key,t.request,t.request_digest,t.business_actions,p.receipt
 FROM weave_team_runs r
 JOIN weave_task_queue q ON q.workspace_id=r.workspace_id AND q.id=r.source_task_id AND q.run_snapshot_id=r.run_snapshot_id
 JOIN weave_kernel_publication_requests p ON p.workspace_id=q.workspace_id AND p.request_id=q.context_key AND p.operation='candidate_run'
 JOIN weave_team_development_trials t ON t.workspace_id=p.workspace_id AND p.request_id='development:'||t.request_id::text AND t.team_id=r.team_id AND t.workflow_id=r.workflow_id
 WHERE r.workspace_id=$1 AND r.run_id=$2 AND r.run_snapshot_id=$3
 AND p.receipt->>'task_id'=q.id AND p.receipt->>'run_id'=r.run_id AND p.receipt->>'run_snapshot_id'=r.run_snapshot_id
 AND p.actor_subject=q.actor_subject AND p.receipt->'subject'=p.actor_subject
		 AND p.receipt->'revision'->>'workflow_id'=r.workflow_id AND p.receipt->'revision'->>'workflow_version'=r.workflow_version::text`, c.WorkspaceID, c.RunID, c.RunSnapshotID).Scan(&trialActor, &trialTeam, &trialRequestID, &trialRequest, &trialRequestDigest, &trialActions, &admissionReceipt)
		if trialErr == nil {
			var request publication.CandidateRunRequest
			var receipt publication.AdmissionReceipt
			var trialInput string
			var actions []businessaction.DevelopmentAction
			if json.Unmarshal(trialRequest, &request) != nil || json.Unmarshal(admissionReceipt, &receipt) != nil || json.Unmarshal(trialActions, &actions) != nil || json.Unmarshal(request.Input, &trialInput) != nil ||
				request.RequestID != trialRequestID || request.InputVersion != inputID || request.SourceRef != "team-development:"+trialTeam || request.Purpose != "developer-trial" ||
				request.Candidate.WorkspaceID != c.WorkspaceID || request.Candidate.WorkflowID != workflowID || request.Candidate.WorkflowVersion != workflowVersion ||
				receipt.Verify(execution.WithSubject(ctx, execution.Subject{WorkspaceID: c.WorkspaceID, UserID: trialActor}), request) != nil {
				return frame, errors.New("trial admission binding mismatch")
			}
			payload, payloadErr := frozen.DecodeArtifactEnvelopeV1(request.Candidate)
			if payloadErr != nil {
				return frame, errors.New("trial candidate binding mismatch")
			}
			graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
			if report != nil && len(report.Issues) > 0 {
				return frame, errors.New("trial workflow binding mismatch")
			}
			normalizedActions, actionErr := businessaction.ValidateDevelopmentActions(machine.GraphBusinessCapabilities(graph, payload), actions)
			if actionErr != nil {
				return frame, errors.New("trial action scope mismatch")
			}
			actionScopeDigest, digestErr := businessaction.DevelopmentTrialRequestDigest(ctx, request, normalizedActions)
			if digestErr != nil || actionScopeDigest != trialRequestDigest {
				return frame, errors.New("trial action scope digest mismatch")
			}
			encodedInput, _ := json.Marshal(trialInput)
			inputDigest := sha256.Sum256(encodedInput)
			if trialActor == "" || inputID != hex.EncodeToString(inputDigest[:]) {
				return frame, errors.New("trial input binding mismatch")
			}
			frame.Scope.SubjectID = trialActor
			frame.Scope.DevelopmentTrial = true
			frame.Scope.AllowedCapabilityIDs = []string{}
			for _, action := range normalizedActions {
				if action.SimulationAuthorized {
					frame.Scope.AllowedCapabilityIDs = append(frame.Scope.AllowedCapabilityIDs, action.CapabilityID)
				}
			}
			sort.Strings(frame.Scope.AllowedCapabilityIDs)
		} else if !errors.Is(trialErr, pgx.ErrNoRows) {
			return frame, trialErr
		} else {
			var actions, resources []byte
			var taskSHA, revocation string
			var closed bool
			err = pool.QueryRow(ctx, `SELECT i.user_id,i.task_sha256,i.closed_at IS NOT NULL,d.allowed_actions,d.resources,COALESCE(d.revocation_reason,'')
 FROM weave_dispatch_input_revisions i JOIN weave_task_business_delegations d ON d.workspace_id=i.workspace_id AND d.input_revision_id=i.input_revision_id AND d.user_id=i.user_id
 WHERE i.workspace_id=$1 AND i.input_revision_id=$2 AND i.consumed_run_id=$3 AND i.workflow_id=$4 AND i.workflow_version=$5 AND d.workflow_id=i.workflow_id AND d.workflow_version=i.workflow_version`, c.WorkspaceID, inputID, c.RunID, workflowID, workflowVersion).Scan(&frame.Scope.SubjectID, &taskSHA, &closed, &actions, &resources, &revocation)
			if err != nil {
				return frame, err
			}
			frame.Scope.Closed = frame.Scope.Closed || closed || revocation == "employee_cancel" || revocation == "subject_inactive" || revocation == "account_disabled"
			if json.Unmarshal(actions, &frame.Scope.AllowedCapabilityIDs) != nil || frame.Scope.AllowedCapabilityIDs == nil {
				return frame, errors.New("frozen action scope unavailable")
			}
			sort.Strings(frame.Scope.AllowedCapabilityIDs)
			record, recordErr := businessaction.CompletionRecordBinding(resources, inputID, taskSHA)
			if recordErr != nil {
				return frame, recordErr
			}
			if record != nil {
				frame.Scope.ObjectName, frame.Scope.RecordID = record.ObjectName, record.RecordID
			}
		}
		events, err := (&teamrun.PGActivityStore{Transactions: pool}).ListBusinessActionEvents(ctx, c.WorkspaceID, c.RunID)
		if err != nil {
			return frame, err
		}
		frame.Receipts, err = teamrun.ProjectBusinessCompletionReceipts(events)
		return frame, err
	}
}

func businessReceiptVerifier(read deliverycheck.BusinessReceiptReader) deliverable.Verifier {
	return func(ctx context.Context, input deliverable.VerificationInput) (deliverable.CheckResult, error) {
		params, err := deliverycheck.ParseBusinessReceiptParameters(input.Check.Parameters)
		if err != nil {
			return deliverable.CheckResult{}, err
		}
		if read == nil {
			return deliverable.CheckResult{}, errors.New("business receipt reader unavailable")
		}
		frame, err := read(ctx, input.Candidate)
		if err != nil {
			return deliverable.CheckResult{}, err
		}
		if err := deliverycheck.ValidateBusinessReceiptFrame(frame, input.Candidate); err != nil {
			return deliverable.CheckResult{}, err
		}
		check, err := deliverycheck.BusinessReceiptCheck(frame.Contract)
		if err != nil || check == nil || check.ID != input.Check.ID || string(check.Parameters) != string(input.Check.Parameters) {
			return deliverable.CheckResult{}, errors.New("frozen business receipt check changed")
		}
		final, _, err := machine.NormalizeWorkbenchResultV1(input.Candidate.Output)
		if err != nil {
			return deliverable.CheckResult{}, errors.New("business receipt result protocol invalid")
		}
		return deliverycheck.EvaluateBusinessReceipts(params, frame.Scope, frame.Receipts, deliverycheck.BusinessReceiptResult{Disposition: final.Disposition, MissingItems: final.MissingItems}).Result, nil
	}
}
