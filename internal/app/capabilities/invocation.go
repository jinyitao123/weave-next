package capabilities

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/app/workflowdispatch"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/base/teamrun"
)

var requestIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// InvocationService owns the service-app submission, read and cancellation
// boundary. It creates no execution state of its own; every invocation points
// at the existing immutable snapshot, queue task and TeamRun.
type InvocationService struct {
	Store    *Store
	Dispatch *workflowdispatch.Service
	Tasks    *taskqueue.Store
	Runs     *teamrun.PGStore
	Cancel   *teamrun.CancelService
}

type admittedRelease struct {
	release    Release
	grantID    string
	grantLimit int
	appLimit   int
}

func (service *InvocationService) Submit(
	ctx context.Context,
	request SubmitInvocationRequest,
) (InvocationReceipt, error) {
	if service == nil || service.Store == nil || service.Store.pool == nil ||
		service.Dispatch == nil || service.Tasks == nil || service.Runs == nil {
		return InvocationReceipt{}, fmt.Errorf("%w: invocation dependencies are incomplete", ErrDisabled)
	}
	if err := validateSubmitRequest(request); err != nil {
		return InvocationReceipt{}, err
	}
	if !request.Principal.HasScope("invoke") {
		return InvocationReceipt{}, ErrScopeDenied
	}
	if len(request.Input) > hardMaxInputBytes {
		return InvocationReceipt{}, &ResourceLimitError{Limit: "platform_max_input_bytes"}
	}
	canonicalInput, err := frozen.CanonicalizeJSONRFC8785(request.Input)
	if err != nil {
		return InvocationReceipt{}, fmt.Errorf("%w: input must be one strict JSON value", ErrInvalid)
	}
	inputDigest := sha256.Sum256(canonicalInput)
	inputHash := hex.EncodeToString(inputDigest[:])
	fingerprint := invocationFingerprint(request, inputHash)
	invocationID, runID, taskID := invocationIDs(
		request.Principal.WorkspaceID, request.Principal.AppID, request.RequestID,
	)

	tx, err := service.Store.pool.Begin(ctx)
	if err != nil {
		return InvocationReceipt{}, fmt.Errorf("begin capability invocation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockRequestIdentity(ctx, tx, request.Principal, request.RequestID); err != nil {
		return InvocationReceipt{}, err
	}
	appLimit, err := service.lockActivePrincipalTx(ctx, tx, request.Principal, "invoke")
	if err != nil {
		return InvocationReceipt{}, err
	}
	if existing, present, err := loadInvocationByRequestTx(
		ctx, tx, request.Principal.WorkspaceID, request.Principal.AppID, request.RequestID,
	); err != nil {
		return InvocationReceipt{}, err
	} else if present {
		if existing.RequestFingerprint != fingerprint {
			return InvocationReceipt{}, ErrRequestConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return InvocationReceipt{}, fmt.Errorf("commit invocation replay: %w", err)
		}
		return receipt(existing, true), nil
	}
	var cancellationExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM weave_capability_invocation_cancellations
		WHERE workspace_id=$1 AND app_id=$2 AND request_id=$3
	)`, request.Principal.WorkspaceID, request.Principal.AppID, request.RequestID).Scan(&cancellationExists); err != nil {
		return InvocationReceipt{}, fmt.Errorf("read early capability cancellation: %w", err)
	}
	if cancellationExists {
		return InvocationReceipt{}, ErrCancelledBeforeSubmit
	}

	admitted, err := service.lockAdmittedReleaseTx(
		ctx, tx, request.Principal, request.CapabilityID, request.ReleaseVersion, appLimit,
	)
	if err != nil {
		return InvocationReceipt{}, err
	}
	if err := validateValueResources(canonicalInput, admitted.release.ExecutionLimits, false); err != nil {
		return InvocationReceipt{}, err
	}
	if err := validateInputContract(admitted.release.InputContract, canonicalInput); err != nil {
		return InvocationReceipt{}, err
	}
	if err := service.checkCapacityTx(ctx, tx, request, admitted); err != nil {
		return InvocationReceipt{}, err
	}

	workflowVersion := admitted.release.WorkflowVersion
	prepared, err := service.Dispatch.PrepareTx(ctx, tx, workflowdispatch.AdmissionRequest{
		WorkspaceID: request.Principal.WorkspaceID,
		WorkflowID:  admitted.release.WorkflowID, WorkflowVersion: &workflowVersion,
		SourceRef: invocationID, TriggerType: "api", RunID: runID,
	})
	if err != nil {
		return InvocationReceipt{}, fmt.Errorf("admit capability workflow: %w", err)
	}
	payload, err := json.Marshal(struct {
		SchemaVersion int             `json:"schema_version"`
		Kind          string          `json:"kind"`
		Input         json.RawMessage `json:"input"`
	}{1, "capability_invocation", canonicalInput})
	if err != nil {
		return InvocationReceipt{}, fmt.Errorf("encode capability queue envelope: %w", err)
	}
	if _, err := service.Dispatch.PersistTx(ctx, tx, prepared, workflowdispatch.PersistRequest{
		TaskID: taskID, TaskSource: "api", ContextKey: "capability:" + fingerprint,
		Payload: payload,
	}); err != nil {
		return InvocationReceipt{}, fmt.Errorf("persist capability workflow: %w", err)
	}

	acceptedAt := service.Store.clock.Now().UTC().Truncate(time.Microsecond)
	deadlineAt := acceptedAt.Add(time.Duration(
		admitted.release.ExecutionLimits.QueueTimeoutSeconds+
			admitted.release.ExecutionLimits.ExecutionTimeoutSeconds,
	) * time.Second)
	invocation := Invocation{
		WorkspaceID: request.Principal.WorkspaceID, AppID: request.Principal.AppID,
		InvocationID: invocationID, RequestID: request.RequestID,
		CapabilityID: request.CapabilityID, ReleaseVersion: request.ReleaseVersion,
		ReleaseID: admitted.release.ID, GrantID: admitted.grantID,
		CredentialID:        request.Principal.CredentialID,
		ArtifactContentHash: admitted.release.ArtifactContentHash,
		Normalization:       NormalizationV1, InputHash: inputHash,
		RequestFingerprint: fingerprint, ConcurrencyKey: cloneStringPointer(request.ConcurrencyKey),
		RunID: runID, TaskID: taskID, AcceptedAt: acceptedAt, DeadlineAt: deadlineAt,
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_capability_invocations(
			workspace_id,app_id,invocation_id,request_id,capability_id,release_version,
			release_id,grant_id,credential_id,artifact_content_hash,normalization,
			input_json,input_canonical,input_hash,request_fingerprint,concurrency_key,
			run_id,task_id,accepted_at,deadline_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
	`, invocation.WorkspaceID, invocation.AppID, invocation.InvocationID,
		invocation.RequestID, invocation.CapabilityID, invocation.ReleaseVersion,
		invocation.ReleaseID, invocation.GrantID, invocation.CredentialID,
		invocation.ArtifactContentHash, invocation.Normalization, canonicalInput,
		string(canonicalInput), invocation.InputHash, invocation.RequestFingerprint,
		invocation.ConcurrencyKey, invocation.RunID, invocation.TaskID,
		invocation.AcceptedAt, invocation.DeadlineAt); err != nil {
		return InvocationReceipt{}, fmt.Errorf("insert capability invocation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return InvocationReceipt{}, fmt.Errorf("commit capability invocation: %w", err)
	}
	return receipt(invocation, false), nil
}

