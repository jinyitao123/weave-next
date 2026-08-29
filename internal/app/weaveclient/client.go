package weaveclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const maxResponseBytes = 16 << 20

var stableCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,80}$`)

type Error struct {
	Code       string
	StatusCode int
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return "weave API: " + e.Code
}

type Client struct {
	baseURL      *url.URL
	apiKey       string
	httpClient   *http.Client
	pollInterval time.Duration
	waitTimeout  time.Duration
}

func New(config Config, httpClient *http.Client) (*Client, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	baseURL, _ := url.Parse(strings.TrimRight(strings.TrimSpace(config.BaseURL), "/"))
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	pollInterval := config.PollInterval
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	waitTimeout := config.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = 45 * time.Second
	}
	return &Client{
		baseURL: baseURL, apiKey: strings.TrimSpace(config.APIKey),
		httpClient: httpClient, pollInterval: pollInterval, waitTimeout: waitTimeout,
	}, nil
}

type TeamCreateRequest struct {
	YAML            string          `json:"yaml,omitempty"`
	Sample          string          `json:"sample,omitempty"`
	Overrides       map[string]any  `json:"overrides,omitempty"`
	DeclarativeSpec json.RawMessage `json:"declarative_spec,omitempty"`
	IdempotencyKey  string          `json:"idempotency_key"`
}

type DispatchRequest struct {
	TeamID          string
	Task            string
	Mode            string
	WorkflowID      string
	WorkflowVersion *int
	ClientRequestID string
	ProjectID       string
	ConversationID  string
}

type ResumeRequest struct {
	RunID string         `json:"run_id"`
	Agent string         `json:"agent"`
	Input map[string]any `json:"input"`
}

type HumanTaskCompleteRequest struct {
	RunID          string          `json:"-"`
	Payload        json.RawMessage `json:"payload"`
	IdempotencyKey string          `json:"idempotency_key"`
}

type ProviderAddRequest struct {
	ID                       string   `json:"id"`
	Name                     string   `json:"name"`
	BaseURL                  string   `json:"base_url"`
	APIKey                   string   `json:"api_key"`
	Models                   []string `json:"models"`
	JSONObjectMode           bool     `json:"json_object_mode,omitempty"`
	ThinkingDefaultMode      string   `json:"thinking_default_mode,omitempty"`
	ThinkingDisableWithTools bool     `json:"thinking_disable_with_tools,omitempty"`
	AttemptTimeoutSeconds    int      `json:"attempt_timeout_seconds,omitempty"`
}

type APIKeyCreateRequest struct {
	Name      string     `json:"name"`
	Role      string     `json:"role,omitempty"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func (c *Client) TeamTemplateList(ctx context.Context) (json.RawMessage, error) {
	return c.getJSON(ctx, "/v1/team-templates/samples")
}

func (c *Client) ProviderList(ctx context.Context) (json.RawMessage, error) {
	return c.getJSON(ctx, "/v1/providers")
}

func (c *Client) ProviderAdd(ctx context.Context, request ProviderAddRequest) (json.RawMessage, error) {
	return c.sendJSON(ctx, http.MethodPost, "/v1/providers", request)
}

func (c *Client) APIKeyCreate(ctx context.Context, request APIKeyCreateRequest) (json.RawMessage, error) {
	return c.sendJSON(ctx, http.MethodPost, "/v1/auth/api-keys", request)
}

func (c *Client) RuntimeCreate(ctx context.Context, name string) (json.RawMessage, error) {
	return c.sendJSON(ctx, http.MethodPost, "/v1/runtimes", map[string]string{"name": name})
}

func (c *Client) TeamList(ctx context.Context, status string, summary bool) (json.RawMessage, error) {
	query := url.Values{}
	if strings.TrimSpace(status) != "" {
		query.Set("status", strings.TrimSpace(status))
	}
	if summary {
		query.Set("include", "summary")
	}
	path := "/v1/teams"
	if len(query) != 0 {
		path += "?" + query.Encode()
	}
	return c.getJSON(ctx, path)
}

