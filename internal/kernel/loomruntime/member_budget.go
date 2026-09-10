package loomruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// MemberBudgetPause exposes the saved mechanism boundary, independently of
// delivery acceptance. Graph errors must be handled before reading this value.
type MemberBudgetPause = execution.MemberBudgetPause

func ReadMemberBudgetPause(result *loom.RunResult, graph string) (*MemberBudgetPause, error) {
	if result == nil {
		return nil, nil
	}
	outcome, present, err := stdlib.ReadToolLoopOutcome(result.State)
	if err != nil || !present {
		return nil, err
	}
	switch outcome.Reason {
	case stdlib.ToolLoopFinalResponse, stdlib.ToolLoopToolStop, "":
		return nil, nil
	case stdlib.ToolLoopSliceLimit, stdlib.ToolLoopTotalLimit, stdlib.ToolLoopRepeatLimit, stdlib.ToolLoopProviderLength, stdlib.ToolLoopProviderStop:
	default:
		return nil, errors.New("unsupported controlled member pause")
	}
	if !result.Yielded || result.StopReason != loom.StopYielded || outcome.RunID != result.RunID {
		return nil, errors.New("controlled member pause lacks a graph yield")
	}
	seq, err := memberBudgetSeq(result.State["__checkpoint_seq"])
	if err != nil {
		return nil, err
	}
	token, _ := result.State["__yield_token"].(string)
	if token == "" || graph == "" {
		return nil, errors.New("controlled member pause identity is incomplete")
	}
	return &MemberBudgetPause{MemberRunID: result.RunID, Graph: graph, CheckpointSeq: seq, YieldToken: token,
		Slice: outcome.Slice, RoundsUsed: outcome.TotalRoundsUsed, AuthorizedTotalRounds: outcome.AuthorizedTotalRounds, Reason: string(outcome.Reason)}, nil
}

func memberBudgetSeq(value any) (int64, error) {
	raw, err := json.Marshal(value)
	var seq int64
	if err != nil || json.Unmarshal(raw, &seq) != nil || seq < 1 {
		return 0, errors.New("invalid member budget checkpoint sequence")
	}
	return seq, nil
}

type memberBudgetGrant struct {
	Grant       stdlib.ToolLoopResumeGrant `json:"grant"`
	SourceHash  string                     `json:"source_hash"`
	ParentRunID string                     `json:"parent_run_id"`
	CallID      string                     `json:"call_id"`
}

func budgetGrantNS(workspace string) string  { return "member-budget-grant:" + workspace }
func budgetGrantKey(runID, id string) string { return runID + "/" + id }

// AuthorizeMemberBudgetTx runs under the existing parent lock after all member
// attempts have stopped. The caller commits this with parent resume and enqueue.
func AuthorizeMemberBudgetTx(ctx context.Context, tx pgx.Tx, workspace, parent, call, id string, pause MemberBudgetPause, ceiling uint64) error {
	if id == "" || workspace == "" || parent == "" || call == "" {
		return errors.New("member budget authorization identity is incomplete")
	}
	var runID string
	var seq int64
	var terminal []byte
	if err := tx.QueryRow(ctx, `SELECT member_run_id,checkpoint_seq,result FROM weave_workflow_member_runs
 WHERE workspace_id=$1 AND parent_run_id=$2 AND call_id=$3 FOR UPDATE`, workspace, parent, call).Scan(&runID, &seq, &terminal); err != nil {
		return err
	}
	if runID != pause.MemberRunID || seq != pause.CheckpointSeq || len(terminal) > 0 {
		return ErrMemberIdentityConflict
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT value FROM loom_store WHERE namespace=$1 AND key=$2`, "checkpoint:"+pause.Graph, runID).Scan(&raw); err != nil {
		return err
	}
	var checkpoint memberCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return err
	}
	saved, err := ReadMemberBudgetPause(&loom.RunResult{RunID: runID, State: checkpoint.State, Yielded: true, StopReason: loom.StopYielded}, checkpoint.Graph)
	if err != nil {
		return err
	}
	if saved == nil || *saved != pause || checkpoint.Seq != seq || checkpoint.ParentRun != parent {
		return ErrMemberIdentityConflict
	}
	if ceiling == 0 {
		ceiling = pause.AuthorizedTotalRounds
	}
	grant := memberBudgetGrant{ParentRunID: parent, CallID: call, Grant: stdlib.ToolLoopResumeGrant{ID: id, ExpectedRunID: runID, ExpectedCheckpointSeq: seq, ExpectedYieldToken: pause.YieldToken, ExpectedSlice: pause.Slice, AuthorizedTotalRounds: ceiling}}
	if _, err := stdlib.PrepareToolLoopResume(checkpoint.State, grant.Grant); err != nil {
		return err
	}
	grant.SourceHash, err = frozen.HashCanonicalJSON(raw)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO loom_store(namespace,key,value,updated_at) VALUES($1,$2,$3,statement_timestamp()) ON CONFLICT(namespace,key) DO NOTHING`, budgetGrantNS(workspace), budgetGrantKey(runID, id), encoded)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("member budget authorization already exists")
	}
	return nil
}

