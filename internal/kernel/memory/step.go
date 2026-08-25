package memory

import (
	"context"
	"strings"

	"github.com/jinyitao123/loom"
)

// RetrieveConfig configures the memory retrieval step.
type RetrieveConfig struct {
	Service           *Service
	TopK              int    // default 5
	Scope             string // "tenant" (default) | "user" | "session"
	AdditionalSources []RetrieveSource
}

// RetrieveSource is an explicitly scoped memory source appended after the
// Agent's normal scoped memory. Callers use this for immutable Project memory
// namespaces without letting runtime state construct arbitrary namespaces.
type RetrieveSource struct {
	Namespace string
	Label     string
	Source    string
}

// NewRetrieveStep creates a step that recalls relevant memories
// and appends them to the system prompt (__system_prompt in state).
func NewRetrieveStep(cfg RetrieveConfig) loom.Step {
	if cfg.TopK <= 0 {
		cfg.TopK = 5
	}

	return func(ctx context.Context, state loom.State) (loom.State, error) {
		userMsg, _ := state["last_user_message"].(string)
		if userMsg == "" {
			return loom.State{}, nil
		}

		tenant, _ := state["tenant"].(string)
		agentName, _ := state["agent_name"].(string)
		if tenant == "" || agentName == "" {
			return loom.State{}, nil
		}

		userID, _ := state["user_id"].(string)
		sessionID, _ := state["session_id"].(string)
		sources := []RetrieveSource{{
			Namespace: ScopedNamespace(tenant, userID, agentName, sessionID, cfg.Scope),
			Label:     "Avatar 通用记忆",
			Source:    "avatar",
		}}
		sources = append(sources, cfg.AdditionalSources...)

		var sections []string
		for _, source := range sources {
			if source.Namespace == "" {
				continue
			}
			memories, err := cfg.Service.Recall(ctx, source.Namespace, userMsg, cfg.TopK)
			if err != nil || len(memories) == 0 {
				// Memory recall failure is non-fatal; other isolated sources still
				// get a chance to contribute.
				continue
			}
			label := strings.TrimSpace(source.Label)
			if label == "" {
				label = "相关记忆"
			}
			marker := strings.TrimSpace(source.Source)
			parts := make([]string, 0, len(memories))
			for _, item := range memories {
				prefix := ""
				if marker != "" {
					prefix = "[" + marker + "] "
				}
				parts = append(parts, "- "+prefix+item.Content)
			}
			sections = append(sections, "## "+label+"\n"+strings.Join(parts, "\n"))
		}
		if len(sections) == 0 {
			return loom.State{}, nil
		}
		memoryBlock := "\n\n" + strings.Join(sections, "\n\n")

		existingPrompt, _ := state["__system_prompt"].(string)
		return loom.State{
			"__system_prompt": existingPrompt + memoryBlock,
		}, nil
	}
}
