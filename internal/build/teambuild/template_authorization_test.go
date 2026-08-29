package teambuild

import (
	"errors"
	"testing"
	"time"
)

func TestValidateTemplateAuthorizationOptions(t *testing.T) {
	token := &BlueprintRevisionToken{RevisionNo: 1, BlueprintHash: testSHA('a'), ChangeSetHash: testSHA('b')}
	policy := &TemplateAuthorizationPolicy{AutoBudgetThresholdUSD: 5, DailyBudgetUSD: 25, MonthlyBudgetUSD: 250, MaxConcurrent: 2}
	valid := AuthorizeOptions{
		Authority: AuthorizationTemplateAuto, RevisionToken: token,
		DecisionSubject: TemplateAuthorizerSubject, TemplatePolicy: policy,
	}
	if err := validateBuildAuthorizationOptions(ModeCreate, ExecutionStrategyTemplateInstantiate, "user-1", valid, token); err != nil {
		t.Fatalf("valid template authorization error = %v", err)
	}
	cases := []struct {
		name      string
		mode      string
		strategy  string
		confirmed string
		mutate    func(AuthorizeOptions) AuthorizeOptions
		want      error
	}{
		{name: "wrong strategy", mode: ModeCreate, strategy: ExecutionStrategyCompilerV1, confirmed: "user-1", mutate: identityAuthorizeOptions, want: ErrBlueprintRevisionMismatch},
		{name: "decision subject as user", mode: ModeCreate, strategy: ExecutionStrategyTemplateInstantiate, confirmed: TemplateAuthorizerSubject, mutate: identityAuthorizeOptions, want: ErrBlueprintRevisionMismatch},
		{name: "missing revision", mode: ModeCreate, strategy: ExecutionStrategyTemplateInstantiate, confirmed: "user-1", mutate: func(value AuthorizeOptions) AuthorizeOptions { value.RevisionToken = nil; return value }, want: ErrCompilerRevisionRequired},
		{name: "missing policy", mode: ModeCreate, strategy: ExecutionStrategyTemplateInstantiate, confirmed: "user-1", mutate: func(value AuthorizeOptions) AuthorizeOptions { value.TemplatePolicy = nil; return value }, want: ErrBlueprintRevisionMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBuildAuthorizationOptions(tc.mode, tc.strategy, tc.confirmed, tc.mutate(valid), token)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestTemplateReceiptRequiresDecisionFacts(t *testing.T) {
	receipt := BuildAuthorizationReceipt{
		workspaceID: "workspace", buildRunID: "run", contractHash: testSHA('c'),
		mode: ModeCreate, authority: AuthorizationTemplateAuto,
		revisionToken:   &BlueprintRevisionToken{RevisionNo: 1, BlueprintHash: testSHA('a'), ChangeSetHash: testSHA('b')},
		decisionSubject: TemplateAuthorizerSubject, decisionReason: "within threshold",
		expiresAt: testFutureTime(), confirmedBy: "user-1", createdAt: testPastTime(),
	}
	if !receipt.Valid() {
		t.Fatal("complete template receipt is invalid")
	}
	if receipt.DecisionSubject() != TemplateAuthorizerSubject || receipt.DecisionReason() == "" {
		t.Fatalf("receipt decision = %q/%q", receipt.DecisionSubject(), receipt.DecisionReason())
	}
	receipt.decisionReason = ""
	if receipt.Valid() {
		t.Fatal("template receipt without a decision reason is valid")
	}
}

func identityAuthorizeOptions(value AuthorizeOptions) AuthorizeOptions { return value }

func testSHA(value byte) string { return string(makeFilledBytes(value, 64)) }

func makeFilledBytes(value byte, count int) []byte {
	result := make([]byte, count)
	for i := range result {
		result[i] = value
	}
	return result
}

func testPastTime() time.Time { return time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC) }

func testFutureTime() time.Time { return time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC) }
