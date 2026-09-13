package publicationservice

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/admissionfence"
	"github.com/jinyitao123/weave/internal/kernel/revocation"
)

var _ admissionfence.Service = (*Service)(nil)

// TransitionFence serializes with physical acceptance on the resource rows.
// The product must persist its intent first; a regrant additionally requires
// the new product authorization to have committed before this call.
func (s *Service) TransitionFence(ctx context.Context, command admissionfence.Command) (admissionfence.Receipt, error) {
	if s == nil || s.pool == nil || s.authority == nil {
		return admissionfence.Receipt{}, errors.New("resource fence service unavailable")
	}
	digest, err := command.Fingerprint(ctx)
	if err != nil {
		return admissionfence.Receipt{}, err
	}
	authority, ok := s.authority.(admissionfence.Authority)
	if !ok {
		return admissionfence.Receipt{}, errors.New("resource fence authority unavailable")
	}
	var expected []admissionfence.State
	if command.Action == admissionfence.Regrant {
		resources, _ := admissionfence.Normalize(command.Resources, false)
		expected, err = s.captureFenceStates(ctx, command.WorkspaceID, resources, true)
		if err != nil {
			return admissionfence.Receipt{}, err
		}
	}
	if err = authority.AuthorizeFence(ctx, command); err != nil {
		return admissionfence.Receipt{}, err
	}
	tx, err := s.begin(ctx, command.WorkspaceID, "fence:"+command.OperationID)
	if err != nil {
		return admissionfence.Receipt{}, err
	}
	defer tx.Rollback(ctx)
	var raw, actor []byte
	var priorDigest string
	err = tx.QueryRow(ctx, `SELECT actor_subject,request_digest,receipt FROM weave_resource_admission_operations WHERE workspace_id=$1 AND operation_id=$2`, command.WorkspaceID, command.OperationID).Scan(&actor, &priorDigest, &raw)
	if err == nil {
		var subject execution.Subject
		if err = json.Unmarshal(actor, &subject); err != nil {
			return admissionfence.Receipt{}, err
		}
		current, _ := execution.RequireSubject(ctx, command.WorkspaceID)
		if subject != current {
			return admissionfence.Receipt{}, execution.ErrSubjectMismatch
		}
		if priorDigest != digest {
			return admissionfence.Receipt{}, admissionfence.ErrConflict
		}
		var receipt admissionfence.Receipt
		if err = json.Unmarshal(raw, &receipt); err != nil {
			return receipt, err
		}
		return receipt, receipt.Verify(ctx, command)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return admissionfence.Receipt{}, err
	}
	if command.Action == admissionfence.Regrant {
		// A newer revocation must invalidate an already-authorized regrant too.
		if err = checkFenceStatesTx(ctx, tx, command.WorkspaceID, expected, true); err != nil {
			return admissionfence.Receipt{}, err
		}
	}
	resources, _ := admissionfence.Normalize(command.Resources, false)
	subject, _ := execution.RequireSubject(ctx, command.WorkspaceID)
	receipt := admissionfence.Receipt{Version: admissionfence.ContractVersion, WorkspaceID: command.WorkspaceID, OperationID: command.OperationID, Digest: digest, Subject: subject, Action: command.Action}
	for _, resource := range resources {
		if err = ensureFenceTx(ctx, tx, command.WorkspaceID, resource); err != nil {
			return receipt, err
		}
		state := admissionfence.State{Resource: resource, Blocked: command.Action == admissionfence.Block}
		if err = tx.QueryRow(ctx, `UPDATE weave_resource_admission_fences SET epoch=epoch+1,blocked=$5 WHERE workspace_id=$1 AND resource_kind=$2 AND resource_id=$3 AND resource_version=$4 RETURNING epoch`, command.WorkspaceID, resource.Kind, resource.ID, resource.Version, state.Blocked).Scan(&state.Epoch); err != nil {
			return receipt, err
		}
		receipt.States = append(receipt.States, state)
	}
	actor, _ = json.Marshal(subject)
	raw, _ = json.Marshal(receipt)
	if _, err = tx.Exec(ctx, `INSERT INTO weave_resource_admission_operations(workspace_id,operation_id,actor_subject,request_digest,receipt)VALUES($1,$2,$3,$4,$5)`, command.WorkspaceID, command.OperationID, string(actor), digest, string(raw)); err != nil {
		return receipt, err
	}
	if err = tx.Commit(ctx); err != nil {
		return admissionfence.Receipt{}, err
	}
	return receipt, nil
}

