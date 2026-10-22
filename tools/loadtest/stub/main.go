// Command stub is a local model stand-in for Weave load tests.
//
// It serves an OpenAI-compatible POST /v1/chat/completions (streaming and
// non-streaming) and the System One decision protocol POST /v1/systemone.
// Replies are synthesized: when the request carries a json_schema
// response_format, the reply is a minimal instance of that schema, so member
// output contracts are satisfied without a real model.
//
// Latency, first-byte delay and faults (429 with Retry-After, 503, hang until
// the client cancels, truncated streams, missing usage) are configured per
// model with flags or at run time through POST /config. Every call is
// recorded with its wall-clock window, the probe token found in the request
// body and the member marker, so a driver can subtract model time from an
// end-to-end latency. GET /stats reports in-flight counters.
//
// The stub only uses the standard library and is never part of a production
// binary.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Profile is the behavior for one model (or the default).
type Profile struct {
	// Latency is the total model time: fixed:2s | uniform:1s:3s |
	// lognormal:<median>:<sigma> | longtail:<base>:<p>:<tail> |
	// bimodal:<fast>:<slow>:<p_slow>.
	Latency string `json:"latency"`
	// TTFB is the delay before the first streamed byte; it is part of
	// Latency, not added to it. Ignored for non-streaming replies.
	TTFB string `json:"ttfb,omitempty"`
	// P429 is the probability of answering 429 with Retry-After.
	P429 float64 `json:"p429,omitempty"`
	// RetryAfterSeconds is sent with 429 replies.
	RetryAfterSeconds int `json:"retry_after_s,omitempty"`
	// P503 is the probability of answering 503.
	P503 float64 `json:"p503,omitempty"`
	// PHang is the probability of holding the request until the client
	// cancels it (or HangMax elapses, then 504).
	PHang float64 `json:"p_hang,omitempty"`
	// HangMax bounds a hang; empty means 10m.
	HangMax string `json:"hang_max,omitempty"`
	// PTruncate is the probability of ending a stream without a finish
	// reason or [DONE].
	PTruncate float64 `json:"p_truncate,omitempty"`
	// NoUsage omits token usage from replies.
	NoUsage bool `json:"no_usage,omitempty"`

	latency  sampler
	ttfb     time.Duration
	hangMax  time.Duration
	compiled bool
}

// Config is the whole stub configuration.
type Config struct {
	Default Profile            `json:"default"`
	Models  map[string]Profile `json:"models,omitempty"`
}

type sampler func(r *rand.Rand) time.Duration

func parseSampler(spec string) (sampler, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = "fixed:0s"
	}
	parts := strings.Split(spec, ":")
	durs := func(idx ...int) ([]time.Duration, error) {
		out := make([]time.Duration, 0, len(idx))
		for _, i := range idx {
			if i >= len(parts) {
				return nil, fmt.Errorf("latency %q: missing field %d", spec, i)
			}
			d, err := time.ParseDuration(parts[i])
			if err != nil {
				return nil, fmt.Errorf("latency %q: %w", spec, err)
			}
			out = append(out, d)
		}
		return out, nil
	}
	num := func(i int) (float64, error) {
		if i >= len(parts) {
			return 0, fmt.Errorf("latency %q: missing field %d", spec, i)
		}
		return strconv.ParseFloat(parts[i], 64)
	}
	switch parts[0] {
	case "fixed":
		d, err := durs(1)
		if err != nil {
			return nil, err
		}
		return func(*rand.Rand) time.Duration { return d[0] }, nil
	case "uniform":
		d, err := durs(1, 2)
		if err != nil {
			return nil, err
		}
		return func(r *rand.Rand) time.Duration {
			return d[0] + time.Duration(r.Float64()*float64(d[1]-d[0]))
		}, nil
	case "lognormal":
		d, err := durs(1)
		if err != nil {
			return nil, err
		}
		sigma, err := num(2)
		if err != nil {
			return nil, err
		}
		return func(r *rand.Rand) time.Duration {
			return time.Duration(float64(d[0]) * math.Exp(sigma*r.NormFloat64()))
		}, nil
	case "longtail":
		d, err := durs(1, 3)
		if err != nil {
			return nil, err
		}
		p, err := num(2)
		if err != nil {
			return nil, err
		}
		return func(r *rand.Rand) time.Duration {
			if r.Float64() < p {
				return d[1]
			}
			return d[0]
		}, nil
	case "bimodal":
		d, err := durs(1, 2)
		if err != nil {
			return nil, err
		}
		p, err := num(3)
		if err != nil {
			return nil, err
		}
		return func(r *rand.Rand) time.Duration {
			if r.Float64() < p {
				return d[1]
			}
			return d[0]
		}, nil
	}
	return nil, fmt.Errorf("latency %q: unknown distribution", spec)
}

