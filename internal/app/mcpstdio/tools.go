package mcpstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/app/weaveclient"
)

type capabilityPlanArguments struct {
	BusinessRequest string `json:"business_request"`
	Model           string `json:"model"`
	CapabilityID    string `json:"capability_id,omitempty"`
	IdempotencyKey  string `json:"idempotency_key"`
}

type capabilityPublishArguments struct {
	CapabilityID string `json:"capability_id"`
	Revision     int64  `json:"revision"`
}

type ToolDispatcher struct {
	client              *weaveclient.Client
	boundDispatchHidden bool
}

func NewToolDispatcher(client *weaveclient.Client) *ToolDispatcher {
	return &ToolDispatcher{client: client, boundDispatchHidden: os.Getenv("WEAVE_WORKBENCH_BOUND_DISPATCH") == "1"}
}

func (d *ToolDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	listed := make([]contract.ToolDef, 0, len(toolDefinitions))
	for _, definition := range toolDefinitions {
		if d.boundDispatchHidden && definition.Name == "team_dispatch" {
			continue
		}
		listed = append(listed, definition)
	}
	return listed, nil
}

func (d *ToolDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if d == nil || d.client == nil {
		return toolError(call.ID, "client_unavailable"), nil
	}
	switch call.Name {
	case "capability_list":
		var input struct{}
		if err := decodeArguments(call.Args, &input); err != nil {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.CapabilityList(ctx)
		if err == nil {
			result, err = compactCapabilityCatalog(result)
		}
		return documentResult(call.ID, result, err), nil
	case "capability_plan":
		var input capabilityPlanArguments
		if err := decodeArguments(call.Args, &input); err != nil || strings.TrimSpace(input.BusinessRequest) == "" || strings.TrimSpace(input.Model) == "" || strings.TrimSpace(input.IdempotencyKey) == "" {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.CapabilityPlan(ctx, weaveclient.CapabilityPlanRequest{Prompt: input.BusinessRequest, Model: input.Model, CapabilityID: input.CapabilityID, IdempotencyKey: input.IdempotencyKey})
		if err == nil {
			result, err = compactCapabilityPlan(result)
		}
		return documentResult(call.ID, result, err), nil
	case "capability_publish":
		var input capabilityPublishArguments
		if err := decodeArguments(call.Args, &input); err != nil || strings.TrimSpace(input.CapabilityID) == "" || input.Revision < 1 {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.CapabilityPublish(ctx, input.CapabilityID, input.Revision)
		if err == nil {
			result, err = compactCapabilityPublication(result)
		}
		return documentResult(call.ID, result, err), nil
	case "capability_invoke":
		var input struct {
			CapabilityID string          `json:"capability_id"`
			Revision     int64           `json:"revision"`
			RequestID    string          `json:"request_id"`
			Input        json.RawMessage `json:"input"`
		}
		if err := decodeArguments(call.Args, &input); err != nil {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.CapabilityInvoke(ctx, input.CapabilityID, input.Revision, input.RequestID, input.Input)
		return documentResult(call.ID, result, err), nil
	case "capability_status":
		var input struct {
			InvocationID string `json:"invocation_id"`
		}
		if err := decodeArguments(call.Args, &input); err != nil {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.CapabilityInvocation(ctx, input.InvocationID)
		return documentResult(call.ID, result, err), nil
	case "capability_history":
		var input struct{}
		if err := decodeArguments(call.Args, &input); err != nil {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.CapabilityHistory(ctx)
		return documentResult(call.ID, result, err), nil
	case "capability_resume":
		var input struct {
			InvocationID string          `json:"invocation_id"`
			StepID       string          `json:"step_id"`
			Response     json.RawMessage `json:"response"`
		}
		if err := decodeArguments(call.Args, &input); err != nil {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.CapabilityResume(ctx, input.InvocationID, input.StepID, input.Response)
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
		result, err := d.client.TeamList(ctx, input.Status, true)
		if err != nil {
			return documentResult(call.ID, nil, err), nil
		}
		result, err = compactTeamList(result)
		if err != nil {
			return toolError(call.ID, "invalid_api_response"), nil
		}
		return documentResult(call.ID, result, nil), nil
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
		var input struct{}
		if err := decodeArguments(call.Args, &input); err != nil {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.UsageSummary(ctx)
		return documentResult(call.ID, result, err), nil
	case "team_dispatch":
		var input struct {
			TeamID          string          `json:"team_id"`
			Task            json.RawMessage `json:"task"`
			InputRevisionID string          `json:"input_revision_id"`
			ClientRequestID json.RawMessage `json:"client_request_id"`
			Wait            bool            `json:"wait"`
		}
		if err := decodeArguments(call.Args, &input); err != nil {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		if strings.TrimSpace(input.InputRevisionID) == "" {
			return toolError(call.ID, "dispatch_input_required"), nil
		}
		var task string
		if len(input.Task) > 0 && (bytes.Equal(bytes.TrimSpace(input.Task), []byte("null")) || json.Unmarshal(input.Task, &task) != nil) {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		var clientRequestID string
		if len(input.ClientRequestID) > 0 && (json.Unmarshal(input.ClientRequestID, &clientRequestID) != nil ||
			clientRequestID == "" || strings.TrimSpace(clientRequestID) != clientRequestID) {
			return toolError(call.ID, "invalid_client_request_id"), nil
		}
		request := weaveclient.DispatchRequest{
			TeamID: input.TeamID, Task: task, TaskProvided: len(input.Task) > 0,
			InputRevisionID: input.InputRevisionID, ClientRequestID: clientRequestID,
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
		body, err := normalizeDispatchResult(id, result)
		if err != nil {
			return toolError(call.ID, "output_failed"), nil
		}
		return &contract.ToolResult{CallID: call.ID, Content: string(body)}, nil
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
	case "team_run_activity":
		var input struct {
			RunID string `json:"run_id"`
		}
		if err := decodeArguments(call.Args, &input); err != nil || strings.TrimSpace(input.RunID) == "" {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.TeamRunActivity(ctx, input.RunID)
		return documentResult(call.ID, result, err), nil
	case "team_run_stop":
		var input struct {
			RunID          string `json:"run_id"`
			Reason         string `json:"reason"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := decodeArguments(call.Args, &input); err != nil || strings.TrimSpace(input.RunID) == "" || strings.TrimSpace(input.IdempotencyKey) == "" {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.TeamRunStop(ctx, input.RunID, input.Reason, input.IdempotencyKey)
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
		var input struct {
			RunID          string          `json:"run_id"`
			InteractionID  string          `json:"interaction_id"`
			Payload        json.RawMessage `json:"payload"`
			IdempotencyKey string          `json:"idempotency_key"`
		}
		if err := decodeArguments(call.Args, &input); err != nil || strings.TrimSpace(input.RunID) == "" ||
			strings.TrimSpace(input.InteractionID) == "" || strings.TrimSpace(input.IdempotencyKey) == "" || len(input.Payload) == 0 {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.HumanTaskComplete(ctx, weaveclient.HumanTaskCompleteRequest{
			RunID: input.RunID, InteractionID: input.InteractionID,
			Payload: input.Payload, IdempotencyKey: input.IdempotencyKey,
		})
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
			Limit  int    `json:"limit"`
			Offset int    `json:"offset"`
			RunID  string `json:"run_id"`
		}
		if err := decodeArguments(call.Args, &input); err != nil || input.Limit < 0 || input.Offset < 0 {
			return toolError(call.ID, "invalid_arguments"), nil
		}
		result, err := d.client.DeliverableListForRun(ctx, input.RunID, input.Limit, input.Offset)
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

func normalizeDispatchResult(clientRequestID string, document json.RawMessage) ([]byte, error) {
	result := map[string]any{"client_request_id": clientRequestID, "status": "queued"}
	var runResponse struct {
		Runs []struct {
			RunID       string `json:"run_id"`
			ParentRunID string `json:"parent_run_id"`
			Status      string `json:"status"`
			StopReason  string `json:"stop_reason"`
		} `json:"runs"`
	}
	if json.Unmarshal(document, &runResponse) == nil && len(runResponse.Runs) != 0 {
		run := runResponse.Runs[0]
		for _, candidate := range runResponse.Runs {
			if strings.TrimSpace(candidate.ParentRunID) == "" {
				run = candidate
				break
			}
		}
		result["run_id"] = run.RunID
		result["status"] = productDispatchStatus(run.Status)
		if run.StopReason != "" {
			result["stop_reason"] = run.StopReason
		}
		return json.Marshal(result)
	}
	var direct map[string]any
	if err := json.Unmarshal(document, &direct); err != nil {
		return nil, err
	}
	for _, key := range []string{"run_id", "workflow_id", "workflow_version", "task_id", "project_id", "conversation_id", "error_code"} {
		if value, ok := direct[key]; ok {
			result[key] = value
		}
	}
	if status, _ := direct["status"].(string); status != "" {
		result["status"] = productDispatchStatus(status)
	}
	return json.Marshal(result)
}

func productDispatchStatus(status string) string {
	switch strings.TrimSpace(status) {
	case "succeeded", "completed":
		return "completed"
	case "failed", "cancelled", "abandoned":
		return "failed"
	case "yielded", "waiting", "waiting_human":
		return "yielded"
	case "running", "executing", "in_progress":
		return "running"
	default:
		return "queued"
	}
}

type compactTeam struct {
	TeamID            string   `json:"team_id"`
	Name              string   `json:"name"`
	DisplayName       string   `json:"display_name"`
	Status            string   `json:"status"`
	Objective         string   `json:"objective"`
	PrimaryScenario   string   `json:"primary_scenario"`
	SuccessCriteria   string   `json:"success_criteria"`
	Responsibilities  []string `json:"responsibilities"`
	DefaultWorkflowID string   `json:"default_workflow_id,omitempty"`
	WorkflowAvailable bool     `json:"workflow_available"`
	Health            string   `json:"health,omitempty"`
}

func compactTeamList(document json.RawMessage) (json.RawMessage, error) {
	var items []struct {
		Team struct {
			ID                string `json:"id"`
			Name              string `json:"name"`
			DisplayName       string `json:"display_name"`
			Status            string `json:"status"`
			Objective         string `json:"objective"`
			PrimaryScenario   string `json:"primary_scenario"`
			SuccessCriteria   string `json:"success_criteria"`
			DefaultWorkflowID string `json:"default_workflow_id"`
		} `json:"team"`
		Workers []struct {
			Duty              string `json:"duty"`
			WhenToUse         string `json:"when_to_use"`
			ResultRequirement string `json:"result_requirement"`
		} `json:"workers"`
		Summary *struct {
			PublishedWorkflowCount int `json:"published_workflow_count"`
			Health                 *struct {
				Conclusion string `json:"conclusion"`
			} `json:"health"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(document, &items); err != nil {
		return nil, err
	}
	result := make([]compactTeam, 0, len(items))
	for _, item := range items {
		seen := map[string]bool{}
		responsibilities := make([]string, 0, len(item.Workers)*2)
		for _, worker := range item.Workers {
			for _, value := range []string{worker.Duty, worker.WhenToUse, worker.ResultRequirement} {
				value = strings.TrimSpace(value)
				if value != "" && !seen[value] {
					seen[value] = true
					responsibilities = append(responsibilities, value)
				}
			}
		}
		compact := compactTeam{
			TeamID: item.Team.ID, Name: item.Team.Name, DisplayName: item.Team.DisplayName, Status: item.Team.Status,
			Objective: item.Team.Objective, PrimaryScenario: item.Team.PrimaryScenario,
			SuccessCriteria: item.Team.SuccessCriteria, Responsibilities: responsibilities,
			DefaultWorkflowID: item.Team.DefaultWorkflowID,
		}
		if item.Summary != nil {
			compact.WorkflowAvailable = item.Team.DefaultWorkflowID != "" && item.Summary.PublishedWorkflowCount > 0
			if item.Summary.Health != nil {
				compact.Health = item.Summary.Health.Conclusion
			}
		}
		result = append(result, compact)
	}
	return json.Marshal(result)
}

type capabilityDefinitionDocument struct {
	CapabilityID string `json:"capability_id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	InputSchema  struct {
		Properties map[string]struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Type        string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	} `json:"input_schema"`
	OutputSchema struct {
		Properties map[string]struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Type        string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	} `json:"output_schema"`
	Roles []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"roles"`
	Steps []struct {
		Name   string `json:"name"`
		RoleID string `json:"role_id"`
		Kind   string `json:"kind"`
	} `json:"steps"`
}

func compactCapabilityPlan(document json.RawMessage) (json.RawMessage, error) {
	var response struct {
		Definition capabilityDefinitionDocument `json:"definition"`
	}
	if err := json.Unmarshal(document, &response); err != nil {
		return nil, err
	}
	return json.Marshal(capabilityBusinessView(response.Definition, nil, "draft"))
}

func compactCapabilityCatalog(document json.RawMessage) (json.RawMessage, error) {
	var response struct {
		Drafts   []capabilityDefinitionDocument `json:"drafts"`
		Versions []struct {
			CapabilityID string `json:"capability_id"`
			Revision     int64  `json:"revision"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(document, &response); err != nil {
		return nil, err
	}
	versions := make(map[string][]int64)
	for _, item := range response.Versions {
		versions[item.CapabilityID] = append(versions[item.CapabilityID], item.Revision)
	}
	items := make([]map[string]any, 0, len(response.Drafts))
	for _, definition := range response.Drafts {
		items = append(items, capabilityBusinessView(definition, versions[definition.CapabilityID], "draft"))
	}
	return json.Marshal(map[string]any{"capabilities": items})
}

func compactCapabilityPublication(document json.RawMessage) (json.RawMessage, error) {
	var response struct {
		CapabilityID string                       `json:"capability_id"`
		Revision     int64                        `json:"revision"`
		Definition   capabilityDefinitionDocument `json:"definition"`
	}
	if err := json.Unmarshal(document, &response); err != nil {
		return nil, err
	}
	view := capabilityBusinessView(response.Definition, []int64{response.Revision}, "published")
	view["capability_id"] = response.CapabilityID
	view["revision"] = response.Revision
	return json.Marshal(view)
}

func capabilityBusinessView(definition capabilityDefinitionDocument, versions []int64, status string) map[string]any {
	roleNames := make(map[string]string, len(definition.Roles))
	roles := make([]map[string]string, 0, len(definition.Roles))
	for _, role := range definition.Roles {
		roleNames[role.ID] = role.Name
		roles = append(roles, map[string]string{"name": role.Name, "responsibilities": role.Description})
	}
	flow := make([]map[string]string, 0, len(definition.Steps))
	for _, step := range definition.Steps {
		processing := "work"
		if step.Kind != "worker" {
			processing = "collect"
		}
		flow = append(flow, map[string]string{"name": step.Name, "responsible_role": roleNames[step.RoleID], "processing": processing})
	}
	fields := func(properties map[string]struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Type        string `json:"type"`
	}, required []string) []map[string]any {
		needed := make(map[string]bool, len(required))
		for _, name := range required {
			needed[name] = true
		}
		result := make([]map[string]any, 0, len(properties))
		for key, field := range properties {
			label := field.Title
			if label == "" {
				label = key
			}
			result = append(result, map[string]any{"name": label, "description": field.Description, "type": field.Type, "required": needed[key]})
		}
		sort.Slice(result, func(i, j int) bool { return fmt.Sprint(result[i]["name"]) < fmt.Sprint(result[j]["name"]) })
		return result
	}
	return map[string]any{
		"capability_id": definition.CapabilityID, "name": definition.Name, "purpose": definition.Description,
		"status": status, "published_versions": versions, "roles": roles, "flow": flow,
		"inputs":  fields(definition.InputSchema.Properties, definition.InputSchema.Required),
		"outputs": fields(definition.OutputSchema.Properties, definition.OutputSchema.Required),
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
		var apiErr *weaveclient.Error
		if errors.As(err, &apiErr) && len(apiErr.Problems) != 0 {
			encoded, _ := json.Marshal(struct {
				Error    string                `json:"error"`
				Problems []weaveclient.Problem `json:"problems"`
			}{Error: apiErr.Code, Problems: apiErr.Problems})
			return &contract.ToolResult{CallID: callID, Content: string(encoded), IsError: true}
		}
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
	"capability_list":     {Role: "admin", Scopes: []string{"admin"}},
	"capability_plan":     {Role: "admin", Scopes: []string{"admin"}},
	"capability_publish":  {Role: "admin", Scopes: []string{"admin"}},
	"capability_invoke":   {Role: "any", Scopes: []string{"admin"}},
	"capability_status":   {Role: "any", Scopes: []string{"admin"}},
	"capability_history":  {Role: "any", Scopes: []string{"admin"}},
	"capability_resume":   {Role: "any", Scopes: []string{"admin"}},
	"provider_list":       {Role: "any", Scopes: []string{"admin"}},
	"provider_add":        {Role: "admin", Scopes: []string{"admin"}},
	"apikey_create":       {Role: "admin", Scopes: []string{"admin"}},
	"runtime_create":      {Role: "any", Scopes: []string{"org"}},
	"team_list":           {Role: "any", Scopes: []string{"org"}},
	"team_status":         {Role: "any", Scopes: []string{"org"}},
	"usage_summary":       {Role: "any", Scopes: []string{"runs", "org"}},
	"team_dispatch":       {Role: "any", Scopes: []string{"org", "chat"}},
	"dispatch_status":     {Role: "any", Scopes: []string{"chat"}},
	"team_run_status":     {Role: "any", Scopes: []string{"runs"}},
	"team_run_activity":   {Role: "any", Scopes: []string{"runs"}},
	"team_run_stop":       {Role: "any", Scopes: []string{"runs"}},
	"human_task_list":     {Role: "workspace_member", Scopes: []string{"runs"}},
	"human_task_get":      {Role: "workspace_member", Scopes: []string{"runs"}},
	"human_task_complete": {Role: "workspace_member", Scopes: []string{"runs"}},
	"resume":              {Role: "any", Scopes: []string{"chat"}},
	"deliverable_list":    {Role: "any", Scopes: []string{"chat"}},
	"deliverable_get":     {Role: "any", Scopes: []string{"chat"}},
}

var toolDefinitions = []contract.ToolDef{
	{
		Name:        "capability_list",
		ReadOnly:    true,
		Description: "List reusable capabilities as business summaries with draft and published-version status. Use this before proposing a new capability so an existing one can be reused. Internal definitions are omitted. Requires capability management access.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	},
	{
		Name:        "capability_plan",
		Description: "Ask Weave to draft or revise a reusable capability from a business request. This creates a reviewable draft only; it does not publish or make the capability callable. Return the proposal in business language and request confirmation before capability_publish. Reuse the same idempotency key when retrying the same unresolved plan. Requires capability management access.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"business_request":{"type":"string","minLength":1,"maxLength":12000},"model":{"type":"string","minLength":1},"capability_id":{"type":"string","description":"Existing draft to revise; omit for a new capability."},"idempotency_key":{"type":"string","format":"uuid"}},"required":["business_request","model","idempotency_key"],"additionalProperties":false}`),
	},
	{
		Name:        "capability_publish",
		Description: "Publish one exact reviewed capability draft as an immutable revision. Use only after the user confirms the proposal, expected inputs, outputs and publication. Later changes require a new revision. Requires capability management access.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"capability_id":{"type":"string","minLength":1},"revision":{"type":"integer","minimum":1}},"required":["capability_id","revision"],"additionalProperties":false}`),
	},
	{Name: "capability_invoke", Description: "Run one published capability revision with business input. Returns a durable invocation that can be followed with capability_status.", InputSchema: json.RawMessage(`{"type":"object","properties":{"capability_id":{"type":"string"},"revision":{"type":"integer","minimum":1},"request_id":{"type":"string"},"input":{"type":"object"}},"required":["capability_id","revision","request_id","input"],"additionalProperties":false}`)},
	{Name: "capability_status", ReadOnly: true, Description: "Read one capability run, its execution history, quota usage and any pending human confirmation.", InputSchema: json.RawMessage(`{"type":"object","properties":{"invocation_id":{"type":"string"}},"required":["invocation_id"],"additionalProperties":false}`)},
	{Name: "capability_history", ReadOnly: true, Description: "List capability runs belonging to the current user.", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)},
	{Name: "capability_resume", Description: "Complete the pending human confirmation for a capability run and resume from its checkpoint.", InputSchema: json.RawMessage(`{"type":"object","properties":{"invocation_id":{"type":"string"},"step_id":{"type":"string"},"response":{}},"required":["invocation_id","step_id","response"],"additionalProperties":false}`)},
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
		Description: "List compact team-matching facts: purpose, scenario, responsibilities, success criteria, default workflow availability, and health. Internal prompts and worker context are omitted. Requires organization access. Errors: http_400, http_401, http_403.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","enum":["active","needs_repair","building","archived","all"]},"summary":{"type":"boolean"}},"additionalProperties":false}`),
	},
	{
		Name: "team_status", ReadOnly: true,
		Description: "Get one team's roster, lifecycle status, current default workflow health, and operational summary by exact team_id. Health is observational and never blocks dispatch. Requires organization access. Errors: http_401, http_403, http_404.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"team_id":{"type":"string"}},"required":["team_id"],"additionalProperties":false}`),
	},
	{
		Name: "usage_summary", ReadOnly: true,
		Description: "Summarize workspace run usage. Requires run access. Errors: http_400, http_401, http_403, http_404.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	},
	{
		Name:        "team_dispatch",
		Description: "Dispatch the exact input revision already registered by Workbench through its fixed published workflow. Workbench owns the original user input and its confirmation; never reconstruct task text or invent a revision. The revision fixes the team, workflow, task and client_request_id. Omit task to use the saved original; if supplied it must match byte for byte. An identical consumed revision replays its existing run, while old unconsumed revisions or changed facts are rejected. Leave wait=false and observe the returned run with team_run_activity unless a synchronous wait was explicitly requested. Errors: dispatch_input_required, dispatch_input_not_found, dispatch_input_mismatch, dispatch_input_superseded, team_not_found, team_not_active, workflow_not_published, http_401, http_403.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"team_id":{"type":"string"},"input_revision_id":{"type":"string","format":"uuid"},"task":{"type":"string"},"client_request_id":{"type":"string","format":"uuid"},"wait":{"type":"boolean","default":false}},"required":["team_id","input_revision_id"],"additionalProperties":false}`),
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
		Name: "team_run_activity", ReadOnly: true,
		Description: "Read bounded activity facts for one exact team run, including status, stages, runtime, and completeness. Requires run access.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string"}},"required":["run_id"],"additionalProperties":false}`),
	},
	{
		Name:        "team_run_stop",
		Description: "Request idempotent cancellation of one exact team run. Repeat the same idempotency_key to recover an unknown response. Requires run access.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string"},"reason":{"type":"string"},"idempotency_key":{"type":"string","minLength":1,"maxLength":256}},"required":["run_id","idempotency_key"],"additionalProperties":false}`),
	},
	{
		Name: "human_task_list", ReadOnly: true,
		Description: "List current workspace human tasks as paginated summaries without predecessor content. Requires current workspace membership. Returns task summaries, total, and next_cursor. Errors: http_400, http_401, http_403.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":100},"cursor":{"type":"string"}},"additionalProperties":false}`),
	},
	{
		Name: "human_task_get", ReadOnly: true,
		Description: "Read one human task and its predecessor outputs. Requires current workspace membership. path is an RFC 6901 JSON Pointer and returns the selected JSON value itself; use offset and limit to page selected strings, arrays, or objects. Errors: invalid_json_pointer, human_task_value_not_found, human_task_value_too_large, http_401, http_403, http_404.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string"},"path":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":10000}},"required":["run_id"],"additionalProperties":false}`),
	},
	{
		Name:        "human_task_complete",
		Description: "Complete one human task with the exact interaction_id returned for the current question, a payload matching its resume_schema, and a caller-supplied idempotency_key. Requires current workspace membership. Returns queued status and whether the completion was idempotent. Errors: http_400, http_401, http_403, http_404, http_409, http_422.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string"},"interaction_id":{"type":"string","minLength":1,"maxLength":256},"payload":{},"idempotency_key":{"type":"string","minLength":1,"maxLength":256}},"required":["run_id","interaction_id","payload","idempotency_key"],"additionalProperties":false}`),
	},
	{
		Name:        "resume",
		Description: "Resume a yielded run with human input. Requires chat access plus the yielded run_id and its lead agent name. Returns the server event stream as text. Errors: invalid_arguments, stream_required, http_401, http_403, http_404, http_409.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string"},"agent":{"type":"string"},"input":{"type":"object"}},"required":["run_id","agent","input"],"additionalProperties":false}`),
	},
	{
		Name: "deliverable_list", ReadOnly: true,
		Description: "List saved deliverables. Requires chat access. Set run_id to isolate one exact team run. A dispatch produces a deliverable only when an agent explicitly saves one. Returns deliverable records. Errors: http_401, http_403.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string"},"limit":{"type":"integer","minimum":0},"offset":{"type":"integer","minimum":0}},"additionalProperties":false}`),
	},
	{
		Name: "deliverable_get", ReadOnly: true,
		Description: "Get one saved deliverable by exact id. Requires chat access. For JSON content, path is an RFC 6901 pointer and returns the selected value itself; use offset and limit to page selected strings, arrays, or objects. Errors: final_deliverable_not_found, invalid_json_pointer, deliverable_value_not_found, deliverable_value_too_large, http_401, http_403.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"path":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":10000}},"required":["id"],"additionalProperties":false}`),
	},
}

var _ contract.ToolDispatcher = (*ToolDispatcher)(nil)
