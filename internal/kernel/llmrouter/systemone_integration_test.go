package llmrouter

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

// TestSystemOneRealProvider calls a real System One server, for example a
// local Ollama serving nimble:
//
//	WEAVE_SYSTEMONE_TEST_URL=http://192.168.230.1:11434 go test ./internal/kernel/llmrouter -run RealProvider
//
// WEAVE_SYSTEMONE_TEST_MODEL overrides the model (default nimble:latest).
func TestSystemOneRealProvider(t *testing.T) {
	baseURL := os.Getenv("WEAVE_SYSTEMONE_TEST_URL")
	if baseURL == "" {
		t.Skip("WEAVE_SYSTEMONE_TEST_URL is not set")
	}
	model := os.Getenv("WEAVE_SYSTEMONE_TEST_MODEL")
	if model == "" {
		model = "nimble:latest"
	}
	router := New(model)
	router.RegisterProvider(ProviderConfig{ID: "systemone", Protocol: ProtocolSystemOne, BaseURL: baseURL, Models: []string{model}, AttemptTimeoutSeconds: 60})
	response, err := router.Chat(t.Context(), contract.ChatRequest{Messages: []contract.Message{
		{Role: "system", Content: "You command a small river clan that values survival over glory."},
		{Role: "user", Content: "Decide the next move.\n\nInputs:\n" + `{"request":{"state":{"our_warriors":4,"enemy_warriors":11,"season":"winter"},"questions":` + systemOneQuestions + `}}`},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var answers map[string]map[string]any
	if err := json.Unmarshal([]byte(response.Content), &answers); err != nil {
		t.Fatalf("content is not an answers object: %v", err)
	}
	if len(answers) != 3 || answers["act"]["type"] != "choice" || answers["risk"]["type"] != "score" || answers["win"]["type"] != "noul" {
		t.Fatalf("answers = %s", response.Content)
	}
	if response.Usage.InputTokens <= 0 || response.StopReason != "stop" {
		t.Fatalf("response = %+v", response)
	}
	t.Logf("answers: %s (input tokens %d)", response.Content, response.Usage.InputTokens)
}
