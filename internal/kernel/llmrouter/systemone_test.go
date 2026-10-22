package llmrouter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

type systemOneStub struct {
	mu       sync.Mutex
	path     string
	auth     []string
	hasAuth  bool
	body     map[string]json.RawMessage
	status   int
	response string
}

func (s *systemOneStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.path = r.URL.Path
		s.auth, s.hasAuth = r.Header["Authorization"]
		raw, _ := io.ReadAll(r.Body)
		s.body = nil
		_ = json.Unmarshal(raw, &s.body)
		status := s.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(s.response))
	}))
	t.Cleanup(server.Close)
	return server
}

const systemOneQuestions = `{"act":{"type":"choice","instructions":"Pick an action","criteria":{"attack":"Attack","flee":"Flee"}},` +
	`"risk":{"type":"score","instructions":"How risky","criteria":["low","medium","high"]},` +
	`"win":{"type":"noul","instructions":"Will we win"}}`

const systemOneAnswers = `{"act":{"type":"choice","choice":"attack","probabilities":{"attack":0.7,"flee":0.3},"confidence":0.4},` +
	`"risk":{"type":"score","score":1.3,"legend":{"0":"low","1":"medium","2":"high"},"probabilities":{"0":0.2,"1":0.3,"2":0.5},"confidence":0},` +
	`"win":{"type":"noul","noul":0.25}}`

func systemOneOK() string {
	return `{"model":"nimble:latest","answers":` + systemOneAnswers + `,"usage":{"input_tokens":879,"output_tokens":3}}`
}

