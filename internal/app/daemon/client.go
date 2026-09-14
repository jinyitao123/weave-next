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

	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
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
	capabilities []runtimeprotocol.EngineCapability,
	totalSlots int,
) error {
	response, err := c.do(ctx, http.MethodPost, "/v1/runtime/hello", runtimeprotocol.HostHelloRequest{Versioned: runtimeprotocol.NewVersioned(), Engines: engines, EngineCapabilities: capabilities, TotalSlots: totalSlots})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := expectStatus(response, http.StatusOK); err != nil {
		return err
	}
	var hello runtimeprotocol.HostHelloResponse
	if err := json.NewDecoder(response.Body).Decode(&hello); err != nil {
		return fmt.Errorf("daemon: decode hello response: %w", err)
	}
	if err := hello.Versioned.Validate(); err != nil {
		return err
	}
	return nil
}

func (c *runtimeClient) heartbeat(ctx context.Context, activeSlots int) error {
	return c.postNoContent(ctx, "/v1/runtime/heartbeat", runtimeprotocol.HostHeartbeatRequest{Versioned: runtimeprotocol.NewVersioned(), ActiveSlots: activeSlots})
}

func (c *runtimeClient) claim(ctx context.Context, waitSeconds int) (*runtimeprotocol.ExecutionClaim, error) {
	response, err := c.do(ctx, http.MethodPost, "/v1/runtime/claim", runtimeprotocol.ClaimRequest{Versioned: runtimeprotocol.NewVersioned(), WaitSeconds: waitSeconds})
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
	var envelope runtimeprotocol.ClaimResponse
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("daemon: decode claim response: %w", err)
	}
	if err := envelope.Versioned.Validate(); err != nil {
		return nil, err
	}
	if envelope.Claim == nil {
		return nil, errors.New("daemon: claim response omitted claim")
	}
	if err := envelope.Claim.Validate(); err != nil {
		return nil, err
	}
	return envelope.Claim, nil
}

func (c *runtimeClient) renew(ctx context.Context, claim *runtimeprotocol.ExecutionClaim) error {
	return c.postTaskNoContent(ctx, claim.TaskID, "renew", runtimeprotocol.LeaseRequest{Versioned: runtimeprotocol.NewVersioned(), TaskID: claim.TaskID, ClaimEpoch: claim.ClaimEpoch, Subject: claim.Subject})
}

func (c *runtimeClient) stopped(ctx context.Context, claim *runtimeprotocol.ExecutionClaim, result *runtimeprotocol.ExecutionReceipt) error {
	receiptID, digest := "", ""
	if result != nil {
		receiptID, digest = result.Identity(), result.Digest()
	}
	return c.postTaskNoContent(ctx, claim.TaskID, "stopped", runtimeprotocol.StoppedReceipt{ReceiptID: receiptID, ResultDigest: digest, Result: result, Versioned: runtimeprotocol.NewVersioned(), SchemaVersion: runtimeprotocol.ReceiptSchemaV1, TaskID: claim.TaskID, ClaimEpoch: claim.ClaimEpoch, Subject: claim.Subject})
}

func (c *runtimeClient) complete(ctx context.Context, receipt runtimeprotocol.ExecutionReceipt) error {
	return c.postTaskNoContent(ctx, receipt.TaskID, "complete", receipt)
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
	request.Header.Set(runtimeprotocol.HeaderVersion, runtimeprotocol.ProtocolVersion)
	writeTaskProof(ctx, request.Header)
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
