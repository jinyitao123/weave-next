package compiler

import (
	"context"
	"sync"

	"github.com/jinyitao123/loom/contract"
)

const CodeCompileTimeHostCall = "workflow_compile_time_host_call"

var ErrCompileTimeHostCall = &CompilerError{code: CodeCompileTimeHostCall}

type compileTimeHostViolation struct {
	mu     sync.Mutex
	sticky error
}

func (v *compileTimeHostViolation) record(err error) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.sticky == nil {
		v.sticky = err
	}
}

func (v *compileTimeHostViolation) err() error {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.sticky
}

// NoOpLLM rejects LLM execution during isolated candidate compilation.
type NoOpLLM struct {
	*compileTimeHostViolation
}

func (l *NoOpLLM) Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
	l.compileTimeHostViolation.record(ErrCompileTimeHostCall)
	return nil, ErrCompileTimeHostCall
}

func (l *NoOpLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	l.compileTimeHostViolation.record(ErrCompileTimeHostCall)
	return nil, ErrCompileTimeHostCall
}

// CompileGuardDispatcher exposes no tools and rejects compile-time dispatch.
type CompileGuardDispatcher struct {
	*compileTimeHostViolation
}

func (*CompileGuardDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	return nil, nil
}

func (d *CompileGuardDispatcher) Dispatch(context.Context, contract.ToolCall) (*contract.ToolResult, error) {
	d.compileTimeHostViolation.record(ErrCompileTimeHostCall)
	return nil, ErrCompileTimeHostCall
}
