package loomruntime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
)

// ErrUsageStreamUnsupported prevents a graph from bypassing the synchronous
// usage boundary. Transport and management calls without a run scope remain
// transparent.
var ErrUsageStreamUnsupported = errors.New("stream is unsupported in a bound usage scope")

type usageRunScopeContextKey struct{}
type usageCallFrameContextKey struct{}

type attemptIDGenerator func() (string, error)

type usageRunScope struct {
	mu          sync.Mutex
	bound       bool
	runID       string
	step        string
	state       loom.State
	accumulator UsageAccumulator
}

type usageCallFrame struct {
	mu        sync.Mutex
	scope     *usageRunScope
	callID    string
	attemptID string
	generate  attemptIDGenerator
	reported  bool
}

// PhysicalUsageReport is an out-of-band receipt for contract.LLM adapters
// whose underlying transport can expose retries and failed-attempt spend.
type PhysicalUsageReport struct {
	Usage     contract.Usage
	ToolCalls int
	HasTokens bool
	HasCost   bool
	Source    string
}

func withUsageRunScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, usageRunScopeContextKey{}, &usageRunScope{})
}

func usageScopeFromContext(ctx context.Context) (*usageRunScope, bool) {
	scope, ok := ctx.Value(usageRunScopeContextKey{}).(*usageRunScope)
	return scope, ok && scope != nil
}

func usageCallFrameFromContext(ctx context.Context) (*usageCallFrame, bool) {
	frame, ok := ctx.Value(usageCallFrameContextKey{}).(*usageCallFrame)
	return frame, ok && frame != nil
}

// RestartUsageAttempt starts another physical request under the current
// logical call. It is a no-op outside a PreparedRun usage scope so standalone
// transport adapters retain their existing behavior.
func RestartUsageAttempt(ctx context.Context) error {
	scope, hasScope := usageScopeFromContext(ctx)
	if !hasScope {
		return nil
	}
	frame, hasFrame := usageCallFrameFromContext(ctx)
	if !hasFrame {
		return fmt.Errorf("usage call frame is required to restart an attempt")
	}
	if frame.scope != scope {
		return fmt.Errorf("%w: usage call frame belongs to another run scope", ErrUsageConflict)
	}
	return frame.startNextAttempt()
}

// ReportPhysicalUsage confirms one or more physical transport attempts under
// the current logical LLM call. It is a no-op outside a bound usage scope.
// Adapters must call it before returning an execution error so failed spend is
// checkpointed. The ordinary response.Usage confirmation is then suppressed.
func ReportPhysicalUsage(ctx context.Context, reports []PhysicalUsageReport) error {
	scope, hasScope := usageScopeFromContext(ctx)
	if !hasScope {
		return nil
	}
	frame, hasFrame := usageCallFrameFromContext(ctx)
	if !hasFrame || frame.scope != scope {
		return fmt.Errorf("usage call frame is required to report physical usage")
	}
	for index, report := range reports {
		if index > 0 {
			if err := frame.startNextAttempt(); err != nil {
				return err
			}
		}
		frame.mu.Lock()
		attemptID := frame.attemptID
		frame.mu.Unlock()
		if attemptID == "" {
			return fmt.Errorf("usage physical attempt is not started")
		}
		if err := scope.confirmWithMetadata(frame.callID, attemptID, report.Usage, report.ToolCalls, UsageAttemptMetadata{
			HasTokens: report.HasTokens, HasCost: report.HasCost, Source: report.Source,
		}); err != nil {
			return err
		}
	}
	frame.mu.Lock()
	frame.reported = len(reports) > 0
	frame.mu.Unlock()
	return nil
}

func (f *usageCallFrame) startNextAttempt() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scope == nil || f.callID == "" || f.generate == nil {
		return fmt.Errorf("usage call frame is incomplete")
	}
	attemptID, err := f.scope.startAttempt(f.callID, f.generate)
	if err != nil {
		return err
	}
	f.attemptID = attemptID
	return nil
}