func (p *Profile) compile() error {
	s, err := parseSampler(p.Latency)
	if err != nil {
		return err
	}
	p.latency = s
	p.ttfb = 0
	if p.TTFB != "" {
		if p.ttfb, err = time.ParseDuration(p.TTFB); err != nil {
			return fmt.Errorf("ttfb: %w", err)
		}
	}
	p.hangMax = 10 * time.Minute
	if p.HangMax != "" {
		if p.hangMax, err = time.ParseDuration(p.HangMax); err != nil {
			return fmt.Errorf("hang_max: %w", err)
		}
	}
	if p.RetryAfterSeconds <= 0 {
		p.RetryAfterSeconds = 1
	}
	p.compiled = true
	return nil
}

func (c *Config) compile() error {
	if err := c.Default.compile(); err != nil {
		return fmt.Errorf("default: %w", err)
	}
	for name, p := range c.Models {
		if err := p.compile(); err != nil {
			return fmt.Errorf("model %s: %w", name, err)
		}
		c.Models[name] = p
	}
	return nil
}

// Call is one recorded model request.
type Call struct {
	Seq      int64  `json:"seq"`
	Protocol string `json:"protocol"`
	Model    string `json:"model"`
	Probe    string `json:"probe,omitempty"`
	Member   string `json:"member,omitempty"`
	Stream   bool   `json:"stream"`
	Outcome  string `json:"outcome"`
	StartNS  int64  `json:"start_ns"`
	EndNS    int64  `json:"end_ns"`
	PlanMS   int64  `json:"plan_ms"`
}

type server struct {
	mu     sync.RWMutex
	cfg    Config
	rawCfg json.RawMessage

	inflight    atomic.Int64
	maxInflight atomic.Int64
	seq         atomic.Int64

	callsMu sync.Mutex
	calls   []Call
	outcome map[string]int64
	byModel map[string]int64
	keep    int
	resetAt time.Time
}

var (
	probePattern  = regexp.MustCompile(`ltp-[0-9a-z]{6,40}`)
	memberPattern = regexp.MustCompile(`@@member=([A-Za-z0-9_\-]+)@@`)
)

func (s *server) profile(model string) Profile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.cfg.Models[model]; ok {
		return p
	}
	return s.cfg.Default
}

func (s *server) enter() func() {
	n := s.inflight.Add(1)
	for {
		m := s.maxInflight.Load()
		if n <= m || s.maxInflight.CompareAndSwap(m, n) {
			break
		}
	}
	return func() { s.inflight.Add(-1) }
}

func (s *server) record(c Call) {
	s.callsMu.Lock()
	defer s.callsMu.Unlock()
	s.outcome[c.Outcome]++
	s.byModel[c.Model]++
	if len(s.calls) < s.keep {
		s.calls = append(s.calls, c)
	}
}

