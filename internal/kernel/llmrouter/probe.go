package llmrouter

import (
	"context"
	"errors"
	"net"
	"net/url"
	"time"

	"github.com/jinyitao123/loom/contract"
)

// Probe outcome categories. Each names what to check next; none carries the
// provider's response body, the key or the request.
const (
	ProbeOK               = "ok"
	ProbeUnreachable      = "unreachable"
	ProbeTimeout          = "timeout"
	ProbeUnauthorized     = "unauthorized"
	ProbeEndpointNotFound = "endpoint_not_found"
	ProbeRequestRejected  = "request_rejected"
	ProbeRateLimited      = "rate_limited"
	ProbeProviderError    = "provider_error"
	ProbeInvalidResponse  = "invalid_response"
)

// ProbeResult is what one probe call learned about a provider endpoint.
type ProbeResult struct {
	ChatURL  string `json:"chat_url"`
	Model    string `json:"model"`
	Category string `json:"category"`
	// Status is the provider's HTTP status when it answered with an error, 0
	// when it did not answer or answered successfully.
	Status    int   `json:"status,omitempty"`
	LatencyMS int64 `json:"latency_ms"`
}

// ProbeProvider sends one minimal chat request to model through the same
// client runs build for cfg, so a probe that passes exercises the address,
// protocol and key that runs will use.
func ProbeProvider(ctx context.Context, cfg ProviderConfig, model string, timeout time.Duration) ProbeResult {
	result := ProbeResult{ChatURL: cfg.ChatURL(), Model: model}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	_, err := NewProviderClient(cfg).Chat(ctx, contract.ChatRequest{
		Model:     model,
		Messages:  []contract.Message{{Role: "user", Content: "ping"}},
		MaxTokens: 16,
	})
	result.LatencyMS = time.Since(started).Milliseconds()
	result.Status, result.Category = ClassifyProbeFailure(err)
	return result
}

// ClassifyProbeFailure names the kind of failure a probe call returned and,
// when the provider answered, its HTTP status. It reads typed evidence only.
func ClassifyProbeFailure(err error) (int, string) {
	if err == nil {
		return 0, ProbeOK
	}
	status := DiagnoseCallError(err).HTTPStatus
	switch {
	case status == 401 || status == 403:
		return status, ProbeUnauthorized
	case status == 404 || status == 405:
		return status, ProbeEndpointNotFound
	case status == 429:
		return status, ProbeRateLimited
	case status >= 500:
		return status, ProbeProviderError
	case status >= 400:
		return status, ProbeRequestRejected
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return 0, ProbeTimeout
	}
	var urlErr *url.Error
	var opErr *net.OpError
	var dnsErr *net.DNSError
	if errors.As(err, &urlErr) || errors.As(err, &opErr) || errors.As(err, &dnsErr) {
		return 0, ProbeUnreachable
	}
	return status, ProbeInvalidResponse
}
