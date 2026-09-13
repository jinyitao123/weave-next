// Package engine runs a digital worker on an external CLI agent runtime
// (opencode / codex / claude), as opposed to the in-process loom engine used by
// avatars. A backend materialises an exec environment on disk, spawns the CLI as
// a subprocess, and parses its NDJSON output into a result.
//
// Platform boundary: this package carries zero customer business terms. MCP
// endpoints, headers and instructions all arrive via the caller's RunSpec, which
// is populated from an agent's DB config — never hard-coded here.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Engine names. The empty string and "loom" are handled in-process by the
// caller and never reach this package.
const (
	OpenCode = "opencode"
	Codex    = "codex"
	Claude   = "claude"
)

// ErrUnsupported is returned by New for an engine with no registered backend.
var ErrUnsupported = errors.New("engine: unsupported runtime")

// RunSpec is one worker invocation. WorkDir is materialised by the caller
// (execenv); Env carries provider credentials and per-run WEAVE_* values.
// MCPServerEndpoint contains only a task gateway and the name of its token
// environment variable. It never contains upstream or runtime credentials.
type MCPServerEndpoint struct {
	URL      string
	TokenEnv string
}

type RunSpec struct {
	// DisableTools is derived from the frozen deny-all permission policy. It
	// requests a model-only invocation, including disabling inherited host MCP.
	DisableTools bool
	MCPServers   []MCPServerEndpoint
	WorkDir      string
	Prompt       string
	Model        string // e.g. "openai/gpt-5.5"; empty lets the CLI pick its default
	Env          map[string]string
	Timeout      time.Duration
	ResumeID     string // resume a prior session (optional)
	// EngineVersion binds a CLI-reported usage receipt to the binary observed
	// by the runtime capability probe. Callers must pass the exact advertised
	// version rather than guessing it from the wire format.
	EngineVersion string
	// OutputSchema asks a supporting CLI to constrain its final message.
	OutputSchema json.RawMessage
	// OnPublicEvent receives only publicly emitted CLI messages and tools.
	// It must return promptly and must never receive reasoning items.
	OnPublicEvent func(Event, bool)
}

// RunResult is a worker's terminal outcome.
type RunResult struct {
	Output                   string
	ReportedModels           []string
	RetrySafeBeforeExecution bool
	SessionID                string
	Status                   string // "completed" | "failed" | "timeout"
	Err                      string
	Usage                    *UsageReceipt
	Diagnostics              []Diagnostic
	// Events are bounded, CLI-observed execution facts. They are carried to the
	// TeamRun activity ledger; callers must not infer events that the CLI did not
	// report.
	Events []Event
	// Artifacts are bounded delivery files written by this invocation. Runtime
	// adapters collect outputs/ and explicitly referenced root files so the server
	// can persist real content; the workflow decides final versus failed evidence.
	Artifacts []Artifact
	// ArtifactCollection records independent collection observations without
	// rewriting the original engine status, session, error, or usage.
	ArtifactCollection *fileartifact.CollectionEvidence
	// Attempts is populated by retrying remote executors. Each durable task ID
	// appears at most once so downstream accumulators can deduplicate replays
	// without collapsing distinct physical spend.
	Attempts []UsageAttempt
}

// UsageAttempt binds one physical CLI invocation to its durable task/runtime.
type UsageAttempt struct {
	AttemptID   string
	RuntimeID   string
	Engine      string
	Status      string
	Usage       *UsageReceipt
	Diagnostics []Diagnostic
	Events      []Event
}

// Diagnostic reports a best-effort CLI parsing problem without turning a
// successfully produced worker answer into a failed run.
type Diagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Event is one normalised item from a CLI's NDJSON stream. Adapters emit these
// so callers (and future steering) can observe a run without knowing each CLI's
// wire format.
type Event struct {
	Kind   string `json:"kind"` // text | thinking | tool_call | tool_result | error | log
	Text   string `json:"text,omitempty"`
	Tool   string `json:"tool,omitempty"`
	CallID string `json:"call_id,omitempty"`
	Status string `json:"status,omitempty"`
	Input  string `json:"input,omitempty"`
	Output string `json:"output,omitempty"`
}

// Artifact is one user-visible UTF-8 file collected by the runtime. Path is a
// relative delivery name and never exposes a host path.
type Artifact = fileartifact.File

// Backend runs one worker on a specific CLI runtime.
type Backend interface {
	Name() string
	// Run blocks until the CLI finishes one turn and returns its result.
	// A cancelled ctx or an elapsed RunSpec.Timeout kills the process group.
	Run(ctx context.Context, spec RunSpec) (RunResult, error)
}

// New returns the backend for name, or ErrUnsupported. cliPath is the resolved
// executable path (see config.ResolveEngineCLIPath).
func New(name, cliPath string) (Backend, error) {
	switch name {
	case OpenCode:
		return &opencodeBackend{cliPath: cliPath}, nil
	case Codex:
		return &codexBackend{cliPath: cliPath}, nil
	case Claude:
		return &claudeBackend{cliPath: cliPath}, nil
	default:
		return nil, ErrUnsupported
	}
}

// IsCLIEngine reports whether name designates an external CLI runtime (as
// opposed to "" / "loom", which run in-process).
func IsCLIEngine(name string) bool {
	switch name {
	case OpenCode, Codex, Claude:
		return true
	default:
		return false
	}
}

// envWithCLIPath makes an explicitly resolved CLI self-contained enough to
// launch npm-style wrappers under service managers whose PATH omits the CLI's
// installation directory. This is common for macOS launch agents: LookPath
// can resolve codex during capability discovery while /usr/bin/env cannot
// later find the adjacent node binary from a narrowed task environment.
func envWithCLIPath(env []string, cliPath string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !platformCredentialEnv(key) {
			filtered = append(filtered, entry)
		}
	}
	env = filtered
	if !filepath.IsAbs(cliPath) {
		return env
	}
	dir := filepath.Dir(cliPath)
	pathValue := ""
	pathIndex := -1
	for index, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			pathIndex = index
			pathValue = strings.TrimPrefix(entry, "PATH=")
			break
		}
	}
	for _, existing := range filepath.SplitList(pathValue) {
		if existing == dir {
			return env
		}
	}
	updated := dir
	if pathValue != "" {
		updated += string(os.PathListSeparator) + pathValue
	}
	if pathIndex >= 0 {
		result := append([]string(nil), env...)
		result[pathIndex] = "PATH=" + updated
		return result
	}
	return append(append([]string(nil), env...), "PATH="+updated)
}

// CLI workers receive their scoped invocation values through RunSpec.Env.
// Ambient task tokens and platform credentials belong to the parent service.
func cliAmbientEnv() []string {
	result := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if platformCredentialEnv(key) || strings.HasPrefix(key, "WEAVE_MCP_BOUNDARY_TOKEN_") {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func platformCredentialEnv(key string) bool {
	switch key {
	case "WEAVE_RUNTIME_TOKEN", "WEAVE_RUNTIME_TOKEN_FILE", "WEAVE_SECRET_KEY", "WEAVE_SECRET_KEY_FILE", "JWT_SECRET", "DATABASE_URL", "TEST_DATABASE_URL", "WEAVE_LIVE_DATABASE_URL":
		return true
	}
	return false
}