func validateSubmitRequest(request SubmitInvocationRequest) error {
	if invalidIdentity(request.Principal.WorkspaceID) || invalidIdentity(request.Principal.AppID) ||
		invalidIdentity(request.Principal.CredentialID) || invalidIdentity(request.CapabilityID) ||
		request.ReleaseVersion < 1 || !requestIdentityPattern.MatchString(request.RequestID) ||
		len(request.Input) == 0 {
		return ErrInvalid
	}
	if request.ConcurrencyKey != nil && !requestIdentityPattern.MatchString(*request.ConcurrencyKey) {
		return ErrInvalid
	}
	return nil
}

func lockRequestIdentity(ctx context.Context, tx pgx.Tx, principal Principal, requestID string) error {
	key := strings.Join([]string{principal.WorkspaceID, principal.AppID, requestID}, "\x1f")
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
		return fmt.Errorf("lock capability request identity: %w", err)
	}
	return nil
}

func (service *InvocationService) lockActivePrincipalTx(
	ctx context.Context, tx pgx.Tx, principal Principal, requiredScope string,
) (int, error) {
	var appLimit int
	var scopes []string
	err := tx.QueryRow(ctx, `
		SELECT app.max_concurrent_invocations,credential.scopes
		FROM weave_service_apps AS app
		JOIN weave_service_app_credentials AS credential
		  ON credential.workspace_id=app.workspace_id AND credential.app_id=app.id
		WHERE app.workspace_id=$1 AND app.id=$2 AND app.enabled=true
		  AND credential.id=$3 AND credential.revoked_at IS NULL
		  AND (credential.expires_at IS NULL OR credential.expires_at>$4)
		FOR UPDATE OF app,credential
	`, principal.WorkspaceID, principal.AppID, principal.CredentialID,
		service.Store.clock.Now().UTC()).Scan(&appLimit, &scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrCredentialInvalid
	}
	if err != nil {
		return 0, fmt.Errorf("lock service app principal: %w", err)
	}
	for _, scope := range scopes {
		if scope == requiredScope {
			return appLimit, nil
		}
	}
	return 0, ErrScopeDenied
}