func (c *Client) TeamStatus(ctx context.Context, teamID string) (json.RawMessage, error) {
	return c.getJSON(ctx, "/v1/teams/"+url.PathEscape(strings.TrimSpace(teamID))+"?include=summary")
}

func (c *Client) UsageSummary(ctx context.Context, buildRunID string) (json.RawMessage, error) {
	workspaceUsage, err := c.getJSON(ctx, "/v1/usage")
	if err != nil {
		return nil, err
	}
	result := map[string]json.RawMessage{"workspace_usage": workspaceUsage}
	if strings.TrimSpace(buildRunID) != "" {
		buildUsage, err := c.getJSON(ctx, "/v1/internal/team-build-runs/"+
			url.PathEscape(strings.TrimSpace(buildRunID))+"/usage")
		if err != nil {
			return nil, err
		}
		result["build_usage"] = buildUsage
	}
	return json.Marshal(result)
}

func (c *Client) TeamCreate(ctx context.Context, request TeamCreateRequest) (json.RawMessage, error) {
	if strings.TrimSpace(request.IdempotencyKey) == "" {
		return nil, &Error{Code: "idempotency_key_required"}
	}
	return c.sendJSON(ctx, http.MethodPost, "/v1/teams:from-template", request)
}

func (c *Client) TeamDispatch(ctx context.Context, request DispatchRequest) (string, json.RawMessage, error) {
	teamID := strings.TrimSpace(request.TeamID)
	if teamID == "" || strings.TrimSpace(request.Task) == "" {
		return "", nil, &Error{Code: "team_and_task_required"}
	}
	clientRequestID := strings.TrimSpace(request.ClientRequestID)
	if clientRequestID == "" {
		clientRequestID = uuid.NewString()
	} else if _, err := uuid.Parse(clientRequestID); err != nil {
		return "", nil, &Error{Code: "invalid_client_request_id"}
	}
	body := map[string]any{
		"task": request.Task, "client_request_id": clientRequestID,
	}
	if strings.TrimSpace(request.Mode) != "" {
		body["mode"] = strings.TrimSpace(request.Mode)
	}
	if strings.TrimSpace(request.WorkflowID) != "" {
		body["workflow_id"] = strings.TrimSpace(request.WorkflowID)
	}
	if request.WorkflowVersion != nil {
		body["workflow_version"] = *request.WorkflowVersion
	}
	if strings.TrimSpace(request.ProjectID) != "" {
		body["project_id"] = strings.TrimSpace(request.ProjectID)
	}
	if strings.TrimSpace(request.ConversationID) != "" {
		body["conversation_id"] = strings.TrimSpace(request.ConversationID)
	}
	result, err := c.sendJSON(ctx, http.MethodPost, "/v1/teams/"+url.PathEscape(teamID)+"/dispatch", body)
	if apiErr, ok := err.(*Error); ok && apiErr.StatusCode == http.StatusConflict &&
		apiErr.Code == "client_request_in_progress" {
		result, err = c.DispatchStatus(ctx, clientRequestID)
	}
	return clientRequestID, result, err
}

func (c *Client) TeamDispatchAndWait(ctx context.Context, request DispatchRequest) (string, json.RawMessage, error) {
	clientRequestID, result, err := c.TeamDispatch(ctx, request)
	if err != nil {
		return clientRequestID, nil, err
	}
	if terminalDispatchStatus(result) {
		return clientRequestID, result, nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, c.waitTimeout)
	defer cancel()
	if runID := workflowDispatchRunID(result); runID != "" {
		result, err = c.WaitTeamRun(waitCtx, runID)
		return clientRequestID, result, err
	}
	result, err = c.WaitDispatch(waitCtx, clientRequestID)
	return clientRequestID, result, err
}

func (c *Client) WaitTeamRun(ctx context.Context, snapshotID string) (json.RawMessage, error) {
	var latest json.RawMessage
	for {
		result, err := c.TeamRunStatus(ctx, snapshotID)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) && len(latest) != 0 {
				return latest, nil
			}
			return nil, err
		}
		latest = result
		if terminalTeamRunStatus(result, snapshotID) {
			return result, nil
		}
		timer := time.NewTimer(c.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) && len(latest) != 0 {
				return latest, nil
			}
			return nil, &Error{Code: "poll_cancelled"}
		case <-timer.C:
		}
	}
}

