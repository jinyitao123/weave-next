package teamcompiler

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jinyitao123/loom/contract"
)

type teamInteractionDispatcher struct {
	downstream contract.ToolDispatcher
	assembler  TeamInteractionAssembler
	catalog    TeamInteractionCatalog
}

func newTeamInteractionDispatcher(
	downstream contract.ToolDispatcher,
	assembler TeamInteractionAssembler,
	catalog TeamInteractionCatalog,
) contract.ToolDispatcher {
	return &teamInteractionDispatcher{
		downstream: downstream, assembler: assembler, catalog: catalog,
	}
}

func (d *teamInteractionDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	byName := make(map[string]contract.ToolDef)
	if !isNilValue(d.downstream) {
		tools, err := d.downstream.ListTools(ctx)
		if err != nil {
			return nil, err
		}
		for _, tool := range tools {
			switch tool.Name {
			case "delegate", "dispatch_parallel", "transfer_to":
				continue
			default:
				if _, duplicate := byName[tool.Name]; duplicate {
					return nil, ErrTeamInteractionRouteCollision
				}
				byName[tool.Name] = tool
			}
		}
	}
	if len(d.catalog.Consult) > 0 {
		byName["delegate"] = teamDelegateTool(d.catalog)
	}
	if len(d.catalog.Dispatch) > 0 {
		byName["dispatch_parallel"] = teamDispatchTool(d.catalog)
	}
	if len(d.catalog.Handoff) > 0 {
		byName["transfer_to"] = teamTransferTool(d.catalog)
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]contract.ToolDef, 0, len(names))
	for _, name := range names {
		result = append(result, byName[name])
	}
	return result, nil
}

func (d *teamInteractionDispatcher) Dispatch(
	ctx context.Context,
	call contract.ToolCall,
) (*contract.ToolResult, error) {
	switch call.Name {
	case "delegate":
		workerID, err := delegateWorkerID(call.Args)
		if err != nil {
			return nil, ErrTeamInteractionUnauthorized
		}
		if _, err := d.authorize(ctx, InteractionConsult, workerID, ""); err != nil {
			return nil, err
		}
	case "dispatch_parallel":
		workerIDs, err := dispatchWorkerIDs(call.Args)
		if err != nil || len(workerIDs) == 0 {
			return nil, ErrTeamInteractionUnauthorized
		}
		for _, workerID := range workerIDs {
			if _, err := d.authorize(
				ctx, InteractionDispatch, workerID, "",
			); err != nil {
				return nil, err
			}
		}
	case "transfer_to":
		routeKey, err := transferRouteKey(call.Args)
		if err != nil {
			return nil, ErrTeamHandoffTargetInvalid
		}
		route, exists := d.catalog.Handoff[routeKey]
		if !exists || route.RouteKey != routeKey {
			return nil, ErrTeamHandoffTargetInvalid
		}
		if _, err := d.authorize(
			ctx, InteractionHandoff, route.WorkerAgentID, routeKey,
		); err != nil {
			return nil, codedError(CodeTeamHandoffTargetInvalid, err)
		}
		return &contract.ToolResult{
			CallID:     call.ID,
			ToolName:   call.Name,
			Content:    "Transfer accepted.",
			StatePatch: map[string]any{"__delegate_to": routeKey},
			StopLoop:   true,
		}, nil
	}
	if isNilValue(d.downstream) {
		return nil, ErrTeamInteractionUnauthorized
	}
	return d.downstream.Dispatch(ctx, call)
}

func (d *teamInteractionDispatcher) authorize(
	ctx context.Context,
	kind InteractionKind,
	workerID string,
	routeKey string,
) (AuthorizedWorker, error) {
	return d.assembler.AuthorizeInteraction(
		ctx,
		d.catalog,
		InteractionAuthorizationRequest{
			WorkspaceID:   d.catalog.WorkspaceID,
			TeamID:        d.catalog.TeamID,
			RunID:         d.catalog.RunID,
			RunSnapshotID: d.catalog.RunSnapshotID,
			WorkerAgentID: workerID,
			RouteKey:      routeKey,
			Kind:          kind,
		},
	)
}

func teamDelegateTool(catalog TeamInteractionCatalog) contract.ToolDef {
	return contract.ToolDef{
		Name:        "delegate",
		Description: "Delegate to one frozen team worker by stable worker ID. Available workers:\n" + workerCatalog(catalog.Consult),
		InputSchema: toolInputSchema(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent":   map[string]any{"type": "string", "enum": sortedWorkerIDs(catalog.Consult)},
				"message": map[string]any{"type": "string"},
			},
			"required": []string{"agent", "message"},
		}),
	}
}

func teamDispatchTool(catalog TeamInteractionCatalog) contract.ToolDef {
	return contract.ToolDef{
		Name:        "dispatch_parallel",
		Description: "Dispatch frozen team work by stable worker ID. Available workers:\n" + workerCatalog(catalog.Dispatch),
		InputSchema: toolInputSchema(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tasks": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"worker":  map[string]any{"type": "string", "enum": sortedWorkerIDs(catalog.Dispatch)},
							"message": map[string]any{"type": "string"},
						},
						"required": []string{"worker", "message"},
					},
				},
			},
			"required": []string{"tasks"},
		}),
	}
}

func teamTransferTool(catalog TeamInteractionCatalog) contract.ToolDef {
	var routes strings.Builder
	for _, route := range sortedHandoffRoutes(catalog) {
		fmt.Fprintf(&routes, "\n- %s: %s", route.RouteKey, route.DisplayName)
	}
	return contract.ToolDef{
		Name:        "transfer_to",
		Description: "Transfer final reply authority to one frozen team route. Available routes:" + routes.String(),
		InputSchema: json.RawMessage(`{"type":"object","properties":{"agent":{"type":"string"},"message":{"type":"string"}},"required":["agent"]}`),
		ReadOnly:    false,
	}
}

func workerCatalog(directory map[string]AuthorizedWorker) string {
	var result strings.Builder
	for _, id := range sortedWorkerIDs(directory) {
		worker := directory[id]
		fmt.Fprintf(&result, "- %s: %s\n", id, worker.Duty)
	}
	return result.String()
}

func sortedWorkerIDs(directory map[string]AuthorizedWorker) []string {
	ids := make([]string, 0, len(directory))
	for id := range directory {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func toolInputSchema(schema map[string]any) json.RawMessage {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return encoded
}

func delegateWorkerID(raw string) (string, error) {
	var input struct {
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal([]byte(raw), &input); err != nil || input.Agent == "" {
		return "", ErrTeamInteractionUnauthorized
	}
	return input.Agent, nil
}

func dispatchWorkerIDs(raw string) ([]string, error) {
	var input struct {
		Tasks []struct {
			Worker string `json:"worker"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return nil, err
	}
	result := make([]string, 0, len(input.Tasks))
	for _, task := range input.Tasks {
		if task.Worker == "" {
			return nil, ErrTeamInteractionUnauthorized
		}
		result = append(result, task.Worker)
	}
	return result, nil
}

func transferRouteKey(raw string) (string, error) {
	var input struct {
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal([]byte(raw), &input); err != nil || input.Agent == "" {
		return "", ErrTeamHandoffTargetInvalid
	}
	return input.Agent, nil
}

var _ contract.ToolDispatcher = (*teamInteractionDispatcher)(nil)
