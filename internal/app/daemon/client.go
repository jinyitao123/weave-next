package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

var errLeaseLost = errors.New("daemon: task lease lost")

type runtimeClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func newRuntimeClient(server, token string, httpClient *http.Client) (*runtimeClient, error) {
	parsed, err := url.Parse(server)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("daemon: invalid server URL %q", server)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &runtimeClient{
		baseURL:    strings.TrimRight(server, "/"),
		token:      token,
		httpClient: httpClient,
	}, nil
}

func (c *runtimeClient) hello(
	ctx context.Context,
	engines []string,
	capabilities []runtimes.EngineCapability,
	totalSlots int,
) error {
	response, err := c.do(ctx, http.MethodPost, "/v1/runtime/hello", map[string]any{
		"engines": engines, "engine_capabilities": capabilities, "total_slots": totalSlots,
	})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := expectStatus(response, http.StatusOK); err != nil {
		return err
	}
	var hello struct {
		RuntimeID string `json:"runtime_id"`
		Name      string `json:"name"`
	}
	if err := json.NewDecoder(response.Body).Decode(&hello); err != nil {
		return fmt.Errorf("daemon: decode hello response: %w", err)
	}
	return nil
}

func (c *runtimeClient) heartbeat(ctx context.Context, activeSlots int) error {
	return c.postNoContent(ctx, "/v1/runtime/heartbeat", map[string]int{"active_slots": activeSlots})
}

func (c *runtimeClient) claim(ctx context.Context, waitSeconds int) (*taskqueue.Task, error) {
	response, err := c.do(ctx, http.MethodPost, "/v1/runtime/claim", map[string]int{"wait_seconds": waitSeconds})
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if err := expectStatus(response, http.StatusOK); err != nil {
		return nil, err
	}
	var envelope struct {
		Task *taskqueue.Task `json:"task"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("daemon: decode claim response: %w", err)
	}
	if envelope.Task == nil {
		return nil, errors.New("daemon: claim response omitted task")
	}
	return envelope.Task, nil
}

func (c *runtimeClient) renew(ctx context.Context, taskID string) error {
	return c.postTaskNoContent(ctx, taskID, "renew", nil)
}

func (c *runtimeClient) stopped(ctx context.Context, taskID string) error {
	return c.postTaskNoContent(ctx, taskID, "stopped", nil)
}

func (c *runtimeClient) complete(ctx context.Context, taskID string, result runtimes.EngineExecResult) error {
	return c.postTaskNoContent(ctx, taskID, "complete", result)
}

func (c *runtimeClient) fail(ctx context.Context, taskID, message string) error {
	return c.postTaskNoContent(ctx, taskID, "fail", map[string]string{"error": message})
}

func (c *runtimeClient) downloadAttachment(ctx context.Context, taskID, attachmentID string, dst io.Writer) error {
	path := "/v1/runtime/tasks/" + url.PathEscape(taskID) + "/attachments/" + url.PathEscape(attachmentID)
	response, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := expectStatus(response, http.StatusOK); err != nil {
		return err
	}
	if _, err := io.Copy(dst, response.Body); err != nil {
		return fmt.Errorf("daemon: download attachment %q: %w", attachmentID, err)
	}
	return nil
}

func (c *runtimeClient) postTaskNoContent(ctx context.Context, taskID, action string, body any) error {
	path := "/v1/runtime/tasks/" + url.PathEscape(taskID) + "/" + action
	err := c.postNoContent(ctx, path, body)
	switch statusCode(err) {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict:
		return errLeaseLost
	}
	return err
}

func (c *runtimeClient) postNoContent(ctx context.Context, path string, body any) error {
	response, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return expectStatus(response, http.StatusNoContent)
}

func (c *runtimeClient) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("daemon: encode request: %w", err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, requestBody)
	if err != nil {
		return nil, fmt.Errorf("daemon: create request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("daemon: %s %s: %w", method, path, err)
	}
	return response, nil
}

type httpStatusError struct {
	code int
	body string
}

func (e *httpStatusError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("daemon: server returned HTTP %d", e.code)
	}
	return fmt.Sprintf("daemon: server returned HTTP %d: %s", e.code, e.body)
}

func expectStatus(response *http.Response, want int) error {
	if response.StatusCode == want {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	return &httpStatusError{code: response.StatusCode, body: strings.TrimSpace(string(body))}
}

func statusCode(err error) int {
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) {
		return statusErr.code
	}
	return 0
}
