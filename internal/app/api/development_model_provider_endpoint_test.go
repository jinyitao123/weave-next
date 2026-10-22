package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func resolveEndpoint(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/development/model-providers/endpoint", strings.NewReader(body))
	if err := (&Server{}).handleResolveDevelopmentModelEndpoint(echo.New().NewContext(request, recorder)); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return recorder.Code, decoded
}

// A configuration page sees the exact address before saving, and a refused
// one comes back with the split to confirm rather than a silent rewrite.
func TestResolveModelEndpointShowsTheAddressOrTheSplitToConfirm(t *testing.T) {
	code, body := resolveEndpoint(t, `{"protocol":"openai-completions","base_url":"https://gw.example.test/team-a/","chat_path":""}`)
	if code != http.StatusOK || body["chat_url"] != "https://gw.example.test/team-a/v1/chat/completions" || body["chat_path"] != "/v1/chat/completions" || body["base_url"] != "https://gw.example.test/team-a" {
		t.Fatalf("accepted = %d %v", code, body)
	}
	code, body = resolveEndpoint(t, `{"protocol":"openai-completions","base_url":"https://api.example.test/v1","chat_path":""}`)
	suggestion, _ := body["suggestion"].(map[string]any)
	if code != http.StatusUnprocessableEntity || body["reason"] != "duplicate_path_segment" || body["error"] == "" ||
		suggestion["chat_url"] != "https://api.example.test/v1/chat/completions" || suggestion["base_url"] != "https://api.example.test" {
		t.Fatalf("refused = %d %v", code, body)
	}
	code, body = resolveEndpoint(t, `{"protocol":"openai-completions","base_url":"https://api.example.test/v1/chat/completions","chat_path":""}`)
	if code != http.StatusUnprocessableEntity || body["reason"] != "endpoint_in_base_url" || body["suggestion"] == nil {
		t.Fatalf("full endpoint = %d %v", code, body)
	}
	code, body = resolveEndpoint(t, `{"protocol":"","base_url":"https://api.example.test","chat_path":""}`)
	if code != http.StatusUnprocessableEntity || body["suggestion"] != nil {
		t.Fatalf("no protocol = %d %v", code, body)
	}
}
