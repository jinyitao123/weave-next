// Package embedder provides an HTTP-based Embedder that calls OpenAI-compatible
// /v1/embeddings endpoints.
package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// requestTimeout bounds every embeddings call. Embedding requests are used on
// the chat compile path (semantic skill matching) and in memory writes, where
// callers cannot always thread a cancellable context — a hung endpoint must
// never block a request indefinitely.
const requestTimeout = 10 * time.Second

// HTTPEmbedder implements contract.Embedder via an OpenAI-compatible HTTP API.
type HTTPEmbedder struct {
	baseURL   string
	apiKey    string
	model     string
	dimension int
	client    *http.Client
}

// New creates an HTTPEmbedder.
// baseURL should be like "https://api.openai.com" (no trailing /v1).
func New(baseURL, apiKey, model string, dimension int) *HTTPEmbedder {
	baseURL = strings.TrimRight(baseURL, "/")
	if dimension <= 0 {
		dimension = 1536
	}
	if model == "" {
		model = "text-embedding-3-small"
	}
	return &HTTPEmbedder{
		baseURL:   baseURL,
		apiKey:    apiKey,
		model:     model,
		dimension: dimension,
		client:    &http.Client{Timeout: requestTimeout},
	}
}

type embeddingRequest struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// Embed calls the /v1/embeddings endpoint.
func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(embeddingRequest{
		Input: texts,
		Model: e.model,
	})
	if err != nil {
		return nil, fmt.Errorf("embedder: marshal: %w", err)
	}

	url := e.baseURL + "/v1/embeddings"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedder: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedder: request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("embedder: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedder: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result embeddingResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("embedder: unmarshal: %w", err)
	}

	// Sort by index to ensure correct order.
	embeddings := make([][]float32, len(texts))
	for _, d := range result.Data {
		if d.Index < len(embeddings) {
			embeddings[d.Index] = d.Embedding
		}
	}

	return embeddings, nil
}

// Dimension returns the embedding vector dimension.
func (e *HTTPEmbedder) Dimension() int {
	return e.dimension
}
