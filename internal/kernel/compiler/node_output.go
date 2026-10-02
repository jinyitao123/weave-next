package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

type nodeOutputSchemaKey struct{}

// WithNodeOutputSchema binds a frozen workflow node's schema to this invocation,
// never to a shared agent or model-controlled graph state. A nil schema clears
// an inherited binding for text nodes. Recovered calls bind the same frozen
// schema again before Loom restores its tool-loop policy.
func WithNodeOutputSchema(ctx context.Context, schema json.RawMessage) context.Context {
	return context.WithValue(ctx, nodeOutputSchemaKey{}, append(json.RawMessage(nil), schema...))
}

// NodeOutputViolation contains diagnostic metadata only. Neither logs nor run
// activity need to retain arbitrary model text (which can include materials).
type NodeOutputViolation struct {
	Code         string `json:"code"`
	Path         string `json:"path"`
	OutputSHA256 string `json:"output_sha256"`
	OutputBytes  int    `json:"output_bytes"`
	cause        error
}

func (e *NodeOutputViolation) Error() string {
	return fmt.Sprintf("node output violates contract: %s at %s", e.Code, e.Path)
}
func (e *NodeOutputViolation) Unwrap() error { return e.cause }

// ValidateNodeOutput uses the same validator as the workflow's final gate.
// Returning a bounded JSON pointer identifies a bad field without echoing its
// value or allowing arbitrary property names to inject long diagnostic text.
func ValidateNodeOutput(schema, output json.RawMessage) *NodeOutputViolation {
	_, problems := machine.ValidateRuntimeInput(schema, output)
	if len(problems) == 0 {
		return nil
	}
	return outputViolation(problems[0].Code, problems[0].Path, output)
}

func outputViolation(code, path string, output []byte) *NodeOutputViolation {
	digest := sha256.Sum256(output)
	return &NodeOutputViolation{Code: code, Path: diagnosticFieldPath(path), OutputSHA256: hex.EncodeToString(digest[:]), OutputBytes: len(output)}
}

func diagnosticFieldPath(path string) string {
	if path == "" {
		return "/"
	}
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if len(part) > 64 || strings.IndexFunc(part, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("_-~", r)
		}) >= 0 {
			parts[i] = "[field]"
		}
	}
	path = strings.Join(parts, "/")
	if len(path) > 192 {
		path = strings.ToValidUTF8(path[:192], "") + "…"
	}
	return path
}
