// Package accesschange owns durable product permission changes. Kernel fences
// remain the only authority for whether a resource can enter execution.
package accesschange

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/admissionfence"
)

var (
	ErrInvalid      = errors.New("invalid permission change")
	ErrConflict     = errors.New("permission change identity conflict")
	ErrUnauthorized = errors.New("permission change is not authorized")
)

type RejectedError struct{ Cause error }

func (e *RejectedError) Error() string {
	return "permission change was rejected before any resource was blocked"
}
func (e *RejectedError) Unwrap() error { return e.Cause }

type PendingError struct{ OperationID string }

func (e *PendingError) Error() string { return "a permission change is still being confirmed" }

// Intent is created by a server action, never bound directly from browser JSON.
// Mutation contains only the durable, non-secret desired state; SourceDigest
// binds any additional request content without persisting its secrets.
type Intent struct {
	WorkspaceID  string                    `json:"workspace_id"`
	OperationID  string                    `json:"operation_id"`
	Kind         string                    `json:"kind"`
	TargetID     string                    `json:"target_id"`
	Mutation     json.RawMessage           `json:"mutation"`
	SourceDigest string                    `json:"source_digest"`
	Block        []admissionfence.Resource `json:"block,omitempty"`
	Grant        []admissionfence.Resource `json:"grant,omitempty"`
	// Scope serializes related product mutations; it does not widen any fence.
	Scope []admissionfence.Resource `json:"scope"`
}

type Result struct {
	OperationID  string                  `json:"operation_id"`
	State        string                  `json:"state"`
	Value        json.RawMessage         `json:"value"`
	BlockReceipt *admissionfence.Receipt `json:"block_receipt,omitempty"`
	GrantReceipt *admissionfence.Receipt `json:"grant_receipt,omitempty"`
}

type Mutation func(context.Context, pgx.Tx) (json.RawMessage, error)
type Validator func(context.Context, pgx.Tx) error

type record struct {
	Intent  Intent
	Subject execution.Subject
	Digest  string
	Result  Result
}

func (i Intent) fingerprint(ctx context.Context) (Intent, execution.Subject, string, error) {
	subject, err := execution.RequireSubject(ctx, i.WorkspaceID)
	if err != nil {
		return i, subject, "", err
	}
	if i.OperationID == "" || len(i.OperationID) > 256 || strings.TrimSpace(i.OperationID) != i.OperationID || strings.ContainsRune(i.OperationID, 0) || i.Kind == "" || i.TargetID == "" || !json.Valid(i.Mutation) || len(i.Block)+len(i.Grant) == 0 {
		return i, subject, "", ErrInvalid
	}
	if len(i.SourceDigest) != 64 {
		return i, subject, "", ErrInvalid
	}
	if _, err = hex.DecodeString(i.SourceDigest); err != nil {
		return i, subject, "", ErrInvalid
	}
	if len(i.Block) > 0 {
		i.Block, err = admissionfence.Normalize(i.Block, false)
		if err != nil {
			return i, subject, "", err
		}
	}
	if len(i.Grant) > 0 {
		i.Grant, err = admissionfence.Normalize(i.Grant, false)
		if err != nil {
			return i, subject, "", err
		}
	}
	// Every exact fence key and wildcard parent also participates in product
	// serialization. Additional scope may coordinate a roster as one operation.
	all := append(append(append([]admissionfence.Resource{}, i.Scope...), i.Block...), i.Grant...)
	i.Scope, err = admissionfence.Normalize(all, true)
	if err != nil {
		return i, subject, "", err
	}
	raw, err := json.Marshal(struct {
		Subject execution.Subject `json:"subject"`
		Intent  Intent            `json:"intent"`
	}{subject, i})
	if err != nil {
		return i, subject, "", err
	}
	raw, err = frozen.CanonicalizeJSON(raw)
	if err != nil {
		return i, subject, "", err
	}
	sum := sha256.Sum256(raw)
	return i, subject, hex.EncodeToString(sum[:]), nil
}
func SourceDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	raw, err = frozen.CanonicalizeJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func fenceCommand(i Intent, action admissionfence.Action) admissionfence.Command {
	resources, suffix := i.Block, "block"
	if action == admissionfence.Regrant {
		resources, suffix = i.Grant, "regrant"
	}
	return admissionfence.Command{Version: admissionfence.ContractVersion, WorkspaceID: i.WorkspaceID, OperationID: i.OperationID + "/" + suffix, Action: action, Resources: resources}
}
