package llmrouter

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

func TestProtocolObservationPreservesProviderRequestResponseAndPrivacy(t *testing.T) {
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"finish_reason":"tool_calls","message":{"content":"private-response-marker","reasoning_content":"private-reasoning-marker","tool_calls":[{"id":"private-call-marker","type":"function","function":{"name":"private-tool-marker","arguments":"{ \"value\" : \"private-argument-marker\" }"}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	}))
	defer server.Close()
	client := NewProviderClient(ProviderConfig{BaseURL: server.URL, APIKey: "fixture-private-key-marker", Models: []string{"fixture"}})
	request := contract.ChatRequest{Model: "fixture", Messages: []contract.Message{{Role: "user", Content: "private-request-marker"}}, Tools: []contract.ToolDef{{Name: "private-tool-marker", Description: "private-description-marker", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	plain, plainErr := client.Chat(t.Context(), request)
	var observations []ModelProtocolObservation
	var normalized NormalizedModelProtocol
	observed, observedErr := ObserveModelProtocolCall(t.Context(), client, request, func(value ModelProtocolObservation) { observations = append(observations, value) }, func(value NormalizedModelProtocol) { normalized = value })
	if plainErr != nil || observedErr != nil || !reflect.DeepEqual(plain, observed) || len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatal("probe changed provider request or response")
	}
	if len(observations) != 1 || !observations[0].Complete || !observations[0].RequestSent || observations[0].EmittedCalls != 1 || normalized.Count == nil || *normalized.Count != 1 {
		t.Fatal("complete provider/normalized boundary was not observed")
	}
	raw, _ := json.Marshal(struct {
		Provider   []ModelProtocolObservation
		Normalized NormalizedModelProtocol
	}{observations, normalized})
	for _, marker := range []string{"private-request-marker", "private-response-marker", "private-reasoning-marker", "fixture-private-key-marker", "private-argument-marker", "private-description-marker", "private-call-marker", "private-tool-marker"} {
		if bytes.Contains(raw, []byte(marker)) {
			t.Fatal("private value entered diagnostic metadata")
		}
	}
	if normalized.Tools[0].CanonicalArgumentsSHA256 != protocolDigest([]byte(`{"value":"private-argument-marker"}`)) {
		t.Fatal("canonical tool journal linkage was not preserved")
	}
}

func TestProtocolObservationRejectsFreeTextAndBoundsMetadata(t *testing.T) {
	value := contract.ProtocolObservation{Version: 1, Protocol: "private-protocol-marker", End: "private-error-marker", FinishReason: "private-finish-marker", RequestToolsSHA256: "private-hash-marker", Complete: true}
	for index := 0; index < 20; index++ {
		value.ToolIndexes = append(value.ToolIndexes, index)
		value.Arguments = append(value.Arguments, contract.ToolArgumentObservation{Index: index, Bytes: 10, SHA256: strings.Repeat("a", 64)})
	}
	bounded := boundedProtocolObservation(value)
	raw, _ := json.Marshal(bounded)
	if bounded.Complete || !bounded.Truncated || len(bounded.ToolIndexes) > 16 || len(bounded.Arguments) > 16 || bytes.Contains(raw, []byte("private-")) {
		t.Fatal("invalid protocol strings or unbounded metadata were accepted")
	}
}