func (member *memberExecution) prepareBudgetResumeTx(ctx context.Context, tx pgx.Tx) (*loom.RunResult, error) {
	if member.checkpointSeq == 0 {
		if member.request.ResumeGrantID != "" {
			return nil, ErrMemberIdentityConflict
		}
		return nil, nil
	}
	raw, present, err := member.runner.store.ReadValueTx(ctx, tx, "checkpoint:"+member.request.Graph.Name, member.runID)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, loom.ErrCheckpointNotFound
	}
	var cp memberCheckpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, err
	}
	if cp.RunID != member.runID || cp.Seq != member.checkpointSeq || cp.Graph != member.request.Graph.Name || cp.ParentRun != member.request.ParentRunID {
		return nil, ErrMemberIdentityConflict
	}
	result := &loom.RunResult{RunID: member.runID, State: cp.State, Yielded: true, StopReason: loom.StopYielded}
	pause, err := ReadMemberBudgetPause(result, cp.Graph)
	if err != nil {
		return nil, err
	}
	if id := member.request.ResumeGrantID; id != "" {
		grantRaw, present, err := member.runner.store.ReadValueTx(ctx, tx, budgetGrantNS(member.request.WorkspaceID), budgetGrantKey(member.runID, id))
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, ErrMemberIdentityConflict
		}
		var grant memberBudgetGrant
		if json.Unmarshal(grantRaw, &grant) != nil || grant.ParentRunID != member.request.ParentRunID || grant.CallID != member.request.CallID || grant.Grant.ID != id {
			return nil, ErrMemberIdentityConflict
		}
		_, consumed, err := member.runner.store.ReadValueTx(ctx, tx, budgetGrantNS(member.request.WorkspaceID)+":consumed", budgetGrantKey(member.runID, id))
		if err != nil {
			return nil, err
		}
		if !consumed {
			hash, err := frozen.HashCanonicalJSON(raw)
			if err != nil {
				return nil, err
			}
			if hash != grant.SourceHash {
				return nil, ErrMemberIdentityConflict
			}
			delta, err := stdlib.PrepareToolLoopResume(cp.State, grant.Grant)
			if err != nil {
				return nil, err
			}
			delta["__member_step_complete"] = true
			member.resumeDelta, member.budgetGrant = delta, &grant
			return nil, nil
		}
	}
	if pause != nil {
		return result, nil
	}
	return nil, nil
}

