package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/deliverycheck"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

type developmentPublicationReadiness struct {
	Ready     bool                           `json:"ready"`
	Workflows []developmentWorkflowReadiness `json:"workflows"`
}

type developmentWorkflowReadiness struct {
	WorkflowID            string   `json:"workflow_id"`
	RequiredCapabilityIDs []string `json:"required_capability_ids"`
	CoveredCapabilityIDs  []string `json:"covered_capability_ids"`
	MissingCapabilityIDs  []string `json:"missing_capability_ids"`
	Passed                bool     `json:"passed"`
}

type developmentTrialLedgerScope struct {
	runID, runSnapshotID, inputRevisionID, actorID string
	terminalAt                            time.Time
	actions                               []businessaction.DevelopmentAction
}

type developmentReadQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func buildDevelopmentPublicationReadiness(
	ctx context.Context,
	query developmentReadQueryer,
	workspaceID, teamID string,
	revision, preparedRevision int64,
	prepared []developmentPrepared,
) (developmentPublicationReadiness, error) {
	readiness := developmentPublicationReadiness{Ready: false, Workflows: []developmentWorkflowReadiness{}}
	if query == nil || workspaceID == "" || teamID == "" || revision < 1 || preparedRevision != revision || len(prepared) == 0 {
		return readiness, nil
	}
	readiness.Ready = true
	for _, candidate := range prepared {
		workflow, err := developmentWorkflowPublicationReadiness(ctx, query, workspaceID, teamID, revision, candidate)
		if err != nil {
			return developmentPublicationReadiness{}, err
		}
		readiness.Workflows = append(readiness.Workflows, workflow)
		readiness.Ready = readiness.Ready && workflow.Passed
	}
	if len(readiness.Workflows) == 0 {
		readiness.Ready = false
	}
	return readiness, nil
}

