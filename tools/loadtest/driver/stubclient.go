package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Model names the stub serves and the team binds.
const (
	modelSeat    = "stub-seat"
	modelRuler   = "stub-ruler"
	modelConsult = "stub-consult"
)

type stubClient struct {
	base string
}

type stubCall struct {
	Seq      int64  `json:"seq"`
	Protocol string `json:"protocol"`
	Model    string `json:"model"`
	Probe    string `json:"probe"`
	Member   string `json:"member"`
	Stream   bool   `json:"stream"`
	Outcome  string `json:"outcome"`
	StartNS  int64  `json:"start_ns"`
	EndNS    int64  `json:"end_ns"`
	PlanMS   int64  `json:"plan_ms"`
}

type stubStats struct {
	Inflight    int64            `json:"inflight"`
	MaxInflight int64            `json:"max_inflight"`
	Total       int64            `json:"total"`
	Outcomes    map[string]int64 `json:"outcomes"`
	ByModel     map[string]int64 `json:"by_model"`
}

func (s *stubClient) call(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 60 * time.Second}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("stub %s %s: HTTP %d: %s", method, path, res.StatusCode, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

type stubProfile struct {
	Latency           string  `json:"latency"`
	TTFB              string  `json:"ttfb,omitempty"`
	P429              float64 `json:"p429,omitempty"`
	RetryAfterSeconds int     `json:"retry_after_s,omitempty"`
	P503              float64 `json:"p503,omitempty"`
	PHang             float64 `json:"p_hang,omitempty"`
	HangMax           string  `json:"hang_max,omitempty"`
}

// configure sets the seat/ruler and consult latency profiles.
func (s *stubClient) configure(ctx context.Context, decideLatency, consultLatency string) error {
	return s.setProfiles(ctx, map[string]stubProfile{
		modelSeat: {Latency: decideLatency}, modelRuler: {Latency: decideLatency}, modelConsult: {Latency: consultLatency},
	})
}

func (s *stubClient) setProfiles(ctx context.Context, models map[string]stubProfile) error {
	return s.call(ctx, http.MethodPost, "/config", map[string]any{"default": stubProfile{Latency: "fixed:100ms"}, "models": models}, nil)
}

func (s *stubClient) reset(ctx context.Context) error {
	return s.call(ctx, http.MethodPost, "/stats", nil, nil)
}

func (s *stubClient) stats(ctx context.Context) (stubStats, error) {
	var st stubStats
	err := s.call(ctx, http.MethodGet, "/stats", nil, &st)
	return st, err
}

func (s *stubClient) calls(ctx context.Context) ([]stubCall, error) {
	var out []stubCall
	err := s.call(ctx, http.MethodGet, "/calls", nil, &out)
	return out, err
}
