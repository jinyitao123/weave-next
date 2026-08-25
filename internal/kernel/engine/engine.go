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
type RunSpec struct {
	WorkDir  string
	Prompt   string
	Model    string // e.g. "openai/gpt-5.5"; empty lets the CLI pick its default
	Env      map[string]string
	Timeout  time.Duration
	ResumeID string // resume a prior session (optional)
	// OutputSchema asks a supporting CLI to constrain its final message.
	OutputSchema json.RawMessage
}

// RunResult is a worker's terminal outcome.
type RunResult struct {
	Output    string
	SessionID string
	Status    string // "completed" | "failed" | "timeout"
	Err       string
}

// Event is one normalised item from a CLI's NDJSON stream. Adapters emit these
// so callers (and future steering) can observe a run without knowing each CLI's
// wire format.
type Event struct {
	Kind   string // text | thinking | tool_call | tool_result | error | log
	Text   string
	Tool   string
	CallID string
	Input  string
	Output string
}

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
