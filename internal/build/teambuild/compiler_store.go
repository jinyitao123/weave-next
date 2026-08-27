package teambuild

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrCompilerBundleInvalid          = errors.New("compiler authorization bundle is invalid")
	ErrBlueprintRevisionNotAppendSafe = errors.New("blueprint revision is not append-safe")
	ErrNoReadyOperationStep           = errors.New("no ready compiler operation step")
	ErrOperationStepLeaseLost         = errors.New("compiler operation step lease lost")
	ErrEvaluationOnlyRevision         = errors.New("evaluation-only run forbids blueprint revisions")
)

const (
	OperationStatusPending   = "pending"
	OperationStatusRunning   = "running"
	OperationStatusSucceeded = "succeeded"
	OperationStatusSkipped   = "skipped"
	OperationStatusFailed    = "failed"
)

var compilerOperationTypes = map[string]bool{
	"agent_create": true, "agent_update": true,
	"team_create": true, "team_update": true, "roster_set": true,
	"agent_graph_compile": true, "workflow_compile": true,
	"candidate_run": true, "publish": true,
}

var compilerOperationContracts = map[string]struct {
	compiler     string
	verification []string
}{
	"agent_create":        {"platform.agent.v1", []string{"agent_version_persisted"}},
	"agent_update":        {"platform.agent.v1", []string{"agent_version_persisted"}},
	"team_create":         {"platform.team.v1", []string{"team_version_persisted"}},
	"team_update":         {"platform.team.v1", []string{"team_version_persisted"}},
	"roster_set":          {"platform.roster.v1", []string{"roster_exact"}},
	"agent_graph_compile": {"platform.agent_graph.v1", []string{"agent_graph_machine_valid"}},
	"workflow_compile":    {"platform.workflow_blueprint.v1", []string{"workflow_machine_valid"}},
	"candidate_run":       {"platform.candidate.v1", []string{"candidate_terminal_consistent"}},
	"publish":             {"platform.publish.v1", []string{"published_version_exact"}},
}

var compilerFailureClasses = map[string]bool{
	"blueprint_validation_failure":   true,
	"compile_failure":                true,
	"runtime_infrastructure_failure": true,
	"business_quality_failure":       true,
	"governance_failure":             true,
	"budget_exhausted":               true,
	"cancelled":                      true,
}

var operationIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const emptyCreateBaselineHashV1 = "12c94376e5c16470d9d1fe0e7b50a652610d9e2c62716f3d38f7c74eac5a4f3f"

// CompilerAuthorizationBundle is the persistence ABI between the compiler
// and the build-control store. Raw JSON avoids a package dependency on
// teamforge while every content hash is recomputed server-side before write.
type CompilerAuthorizationBundle struct {
	RevisionNo                   int
	BlueprintJSON                json.RawMessage
	BlueprintHash                string
	ChangeSetJSON                json.RawMessage
	ChangeSetHash                string
	BaselineHash                 string
	BaselineCapturedAt           *time.Time
	TemplateGapAuthorizationJSON json.RawMessage
	TemplateGapAuthorizationHash string
	EvaluationContractHash       string
}

// BlueprintRevision is one immutable compiler authorization revision.
type BlueprintRevision struct {
	WorkspaceID                  string
	BuildRunID                   string
	RevisionNo                   int
	BlueprintJSON                json.RawMessage
	BlueprintHash                string
	ChangeSetJSON                json.RawMessage
	ChangeSetHash                string
	BaselineHash                 string
	BaselineCapturedAt           *time.Time
	WorkflowMode                 string
	TemplateGapAuthorizationJSON json.RawMessage
	TemplateGapAuthorizationHash string
	EvaluationContractHash       string
	SourceReportHash             string
	BlueprintPatchJSON           json.RawMessage
	BlueprintPatchHash           string
	CreatedAt                    time.Time
}

// CompilerRevisionAppend is the append-only persistence request for a
// report-bound business-quality revision. The platform revalidates every
// document and provenance binding inside the append transaction.
type CompilerRevisionAppend struct {
	Bundle             CompilerAuthorizationBundle
	SourceReportHash   string
	BlueprintPatchJSON json.RawMessage
	BlueprintPatchHash string
}

// CompilerBaselinePreview is the server-owned baseline binding used by the
// deterministic compiler during planning. Create mode has no snapshot or
// capture time and binds the canonical empty baseline hash.
type CompilerBaselinePreview struct {
	BaselineHash string
	Snapshot     *BaselineSnapshot
	CapturedAt   *time.Time
}

// OperationStep is one durable operation in a compiled revision DAG.
type OperationStep struct {
	WorkspaceID    string
	BuildRunID     string
	RevisionNo     int
	OperationID    string
	OperationIndex int
	OperationType  string
	Status         string
	DependsOn      []string
	InputHash      string
	LeaseOwner     string
	LeaseEpoch     int64
	LeaseUntil     *time.Time
	Attempt        int
	ErrorClass     string
	ErrorCode      string
	EvidenceJSON   json.RawMessage
	OutputHash     string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	StartedAt      *time.Time
	CompletedAt    *time.Time
}

// OperationAttempt is immutable once it leaves running. Retryable failures
// therefore preserve typed evidence without making the operation step itself
// terminal; the next claim creates a new fenced attempt on the same revision.
type OperationAttempt struct {
	WorkspaceID  string
	BuildRunID   string
	RevisionNo   int
	OperationID  string
	Attempt      int
	LeaseEpoch   int64
	WorkerID     string
	Status       string
	ErrorClass   string
	ErrorCode    string
	EvidenceJSON json.RawMessage
	OutputHash   string
	StartedAt    time.Time
	CompletedAt  *time.Time
}

type compilerChangeSetDocument struct {
	SchemaVersion int                       `json:"schema_version"`
	ChangeSetID   string                    `json:"change_set_id"`
	BaselineHash  string                    `json:"baseline_hash"`
	BlueprintHash string                    `json:"blueprint_hash"`
	Operations    []compilerChangeOperation `json:"operations"`
}

type compilerChangeOperation struct {
	OperationID     string          `json:"operation_id"`
	Type            string          `json:"type"`
	Target          string          `json:"target"`
	ExpectedVersion *int64          `json:"expected_version,omitempty"`
	Input           json.RawMessage `json:"input"`
	InputHash       string          `json:"input_hash"`
	DependsOn       []string        `json:"depends_on"`
	Compiler        string          `json:"compiler"`
	Verification    []string        `json:"verification"`
	RollbackRef     string          `json:"rollback_ref"`
}

type compilerOperationIdentity struct {
	Type            string   `json:"type"`
	Target          string   `json:"target"`
	ExpectedVersion *int64   `json:"expected_version,omitempty"`
	InputHash       string   `json:"input_hash"`
	DependsOn       []string `json:"depends_on"`
	Compiler        string   `json:"compiler"`
	Verification    []string `json:"verification"`
	RollbackRef     string   `json:"rollback_ref"`
}

type compilerChangeSetIdentity struct {
	SchemaVersion int                       `json:"schema_version"`
	BaselineHash  string                    `json:"baseline_hash"`
	BlueprintHash string                    `json:"blueprint_hash"`
	Operations    []compilerChangeOperation `json:"operations"`
}