// ---- OpenAI-compatible chat completions ----

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       json.RawMessage `json:"messages"`
	Stream         bool            `json:"stream"`
	StreamOptions  *struct{ IncludeUsage bool `json:"include_usage"` } `json:"stream_options"`
	ResponseFormat *struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	} `json:"response_format"`
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	done := s.enter()
	defer done()
	start := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "invalid json: " + err.Error()}})
		return
	}
	call := Call{Seq: s.seq.Add(1), Protocol: "openai", Model: req.Model, Stream: req.Stream,
		Probe: probePattern.FindString(string(body)), StartNS: start.UnixNano()}
	if m := memberPattern.FindStringSubmatch(string(body)); m != nil {
		call.Member = m[1]
	}
	prof := s.profile(req.Model)
	rng := rand.New(rand.NewPCG(uint64(start.UnixNano()), uint64(call.Seq)))
	plan := prof.latency(rng)
	if plan < 0 {
		plan = 0
	}
	call.PlanMS = plan.Milliseconds()
	defer func() {
		call.EndNS = time.Now().UnixNano()
		s.record(call)
	}()

	if outcome, ok := s.fault(w, r, prof, rng, plan); ok {
		call.Outcome = outcome
		return
	}

	content := "{}"
	if req.ResponseFormat != nil && req.ResponseFormat.JSONSchema != nil && len(req.ResponseFormat.JSONSchema.Schema) > 0 {
		var schema any
		if err := json.Unmarshal(req.ResponseFormat.JSONSchema.Schema, &schema); err == nil {
			raw, _ := json.Marshal(synth(schema, call.Probe, 0))
			content = string(raw)
		}
	} else if req.ResponseFormat == nil {
		content = "stub reply " + call.Probe
	}
	promptTokens := len(body) / 4
	completionTokens := len(content)/4 + 1
	id := fmt.Sprintf("chatcmpl-stub-%d", call.Seq)

	if !req.Stream {
		if !sleepCtx(r.Context(), plan) {
			call.Outcome = "client_cancelled"
			return
		}
		resp := map[string]any{
			"id": id, "object": "chat.completion", "created": start.Unix(), "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		}
		if !prof.NoUsage {
			resp["usage"] = map[string]int{"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "total_tokens": promptTokens + completionTokens}
		}
		writeJSON(w, http.StatusOK, resp)
		call.Outcome = "ok"
		return
	}

	ttfb := prof.ttfb
	if ttfb > plan {
		ttfb = plan
	}
	if !sleepCtx(r.Context(), ttfb) {
		call.Outcome = "client_cancelled"
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	chunk := func(delta map[string]any, finish any) {
		payload := map[string]any{"id": id, "object": "chat.completion.chunk", "created": start.Unix(), "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		raw, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		if flusher != nil {
			flusher.Flush()
		}
	}
	pieces := splitContent(content, 4)
	rest := plan - ttfb
	chunk(map[string]any{"role": "assistant", "content": pieces[0]}, nil)
	truncate := rng.Float64() < prof.PTruncate
	for i := 1; i < len(pieces); i++ {
		if !sleepCtx(r.Context(), rest/time.Duration(len(pieces))) {
			call.Outcome = "client_cancelled"
			return
		}
		if truncate && i == len(pieces)-1 {
			call.Outcome = "truncated"
			return
		}
		chunk(map[string]any{"content": pieces[i]}, nil)
	}
	if !sleepCtx(r.Context(), rest/time.Duration(len(pieces))) {
		call.Outcome = "client_cancelled"
		return
	}
	chunk(map[string]any{}, "stop")
	if !prof.NoUsage && req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		raw, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": start.Unix(), "model": req.Model,
			"choices": []any{}, "usage": map[string]int{"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "total_tokens": promptTokens + completionTokens}})
		fmt.Fprintf(w, "data: %s\n\n", raw)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	call.Outcome = "ok"
}

// fault applies 429 / 503 / hang. It returns the outcome and true when the
// request was answered with a fault.
func (s *server) fault(w http.ResponseWriter, r *http.Request, prof Profile, rng *rand.Rand, plan time.Duration) (string, bool) {
	x := rng.Float64()
	switch {
	case x < prof.P429:
		w.Header().Set("Retry-After", strconv.Itoa(prof.RetryAfterSeconds))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"message": "stub rate limited", "type": "rate_limit_exceeded", "code": "rate_limit_exceeded"}})
		return "429", true
	case x < prof.P429+prof.P503:
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "stub unavailable", "type": "server_error"}})
		return "503", true
	case x < prof.P429+prof.P503+prof.PHang:
		if sleepCtx(r.Context(), prof.hangMax) {
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": map[string]any{"message": "stub hang limit"}})
			return "hang_timeout", true
		}
		return "hang_cancelled", true
	}
	return "", false
}

// ---- System One ----

type systemOneRequest struct {
	Model     string                     `json:"model"`
	State     json.RawMessage            `json:"state"`
	Questions map[string]json.RawMessage `json:"questions"`
}

