package workflow

import (
	"context"
	"io"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
)

type observingFactoryTestLLM struct{ name string }

func (observingFactoryTestLLM) Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
	return nil, nil
}

func (observingFactoryTestLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, nil
}

type observingFactoryTestTools struct{}

func (observingFactoryTestTools) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{{Name: "test_tool"}}, nil
}

func (observingFactoryTestTools) Dispatch(context.Context, contract.ToolCall) (*contract.ToolResult, error) {
	return &contract.ToolResult{}, nil
}

type observingFactoryTestCloser struct{}

func (observingFactoryTestCloser) Close() error { return nil }

type observingFactoryTestFactory struct {
	buildCalls        int
	buildWithLLMCalls int
	tools             contract.ToolDispatcher
	closer            io.Closer
	originalHookCalls int
}

func (f *observingFactoryTestFactory) Build(context.Context, frozen.FrozenExecutionBundle, RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
	f.buildCalls++
	return f.options(observingFactoryTestLLM{name: "inner"}), f.closer, nil
}

func (f *observingFactoryTestFactory) BuildWithLLM(_ context.Context, _ frozen.FrozenExecutionBundle, _ RuntimeCredentialResolver, llm contract.LLM) (compiler.FrozenBuildOpts, io.Closer, error) {
	f.buildWithLLMCalls++
	return f.options(llm), f.closer, nil
}

func (f *observingFactoryTestFactory) options(llm contract.LLM) compiler.FrozenBuildOpts {
	originalHook := contract.ToolHook{
		Pre: func(_ context.Context, call contract.ToolCall) (contract.ToolCall, error) {
			f.originalHookCalls++
			return call, nil
		},
		Post: func(context.Context, contract.ToolCall, *contract.ToolResult) error {
			f.originalHookCalls++
			return nil
		},
	}
	return compiler.FrozenBuildOpts{
		LLM: llm, Tools: f.tools,
		Hooks: compiler.FrozenHookPoints{ToolHooks: []contract.ToolHook{originalHook}},
	}
}

func TestObserveRuntimeToolsPreservesBuildWithLLM(t *testing.T) {
	for _, withLLM := range []bool{false, true} {
		name := "Build"
		if withLLM {
			name = "BuildWithLLM"
		}
		t.Run(name, func(t *testing.T) {
			tools := observingFactoryTestTools{}
			closer := observingFactoryTestCloser{}
			inner := &observingFactoryTestFactory{tools: tools, closer: closer}
			var events []RuntimeToolEvent
			wrapped := ObserveRuntimeTools(inner, func(_ context.Context, event RuntimeToolEvent) {
				events = append(events, event)
			})
			withLLMFactory, ok := wrapped.(RuntimeHostFactoryWithLLM)
			if !ok {
				t.Fatal("observer wrapper dropped RuntimeHostFactoryWithLLM")
			}

			wantLLM := contract.LLM(observingFactoryTestLLM{name: "inner"})
			if withLLM {
				wantLLM = observingFactoryTestLLM{name: "assigned"}
			}
			var opts compiler.FrozenBuildOpts
			var gotCloser io.Closer
			var err error
			if withLLM {
				opts, gotCloser, err = withLLMFactory.BuildWithLLM(context.Background(), frozen.FrozenExecutionBundle{}, nil, wantLLM)
			} else {
				opts, gotCloser, err = wrapped.Build(context.Background(), frozen.FrozenExecutionBundle{}, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			if opts.LLM != wantLLM {
				t.Fatal("wrapped factory did not preserve the selected LLM")
			}
			if opts.Tools != tools {
				t.Fatal("wrapped factory did not preserve the tool dispatcher")
			}
			if gotCloser != closer {
				t.Fatal("wrapped factory did not preserve the closer")
			}
			if len(opts.Hooks.ToolHooks) != 2 {
				t.Fatalf("tool hook count = %d, want original plus one observer hook", len(opts.Hooks.ToolHooks))
			}
			call := contract.ToolCall{ID: "call-1", Name: "test_tool"}
			for _, hook := range opts.Hooks.ToolHooks {
				if hook.Pre != nil {
					if _, err := hook.Pre(context.Background(), call); err != nil {
						t.Fatal(err)
					}
				}
				if hook.Post != nil {
					if err := hook.Post(context.Background(), call, &contract.ToolResult{IsError: true}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if inner.originalHookCalls != 2 {
				t.Fatalf("original hook calls = %d, want 2", inner.originalHookCalls)
			}
			if len(events) != 2 || events[0].Kind != "tool_started" || events[1].Kind != "tool_completed" ||
				events[0].Tool != call.Name || events[1].Tool != call.Name || !events[1].ResultError {
				t.Fatalf("observer events = %+v", events)
			}
			if withLLM {
				if inner.buildCalls != 0 || inner.buildWithLLMCalls != 1 {
					t.Fatalf("inner calls = Build:%d BuildWithLLM:%d", inner.buildCalls, inner.buildWithLLMCalls)
				}
			} else if inner.buildCalls != 1 || inner.buildWithLLMCalls != 0 {
				t.Fatalf("inner calls = Build:%d BuildWithLLM:%d", inner.buildCalls, inner.buildWithLLMCalls)
			}
		})
	}
}

func TestObserveRuntimeToolsDoesNotAddBuildWithLLMCapability(t *testing.T) {
	inner := RuntimeHostFactoryFunc(func(context.Context, frozen.FrozenExecutionBundle, RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
		return compiler.FrozenBuildOpts{}, observingFactoryTestCloser{}, nil
	})
	wrapped := ObserveRuntimeTools(inner, func(context.Context, RuntimeToolEvent) {})
	if _, ok := wrapped.(RuntimeHostFactoryWithLLM); ok {
		t.Fatal("observer wrapper added BuildWithLLM to a factory that does not support it")
	}
}