func (c *Client) WaitDispatch(ctx context.Context, clientRequestID string) (json.RawMessage, error) {
	var latest json.RawMessage
	for {
		result, err := c.DispatchStatus(ctx, clientRequestID)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) && len(latest) != 0 {
				return latest, nil
			}
			return nil, err
		}
		latest = result
		if terminalDispatchStatus(result) {
			return result, nil
		}
		timer := time.NewTimer(c.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) && len(latest) != 0 {
				return latest, nil
			}
			return nil, &Error{Code: "poll_cancelled"}
		case <-timer.C:
		}
	}
}

func (c *Client) BuildStatus(ctx context.Context, buildRunID string) (json.RawMessage, error) {
	return c.getJSON(ctx, "/v1/internal/team-build-runs/"+url.PathEscape(strings.TrimSpace(buildRunID))+"/progress")
}

func (c *Client) DispatchStatus(ctx context.Context, clientRequestID string) (json.RawMessage, error) {
	return c.getJSON(ctx, "/v1/chat-requests/"+url.PathEscape(strings.TrimSpace(clientRequestID)))
}

func (c *Client) TeamRunStatus(ctx context.Context, snapshotID string) (json.RawMessage, error) {
	query := url.Values{
		"view": {"run"}, "run_snapshot_id": {strings.TrimSpace(snapshotID)},
		"aggregation_mode": {"all-exclusive"},
	}
	return c.getJSON(ctx, "/v1/runs?"+query.Encode())
}

func (c *Client) HumanTaskList(ctx context.Context, limit int, cursor string) (json.RawMessage, error) {
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if strings.TrimSpace(cursor) != "" {
		query.Set("cursor", strings.TrimSpace(cursor))
	}
	path := "/v1/human-tasks"
	if len(query) != 0 {
		path += "?" + query.Encode()
	}
	return c.getJSON(ctx, path)
}

