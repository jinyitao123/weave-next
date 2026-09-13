package credentials

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

type TxReferenceGate func(
	context.Context,
	pgx.Tx,
	frozen.CredentialReference,
) error

type TxProviderResolver func(
	context.Context,
	pgx.Tx,
	frozen.CredentialReference,
) (string, error)

type TxMaterialResolver func(
	context.Context,
	pgx.Tx,
	frozen.CredentialReference,
) (SecretMaterial, error)

type TxRuntimeMaterialResolver func(
	context.Context,
	pgx.Tx,
	frozen.CredentialReference,
	string,
) (SecretMaterial, error)

type TxCredentialSources struct {
	ProviderGate    TxReferenceGate
	ProviderResolve TxProviderResolver
	MCPGate         TxReferenceGate
	MCPResolve      TxMaterialResolver
	RuntimeGate     TxReferenceGate
	RuntimeResolve  TxRuntimeMaterialResolver
	DeliveryGate    TxReferenceGate
	DeliveryResolve TxMaterialResolver
}

type ResolveRequest struct {
	Reference             frozen.CredentialReference
	ExpectedRuntimeEngine string
}

type TxCredentialFacade struct {
	tx          pgx.Tx
	workspaceID string
	sources     TxCredentialSources
}

func NewTxCredentialFacade(
	tx pgx.Tx,
	workspaceID string,
	sources TxCredentialSources,
) (*TxCredentialFacade, error) {
	if nilInterface(tx) ||
		workspaceID == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		!sources.available() {
		return nil, normalizeFacadeError(nil)
	}
	return &TxCredentialFacade{
		tx:          tx,
		workspaceID: workspaceID,
		sources:     sources,
	}, nil
}

func (f *TxCredentialFacade) Validate(
	ctx context.Context,
	ref frozen.CredentialReference,
) error {
	if err := f.validateReference(ctx, ref); err != nil {
		return err
	}

	var err error
	switch ref.Kind {
	case frozen.CredentialProviderAPIKey:
		err = f.sources.ProviderGate(ctx, f.tx, ref)
	case frozen.CredentialMCPServerAccess:
		err = f.sources.MCPGate(ctx, f.tx, ref)
	case frozen.CredentialRuntimeAccess:
		err = f.sources.RuntimeGate(ctx, f.tx, ref)
	case frozen.CredentialDeliveryTargetAccess:
		err = f.sources.DeliveryGate(ctx, f.tx, ref)
	default:
		return normalizeFacadeError(nil)
	}
	if err != nil {
		return normalizeFacadeError(err)
	}
	return nil
}

func (f *TxCredentialFacade) Resolve(
	ctx context.Context,
	request ResolveRequest,
) (SecretMaterial, error) {
	ref := request.Reference
	if err := f.validateReference(ctx, ref); err != nil {
		return SecretMaterial{}, err
	}
	if ref.Kind == frozen.CredentialRuntimeAccess {
		if request.ExpectedRuntimeEngine == "" ||
			request.ExpectedRuntimeEngine != strings.TrimSpace(request.ExpectedRuntimeEngine) {
			return SecretMaterial{}, normalizeFacadeError(nil)
		}
	} else if request.ExpectedRuntimeEngine != "" {
		return SecretMaterial{}, normalizeFacadeError(nil)
	}

	var (
		material SecretMaterial
		err      error
	)
	switch ref.Kind {
	case frozen.CredentialProviderAPIKey:
		var apiKey string
		apiKey, err = f.sources.ProviderResolve(ctx, f.tx, ref)
		if err == nil {
			material = NewSecretMaterial([]byte(apiKey), nil, nil)
		}
	case frozen.CredentialMCPServerAccess:
		material, err = f.sources.MCPResolve(ctx, f.tx, ref)
	case frozen.CredentialRuntimeAccess:
		material, err = f.sources.RuntimeResolve(
			ctx,
			f.tx,
			ref,
			request.ExpectedRuntimeEngine,
		)
	case frozen.CredentialDeliveryTargetAccess:
		material, err = f.sources.DeliveryResolve(ctx, f.tx, ref)
	default:
		return SecretMaterial{}, normalizeFacadeError(nil)
	}
	if err != nil {
		return SecretMaterial{}, normalizeFacadeError(err)
	}
	return material, nil
}

func (f *TxCredentialFacade) validateReference(
	ctx context.Context,
	ref frozen.CredentialReference,
) error {
	if err := ValidateReferenceV1(ref); err != nil {
		return normalizeFacadeError(err)
	}
	if err := AuthorizeReference(ctx, ref); err != nil {
		return normalizeFacadeError(err)
	}
	if f == nil ||
		nilInterface(f.tx) ||
		f.workspaceID == "" ||
		ref.WorkspaceID != f.workspaceID ||
		!f.sources.available() {
		return normalizeFacadeError(nil)
	}
	return nil
}

func (s TxCredentialSources) available() bool {
	return s.ProviderGate != nil &&
		s.ProviderResolve != nil &&
		s.MCPGate != nil &&
		s.MCPResolve != nil &&
		s.RuntimeGate != nil &&
		s.RuntimeResolve != nil &&
		s.DeliveryGate != nil &&
		s.DeliveryResolve != nil
}

type facadeError struct {
	code  string
	cause error
}

func (e *facadeError) Error() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *facadeError) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *facadeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *facadeError) Is(target error) bool {
	if e == nil {
		return false
	}
	targetCoded, ok := target.(CodedError)
	if !ok || nilInterface(targetCoded) {
		return false
	}
	targetCode := targetCoded.Code()
	return targetCode != "" && targetCode == e.code
}

func normalizeFacadeError(cause error) error {
	code := CodeCredentialUnavailable
	foundCode := false
	foundTypedNil := false
	inspectFacadeError(cause, &code, &foundCode, &foundTypedNil, 0)
	if foundTypedNil {
		cause = nil
	}
	return &facadeError{code: code, cause: cause}
}

func inspectFacadeError(
	current error,
	code *string,
	foundCode, foundTypedNil *bool,
	depth int,
) {
	if current == nil || *foundTypedNil || depth > 100 {
		return
	}
	if nilInterface(current) {
		*foundTypedNil = true
		return
	}
	if !*foundCode {
		if codedCurrent, ok := current.(CodedError); ok &&
			!nilInterface(codedCurrent) {
			if sourceCode := codedCurrent.Code(); sourceCode != "" {
				*code = sourceCode
				*foundCode = true
			}
		}
	}
	if many, ok := current.(interface{ Unwrap() []error }); ok {
		for _, child := range many.Unwrap() {
			inspectFacadeError(
				child,
				code,
				foundCode,
				foundTypedNil,
				depth+1,
			)
			if *foundTypedNil {
				return
			}
		}
		return
	}
	if one, ok := current.(interface{ Unwrap() error }); ok {
		inspectFacadeError(
			one.Unwrap(),
			code,
			foundCode,
			foundTypedNil,
			depth+1,
		)
	}
}
