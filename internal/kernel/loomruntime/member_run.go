package loomruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

var (
	ErrMemberIdentityConflict = errors.New("frozen member invocation identity conflict")
	ErrMemberBusy             = errors.New("frozen member invocation is already owned")
)

// MemberRequest contains a previously validated published graph. ParentGuard
// must lock and validate the parent's current execution epoch in the supplied
// transaction. Parent execution epochs fence owners, never logical identity.
type MemberRequest struct {
	WorkspaceID      string
	ParentRunID      string
	RunSnapshotID    string
	NodeID           string
	CallID           string
	ResumeGrantID    string
	ParentGeneration int64
	Bundle           frozen.FrozenExecutionBundle
	ArtifactHash     string
	Graph            *loom.Graph
	Input            loom.State
	Attribution      TerminalAttribution
	ParentGuard      func(context.Context, pgx.Tx) error
	// RetryableFailure is the parent's error policy. A false result permits
	// a failed member terminal; nil keeps failures available for recovery.
	RetryableFailure func(error) bool
}

type MemberRunner struct {
	records TerminalRecordStore
	store   expectedRunAdmissionTxStore
}

func NewMemberRunner(records TerminalRecordStore) (*MemberRunner, error) {
	store, ok := records.(expectedRunAdmissionTxStore)
	if !ok || records == nil {
		return nil, errors.New("member runner requires a transactional PostgreSQL record store")
	}
	return &MemberRunner{records: records, store: store}, nil
}

type memberExecution struct {
	runner        *MemberRunner
	request       MemberRequest
	runID         string
	lease         RunAttemptLease
	checkpointSeq int64
	input         loom.State
	// Journal fields are scoped to one serial graph step; replay reconstructs
	// the existing generic ToolLoop from recorded model/tool responses.
	step        string
	segment     string
	cursor      int64
	state       loom.State
	fatal       error
	resumeDelta loom.State
	budgetGrant *memberBudgetGrant
}

type memberExecutionKey struct{}

func MemberRunID(workspaceID, parentRunID, snapshotID, nodeID, callID string) string {
	encoded, _ := json.Marshal([]string{"workflow-member/1", workspaceID, parentRunID, snapshotID, nodeID, callID})
	return uuid.NewSHA1(uuid.NameSpaceOID, encoded).String()
}

