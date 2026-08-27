package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

const (
	UsageSourceCLIReported = "cli-reported"
	UsageScopeInvocation   = "invocation"
	UsageScopeSession      = "session_cumulative"
	maxRawUsageSummary     = 4096
)

// UsageReceipt is weave's lossless CLI usage carrier. HasTokens and HasCost
// distinguish an unreported dimension from an explicitly reported zero.
type UsageReceipt struct {
	InputTokens   int     `json:"input_tokens"`
	OutputTokens  int     `json:"output_tokens"`
	CostUSD       float64 `json:"cost_usd"`
	HasTokens     bool    `json:"has_tokens"`
	HasCost       bool    `json:"has_cost"`
	Source        string  `json:"source"`
	Scope         string  `json:"scope"`
	EngineVersion string  `json:"engine_version,omitempty"`
	RawSummary    string  `json:"raw_summary,omitempty"`
}

type reportedTokenUsage struct {
	InputTokens  int
	OutputTokens int
}

func diagnostic(code, message string) Diagnostic {
	return Diagnostic{Code: code, Message: message}
}

func bindUsageReceipt(result *RunResult, spec RunSpec) {
	if result == nil {
		return
	}
	if spec.ResumeID != "" {
		if result.Usage != nil {
			result.Diagnostics = append(result.Diagnostics, diagnostic(
				"usage_resume_disabled",
				"CLI usage is not counted for resumed sessions",
			))
		}
		result.Usage = nil
		return
	}
	if result.Usage == nil {
		return
	}
	result.Usage.Source = UsageSourceCLIReported
	result.Usage.Scope = UsageScopeInvocation
	result.Usage.EngineVersion = strings.TrimSpace(spec.EngineVersion)
}

func newUsageReceipt(tokens *reportedTokenUsage, cost *float64, raw string) (*UsageReceipt, []Diagnostic) {
	var diagnostics []Diagnostic
	receipt := &UsageReceipt{
		Source:     UsageSourceCLIReported,
		Scope:      UsageScopeInvocation,
		RawSummary: truncateRawSummary(raw),
	}
	if tokens != nil {
		if tokens.InputTokens < 0 || tokens.OutputTokens < 0 {
			return nil, []Diagnostic{diagnostic("usage_invalid", "reported token counts must be non-negative")}
		}
		receipt.InputTokens = tokens.InputTokens
		receipt.OutputTokens = tokens.OutputTokens
		receipt.HasTokens = true
	} else {
		diagnostics = append(diagnostics, diagnostic("usage_tokens_unreported", "CLI did not report a complete token dimension"))
	}
	if cost != nil {
		if *cost < 0 || math.IsNaN(*cost) || math.IsInf(*cost, 0) {
			return nil, []Diagnostic{diagnostic("usage_invalid", "reported cost must be finite and non-negative")}
		}
		receipt.CostUSD = *cost
		receipt.HasCost = true
	} else {
		diagnostics = append(diagnostics, diagnostic("usage_cost_unreported", "CLI did not report the cost dimension"))
	}
	if !receipt.HasTokens && !receipt.HasCost {
		return nil, append(diagnostics, diagnostic("usage_missing", "CLI terminal event did not contain a usage receipt"))
	}
	return receipt, diagnostics
}

func truncateRawSummary(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) <= maxRawUsageSummary {
		return raw
	}
	return raw[:maxRawUsageSummary]
}

func appendRawSummary(current, line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return current
	}
	if current != "" {
		current += "\n"
	}
	return truncateRawSummary(current + line)
}

func decodeIntField(object map[string]json.RawMessage, key string) (*int, error) {
	raw, exists := object[key]
	if !exists || string(raw) == "null" {
		return nil, nil
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	if value < 0 {
		return nil, fmt.Errorf("%s must be non-negative", key)
	}
	return &value, nil
}

func decodeFloatField(object map[string]json.RawMessage, key string) (*float64, error) {
	raw, exists := object[key]
	if !exists || string(raw) == "null" {
		return nil, nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%s must be a number: %w", key, err)
	}
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, fmt.Errorf("%s must be finite and non-negative", key)
	}
	return &value, nil
}

func addReportedInt(left, right int) (int, error) {
	if right < 0 || left > int(^uint(0)>>1)-right {
		return 0, fmt.Errorf("reported token count overflows int")
	}
	return left + right, nil
}