func (member *memberExecution) consumeBudgetGrantTx(ctx context.Context, tx pgx.Tx) error {
	grant := member.budgetGrant
	if grant == nil {
		return nil
	}
	raw, present, err := member.runner.store.ReadValueTx(ctx, tx, "checkpoint:"+member.request.Graph.Name, member.runID)
	if err != nil {
		return err
	}
	if !present {
		return loom.ErrCheckpointNotFound
	}
	hash, err := frozen.HashCanonicalJSON(raw)
	if err != nil {
		return err
	}
	if hash != grant.SourceHash {
		return ErrMemberIdentityConflict
	}
	key := budgetGrantKey(member.runID, grant.Grant.ID)
	encoded, err := json.Marshal(map[string]any{"source_hash": hash, "segment": member.segment, "attempt_generation": member.lease.AttemptGeneration})
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO loom_store(namespace,key,value,updated_at) VALUES($1,$2,$3,statement_timestamp()) ON CONFLICT(namespace,key) DO NOTHING`, budgetGrantNS(member.request.WorkspaceID)+":consumed", key, encoded)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("member budget grant already consumed")
	}
	return nil
}

// MemberSliceProgressTx is deliberately conservative: a new, nonempty file
// export acknowledged by a tool is observable progress. Ordinary tool text and
// the model's claim of progress cannot automatically authorize another slice.
// Every tool intent in this slice must have a successful, confirmed response.
func MemberSliceProgressTx(ctx context.Context, tx pgx.Tx, workspace string, pause MemberBudgetPause) (bool, error) {
	if pause.Reason != string(stdlib.ToolLoopSliceLimit) || pause.RoundsUsed >= pause.AuthorizedTotalRounds {
		return false, nil
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT value FROM loom_store WHERE namespace=$1 AND key=$2`, "checkpoint:"+pause.Graph, pause.MemberRunID).Scan(&raw); err != nil {
		return false, err
	}
	var cp memberCheckpoint
	if json.Unmarshal(raw, &cp) != nil || cp.Seq != pause.CheckpointSeq || cp.RunID != pause.MemberRunID {
		return false, ErrMemberIdentityConflict
	}
	segment, _ := cp.State["__member_step_segment"].(string)
	parts := strings.SplitN(segment, "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		return false, ErrMemberIdentityConflict
	}
	var entryRaw []byte
	if err := tx.QueryRow(ctx, `SELECT value FROM loom_store WHERE namespace=$1 AND key=$2`, "checkpoint:"+pause.Graph, pause.MemberRunID+"/"+parts[0]).Scan(&entryRaw); err != nil {
		return false, err
	}
	var entry memberCheckpoint
	if json.Unmarshal(entryRaw, &entry) != nil {
		return false, ErrMemberIdentityConflict
	}
	before, err := fileartifact.MemberFiles(entry.State)
	if err != nil {
		return false, err
	}
	known := map[fileartifact.File]bool{}
	for _, file := range before {
		known[file] = true
	}
	rows, err := tx.Query(ctx, `SELECT value FROM loom_store WHERE namespace=$1 AND starts_with(key,$2) ORDER BY key`, "member-operation:"+workspace, pause.MemberRunID+"/"+segment+"/")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	progress := false
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return false, err
		}
		var operation memberOperation
		if json.Unmarshal(data, &operation) != nil {
			return false, ErrMemberIdentityConflict
		}
		if operation.UsageIncomplete {
			return false, nil
		}
		if operation.Kind != "tool" {
			continue
		}
		var result contract.ToolResult
		if len(operation.Response) == 0 || json.Unmarshal(operation.Response, &result) != nil || result.IsError {
			return false, nil
		}
		files, present, err := fileartifact.DecodeMemberReceipt(result.Content)
		if err != nil {
			return false, err
		}
		if present {
			for _, file := range files {
				if !known[file] && strings.TrimSpace(file.Content) != "" {
					progress = true
				}
			}
		}
	}
	return progress, rows.Err()
}

// MemberBudgetCoordinator adapts the persisted member mechanism to the host's
// existing stage-resume transaction without an upward package dependency.
type MemberBudgetCoordinator struct{}

func (MemberBudgetCoordinator) Authorize(ctx context.Context, tx pgx.Tx, workspace, parent, call, id string, pause execution.MemberBudgetPause, ceiling uint64) error {
	return AuthorizeMemberBudgetTx(ctx, tx, workspace, parent, call, id, pause, ceiling)
}
func (MemberBudgetCoordinator) HasProgress(ctx context.Context, tx pgx.Tx, workspace string, pause execution.MemberBudgetPause) (bool, error) {
	return MemberSliceProgressTx(ctx, tx, workspace, pause)
}