func (s *server) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	done := s.enter()
	defer done()
	start := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req systemOneRequest
	if err := json.Unmarshal(body, &req); err != nil || len(req.Questions) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "questions required"})
		return
	}
	call := Call{Seq: s.seq.Add(1), Protocol: "systemone", Model: req.Model,
		Probe: probePattern.FindString(string(body)), StartNS: start.UnixNano()}
	if m := memberPattern.FindStringSubmatch(string(body)); m != nil {
		call.Member = m[1]
	}
	prof := s.profile(req.Model)
	rng := rand.New(rand.NewPCG(uint64(start.UnixNano()), uint64(call.Seq)))
	plan := max(prof.latency(rng), 0)
	call.PlanMS = plan.Milliseconds()
	defer func() {
		call.EndNS = time.Now().UnixNano()
		s.record(call)
	}()
	if outcome, ok := s.fault(w, r, prof, rng, plan); ok {
		call.Outcome = outcome
		return
	}
	answers := map[string]any{}
	for name, raw := range req.Questions {
		var q struct {
			Type     string          `json:"type"`
			Criteria json.RawMessage `json:"criteria"`
		}
		_ = json.Unmarshal(raw, &q)
		switch q.Type {
		case "choice":
			var options map[string]any
			_ = json.Unmarshal(q.Criteria, &options)
			keys := make([]string, 0, len(options))
			for k := range options {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			probs := map[string]float64{}
			for _, k := range keys {
				probs[k] = 1 / float64(len(keys))
			}
			choice := ""
			if len(keys) > 0 {
				choice = keys[rng.IntN(len(keys))]
			}
			answers[name] = map[string]any{"type": "choice", "choice": choice, "confidence": 0.5, "probabilities": probs}
		case "score":
			var levels []any
			_ = json.Unmarshal(q.Criteria, &levels)
			probs := map[string]float64{}
			for i := range levels {
				probs[strconv.Itoa(i)] = 1 / float64(len(levels))
			}
			answers[name] = map[string]any{"type": "score", "score": rng.IntN(max(len(levels), 1)), "confidence": 0.5, "probabilities": probs}
		default:
			answers[name] = map[string]any{"type": "noul", "noul": 0.5, "confidence": 0.5}
		}
	}
	if !sleepCtx(r.Context(), plan) {
		call.Outcome = "client_cancelled"
		return
	}
	resp := map[string]any{"model": req.Model, "answers": answers}
	if !prof.NoUsage {
		resp["usage"] = map[string]int{"input_tokens": len(body) / 4, "output_tokens": 16 * len(answers)}
	}
	writeJSON(w, http.StatusOK, resp)
	call.Outcome = "ok"
}

// ---- control ----

func (s *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		raw := s.rawCfg
		s.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	case http.MethodPost, http.MethodPut:
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.setConfig(raw); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) setConfig(raw []byte) error {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if err := cfg.compile(); err != nil {
		return err
	}
	canonical, _ := json.Marshal(cfg)
	s.mu.Lock()
	s.cfg, s.rawCfg = cfg, canonical
	s.mu.Unlock()
	return nil
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost || r.Method == http.MethodDelete {
		s.callsMu.Lock()
		s.calls, s.outcome, s.byModel, s.resetAt = nil, map[string]int64{}, map[string]int64{}, time.Now()
		s.callsMu.Unlock()
		s.maxInflight.Store(s.inflight.Load())
		writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
		return
	}
	s.callsMu.Lock()
	outcome := make(map[string]int64, len(s.outcome))
	for k, v := range s.outcome {
		outcome[k] = v
	}
	byModel := make(map[string]int64, len(s.byModel))
	for k, v := range s.byModel {
		byModel[k] = v
	}
	recorded := len(s.calls)
	resetAt := s.resetAt
	s.callsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"inflight": s.inflight.Load(), "max_inflight": s.maxInflight.Load(),
		"total": s.seq.Load(), "outcomes": outcome, "by_model": byModel,
		"recorded_calls": recorded, "since": resetAt,
	})
}