func (runner *MemberRunner) admit(ctx context.Context, request MemberRequest) (*memberExecution, *loom.RunResult, error) {
	if runner == nil || request.Graph == nil || request.ParentGuard == nil ||
		request.WorkspaceID == "" || request.ParentRunID == "" || request.RunSnapshotID == "" ||
		request.NodeID == "" || request.CallID == "" || request.ArtifactHash == "" ||
		request.ParentGeneration < 0 || request.Bundle.Agent.WorkspaceID != request.WorkspaceID {
		return nil, nil, errors.New("frozen member entry is incomplete")
	}
	if request.Attribution.workspaceID != request.WorkspaceID ||
		!request.Attribution.aggregationParentRunID.present ||
		request.Attribution.aggregationParentRunID.value != request.ParentRunID {
		return nil, nil, errors.New("frozen member attribution must belong to its parent run")
	}
	runID := MemberRunID(request.WorkspaceID, request.ParentRunID, request.RunSnapshotID, request.NodeID, request.CallID)
	identity, err := json.Marshal(struct {
		Snapshot, Node, Call, Artifact string
		Bundle                         frozen.FrozenExecutionBundle
		Input                          loom.State
	}{request.RunSnapshotID, request.NodeID, request.CallID, request.ArtifactHash, request.Bundle, request.Input})
	if err != nil {
		return nil, nil, err
	}
	identityHash, err := frozen.HashCanonicalJSON(identity)
	if err != nil {
		return nil, nil, err
	}
	input := cloneState(request.Input)
	input["tenant"] = request.WorkspaceID
	input["__run_id"] = runID
	input["__run_started_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	input["__agent_id"] = request.Bundle.Agent.AgentID
	input["__agent_version"] = request.Bundle.Agent.AgentVersion
	input["__execution_scope"] = string(execution.ScopeTeamWorkerLeaf)
	input["__parent_run"] = request.ParentRunID
	input["__parent_seq"] = request.Attribution.parentSeq.value
	initializeUsageAccumulator(input)
	if err := bindTerminalAttribution(input, request.Attribution); err != nil {
		return nil, nil, err
	}
	encodedInput, err := json.Marshal(input)
	if err != nil {
		return nil, nil, err
	}
	tx, err := runner.store.BeginTx(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := request.ParentGuard(ctx, tx); err != nil {
		return nil, nil, err
	}
	// A parent row lock serializes both initial insertion and takeover.
	var savedHash string
	var savedInput, savedResult []byte
	var savedEpoch, seq int64
	err = tx.QueryRow(ctx, `SELECT identity_hash,initial_state,result,parent_generation,checkpoint_seq
		FROM weave_workflow_member_runs WHERE workspace_id=$1 AND parent_run_id=$2 AND call_id=$3 FOR UPDATE`,
		request.WorkspaceID, request.ParentRunID, request.CallID).Scan(&savedHash, &savedInput, &savedResult, &savedEpoch, &seq)
	fresh := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !fresh {
		return nil, nil, err
	}
	if !fresh {
		if savedHash != identityHash {
			return nil, nil, ErrMemberIdentityConflict
		}
		if len(savedResult) > 0 {
			var result memberStoredResult
			if err := json.Unmarshal(savedResult, &result); err != nil {
				return nil, nil, err
			}
			if result.Result.RunID != runID || (result.Result.StopReason != loom.StopCompleted && result.Error == "") {
				return nil, nil, ErrMemberIdentityConflict
			}
			if result.AuthorizationRefusal != nil && (!result.AuthorizationRefusal.Valid() || result.Error == "") {
				return nil, nil, ErrMemberIdentityConflict
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, nil, err
			}
			if result.Error != "" {
				if result.AuthorizationRefusal != nil {
					return nil, &result.Result, execution.AuthorizationRefusalError(*result.AuthorizationRefusal, errors.New(result.Error))
				}
				return nil, &result.Result, errors.New(result.Error)
			}
			return nil, &result.Result, nil
		}
		if savedEpoch >= request.ParentGeneration {
			return nil, nil, ErrMemberBusy
		}
		if err := json.Unmarshal(savedInput, &input); err != nil {
			return nil, nil, err
		}
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO weave_workflow_member_runs
			(workspace_id,parent_run_id,member_run_id,call_id,run_snapshot_id,node_id,parent_generation,identity_hash,initial_state)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, request.WorkspaceID, request.ParentRunID, runID,
			request.CallID, request.RunSnapshotID, request.NodeID, request.ParentGeneration, identityHash, string(encodedInput))
		if err != nil {
			return nil, nil, err
		}
	}
	pending := &memberExecution{runner: runner, request: request, runID: runID, checkpointSeq: seq, input: input}
	paused, err := pending.prepareBudgetResumeTx(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	if paused != nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, err
		}
		return nil, paused, nil
	}
	var lease RunAttemptLease
	if fresh {
		record := expectedRunRecordFromAttribution(runID, request.Bundle.Agent.Name, request.Attribution, time.Now().UTC())
		lease, err = AdmitFrozenAttemptTx(ctx, runner.records, tx, FrozenAttemptAdmission{
			Record: record, AttemptGeneration: 1, AttemptID: uuid.New(), GraphName: request.Graph.Name,
			RunStartedAt: input["__run_started_at"].(string), LeaseTTL: defaultAttemptHeartbeatTTL,
		})
	} else {
		if err := NewPGTerminalStateStore().LockTerminalRun(ctx, tx, request.WorkspaceID, runID); err != nil {
			return nil, nil, err
		}
		current, present, readErr := NewPGTerminalStateStore().ReadAttemptLeaseForUpdate(ctx, tx, request.WorkspaceID, runID)
		if readErr != nil {
			return nil, nil, readErr
		}
		if !present {
			return nil, nil, ErrOrphanAttemptLease
		}
		var advanced bool
		if current.State == AttemptLeaseYielded {
			lease, advanced, err = NewPGTerminalStateStore().ActivateResumeAttemptLease(ctx, tx, attemptLeaseOwner(current), uuid.New(), defaultAttemptHeartbeatTTL)
		} else {
			lease, advanced, err = AdvanceFrozenAttemptTx(ctx, tx, FrozenAttemptAdvance{
				Current: attemptLeaseOwner(current), NextGeneration: current.AttemptGeneration + 1,
				NextAttemptID: uuid.New(), LeaseTTL: defaultAttemptHeartbeatTTL,
			})
		}
		if err == nil && !advanced {
			err = ErrAttemptLeaseOwnerConflict
		}
	}
	if err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_workflow_member_runs SET parent_generation=$3,updated_at=statement_timestamp()
		WHERE workspace_id=$1 AND member_run_id=$2`, request.WorkspaceID, runID, request.ParentGeneration); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	pending.lease = lease
	return pending, nil, nil
}

// guardTx checks both owners under their row locks. The parent lock always
// precedes the member lock, including result commit and checkpoint writes.
func (member *memberExecution) guardTx(ctx context.Context, tx pgx.Tx) error {
	if err := member.request.ParentGuard(ctx, tx); err != nil {
		return err
	}
	var epoch int64
	if err := tx.QueryRow(ctx, `SELECT parent_generation FROM weave_workflow_member_runs
		WHERE workspace_id=$1 AND member_run_id=$2 FOR UPDATE`, member.request.WorkspaceID, member.runID).Scan(&epoch); err != nil {
		return err
	}
	if epoch != member.request.ParentGeneration {
		return ErrAttemptLeaseOwnerConflict
	}
	if err := NewPGTerminalStateStore().LockTerminalRun(ctx, tx, member.request.WorkspaceID, member.runID); err != nil {
		return err
	}
	lease, present, err := NewPGTerminalStateStore().ReadAttemptLeaseForUpdate(ctx, tx, member.request.WorkspaceID, member.runID)
	if err != nil {
		return err
	}
	if !present || !attemptLeaseOwnerEqual(lease, attemptLeaseOwner(member.lease)) || lease.State != AttemptLeaseActive {
		return ErrAttemptLeaseOwnerConflict
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT lease_expires_at > statement_timestamp() FROM weave_run_attempt_leases
		WHERE workspace_id=$1 AND run_id=$2`, member.request.WorkspaceID, member.runID).Scan(&active); err != nil {
		return err
	}
	if !active {
		return ErrAttemptLeaseOwnerConflict
	}
	return nil
}

