package teambuild

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestEvaluateBudgetUsageUsesOnlyStrictExceededDimensions(t *testing.T) {
	budget := Budget{
		MaxInputTokens: 10, MaxOutputTokens: 5, MaxToolCalls: 2, MaxCostUSD: 1.25,
	}
	equal := BudgetUsage{InputTokens: 10, OutputTokens: 5, ToolCalls: 2, CostUSD: 1.25}
	if decision := evaluateBudgetUsage(equal, equal, budget, budget); len(decision.ExceededDims) != 0 {
		t.Fatalf("exact budget equality exceeded dimensions: %v", decision.ExceededDims)
	}

	over := BudgetUsage{InputTokens: 11, OutputTokens: 6, ToolCalls: 3, CostUSD: 1.26}
	want := []string{
		"round input_tokens", "round output_tokens", "round tool_calls", "round cost_usd",
		"total input_tokens", "total output_tokens", "total tool_calls", "total cost_usd",
	}
	if got := evaluateBudgetUsage(over, over, budget, budget).ExceededDims; !reflect.DeepEqual(got, want) {
		t.Fatalf("exceeded dimensions = %v, want %v", got, want)
	}
}

func TestG5BudgetRejectionAndLateChargeRealPG(t *testing.T) {
	ctx := context.Background()
	store := newAuthorizeTestStore(t)
	run := createAuthorizeTestRun(t, ctx, store, "build-g5", ModeCreate)
	run = authorizeBudgetTestRun(t, ctx, store, run)
	moveBudgetTestRunToPublishing(t, ctx, store, run)
	if _, err := store.RecordBudgetUsage(ctx, run.WorkspaceID, run.BuildRunID, BudgetCharge{
		WorkspaceID: run.WorkspaceID, BuildRunID: run.BuildRunID, RoundNo: 1,
		SourceKind: UsageSourceKindCandidateRuntime, SourceRole: SourceRoleFixedWorkflowRoot,
		SourceRunID: "candidate-over", InputTokens: 1001,
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	locked, err := store.LockBuildRunTx(ctx, tx, run.WorkspaceID, run.BuildRunID)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := store.EvaluateBudgetTx(
		ctx, tx, run.WorkspaceID, run.BuildRunID, 1, locked.RoundBudget, locked.TotalBudget,
	)
	if err != nil || len(decision.ExceededDims) == 0 {
		t.Fatalf("G5 decision = %#v, err = %v", decision, err)
	}
	if _, err := store.BlockPublishingBudgetTx(ctx, tx, run.WorkspaceID, run.BuildRunID, "publisher"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	blocked, err := store.GetBuildRun(ctx, run.WorkspaceID, run.BuildRunID)
	if err != nil || blocked.Status != StatusBlocked || blocked.PublishEligible {
		t.Fatalf("blocked run = %#v, err = %v", blocked, err)
	}
	reason, err := store.GetLatestBuildRunTransitionReason(ctx, run.WorkspaceID, run.BuildRunID)
	if err != nil || reason != "budget_exhausted" {
		t.Fatalf("G5 reason = %q, err = %v", reason, err)
	}

	passed := createAuthorizeTestRun(t, ctx, store, "build-passed", ModeCreate)
	passed = authorizeBudgetTestRun(t, ctx, store, passed)
	moveBudgetTestRunToPublishing(t, ctx, store, passed)
	if _, err := store.MarkPublished(ctx, passed.WorkspaceID, passed.BuildRunID, "publisher", FinalRef{Ref: "candidate-hash"}); err != nil {
		t.Fatal(err)
	}
	_, err = store.RecordBudgetUsage(ctx, passed.WorkspaceID, passed.BuildRunID, BudgetCharge{
		WorkspaceID: passed.WorkspaceID, BuildRunID: passed.BuildRunID, RoundNo: 1,
		SourceKind: UsageSourceKindCandidateRuntime, SourceRole: SourceRoleFixedWorkflowRoot,
		SourceRunID: "late-candidate", InputTokens: 1,
	})
	if !errors.Is(err, ErrBudgetUsageAfterPassed) {
		t.Fatalf("late charge error = %v, want ErrBudgetUsageAfterPassed", err)
	}
}

func TestBudgetReauthorizationResetsBudgetFailureRealPG(t *testing.T) {
	ctx := context.Background()
	store := newAuthorizeTestStore(t)
	planning := createAuthorizeTestRun(t, ctx, store, "build-budget-recovery", ModeCreate)
	authorized := authorizeBudgetTestRun(t, ctx, store, planning)
	if _, err := store.TransitionStatus(
		ctx, authorized.WorkspaceID, authorized.BuildRunID,
		StatusAuthorized, StatusRoundRunning, "worker", "candidate started",
	); err != nil {
		t.Fatal(err)
	}

	step, err := store.ClaimReadyOperationStep(
		ctx, authorized.WorkspaceID, authorized.BuildRunID, 1, "worker", time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RetryOperationStep(
		ctx, step.WorkspaceID, step.BuildRunID, step.RevisionNo,
		step.OperationID, "worker", step.LeaseEpoch,
		BudgetExhaustedReason, BudgetExhaustedReason, json.RawMessage(`{"gate":"G1"}`),
	); err != nil {
		t.Fatalf("retry budget failure class: %v", err)
	}
	step, err = store.ClaimReadyOperationStep(
		ctx, authorized.WorkspaceID, authorized.BuildRunID, 1, "worker", time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FailOperationStep(
		ctx, step.WorkspaceID, step.BuildRunID, step.RevisionNo,
		step.OperationID, "worker", step.LeaseEpoch,
		BudgetExhaustedReason, BudgetExhaustedReason, json.RawMessage(`{"gate":"G1"}`),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordBudgetUsage(ctx, authorized.WorkspaceID, authorized.BuildRunID, BudgetCharge{
		WorkspaceID: authorized.WorkspaceID, BuildRunID: authorized.BuildRunID, RoundNo: 1,
		SourceKind: UsageSourceKindBuildAgent, SourceRole: SourceRoleFixedWorkflowRoot,
		SourceRunID: "build-agent-over", InputTokens: 1001,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionStatus(
		ctx, authorized.WorkspaceID, authorized.BuildRunID,
		StatusRoundRunning, StatusBlocked, "worker", BudgetExhaustedReason,
	); err != nil {
		t.Fatal(err)
	}

	newRound := Budget{MaxInputTokens: 2000}
	newTotal := Budget{MaxInputTokens: 6000}
	restored, receipt, err := store.ReauthorizeBudgetBlockedRun(
		ctx, authorized.WorkspaceID, authorized.BuildRunID, "admin-2", newRound, newTotal,
	)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != StatusAuthorized || restored.BriefHash != planning.BriefHash ||
		restored.Brief.RoundBudget != planning.Brief.RoundBudget {
		t.Fatalf("restored run changed frozen brief identity: %#v", restored)
	}
	if !receipt.Valid() || receipt.RoundBudget() != newRound || receipt.TotalBudget() != newTotal ||
		receipt.ConfirmedBy() != "admin-2" {
		t.Fatalf("reauthorization receipt = %#v", receipt)
	}
	steps, err := store.ListOperationSteps(ctx, restored.WorkspaceID, restored.BuildRunID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[0].Status != OperationStatusPending ||
		steps[0].ErrorClass != "" || steps[1].Status != OperationStatusPending {
		t.Fatalf("restored steps = %#v", steps)
	}
	decision, err := store.EvaluateBudget(
		ctx, restored.WorkspaceID, restored.BuildRunID, 1,
		restored.RoundBudget, restored.TotalBudget,
	)
	if err != nil || len(decision.ExceededDims) != 0 {
		t.Fatalf("restored budget decision = %#v, err = %v", decision, err)
	}
}

func TestBudgetOperationRecoveryGuardRejectsOtherTerminalsRealPG(t *testing.T) {
	ctx := context.Background()
	store := newAuthorizeTestStore(t)

	compileFailed := createAuthorizeTestRun(t, ctx, store, "build-compile-failed", ModeCreate)
	compileFailed = authorizeBudgetTestRun(t, ctx, store, compileFailed)
	if _, err := store.TransitionStatus(
		ctx, compileFailed.WorkspaceID, compileFailed.BuildRunID,
		StatusAuthorized, StatusRoundRunning, "worker", "operation started",
	); err != nil {
		t.Fatal(err)
	}
	failedStep, err := store.ClaimReadyOperationStep(
		ctx, compileFailed.WorkspaceID, compileFailed.BuildRunID, 1, "worker", time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	failedStep, err = store.FailOperationStep(
		ctx, failedStep.WorkspaceID, failedStep.BuildRunID, failedStep.RevisionNo,
		failedStep.OperationID, "worker", failedStep.LeaseEpoch,
		"compile_failure", "compile_failed", json.RawMessage(`{"compile":false}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := forceBudgetTestStepPending(ctx, store, failedStep); err == nil {
		t.Fatal("compile_failure terminal step reopened; want database rejection")
	}

	succeeded := createAuthorizeTestRun(t, ctx, store, "build-succeeded", ModeCreate)
	succeeded = authorizeBudgetTestRun(t, ctx, store, succeeded)
	if _, err := store.TransitionStatus(
		ctx, succeeded.WorkspaceID, succeeded.BuildRunID,
		StatusAuthorized, StatusRoundRunning, "worker", "operation started",
	); err != nil {
		t.Fatal(err)
	}
	succeededStep, err := store.ClaimReadyOperationStep(
		ctx, succeeded.WorkspaceID, succeeded.BuildRunID, 1, "worker", time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	succeededStep, err = store.SucceedOperationStep(
		ctx, succeededStep.WorkspaceID, succeededStep.BuildRunID, succeededStep.RevisionNo,
		succeededStep.OperationID, "worker", succeededStep.LeaseEpoch,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		json.RawMessage(`{"compiled":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := forceBudgetTestStepPending(ctx, store, succeededStep); err == nil {
		t.Fatal("succeeded terminal step reopened; want database rejection")
	}
}

func forceBudgetTestStepPending(ctx context.Context, store *Store, step OperationStep) error {
	_, err := store.pool.Exec(ctx, `
		UPDATE weave_team_build_operation_steps
		SET status='pending', lease_owner=NULL, lease_until=NULL,
			error_class=NULL, error_code=NULL, evidence_json=NULL,
			output_hash=NULL, completed_at=NULL, updated_at=updated_at
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3 AND operation_id=$4
	`, step.WorkspaceID, step.BuildRunID, step.RevisionNo, step.OperationID)
	return err
}

func moveBudgetTestRunToPublishing(t *testing.T, ctx context.Context, store *Store, run TeamBuildRun) {
	t.Helper()
	transitions := [][2]string{{StatusAuthorized, StatusRoundRunning}, {StatusRoundRunning, StatusPublishing}}
	for _, transition := range transitions {
		if _, err := store.TransitionStatus(ctx, run.WorkspaceID, run.BuildRunID, transition[0], transition[1], "test", "test transition"); err != nil {
			t.Fatal(err)
		}
	}
}

func authorizeBudgetTestRun(t *testing.T, ctx context.Context, store *Store, run TeamBuildRun) TeamBuildRun {
	t.Helper()
	bundle := budgetTestCompilerBundle(t, run)
	revision, err := store.PersistCompilerAuthorizationBundle(ctx, run.WorkspaceID, run.BuildRunID, bundle)
	if err != nil {
		t.Fatalf("persist compiler authorization bundle: %v", err)
	}
	authorized, receipt, err := store.AuthorizeBuildRun(
		ctx, run.WorkspaceID, run.BuildRunID, "admin-1", nil,
		AuthorizeOptions{
			Authority: AuthorizationContinueBuild,
			RevisionToken: &BlueprintRevisionToken{
				RevisionNo: revision.RevisionNo, BlueprintHash: revision.BlueprintHash,
				ChangeSetHash: revision.ChangeSetHash,
			},
		},
	)
	if err != nil {
		t.Fatalf("authorize build run: %v", err)
	}
	if !receipt.Valid() || authorized.Status != StatusAuthorized || authorized.ConfirmedBy != "admin-1" {
		t.Fatalf("invalid production authorization fixture: run=%#v receipt_valid=%v", authorized, receipt.Valid())
	}
	return authorized
}

func budgetTestCompilerBundle(t *testing.T, run TeamBuildRun) CompilerAuthorizationBundle {
	t.Helper()
	blueprint := TeamBlueprintV1{
		SchemaVersion: BlueprintSchemaVersionV1,
		Mode:          ModeCreate,
		NewTeamName:   "budget-test-team",
		Purpose:       "验证预算终结协议",
		Members: []BlueprintMemberV1{{
			StableRef: "lead", Name: "budget-test-lead", DisplayName: "负责人",
			Role: BlueprintMemberRoleAvatar, ManagementMode: BlueprintManagementManaged,
			Responsibilities: []string{"协调"}, Capabilities: []string{"delegation"},
			ExecutionPolicy: BlueprintExecutionPolicyV1{
				EngineClass: BlueprintEngineStandard, ExecutionMode: BlueprintExecutionToolLoop,
			},
		}},
		LeadRef: "lead",
		Workflow: BlueprintWorkflowV1{
			Mode: BlueprintWorkflowTemplate, Template: BlueprintTemplateDeliveryRework,
			TemplateParameters: &BlueprintWorkflowTemplateParametersV1{
				LeadInstruction: "完成预算验证", PrimaryRef: "lead", ReviewerRef: "lead",
				ParallelWorkerRefs: []string{},
				MaxIterations:      intPointer(1), ResultRequirements: map[string]string{"lead": "结果可验证"},
			},
		},
		RevisionPolicy: BlueprintRevisionPolicyV1{MaxRevisions: 1, AllowedPatchPaths: []string{"/purpose"}},
	}
	blueprintJSON, err := json.Marshal(blueprint)
	if err != nil {
		t.Fatal(err)
	}
	blueprintHash, err := blueprint.BlueprintHash()
	if err != nil {
		t.Fatalf("hash blueprint: %v", err)
	}

	candidate := budgetTestOperation(t, "candidate_run", "budget-test-candidate", nil)
	publish := budgetTestOperation(t, "publish", "budget-test-publish", []string{candidate.OperationID})
	changeSet := compilerChangeSetDocument{
		SchemaVersion: 1, BaselineHash: emptyCreateBaselineHashV1,
		BlueprintHash: blueprintHash, Operations: []compilerChangeOperation{candidate, publish},
	}
	changeSet.ChangeSetID, err = hashDocument(compilerChangeSetIdentity{
		SchemaVersion: changeSet.SchemaVersion, BaselineHash: changeSet.BaselineHash,
		BlueprintHash: changeSet.BlueprintHash, Operations: changeSet.Operations,
	})
	if err != nil {
		t.Fatal(err)
	}
	changeSetJSON, err := json.Marshal(changeSet)
	if err != nil {
		t.Fatal(err)
	}
	changeSetHash, err := hashDocument(changeSet)
	if err != nil {
		t.Fatal(err)
	}
	return CompilerAuthorizationBundle{
		RevisionNo: 1, BlueprintJSON: blueprintJSON, BlueprintHash: blueprintHash,
		ChangeSetJSON: changeSetJSON, ChangeSetHash: changeSetHash,
		BaselineHash: emptyCreateBaselineHashV1, EvaluationContractHash: run.ContractHash,
	}
}

func budgetTestOperation(t *testing.T, operationType, target string, dependsOn []string) compilerChangeOperation {
	t.Helper()
	contract := compilerOperationContracts[operationType]
	input := json.RawMessage(`{}`)
	inputHash, err := canonicalJSONObjectHash(input)
	if err != nil {
		t.Fatal(err)
	}
	operation := compilerChangeOperation{
		Type: operationType, Target: target, Input: input, InputHash: inputHash,
		DependsOn: append(make([]string, 0, len(dependsOn)), dependsOn...), Compiler: contract.compiler,
		Verification: append(make([]string, 0, len(contract.verification)), contract.verification...), RollbackRef: "budget-test-rollback",
	}
	operation.OperationID, err = hashDocument(compilerOperationIdentity{
		Type: operation.Type, Target: operation.Target, InputHash: operation.InputHash,
		DependsOn: operation.DependsOn, Compiler: operation.Compiler,
		Verification: operation.Verification, RollbackRef: operation.RollbackRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func intPointer(value int) *int { return &value }