func TestSystemOneMapsRequestAndResponse(t *testing.T) {
	stub := &systemOneStub{response: systemOneOK()}
	server := stub.server(t)
	llm := NewProviderClient(ProviderConfig{Protocol: ProtocolSystemOne, BaseURL: server.URL + "/", Models: []string{"nimble:latest"}})
	request := contract.ChatRequest{Messages: []contract.Message{
		{Role: "system", Content: "You are the river clan's war chief."},
		{Role: "user", Content: `{"state":{"hp":30},"questions":` + systemOneQuestions + `}`},
	}}
	response, err := llm.Chat(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if stub.path != "/v1/systemone" {
		t.Fatalf("path = %q", stub.path)
	}
	if stub.hasAuth {
		t.Fatalf("empty key sent Authorization %q", stub.auth)
	}
	var model string
	_ = json.Unmarshal(stub.body["model"], &model)
	if model != "nimble:latest" {
		t.Fatalf("default model not used: %q", model)
	}
	if string(stub.body["state"]) != `{"hp":30}` {
		t.Fatalf("state = %s", stub.body["state"])
	}
	var questions map[string]map[string]json.RawMessage
	if err := json.Unmarshal(stub.body["questions"], &questions); err != nil || len(questions) != 3 {
		t.Fatalf("questions = %s", stub.body["questions"])
	}
	var instructions string
	_ = json.Unmarshal(questions["act"]["instructions"], &instructions)
	if instructions != "You are the river clan's war chief.\n\nPick an action" {
		t.Fatalf("system prompt not prefixed: %q", instructions)
	}
	if string(questions["act"]["criteria"]) != `{"attack":"Attack","flee":"Flee"}` {
		t.Fatalf("criteria rewritten: %s", questions["act"]["criteria"])
	}
	var answers map[string]map[string]any
	if err := json.Unmarshal([]byte(response.Content), &answers); err != nil || answers["act"]["choice"] != "attack" || answers["win"]["noul"] != 0.25 {
		t.Fatalf("content is not the answers object: %s", response.Content)
	}
	if response.Usage.InputTokens != 879 || response.Usage.OutputTokens != 3 || response.StopReason != "stop" || len(response.ToolCalls) != 0 {
		t.Fatalf("response = %+v", response)
	}

	stream, err := llm.Stream(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	var content strings.Builder
	var final contract.StreamChunk
	for chunk := range stream {
		content.WriteString(chunk.Content)
		if chunk.Done {
			final = chunk
		}
	}
	if content.String() != response.Content || final.Usage == nil || final.Usage.InputTokens != 879 || final.FinishReason != "stop" {
		t.Fatalf("stream did not wrap chat: %q %+v", content.String(), final)
	}
}

func TestSystemOneFindsPayloadInsideWorkflowPrompt(t *testing.T) {
	stub := &systemOneStub{response: systemOneOK()}
	server := stub.server(t)
	llm := NewProviderClient(ProviderConfig{Protocol: ProtocolSystemOne, BaseURL: server.URL, APIKey: "local-test-key", Models: []string{"nimble:latest"}})
	// A workflow node renders its instruction, then its inputs as JSON; a
	// correction may follow as a later user message.
	prompt := "Decide the next move.\n\nInputs:\n" + `{"request":{"turn":7,"questions":` + systemOneQuestions + `}}`
	_, err := llm.Chat(t.Context(), contract.ChatRequest{Model: "nimble:other", Messages: []contract.Message{
		{Role: "user", Content: prompt},
		{Role: "assistant", Content: "{}"},
		{Role: "user", Content: "Your previous output was invalid; answer again."},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.auth) != 1 || stub.auth[0] != "Bearer local-test-key" {
		t.Fatalf("Authorization = %q", stub.auth)
	}
	if string(stub.body["state"]) != `{"turn":7}` {
		t.Fatalf("state without explicit field must be the payload minus questions: %s", stub.body["state"])
	}
	var model string
	_ = json.Unmarshal(stub.body["model"], &model)
	if model != "nimble:other" {
		t.Fatalf("request model not used: %q", model)
	}
	var questions map[string]map[string]json.RawMessage
	_ = json.Unmarshal(stub.body["questions"], &questions)
	if string(questions["win"]["instructions"]) != `"Will we win"` {
		t.Fatalf("instructions changed without a system prompt: %s", questions["win"]["instructions"])
	}
}

func TestSystemOneRejectsUnstructuredInputAndTools(t *testing.T) {
	stub := &systemOneStub{response: systemOneOK()}
	server := stub.server(t)
	llm := NewProviderClient(ProviderConfig{Protocol: ProtocolSystemOne, BaseURL: server.URL, Models: []string{"nimble:latest"}})
	for name, content := range map[string]string{
		"prose":          "Should we attack?",
		"no questions":   `{"state":{"hp":1}}`,
		"questions list": `{"questions":[1,2]}`,
		"empty":          `{"questions":{}}`,
		"bad type":       `{"questions":{"a":{"type":"rank","criteria":{}}}}`,
		"one level":      `{"questions":{"a":{"type":"score","criteria":["only"]}}}`,
		"bad state":      `{"state":3,"questions":` + systemOneQuestions + `}`,
	} {
		_, err := llm.Chat(t.Context(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: content}}})
		if !errors.Is(err, ErrSystemOneInput) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if !strings.Contains(ErrSystemOneInput.Error(), "输入须为含 questions 的 JSON") {
		t.Fatal("input error must explain the expected input")
	}
	_, err := llm.Chat(t.Context(), contract.ChatRequest{
		Messages: []contract.Message{{Role: "user", Content: `{"questions":` + systemOneQuestions + `}`}},
		Tools:    []contract.ToolDef{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if !errors.Is(err, ErrSystemOneTools) {
		t.Fatalf("tools accepted: %v", err)
	}
	if stub.body != nil {
		t.Fatal("an invalid request reached the provider")
	}
}

func TestSystemOneValidatesAnswers(t *testing.T) {
	answer := func(act, risk, win string) string {
		return `{"answers":{"act":` + act + `,"risk":` + risk + `,"win":` + win + `}}`
	}
	goodAct, goodRisk, goodWin := `{"type":"choice","choice":"flee"}`, `{"type":"score","score":2}`, `{"type":"noul","noul":1}`
	cases := map[string]string{
		"missing answer":    `{"answers":{"act":` + goodAct + `,"risk":` + goodRisk + `}}`,
		"extra answer":      `{"answers":{"act":` + goodAct + `,"risk":` + goodRisk + `,"win":` + goodWin + `,"other":{"type":"noul","noul":0}}}`,
		"no answers":        `{"model":"x"}`,
		"unknown choice":    answer(`{"type":"choice","choice":"hide"}`, goodRisk, goodWin),
		"missing choice":    answer(`{"type":"choice"}`, goodAct, goodWin),
		"score high":        answer(goodAct, `{"type":"score","score":2.01}`, goodWin),
		"score negative":    answer(goodAct, `{"type":"score","score":-0.1}`, goodWin),
		"noul high":         answer(goodAct, goodRisk, `{"type":"noul","noul":1.5}`),
		"noul missing":      answer(goodAct, goodRisk, `{"type":"noul"}`),
		"confidence high":   answer(`{"type":"choice","choice":"flee","confidence":1.2}`, goodRisk, goodWin),
		"wrong type":        answer(goodAct, goodRisk, `{"type":"score","score":0}`),
		"infinite":          answer(goodAct, `{"type":"score","score":1e999}`, goodWin),
		"NaN token":         answer(goodAct, `{"type":"score","score":NaN}`, goodWin),
		"not json":          `<html>`,
		"string score":      answer(goodAct, `{"type":"score","score":"1"}`, goodWin),
		"probability large": answer(`{"type":"choice","choice":"flee","probabilities":{"flee":1e999}}`, goodRisk, goodWin),
	}
	stub := &systemOneStub{}
	server := stub.server(t)
	llm := NewProviderClient(ProviderConfig{Protocol: ProtocolSystemOne, BaseURL: server.URL, Models: []string{"nimble:latest"}})
	request := contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: `{"questions":` + systemOneQuestions + `}`}}}
	for name, response := range cases {
		stub.response = response
		if _, err := llm.Chat(t.Context(), request); !errors.Is(err, ErrSystemOneAnswer) {
			t.Errorf("%s accepted or misclassified: %v", name, err)
		}
	}
	stub.response = answer(goodAct, goodRisk, goodWin)
	response, err := llm.Chat(t.Context(), request)
	if err != nil {
		t.Fatalf("boundary answers rejected: %v", err)
	}
	if response.Usage != (contract.Usage{}) {
		t.Fatal("absent usage invented counts")
	}
}

func TestSystemOneReportsHTTPErrors(t *testing.T) {
	stub := &systemOneStub{status: http.StatusUnprocessableEntity, response: `{"detail":[{"msg":"field required"}]}`}
	server := stub.server(t)
	llm := NewProviderClient(ProviderConfig{Protocol: ProtocolSystemOne, BaseURL: server.URL, Models: []string{"nimble:latest"}})
	request := contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: `{"questions":` + systemOneQuestions + `}`}}}
	_, err := llm.Chat(t.Context(), request)
	if err == nil || !strings.Contains(err.Error(), "HTTP 422") || !strings.Contains(err.Error(), "field required") || isRetryable(err) {
		t.Fatalf("422 = %v", err)
	}
	stub.status = http.StatusServiceUnavailable
	if _, err := llm.Chat(t.Context(), request); err == nil || !isRetryable(err) {
		t.Fatalf("503 must stay retryable: %v", err)
	}
	if _, err := llm.Stream(t.Context(), request); err == nil {
		t.Fatal("stream hid the provider error")
	}
}

func TestSystemOneHonorsAttemptTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	client := newSystemOneClient(ProviderConfig{Protocol: ProtocolSystemOne, BaseURL: server.URL, AttemptTimeoutSeconds: 1})
	if client.client.Timeout.Seconds() != 1 {
		t.Fatalf("timeout = %s", client.client.Timeout)
	}
	if newSystemOneClient(ProviderConfig{}).client.Timeout.Seconds() != DefaultAttemptTimeoutSeconds {
		t.Fatal("default attempt timeout not applied")
	}
	_, err := client.Chat(t.Context(), contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: `{"questions":` + systemOneQuestions + `}`}}})
	if err == nil || !isRetryable(err) {
		t.Fatalf("timeout not reported as a transient error: %v", err)
	}
}

func TestSystemOneProtocolObservationIsUnavailableNotFatal(t *testing.T) {
	stub := &systemOneStub{response: systemOneOK()}
	server := stub.server(t)
	llm := NewProviderClient(ProviderConfig{Protocol: ProtocolSystemOne, BaseURL: server.URL, Models: []string{"nimble:latest"}})
	observed := 0
	var normalized NormalizedModelProtocol
	response, err := ObserveModelProtocolCall(t.Context(), llm, contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: `{"questions":` + systemOneQuestions + `}`}}},
		func(ModelProtocolObservation) { observed++ }, func(value NormalizedModelProtocol) { normalized = value })
	if err != nil || response == nil || observed != 0 || normalized.Error {
		t.Fatalf("observation: observed=%d normalized=%+v err=%v", observed, normalized, err)
	}
}

