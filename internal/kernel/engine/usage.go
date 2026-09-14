package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// BinaryVersion observes the exact CLI version used for a local invocation.
// "unavailable" is explicit metadata, never an inferred version.
func BinaryVersion(ctx context.Context, binaryPath string) string {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, binaryPath, "--version")
	// A version probe is capability discovery, not an engine invocation. Keep
	// provider keys, runtime tokens, user config paths and task credentials out
	// of the probe so a legacy Host cannot observe the operator's identity.
	cmd.Env = envWithCLIPath(versionProbeEnv(), binaryPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "unavailable"
	}
	version := strings.TrimSpace(string(output))
	if line, _, found := strings.Cut(version, "\n"); found {
		version = strings.TrimSpace(line)
	}
	if len(version) > 160 {
		version = version[:160]
	}
	if version == "" {
		return "unavailable"
	}
	return version
}

func versionProbeEnv() []string {
	allowed := map[string]bool{
		"PATH": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true,
		"TZ": true, "TERM": true, "SHELL": true, "SSL_CERT_FILE": true,
		"SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
	}
	result := make([]string, 0, len(allowed))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if allowed[key] {
			result = append(result, entry)
		}
	}
	return result
}

// ValidateUsageReceipt checks the untrusted daemon boundary. It validates
// reported values and provenance but never tries to verify the CLI's honesty.
func ValidateUsageReceipt(receipt *UsageReceipt) error {
	if receipt == nil {
		return nil
	}
	if receipt.Source != UsageSourceCLIReported {
		return fmt.Errorf("usage source must be %q", UsageSourceCLIReported)
	}
	if receipt.Scope != UsageScopeInvocation && receipt.Scope != UsageScopeSession {
		return fmt.Errorf("usage scope is invalid")
	}
	if strings.TrimSpace(receipt.EngineVersion) == "" || receipt.EngineVersion == "unavailable" || len(receipt.EngineVersion) > 160 {
		return fmt.Errorf("usage engine_version is invalid")
	}
	if !receipt.HasTokens && !receipt.HasCost {
		return fmt.Errorf("usage receipt has no reported dimension")
	}
	if receipt.InputTokens < 0 || receipt.OutputTokens < 0 {
		return fmt.Errorf("usage token counts must be non-negative")
	}
	if !receipt.HasTokens && (receipt.InputTokens != 0 || receipt.OutputTokens != 0) {
		return fmt.Errorf("unreported token dimension must be zero-valued")
	}
	if receipt.CostUSD < 0 || math.IsNaN(receipt.CostUSD) || math.IsInf(receipt.CostUSD, 0) {
		return fmt.Errorf("usage cost must be finite and non-negative")
	}
	if !receipt.HasCost && receipt.CostUSD != 0 {
		return fmt.Errorf("unreported cost dimension must be zero-valued")
	}
	if len(receipt.RawSummary) > maxRawUsageSummary {
		return fmt.Errorf("usage raw_summary exceeds %d bytes", maxRawUsageSummary)
	}
	return nil
}

// ValidateDiagnostics bounds the untrusted diagnostic channel.
func ValidateDiagnostics(diagnostics []Diagnostic) error {
	if len(diagnostics) > 32 {
		return fmt.Errorf("too many engine diagnostics")
	}
	for index, item := range diagnostics {
		if strings.TrimSpace(item.Code) == "" || len(item.Code) > 80 || len(item.Message) > 1024 {
			return fmt.Errorf("engine diagnostic %d is invalid", index)
		}
	}
	return nil
}

// ValidateEvents bounds the untrusted runtime activity channel. Event input
// and output are previews, not an alternate artifact transport.
func ValidateEvents(events []Event) error {
	if len(events) > 200 {
		return fmt.Errorf("too many engine events")
	}
	for index, event := range events {
		switch event.Kind {
		case "text", "thinking", "tool_call", "tool_result", "error", "log":
		default:
			return fmt.Errorf("engine event %d kind is invalid", index)
		}
		if len(event.Text) > 4096 || len(event.Tool) > 160 || len(event.CallID) > 200 ||
			len(event.Input) > 4096 || len(event.Output) > 4096 {
			return fmt.Errorf("engine event %d exceeds its size bound", index)
		}
		if event.Status != "" && event.Status != "running" && event.Status != "ok" && event.Status != "error" {
			return fmt.Errorf("engine event %d status is invalid", index)
		}
		if !utf8.ValidString(event.Text) || !utf8.ValidString(event.Tool) || !utf8.ValidString(event.CallID) ||
			!utf8.ValidString(event.Input) || !utf8.ValidString(event.Output) {
			return fmt.Errorf("engine event %d is not valid UTF-8", index)
		}
	}
	return nil
}

const (
	MaxArtifactCount       = fileartifact.MaxArtifactCount
	MaxArtifactBytes       = fileartifact.MaxArtifactBytes
	MaxArtifactsTotalBytes = fileartifact.MaxArtifactsTotalBytes
)

// ReferencesArtifact recognizes an explicit final-answer file reference, not a
// filename embedded in another path or a prose substring.
func ReferencesArtifact(output, name string) bool {
	pattern := "(^|[\\s`\"'\\[(])" + regexp.QuoteMeta(name) + "($|[\\s`\"'\\]),:;!?]|\\.(?:\\s|$))"
	return regexp.MustCompile(pattern).MatchString(output)
}

// ValidateArtifacts verifies runtime-produced files before accepting a receipt.
func ValidateArtifacts(artifacts []Artifact) error { return fileartifact.Validate(artifacts) }

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
	version := strings.TrimSpace(spec.EngineVersion)
	if version == "" || version == "unavailable" {
		result.Diagnostics = append(result.Diagnostics, diagnostic(
			"usage_engine_version_unavailable",
			"CLI usage receipt is discarded because the binary version is unavailable",
		))
		result.Usage = nil
		return
	}
	result.Usage.Source = UsageSourceCLIReported
	result.Usage.Scope = UsageScopeInvocation
	result.Usage.EngineVersion = version
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
	raw = raw[:maxRawUsageSummary]
	for !utf8.ValidString(raw) {
		raw = raw[:len(raw)-1]
	}
	return raw
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
