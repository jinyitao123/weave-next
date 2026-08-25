package credentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

const CodeCredentialVersionUnsupported = "workflow_credential_version_unsupported"

const redactedSecret = "[REDACTED_SECRET]"

var (
	ErrCredentialVersionUnsupported = &Error{code: CodeCredentialVersionUnsupported}
	ErrSecretMaterialSerialization  = errors.New("secret material serialization is forbidden")
)

type CodedError interface {
	error
	Code() string
}

type TxReferenceSource interface {
	ValidateReferenceTx(context.Context, pgx.Tx, frozen.CredentialReference) error
}

type referenceSourceError struct {
	code  string
	cause error
}

func (e *referenceSourceError) Error() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *referenceSourceError) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *referenceSourceError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *referenceSourceError) Is(target error) bool {
	if e == nil {
		return false
	}
	targetCoded, ok := target.(CodedError)
	return ok && targetCoded.Code() != "" && targetCoded.Code() == e.code
}

type TxEncoder struct {
	tx          pgx.Tx
	workspaceID string
	sources     map[frozen.CredentialKind]TxReferenceSource
}

func NewTxEncoder(
	tx pgx.Tx,
	workspaceID string,
	sources map[frozen.CredentialKind]TxReferenceSource,
) (*TxEncoder, error) {
	if nilInterface(tx) ||
		workspaceID == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		sources == nil {
		return nil, coded(
			CodeCredentialUnavailable,
			"exact workspace, transaction, and source registry are required",
		)
	}

	copiedSources := make(map[frozen.CredentialKind]TxReferenceSource, len(sources))
	for kind, source := range sources {
		copiedSources[kind] = source
	}
	return &TxEncoder{
		tx:          tx,
		workspaceID: workspaceID,
		sources:     copiedSources,
	}, nil
}

func ValidateReferenceV1(ref frozen.CredentialReference) error {
	if ref.CredentialVersion != nil {
		return coded(
			CodeCredentialVersionUnsupported,
			"credential_reference v1 requires a null credential version",
		)
	}
	if ref.WorkspaceID == "" ||
		ref.WorkspaceID != strings.TrimSpace(ref.WorkspaceID) ||
		ref.ResourceID == "" ||
		ref.ResourceID != strings.TrimSpace(ref.ResourceID) {
		return coded(
			CodeCredentialUnavailable,
			"exact credential reference identity is required",
		)
	}
	if err := frozen.ValidateCredentialReference(ref); err != nil {
		return coded(CodeCredentialUnavailable, "credential reference is invalid")
	}
	return nil
}

func (e *TxEncoder) EncodeReference(
	ctx context.Context,
	kind, resourceID, slot string,
) (frozen.CredentialReference, error) {
	if e == nil || nilInterface(e.tx) || e.sources == nil {
		return frozen.CredentialReference{}, coded(
			CodeCredentialUnavailable,
			"credential reference encoder is unavailable",
		)
	}
	ref := frozen.CredentialReference{
		SchemaVersion:     frozen.FrozenSchemaVersion,
		WorkspaceID:       e.workspaceID,
		Kind:              frozen.CredentialKind(kind),
		ResourceID:        resourceID,
		Slot:              slot,
		CredentialVersion: nil,
	}
	if err := ValidateReferenceV1(ref); err != nil {
		return frozen.CredentialReference{}, err
	}

	source, ok := e.sources[ref.Kind]
	if !ok || nilInterface(source) {
		return frozen.CredentialReference{}, coded(
			CodeCredentialUnavailable,
			"credential reference source is unavailable",
		)
	}
	if err := source.ValidateReferenceTx(ctx, e.tx, ref); err != nil {
		return frozen.CredentialReference{}, wrapReferenceSourceError(err)
	}
	return ref, nil
}

func wrapReferenceSourceError(cause error) error {
	code := CodeCredentialUnavailable
	if nilInterface(cause) {
		return &referenceSourceError{code: code}
	}
	var codedCause CodedError
	if errors.As(cause, &codedCause) {
		if nilInterface(codedCause) {
			return &referenceSourceError{code: code}
		}
		if sourceCode := codedCause.Code(); sourceCode != "" {
			code = sourceCode
		}
	}
	return &referenceSourceError{code: code, cause: cause}
}

type SecretMaterial struct {
	state *secretMaterialState
}

type secretMaterialState struct {
	value       []byte
	headers     map[string][]byte
	environment map[string][]byte
}

func NewSecretMaterial(
	value []byte,
	headers map[string][]byte,
	environment map[string][]byte,
) SecretMaterial {
	return SecretMaterial{
		state: &secretMaterialState{
			value:       cloneSecretBytes(value),
			headers:     cloneSecretMap(headers),
			environment: cloneSecretMap(environment),
		},
	}
}

func (m SecretMaterial) Value() []byte {
	if m.state == nil {
		return nil
	}
	return cloneSecretBytes(m.state.value)
}

func (m SecretMaterial) Headers() map[string][]byte {
	if m.state == nil {
		return nil
	}
	return cloneSecretMap(m.state.headers)
}

func (m SecretMaterial) Environment() map[string][]byte {
	if m.state == nil {
		return nil
	}
	return cloneSecretMap(m.state.environment)
}

func (m SecretMaterial) Empty() bool {
	return m.state == nil ||
		len(m.state.value) == 0 &&
			len(m.state.headers) == 0 &&
			len(m.state.environment) == 0
}

func (SecretMaterial) MarshalJSON() ([]byte, error) {
	return nil, ErrSecretMaterialSerialization
}

func (SecretMaterial) MarshalText() ([]byte, error) {
	return nil, ErrSecretMaterialSerialization
}

func (SecretMaterial) String() string {
	return redactedSecret
}

func (SecretMaterial) GoString() string {
	return redactedSecret
}

func (SecretMaterial) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(redactedSecret))
}

func (SecretMaterial) LogValue() slog.Value {
	return slog.StringValue(redactedSecret)
}

func cloneSecretBytes(value []byte) []byte {
	return append([]byte(nil), value...)
}

func cloneSecretMap(value map[string][]byte) map[string][]byte {
	if value == nil {
		return nil
	}
	cloned := make(map[string][]byte, len(value))
	for key, secret := range value {
		cloned[key] = cloneSecretBytes(secret)
	}
	return cloned
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