func (service *InvocationService) lockAdmittedReleaseTx(
	ctx context.Context,
	tx pgx.Tx,
	principal Principal,
	capabilityID string,
	releaseVersion, appLimit int,
) (admittedRelease, error) {
	var release Release
	var limitsJSON, policyJSON json.RawMessage
	var grantID string
	var grantLimit int
	err := tx.QueryRow(ctx, `
		SELECT release.workspace_id,release.capability_id,release.version,release.id,
		       release.workflow_id,release.workflow_version,release.artifact_content_hash,
		       release.contract_dialect,release.normalization,release.input_contract,
		       release.output_contract,release.input_mapping,release.execution_limits,
		       release.result_policy,release.created_by,release.created_at,
		       binding.id,binding.max_concurrent
		FROM weave_capability_releases AS release
		JOIN weave_capabilities AS capability
		  ON capability.workspace_id=release.workspace_id AND capability.id=release.capability_id
		JOIN weave_capability_release_controls AS control
		  ON control.workspace_id=release.workspace_id
		 AND control.capability_id=release.capability_id AND control.version=release.version
		JOIN weave_service_app_release_grants AS binding
		  ON binding.workspace_id=release.workspace_id
		 AND binding.capability_id=release.capability_id
		 AND binding.release_version=release.version
		WHERE release.workspace_id=$1 AND release.capability_id=$2 AND release.version=$3
		  AND binding.app_id=$4 AND binding.revoked_at IS NULL
		  AND capability.enabled=true AND control.enabled=true
		FOR SHARE OF release,capability,control,binding
	`, principal.WorkspaceID, capabilityID, releaseVersion, principal.AppID).Scan(
		&release.WorkspaceID, &release.CapabilityID, &release.Version, &release.ID,
		&release.WorkflowID, &release.WorkflowVersion, &release.ArtifactContentHash,
		&release.ContractDialect, &release.Normalization, &release.InputContract,
		&release.OutputContract, &release.InputMapping, &limitsJSON, &policyJSON,
		&release.CreatedBy, &release.CreatedAt, &grantID, &grantLimit,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return admittedRelease{}, ErrGrantUnavailable
	}
	if err != nil {
		return admittedRelease{}, fmt.Errorf("lock admitted capability release: %w", err)
	}
	if err := decodeExactJSON(limitsJSON, &release.ExecutionLimits); err != nil {
		return admittedRelease{}, fmt.Errorf("%w: execution limits are invalid", ErrReleaseUnsafe)
	}
	if err := decodeExactJSON(policyJSON, &release.ResultPolicy); err != nil {
		return admittedRelease{}, fmt.Errorf("%w: result policy is invalid", ErrReleaseUnsafe)
	}
	var inputMapping struct {
		SchemaVersion int    `json:"schema_version"`
		Mode          string `json:"mode"`
	}
	if decodeExactJSON(release.InputMapping, &inputMapping) != nil ||
		release.ContractDialect != ContractDialectV1 || release.Normalization != NormalizationV1 ||
		inputMapping.SchemaVersion != 1 || inputMapping.Mode != "direct_run_input" {
		return admittedRelease{}, ErrReleaseUnsafe
	}
	return admittedRelease{release: release, grantID: grantID, grantLimit: grantLimit, appLimit: appLimit}, nil
}

