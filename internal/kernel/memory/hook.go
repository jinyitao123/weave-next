package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
)

const extractPrompt = `从以下对话中提取值得长期记住的关键事实。每行一条，只输出事实本身，不要编号。
如果没有值得记住的信息，只输出 NONE。

只提取以下类型的信息：
- 用户的偏好、角色、背景
- 重要的决策或结论
- 项目的关键约束或需求
- 需要在未来对话中记住的具体信息

不要提取：
- 临时性的调试信息
- 代码片段
- 显而易见的技术操作步骤

用户消息：
%s

助手回复：
%s`

// UpdateFunc is called after auto-remember stores new facts.
// count is the number of new facts stored.
type UpdateFunc func(count int)

type sessionExecutionLeaseContextKey struct{}

// ContextWithSessionExecutionLease binds one immutable lease handle to hooks
// launched by the current team-session run.
func ContextWithSessionExecutionLease(
	ctx context.Context,
	handle SessionExecutionLeaseHandle,
) context.Context {
	return context.WithValue(ctx, sessionExecutionLeaseContextKey{}, handle)
}

func sessionExecutionLeaseFromContext(
	ctx context.Context,
) (SessionExecutionLeaseHandle, bool) {
	handle, ok := ctx.Value(sessionExecutionLeaseContextKey{}).(SessionExecutionLeaseHandle)
	return handle, ok
}

// AutoRememberHook returns an AfterHook that extracts key facts from the
// conversation after the "chat" step and stores them as long-term memories.
//
// The extraction runs asynchronously to avoid blocking the response.
// If onUpdate is non-nil, it is called after facts are stored.
func AutoRememberHook(llm contract.LLM, memorySvc *Service, model, tenant, agent, scope string, onUpdate ...UpdateFunc) loom.StepHook {
	return autoRememberHook(llm, memorySvc, model, tenant, agent, scope, "", "auto", onUpdate...)
}

// AutoRememberHookForNamespace stores extracted facts in one caller-resolved
// immutable namespace. It is used by Project conversations so facts never
// spill into another Project or the Avatar's general memory.
func AutoRememberHookForNamespace(
	llm contract.LLM,
	memorySvc *Service,
	model, tenant, agent, scope, namespace, metadataSource string,
	onUpdate ...UpdateFunc,
) loom.StepHook {
	return autoRememberHook(
		llm, memorySvc, model, tenant, agent, scope, namespace, metadataSource, onUpdate...,
	)
}

func autoRememberHook(
	llm contract.LLM,
	memorySvc *Service,
	model, tenant, agent, scope, namespace, metadataSource string,
	onUpdate ...UpdateFunc,
) loom.StepHook {
	if metadataSource == "" {
		metadataSource = "auto"
	}
	return func(ctx context.Context, stepName string, state loom.State) error {
		if stepName != "chat" {
			return nil
		}

		output, _ := state["output"].(string)
		userMsg, _ := state["last_user_message"].(string)
		if output == "" || userMsg == "" {
			return nil
		}

		// Skip very short exchanges (unlikely to contain memorable facts).
		if len(userMsg) < 10 && len(output) < 50 {
			return nil
		}

		userID, _ := state["user_id"].(string)
		sessionID, _ := state["session_id"].(string)
		ns := namespace
		if ns == "" {
			ns = ScopedNamespace(tenant, userID, agent, sessionID, scope)
		}
		leaseHandle, fenced := sessionExecutionLeaseFromContext(ctx)

		// Run extraction asynchronously with a timeout to prevent goroutine leaks.
		go func() {
			bgCtx, bgCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer bgCancel()

			prompt := fmt.Sprintf(extractPrompt, userMsg, output)

			resp, err := llm.Chat(bgCtx, contract.ChatRequest{
				Model: model,
				Messages: []contract.Message{
					{Role: "user", Content: prompt},
				},
				Effort: contract.EffortLow,
			})
			if err != nil {
				slog.Warn("auto-remember: extraction failed", "error", err)
				return
			}

			text := strings.TrimSpace(resp.Content)
			if text == "" || text == "NONE" || strings.HasPrefix(text, "NONE") {
				return
			}

			lines := strings.Split(text, "\n")
			stored := 0
			for _, line := range lines {
				fact := strings.TrimSpace(line)
				// Remove leading markers like "- ", "* ", "1. "
				fact = strings.TrimLeft(fact, "-*•")
				fact = strings.TrimSpace(fact)
				if len(fact) < 5 {
					continue
				}

				var isNew bool
				var err error
				if fenced {
					_, isNew, err = memorySvc.RememberIfNewFenced(
						bgCtx, leaseHandle, ns, fact, map[string]any{"source": metadataSource},
					)
				} else {
					_, isNew, err = memorySvc.RememberIfNew(
						bgCtx, ns, fact, map[string]any{"source": metadataSource},
					)
				}
				if err != nil {
					slog.Warn("auto-remember: store failed", "error", err, "fact", fact)
					continue
				}
				if isNew {
					stored++
				} else {
					slog.Debug("auto-remember: deduplicated", "fact", fact)
				}
			}

			if stored > 0 {
				slog.Info("auto-remember: stored facts", "agent", agent, "count", stored)
				if len(onUpdate) > 0 && onUpdate[0] != nil {
					onUpdate[0](stored)
				}
			}
		}()

		return nil
	}
}