func developmentWorkflowPublicationReadiness(
	ctx context.Context,
	query developmentReadQueryer,
	workspaceID, teamID string,
	revision int64,
	candidate developmentPrepared,
) (developmentWorkflowReadiness, error) {
	result := developmentWorkflowReadiness{
		WorkflowID:            candidate.ID,
		RequiredCapabilityIDs: []string{},
		CoveredCapabilityIDs:  []string{},
		MissingCapabilityIDs:  []string{},
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(candidate.Envelope)
	if err != nil {
		return result, err
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) > 0 {
		return result, nil
	}
	check, checkErr := deliverycheck.BusinessReceiptCheck(graph.DeliveryContract)
	contractValid := checkErr == nil && machine.RequireBusinessReceiptGraph(graph, payload) == nil
	var params deliverycheck.BusinessReceiptParameters
	if check != nil && checkErr == nil {
		params, checkErr = deliverycheck.ParseBusinessReceiptParameters(check.Parameters)
		contractValid = contractValid && checkErr == nil
		if checkErr == nil {
			result.RequiredCapabilityIDs = append(result.RequiredCapabilityIDs, params.RequiredCapabilityIDs...)
			sort.Strings(result.RequiredCapabilityIDs)
		}
	}

	// Trials of this candidate count whoever ran them; each one is still
	// verified against the account that started it.
	queryRows, err := query.Query(ctx, `SELECT t.request_id::text,t.actor_id,t.request_digest,t.request,t.business_actions,
		r.run_id,r.run_snapshot_id,r.terminal_at,p.receipt
		FROM weave_team_development_trials t
		JOIN weave_team_runs r ON r.workspace_id=t.workspace_id AND r.team_id=t.team_id AND r.workflow_id=t.workflow_id
		JOIN weave_task_queue q ON q.workspace_id=r.workspace_id AND q.id=r.source_task_id AND q.run_snapshot_id=r.run_snapshot_id
		JOIN weave_kernel_publication_requests p ON p.workspace_id=q.workspace_id AND p.request_id=q.context_key AND p.operation='candidate_run'
		WHERE t.workspace_id=$1 AND t.team_id=$2 AND t.revision=$3 AND t.workflow_id=$4
		  AND t.request->'candidate'->>'content_hash'=$5
		  AND p.request_id='development:'||t.request_id::text AND q.source_ref='team-development:'||t.team_id
		  AND p.receipt->>'task_id'=q.id AND p.receipt->>'run_id'=r.run_id AND p.receipt->>'run_snapshot_id'=r.run_snapshot_id
		  AND p.actor_subject=q.actor_subject AND p.receipt->'subject'=p.actor_subject
		  AND p.receipt->'revision'->>'workflow_id'=r.workflow_id
		  AND p.receipt->'revision'->>'workflow_version'=r.workflow_version::text
		  AND r.status='succeeded'
		ORDER BY t.created_at`, workspaceID, teamID, revision, candidate.ID, candidate.Envelope.ContentHash)
	if err != nil {
		return result, err
	}
	defer queryRows.Close()
	covered := map[string]bool{}
	foundSuccessfulTrial := false
	trialsWithRequiredActions := []developmentTrialLedgerScope{}
	requestedActions := machine.GraphBusinessCapabilities(graph, payload)
	for queryRows.Next() {
		var requestID, actorID, storedDigest, runID, runSnapshotID string
		var requestRaw, actionsRaw, receiptRaw []byte
		var terminalAt pgtype.Timestamptz
		if err := queryRows.Scan(&requestID, &actorID, &storedDigest, &requestRaw, &actionsRaw, &runID, &runSnapshotID, &terminalAt, &receiptRaw); err != nil {
			return result, err
		}
		if !terminalAt.Valid {
			continue
		}
		var request publication.CandidateRunRequest
		var admission publication.AdmissionReceipt
		var actions []businessaction.DevelopmentAction
		var trialInput string
		if actorID == "" || json.Unmarshal(requestRaw, &request) != nil || json.Unmarshal(actionsRaw, &actions) != nil ||
			json.Unmarshal(receiptRaw, &admission) != nil || json.Unmarshal(request.Input, &trialInput) != nil ||
			request.RequestID != "development:"+requestID || request.SourceRef != "team-development:"+teamID ||
			request.Purpose != "developer-trial" || request.Candidate.WorkspaceID != workspaceID ||
			request.Candidate.WorkflowID != candidate.ID || request.Candidate.WorkflowVersion != candidate.Envelope.WorkflowVersion ||
			request.Candidate.ContentHash != candidate.Envelope.ContentHash || admission.RunID != runID || admission.RunSnapshotID != runSnapshotID ||
			admission.Verify(execution.WithSubject(ctx, execution.Subject{WorkspaceID: workspaceID, UserID: actorID}), request) != nil {
			continue
		}
		normalizedActions, err := businessaction.ValidateDevelopmentActions(requestedActions, actions)
		if err != nil {
			continue
		}
		requestDigest, err := businessaction.DevelopmentTrialRequestDigest(ctx, request, normalizedActions)
		if err != nil || requestDigest != storedDigest {
			continue
		}
		inputBytes, _ := json.Marshal(trialInput)
		inputHash := sha256.Sum256(inputBytes)
		if request.InputVersion == "" || request.InputVersion != hex.EncodeToString(inputHash[:]) {
			continue
		}
		foundSuccessfulTrial = true
		if contractValid && len(result.RequiredCapabilityIDs) > 0 {
			trialsWithRequiredActions = append(trialsWithRequiredActions, developmentTrialLedgerScope{
				runID: runID, runSnapshotID: runSnapshotID, inputRevisionID: request.InputVersion, actorID: actorID,
				terminalAt: terminalAt.Time.UTC(), actions: normalizedActions,
			})
		}
	}
	if err := queryRows.Err(); err != nil {
		return result, err
	}
	queryRows.Close()

	// A pgx.Tx owns a single connection. Drain and close the trial cursor before
	// reading each durable action ledger through that same transaction.
	for _, trial := range trialsWithRequiredActions {
		events, err := listDevelopmentBusinessActionEvents(ctx, query, workspaceID, trial.runID)
		if err != nil {
			return result, err
		}
		receipts, err := teamrun.ProjectBusinessCompletionReceipts(events)
		if err != nil {
			continue
		}
		allowed := []string{}
		for _, action := range trial.actions {
			if action.SimulationAuthorized {
				allowed = append(allowed, action.CapabilityID)
			}
		}
		sort.Strings(allowed)
		scope := deliverycheck.BusinessReceiptScope{
			WorkspaceID: workspaceID, RunID: trial.runID, RunSnapshotID: trial.runSnapshotID,
			InputRevisionID: trial.inputRevisionID, SubjectID: trial.actorID,
			AllowedCapabilityIDs: allowed, DevelopmentTrial: true, TerminalAt: &trial.terminalAt,
		}
		evaluation := deliverycheck.EvaluateBusinessReceipts(params, scope, receipts, deliverycheck.BusinessReceiptResult{Disposition: "complete"})
		if evaluation.HardStop {
			continue
		}
		var evidence struct {
			Matched []string `json:"matched_capability_ids"`
		}
		if json.Unmarshal(evaluation.Result.Evidence, &evidence) != nil {
			continue
		}
		for _, capabilityID := range evidence.Matched {
			covered[capabilityID] = true
		}
	}
	for _, capabilityID := range result.RequiredCapabilityIDs {
		if covered[capabilityID] {
			result.CoveredCapabilityIDs = append(result.CoveredCapabilityIDs, capabilityID)
		} else {
			result.MissingCapabilityIDs = append(result.MissingCapabilityIDs, capabilityID)
		}
	}
	result.Passed = contractValid && foundSuccessfulTrial && len(result.MissingCapabilityIDs) == 0
	return result, nil
}

func listDevelopmentBusinessActionEvents(ctx context.Context, query developmentReadQueryer, workspaceID, runID string) ([]teamrun.ActivityEvent, error) {
	var count int
	if err := query.QueryRow(ctx, `SELECT count(*) FROM weave_team_run_activity_events
		WHERE workspace_id=$1 AND run_id=$2 AND kind='business_action_started'`, workspaceID, runID).Scan(&count); err != nil {
		return nil, err
	}
	if count > teamrun.MaxBusinessActionOutcomesPerRun {
		return nil, teamrun.ErrBusinessActionOutcomeLimitExceeded
	}
	rows, err := query.Query(ctx, `SELECT workspace_id,run_id,seq,event_id,kind,
		COALESCE(node_id,''),COALESCE(member_id,''),COALESCE(member_version,0),detail,occurred_at
		FROM weave_team_run_activity_events WHERE workspace_id=$1 AND run_id=$2
		AND kind IN ('business_action_started','business_action_result') ORDER BY seq`, workspaceID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []teamrun.ActivityEvent{}
	for rows.Next() {
		var event teamrun.ActivityEvent
		if err := rows.Scan(&event.WorkspaceID, &event.RunID, &event.Seq, &event.EventID, &event.Kind,
			&event.NodeID, &event.MemberID, &event.MemberVersion, &event.Detail, &event.OccurredAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}
