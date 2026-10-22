package llmrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jinyitao123/loom/contract"
)

// Provider protocols. A provider speaks exactly one wire protocol; the member
// that uses its model is an ordinary Loom model member either way.
const (
	// ProtocolOpenAI is OpenAI-compatible chat completions (the default).
	ProtocolOpenAI = "openai"
	// ProtocolSystemOne is the TypeSafe System One decision protocol:
	// one POST {base_url}/v1/systemone per model call.
	ProtocolSystemOne = "systemone"
)

// ErrUnknownProtocol rejects a provider protocol outside the supported set.
var ErrUnknownProtocol = errors.New("provider protocol must be openai or systemone")

// NormalizeProtocol maps the empty value to the default protocol and rejects
// unknown values. Values are exact; no case folding or trimming.
func NormalizeProtocol(protocol string) (string, error) {
	switch protocol {
	case "", ProtocolOpenAI:
		return ProtocolOpenAI, nil
	case ProtocolSystemOne:
		return ProtocolSystemOne, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownProtocol, protocol)
	}
}

// ErrSystemOneInput is returned when a model call carries no structured
// System One questions. Decision models answer only those.
var ErrSystemOneInput = errors.New("systemone: 该模型只回答结构化问题：输入须为含 questions 的 JSON")

// ErrSystemOneTools is returned for a request that offers tools.
var ErrSystemOneTools = errors.New("systemone: 决策模型不调用工具：请移除该成员的工具")

// ErrSystemOneAnswer marks a provider answer that violates the protocol.
var ErrSystemOneAnswer = errors.New("systemone: invalid answer")

const (
	systemOnePath             = "/v1/systemone"
	systemOneMaxQuestions     = 64
	systemOneMaxChoices       = 255
	systemOneMinScoreLevels   = 2
	systemOneMaxScoreLevels   = 10
	systemOneMaxResponseBytes = 8 << 20
	systemOneMaxErrorBody     = 4 << 10
)

// systemOneClient implements contract.LLM over the System One protocol.
// It never goes through the OpenAI client, so OpenAI-only behavior (protocol
// observation, thinking control, reasoning replay, json_object mode) does not
// apply: observers simply see no frames and schemas are verified locally by
// the member's ordinary output validation.
type systemOneClient struct {
	endpoint     string
	apiKey       string
	defaultModel string
	client       *http.Client
}

func newSystemOneClient(cfg ProviderConfig) *systemOneClient {
	defaultModel := ""
	if len(cfg.Models) > 0 {
		defaultModel = cfg.Models[0]
	}
	attemptTimeoutSeconds := cfg.AttemptTimeoutSeconds
	if attemptTimeoutSeconds <= 0 {
		attemptTimeoutSeconds = DefaultAttemptTimeoutSeconds
	}
	return &systemOneClient{
		endpoint:     strings.TrimSuffix(strings.TrimRight(cfg.BaseURL, "/"), "/v1") + systemOnePath,
		apiKey:       cfg.APIKey,
		defaultModel: defaultModel,
		client:       &http.Client{Timeout: time.Duration(attemptTimeoutSeconds) * time.Second, Transport: cfg.Transport},
	}
}

// systemOneQuestion keeps the caller's wire object and the parsed facts
// needed to validate the answer.
type systemOneQuestion struct {
	wire     map[string]json.RawMessage
	kind     string
	choices  map[string]struct{}
	levels   int
	original string
}

type systemOneRequest struct {
	Model     string                     `json:"model"`
	State     json.RawMessage            `json:"state"`
	Questions map[string]json.RawMessage `json:"questions"`
}

type systemOneResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (c *systemOneClient) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	if len(req.Tools) > 0 {
		return nil, ErrSystemOneTools
	}
	model := req.Model
	if model == "" {
		model = c.defaultModel
	}
	wire, questions, err := buildSystemOneRequest(model, req.Messages)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("systemone: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("systemone: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("systemone: http: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, systemOneMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("systemone: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		detail := respBody
		if len(detail) > systemOneMaxErrorBody {
			detail = detail[:systemOneMaxErrorBody]
		}
		// The "HTTP NNN" form keeps FallbackLLM's retry classification.
		return nil, &systemOneHTTPError{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), detail: strings.TrimSpace(string(detail))}
	}
	if len(respBody) > systemOneMaxResponseBytes {
		return nil, fmt.Errorf("%w: response exceeds %d bytes", ErrSystemOneAnswer, systemOneMaxResponseBytes)
	}
	var decoded systemOneResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrSystemOneAnswer, err)
	}
	if err := validateSystemOneAnswers(questions, decoded.Answers); err != nil {
		return nil, err
	}
	content, err := json.Marshal(decoded.Answers)
	if err != nil {
		return nil, fmt.Errorf("%w: encode answers: %v", ErrSystemOneAnswer, err)
	}
	response := &contract.ChatResponse{Content: string(content), StopReason: "stop"}
	if decoded.Usage != nil {
		response.Usage = contract.Usage{InputTokens: decoded.Usage.InputTokens, OutputTokens: decoded.Usage.OutputTokens}
	}
	return response, nil
}

// Stream wraps one Chat call: a content chunk, then the terminal chunk.
func (c *systemOneClient) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	response, err := c.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	usage := response.Usage
	stream := make(chan contract.StreamChunk, 2)
	stream <- contract.StreamChunk{Content: response.Content}
	stream <- contract.StreamChunk{Done: true, Usage: &usage, FinishReason: response.StopReason}
	close(stream)
	return stream, nil
}

var _ contract.LLM = (*systemOneClient)(nil)

// buildSystemOneRequest maps a chat request to one System One call. The
// payload is the newest user message carrying a JSON object with
// "questions"; the object may be the whole message or embedded in it (a
// workflow node renders its inputs after its instruction text), at any depth.
// System messages are the member's identity and are prefixed to every
// question's instructions.
func buildSystemOneRequest(model string, messages []contract.Message) (systemOneRequest, map[string]systemOneQuestion, error) {
	var payload map[string]json.RawMessage
	for index := len(messages) - 1; index >= 0 && payload == nil; index-- {
		if messages[index].Role == "user" {
			payload = findSystemOnePayload(messages[index].Content)
		}
	}
	if payload == nil {
		return systemOneRequest{}, nil, ErrSystemOneInput
	}
	var rawQuestions map[string]json.RawMessage
	if err := json.Unmarshal(payload["questions"], &rawQuestions); err != nil || rawQuestions == nil {
		return systemOneRequest{}, nil, fmt.Errorf("%w: questions 须为对象", ErrSystemOneInput)
	}
	if len(rawQuestions) == 0 || len(rawQuestions) > systemOneMaxQuestions {
		return systemOneRequest{}, nil, fmt.Errorf("%w: questions 须含 1–%d 道题", ErrSystemOneInput, systemOneMaxQuestions)
	}

	var systemParts []string
	for _, message := range messages {
		if message.Role == "system" && strings.TrimSpace(message.Content) != "" {
			systemParts = append(systemParts, strings.TrimSpace(message.Content))
		}
	}
	prefix := strings.Join(systemParts, "\n\n")

	questions := make(map[string]systemOneQuestion, len(rawQuestions))
	wireQuestions := make(map[string]json.RawMessage, len(rawQuestions))
	for name, raw := range rawQuestions {
		question, err := parseSystemOneQuestion(name, raw)
		if err != nil {
			return systemOneRequest{}, nil, err
		}
		instructions := question.original
		if prefix != "" {
			instructions = strings.TrimSpace(prefix + "\n\n" + instructions)
		}
		encoded, err := json.Marshal(instructions)
		if err != nil {
			return systemOneRequest{}, nil, fmt.Errorf("%w: question %q instructions", ErrSystemOneInput, name)
		}
		question.wire["instructions"] = encoded
		wire, err := json.Marshal(question.wire)
		if err != nil {
			return systemOneRequest{}, nil, fmt.Errorf("%w: question %q", ErrSystemOneInput, name)
		}
		questions[name] = question
		wireQuestions[name] = wire
	}

	state, ok := payload["state"]
	if !ok || isJSONNull(state) {
		rest := make(map[string]json.RawMessage, len(payload))
		for key, value := range payload {
			if key != "questions" && key != "state" {
				rest[key] = value
			}
		}
		encoded, err := json.Marshal(rest)
		if err != nil {
			return systemOneRequest{}, nil, fmt.Errorf("%w: state", ErrSystemOneInput)
		}
		state = encoded
	} else if !isJSONObjectOrString(state) {
		return systemOneRequest{}, nil, fmt.Errorf("%w: state 须为字符串或对象", ErrSystemOneInput)
	}
	return systemOneRequest{Model: model, State: state, Questions: wireQuestions}, questions, nil
}

