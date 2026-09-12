package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/base/teamrun"
)

func (service *InvocationService) Get(
	ctx context.Context,
	principal Principal,
	invocationID string,
) (InvocationStatus, error) {
	if service == nil || service.Store == nil || service.Store.pool == nil ||
		invalidIdentity(invocationID) || invalidIdentity(principal.WorkspaceID) ||
		invalidIdentity(principal.AppID) || invalidIdentity(principal.CredentialID) {
		return InvocationStatus{}, ErrInvalid
	}
	if !principal.HasScope("read") {
		return InvocationStatus{}, ErrScopeDenied
	}
	if err := service.validateActivePrincipal(ctx, principal, "read"); err != nil {
		return InvocationStatus{}, err
	}

	var (
		invocation                  Invocation
		outputContract, limitsJSON  json.RawMessage
		policyJSON, taskResult      json.RawMessage
		taskStatus                  string
		taskUpdatedAt               time.Time
		runStatus, runErrorCode     *string
		runUpdatedAt, runTerminalAt *time.Time
		cancelled                   bool
	)
	row := service.Store.pool.QueryRow(ctx, `
		SELECT `+qualifiedInvocationColumns("invocation")+`,
		       release.output_contract,release.execution_limits,release.result_policy,
		       task.status,task.result,task.updated_at,
		       run.status,run.error_code,run.updated_at,run.terminal_at,
		       EXISTS(SELECT 1 FROM weave_capability_invocation_cancellations AS cancellation
		              WHERE cancellation.workspace_id=invocation.workspace_id
		                AND cancellation.app_id=invocation.app_id
		                AND cancellation.request_id=invocation.request_id)
		FROM weave_capability_invocations AS invocation
		JOIN weave_capability_releases AS release
		  ON release.workspace_id=invocation.workspace_id
		 AND release.id=invocation.release_id
		JOIN weave_task_queue AS task
		  ON task.workspace_id=invocation.workspace_id AND task.id=invocation.task_id
		LEFT JOIN weave_team_runs AS run
		  ON run.workspace_id=invocation.workspace_id AND run.run_id=invocation.run_id
		WHERE invocation.workspace_id=$1 AND invocation.app_id=$2
		  AND invocation.invocation_id=$3
	`, principal.WorkspaceID, principal.AppID, invocationID)
	if err := row.Scan(
		&invocation.WorkspaceID, &invocation.AppID, &invocation.InvocationID,
		&invocation.RequestID, &invocation.CapabilityID, &invocation.ReleaseVersion,
		&invocation.ReleaseID, &invocation.GrantID, &invocation.CredentialID,
		&invocation.ArtifactContentHash, &invocation.Normalization,
		&invocation.InputHash, &invocation.RequestFingerprint, &invocation.ConcurrencyKey,
		&invocation.RunID, &invocation.TaskID, &invocation.AcceptedAt, &invocation.DeadlineAt,
		&outputContract, &limitsJSON, &policyJSON,
		&taskStatus, &taskResult, &taskUpdatedAt,
		&runStatus, &runErrorCode, &runUpdatedAt, &runTerminalAt, &cancelled,
	); errors.Is(err, pgx.ErrNoRows) {
		return InvocationStatus{}, ErrNotFound
	} else if err != nil {
		return InvocationStatus{}, fmt.Errorf("read capability invocation: %w", err)
	}
	var limits ExecutionLimits
	var policy ResultPolicy
	if decodeExactJSON(limitsJSON, &limits) != nil || decodeExactJSON(policyJSON, &policy) != nil {
		return InvocationStatus{}, ErrReleaseUnsafe
	}

	status := InvocationStatus{Invocation: invocation}
	status.ExecutionStatus = invocationExecutionStatus(taskStatus, runStatus)
	status.CancellationStatus = invocationCancellationStatus(cancelled, taskStatus, runStatus)
	if runUpdatedAt != nil {
		value := *runUpdatedAt
		status.UpdatedAt = &value
	} else {
		value := taskUpdatedAt
		status.UpdatedAt = &value
	}
	if runTerminalAt != nil {
		value := *runTerminalAt
		status.TerminalAt = &value
	}
	if status.ExecutionStatus == "failed" || status.ExecutionStatus == "abandoned" {
		status.FailureCode = "execution_failed"
	}
	status.ResultAvailability = "pending"
	outputValidated := false
	if runStatus != nil && *runStatus == string(teamrun.StatusSucceeded) &&
		taskStatus == taskqueue.StatusCompleted {
		output, err := extractWorkflowOutput(taskResult)
		if err != nil {
			status.ResultAvailability = "contract_invalid"
			status.ResultProblems = []SchemaProblem{{Code: "result_envelope_invalid"}}
		} else if err := validateValueResources(output, limits, true); err != nil {
			status.ResultAvailability = "contract_invalid"
			if limit := new(ResourceLimitError); errors.As(err, &limit) {
				status.ResultProblems = []SchemaProblem{{Code: limit.Limit}}
			} else {
				status.ResultProblems = []SchemaProblem{{Code: "result_json_invalid"}}
			}
		} else if problems := validateOutputContract(outputContract, output); len(problems) != 0 {
			status.ResultAvailability = "contract_invalid"
			status.ResultProblems = problems
		} else if filtered, err := filterResultFields(output, policy.ExposedFields); err != nil {
			status.ResultAvailability = "contract_invalid"
			status.ResultProblems = []SchemaProblem{{Code: "result_shape_invalid"}}
		} else {
			status.ResultAvailability = "available"
			status.Result = filtered
			outputValidated = true
		}
	} else if terminalExecutionStatus(status.ExecutionStatus) {
		status.ResultAvailability = "unavailable"
	}
	if policy.IncludeContractEvidence {
		status.ContractEvidence = &ContractEvidence{
			InputHash: invocation.InputHash, ArtifactContentHash: invocation.ArtifactContentHash,
			ContractDialect: ContractDialectV1, Normalization: invocation.Normalization,
			OutputValidated: outputValidated,
		}
	}
	if policy.IncludeUsage {
		available := false
		status.UsageAvailable = &available
	}
	_ = runErrorCode // Internal failure details never cross the service boundary.
	return status, nil
}

