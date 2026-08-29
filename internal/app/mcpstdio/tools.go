package mcpstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/app/weaveclient"
)

type ToolDispatcher struct {
	client *weaveclient.Client
}

func NewToolDispatcher(client *weaveclient.Client) *ToolDispatcher {
	return &ToolDispatcher{client: client}
}

func (d *ToolDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	return append([]contract.ToolDef(nil), toolDefinitions...), nil
}

func (d *ToolDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if d == nil || d.client == nil {
		return toolError(call.ID, "client_unavailable"), nil
	}
	switch call.Name {
	case "team_template_list":
		var input struct{}
		if err := decodeArguments(call.Args, &input); err != nil {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.TeamTemplateList(ctx)
		return documentResult(call.ID, result, err), nil
	case "team_create":
		var input weaveclient.TeamCreateRequest
		if err := decodeArguments(call.Args, &input); err != nil {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.TeamCreate(ctx, input)
		return documentResult(call.ID, result, err), nil
	case "provider_list":
		var input struct{}
		if err := decodeArguments(call.Args, &input); err != nil {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.ProviderList(ctx)
		return documentResult(call.ID, result, err), nil
	case "provider_add":
		var input weaveclient.ProviderAddRequest
		if err := decodeArguments(call.Args, &input); err != nil || strings.TrimSpace(input.Name) == "" ||
			strings.TrimSpace(input.BaseURL) == "" || strings.TrimSpace(input.APIKey) == "" || len(input.Models) == 0 {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.ProviderAdd(ctx, input)
		return documentResult(call.ID, result, err), nil
	case "apikey_create":
		var input weaveclient.APIKeyCreateRequest
		if err := decodeArguments(call.Args, &input); err != nil || strings.TrimSpace(input.Name) == "" || len(input.Scopes) == 0 {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.APIKeyCreate(ctx, input)
		return documentResult(call.ID, result, err), nil
	case "runtime_create":
		var input struct {
			Name string `json:"name"`
		}
		if err := decodeArguments(call.Args, &input); err != nil || strings.TrimSpace(input.Name) == "" {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.RuntimeCreate(ctx, input.Name)
		return documentResult(call.ID, result, err), nil
	case "team_list":
		var input struct {
			Status  string `json:"status"`
			Summary bool   `json:"summary"`
		}
		if err := decodeArguments(call.Args, &input); err != nil {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.TeamList(ctx, input.Status, input.Summary)
		return documentResult(call.ID, result, err), nil
	case "team_status":
		var input struct {
			TeamID string `json:"team_id"`
		}
		if err := decodeArguments(call.Args, &input); err != nil || strings.TrimSpace(input.TeamID) == "" {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.TeamStatus(ctx, input.TeamID)
		return documentResult(call.ID, result, err), nil
	case "usage_summary":
		var input struct {
			BuildID string `json:"build_id"`
		}
		if err := decodeArguments(call.Args, &input); err != nil {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.UsageSummary(ctx, input.BuildID)
		return documentResult(call.ID, result, err), nil
	case "team_dispatch":
		var input struct {
			TeamID          string `json:"team_id"`
			Task            string `json:"task"`
			ClientRequestID string `json:"client_request_id"`
			ProjectID       string `json:"project_id"`
			Wait            bool   `json:"wait"`
		}
		if err := decodeArguments(call.Args, &input); err != nil {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		request := weaveclient.DispatchRequest{
			TeamID: input.TeamID, Task: input.Task,
			ClientRequestID: input.ClientRequestID, ProjectID: input.ProjectID,
		}
		var id string
		var result json.RawMessage
		var err error
		if input.Wait {
			id, result, err = d.client.TeamDispatchAndWait(ctx, request)
		} else {
			id, result, err = d.client.TeamDispatch(ctx, request)
		}
		if err != nil {
			return toolError(call.ID, clientErrorCode(err)), nil
		}
		body, err := json.Marshal(map[string]any{"client_request_id": id, "result": result})
		if err != nil {
			return toolError(call.ID, "output_failed"), nil
		}
		return &contract.ToolResult{CallID: call.ID, Content: string(body)}, nil
	case "build_status":
		var input struct {
			BuildID string `json:"build_id"`
		}
		if err := decodeArguments(call.Args, &input); err != nil || strings.TrimSpace(input.BuildID) == "" {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.BuildStatus(ctx, input.BuildID)
		return documentResult(call.ID, result, err), nil
	case "dispatch_status":
		var input struct {
			ClientRequestID string `json:"client_request_id"`
		}
		if err := decodeArguments(call.Args, &input); err != nil || strings.TrimSpace(input.ClientRequestID) == "" {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.DispatchStatus(ctx, input.ClientRequestID)
		return documentResult(call.ID, result, err), nil
	case "team_run_status":
		var input struct {
			SnapshotID string `json:"snapshot_id"`
		}
		if err := decodeArguments(call.Args, &input); err != nil || strings.TrimSpace(input.SnapshotID) == "" {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.TeamRunStatus(ctx, input.SnapshotID)
		return documentResult(call.ID, result, err), nil
	case "human_task_list":
		var input struct {
			Limit  int    `json:"limit"`
			Cursor string `json:"cursor"`
		}
		if err := decodeArguments(call.Args, &input); err != nil || input.Limit < 0 || input.Limit > 100 {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.HumanTaskList(ctx, input.Limit, input.Cursor)
		return documentResult(call.ID, result, err), nil
	case "human_task_get":
		var input struct {
			RunID  string `json:"run_id"`
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := decodeArguments(call.Args, &input); err != nil || strings.TrimSpace(input.RunID) == "" ||
			input.Offset < 0 || input.Limit < 0 || input.Limit > 10_000 || (input.Offset > 0 && input.Limit == 0) {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.HumanTaskGet(ctx, input.RunID, input.Path, input.Offset, input.Limit)
		return documentResult(call.ID, result, err), nil
	case "human_task_complete":
		var input weaveclient.HumanTaskCompleteRequest
		if err := decodeArguments(call.Args, &input); err != nil || strings.TrimSpace(input.RunID) == "" ||
			strings.TrimSpace(input.IdempotencyKey) == "" || len(input.Payload) == 0 {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.HumanTaskComplete(ctx, input)
		return documentResult(call.ID, result, err), nil
	case "resume":
		var input weaveclient.ResumeRequest
		if err := decodeArguments(call.Args, &input); err != nil ||
			strings.TrimSpace(input.RunID) == "" || strings.TrimSpace(input.Agent) == "" {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.Resume(ctx, input)
		if err != nil {
			return toolError(call.ID, clientErrorCode(err)), nil
		}
		return &contract.ToolResult{CallID: call.ID, Content: string(result)}, nil
	case "deliverable_list":
		var input struct {
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
		}
		if err := decodeArguments(call.Args, &input); err != nil || input.Limit < 0 || input.Offset < 0 {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.DeliverableList(ctx, input.Limit, input.Offset)
		return documentResult(call.ID, result, err), nil
	case "deliverable_get":
		var input struct {
			ID     string `json:"id"`
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := decodeArguments(call.Args, &input); err != nil || strings.TrimSpace(input.ID) == "" ||
			input.Offset < 0 || input.Limit < 0 || input.Limit > 10_000 || (input.Offset > 0 && input.Limit == 0) {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.DeliverableGetPath(ctx, input.ID, input.Path, input.Offset, input.Limit)
		return documentResult(call.ID, result, err), nil
	default:
		return toolError(call.ID, "unknown_tool"), nil
	}
}

func decodeArguments(raw string, target any) error {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("multiple argument values")
	}
	return nil
}

func documentResult(callID string, document json.RawMessage, err error) *contract.ToolResult {
	if err != nil {
		return toolError(callID, clientErrorCode(err))
	}
	if len(document) == 0 {
		document = json.RawMessage(`null`)
	}
	return &contract.ToolResult{CallID: callID, Content: string(document)}
}

func clientErrorCode(err error) string {
	var apiErr *weaveclient.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return "tool_failed"
}

func toolError(callID, code string) *contract.ToolResult {
	encoded, _ := json.Marshal(map[string]string{"error": code})
	return &contract.ToolResult{CallID: callID, Content: string(encoded), IsError: true}
}

type toolAccessPolicy struct {
	Role   string
	Scopes []string
}

var toolAccessPolicies = map[string]toolAccessPolicy{
	"team_template_list":  {Role: "any", Scopes: []string{"org"}},
	"team_create":         {Role: "admin", Scopes: []string{"org"}},
	"provider_list":       {Role: "any", Scopes: []string{"admin"}},
	"provider_add":        {Role: "admin", Scopes: []string{"admin"}},
	"apikey_create":       {Role: "admin", Scopes: []string{"admin"}},
	"runtime_create":      {Role: "any", Scopes: []string{"org"}},
	"team_list":           {Role: "any", Scopes: []string{"org"}},
	"team_status":         {Role: "any", Scopes: []string{"org"}},
	"usage_summary":       {Role: "any", Scopes: []string{"runs", "org"}},
	"team_dispatch":       {Role: "any", Scopes: []string{"org", "chat"}},
	"build_status":        {Role: "any", Scopes: []string{"org"}},
	"dispatch_status":     {Role: "any", Scopes: []string{"chat"}},
	"team_run_status":     {Role: "any", Scopes: []string{"runs"}},
	"human_task_list":     {Role: "workspace_member", Scopes: []string{"runs"}},
	"human_task_get":      {Role: "workspace_member", Scopes: []string{"runs"}},
	"human_task_complete": {Role: "workspace_member", Scopes: []string{"runs"}},
	"resume":              {Role: "any", Scopes: []string{"chat"}},
	"deliverable_list":    {Role: "any", Scopes: []string{"chat"}},
	"deliverable_get":     {Role: "any", Scopes: []string{"chat"}},
}

var toolDefinitions = []contract.ToolDef{
	{
		Name: "team_template_list", ReadOnly: true,
		Description: "List available team templates. Requires an API key with organization access. Returns names, descriptions, and YAML. Errors: http_401, http_403.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	},
	{
		Name:        "team_create",
		Description: "Create a team from YAML or a named sample, optionally with a declarative workflow specification. Requires administrator and organization access plus a caller-supplied idempotency_key UUID. Returns team and build identifiers with current status. Errors: idempotency_key_required, template_idempotency_conflict, http_401, http_403, http_422.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"yaml":{"type":"string"},"sample":{"type":"string"},"overrides":{"type":"object"},"declarative_spec":{"type":"object"},"idempotency_key":{"type":"string","format":"uuid"}},"required":["idempotency_key"],"anyOf":[{"required":["yaml"]},{"required":["sample"]}],"additionalProperties":false}`),
	},
	{
		Name: "provider_list", ReadOnly: true,
		Description: "List configured model providers without secret values. Requires admin access. Returns provider metadata and immutable revision facts. Errors: http_401, http_403, http_500.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	},
	{
		Name:        "provider_add",
		Description: "Add or revise an OpenAI-compatible model provider. Requires administrator role and admin access. The API key is accepted as secret input and is never returned. Errors: invalid_arguments, http_400, http_401, http_403, http_409.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"name":{"type":"string"},"base_url":{"type":"string"},"api_key":{"type":"string"},"models":{"type":"array","items":{"type":"string"},"minItems":1},"json_object_mode":{"type":"boolean"},"thinking_default_mode":{"type":"string"},"thinking_disable_with_tools":{"type":"boolean"},"attempt_timeout_seconds":{"type":"integer","minimum":0}},"required":["name","base_url","api_key","models"],"additionalProperties":false}`),
	},
	{
		Name:        "apikey_create",
		Description: "Create an API key owned by the current authenticated user. Requires administrator role and admin access. The raw key is returned once. Errors: invalid_arguments, http_400, http_401, http_403.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"role":{"type":"string"},"scopes":{"type":"array","items":{"type":"string"},"minItems":1},"expires_at":{"type":"string","format":"date-time"}},"required":["name","scopes"],"additionalProperties":false}`),
	},
	{
		Name:        "runtime_create",
		Description: "Create a CLI runtime registration. Requires organization access. Returns the one-time runtime token plus ready-to-run direct, install-script, and Docker commands. Errors: invalid_arguments, http_401, http_403, http_500.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`),
	},
	{
		Name: "team_list", ReadOnly: true,
		Description: "List workspace teams, optionally filtered by lifecycle status and expanded with summaries. Requires organization access. Errors: http_400, http_401, http_403.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","enum":["active","needs_repair","building","archived","all"]},"summary":{"type":"boolean"}},"additionalProperties":false}`),
	},
	{
		Name: "team_status", ReadOnly: true,
		Description: "Get one team's roster, lifecycle status, and operational summary by exact team_id. Requires organization access. Errors: http_401, http_403, http_404.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"team_id":{"type":"string"}},"required":["team_id"],"additionalProperties":false}`),
	},
	{
		Name: "usage_summary", ReadOnly: true,
		Description: "Summarize workspace run usage and optionally include one exact build's budget usage and sources. Requires run access; a build_id also requires organization access. Errors: http_400, http_401, http_403, http_404.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"build_id":{"type":"string"}},"additionalProperties":false}`),
	},
	{
		Name:        "team_dispatch",
		Description: "Send a task asynchronously to an active team's lead. Requires organization and chat access. A supplied client_request_id UUID makes retries converge; keep the same API key for the full dispatch lifecycle. Set wait to return completed, failed, or yielded status. Leads backed by CLI runtimes reject asynchronous dispatch with loom_runtime_inference_requires_stream. Errors: team_not_found, team_not_active, team_lead_unavailable, invalid_client_request_id, client_request_conflict, http_401, http_403.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"team_id":{"type":"string"},"task":{"type":"string"},"client_request_id":{"type":"string","format":"uuid"},"project_id":{"type":"string"},"wait":{"type":"boolean","default":false}},"required":["team_id","task"],"additionalProperties":false}`),
	},
	{
		Name: "build_status", ReadOnly: true,
		Description: "Get one team build's progress. Requires organization access and the exact build_id returned by team_create. Returns build status and visible steps. Errors: http_401, http_403, http_404.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"build_id":{"type":"string"}},"required":["build_id"],"additionalProperties":false}`),
	},
	{
		Name: "dispatch_status", ReadOnly: true,
		Description: "Get one dispatch request's status. Requires chat access, the exact client_request_id, and the same API key used to dispatch. Returns queued, completed, failed, or yielded details when available. Errors: chat_request_not_found, http_401, http_403.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"client_request_id":{"type":"string","format":"uuid"}},"required":["client_request_id"],"additionalProperties":false}`),
	},
	{
		Name: "team_run_status", ReadOnly: true,
		Description: "Get run records for one exact team run snapshot. Requires run access and snapshot_id; IDs are not auto-detected. Returns matching run summaries. Errors: http_400, http_401, http_403.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"snapshot_id":{"type":"string"}},"required":["snapshot_id"],"additionalProperties":false}`),
	},
	{
		Name: "human_task_list", ReadOnly: true,
		Description: "List current workspace human tasks as paginated summaries without predecessor content. Requires run access and current workspace membership. Returns task summaries, total, and next_cursor. Errors: http_400, http_401, http_403.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":100},"cursor":{"type":"string"}},"additionalProperties":false}`),
	},
	{
		Name: "human_task_get", ReadOnly: true,
		Description: "Read one human task and its predecessor outputs. Requires run access and current workspace membership. path is an RFC 6901 JSON Pointer and returns the selected JSON value itself; use offset and limit to page selected strings, arrays, or objects. Errors: invalid_json_pointer, human_task_value_not_found, human_task_value_too_large, http_401, http_403, http_404.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string"},"path":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":10000}},"required":["run_id"],"additionalProperties":false}`),
	},
	{
		Name:        "human_task_complete",
		Description: "Complete one human task with a payload matching its resume_schema and a caller-supplied idempotency_key. Requires run access and current workspace membership. Returns queued status and whether the completion was idempotent. Errors: http_400, http_401, http_403, http_404, http_409, http_422.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string"},"payload":{},"idempotency_key":{"type":"string","minLength":1,"maxLength":256}},"required":["run_id","payload","idempotency_key"],"additionalProperties":false}`),
	},
	{
		Name:        "resume",
		Description: "Resume a yielded run with human input. Requires chat access plus the yielded run_id and its lead agent name. Returns the server event stream as text. Errors: invalid_arguments, stream_required, http_401, http_403, http_404, http_409.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string"},"agent":{"type":"string"},"input":{"type":"object"}},"required":["run_id","agent","input"],"additionalProperties":false}`),
	},
	{
		Name: "deliverable_list", ReadOnly: true,
		Description: "List saved deliverables. Requires chat access. Team and run filtering are unavailable. A dispatch produces a deliverable only when an agent explicitly saves one. Returns deliverable records. Errors: http_401, http_403.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":0},"offset":{"type":"integer","minimum":0}},"additionalProperties":false}`),
	},
	{
		Name: "deliverable_get", ReadOnly: true,
		Description: "Get one saved deliverable by exact id. Requires chat access. For JSON content, path is an RFC 6901 pointer and returns the selected value itself; use offset and limit to page selected strings, arrays, or objects. Errors: final_deliverable_not_found, invalid_json_pointer, deliverable_value_not_found, deliverable_value_too_large, http_401, http_403.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"path":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":10000}},"required":["id"],"additionalProperties":false}`),
	},
}

var _ contract.ToolDispatcher = (*ToolDispatcher)(nil)
