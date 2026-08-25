package teambuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuthorizeBuildRun atomically freezes the run (brief/contract/baseline),
// transitions planning -> authorized, and mints the build authorization
// receipt from the frozen facts. The optimize baseline is always captured
// server-side inside this transaction from the live Team/Roster/Agents/
// Workflows; client-supplied baselines are no longer accepted, so the legacy
// baseline parameter is ignored and kept only for call-site compatibility.
func (s *Store) AuthorizeBuildRun(
	ctx context.Context,
	workspaceID, buildRunID, confirmedBy string,
	_ *BaselineSnapshot,
	options ...AuthorizeOptions,
) (TeamBuildRun, BuildAuthorizationReceipt, error) {
	if workspaceID == "" || buildRunID == "" {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, errors.New("authorize build run: workspace_id and build_run_id are required")
	}
	if strings.TrimSpace(confirmedBy) == "" {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, errors.New("authorize build run: confirmed_by is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("begin authorize build run: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var mode, executionStrategy, status, briefHash, contractHash string
	var briefRaw, contractRaw, scopeRaw, roundBudgetRaw, totalBudgetRaw []byte
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT mode, execution_strategy, status, brief_json, brief_hash, contract_json, contract_hash,
			asset_scope_json, round_budget_json, total_budget_json, expires_at
		FROM weave_team_build_runs
		WHERE workspace_id=$1 AND build_run_id=$2
		FOR UPDATE
	`, workspaceID, buildRunID).Scan(
		&mode, &executionStrategy, &status, &briefRaw, &briefHash, &contractRaw, &contractHash,
		&scopeRaw, &roundBudgetRaw, &totalBudgetRaw, &expiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", ErrBuildRunNotFound)
	}
	if err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", err)
	}
	if status != StatusPlanning {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", ErrBuildRunNotPlanning)
	}

	var brief BuildBrief
	var contract EvaluationContract
	if err := json.Unmarshal(briefRaw, &brief); err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: decode stored brief: %w", err)
	}
	if err := json.Unmarshal(contractRaw, &contract); err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: decode stored contract: %w", err)
	}
	recomputedBriefHash, err := brief.Hash()
	if err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", err)
	}
	recomputedContractHash, err := contract.Hash()
	if err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", err)
	}
	if recomputedBriefHash != briefHash || recomputedContractHash != contractHash {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, errors.New("authorize build run: stored hash does not match frozen content")
	}

	auth := effectiveAuthorizeOptions(options)
	var baselineForInsert any
	baselineHash := ""
	var revisionToken *BlueprintRevisionToken
	switch mode {
	case ModeOptimize:
		var snapshot BaselineSnapshot
		if executionStrategy == ExecutionStrategyCompilerV1 {
			capturedAt, err := s.latestCompilerBaselineCapturedAtTx(ctx, tx, workspaceID, buildRunID)
			if err != nil {
				return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", err)
			}
			snapshot, err = s.captureBaselineTxAt(ctx, tx, workspaceID, brief, capturedAt)
		} else {
			snapshot, err = s.captureBaselineTx(ctx, tx, workspaceID, brief)
		}
		if err != nil {
			return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", err)
		}
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: encode baseline snapshot: %w", err)
		}
		baselineForInsert = encoded
		baselineHash = snapshot.ContentHash
		if executionStrategy == ExecutionStrategyCompilerV1 {
			revisionToken, err = s.requireLatestCompilerRevisionTx(
				ctx, tx, workspaceID, buildRunID, mode, contractHash, baselineHash,
			)
			if err != nil {
				return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", err)
			}
		}
	case ModeCreate:
		if !isCompilerExecutionStrategy(executionStrategy) {
			return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", ErrCompilerRevisionRequired)
		}
		revisionToken, err = s.requireLatestCompilerRevisionTx(
			ctx, tx, workspaceID, buildRunID, mode, contractHash, baselineHash,
		)
		if err != nil {
			return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", err)
		}
	default:
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: invalid mode %q", mode)
	}
	if isCompilerExecutionStrategy(executionStrategy) {
		if err := s.validateCompilerAuthorizationBundleTx(
			ctx, tx, workspaceID, buildRunID, mode, executionStrategy, contractHash, baselineHash, confirmedBy,
		); err != nil {
			return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", err)
		}
	}
	if err := validateBuildAuthorizationOptions(mode, executionStrategy, confirmedBy, auth, revisionToken); err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", err)
	}

	var totalBudget Budget
	if err := json.Unmarshal(totalBudgetRaw, &totalBudget); err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: decode total budget: %w", err)
	}
	now := s.clock.Now()
	decisionReason := ""
	if auth.Authority == AuthorizationTemplateAuto {
		decisionReason, err = s.reserveTemplateAuthorizationTx(
			ctx, tx, workspaceID, buildRunID, totalBudget.MaxCostUSD, *auth.TemplatePolicy, now,
		)
		if err != nil {
			return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", err)
		}
	}
	transitionReason := "workspace admin confirmed brief and contract"
	if decisionReason != "" {
		transitionReason = TemplateAuthorizerSubject + ": " + decisionReason
	}
	if baselineHash != "" {
		transitionReason = "workspace admin confirmed brief and contract; baseline " + baselineHash + " frozen"
	}
	run, err := scanBuildRun(tx.QueryRow(ctx, `
		UPDATE weave_team_build_runs
		SET status='authorized', confirmed_by=$3, baseline_snapshot_json=$4,
			authorization_authority=$5, authorized_revision_no=$6,
			authorized_blueprint_hash=$7, authorized_change_set_hash=$8,
			authorization_decision_subject=$9, authorization_decision_reason=$10,
			authorization_decided_at=$11, updated_at=$11
		WHERE workspace_id=$1 AND build_run_id=$2 AND status='planning'
		RETURNING `+buildRunColumns+`
	`, workspaceID, buildRunID, confirmedBy, baselineForInsert,
		auth.Authority, nullableInt(authRevisionNo(auth)), nullableString(authBlueprintHash(auth)),
		nullableString(authChangeSetHash(auth)), nullableString(auth.DecisionSubject),
		nullableString(decisionReason), now))
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", ErrBuildRunNotPlanning)
	}
	if err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", err)
	}

	nextSeq, err := nextTransitionSeq(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_build_run_transitions (
			workspace_id, build_run_id, seq, from_status, to_status,
			reason, actor, created_at
		) VALUES ($1,$2,$3,'planning','authorized',
			$4,$5,$6)
	`, workspaceID, buildRunID, nextSeq, transitionReason, confirmedBy, now); err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run transition: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("commit authorize build run: %w", err)
	}

	var scope AssetScope
	if err := json.Unmarshal(scopeRaw, &scope); err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: decode asset scope: %w", err)
	}
	var roundBudget Budget
	if err := json.Unmarshal(roundBudgetRaw, &roundBudget); err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf("authorize build run: decode round budget: %w", err)
	}
	receipt := BuildAuthorizationReceipt{
		workspaceID:     workspaceID,
		buildRunID:      buildRunID,
		contractHash:    contractHash,
		mode:            mode,
		authority:       auth.Authority,
		revisionToken:   cloneBlueprintRevisionToken(auth.RevisionToken),
		decisionSubject: auth.DecisionSubject,
		decisionReason:  decisionReason,
		assetScope:      cloneAssetScope(scope),
		roundBudget:     roundBudget,
		totalBudget:     totalBudget,
		expiresAt:       expiresAt,
		confirmedBy:     confirmedBy,
		createdAt:       now,
	}
	return run, receipt, nil
}