func (s *server) handleCalls(w http.ResponseWriter, r *http.Request) {
	probe := r.URL.Query().Get("probe")
	s.callsMu.Lock()
	out := make([]Call, 0, len(s.calls))
	for _, c := range s.calls {
		if probe == "" || c.Probe == probe {
			out = append(out, c)
		}
	}
	s.callsMu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleModels(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	names := make([]string, 0, len(s.cfg.Models))
	for name := range s.cfg.Models {
		names = append(names, name)
	}
	s.mu.RUnlock()
	sort.Strings(names)
	data := make([]any, 0, len(names))
	for _, n := range names {
		data = append(data, map[string]string{"id": n, "object": "model", "owned_by": "loadtest-stub"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// ---- helpers ----

// synth returns a minimal value that satisfies a JSON schema of the shapes
// Weave member contracts use (objects, strings, numbers, enums, arrays).
func synth(schema any, probe string, depth int) any {
	m, ok := schema.(map[string]any)
	if !ok || depth > 12 {
		return nil
	}
	if v, ok := m["const"]; ok {
		return v
	}
	if e, ok := m["enum"].([]any); ok && len(e) > 0 {
		return e[0]
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if alts, ok := m[key].([]any); ok && len(alts) > 0 {
			return synth(alts[0], probe, depth+1)
		}
	}
	if all, ok := m["allOf"].([]any); ok && len(all) > 0 {
		return synth(all[0], probe, depth+1)
	}
	typ := ""
	switch t := m["type"].(type) {
	case string:
		typ = t
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok && s != "null" {
				typ = s
				break
			}
		}
	}
	if typ == "" {
		if _, ok := m["properties"]; ok {
			typ = "object"
		}
	}
	switch typ {
	case "object":
		out := map[string]any{}
		props, _ := m["properties"].(map[string]any)
		required := map[string]bool{}
		if req, ok := m["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					required[s] = true
				}
			}
		}
		for name, sub := range props {
			if required[name] || len(required) == 0 {
				out[name] = synth(sub, probe, depth+1)
			}
		}
		return out
	case "array":
		n := 0
		if mi, ok := m["minItems"].(float64); ok {
			n = int(mi)
		}
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, synth(m["items"], probe, depth+1))
		}
		return out
	case "string":
		text := "stub " + probe
		if mi, ok := m["minLength"].(float64); ok && len(text) < int(mi) {
			text += strings.Repeat("x", int(mi)-len(text))
		}
		if ma, ok := m["maxLength"].(float64); ok && len(text) > int(ma) {
			text = text[:int(ma)]
		}
		return text
	case "integer":
		if mi, ok := m["minimum"].(float64); ok {
			return int(math.Ceil(mi))
		}
		return 0
	case "number":
		if mi, ok := m["minimum"].(float64); ok {
			return mi
		}
		return 0
	case "boolean":
		return false
	case "null":
		return nil
	}
	return nil
}

func splitContent(s string, n int) []string {
	if n < 1 || len(s) < n {
		return []string{s}
	}
	runes := []rune(s)
	size := (len(runes) + n - 1) / n
	out := make([]string, 0, n)
	for i := 0; i < len(runes); i += size {
		end := min(i+size, len(runes))
		out = append(out, string(runes[i:end]))
	}
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18090", "listen address (keep it on loopback)")
	latency := flag.String("latency", "fixed:2s", "default latency distribution")
	ttfb := flag.String("ttfb", "", "default time to first streamed byte")
	p429 := flag.Float64("p429", 0, "probability of 429")
	retryAfter := flag.Int("retry-after", 1, "Retry-After seconds on 429")
	p503 := flag.Float64("p503", 0, "probability of 503")
	pHang := flag.Float64("p-hang", 0, "probability of hanging until the client cancels")
	pTrunc := flag.Float64("p-truncate", 0, "probability of a truncated stream")
	noUsage := flag.Bool("no-usage", false, "omit token usage")
	models := flag.String("models", "stub-flash,stub-pro,stub-decision", "comma-separated model names served with the default profile")
	configFile := flag.String("config", "", "JSON config file (overrides the profile flags)")
	keep := flag.Int("keep-calls", 2_000_000, "maximum recorded calls between resets")
	flag.Parse()

	s := &server{outcome: map[string]int64{}, byModel: map[string]int64{}, keep: *keep, resetAt: time.Now()}
	var raw []byte
	if *configFile != "" {
		var err error
		if raw, err = os.ReadFile(*configFile); err != nil {
			log.Fatalf("read config: %v", err)
		}
	} else {
		def := Profile{Latency: *latency, TTFB: *ttfb, P429: *p429, RetryAfterSeconds: *retryAfter,
			P503: *p503, PHang: *pHang, PTruncate: *pTrunc, NoUsage: *noUsage}
		cfg := Config{Default: def, Models: map[string]Profile{}}
		for _, m := range strings.Split(*models, ",") {
			if m = strings.TrimSpace(m); m != "" {
				cfg.Models[m] = def
			}
		}
		raw, _ = json.Marshal(cfg)
	}
	if err := s.setConfig(raw); err != nil {
		log.Fatalf("config: %v", err)
	}

	mux := http.NewServeMux()
	// Any base URL spelling (with or without /v1) reaches the same handlers.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			s.handleChat(w, r)
		case strings.HasSuffix(r.URL.Path, "/systemone"):
			s.handleSystemOne(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/config", s.handleConfig)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/calls", s.handleCalls)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	log.Printf("model stub listening on http://%s", ln.Addr())
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