func bindUsageBeforeStep(ctx context.Context, step string, state loom.State) error {
	scope, ok := usageScopeFromContext(ctx)
	if !ok {
		return nil
	}
	if step == "" {
		return fmt.Errorf("usage step is required")
	}
	runID, ok := state["__run_id"].(string)
	if !ok || runID == "" {
		return fmt.Errorf("usage run_id is required")
	}
	accumulator, err := LoadUsageAccumulator(state)
	if err != nil {
		return fmt.Errorf("load usage accumulator: %w", err)
	}
	if owner := accumulator.ownedRunID(); owner != "" && owner != runID {
		return fmt.Errorf(
			"%w: accumulator belongs to run_id %q, not %q",
			ErrUsageConflict,
			owner,
			runID,
		)
	}

	scope.mu.Lock()
	defer scope.mu.Unlock()
	scope.bound = true
	scope.runID = runID
	scope.step = step
	scope.state = state
	scope.accumulator = accumulator
	return nil
}

func (s *usageRunScope) nextCall() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.bound || s.state == nil {
		return "", fmt.Errorf("usage scope is not bound to a graph step")
	}
	callID, err := s.accumulator.NextCall(s.runID, s.step)
	if err != nil {
		return "", err
	}
	if err := StoreUsageAccumulator(s.state, s.accumulator); err != nil {
		return "", fmt.Errorf("store usage logical call: %w", err)
	}
	return callID, nil
}

func (s *usageRunScope) startAttempt(
	callID string,
	generate attemptIDGenerator,
) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.bound || s.state == nil {
		return "", fmt.Errorf("usage scope is not bound to a graph step")
	}
	if generate == nil {
		return "", fmt.Errorf("usage attempt_id generator is required")
	}
	attemptID, err := generate()
	if err != nil {
		return "", fmt.Errorf("generate usage attempt_id: %w", err)
	}
	if err := validateUsageAttemptID(attemptID); err != nil {
		return "", err
	}
	if owner, exists := s.accumulator.attemptOwners[attemptID]; exists {
		return "", fmt.Errorf(
			"%w: attempt_id %q already belongs to %q",
			ErrUsageConflict,
			attemptID,
			owner,
		)
	}
	if err := s.accumulator.StartAttempt(callID, attemptID); err != nil {
		return "", err
	}
	if err := StoreUsageAccumulator(s.state, s.accumulator); err != nil {
		return "", fmt.Errorf("store usage attempt: %w", err)
	}
	return attemptID, nil
}

func (s *usageRunScope) confirm(
	callID string,
	attemptID string,
	usage contract.Usage,
	toolCalls int,
) error {
	return s.confirmWithMetadata(callID, attemptID, usage, toolCalls, UsageAttemptMetadata{
		HasTokens: true, HasCost: true,
	})
}

func (s *usageRunScope) confirmWithMetadata(
	callID string,
	attemptID string,
	usage contract.Usage,
	toolCalls int,
	metadata UsageAttemptMetadata,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.bound || s.state == nil {
		return fmt.Errorf("usage scope is not bound to a graph step")
	}
	if err := s.accumulator.ConfirmAttemptWithMetadata(callID, attemptID, usage, toolCalls, metadata); err != nil {
		return err
	}
	if err := StoreUsageAccumulator(s.state, s.accumulator); err != nil {
		return fmt.Errorf("store confirmed usage: %w", err)
	}
	return nil
}

func randomUsageAttemptID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("read random usage attempt_id: %w", err)
	}
	return "ua1_" + hex.EncodeToString(random[:]), nil
}

// WithUsageRunScope returns a context bound to a fresh logical usage scope.
// It mirrors the scope PreparedRun installs before graph execution and is the
// entry point for frozen graph runners that want the same logical
// accumulator, before-step binding, and physical usage boundary.
func WithUsageRunScope(ctx context.Context) context.Context {
	return withUsageRunScope(ctx)
}

// BindUsageBeforeStep binds the current graph step to the run's usage scope
// for frozen graphs. It is a no-op outside a usage-scoped run, so frozen
// graphs executed without WithUsageRunScope keep their previous behavior
// exactly.
func BindUsageBeforeStep(ctx context.Context, step string, state loom.State) error {
	return bindUsageBeforeStep(ctx, step, state)
}

// NewUsageBoundaryLLM wraps the physical execution LLM at the confirmed-usage
// boundary. generate supplies one fresh physical attempt id per request.
func NewUsageBoundaryLLM(
	inner contract.LLM,
	generate func() (string, error),
) contract.LLM {
	return newUsageBoundaryLLM(inner, generate)
}