func isCompilerExecutionStrategy(strategy string) bool {
	return strategy == ExecutionStrategyCompilerV1 || strategy == ExecutionStrategyTemplateInstantiate
}

func effectiveAuthorizeOptions(options []AuthorizeOptions) AuthorizeOptions {
	var auth AuthorizeOptions
	if len(options) > 0 {
		auth = options[0]
	}
	auth.Authority = strings.TrimSpace(auth.Authority)
	auth.DecisionSubject = strings.TrimSpace(auth.DecisionSubject)
	auth.RevisionToken = cloneBlueprintRevisionToken(auth.RevisionToken)
	if auth.TemplatePolicy != nil {
		policy := *auth.TemplatePolicy
		auth.TemplatePolicy = &policy
	}
	return auth
}

func validateBuildAuthorizationOptions(
	mode, executionStrategy, confirmedBy string,
	auth AuthorizeOptions,
	latest *BlueprintRevisionToken,
) error {
	switch auth.Authority {
	case "":
		return fmt.Errorf("%w: authority is required", ErrBlueprintRevisionMismatch)
	case AuthorizationAutoBuild:
		if auth.DecisionSubject != "" || auth.TemplatePolicy != nil {
			return fmt.Errorf("%w: auto_build forbids template authorization facts", ErrBlueprintRevisionMismatch)
		}
		if auth.RevisionToken != nil {
			return fmt.Errorf("%w: auto_build cannot carry a reviewed revision token", ErrBlueprintRevisionMismatch)
		}
		return nil
	case AuthorizationContinueBuild, "reviewed_blueprint":
		if auth.DecisionSubject != "" || auth.TemplatePolicy != nil {
			return fmt.Errorf("%w: reviewed authorization forbids template authorization facts", ErrBlueprintRevisionMismatch)
		}
		if mode != ModeCreate && mode != ModeOptimize {
			return fmt.Errorf("%w: continue build requires create or optimize mode", ErrBlueprintRevisionMismatch)
		}
		if auth.RevisionToken == nil || latest == nil {
			return ErrCompilerRevisionRequired
		}
		if *auth.RevisionToken != *latest {
			return ErrBlueprintRevisionMismatch
		}
		return nil
	case AuthorizationTemplateAuto:
		if mode != ModeCreate || executionStrategy != ExecutionStrategyTemplateInstantiate {
			return fmt.Errorf("%w: template_auto requires a template_instantiate create run", ErrBlueprintRevisionMismatch)
		}
		if strings.TrimSpace(confirmedBy) == "" || confirmedBy == TemplateAuthorizerSubject {
			return fmt.Errorf("%w: template_auto confirmed_by must be the real submitting user", ErrBlueprintRevisionMismatch)
		}
		if auth.DecisionSubject != TemplateAuthorizerSubject || auth.TemplatePolicy == nil {
			return fmt.Errorf("%w: template_auto requires platform decision facts", ErrBlueprintRevisionMismatch)
		}
		if err := validateTemplateAuthorizationPolicy(*auth.TemplatePolicy); err != nil {
			return fmt.Errorf("%w: %v", ErrBlueprintRevisionMismatch, err)
		}
		if auth.RevisionToken == nil || latest == nil {
			return ErrCompilerRevisionRequired
		}
		if *auth.RevisionToken != *latest {
			return ErrBlueprintRevisionMismatch
		}
		return nil
	default:
		return fmt.Errorf("%w: invalid authorization authority %q", ErrBlueprintRevisionMismatch, auth.Authority)
	}
}

func authRevisionNo(auth AuthorizeOptions) int {
	if auth.RevisionToken == nil {
		return 0
	}
	return auth.RevisionToken.RevisionNo
}

func authBlueprintHash(auth AuthorizeOptions) string {
	if auth.RevisionToken == nil {
		return ""
	}
	return auth.RevisionToken.BlueprintHash
}

func authChangeSetHash(auth AuthorizeOptions) string {
	if auth.RevisionToken == nil {
		return ""
	}
	return auth.RevisionToken.ChangeSetHash
}
