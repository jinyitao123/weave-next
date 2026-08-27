package teambuild

import (
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
