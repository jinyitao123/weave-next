package accesschange

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/admissionfence"
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) reserve(ctx context.Context, intent Intent, validators ...Validator) (record, error) {
	intent, subject, digest, err := intent.fingerprint(ctx)
	if err != nil {
		return record{}, err
	}
	if s == nil || s.pool == nil {
		return record{}, errors.New("permission change store unavailable")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return record{}, err
	}
	defer tx.Rollback(ctx)
	if err = authorizeOperatorTx(ctx, tx, subject, intent); err != nil {
		return record{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, intent.WorkspaceID+"/access-change/"+intent.OperationID); err != nil {
		return record{}, err
	}
	existing, err := readRecordTx(ctx, tx, intent.WorkspaceID, intent.OperationID, true)
	if err == nil {
		if existing.Subject != subject || existing.Digest != digest {
			return record{}, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return record{}, err
	}
	for _, resource := range intent.Scope {
		_, err = tx.Exec(ctx, `INSERT INTO weave_access_change_resources(workspace_id,resource_kind,resource_id,resource_version,operation_id)VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, intent.WorkspaceID, resource.Kind, resource.ID, resource.Version, intent.OperationID)
		if err != nil {
			return record{}, err
		}
		var owner string
		if err = tx.QueryRow(ctx, `SELECT operation_id FROM weave_access_change_resources WHERE workspace_id=$1 AND resource_kind=$2 AND resource_id=$3 AND resource_version=$4 FOR UPDATE`, intent.WorkspaceID, resource.Kind, resource.ID, resource.Version).Scan(&owner); err != nil {
			return record{}, err
		}
		if owner != intent.OperationID {
			var state string
			if err = tx.QueryRow(ctx, `SELECT state FROM weave_access_change_operations WHERE workspace_id=$1 AND operation_id=$2`, intent.WorkspaceID, owner).Scan(&state); err != nil {
				return record{}, err
			}
			if state != "completed" {
				return record{}, &PendingError{OperationID: owner}
			}
			if _, err = tx.Exec(ctx, `UPDATE weave_access_change_resources SET operation_id=$5 WHERE workspace_id=$1 AND resource_kind=$2 AND resource_id=$3 AND resource_version=$4`, intent.WorkspaceID, resource.Kind, resource.ID, resource.Version, intent.OperationID); err != nil {
				return record{}, err
			}
		}
	}
	for _, validate := range validators {
		if validate != nil {
			if err = validate(ctx, tx); err != nil {
				return record{}, &RejectedError{Cause: err}
			}
		}
	}
	raw, _ := json.Marshal(intent)
	actor, _ := json.Marshal(subject)
	if _, err = tx.Exec(ctx, `INSERT INTO weave_access_change_operations(workspace_id,operation_id,actor_subject,request_digest,intent)VALUES($1,$2,$3,$4,$5)`, intent.WorkspaceID, intent.OperationID, string(actor), digest, string(raw)); err != nil {
		return record{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return record{}, err
	}
	return record{Intent: intent, Subject: subject, Digest: digest, Result: Result{OperationID: intent.OperationID, State: "prepared"}}, nil
}
func readRecordTx(ctx context.Context, tx pgx.Tx, ws, id string, lock bool) (record, error) {
	var r record
	var intent, actor, block, value, grant []byte
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	err := tx.QueryRow(ctx, `SELECT intent,actor_subject,request_digest,state,block_receipt,product_result,grant_receipt FROM weave_access_change_operations WHERE workspace_id=$1 AND operation_id=$2`+suffix, ws, id).Scan(&intent, &actor, &r.Digest, &r.Result.State, &block, &value, &grant)
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(intent, &r.Intent); err != nil {
		return r, err
	}
	if err = json.Unmarshal(actor, &r.Subject); err != nil {
		return r, err
	}
	r.Result.OperationID = id
	r.Result.Value = value
	if block != nil {
		r.Result.BlockReceipt = &admissionfence.Receipt{}
		if err = json.Unmarshal(block, r.Result.BlockReceipt); err != nil {
			return r, err
		}
	}
	if grant != nil {
		r.Result.GrantReceipt = &admissionfence.Receipt{}
		if err = json.Unmarshal(grant, r.Result.GrantReceipt); err != nil {
			return r, err
		}
	}
	return r, nil
}
func requireCurrentTx(ctx context.Context, tx pgx.Tx, r record) error {
	subject, err := execution.RequireSubject(ctx, r.Intent.WorkspaceID)
	if err != nil {
		return err
	}
	if subject != r.Subject {
		return execution.ErrSubjectMismatch
	}
	if err = authorizeOperatorTx(ctx, tx, subject, r.Intent); err != nil {
		return err
	}
	for _, key := range r.Intent.Scope {
		var owner string
		if err = tx.QueryRow(ctx, `SELECT operation_id FROM weave_access_change_resources WHERE workspace_id=$1 AND resource_kind=$2 AND resource_id=$3 AND resource_version=$4 FOR SHARE`, r.Intent.WorkspaceID, key.Kind, key.ID, key.Version).Scan(&owner); err != nil {
			return err
		}
		if owner != r.Intent.OperationID {
			return ErrConflict
		}
	}
	return nil
}

// AuthorizeFence is used only by the product authority callback. An operator
// cannot manufacture a Kernel grant: the exact persisted operation must own
// every resource, and Regrant requires the product change's committed result.
func (s *Store) AuthorizeFence(ctx context.Context, command admissionfence.Command) error {
	if s == nil || s.pool == nil {
		return ErrUnauthorized
	}
	suffix := "/block"
	if command.Action == admissionfence.Regrant {
		suffix = "/regrant"
	}
	if !strings.HasSuffix(command.OperationID, suffix) {
		return ErrUnauthorized
	}
	id := strings.TrimSuffix(command.OperationID, suffix)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	r, err := readRecordTx(ctx, tx, command.WorkspaceID, id, false)
	if err != nil {
		return err
	}
	if err = requireCurrentTx(ctx, tx, r); err != nil {
		return err
	}
	expected := fenceCommand(r.Intent, command.Action)
	actualDigest, err := command.Fingerprint(ctx)
	if err != nil {
		return err
	}
	expectedDigest, err := expected.Fingerprint(ctx)
	if err != nil {
		return err
	}
	if actualDigest != expectedDigest {
		return ErrConflict
	}
	if command.Action == admissionfence.Regrant && r.Result.State != "applied" && r.Result.State != "completed" {
		return ErrUnauthorized
	}
	return nil
}

func authorizeOperatorTx(ctx context.Context, tx pgx.Tx, subject execution.Subject, intent Intent) error {
	var active, administrator bool
	if subject.UserID != "" {
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_users WHERE tenant_id=$1 AND id=$2 AND NOT disabled),EXISTS(SELECT 1 FROM weave_users u WHERE u.tenant_id=$1 AND u.id=$2 AND NOT u.disabled AND (u.role IN ('admin','owner') OR EXISTS(SELECT 1 FROM weave_members m WHERE m.workspace_id=$1 AND m.user_id=u.id AND m.role='owner')))`, subject.WorkspaceID, subject.UserID).Scan(&active, &administrator)
		if err != nil {
			return err
		}
	} else if strings.HasPrefix(subject.ServiceID, "api-key:") {
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_api_keys WHERE tenant_id=$1 AND id=$2 AND (expires_at IS NULL OR expires_at>now())),EXISTS(SELECT 1 FROM weave_api_keys WHERE tenant_id=$1 AND id=$2 AND role IN ('admin','owner') AND (expires_at IS NULL OR expires_at>now()))`, subject.WorkspaceID, strings.TrimPrefix(subject.ServiceID, "api-key:")).Scan(&active, &administrator)
		if err != nil {
			return err
		}
	}
	if !active {
		return ErrUnauthorized
	}
	if administrator {
		return nil
	}
	// Ordinary users may only change their own personal credential references.
	for _, resource := range append(append([]admissionfence.Resource{}, intent.Block...), intent.Grant...) {
		encoded := resource.ID
		if resource.Kind == "credential" {
			var slot []string
			if json.Unmarshal([]byte(encoded), &slot) != nil || len(slot) != 2 {
				return ErrUnauthorized
			}
			encoded = slot[0]
		} else if resource.Kind != "credential_resource" {
			return ErrUnauthorized
		}
		var owner []string
		if json.Unmarshal([]byte(encoded), &owner) != nil || len(owner) != 5 || owner[2] != "user" || owner[3] != subject.UserID || owner[4] != "" {
			return ErrUnauthorized
		}
	}
	return nil
}

func (s *Store) saveFence(ctx context.Context, r record, receipt admissionfence.Receipt) (record, error) {
	command := fenceCommand(r.Intent, receipt.Action)
	if err := receipt.Verify(ctx, command); err != nil {
		return r, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return r, err
	}
	defer tx.Rollback(ctx)
	current, err := readRecordTx(ctx, tx, r.Intent.WorkspaceID, r.Intent.OperationID, true)
	if err != nil {
		return r, err
	}
	if current.Digest != r.Digest {
		return r, ErrConflict
	}
	if err = requireCurrentTx(ctx, tx, current); err != nil {
		return r, err
	}
	raw, _ := json.Marshal(receipt)
	if receipt.Action == admissionfence.Block {
		if current.Result.BlockReceipt != nil {
			return current, nil
		}
		if current.Result.State != "prepared" {
			return r, ErrConflict
		}
		_, err = tx.Exec(ctx, `UPDATE weave_access_change_operations SET block_receipt=$3,state='blocked',updated_at=now() WHERE workspace_id=$1 AND operation_id=$2`, r.Intent.WorkspaceID, r.Intent.OperationID, string(raw))
		current.Result.State = "blocked"
		current.Result.BlockReceipt = &receipt
	} else {
		if current.Result.GrantReceipt != nil {
			return current, nil
		}
		if current.Result.State != "applied" {
			return r, ErrConflict
		}
		_, err = tx.Exec(ctx, `UPDATE weave_access_change_operations SET grant_receipt=$3,state='completed',updated_at=now() WHERE workspace_id=$1 AND operation_id=$2`, r.Intent.WorkspaceID, r.Intent.OperationID, string(raw))
		current.Result.State = "completed"
		current.Result.GrantReceipt = &receipt
	}
	if err != nil {
		return r, err
	}
	if err = tx.Commit(ctx); err != nil {
		return r, err
	}
	return current, nil
}
func (s *Store) apply(ctx context.Context, r record, mutation Mutation) (record, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return r, err
	}
	defer tx.Rollback(ctx)
	current, err := readRecordTx(ctx, tx, r.Intent.WorkspaceID, r.Intent.OperationID, true)
	if err != nil {
		return r, err
	}
	if current.Digest != r.Digest {
		return r, ErrConflict
	}
	if current.Result.State == "applied" || current.Result.State == "completed" {
		return current, nil
	}
	if err = requireCurrentTx(ctx, tx, current); err != nil {
		return r, err
	}
	if len(r.Intent.Block) > 0 && current.Result.BlockReceipt == nil {
		return r, ErrConflict
	}
	if mutation == nil {
		return r, ErrInvalid
	}
	value, err := mutation(ctx, tx)
	if err != nil {
		return r, err
	}
	if !json.Valid(value) {
		return r, ErrInvalid
	}
	state := "applied"
	if len(r.Intent.Grant) == 0 {
		state = "completed"
	}
	_, err = tx.Exec(ctx, `UPDATE weave_access_change_operations SET product_result=$3,state=$4,updated_at=now() WHERE workspace_id=$1 AND operation_id=$2`, r.Intent.WorkspaceID, r.Intent.OperationID, string(value), state)
	if err != nil {
		return r, err
	}
	if err = tx.Commit(ctx); err != nil {
		return r, err
	}
	current.Result.State = state
	current.Result.Value = value
	return current, nil
}

// Get exposes only the same actor's persisted outcome. It does not recover or
// re-execute an operation and cannot transfer ownership to another operator.
func (s *Store) Get(ctx context.Context, workspaceID, operationID string) (Result, error) {
	if s == nil || s.pool == nil {
		return Result{}, ErrUnauthorized
	}
	subject, err := execution.RequireSubject(ctx, workspaceID)
	if err != nil {
		return Result{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	r, err := readRecordTx(ctx, tx, workspaceID, operationID, false)
	if err != nil {
		return Result{}, err
	}
	if r.Subject != subject {
		return Result{}, execution.ErrSubjectMismatch
	}
	if err = authorizeOperatorTx(ctx, tx, subject, r.Intent); err != nil {
		return Result{}, err
	}
	return r.Result, nil
}
