package loomruntime

import (
	"fmt"
	"strings"
	"time"

	"github.com/jinyitao123/loom"
)

// TerminalAssemblyInput contains deterministic inputs for a pure terminal candidate.
type TerminalAssemblyInput struct {
	Tenant      string
	Agent       string
	StartedAt   time.Time
	EndedAt     time.Time
	Result      Result
	RunErr      error
	Attribution TerminalAttribution
}

type terminalV3BaseAssemblyInput struct {
	Tenant      string
	Agent       string
	RunID       string
	StartedAt   time.Time
	EndedAt     time.Time
	Status      string
	StopReason  string
	Step        string
	Summary     string
	Attribution TerminalAttribution
	Exclusive   TerminalUsage
}

// AssembleTerminalV3 builds a validated schema-v3 candidate without storage side effects.
func AssembleTerminalV3(input TerminalAssemblyInput) (TerminalEntryV3, error) {
	if input.Tenant == "" {
		return TerminalEntryV3{}, fmt.Errorf("assemble terminal schema-v3: tenant is required")
	}
	if input.Agent == "" {
		return TerminalEntryV3{}, fmt.Errorf("assemble terminal schema-v3: agent is required")
	}
	if input.StartedAt.IsZero() {
		return TerminalEntryV3{}, fmt.Errorf("assemble terminal schema-v3: started_at is required")
	}
	if input.EndedAt.IsZero() {
		return TerminalEntryV3{}, fmt.Errorf("assemble terminal schema-v3: ended_at is required")
	}
	if input.EndedAt.Before(input.StartedAt) {
		return TerminalEntryV3{}, fmt.Errorf("assemble terminal schema-v3: ended_at precedes started_at")
	}
	if err := validateTerminalAttributionForResult(
		input.Attribution,
		input.Tenant,
		input.Result,
	); err != nil {
		return TerminalEntryV3{}, fmt.Errorf("assemble terminal schema-v3 attribution: %w", err)
	}

	exclusive, err := validatedTerminalExclusiveUsage(input.Result.State)
	if err != nil {
		return TerminalEntryV3{}, err
	}

	status := "failed"
	if input.Result.StopReason == loom.StopYielded {
		status = "yielded"
	} else if input.Result.StopReason == loom.StopCompleted && input.RunErr == nil {
		status = "success"
	}
	return assembleTerminalV3Base(terminalV3BaseAssemblyInput{
		Tenant:      input.Tenant,
		Agent:       input.Agent,
		RunID:       input.Result.RunID,
		StartedAt:   input.StartedAt,
		EndedAt:     input.EndedAt,
		Status:      status,
		StopReason:  terminalStopReason(input.Result.StopReason, input.RunErr),
		Step:        input.Result.LastStep,
		Summary:     truncateRunes(stateString(input.Result.State, "last_user_message"), 120),
		Attribution: input.Attribution,
		Exclusive:   exclusive,
	})
}

func validatedTerminalExclusiveUsage(state loom.State) (TerminalUsage, error) {
	rawAccumulator, exists := state[usageAccumulatorStateKey]
	if !exists {
		return TerminalUsage{}, fmt.Errorf(
			"assemble terminal schema-v3: State %s key is missing",
			usageAccumulatorStateKey,
		)
	}
	if rawAccumulator == nil {
		return TerminalUsage{}, fmt.Errorf(
			"assemble terminal schema-v3: State %s value is nil",
			usageAccumulatorStateKey,
		)
	}
	accumulator, err := LoadUsageAccumulator(state)
	if err != nil {
		return TerminalUsage{}, fmt.Errorf(
			"assemble terminal schema-v3: load usage accumulator: %w",
			err,
		)
	}
	totals, err := terminalUsageTotals(accumulator)
	if err != nil {
		return TerminalUsage{}, fmt.Errorf(
			"assemble terminal schema-v3: usage accumulator totals: %w",
			err,
		)
	}
	exclusive := TerminalUsage{
		InputTokens:  totals.InputTokens,
		OutputTokens: totals.OutputTokens,
		CostUSD:      totals.CostUSD,
		ToolCalls:    totals.ToolCalls,
	}
	if err := validateTerminalUsage(exclusive); err != nil {
		return TerminalUsage{}, fmt.Errorf(
			"assemble terminal schema-v3: invalid self_exclusive: %w",
			err,
		)
	}
	return exclusive, nil
}

func assembleTerminalV3Base(
	input terminalV3BaseAssemblyInput,
) (TerminalEntryV3, error) {
	exclusive := input.Exclusive
	subtree, err := addTerminalUsage(TerminalUsage{}, exclusive)
	if err != nil {
		return TerminalEntryV3{}, fmt.Errorf(
			"assemble terminal schema-v3: invalid initial subtree_total: %w",
			err,
		)
	}

	attribution := input.Attribution.inputCopy()
	entry := TerminalEntryV3{
		SchemaVersion:          terminalSchemaVersionV3,
		RunID:                  input.RunID,
		Agent:                  input.Agent,
		Tenant:                 input.Tenant,
		AttributionScope:       attribution.Scope,
		TeamID:                 attribution.TeamID,
		WorkflowID:             attribution.WorkflowID,
		WorkflowVersion:        attribution.WorkflowVersion,
		RunSnapshotID:          attribution.RunSnapshotID,
		ConversationID:         attribution.ConversationID,
		ParentRunID:            attribution.ParentRunID,
		ParentSeq:              attribution.ParentSeq,
		AggregationParentRunID: attribution.AggregationParentRunID,
		TaskGroupID:            attribution.TaskGroupID,
		Status:                 input.Status,
		StopReason:             input.StopReason,
		StartedAt:              input.StartedAt.Format(time.RFC3339Nano),
		EndedAt:                input.EndedAt.Format(time.RFC3339Nano),
		DurationMs:             input.EndedAt.Sub(input.StartedAt).Milliseconds(),
		TokensIn:               exclusive.InputTokens,
		TokensOut:              exclusive.OutputTokens,
		CostUSD:                exclusive.CostUSD,
		ToolCalls:              exclusive.ToolCalls,
		Step:                   input.Step,
		Summary:                input.Summary,
		SelfExclusive:          exclusive,
		ChildBreakdown:         make([]TerminalChildBreakdownV3, 0),
		SubtreeTotal:           subtree,
	}
	if err := ValidateTerminalV3(entry); err != nil {
		return TerminalEntryV3{}, fmt.Errorf(
			"assemble terminal schema-v3: validate candidate: %w",
			err,
		)
	}
	return entry, nil
}

func terminalUsageTotals(accumulator UsageAccumulator) (totals UsageTotals, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			totals = UsageTotals{}
			err = fmt.Errorf("invalid UsageAccumulator invariant: %v", recovered)
		}
	}()
	return accumulator.Totals(), nil
}

func terminalStopReason(reason loom.StopReason, runErr error) string {
	if reason != "" {
		return string(reason)
	}
	if runErr == nil {
		return string(loom.StopCompleted)
	}
	message := strings.ToLower(runErr.Error())
	switch {
	case strings.Contains(message, "max iter"):
		return string(loom.StopMaxIter)
	case strings.Contains(message, "budget"):
		return string(loom.StopBudget)
	case strings.Contains(message, "hook"):
		return string(loom.StopHookAbort)
	default:
		return string(loom.StopError)
	}
}

func stateString(state loom.State, keys ...string) string {
	for _, key := range keys {
		if value, ok := state[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}