// PersistCompilerAuthorizationBundle validates and appends one immutable
// revision while the run is still planning. The same transaction materializes
// its pending operation steps and selects compiler-v1 for this run.
func (s *Store) PersistCompilerAuthorizationBundle(
	ctx context.Context,
	workspaceID, buildRunID string,
	bundle CompilerAuthorizationBundle,
) (BlueprintRevision, error) {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(buildRunID) == "" {
		return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w: workspace_id and build_run_id are required", ErrCompilerBundleInvalid)
	}
	blueprint, changeSet, workflowMode, authorityJSON, err := validateCompilerBundleDocuments(bundle)
	if err != nil {
		return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w", err)
	}
	if !canonicalSHA256Pattern.MatchString(bundle.BaselineHash) {
		return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w: baseline_hash is required", ErrCompilerBundleInvalid)
	}
	if blueprint.Mode == ModeCreate && bundle.BaselineHash != emptyCreateBaselineHashV1 {
		return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w: create baseline_hash must bind the canonical empty baseline", ErrCompilerBundleInvalid)
	}
	if blueprint.Mode == ModeCreate && bundle.BaselineCapturedAt != nil {
		return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w: create baseline_captured_at must be empty", ErrCompilerBundleInvalid)
	}
	if blueprint.Mode == ModeOptimize {
		if bundle.BaselineCapturedAt == nil || !validUTCTimestamp(*bundle.BaselineCapturedAt) {
			return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w: optimize baseline_captured_at must be a valid UTC timestamp", ErrCompilerBundleInvalid)
		}
	}
	if !canonicalSHA256Pattern.MatchString(bundle.EvaluationContractHash) {
		return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w: evaluation_contract_hash is invalid", ErrCompilerBundleInvalid)
	}
	if bundle.RevisionNo < 1 || bundle.RevisionNo > blueprint.RevisionPolicy.MaxRevisions {
		return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w: revision_no exceeds revision policy", ErrCompilerBundleInvalid)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BlueprintRevision{}, fmt.Errorf("begin persist compiler bundle: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var runMode, status, briefHash, contractHash, currentStrategy string
	var evaluationOnly bool
	var assetScopeJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT mode, status, brief_hash, contract_hash, asset_scope_json, execution_strategy, evaluation_only
		FROM weave_team_build_runs
		WHERE workspace_id=$1 AND build_run_id=$2
		FOR UPDATE
	`, workspaceID, buildRunID).Scan(&runMode, &status, &briefHash, &contractHash, &assetScopeJSON, &currentStrategy, &evaluationOnly); errors.Is(err, pgx.ErrNoRows) {
		return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w", ErrBuildRunNotFound)
	} else if err != nil {
		return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w", err)
	}
	if status != StatusPlanning {
		return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w", ErrBuildRunNotPlanning)
	}
	if runMode != blueprint.Mode {
		return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w: blueprint mode does not match run", ErrCompilerBundleInvalid)
	}
	if contractHash != bundle.EvaluationContractHash {
		return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w: evaluation contract hash mismatch", ErrCompilerBundleInvalid)
	}
	if evaluationOnly {
		if blueprint.RevisionPolicy.MaxRevisions != 1 || bundle.RevisionNo != 1 {
			return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w: evaluation_only requires exactly one revision", ErrCompilerBundleInvalid)
		}
		for _, operation := range changeSet.Operations {
			switch operation.Type {
			case "workflow_compile", "candidate_run", "publish":
			default:
				return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w: evaluation_only forbids %s", ErrCompilerBundleInvalid, operation.Type)
			}
		}
	}
	selectedStrategy := ExecutionStrategyCompilerV1
	if currentStrategy == ExecutionStrategyTemplateInstantiate {
		selectedStrategy = ExecutionStrategyTemplateInstantiate
	}
	if err := validateCompilerChangeSetStrategy(changeSet, selectedStrategy); err != nil {
		return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w: %v", ErrCompilerBundleInvalid, err)
	}
	if workflowMode == BlueprintWorkflowDeclarativeV1 {
		// Frozen specs are trusted to come from the platform compiler entrypoint; Store only checks their hash/envelope and BuildRun binding, not machine validity.
		binding, err := declarativeWorkflowBuildBinding(changeSet)
		if err != nil {
			return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w: %v", ErrCompilerBundleInvalid, err)
		}
		var runScope AssetScope
		if err := decodeJSONObject(assetScopeJSON, &runScope); err != nil {
			return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: decode run asset scope: %w", err)
		}
		if binding.BuildRunID != buildRunID || binding.BriefHash != briefHash ||
			binding.ContractHash != contractHash || binding.BaselineHash != bundle.BaselineHash ||
			!reflect.DeepEqual(binding.AssetScope, runScope) {
			return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w: frozen declarative_v1 BuildRun binding mismatch", ErrCompilerBundleInvalid)
		}
	}

	var latest int
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(revision_no),0)
		FROM weave_team_build_blueprint_revisions
		WHERE workspace_id=$1 AND build_run_id=$2
	`, workspaceID, buildRunID).Scan(&latest); err != nil {
		return BlueprintRevision{}, fmt.Errorf("derive compiler revision: %w", err)
	}
	if bundle.RevisionNo != latest+1 {
		return BlueprintRevision{}, fmt.Errorf("persist compiler bundle: %w: expected revision %d", ErrBlueprintRevisionNotAppendSafe, latest+1)
	}

	now := s.clock.Now()
	revision := BlueprintRevision{
		WorkspaceID: workspaceID, BuildRunID: buildRunID, RevisionNo: bundle.RevisionNo,
		BlueprintJSON: append(json.RawMessage(nil), bundle.BlueprintJSON...), BlueprintHash: bundle.BlueprintHash,
		ChangeSetJSON: append(json.RawMessage(nil), bundle.ChangeSetJSON...), ChangeSetHash: bundle.ChangeSetHash,
		BaselineHash: bundle.BaselineHash, BaselineCapturedAt: cloneTimePointer(bundle.BaselineCapturedAt), WorkflowMode: workflowMode,
		TemplateGapAuthorizationJSON: authorityJSON,
		TemplateGapAuthorizationHash: bundle.TemplateGapAuthorizationHash,
		EvaluationContractHash:       bundle.EvaluationContractHash, CreatedAt: now,
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_build_blueprint_revisions (
			workspace_id, build_run_id, revision_no,
			blueprint_json, blueprint_hash, change_set_json, change_set_hash,
			baseline_hash, baseline_captured_at, workflow_mode,
			template_gap_authorization_json, template_gap_authorization_hash,
			evaluation_contract_hash, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	`, workspaceID, buildRunID, bundle.RevisionNo,
		bundle.BlueprintJSON, bundle.BlueprintHash, bundle.ChangeSetJSON, bundle.ChangeSetHash,
		bundle.BaselineHash, bundle.BaselineCapturedAt, workflowMode,
		nullableRaw(authorityJSON), nullableString(bundle.TemplateGapAuthorizationHash),
		bundle.EvaluationContractHash, now); err != nil {
		return BlueprintRevision{}, fmt.Errorf("insert compiler revision: %w", err)
	}
	for index, operation := range changeSet.Operations {
		dependencies, err := json.Marshal(operation.DependsOn)
		if err != nil {
			return BlueprintRevision{}, fmt.Errorf("encode operation dependencies: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO weave_team_build_operation_steps (
				workspace_id, build_run_id, revision_no, operation_id,
				operation_index, operation_type, status, depends_on, input_hash,
				created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,'pending',$7::jsonb,$8,$9,$9)
		`, workspaceID, buildRunID, bundle.RevisionNo, operation.OperationID,
			index, operation.Type, string(dependencies), operation.InputHash, now); err != nil {
			return BlueprintRevision{}, fmt.Errorf("insert compiler operation step %q: %w", operation.OperationID, err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_team_build_runs
		SET execution_strategy=$3, updated_at=$4
		WHERE workspace_id=$1 AND build_run_id=$2 AND status='planning'
	`, workspaceID, buildRunID, selectedStrategy, now); err != nil {
		return BlueprintRevision{}, fmt.Errorf("select compiler execution strategy: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return BlueprintRevision{}, fmt.Errorf("commit compiler bundle: %w", err)
	}
	return revision, nil
}

// AppendCompilerRevisionFromPatch atomically appends one report-bound
// business revision while execution remains round_running. It carries
// forward only succeeded/skipped non-candidate operations whose complete
// content-addressed operation_id is unchanged; candidate_run and publish are
// always materialized pending for fresh execution.
func (s *Store) AppendCompilerRevisionFromPatch(
	ctx context.Context,
	workspaceID, buildRunID string,
	request CompilerRevisionAppend,
) (BlueprintRevision, error) {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(buildRunID) == "" {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: workspace_id and build_run_id are required", ErrCompilerBundleInvalid)
	}
	blueprint, changeSet, workflowMode, authorityJSON, err := validateCompilerBundleDocuments(request.Bundle)
	if err != nil {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w", err)
	}
	var patch BlueprintPatchV1
	if err := decodeJSONObject(request.BlueprintPatchJSON, &patch); err != nil {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: blueprint_patch_json: %v", ErrCompilerBundleInvalid, err)
	}
	patchHash, err := patch.BlueprintPatchHash()
	if err != nil || patchHash != request.BlueprintPatchHash {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: blueprint patch hash mismatch", ErrCompilerBundleInvalid)
	}
	if patch.SourceReportHash != request.SourceReportHash || !canonicalSHA256Pattern.MatchString(request.SourceReportHash) {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: source report binding mismatch", ErrCompilerBundleInvalid)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BlueprintRevision{}, fmt.Errorf("begin append compiler revision: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var runMode, status, contractHash, strategy string
	var evaluationOnly bool
	if err := tx.QueryRow(ctx, `
		SELECT mode, status, contract_hash, execution_strategy, evaluation_only
		FROM weave_team_build_runs
		WHERE workspace_id=$1 AND build_run_id=$2
		FOR UPDATE
	`, workspaceID, buildRunID).Scan(&runMode, &status, &contractHash, &strategy, &evaluationOnly); errors.Is(err, pgx.ErrNoRows) {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w", ErrBuildRunNotFound)
	} else if err != nil {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w", err)
	}
	if status != StatusRoundRunning || strategy != ExecutionStrategyCompilerV1 {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w", ErrBuildRunNotRoundRunning)
	}
	if evaluationOnly {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w", ErrEvaluationOnlyRevision)
	}
	if err := validateCompilerChangeSetStrategy(changeSet, ExecutionStrategyCompilerV1); err != nil {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: %v", ErrCompilerBundleInvalid, err)
	}

	latest, err := getLatestBlueprintRevisionTx(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w", err)
	}
	if request.Bundle.RevisionNo != latest.RevisionNo+1 {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: expected revision %d", ErrBlueprintRevisionNotAppendSafe, latest.RevisionNo+1)
	}
	var currentBlueprint TeamBlueprintV1
	if err := decodeJSONObject(latest.BlueprintJSON, &currentBlueprint); err != nil {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: latest blueprint invalid", ErrCompilerBundleInvalid)
	}
	if latest.RevisionNo >= currentBlueprint.RevisionPolicy.MaxRevisions || request.Bundle.RevisionNo > currentBlueprint.RevisionPolicy.MaxRevisions {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: revision policy exhausted", ErrBlueprintRevisionNotAppendSafe)
	}
	if err := ValidateBlueprintPatchV1(patch, currentBlueprint.RevisionPolicy); err != nil {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: %v", ErrCompilerBundleInvalid, err)
	}
	revised, err := ApplyBlueprintPatchV1(currentBlueprint, patch)
	if err != nil {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: %v", ErrCompilerBundleInvalid, err)
	}
	revisedHash, err := revised.BlueprintHash()
	if err != nil || revisedHash != request.Bundle.BlueprintHash || !reflect.DeepEqual(revised, blueprint) {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: compiled blueprint is not the exact patch result", ErrCompilerBundleInvalid)
	}
	if runMode != blueprint.Mode || contractHash != request.Bundle.EvaluationContractHash ||
		latest.EvaluationContractHash != request.Bundle.EvaluationContractHash ||
		latest.BaselineHash != request.Bundle.BaselineHash ||
		!sameOptionalTime(latest.BaselineCapturedAt, request.Bundle.BaselineCapturedAt) ||
		latest.WorkflowMode != workflowMode ||
		latest.TemplateGapAuthorizationHash != request.Bundle.TemplateGapAuthorizationHash ||
		!bytes.Equal(latest.TemplateGapAuthorizationJSON, authorityJSON) {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: revision widened or changed frozen authorization", ErrCompilerBundleInvalid)
	}

	var storedReportHash string
	var storedReportJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT report_hash, report_json
		FROM weave_team_build_run_reports
		WHERE workspace_id=$1 AND build_run_id=$2 AND round_no=$3
		FOR SHARE
	`, workspaceID, buildRunID, latest.RevisionNo).Scan(&storedReportHash, &storedReportJSON); errors.Is(err, pgx.ErrNoRows) {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: current immutable candidate report is missing", ErrCompilerBundleInvalid)
	} else if err != nil {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w", err)
	}
	var storedReport EvaluationReport
	if err := json.Unmarshal(storedReportJSON, &storedReport); err != nil {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: stored report is invalid", ErrCompilerBundleInvalid)
	}
	recomputedReportHash, err := storedReport.Hash()
	if err != nil || storedReportHash != request.SourceReportHash || recomputedReportHash != request.SourceReportHash {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: source report is not the current immutable report", ErrCompilerBundleInvalid)
	}
	var candidateEvidence []byte
	if err := tx.QueryRow(ctx, `
		SELECT evidence_json
		FROM weave_team_build_operation_steps
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3
		  AND operation_type='candidate_run' AND status='failed'
		  AND error_class='business_quality_failure'
	`, workspaceID, buildRunID, latest.RevisionNo).Scan(&candidateEvidence); errors.Is(err, pgx.ErrNoRows) {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: current candidate business failure is missing", ErrCompilerBundleInvalid)
	} else if err != nil {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w", err)
	}
	var candidateResult struct {
		Conclusion      string           `json:"Conclusion"`
		FailureCategory string           `json:"FailureCategory"`
		Report          EvaluationReport `json:"Report"`
		Diagnosis       struct {
			Class          string   `json:"class"`
			RevisionAction string   `json:"revision_action"`
			EvidenceRefs   []string `json:"evidence_refs"`
		} `json:"Diagnosis"`
	}
	if err := json.Unmarshal(candidateEvidence, &candidateResult); err != nil {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: candidate evidence is invalid", ErrCompilerBundleInvalid)
	}
	candidateResult.Report.RoundNo = latest.RevisionNo
	candidateResult.Report.Conclusion = candidateResult.Conclusion
	candidateResult.Report.FailureCategory = candidateResult.FailureCategory
	candidateReportHash, err := candidateResult.Report.Hash()
	if err != nil || candidateReportHash != request.SourceReportHash {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: report does not match current candidate evidence", ErrCompilerBundleInvalid)
	}
	if candidateResult.Diagnosis.Class != "business_quality_failure" ||
		candidateResult.Diagnosis.RevisionAction != "request_blueprint_patch" {
		return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: candidate diagnosis does not authorize a business-quality patch", ErrCompilerBundleInvalid)
	}
	provableEvidence := make(map[string]struct{})
	for _, ref := range candidateResult.Diagnosis.EvidenceRefs {
		if ref = strings.TrimSpace(ref); ref != "" {
			provableEvidence[ref] = struct{}{}
		}
	}
	for _, gate := range storedReport.HardGateResults {
		if ref := strings.TrimSpace(gate.EvidenceRef); ref != "" {
			provableEvidence[ref] = struct{}{}
		}
	}
	for _, scenario := range storedReport.ScenarioResults {
		if ref := strings.TrimSpace(scenario.RunID); ref != "" {
			provableEvidence[ref] = struct{}{}
		}
		if ref := strings.TrimSpace(scenario.ArtifactRef); ref != "" {
			provableEvidence[ref] = struct{}{}
		}
	}
	for _, change := range patch.Changes {
		for _, ref := range change.EvidenceRefs {
			if _, ok := provableEvidence[strings.TrimSpace(ref)]; !ok {
				return BlueprintRevision{}, fmt.Errorf("append compiler revision: %w: patch evidence ref %q is not provable from the candidate diagnosis or report", ErrCompilerBundleInvalid, ref)
			}
		}
	}

	now := s.clock.Now()
	revision := BlueprintRevision{
		WorkspaceID: workspaceID, BuildRunID: buildRunID, RevisionNo: request.Bundle.RevisionNo,
		BlueprintJSON: append(json.RawMessage(nil), request.Bundle.BlueprintJSON...), BlueprintHash: request.Bundle.BlueprintHash,
		ChangeSetJSON: append(json.RawMessage(nil), request.Bundle.ChangeSetJSON...), ChangeSetHash: request.Bundle.ChangeSetHash,
		BaselineHash: request.Bundle.BaselineHash, BaselineCapturedAt: cloneTimePointer(request.Bundle.BaselineCapturedAt),
		WorkflowMode: workflowMode, TemplateGapAuthorizationJSON: append(json.RawMessage(nil), authorityJSON...),
		TemplateGapAuthorizationHash: request.Bundle.TemplateGapAuthorizationHash,
		EvaluationContractHash:       request.Bundle.EvaluationContractHash,
		SourceReportHash:             request.SourceReportHash, BlueprintPatchJSON: append(json.RawMessage(nil), request.BlueprintPatchJSON...),
		BlueprintPatchHash: request.BlueprintPatchHash, CreatedAt: now,
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_build_blueprint_revisions (
			workspace_id, build_run_id, revision_no,
			blueprint_json, blueprint_hash, change_set_json, change_set_hash,
			baseline_hash, baseline_captured_at, workflow_mode,
			template_gap_authorization_json, template_gap_authorization_hash,
			evaluation_contract_hash, source_report_hash,
			blueprint_patch_json, blueprint_patch_hash, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
	`, workspaceID, buildRunID, revision.RevisionNo,
		revision.BlueprintJSON, revision.BlueprintHash, revision.ChangeSetJSON, revision.ChangeSetHash,
		revision.BaselineHash, revision.BaselineCapturedAt, revision.WorkflowMode,
		nullableRaw(revision.TemplateGapAuthorizationJSON), nullableString(revision.TemplateGapAuthorizationHash),
		revision.EvaluationContractHash, revision.SourceReportHash,
		revision.BlueprintPatchJSON, revision.BlueprintPatchHash, now); err != nil {
		return BlueprintRevision{}, fmt.Errorf("insert compiler patch revision: %w", err)
	}
	for index, operation := range changeSet.Operations {
		dependencies, err := json.Marshal(operation.DependsOn)
		if err != nil {
			return BlueprintRevision{}, fmt.Errorf("encode operation dependencies: %w", err)
		}
		status := OperationStatusPending
		var evidence any
		var outputHash any
		var startedAt any
		var completedAt any
		if operation.Type != "candidate_run" && operation.Type != "publish" {
			var previousStatus, previousOutput string
			err := tx.QueryRow(ctx, `
				SELECT status, output_hash
				FROM weave_team_build_operation_steps
				WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3 AND operation_id=$4
				  AND status IN ('succeeded','skipped')
			`, workspaceID, buildRunID, latest.RevisionNo, operation.OperationID).Scan(&previousStatus, &previousOutput)
			if err == nil {
				status = OperationStatusSkipped
				evidence = fmt.Sprintf(`{"reused_from_revision":%d,"source_operation_id":%q,"source_status":%q}`,
					latest.RevisionNo, operation.OperationID, previousStatus)
				outputHash = previousOutput
				startedAt, completedAt = now, now
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return BlueprintRevision{}, fmt.Errorf("inspect reusable compiler operation: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO weave_team_build_operation_steps (
				workspace_id, build_run_id, revision_no, operation_id,
				operation_index, operation_type, status, depends_on, input_hash,
				evidence_json, output_hash, created_at, updated_at, started_at, completed_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10::jsonb,$11,$12,$12,$13,$14)
		`, workspaceID, buildRunID, revision.RevisionNo, operation.OperationID,
			index, operation.Type, status, string(dependencies), operation.InputHash,
			evidence, outputHash, now, startedAt, completedAt); err != nil {
			return BlueprintRevision{}, fmt.Errorf("insert compiler patch operation step %q: %w", operation.OperationID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return BlueprintRevision{}, fmt.Errorf("commit compiler patch revision: %w", err)
	}
	return revision, nil
}

func sameOptionalTime(first, second *time.Time) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	return first.Equal(*second)
}

func getLatestBlueprintRevisionTx(ctx context.Context, tx pgx.Tx, workspaceID, buildRunID string) (BlueprintRevision, error) {
	var revision BlueprintRevision
	var baselineHash, authorityHash, sourceReportHash, patchHash *string
	var authorityRaw, patchRaw []byte
	err := tx.QueryRow(ctx, `
		SELECT workspace_id, build_run_id, revision_no,
			blueprint_json, blueprint_hash, change_set_json, change_set_hash,
			baseline_hash, baseline_captured_at, workflow_mode,
			template_gap_authorization_json, template_gap_authorization_hash,
			evaluation_contract_hash, source_report_hash,
			blueprint_patch_json, blueprint_patch_hash, created_at
		FROM weave_team_build_blueprint_revisions
		WHERE workspace_id=$1 AND build_run_id=$2
		ORDER BY revision_no DESC LIMIT 1
		FOR UPDATE
	`, workspaceID, buildRunID).Scan(
		&revision.WorkspaceID, &revision.BuildRunID, &revision.RevisionNo,
		&revision.BlueprintJSON, &revision.BlueprintHash, &revision.ChangeSetJSON, &revision.ChangeSetHash,
		&baselineHash, &revision.BaselineCapturedAt, &revision.WorkflowMode, &authorityRaw, &authorityHash,
		&revision.EvaluationContractHash, &sourceReportHash, &patchRaw, &patchHash, &revision.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return BlueprintRevision{}, ErrBuildRunNotFound
	}
	if err != nil {
		return BlueprintRevision{}, err
	}
	if baselineHash != nil {
		revision.BaselineHash = *baselineHash
	}
	if authorityHash != nil {
		revision.TemplateGapAuthorizationHash = *authorityHash
	}
	if sourceReportHash != nil {
		revision.SourceReportHash = *sourceReportHash
	}
	if patchHash != nil {
		revision.BlueprintPatchHash = *patchHash
	}
	revision.TemplateGapAuthorizationJSON = append(json.RawMessage(nil), authorityRaw...)
	revision.BlueprintPatchJSON = append(json.RawMessage(nil), patchRaw...)
	return revision, nil
}

func validUTCTimestamp(value time.Time) bool {
	// PostgreSQL timestamptz stores an absolute instant and pgx may attach the
	// session location when scanning it. Callers normalize with UTC() before
	// hashing; a non-zero instant is therefore the durable validity condition.
	return !value.IsZero()
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func validateCompilerBundleDocuments(bundle CompilerAuthorizationBundle) (
	TeamBlueprintV1, compilerChangeSetDocument, string, json.RawMessage, error,
) {
	var blueprint TeamBlueprintV1
	if err := decodeJSONObject(bundle.BlueprintJSON, &blueprint); err != nil {
		return TeamBlueprintV1{}, compilerChangeSetDocument{}, "", nil, fmt.Errorf("%w: blueprint_json: %v", ErrCompilerBundleInvalid, err)
	}
	blueprintHash, err := blueprint.BlueprintHash()
	if err != nil {
		return TeamBlueprintV1{}, compilerChangeSetDocument{}, "", nil, fmt.Errorf("%w: %v", ErrCompilerBundleInvalid, err)
	}
	if blueprintHash != bundle.BlueprintHash {
		return TeamBlueprintV1{}, compilerChangeSetDocument{}, "", nil, fmt.Errorf("%w: blueprint hash mismatch", ErrCompilerBundleInvalid)
	}

	changeSet, canonicalChangeSetHash, err := validateCompilerChangeSet(bundle.ChangeSetJSON)
	if err != nil {
		return TeamBlueprintV1{}, compilerChangeSetDocument{}, "", nil, fmt.Errorf("%w: %v", ErrCompilerBundleInvalid, err)
	}
	if canonicalChangeSetHash != bundle.ChangeSetHash {
		return TeamBlueprintV1{}, compilerChangeSetDocument{}, "", nil, fmt.Errorf("%w: change set hash mismatch", ErrCompilerBundleInvalid)
	}
	if changeSet.BlueprintHash != bundle.BlueprintHash {
		return TeamBlueprintV1{}, compilerChangeSetDocument{}, "", nil, fmt.Errorf("%w: change set blueprint binding mismatch", ErrCompilerBundleInvalid)
	}
	if changeSet.BaselineHash != bundle.BaselineHash {
		return TeamBlueprintV1{}, compilerChangeSetDocument{}, "", nil, fmt.Errorf("%w: change set baseline binding mismatch", ErrCompilerBundleInvalid)
	}

	workflowMode := blueprint.Workflow.Mode
	var authorityJSON json.RawMessage
	switch workflowMode {
	case BlueprintWorkflowTemplate:
		if len(bundle.TemplateGapAuthorizationJSON) != 0 || bundle.TemplateGapAuthorizationHash != "" {
			return TeamBlueprintV1{}, compilerChangeSetDocument{}, "", nil, fmt.Errorf("%w: template mode forbids template-gap authority", ErrCompilerBundleInvalid)
		}
	case BlueprintWorkflowCustom:
		var fact TemplateGapAuthorizationV1
		if err := decodeJSONObject(bundle.TemplateGapAuthorizationJSON, &fact); err != nil {
			return TeamBlueprintV1{}, compilerChangeSetDocument{}, "", nil, fmt.Errorf("%w: invalid template-gap authority: %v", ErrCompilerBundleInvalid, err)
		}
		if blueprint.Workflow.TemplateGapAuthorization == nil || !reflect.DeepEqual(fact, *blueprint.Workflow.TemplateGapAuthorization) {
			return TeamBlueprintV1{}, compilerChangeSetDocument{}, "", nil, fmt.Errorf("%w: template-gap authority scope mismatch", ErrCompilerBundleInvalid)
		}
		authorityHash, err := canonicalJSONObjectHash(bundle.TemplateGapAuthorizationJSON)
		if err != nil || authorityHash != bundle.TemplateGapAuthorizationHash {
			return TeamBlueprintV1{}, compilerChangeSetDocument{}, "", nil, fmt.Errorf("%w: template-gap authority hash mismatch", ErrCompilerBundleInvalid)
		}
		authorityJSON = append(json.RawMessage(nil), bundle.TemplateGapAuthorizationJSON...)
	case BlueprintWorkflowDeclarativeV1:
		if len(bundle.TemplateGapAuthorizationJSON) != 0 || bundle.TemplateGapAuthorizationHash != "" {
			return TeamBlueprintV1{}, compilerChangeSetDocument{}, "", nil, fmt.Errorf("%w: declarative_v1 mode forbids template-gap authority", ErrCompilerBundleInvalid)
		}
		if err := validateDeclarativeWorkflowOperationBinding(blueprint, changeSet, bundle); err != nil {
			return TeamBlueprintV1{}, compilerChangeSetDocument{}, "", nil, fmt.Errorf("%w: %v", ErrCompilerBundleInvalid, err)
		}
	default:
		return TeamBlueprintV1{}, compilerChangeSetDocument{}, "", nil, fmt.Errorf("%w: unknown workflow mode", ErrCompilerBundleInvalid)
	}
	return blueprint, changeSet, workflowMode, authorityJSON, nil
}

func validateDeclarativeWorkflowOperationBinding(
	blueprint TeamBlueprintV1,
	changeSet compilerChangeSetDocument,
	bundle CompilerAuthorizationBundle,
) error {
	binding, err := declarativeWorkflowBuildBinding(changeSet)
	if err != nil {
		return err
	}
	if binding.SourceSpecHash != blueprint.Workflow.DeclarativeSpecHash ||
		binding.ContractHash != bundle.EvaluationContractHash ||
		binding.BaselineHash != bundle.BaselineHash {
		return errors.New("declarative_v1 workflow input binding is invalid")
	}
	return nil
}

type compilerDeclarativeBuildBinding struct {
	SourceSpecHash string
	SpecHash       string
	BuildRunID     string
	BriefHash      string
	ContractHash   string
	AssetScope     AssetScope
	BaselineHash   string
}

func declarativeWorkflowBuildBinding(changeSet compilerChangeSetDocument) (compilerDeclarativeBuildBinding, error) {
	var workflowOperation *compilerChangeOperation
	for index := range changeSet.Operations {
		operation := &changeSet.Operations[index]
		if operation.Type != "workflow_compile" {
			continue
		}
		if workflowOperation != nil {
			return compilerDeclarativeBuildBinding{}, errors.New("declarative_v1 change set has multiple workflow_compile operations")
		}
		workflowOperation = operation
	}
	if workflowOperation == nil {
		return compilerDeclarativeBuildBinding{}, errors.New("declarative_v1 change set requires a workflow_compile operation")
	}
	if workflowOperation.Compiler != "platform.workflow_declarative.v1" {
		return compilerDeclarativeBuildBinding{}, errors.New("declarative_v1 workflow_compile uses the wrong compiler")
	}
	var input struct {
		Mode           string          `json:"mode"`
		SourceSpecHash string          `json:"source_spec_hash"`
		SpecHash       string          `json:"spec_hash"`
		CompiledHash   string          `json:"compiled_hash"`
		FrozenSpec     json.RawMessage `json:"frozen_spec"`
	}
	if err := decodeJSONObject(workflowOperation.Input, &input); err != nil {
		return compilerDeclarativeBuildBinding{}, fmt.Errorf("declarative_v1 workflow input is invalid: %w", err)
	}
	if input.Mode != BlueprintWorkflowDeclarativeV1 ||
		!canonicalSHA256Pattern.MatchString(input.SpecHash) ||
		!canonicalSHA256Pattern.MatchString(input.CompiledHash) ||
		len(input.FrozenSpec) == 0 {
		return compilerDeclarativeBuildBinding{}, errors.New("declarative_v1 workflow input binding is invalid")
	}
	sourceSpecHash := input.SourceSpecHash
	if sourceSpecHash == "" {
		// Compatibility for unchanged declarative revisions compiled before
		// AgentVersion rebinding introduced a distinct effective spec hash.
		sourceSpecHash = input.SpecHash
	}
	if !canonicalSHA256Pattern.MatchString(sourceSpecHash) {
		return compilerDeclarativeBuildBinding{}, errors.New("declarative_v1 source spec binding is invalid")
	}
	var frozen struct {
		SchemaVersion int    `json:"schema_version"`
		SpecHash      string `json:"spec_hash"`
		BuildBinding  struct {
			BuildRunID   string     `json:"build_run_id"`
			BriefHash    string     `json:"brief_hash"`
			ContractHash string     `json:"contract_hash"`
			AssetScope   AssetScope `json:"asset_scope"`
			BaselineHash string     `json:"baseline_hash"`
		} `json:"build_binding"`
		WorkerBindings  json.RawMessage `json:"worker_bindings"`
		Spec            json.RawMessage `json:"spec"`
		TriggerConfig   json.RawMessage `json:"trigger_config"`
		GraphDefinition json.RawMessage `json:"graph_definition"`
	}
	if err := decodeJSONObject(input.FrozenSpec, &frozen); err != nil {
		return compilerDeclarativeBuildBinding{}, fmt.Errorf("frozen declarative_v1 spec is invalid: %w", err)
	}
	if frozen.SchemaVersion != 1 || frozen.SpecHash != input.SpecHash ||
		!canonicalSHA256Pattern.MatchString(frozen.BuildBinding.BriefHash) ||
		!canonicalSHA256Pattern.MatchString(frozen.BuildBinding.ContractHash) ||
		!canonicalSHA256Pattern.MatchString(frozen.BuildBinding.BaselineHash) ||
		strings.TrimSpace(frozen.BuildBinding.BuildRunID) == "" ||
		len(frozen.WorkerBindings) == 0 || len(frozen.Spec) == 0 ||
		len(frozen.TriggerConfig) == 0 || len(frozen.GraphDefinition) == 0 {
		return compilerDeclarativeBuildBinding{}, errors.New("frozen declarative_v1 BuildRun binding is invalid")
	}
	var frozenFields map[string]json.RawMessage
	if err := json.Unmarshal(input.FrozenSpec, &frozenFields); err != nil {
		return compilerDeclarativeBuildBinding{}, err
	}
	frozenFields["spec_hash"] = json.RawMessage(`""`)
	recomputedHash, err := hashDocument(frozenFields)
	if err != nil || recomputedHash != frozen.SpecHash {
		return compilerDeclarativeBuildBinding{}, errors.New("frozen declarative_v1 spec hash mismatch")
	}
	compiledHash, err := hashDocument(struct {
		Trigger json.RawMessage `json:"trigger_config"`
		Graph   json.RawMessage `json:"graph_definition"`
	}{frozen.TriggerConfig, frozen.GraphDefinition})
	if err != nil || compiledHash != input.CompiledHash {
		return compilerDeclarativeBuildBinding{}, errors.New("frozen declarative_v1 compiled graph hash mismatch")
	}
	return compilerDeclarativeBuildBinding{
		SourceSpecHash: sourceSpecHash,
		SpecHash:       frozen.SpecHash,
		BuildRunID:     frozen.BuildBinding.BuildRunID,
		BriefHash:      frozen.BuildBinding.BriefHash,
		ContractHash:   frozen.BuildBinding.ContractHash,
		AssetScope:     frozen.BuildBinding.AssetScope,
		BaselineHash:   frozen.BuildBinding.BaselineHash,
	}, nil
}

func validateCompilerChangeSet(raw json.RawMessage) (compilerChangeSetDocument, string, error) {
	var document compilerChangeSetDocument
	if err := decodeJSONObject(raw, &document); err != nil {
		return compilerChangeSetDocument{}, "", err
	}
	if document.SchemaVersion != 1 || !canonicalSHA256Pattern.MatchString(document.BlueprintHash) {
		return compilerChangeSetDocument{}, "", errors.New("invalid change set header")
	}
	if !canonicalSHA256Pattern.MatchString(document.BaselineHash) {
		return compilerChangeSetDocument{}, "", errors.New("invalid change set baseline_hash")
	}
	if len(document.Operations) == 0 {
		return compilerChangeSetDocument{}, "", errors.New("change set operations are required")
	}
	seen := make(map[string]bool, len(document.Operations))
	known := make(map[string]int, len(document.Operations))
	for index, operation := range document.Operations {
		known[operation.OperationID] = index
	}
	for index := range document.Operations {
		operation := &document.Operations[index]
		if !operationIDPattern.MatchString(operation.OperationID) || seen[operation.OperationID] {
			return compilerChangeSetDocument{}, "", fmt.Errorf("invalid or duplicate operation_id at index %d", index)
		}
		seen[operation.OperationID] = true
		contract, knownType := compilerOperationContracts[operation.Type]
		if !compilerOperationTypes[operation.Type] || !knownType {
			return compilerChangeSetDocument{}, "", fmt.Errorf("invalid operation type at index %d", index)
		}
		expectedCompiler := contract.compiler
		if operation.Type == "workflow_compile" &&
			(operation.Compiler == "platform.workflow_custom_ref.v1" || operation.Compiler == "platform.workflow_declarative.v1") {
			expectedCompiler = operation.Compiler
		}
		if operation.Compiler != expectedCompiler || !reflect.DeepEqual(operation.Verification, contract.verification) {
			return compilerChangeSetDocument{}, "", fmt.Errorf("invalid operation compiler contract at index %d", index)
		}
		if strings.TrimSpace(operation.Target) == "" || strings.TrimSpace(operation.RollbackRef) == "" {
			return compilerChangeSetDocument{}, "", fmt.Errorf("operation target and rollback_ref are required at index %d", index)
		}
		inputHash, err := canonicalJSONObjectHash(operation.Input)
		if err != nil || inputHash != operation.InputHash {
			return compilerChangeSetDocument{}, "", fmt.Errorf("operation input hash mismatch at index %d", index)
		}
		canonicalInput, _ := canonicalJSONObject(operation.Input)
		operation.Input = canonicalInput
		if !sort.StringsAreSorted(operation.DependsOn) || !sort.StringsAreSorted(operation.Verification) {
			return compilerChangeSetDocument{}, "", fmt.Errorf("operation lists must be canonical at index %d", index)
		}
		seenDependencies := make(map[string]bool, len(operation.DependsOn))
		for _, dependency := range operation.DependsOn {
			dependencyIndex, exists := known[dependency]
			if dependency == operation.OperationID || !exists || dependencyIndex >= index || seenDependencies[dependency] {
				return compilerChangeSetDocument{}, "", fmt.Errorf("invalid dependency at index %d", index)
			}
			seenDependencies[dependency] = true
		}
		identity := compilerOperationIdentity{
			Type: operation.Type, Target: operation.Target, ExpectedVersion: operation.ExpectedVersion,
			InputHash: operation.InputHash, DependsOn: operation.DependsOn, Compiler: operation.Compiler,
			Verification: operation.Verification, RollbackRef: operation.RollbackRef,
		}
		operationID, err := hashDocument(identity)
		if err != nil || operationID != operation.OperationID {
			return compilerChangeSetDocument{}, "", fmt.Errorf("operation identity mismatch at index %d", index)
		}
	}
	if err := validateOperationDAG(document.Operations); err != nil {
		return compilerChangeSetDocument{}, "", err
	}
	changeSetID, err := hashDocument(compilerChangeSetIdentity{
		SchemaVersion: document.SchemaVersion, BaselineHash: document.BaselineHash,
		BlueprintHash: document.BlueprintHash, Operations: document.Operations,
	})
	if err != nil || changeSetID != document.ChangeSetID {
		return compilerChangeSetDocument{}, "", errors.New("change_set_id mismatch")
	}
	canonicalHash, err := hashDocument(document)
	if err != nil {
		return compilerChangeSetDocument{}, "", err
	}
	return document, canonicalHash, nil
}

func validateOperationDAG(operations []compilerChangeOperation) error {
	state := make(map[string]uint8, len(operations))
	byID := make(map[string]compilerChangeOperation, len(operations))
	for _, operation := range operations {
		byID[operation.OperationID] = operation
	}
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return errors.New("change set operations contain a dependency cycle")
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, dependency := range byID[id].DependsOn {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func validateCompilerChangeSetStrategy(changeSet compilerChangeSetDocument, strategy string) error {
	candidateIndex, publishIndex := -1, -1
	candidateCount, publishCount := 0, 0
	for index, operation := range changeSet.Operations {
		switch operation.Type {
		case "candidate_run":
			candidateIndex, candidateCount = index, candidateCount+1
		case "publish":
			publishIndex, publishCount = index, publishCount+1
		}
	}
	switch strategy {
	case ExecutionStrategyTemplateInstantiate:
		if candidateCount != 0 || publishCount != 0 {
			return errors.New("template_instantiate forbids candidate_run and publish")
		}
		return nil
	case ExecutionStrategyCompilerV1:
		if candidateCount != 1 || publishCount != 1 ||
			publishIndex != len(changeSet.Operations)-1 || candidateIndex != publishIndex-1 {
			return errors.New("compiler_v1 requires terminal candidate_run and publish operations")
		}
		candidate := changeSet.Operations[candidateIndex]
		want := make([]string, 0, candidateIndex)
		for _, operation := range changeSet.Operations[:candidateIndex] {
			want = append(want, operation.OperationID)
		}
		sort.Strings(want)
		if !reflect.DeepEqual(candidate.DependsOn, want) {
			return errors.New("compiler_v1 candidate_run must depend on every materialization operation")
		}
		publish := changeSet.Operations[publishIndex]
		if len(publish.DependsOn) != 1 || publish.DependsOn[0] != candidate.OperationID {
			return errors.New("compiler_v1 publish must depend only on candidate_run")
		}
		return nil
	default:
		return fmt.Errorf("unsupported compiler execution strategy %q", strategy)
	}
}

func (s *Store) validateCompilerAuthorizationBundleTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, buildRunID, runMode, executionStrategy, contractHash, actualBaselineHash, confirmedBy string,
) error {
	var revision BlueprintRevision
	var baselineHash, authorityHash, sourceReportHash, patchHash *string
	var authorityRaw, patchRaw []byte
	err := tx.QueryRow(ctx, `
		SELECT workspace_id, build_run_id, revision_no,
			blueprint_json, blueprint_hash, change_set_json, change_set_hash,
			baseline_hash, baseline_captured_at, workflow_mode,
			template_gap_authorization_json, template_gap_authorization_hash,
			evaluation_contract_hash, source_report_hash,
			blueprint_patch_json, blueprint_patch_hash, created_at
		FROM weave_team_build_blueprint_revisions
		WHERE workspace_id=$1 AND build_run_id=$2
		ORDER BY revision_no DESC
		LIMIT 1
		FOR UPDATE
	`, workspaceID, buildRunID).Scan(
		&revision.WorkspaceID, &revision.BuildRunID, &revision.RevisionNo,
		&revision.BlueprintJSON, &revision.BlueprintHash, &revision.ChangeSetJSON, &revision.ChangeSetHash,
		&baselineHash, &revision.BaselineCapturedAt, &revision.WorkflowMode, &authorityRaw, &authorityHash,
		&revision.EvaluationContractHash, &sourceReportHash,
		&patchRaw, &patchHash, &revision.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: compiler-v1 run has no persisted revision", ErrCompilerBundleInvalid)
	}
	if err != nil {
		return err
	}
	if baselineHash != nil {
		revision.BaselineHash = *baselineHash
	}
	if authorityHash != nil {
		revision.TemplateGapAuthorizationHash = *authorityHash
	}
	revision.TemplateGapAuthorizationJSON = authorityRaw
	if sourceReportHash != nil {
		revision.SourceReportHash = *sourceReportHash
	}
	if patchHash != nil {
		revision.BlueprintPatchHash = *patchHash
	}
	revision.BlueprintPatchJSON = patchRaw
	bundle := CompilerAuthorizationBundle{
		RevisionNo:    revision.RevisionNo,
		BlueprintJSON: revision.BlueprintJSON, BlueprintHash: revision.BlueprintHash,
		ChangeSetJSON: revision.ChangeSetJSON, ChangeSetHash: revision.ChangeSetHash,
		BaselineHash:                 revision.BaselineHash,
		BaselineCapturedAt:           revision.BaselineCapturedAt,
		TemplateGapAuthorizationJSON: revision.TemplateGapAuthorizationJSON,
		TemplateGapAuthorizationHash: revision.TemplateGapAuthorizationHash,
		EvaluationContractHash:       revision.EvaluationContractHash,
	}
	blueprint, changeSet, workflowMode, _, err := validateCompilerBundleDocuments(bundle)
	if err != nil {
		return err
	}
	if blueprint.Mode != runMode || workflowMode != revision.WorkflowMode {
		return fmt.Errorf("%w: run or workflow binding mismatch", ErrCompilerBundleInvalid)
	}
	if err := validateCompilerChangeSetStrategy(changeSet, executionStrategy); err != nil {
		return fmt.Errorf("%w: %v", ErrCompilerBundleInvalid, err)
	}
	if revision.EvaluationContractHash != contractHash {
		return fmt.Errorf("%w: evaluation contract binding mismatch", ErrCompilerBundleInvalid)
	}
	if workflowMode == BlueprintWorkflowDeclarativeV1 {
		binding, err := declarativeWorkflowBuildBinding(changeSet)
		expectedDeclarativeBaselineHash := actualBaselineHash
		if runMode == ModeCreate {
			expectedDeclarativeBaselineHash = emptyCreateBaselineHashV1
		}
		if err != nil || binding.BuildRunID != buildRunID ||
			binding.ContractHash != contractHash || binding.BaselineHash != expectedDeclarativeBaselineHash {
			return fmt.Errorf("%w: frozen declarative_v1 BuildRun binding mismatch", ErrCompilerBundleInvalid)
		}
	}
	if runMode == ModeOptimize {
		if revision.BaselineCapturedAt == nil || !validUTCTimestamp(*revision.BaselineCapturedAt) ||
			revision.BaselineHash == "" || revision.BaselineHash != actualBaselineHash || changeSet.BaselineHash != actualBaselineHash {
			return fmt.Errorf("%w: baseline binding mismatch", ErrCompilerBundleInvalid)
		}
	} else if revision.BaselineHash != emptyCreateBaselineHashV1 ||
		revision.BaselineCapturedAt != nil || changeSet.BaselineHash != emptyCreateBaselineHashV1 || actualBaselineHash != "" {
		return fmt.Errorf("%w: create run must bind the canonical empty baseline", ErrCompilerBundleInvalid)
	}
	if workflowMode == BlueprintWorkflowCustom {
		fact := blueprint.Workflow.TemplateGapAuthorization
		if fact == nil || !fact.Confirmed || strings.TrimSpace(fact.AuthorityRef) != strings.TrimSpace(confirmedBy) {
			return fmt.Errorf("%w: custom workflow lacks matching administrator template-gap authority", ErrCompilerBundleInvalid)
		}
	}
	var stepCount int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM weave_team_build_operation_steps
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3
	`, workspaceID, buildRunID, revision.RevisionNo).Scan(&stepCount); err != nil {
		return err
	}
	if stepCount != len(changeSet.Operations) {
		return fmt.Errorf("%w: operation step materialization mismatch", ErrCompilerBundleInvalid)
	}
	return nil
}

func (s *Store) requireLatestCompilerRevisionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, buildRunID, mode, contractHash, actualBaselineHash string,
) (*BlueprintRevisionToken, error) {
	var revisionNo int
	var revisionContractHash, revisionBaselineHash string
	var blueprintHash, changeSetHash string
	err := tx.QueryRow(ctx, `
		SELECT revision_no, evaluation_contract_hash, baseline_hash,
			blueprint_hash, change_set_hash
		FROM weave_team_build_blueprint_revisions
		WHERE workspace_id=$1 AND build_run_id=$2
		ORDER BY revision_no DESC
		LIMIT 1
		FOR UPDATE
	`, workspaceID, buildRunID).Scan(&revisionNo, &revisionContractHash, &revisionBaselineHash, &blueprintHash, &changeSetHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCompilerRevisionRequired
	}
	if err != nil {
		return nil, err
	}
	if revisionContractHash != contractHash {
		return nil, fmt.Errorf("%w: evaluation contract binding mismatch", ErrCompilerBundleInvalid)
	}
	switch mode {
	case ModeCreate:
		if revisionBaselineHash != emptyCreateBaselineHashV1 || actualBaselineHash != "" {
			return nil, fmt.Errorf("%w: create run must bind the canonical empty baseline", ErrCompilerBundleInvalid)
		}
	case ModeOptimize:
		if actualBaselineHash == "" || revisionBaselineHash != actualBaselineHash {
			return nil, fmt.Errorf("%w: optimize baseline binding mismatch", ErrCompilerBundleInvalid)
		}
	default:
		return nil, fmt.Errorf("%w: unsupported compiler mode %q", ErrCompilerBundleInvalid, mode)
	}
	return &BlueprintRevisionToken{RevisionNo: revisionNo, BlueprintHash: blueprintHash, ChangeSetHash: changeSetHash}, nil
}

func (s *Store) latestCompilerBaselineCapturedAtTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, buildRunID string,
) (time.Time, error) {
	var capturedAt *time.Time
	err := tx.QueryRow(ctx, `
		SELECT baseline_captured_at
		FROM weave_team_build_blueprint_revisions
		WHERE workspace_id=$1 AND build_run_id=$2
		ORDER BY revision_no DESC
		LIMIT 1
		FOR UPDATE
	`, workspaceID, buildRunID).Scan(&capturedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("%w: compiler-v1 run has no persisted revision", ErrCompilerBundleInvalid)
	}
	if err != nil {
		return time.Time{}, err
	}
	if capturedAt == nil || !validUTCTimestamp(*capturedAt) {
		return time.Time{}, fmt.Errorf("%w: optimize revision has no valid baseline_captured_at", ErrCompilerBundleInvalid)
	}
	return capturedAt.UTC(), nil
}

func decodeJSONObject(raw json.RawMessage, target any) error {
	if len(raw) == 0 || !json.Valid(raw) {
		return errors.New("valid JSON is required")
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil || shape == nil {
		return errors.New("JSON object is required")
	}
	return json.Unmarshal(raw, target)
}

func canonicalJSONObject(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, errors.New("valid JSON is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("JSON object is required")
	}
	encoded, err := json.Marshal(value)
	return json.RawMessage(encoded), err
}

// GetLatestBlueprintRevision returns the newest immutable compiler revision.
func (s *Store) GetLatestBlueprintRevision(
	ctx context.Context, workspaceID, buildRunID string,
) (BlueprintRevision, error) {
	var revision BlueprintRevision
	var baselineHash, authorityHash, sourceReportHash, patchHash *string
	var authorityRaw, patchRaw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT workspace_id, build_run_id, revision_no,
			blueprint_json, blueprint_hash, change_set_json, change_set_hash,
			baseline_hash, baseline_captured_at, workflow_mode,
			template_gap_authorization_json, template_gap_authorization_hash,
			evaluation_contract_hash, source_report_hash,
			blueprint_patch_json, blueprint_patch_hash, created_at
		FROM weave_team_build_blueprint_revisions
		WHERE workspace_id=$1 AND build_run_id=$2
		ORDER BY revision_no DESC LIMIT 1
	`, workspaceID, buildRunID).Scan(
		&revision.WorkspaceID, &revision.BuildRunID, &revision.RevisionNo,
		&revision.BlueprintJSON, &revision.BlueprintHash,
		&revision.ChangeSetJSON, &revision.ChangeSetHash,
		&baselineHash, &revision.BaselineCapturedAt, &revision.WorkflowMode, &authorityRaw, &authorityHash,
		&revision.EvaluationContractHash, &sourceReportHash,
		&patchRaw, &patchHash, &revision.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return BlueprintRevision{}, ErrBuildRunNotFound
	}
	if err != nil {
		return BlueprintRevision{}, fmt.Errorf("get latest compiler revision: %w", err)
	}
	if baselineHash != nil {
		revision.BaselineHash = *baselineHash
	}
	if authorityHash != nil {
		revision.TemplateGapAuthorizationHash = *authorityHash
	}
	revision.TemplateGapAuthorizationJSON = append(json.RawMessage(nil), authorityRaw...)
	if sourceReportHash != nil {
		revision.SourceReportHash = *sourceReportHash
	}
	if patchHash != nil {
		revision.BlueprintPatchHash = *patchHash
	}
	revision.BlueprintPatchJSON = append(json.RawMessage(nil), patchRaw...)
	return revision, nil
}

func canonicalJSONObjectHash(raw json.RawMessage) (string, error) {
	canonical, err := canonicalJSONObject(raw)
	if err != nil {
		return "", err
	}
	var value any
	if err := json.Unmarshal(canonical, &value); err != nil {
		return "", err
	}
	return hashDocument(value)
}

func nullableRaw(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

// ListOperationSteps returns a stable operation-index projection.
func (s *Store) ListOperationSteps(
	ctx context.Context, workspaceID, buildRunID string, revisionNo int,
) ([]OperationStep, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT workspace_id, build_run_id, revision_no, operation_id,
			operation_index, operation_type, status, depends_on, input_hash,
			lease_owner, lease_epoch, lease_until, attempt,
			error_class, error_code, evidence_json, output_hash,
			created_at, updated_at, started_at, completed_at
		FROM weave_team_build_operation_steps
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3
		ORDER BY operation_index
	`, workspaceID, buildRunID, revisionNo)
	if err != nil {
		return nil, fmt.Errorf("list compiler operation steps: %w", err)
	}
	defer rows.Close()
	var steps []OperationStep
	for rows.Next() {
		step, err := scanOperationStep(rows)
		if err != nil {
			return nil, fmt.Errorf("list compiler operation steps: %w", err)
		}
		steps = append(steps, step)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list compiler operation steps: %w", err)
	}
	return steps, nil
}

// ListOperationAttempts returns the immutable attempt ledger for one step.
func (s *Store) ListOperationAttempts(
	ctx context.Context, workspaceID, buildRunID string, revisionNo int, operationID string,
) ([]OperationAttempt, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT workspace_id, build_run_id, revision_no, operation_id,
			attempt, lease_epoch, worker_id, status,
			error_class, error_code, evidence_json, output_hash,
			started_at, completed_at
		FROM weave_team_build_operation_attempts
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3 AND operation_id=$4
		ORDER BY attempt
	`, workspaceID, buildRunID, revisionNo, operationID)
	if err != nil {
		return nil, fmt.Errorf("list compiler operation attempts: %w", err)
	}
	defer rows.Close()
	var attempts []OperationAttempt
	for rows.Next() {
		attempt, err := scanOperationAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("list compiler operation attempts: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list compiler operation attempts: %w", err)
	}
	return attempts, nil
}

// ClaimReadyOperationStep leases one dependency-ready operation. Repeating a
// claim by the same worker returns its live lease without increasing attempt;
// an expired running lease is resumable with a strictly larger fence epoch.
func (s *Store) ClaimReadyOperationStep(
	ctx context.Context,
	workspaceID, buildRunID string,
	revisionNo int,
	workerID string,
	leaseDuration time.Duration,
) (OperationStep, error) {
	if strings.TrimSpace(workerID) == "" || leaseDuration <= 0 {
		return OperationStep{}, fmt.Errorf("claim compiler operation step: %w: worker and positive lease are required", ErrCompilerBundleInvalid)
	}
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OperationStep{}, fmt.Errorf("begin claim compiler operation step: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if step, err := scanOperationStep(tx.QueryRow(ctx, operationStepSelect+`
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3
		  AND status='running' AND lease_owner=$4 AND lease_until>$5
		ORDER BY operation_index LIMIT 1
	`, workspaceID, buildRunID, revisionNo, workerID, now)); err == nil {
		if err := tx.Commit(ctx); err != nil {
			return OperationStep{}, fmt.Errorf("commit idempotent compiler operation claim: %w", err)
		}
		return step, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return OperationStep{}, fmt.Errorf("find existing compiler operation lease: %w", err)
	}

	candidate, err := scanOperationStep(tx.QueryRow(ctx, `
		SELECT `+operationStepAliasColumns+`
		FROM weave_team_build_operation_steps s
		WHERE s.workspace_id=$1 AND s.build_run_id=$2 AND s.revision_no=$3
		  AND (s.status='pending' OR (s.status='running' AND s.lease_until<=$4))
		  AND NOT EXISTS (
			SELECT 1
			FROM jsonb_array_elements_text(s.depends_on) dependency(operation_id)
			LEFT JOIN weave_team_build_operation_steps prerequisite
			  ON prerequisite.workspace_id=s.workspace_id
			 AND prerequisite.build_run_id=s.build_run_id
			 AND prerequisite.revision_no=s.revision_no
			 AND prerequisite.operation_id=dependency.operation_id
			WHERE prerequisite.operation_id IS NULL
			   OR prerequisite.status NOT IN ('succeeded','skipped')
		  )
		ORDER BY s.operation_index
		FOR UPDATE OF s SKIP LOCKED
		LIMIT 1
	`, workspaceID, buildRunID, revisionNo, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return OperationStep{}, ErrNoReadyOperationStep
	}
	if err != nil {
		return OperationStep{}, fmt.Errorf("claim compiler operation step: %w", err)
	}
	if candidate.Status == OperationStatusRunning {
		expiryEvidence := json.RawMessage(`{"reason":"lease_expired"}`)
		command, err := tx.Exec(ctx, `
			UPDATE weave_team_build_operation_attempts
			SET status='retryable_failed',
				error_class='runtime_infrastructure_failure', error_code='operation_lease_expired',
				evidence_json=$7, completed_at=$6
			WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3
			  AND operation_id=$4 AND attempt=$5 AND status='running'
		`, workspaceID, buildRunID, revisionNo, candidate.OperationID, candidate.Attempt, now, expiryEvidence)
		if err != nil {
			return OperationStep{}, fmt.Errorf("close expired compiler operation attempt: %w", err)
		}
		if command.RowsAffected() != 1 {
			return OperationStep{}, errors.New("close expired compiler operation attempt: attempt row is missing")
		}
	}
	step, err := scanOperationStep(tx.QueryRow(ctx, `
		UPDATE weave_team_build_operation_steps step
		SET status='running', lease_owner=$5, lease_epoch=step.lease_epoch+1,
			lease_until=$7, attempt=step.attempt+1,
			started_at=COALESCE(step.started_at,$6), updated_at=$6
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3 AND operation_id=$4
		RETURNING `+operationStepReturningColumns,
		workspaceID, buildRunID, revisionNo, candidate.OperationID, workerID, now, now.Add(leaseDuration)))
	if err != nil {
		return OperationStep{}, fmt.Errorf("claim compiler operation step: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_build_operation_attempts (
			workspace_id, build_run_id, revision_no, operation_id,
			attempt, lease_epoch, worker_id, status, started_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,'running',$8)
	`, workspaceID, buildRunID, revisionNo, step.OperationID,
		step.Attempt, step.LeaseEpoch, workerID, now); err != nil {
		return OperationStep{}, fmt.Errorf("record compiler operation attempt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return OperationStep{}, fmt.Errorf("commit compiler operation claim: %w", err)
	}
	return step, nil
}

// RenewOperationStepLease extends only the current live fencing token.
func (s *Store) RenewOperationStepLease(
	ctx context.Context, workspaceID, buildRunID string, revisionNo int,
	operationID, workerID string, leaseEpoch int64, leaseDuration time.Duration,
) (OperationStep, error) {
	if leaseDuration <= 0 {
		return OperationStep{}, errors.New("renew compiler operation step: positive lease is required")
	}
	now := s.clock.Now()
	step, err := scanOperationStep(s.pool.QueryRow(ctx, `
		UPDATE weave_team_build_operation_steps
		SET lease_until=$7, updated_at=$6
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3
		  AND operation_id=$4 AND status='running'
		  AND lease_owner=$5 AND lease_epoch=$8
		RETURNING `+operationStepColumns,
		workspaceID, buildRunID, revisionNo, operationID, workerID, now,
		now.Add(leaseDuration), leaseEpoch))
	if errors.Is(err, pgx.ErrNoRows) {
		return OperationStep{}, ErrOperationStepLeaseLost
	}
	return step, err
}

func (s *Store) SucceedOperationStep(
	ctx context.Context, workspaceID, buildRunID string, revisionNo int,
	operationID, workerID string, leaseEpoch int64,
	outputHash string, evidenceJSON json.RawMessage,
) (OperationStep, error) {
	return s.finishOperationStep(ctx, workspaceID, buildRunID, revisionNo,
		operationID, workerID, leaseEpoch, OperationStatusSucceeded,
		outputHash, "", "", evidenceJSON)
}

func (s *Store) SkipOperationStep(
	ctx context.Context, workspaceID, buildRunID string, revisionNo int,
	operationID, workerID string, leaseEpoch int64,
	outputHash string, evidenceJSON json.RawMessage,
) (OperationStep, error) {
	return s.finishOperationStep(ctx, workspaceID, buildRunID, revisionNo,
		operationID, workerID, leaseEpoch, OperationStatusSkipped,
		outputHash, "", "", evidenceJSON)
}

func (s *Store) FailOperationStep(
	ctx context.Context, workspaceID, buildRunID string, revisionNo int,
	operationID, workerID string, leaseEpoch int64,
	errorClass, errorCode string, evidenceJSON json.RawMessage,
) (OperationStep, error) {
	return s.finishOperationStep(ctx, workspaceID, buildRunID, revisionNo,
		operationID, workerID, leaseEpoch, OperationStatusFailed,
		"", errorClass, errorCode, evidenceJSON)
}

// RetryOperationStep preserves the current typed attempt failure and returns
// the operation to pending. It does not change revision identity and carries
// no business deadline; a later claim creates attempt+1 with a new fence.
func (s *Store) RetryOperationStep(
	ctx context.Context, workspaceID, buildRunID string, revisionNo int,
	operationID, workerID string, leaseEpoch int64,
	errorClass, errorCode string, evidenceJSON json.RawMessage,
) (OperationStep, error) {
	if errorClass != "compile_failure" && errorClass != "runtime_infrastructure_failure" &&
		errorClass != "budget_exhausted" {
		return OperationStep{}, errors.New("retry compiler operation step: failure class is not retryable")
	}
	if strings.TrimSpace(errorCode) == "" {
		return OperationStep{}, errors.New("retry compiler operation step: error_code is required")
	}
	if _, err := canonicalJSONObject(evidenceJSON); err != nil {
		return OperationStep{}, fmt.Errorf("retry compiler operation step: evidence must be a JSON object: %w", err)
	}
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OperationStep{}, fmt.Errorf("begin retry compiler operation step: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, err := tx.Exec(ctx, `
		UPDATE weave_team_build_operation_attempts
		SET status='retryable_failed', error_class=$8, error_code=$9,
			evidence_json=$10, completed_at=$7
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3
		  AND operation_id=$4 AND lease_epoch=$6 AND worker_id=$5 AND status='running'
	`, workspaceID, buildRunID, revisionNo, operationID, workerID, leaseEpoch, now,
		errorClass, errorCode, evidenceJSON)
	if err != nil || command.RowsAffected() != 1 {
		return OperationStep{}, ErrOperationStepLeaseLost
	}
	step, err := scanOperationStep(tx.QueryRow(ctx, `
		UPDATE weave_team_build_operation_steps step
		SET status='pending', lease_owner=NULL, lease_until=NULL, updated_at=$7
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3
		  AND operation_id=$4 AND status='running'
		  AND lease_owner=$5 AND lease_epoch=$6 AND lease_until>$7
		RETURNING `+operationStepReturningColumns,
		workspaceID, buildRunID, revisionNo, operationID, workerID, leaseEpoch, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return OperationStep{}, ErrOperationStepLeaseLost
	}
	if err != nil {
		return OperationStep{}, fmt.Errorf("retry compiler operation step: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return OperationStep{}, fmt.Errorf("commit retry compiler operation step: %w", err)
	}
	return step, nil
}

func (s *Store) finishOperationStep(
	ctx context.Context, workspaceID, buildRunID string, revisionNo int,
	operationID, workerID string, leaseEpoch int64, status,
	outputHash, errorClass, errorCode string, evidenceJSON json.RawMessage,
) (OperationStep, error) {
	if _, err := canonicalJSONObject(evidenceJSON); err != nil {
		return OperationStep{}, fmt.Errorf("finish compiler operation step: evidence must be a JSON object: %w", err)
	}
	if (status == OperationStatusSucceeded || status == OperationStatusSkipped) && !canonicalSHA256Pattern.MatchString(outputHash) {
		return OperationStep{}, errors.New("finish compiler operation step: output_hash is invalid")
	}
	if status == OperationStatusFailed && (!compilerFailureClasses[errorClass] || strings.TrimSpace(errorCode) == "") {
		return OperationStep{}, errors.New("finish compiler operation step: typed error class and code are required")
	}
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OperationStep{}, fmt.Errorf("begin finish compiler operation step: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, err := tx.Exec(ctx, `
		UPDATE weave_team_build_operation_attempts
		SET status=$8, error_class=$9, error_code=$10,
			evidence_json=$11, output_hash=$12, completed_at=$7
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3
		  AND operation_id=$4 AND worker_id=$5 AND lease_epoch=$6 AND status='running'
	`, workspaceID, buildRunID, revisionNo, operationID, workerID, leaseEpoch, now,
		status, nullableString(errorClass), nullableString(errorCode), evidenceJSON, nullableString(outputHash))
	if err != nil || command.RowsAffected() != 1 {
		return OperationStep{}, ErrOperationStepLeaseLost
	}
	step, err := scanOperationStep(tx.QueryRow(ctx, `
		UPDATE weave_team_build_operation_steps
		SET status=$8, lease_owner=NULL, lease_until=NULL,
			error_class=$9, error_code=$10, evidence_json=$11, output_hash=$12,
			completed_at=$7, updated_at=$7
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3
		  AND operation_id=$4 AND status='running'
		  AND lease_owner=$5 AND lease_epoch=$6 AND lease_until>$7
		RETURNING `+operationStepColumns,
		workspaceID, buildRunID, revisionNo, operationID, workerID, leaseEpoch, now,
		status, nullableString(errorClass), nullableString(errorCode), evidenceJSON,
		nullableString(outputHash)))
	if errors.Is(err, pgx.ErrNoRows) {
		return OperationStep{}, ErrOperationStepLeaseLost
	}
	if err != nil {
		return OperationStep{}, fmt.Errorf("finish compiler operation step: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return OperationStep{}, fmt.Errorf("commit finish compiler operation step: %w", err)
	}
	return step, nil
}

const operationStepColumns = `
	workspace_id, build_run_id, revision_no, operation_id,
	operation_index, operation_type, status, depends_on, input_hash,
	lease_owner, lease_epoch, lease_until, attempt,
	error_class, error_code, evidence_json, output_hash,
	created_at, updated_at, started_at, completed_at
`

const operationStepReturningColumns = `
	step.workspace_id, step.build_run_id, step.revision_no, step.operation_id,
	step.operation_index, step.operation_type, step.status, step.depends_on, step.input_hash,
	step.lease_owner, step.lease_epoch, step.lease_until, step.attempt,
	step.error_class, step.error_code, step.evidence_json, step.output_hash,
	step.created_at, step.updated_at, step.started_at, step.completed_at
`

const operationStepAliasColumns = `
	s.workspace_id, s.build_run_id, s.revision_no, s.operation_id,
	s.operation_index, s.operation_type, s.status, s.depends_on, s.input_hash,
	s.lease_owner, s.lease_epoch, s.lease_until, s.attempt,
	s.error_class, s.error_code, s.evidence_json, s.output_hash,
	s.created_at, s.updated_at, s.started_at, s.completed_at
`

const operationStepSelect = `SELECT ` + operationStepColumns + ` FROM weave_team_build_operation_steps `

func scanOperationStep(row rowScanner) (OperationStep, error) {
	var step OperationStep
	var dependenciesRaw []byte
	var leaseOwner, errorClass, errorCode, outputHash *string
	var evidenceRaw []byte
	if err := row.Scan(
		&step.WorkspaceID, &step.BuildRunID, &step.RevisionNo, &step.OperationID,
		&step.OperationIndex, &step.OperationType, &step.Status, &dependenciesRaw, &step.InputHash,
		&leaseOwner, &step.LeaseEpoch, &step.LeaseUntil, &step.Attempt,
		&errorClass, &errorCode, &evidenceRaw, &outputHash,
		&step.CreatedAt, &step.UpdatedAt, &step.StartedAt, &step.CompletedAt,
	); err != nil {
		return OperationStep{}, err
	}
	if err := json.Unmarshal(dependenciesRaw, &step.DependsOn); err != nil {
		return OperationStep{}, fmt.Errorf("decode operation dependencies: %w", err)
	}
	if leaseOwner != nil {
		step.LeaseOwner = *leaseOwner
	}
	if errorClass != nil {
		step.ErrorClass = *errorClass
	}
	if errorCode != nil {
		step.ErrorCode = *errorCode
	}
	if outputHash != nil {
		step.OutputHash = *outputHash
	}
	step.EvidenceJSON = append(json.RawMessage(nil), evidenceRaw...)
	return step, nil
}

func scanOperationAttempt(row rowScanner) (OperationAttempt, error) {
	var attempt OperationAttempt
	var errorClass, errorCode, outputHash *string
	var evidenceRaw []byte
	if err := row.Scan(
		&attempt.WorkspaceID, &attempt.BuildRunID, &attempt.RevisionNo, &attempt.OperationID,
		&attempt.Attempt, &attempt.LeaseEpoch, &attempt.WorkerID, &attempt.Status,
		&errorClass, &errorCode, &evidenceRaw, &outputHash,
		&attempt.StartedAt, &attempt.CompletedAt,
	); err != nil {
		return OperationAttempt{}, err
	}
	if errorClass != nil {
		attempt.ErrorClass = *errorClass
	}
	if errorCode != nil {
		attempt.ErrorCode = *errorCode
	}
	if outputHash != nil {
		attempt.OutputHash = *outputHash
	}
	attempt.EvidenceJSON = append(json.RawMessage(nil), evidenceRaw...)
	return attempt, nil
}