func parseSystemOneQuestion(name string, raw json.RawMessage) (systemOneQuestion, error) {
	invalid := func(detail string) error {
		return fmt.Errorf("%w: question %q %s", ErrSystemOneInput, name, detail)
	}
	if strings.TrimSpace(name) == "" {
		return systemOneQuestion{}, fmt.Errorf("%w: question name is empty", ErrSystemOneInput)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil || wire == nil {
		return systemOneQuestion{}, invalid("须为对象")
	}
	question := systemOneQuestion{wire: wire}
	if err := json.Unmarshal(wire["type"], &question.kind); err != nil {
		return systemOneQuestion{}, invalid("缺少 type")
	}
	if rawInstructions, ok := wire["instructions"]; ok && !isJSONNull(rawInstructions) {
		if err := json.Unmarshal(rawInstructions, &question.original); err != nil {
			return systemOneQuestion{}, invalid("instructions 须为字符串")
		}
	}
	criteria, hasCriteria := wire["criteria"]
	switch question.kind {
	case "choice":
		var options map[string]string
		if err := json.Unmarshal(criteria, &options); err != nil || len(options) == 0 || len(options) > systemOneMaxChoices {
			return systemOneQuestion{}, invalid(fmt.Sprintf("choice 的 criteria 须为 1–%d 项的对象", systemOneMaxChoices))
		}
		question.choices = make(map[string]struct{}, len(options))
		for key := range options {
			question.choices[key] = struct{}{}
		}
	case "score":
		var levels []string
		if err := json.Unmarshal(criteria, &levels); err != nil || len(levels) < systemOneMinScoreLevels || len(levels) > systemOneMaxScoreLevels {
			return systemOneQuestion{}, invalid(fmt.Sprintf("score 的 criteria 须为 %d–%d 级的数组", systemOneMinScoreLevels, systemOneMaxScoreLevels))
		}
		question.levels = len(levels)
	case "noul":
		if hasCriteria && !isJSONNull(criteria) {
			var labels map[string]string
			if err := json.Unmarshal(criteria, &labels); err != nil {
				return systemOneQuestion{}, invalid("noul 的 criteria 须为对象")
			}
			for key := range labels {
				if key != "true" && key != "false" {
					return systemOneQuestion{}, invalid("noul 的 criteria 只能含 true 与 false")
				}
			}
		}
	default:
		return systemOneQuestion{}, invalid("type 须为 choice、score 或 noul")
	}
	return question, nil
}

// findSystemOnePayload returns the shallowest JSON object holding a
// "questions" key: the whole text first, then each object embedded in it.
func findSystemOnePayload(text string) map[string]json.RawMessage {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	var whole json.RawMessage
	if json.Unmarshal([]byte(trimmed), &whole) == nil {
		return searchSystemOnePayload(whole)
	}
	for offset := 0; offset < len(text); {
		start := strings.IndexByte(text[offset:], '{')
		if start < 0 {
			return nil
		}
		start += offset
		decoder := json.NewDecoder(strings.NewReader(text[start:]))
		var candidate json.RawMessage
		if decoder.Decode(&candidate) != nil {
			offset = start + 1
			continue
		}
		if found := searchSystemOnePayload(candidate); found != nil {
			return found
		}
		offset = start + int(decoder.InputOffset())
	}
	return nil
}

// searchSystemOnePayload walks objects and arrays breadth first.
func searchSystemOnePayload(root json.RawMessage) map[string]json.RawMessage {
	queue := []json.RawMessage{root}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		trimmed := bytes.TrimSpace(current)
		if len(trimmed) == 0 {
			continue
		}
		switch trimmed[0] {
		case '{':
			var object map[string]json.RawMessage
			if json.Unmarshal(trimmed, &object) != nil {
				continue
			}
			if _, ok := object["questions"]; ok {
				return object
			}
			keys := make([]string, 0, len(object))
			for key := range object {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				queue = append(queue, object[key])
			}
		case '[':
			var array []json.RawMessage
			if json.Unmarshal(trimmed, &array) == nil {
				queue = append(queue, array...)
			}
		}
	}
	return nil
}

