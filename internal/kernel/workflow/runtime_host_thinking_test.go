package workflow

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
)

type thinkingTestResolver struct{}

func (thinkingTestResolver) Validate(context.Context, frozen.CredentialReference) error { return nil }
func (thinkingTestResolver) Resolve(context.Context, credentials.ResolveRequest) (credentials.SecretMaterial, error) {
	return credentials.NewSecretMaterial([]byte("test-key"), nil, nil), nil
}

func TestFrozenSystemDeepSeekMemberDisablesThinkingAndEffortTogether(t *testing.T) {
	for _, systemProvider := range []bool{true, false} {
		t.Run(map[bool]string{true: "system-deepseek", false: "other-provider"}[systemProvider], func(t *testing.T) {
			var captured map[string]json.RawMessage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || json.Unmarshal(body, &captured) != nil {
					t.Errorf("invalid model request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			binding := frozen.FrozenModelBinding{ProviderID: "custom", ModelID: "deepseek-flash", BaseURL: server.URL}
			if systemProvider {
				binding.ProviderID = "system/deepseek"
				binding.CredentialRef.ServiceID = "system-provider:deepseek"
			}
			bundle := frozen.FrozenExecutionBundle{Agent: frozen.FrozenAgentRecord{Model: "deepseek-flash"}, PrimaryModel: binding}
			opts, closer, err := NewRuntimeHostFactory().Build(t.Context(), bundle, thinkingTestResolver{})
			if err != nil {
				t.Fatal(err)
			}
			defer closer.Close()
			_, err = opts.LLM.Chat(t.Context(), contract.ChatRequest{Model: "deepseek-flash", Messages: []contract.Message{{Role: "user", Content: "check"}},
				Tools: []contract.ToolDef{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}, Effort: contract.EffortHigh})
			if err != nil {
				t.Fatal(err)
			}
			if systemProvider {
				if string(captured["thinking"]) != `{"type":"disabled"}` || string(captured["reasoning_effort"]) != `"none"` {
					t.Fatalf("frozen DeepSeek member sent conflicting thinking controls: thinking=%s effort=%s", captured["thinking"], captured["reasoning_effort"])
				}
			} else if _, present := captured["thinking"]; present {
				t.Fatalf("unrelated provider unexpectedly received DeepSeek thinking control")
			}
		})
	}
}