// NewRandomUsageAttemptID returns a fresh random physical usage attempt id.
func NewRandomUsageAttemptID() (string, error) {
	return randomUsageAttemptID()
}

func validateUsageAttemptID(attemptID string) error {
	if len(attemptID) != len("ua1_")+32 || !strings.HasPrefix(attemptID, "ua1_") {
		return fmt.Errorf("invalid usage attempt_id %q", attemptID)
	}
	encoded := attemptID[len("ua1_"):]
	if encoded != strings.ToLower(encoded) {
		return fmt.Errorf("invalid usage attempt_id %q", attemptID)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return fmt.Errorf("invalid usage attempt_id %q", attemptID)
	}
	return nil
}

type logicalUsageLLM struct {
	inner contract.LLM
}

func newLogicalUsageLLM(inner contract.LLM) contract.LLM {
	return &logicalUsageLLM{inner: inner}
}

// NewLogicalUsageLLM wraps a graph-facing LLM with the logical-call wrapper
// that allocates one stable logical call per chat invocation inside a bound
// usage scope. Outside a scope it forwards transparently.
func NewLogicalUsageLLM(inner contract.LLM) contract.LLM {
	return newLogicalUsageLLM(inner)
}

func (l *logicalUsageLLM) Chat(
	ctx context.Context,
	request contract.ChatRequest,
) (*contract.ChatResponse, error) {
	scope, ok := usageScopeFromContext(ctx)
	if !ok {
		return l.inner.Chat(ctx, request)
	}
	callID, err := scope.nextCall()
	if err != nil {
		return nil, err
	}
	frame := &usageCallFrame{scope: scope, callID: callID}
	return l.inner.Chat(
		context.WithValue(ctx, usageCallFrameContextKey{}, frame),
		request,
	)
}

func (l *logicalUsageLLM) Stream(
	ctx context.Context,
	request contract.ChatRequest,
) (<-chan contract.StreamChunk, error) {
	if _, ok := usageScopeFromContext(ctx); ok {
		return nil, ErrUsageStreamUnsupported
	}
	return l.inner.Stream(ctx, request)
}

type usageBoundaryLLM struct {
	inner    contract.LLM
	generate attemptIDGenerator
}

func newUsageBoundaryLLM(
	inner contract.LLM,
	generate attemptIDGenerator,
) contract.LLM {
	return &usageBoundaryLLM{inner: inner, generate: generate}
}

func (l *usageBoundaryLLM) Chat(
	ctx context.Context,
	request contract.ChatRequest,
) (*contract.ChatResponse, error) {
	scope, ok := usageScopeFromContext(ctx)
	if !ok {
		return l.inner.Chat(ctx, request)
	}

	frame, hasFrame := usageCallFrameFromContext(ctx)
	if !hasFrame {
		callID, err := scope.nextCall()
		if err != nil {
			return nil, err
		}
		frame = &usageCallFrame{
			scope: scope, callID: callID, generate: l.generate,
		}
		ctx = context.WithValue(ctx, usageCallFrameContextKey{}, frame)
	} else if frame.scope != scope {
		return nil, fmt.Errorf("%w: usage call frame belongs to another run scope", ErrUsageConflict)
	} else {
		frame.mu.Lock()
		if frame.generate == nil {
			frame.generate = l.generate
		}
		frame.mu.Unlock()
	}

	if err := frame.startNextAttempt(); err != nil {
		return nil, err
	}
	response, err := l.inner.Chat(ctx, request)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, fmt.Errorf("LLM returned a nil response without an error")
	}
	frame.mu.Lock()
	confirmedAttemptID := frame.attemptID
	reported := frame.reported
	frame.mu.Unlock()
	if !reported {
		if err := scope.confirm(frame.callID, confirmedAttemptID, response.Usage, len(response.ToolCalls)); err != nil {
			return nil, err
		}
	}
	return response, nil
}

func (l *usageBoundaryLLM) Stream(
	ctx context.Context,
	request contract.ChatRequest,
) (<-chan contract.StreamChunk, error) {
	if _, ok := usageScopeFromContext(ctx); ok {
		return nil, ErrUsageStreamUnsupported
	}
	return l.inner.Stream(ctx, request)
}