func TestNormalizeProtocol(t *testing.T) {
	for input, want := range map[string]string{"": ProtocolOpenAI, "openai": ProtocolOpenAI, "systemone": ProtocolSystemOne} {
		if got, err := NormalizeProtocol(input); err != nil || got != want {
			t.Fatalf("%q -> %q %v", input, got, err)
		}
	}
	for _, input := range []string{"OpenAI", " systemone", "anthropic"} {
		if _, err := NormalizeProtocol(input); !errors.Is(err, ErrUnknownProtocol) {
			t.Fatalf("%q accepted", input)
		}
	}
}

func TestClaimProviderAcceptsEmptyKeyAndKeepsProtocol(t *testing.T) {
	source := &claimProviderSource{configs: []ProviderConfig{
		{ID: "nimble", BaseURL: "http://models.invalid", Protocol: ProtocolSystemOne, Models: []string{"nimble:latest"}, CredentialScope: frozen.CredentialScopeUser, CredentialUserID: "alice"},
		{ID: "nowhere", Models: []string{"no-address"}, CredentialScope: frozen.CredentialScopeUser, CredentialUserID: "alice"},
	}}
	resolver := NewResolver(New("nimble:latest"))
	resolver.SetSource(source)
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "alice"})
	cfg, err := resolver.ResolveProviderConfiguration(ctx, "workspace", "nimble:latest")
	if err != nil || cfg.APIKey != "" || cfg.Protocol != ProtocolSystemOne || cfg.BaseURL != "http://models.invalid" {
		t.Fatalf("keyless provider not claimable: %+v %v", cfg, err)
	}
	if _, err := resolver.ResolveProviderConfiguration(ctx, "workspace", "no-address"); err == nil {
		t.Fatal("provider without an address was claimable")
	}
}

func TestListProvidersShowsUnsetKeyAsEmpty(t *testing.T) {
	router := New("model")
	router.RegisterProvider(ProviderConfig{ID: "keyless", BaseURL: "http://models.invalid", Models: []string{"model"}})
	router.RegisterProvider(ProviderConfig{ID: "keyed", BaseURL: "http://models.invalid", APIKey: "sk-test-1234567890", Models: []string{"other"}})
	for _, cfg := range router.ListProviders() {
		if (cfg.ID == "keyless") != (cfg.APIKey == "") {
			t.Fatalf("%s rendered key as %q", cfg.ID, cfg.APIKey)
		}
	}
}

func TestSystemOneValidatesProbabilities(t *testing.T) {
	choice, err := parseSystemOneQuestion("action", json.RawMessage(`{"type":"choice","criteria":{"hold":"守","advance":"进"}}`))
	if err != nil {
		t.Fatal(err)
	}
	score, err := parseSystemOneQuestion("morale", json.RawMessage(`{"type":"score","criteria":["溃","动","可","坚"]}`))
	if err != nil {
		t.Fatal(err)
	}
	questions := map[string]systemOneQuestion{"action": choice, "morale": score}
	valid := func(action, morale string) map[string]json.RawMessage {
		return map[string]json.RawMessage{"action": json.RawMessage(action), "morale": json.RawMessage(morale)}
	}
	// Shape returned by nimble on 2026-10-21; probabilities need not sum to 1.
	if err := validateSystemOneAnswers(questions, valid(
		`{"type":"choice","choice":"hold","probabilities":{"hold":0.79,"advance":0.12},"confidence":0.72}`,
		`{"type":"score","score":1.02,"probabilities":{"0":0.02,"1":0.94,"2":0.03,"3":0.004}}`)); err != nil {
		t.Fatalf("valid answers rejected: %v", err)
	}
	for name, answers := range map[string]map[string]json.RawMessage{
		"probability above one":  valid(`{"choice":"hold","probabilities":{"hold":1.5}}`, `{"score":1}`),
		"negative probability":   valid(`{"choice":"hold","probabilities":{"hold":-0.1}}`, `{"score":1}`),
		"unknown choice outcome": valid(`{"choice":"hold","probabilities":{"flee":0.2}}`, `{"score":1}`),
		"score level past end":   valid(`{"choice":"hold"}`, `{"score":1,"probabilities":{"4":0.1}}`),
		"non-canonical level":    valid(`{"choice":"hold"}`, `{"score":1,"probabilities":{"01":0.1}}`),
	} {
		if err := validateSystemOneAnswers(questions, answers); !errors.Is(err, ErrSystemOneAnswer) {
			t.Errorf("%s: want ErrSystemOneAnswer, got %v", name, err)
		}
	}
}
