package teambuild

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
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