func validateSystemOneAnswers(questions map[string]systemOneQuestion, answers map[string]json.RawMessage) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrSystemOneAnswer}, args...)...)
	}
	if answers == nil {
		return invalid("answers missing")
	}
	for name := range answers {
		if _, ok := questions[name]; !ok {
			return invalid("unexpected answer %q", name)
		}
	}
	for name, question := range questions {
		raw, ok := answers[name]
		if !ok {
			return invalid("missing answer %q", name)
		}
		var answer struct {
			Type          string             `json:"type"`
			Choice        *string            `json:"choice"`
			Score         *float64           `json:"score"`
			Noul          *float64           `json:"noul"`
			Confidence    *float64           `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
		}
		if err := json.Unmarshal(raw, &answer); err != nil {
			return invalid("answer %q: %v", name, err)
		}
		if answer.Type != "" && answer.Type != question.kind {
			return invalid("answer %q type %q, want %q", name, answer.Type, question.kind)
		}
		if answer.Confidence != nil && !unitInterval(*answer.Confidence) {
			return invalid("answer %q confidence outside [0,1]", name)
		}
		// Probabilities are bounded per outcome and keyed by a real outcome. Their
		// sum is not enforced: providers may omit or round low-mass outcomes.
		for key, probability := range answer.Probabilities {
			if !unitInterval(probability) {
				return invalid("answer %q probability %q outside [0,1]", name, key)
			}
			if !question.hasOutcome(key) {
				return invalid("answer %q probability for unknown outcome %q", name, key)
			}
		}
		switch question.kind {
		case "choice":
			if answer.Choice == nil {
				return invalid("answer %q has no choice", name)
			}
			if _, ok := question.choices[*answer.Choice]; !ok {
				return invalid("answer %q choice %q is not a criteria key", name, *answer.Choice)
			}
		case "score":
			if answer.Score == nil || !finite(*answer.Score) || *answer.Score < 0 || *answer.Score > float64(question.levels-1) {
				return invalid("answer %q score outside [0,%d]", name, question.levels-1)
			}
		case "noul":
			if answer.Noul == nil || !unitInterval(*answer.Noul) {
				return invalid("answer %q noul outside [0,1]", name)
			}
		}
	}
	return nil
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func unitInterval(value float64) bool { return finite(value) && value >= 0 && value <= 1 }

func isJSONNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func isJSONObjectOrString(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '"')
}

// hasOutcome reports whether key names an outcome of the question: a criteria
// key for choice, a level index "0".."n-1" for score, "true"/"false" for noul.
func (q systemOneQuestion) hasOutcome(key string) bool {
	switch q.kind {
	case "choice":
		_, ok := q.choices[key]
		return ok
	case "score":
		level, err := strconv.Atoi(key)
		return err == nil && strconv.Itoa(level) == key && level >= 0 && level < q.levels
	case "noul":
		return key == "true" || key == "false"
	}
	return false
}