func (c *Client) HumanTaskGet(ctx context.Context, runID, pointer string, offset, limit int) (json.RawMessage, error) {
	query := url.Values{}
	if pointer != "" {
		query.Set("path", pointer)
	}
	if limit > 0 {
		query.Set("offset", strconv.Itoa(offset))
		query.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/human-tasks/" + url.PathEscape(strings.TrimSpace(runID))
	if len(query) != 0 {
		path += "?" + query.Encode()
	}
	return c.getJSON(ctx, path)
}

func (c *Client) HumanTaskComplete(ctx context.Context, request HumanTaskCompleteRequest) (json.RawMessage, error) {
	input := struct {
		Payload        json.RawMessage `json:"payload"`
		IdempotencyKey string          `json:"idempotency_key"`
	}{Payload: request.Payload, IdempotencyKey: request.IdempotencyKey}
	return c.sendJSON(ctx, http.MethodPost,
		"/v1/human-tasks/"+url.PathEscape(strings.TrimSpace(request.RunID))+"/complete", input)
}

func (c *Client) Resume(ctx context.Context, request ResumeRequest) ([]byte, error) {
	body := struct {
		ResumeRequest
		Stream bool `json:"stream"`
	}{ResumeRequest: request, Stream: true}
	return c.send(ctx, http.MethodPost, "/v1/resume", body)
}

func (c *Client) DeliverableList(ctx context.Context, limit, offset int) (json.RawMessage, error) {
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	path := "/v1/deliverables"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return c.getJSON(ctx, path)
}

func (c *Client) DeliverableGet(ctx context.Context, id string) (json.RawMessage, error) {
	return c.DeliverableGetPath(ctx, id, "", 0, 0)
}

func (c *Client) DeliverableGetPath(ctx context.Context, id, pointer string, offset, limit int) (json.RawMessage, error) {
	query := url.Values{}
	if pointer != "" {
		query.Set("path", pointer)
	}
	if limit > 0 {
		query.Set("offset", strconv.Itoa(offset))
		query.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/deliverables/" + url.PathEscape(strings.TrimSpace(id))
	if len(query) != 0 {
		path += "?" + query.Encode()
	}
	return c.getJSON(ctx, path)
}

type teamDetail struct {
	Team struct {
		Status string `json:"status"`
	} `json:"team"`
	Lead *struct {
		Name string `json:"name"`
	} `json:"lead"`
}

func (c *Client) resolveTeamLead(ctx context.Context, teamID string) (teamDetail, error) {
	body, err := c.getJSON(ctx, "/v1/teams/"+url.PathEscape(teamID))
	if err != nil {
		if apiErr, ok := err.(*Error); ok && apiErr.StatusCode == http.StatusNotFound {
			return teamDetail{}, &Error{Code: "team_not_found", StatusCode: http.StatusNotFound}
		}
		return teamDetail{}, err
	}
	var team teamDetail
	if err := json.Unmarshal(body, &team); err != nil {
		return teamDetail{}, &Error{Code: "invalid_api_response"}
	}
	if team.Team.Status != "active" {
		return teamDetail{}, &Error{Code: "team_not_active", StatusCode: http.StatusConflict}
	}
	if team.Lead == nil || strings.TrimSpace(team.Lead.Name) == "" {
		return teamDetail{}, &Error{Code: "team_lead_unavailable", StatusCode: http.StatusConflict}
	}
	return team, nil
}

func terminalDispatchStatus(body json.RawMessage) bool {
	var response struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &response) != nil {
		return false
	}
	switch response.Status {
	case "completed", "failed", "yielded":
		return true
	default:
		return false
	}
}

func workflowDispatchRunID(body json.RawMessage) string {
	var response struct {
		RunID      string `json:"run_id"`
		WorkflowID string `json:"workflow_id"`
	}
	if json.Unmarshal(body, &response) != nil || response.WorkflowID == "" {
		return ""
	}
	return response.RunID
}

func terminalTeamRunStatus(body json.RawMessage, snapshotID string) bool {
	var response struct {
		Runs []struct {
			RunID  string `json:"run_id"`
			Status string `json:"status"`
		} `json:"runs"`
	}
	if json.Unmarshal(body, &response) != nil || len(response.Runs) == 0 {
		return false
	}
	status := ""
	for _, run := range response.Runs {
		if strings.TrimSpace(run.RunID) == strings.TrimSpace(snapshotID) {
			status = run.Status
			break
		}
	}
	if status == "" && len(response.Runs) == 1 {
		status = response.Runs[0].Status
	}
	switch status {
	case "succeeded", "failed", "cancelled", "abandoned":
		return true
	default:
		return false
	}
}

func (c *Client) getJSON(ctx context.Context, path string) (json.RawMessage, error) {
	body, err := c.send(ctx, http.MethodGet, path, nil)
	return json.RawMessage(body), err
}

func (c *Client) sendJSON(ctx context.Context, method, path string, input any) (json.RawMessage, error) {
	body, err := c.send(ctx, method, path, input)
	return json.RawMessage(body), err
}

func (c *Client) send(ctx context.Context, method, path string, input any) ([]byte, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, &Error{Code: "invalid_request"}
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL.String()+path, body)
	if err != nil {
		return nil, &Error{Code: "invalid_request"}
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, &Error{Code: "transport_error"}
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &Error{Code: "response_read_failed", StatusCode: response.StatusCode}
	}
	if len(responseBody) > maxResponseBytes {
		return nil, &Error{Code: "response_too_large", StatusCode: response.StatusCode}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, apiError(response.StatusCode, responseBody)
	}
	return responseBody, nil
}

func apiError(status int, body []byte) error {
	var response struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &response)
	code := strings.TrimSpace(response.Code)
	if !stableCodePattern.MatchString(code) {
		code = strings.TrimSpace(response.Error)
	}
	if !stableCodePattern.MatchString(code) {
		code = fmt.Sprintf("http_%d", status)
	}
	return &Error{Code: code, StatusCode: status}
}
