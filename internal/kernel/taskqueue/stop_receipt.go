package taskqueue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
)

// RecordStopReceipt atomically retains bounded, normalized failed execution
// evidence and its physical usage. It never treats evidence as completion.
func (s *Store) RecordStopReceipt(ctx context.Context, workspaceID, id, workerID string, epoch int64, receiptID, digest string, body json.RawMessage, usage *execution.TerminalUsage) error {
	subject, err := execution.RequireSubject(ctx, workspaceID)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(body)
	if receiptID == "" || len(receiptID) > 512 || len(body) == 0 || len(body) > 4<<20 || !json.Valid(body) || digest != hex.EncodeToString(hash[:]) {
		return errors.New("invalid stop evidence")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var currentEpoch, stoppedEpoch int64
	var owner, stoppedOwner string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT claim_epoch,stopped_epoch,COALESCE(worker_id,''),COALESCE(stopped_worker_id,''),actor_subject FROM weave_task_queue WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, id).Scan(&currentEpoch, &stoppedEpoch, &owner, &stoppedOwner, &raw)
	if err != nil {
		return err
	}
	var actor execution.Subject
	if json.Unmarshal(raw, &actor) != nil || actor != subject || currentEpoch != epoch || (owner != workerID && !(owner == "" && stoppedEpoch == epoch && stoppedOwner == workerID)) {
		return errors.New("stop evidence claim mismatch")
	}
	var existingID, existingDigest string
	err = tx.QueryRow(ctx, `SELECT receipt_id,receipt_digest FROM weave_task_stop_receipts WHERE workspace_id=$1 AND task_id=$2 AND claim_epoch=$3`, workspaceID, id, epoch).Scan(&existingID, &existingDigest)
	if err == nil {
		if existingID != receiptID || existingDigest != digest {
			return errors.New("conflicting stop evidence")
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO weave_task_stop_receipts(workspace_id,task_id,claim_epoch,receipt_id,receipt_digest,receipt) VALUES($1,$2,$3,$4,$5,$6)`, workspaceID, id, epoch, receiptID, digest, body); err != nil {
		return err
	}
	if err = s.recordClaimUsage(ctx, tx, id, workerID, epoch, usage); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