func (service *InvocationService) validateActivePrincipal(
	ctx context.Context, principal Principal, requiredScope string,
) error {
	var scopes []string
	err := service.Store.pool.QueryRow(ctx, `
		SELECT credential.scopes
		FROM weave_service_apps AS app
		JOIN weave_service_app_credentials AS credential
		  ON credential.workspace_id=app.workspace_id AND credential.app_id=app.id
		WHERE app.workspace_id=$1 AND app.id=$2 AND app.enabled=true
		  AND credential.id=$3 AND credential.revoked_at IS NULL
		  AND (credential.expires_at IS NULL OR credential.expires_at>$4)
	`, principal.WorkspaceID, principal.AppID, principal.CredentialID,
		service.Store.clock.Now().UTC()).Scan(&scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCredentialInvalid
	}
	if err != nil {
		return fmt.Errorf("validate service app principal: %w", err)
	}
	for _, scope := range scopes {
		if scope == requiredScope {
			return nil
		}
	}
	return ErrScopeDenied
}

func qualifiedInvocationColumns(alias string) string {
	columns := strings.Split(invocationColumns, ",")
	for index := range columns {
		columns[index] = alias + "." + strings.TrimSpace(columns[index])
	}
	return strings.Join(columns, ",")
}

func extractWorkflowOutput(result json.RawMessage) (json.RawMessage, error) {
	var envelope struct {
		Output json.RawMessage `json:"output"`
	}
	if err := decodeExactJSON(result, &envelope); err != nil || len(envelope.Output) == 0 {
		return nil, errors.New("workflow result envelope is invalid")
	}
	return envelope.Output, nil
}

func filterResultFields(output json.RawMessage, fields []string) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(output, &object); err != nil || object == nil {
		return nil, errors.New("workflow result is not an object")
	}
	filtered := make(map[string]json.RawMessage, len(fields))
	for _, field := range fields {
		value, present := object[field]
		if present {
			filtered[field] = value
		}
	}
	return json.Marshal(filtered)
}

func invocationExecutionStatus(taskStatus string, runStatus *string) string {
	if runStatus != nil {
		return *runStatus
	}
	switch taskStatus {
	case taskqueue.StatusQueued, taskqueue.StatusDispatched:
		return "accepted"
	case taskqueue.StatusRunning, taskqueue.StatusCancelRequested:
		return "starting"
	case taskqueue.StatusCancelled:
		return "cancelled"
	case taskqueue.StatusTimedOut:
		return "abandoned"
	case taskqueue.StatusFailed, taskqueue.StatusSuperseded, taskqueue.StatusCut:
		return "failed"
	default:
		return "unknown"
	}
}

func invocationCancellationStatus(cancelled bool, taskStatus string, runStatus *string) string {
	if !cancelled {
		return "not_requested"
	}
	if runStatus != nil {
		switch teamrun.Status(*runStatus) {
		case teamrun.StatusCancelled, teamrun.StatusAbandoned:
			return "confirmed"
		case teamrun.StatusSucceeded, teamrun.StatusFailed:
			return "not_applied"
		default:
			return "requested"
		}
	}
	switch taskStatus {
	case taskqueue.StatusCancelled, taskqueue.StatusTimedOut:
		return "confirmed"
	case taskqueue.StatusCompleted, taskqueue.StatusFailed, taskqueue.StatusSuperseded, taskqueue.StatusCut:
		return "not_applied"
	default:
		return "requested"
	}
}

func terminalExecutionStatus(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "abandoned":
		return true
	default:
		return false
	}
}