func (member *memberExecution) check(ctx context.Context) error {
	tx, err := member.runner.store.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := member.guardTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (runner *MemberRunner) Run(ctx context.Context, request MemberRequest) (outcome *loom.RunResult, outcomeErr error) {
	member, completed, err := runner.admit(ctx, request)
	if err != nil || completed != nil {
		return completed, err
	}
	registry, err := NewExpectedRunRegistry(runner.records)
	if err != nil {
		return nil, err
	}
	terminalCommitted := false
	defer func() {
		if terminalCommitted {
			return
		}
		// Graph execution has returned before releasing this owner. A cancelled
		// request must still leave durable stop evidence for the parent UI.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, releaseErr := registry.(AttemptLeaseLifecycle).MarkAttemptYielded(stopCtx, attemptLeaseOwner(member.lease))
		if releaseErr != nil {
			outcomeErr = errors.Join(outcomeErr, releaseErr)
		}
	}()
	heartbeat, ok := registry.(AttemptLeaseHeartbeat)
	if !ok {
		return nil, ErrA4AttemptLeaseHeartbeatUnsupported
	}
	scope, err := startAttemptHeartbeat(ctx, heartbeat, member.lease, time.Now(), defaultAttemptHeartbeatConfig())
	if err != nil {
		return nil, err
	}
	defer scope.Stop()
	execCtx := WithUsageRunScope(context.WithValue(scope.Context(), memberExecutionKey{}, member))
	store := &memberCheckpointStore{member: member}
	var result *loom.RunResult
	if member.checkpointSeq > 0 {
		result, err = request.Graph.Resume(execCtx, member.runID, member.resumeDelta, store)
	} else {
		result, err = request.Graph.Run(execCtx, member.input, store)
	}
	if heartbeatErr := scope.Stop(); heartbeatErr != nil {
		return result, errors.Join(err, heartbeatErr)
	}
	if result == nil {
		return result, err
	}
	runErr := err
	if runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) ||
		errors.Is(runErr, ErrMemberOutcomeUnknown) || request.RetryableFailure == nil || request.RetryableFailure(runErr)) {
		return result, runErr
	}
	if runErr == nil && result.StopReason != loom.StopCompleted {
		return result, nil
	}
	saved := memberStoredResult{Result: *result}
	if runErr != nil {
		saved.Error = runErr.Error()
		if proof, trusted := execution.AuthorizationRefusalFromError(runErr); trusted {
			saved.AuthorizationRefusal = &proof
		}
	}
	encoded, err := json.Marshal(saved)
	if err != nil {
		return result, err
	}
	startedAt, err := time.Parse(time.RFC3339Nano, member.lease.RunStartedAt)
	if err != nil {
		return result, err
	}
	candidate, err := AssembleTerminalV3(TerminalAssemblyInput{
		Tenant: request.WorkspaceID, Agent: request.Bundle.Agent.Name,
		StartedAt: startedAt, EndedAt: time.Now(), Result: FromRunResult(result), RunErr: runErr, Attribution: request.Attribution,
	})
	if err != nil {
		return result, err
	}
	if incomplete, _ := result.State["__member_usage_incomplete"].(bool); incomplete {
		complete := false
		candidate.UsageComplete, candidate.UsageIncompleteReason = &complete, "member_model_response_lost"
	}
	err = commitNormalTerminalPGWithOutcome(ctx, runner.store, NormalTerminalCommit{
		Candidate: candidate, Owner: attemptLeaseOwner(member.lease),
		BeforeLock: func(ctx context.Context, tx pgx.Tx) error {
			if err := member.guardTx(ctx, tx); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE weave_workflow_member_runs SET result=$3,updated_at=statement_timestamp()
				WHERE workspace_id=$1 AND member_run_id=$2 AND result IS NULL`, request.WorkspaceID, member.runID, string(encoded))
			return err
		},
	}, nil)
	if err != nil {
		return result, &TerminalPersistenceError{Err: fmt.Errorf("commit member result: %w", err)}
	}
	terminalCommitted = true
	return result, runErr
}

type memberStoredResult struct {
	Result loom.RunResult `json:"result"`
	Error  string         `json:"error,omitempty"`
	// Terminal member replay bypasses the operation journal. Preserve the
	// trusted proof here too, rather than reconstructing one from error text.
	AuthorizationRefusal *execution.AuthorizationRefusal `json:"authorization_refusal,omitempty"`
}
