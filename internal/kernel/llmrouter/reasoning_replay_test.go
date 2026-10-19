package llmrouter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

func TestExplicitThinkingProfileReplaysHistoryWithoutChangingItsMode(t *testing.T) {
	for _, mode := range []string{"", "disabled", "enabled"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []map[string]any  `json:"messages"`
					Thinking map[string]string `json:"thinking"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				_, replayed := body.Messages[0]["reasoning_content"]
				if replayed != (mode != "") || body.Thinking["type"] != mode {
					t.Errorf("profile changed: mode=%q body=%+v", mode, body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer server.Close()
			model := NewProviderClient(ProviderConfig{Protocol: "openai-completions", BaseURL: server.URL, APIKey: "synthetic", Models: []string{"synthetic"}, ThinkingDefaultMode: mode})
			_, err := model.Chat(t.Context(), contract.ChatRequest{Messages: []contract.Message{{Role: "assistant", Content: "ok", Reasoning: []contract.Reasoning{{Text: "synthetic continuity"}}}}})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