func (service *InvocationService) checkCapacityTx(
	ctx context.Context,
	tx pgx.Tx,
	request SubmitInvocationRequest,
	release admittedRelease,
) error {
	var appActive, releaseActive int
	err := tx.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE invocation.app_id=$2),
		  count(*) FILTER (WHERE invocation.app_id=$2 AND invocation.capability_id=$3
		                   AND invocation.release_version=$4)
		FROM weave_capability_invocations AS invocation
		JOIN weave_task_queue AS task
		  ON task.workspace_id=invocation.workspace_id AND task.id=invocation.task_id
		LEFT JOIN weave_team_runs AS run
		  ON run.workspace_id=invocation.workspace_id AND run.run_id=invocation.run_id
		WHERE invocation.workspace_id=$1
		  AND CASE WHEN run.run_id IS NULL
		    THEN task.status NOT IN ('completed','failed','cancelled','superseded','cut','timed_out')
		    ELSE run.status NOT IN ('succeeded','failed','cancelled','abandoned')
		  END
	`, request.Principal.WorkspaceID, request.Principal.AppID,
		request.CapabilityID, request.ReleaseVersion).Scan(&appActive, &releaseActive)
	if err != nil {
		return fmt.Errorf("count active capability invocations: %w", err)
	}
	if appActive >= release.appLimit || releaseActive >= release.grantLimit {
		return ErrCapacityExceeded
	}
	if request.ConcurrencyKey == nil {
		return nil
	}
	var busy bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1
		FROM weave_capability_invocations AS invocation
		JOIN weave_task_queue AS task
		  ON task.workspace_id=invocation.workspace_id AND task.id=invocation.task_id
		LEFT JOIN weave_team_runs AS run
		  ON run.workspace_id=invocation.workspace_id AND run.run_id=invocation.run_id
		WHERE invocation.workspace_id=$1 AND invocation.app_id=$2
		  AND invocation.capability_id=$3 AND invocation.concurrency_key=$4
		  AND CASE WHEN run.run_id IS NULL
		    THEN task.status NOT IN ('completed','failed','cancelled','superseded','cut','timed_out')
		    ELSE run.status NOT IN ('succeeded','failed','cancelled','abandoned')
		  END
	)`, request.Principal.WorkspaceID, request.Principal.AppID,
		request.CapabilityID, *request.ConcurrencyKey).Scan(&busy)
	if err != nil {
		return fmt.Errorf("check capability concurrency key: %w", err)
	}
	if busy {
		return ErrConcurrencyBusy
	}
	return nil
}

func invocationFingerprint(request SubmitInvocationRequest, inputHash string) string {
	concurrency := "null"
	if request.ConcurrencyKey != nil {
		concurrency = "string:" + *request.ConcurrencyKey
	}
	facts := strings.Join([]string{
		request.Principal.WorkspaceID, request.Principal.AppID,
		request.CapabilityID, strconv.Itoa(request.ReleaseVersion),
		NormalizationV1, inputHash, concurrency,
	}, "\x1f")
	digest := sha256.Sum256([]byte(facts))
	return hex.EncodeToString(digest[:])
}

func invocationIDs(workspaceID, appID, requestID string) (string, string, string) {
	identity := strings.Join([]string{workspaceID, appID, requestID}, "\x1f")
	return "inv-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte("capability-invocation\x1f"+identity)).String(),
		"run-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte("capability-run\x1f"+identity)).String(),
		"task-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte("capability-task\x1f"+identity)).String()
}

func receipt(invocation Invocation, replay bool) InvocationReceipt {
	return InvocationReceipt{
		Invocation: invocation, Replay: replay,
		QueryPath: "/v1/invocations/" + invocation.InvocationID,
	}
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

type invocationScanner interface{ Scan(...any) error }

func scanInvocation(row invocationScanner) (Invocation, error) {
	var invocation Invocation
	err := row.Scan(
		&invocation.WorkspaceID, &invocation.AppID, &invocation.InvocationID,
		&invocation.RequestID, &invocation.CapabilityID, &invocation.ReleaseVersion,
		&invocation.ReleaseID, &invocation.GrantID, &invocation.CredentialID,
		&invocation.ArtifactContentHash, &invocation.Normalization,
		&invocation.InputHash, &invocation.RequestFingerprint, &invocation.ConcurrencyKey,
		&invocation.RunID, &invocation.TaskID, &invocation.AcceptedAt, &invocation.DeadlineAt,
	)
	return invocation, err
}

const invocationColumns = `workspace_id,app_id,invocation_id,request_id,
	capability_id,release_version,release_id,grant_id,credential_id,
	artifact_content_hash,normalization,input_hash,request_fingerprint,concurrency_key,
	run_id,task_id,accepted_at,deadline_at`

func loadInvocationByRequestTx(
	ctx context.Context, tx pgx.Tx, workspaceID, appID, requestID string,
) (Invocation, bool, error) {
	invocation, err := scanInvocation(tx.QueryRow(ctx, `SELECT `+invocationColumns+`
		FROM weave_capability_invocations
		WHERE workspace_id=$1 AND app_id=$2 AND request_id=$3`, workspaceID, appID, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invocation{}, false, nil
	}
	if err != nil {
		return Invocation{}, false, fmt.Errorf("read capability invocation replay: %w", err)
	}
	return invocation, true, nil
}
