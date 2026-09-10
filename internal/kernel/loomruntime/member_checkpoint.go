package loomruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom"
)

type memberCheckpointStore struct{ member *memberExecution }

type memberCheckpoint struct {
	Schema     int        `json:"schema_version"`
	RunID      string     `json:"run_id"`
	Graph      string     `json:"graph"`
	Seq        int64      `json:"seq"`
	ParentRun  string     `json:"parent_run,omitempty"`
	ParentSeq  int64      `json:"parent_seq,omitempty"`
	LastStep   string     `json:"last_step"`
	State      loom.State `json:"state"`
	YieldPhase string     `json:"yield_phase"`
	SavedAt    time.Time  `json:"saved_at"`
}

func (store *memberCheckpointStore) namespace() string {
	return "checkpoint:" + store.member.request.Graph.Name
}

func (store *memberCheckpointStore) Get(ctx context.Context, ns, key string) ([]byte, error) {
	if ns != store.namespace() || (key != store.member.runID && !strings.HasPrefix(key, store.member.runID+"/")) {
		return nil, errors.New("member checkpoint locator is outside its run")
	}
	raw, present, err := store.member.runner.records.ReadValue(ctx, ns, key)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, loom.ErrCheckpointNotFound
	}
	return raw, nil
}

func (store *memberCheckpointStore) Put(ctx context.Context, ns, key string, raw []byte) error {
	member := store.member
	var checkpoint memberCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return err
	}
	if ns != store.namespace() || checkpoint.RunID != member.runID || checkpoint.Graph != member.request.Graph.Name ||
		checkpoint.Schema != loom.CurrentCheckpointSchema || checkpoint.Seq < 1 ||
		checkpoint.State["__run_id"] != member.runID {
		return errors.New("member checkpoint identity is invalid")
	}
	historyKey := fmt.Sprintf("%s/%012d", member.runID, checkpoint.Seq)
	if key != member.runID && key != historyKey {
		return errors.New("member checkpoint key is invalid")
	}
	tx, err := member.runner.store.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := member.guardTx(ctx, tx); err != nil {
		return err
	}
	if err := store.putTx(ctx, tx, ns, key, historyKey, raw, checkpoint.Seq); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if key == member.runID {
		member.checkpointSeq = checkpoint.Seq
	}
	return nil
}

func (store *memberCheckpointStore) putTx(ctx context.Context, tx pgx.Tx, ns, key, historyKey string, raw []byte, seq int64) error {
	member := store.member
	prior, present, err := member.runner.store.ReadValueTx(ctx, tx, ns, historyKey)
	if err != nil {
		return err
	}
	if present && !bytes.Equal(prior, raw) {
		return errors.New("member checkpoint immutable history conflict")
	}
	if key == historyKey {
		if !present {
			return errors.New("member history is missing its atomic latest write")
		}
		return nil
	}
	var current int64
	if err := tx.QueryRow(ctx, `SELECT checkpoint_seq FROM weave_workflow_member_runs
		WHERE workspace_id=$1 AND member_run_id=$2`, member.request.WorkspaceID, member.runID).Scan(&current); err != nil {
		return err
	}
	if seq == current && present {
		return nil
	}
	if seq != current+1 {
		return fmt.Errorf("member checkpoint sequence conflict: current=%d next=%d", current, seq)
	}
	if err := member.runner.store.PutValueTx(ctx, tx, ns, historyKey, raw); err != nil {
		return err
	}
	if err := member.runner.store.PutValueTx(ctx, tx, ns, member.runID, raw); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE weave_workflow_member_runs SET checkpoint_seq=$3,updated_at=statement_timestamp()
		WHERE workspace_id=$1 AND member_run_id=$2`, member.request.WorkspaceID, member.runID, seq)
	return err
}

// The member contract retains the whole execution history. Loom's legacy
// rolling-window eviction cannot remove evidence needed by its journal.
func (store *memberCheckpointStore) Delete(context.Context, string, string) error {
	return errors.New("member checkpoint history is immutable")
}

func (store *memberCheckpointStore) List(ctx context.Context, ns, prefix string) ([]string, error) {
	if ns != store.namespace() || !strings.HasPrefix(prefix, store.member.runID) {
		return nil, errors.New("member checkpoint listing is outside its run")
	}
	keys, err := store.member.runner.records.ListKeys(ctx, ns)
	if err != nil {
		return nil, err
	}
	selected := []string{}
	for _, key := range keys {
		if strings.HasPrefix(key, prefix) {
			selected = append(selected, key)
		}
	}
	return selected, nil
}

func (*memberCheckpointStore) Tx(context.Context, func(loom.Store) error) error {
	return errors.New("member checkpoint store does not expose nested transactions")
}

// writeBoundaryTx records a mid-step replay point together with an operation
// receipt. The state's transcript at step entry and the immutable journal
// suffice to reconstruct every model response, pending queue and tool result.
func (member *memberExecution) writeBoundaryTx(ctx context.Context, tx pgx.Tx) (int64, error) {
	var current int64
	if err := tx.QueryRow(ctx, `SELECT checkpoint_seq FROM weave_workflow_member_runs
		WHERE workspace_id=$1 AND member_run_id=$2`, member.request.WorkspaceID, member.runID).Scan(&current); err != nil {
		return 0, err
	}
	seq := current + 1
	state := cloneState(member.state)
	state["__seq"] = seq
	state["__member_journal_cursor"] = member.cursor
	checkpoint := memberCheckpoint{
		Schema: loom.CurrentCheckpointSchema, RunID: member.runID, Graph: member.request.Graph.Name,
		Seq: seq, ParentRun: member.request.ParentRunID, ParentSeq: member.request.Attribution.parentSeq.value,
		LastStep: member.step, State: state, YieldPhase: "mid_step", SavedAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		return 0, err
	}
	store := &memberCheckpointStore{member: member}
	if err := store.putTx(ctx, tx, store.namespace(), member.runID, fmt.Sprintf("%s/%012d", member.runID, seq), raw, seq); err != nil {
		return 0, err
	}
	return seq, nil
}