// captureResourceFences runs BEFORE product authorization. Capturing after it
// would let a revoke/regrant cycle bless an authorization from an older epoch.
func (s *Service) captureResourceFences(ctx context.Context, envelope frozen.ArtifactEnvelopeV1) ([]admissionfence.State, error) {
	if s == nil || s.pool == nil || s.authority == nil {
		return nil, errors.New("resource fence service unavailable")
	}
	subject, err := execution.RequireSubject(ctx, envelope.WorkspaceID)
	if err != nil {
		return nil, err
	}
	resources, err := envelopeResources(subject, envelope)
	if err != nil {
		return nil, err
	}
	return s.captureFenceStates(ctx, subject.WorkspaceID, resources, false)
}
func (s *Service) captureFenceStates(ctx context.Context, workspaceID string, resources []admissionfence.Resource, allowBlocked bool) ([]admissionfence.State, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	states := make([]admissionfence.State, 0, len(resources))
	for _, resource := range resources {
		if err = ensureFenceTx(ctx, tx, workspaceID, resource); err != nil {
			return nil, err
		}
		state := admissionfence.State{Resource: resource}
		if err = tx.QueryRow(ctx, `SELECT epoch,blocked FROM weave_resource_admission_fences WHERE workspace_id=$1 AND resource_kind=$2 AND resource_id=$3 AND resource_version=$4`, workspaceID, resource.Kind, resource.ID, resource.Version).Scan(&state.Epoch, &state.Blocked); err != nil {
			return nil, err
		}
		if state.Blocked && !allowBlocked {
			return nil, admissionfence.ErrBlocked
		}
		states = append(states, state)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return states, nil
}
func ensureFenceTx(ctx context.Context, tx pgx.Tx, workspaceID string, r admissionfence.Resource) error {
	_, err := tx.Exec(ctx, `INSERT INTO weave_resource_admission_fences(workspace_id,resource_kind,resource_id,resource_version)VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, workspaceID, r.Kind, r.ID, r.Version)
	return err
}

// checkResourceFencesTx retains shared locks through the snapshot/task commit.
// A completed Block cannot be overtaken by an already-authorized admission.
func checkResourceFencesTx(ctx context.Context, tx pgx.Tx, workspaceID string, states []admissionfence.State) error {
	return checkFenceStatesTx(ctx, tx, workspaceID, states, false)
}
func checkFenceStatesTx(ctx context.Context, tx pgx.Tx, workspaceID string, states []admissionfence.State, allowBlocked bool) error {
	if len(states) == 0 {
		return admissionfence.ErrInvalid
	}
	lock := " FOR SHARE"
	if allowBlocked {
		// Regrant will update these rows; lock exclusively now to avoid two
		// simultaneous grants both upgrading a shared lock.
		lock = " FOR UPDATE"
	}
	for _, expected := range states {
		var epoch int64
		var blocked bool
		r := expected.Resource
		if err := tx.QueryRow(ctx, `SELECT epoch,blocked FROM weave_resource_admission_fences WHERE workspace_id=$1 AND resource_kind=$2 AND resource_id=$3 AND resource_version=$4`+lock, workspaceID, r.Kind, r.ID, r.Version).Scan(&epoch, &blocked); err != nil {
			return err
		}
		if blocked && !allowBlocked {
			return admissionfence.ErrBlocked
		}
		if epoch != expected.Epoch {
			return admissionfence.ErrChanged
		}
	}
	return nil
}
func envelopeResources(subject execution.Subject, envelope frozen.ArtifactEnvelopeV1) ([]admissionfence.Resource, error) {
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return nil, err
	}
	resources := []admissionfence.Resource{admissionfence.Actor(subject), admissionfence.Team(payload.Team.TeamID), admissionfence.Workflow(envelope.WorkflowID, envelope.WorkflowVersion)}
	if subject.UserID != "" {
		resources = append(resources, admissionfence.Member(subject.UserID))
	}
	references, err := revocation.ExtractGraphReferences(payload.GraphDefinition)
	if err != nil {
		return nil, err
	}
	for _, reference := range references {
		r := admissionfence.Worker(payload.Team.TeamID, reference.WorkerAgentID)
		r.Version = reference.Kind
		resources = append(resources, r)
	}
	add := func(ref frozen.CredentialReference, version int64) error {
		if err := frozen.ValidateCredentialReference(ref); err != nil {
			return err
		}
		if ref.WorkspaceID != subject.WorkspaceID {
			return execution.ErrSubjectMismatch
		}
		resources = append(resources, admissionfence.CredentialResource(ref), admissionfence.Credential(ref, version))
		return nil
	}
	for _, bundle := range payload.Bundles {
		for _, ref := range bundle.Credentials {
			if err = add(ref, 0); err != nil {
				return nil, err
			}
		}
		if bundle.PrimaryModel.ProviderID != "" {
			if err = add(bundle.PrimaryModel.CredentialRef, bundle.PrimaryModel.ProviderRevision); err != nil {
				return nil, err
			}
		}
		for _, model := range bundle.FallbackModels {
			if err = add(model.CredentialRef, model.ProviderRevision); err != nil {
				return nil, err
			}
		}
		for _, binding := range bundle.MCPBindings {
			if err = add(binding.AccessRef, binding.ServerRevision); err != nil {
				return nil, err
			}
		}
		if bundle.Runtime != nil {
			if err = add(bundle.Runtime.AccessRef, bundle.Runtime.RuntimeRevision); err != nil {
				return nil, err
			}
		}
	}
	for _, target := range payload.DeliveryTargets {
		if err = add(target.AccessRef, target.TargetRevision); err != nil {
			return nil, err
		}
		for _, binding := range target.CredentialBindings {
			if err = add(binding.CredentialRef, target.TargetRevision); err != nil {
				return nil, err
			}
		}
	}
	return admissionfence.Normalize(resources, true)
}
