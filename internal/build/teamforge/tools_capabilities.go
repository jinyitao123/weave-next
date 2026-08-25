package teamforge

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jinyitao123/loom/contract"
)

const skillNamespacePrefix = "skill:"

type skillSummaryJSON struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Description  string    `json:"description,omitempty"`
	AlwaysActive bool      `json:"always_active,omitempty"`
	Category     string    `json:"category,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type mcpserverSummaryJSON struct {
	ID                 string     `json:"id"`
	Slug               string     `json:"slug"`
	DisplayName        string     `json:"display_name"`
	Transport          string     `json:"transport"`
	Enabled            bool       `json:"enabled"`
	Status             string     `json:"status"`
	FunctionalRevision int64      `json:"functional_revision"`
	ProtocolVersion    string     `json:"protocol_version,omitempty"`
	LastError          string     `json:"last_error,omitempty"`
	ToolCount          int        `json:"tool_count"`
	AgentCount         int        `json:"agent_count"`
	LastProbedAt       *time.Time `json:"last_probed_at,omitempty"`
}

type providerSummaryJSON struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	BaseURL          string   `json:"base_url"`
	Models           []string `json:"models"`
	JSONObjectMode   bool     `json:"json_object_mode,omitempty"`
	Enabled          bool     `json:"enabled"`
	LatestRevision   int64    `json:"latest_revision"`
	SourceKind       string   `json:"source_kind"`
	SourceProviderID *string  `json:"source_provider_id,omitempty"`
}

type runtimeSummaryJSON struct {
	ID                 string     `json:"id"`
	Name               string     `json:"name"`
	Engines            []string   `json:"engines"`
	Enabled            bool       `json:"enabled"`
	Online             bool       `json:"online"`
	FunctionalRevision int64      `json:"functional_revision"`
	LastHeartbeatAt    *time.Time `json:"last_heartbeat_at,omitempty"`
}

type agentGraphPolicyJSON struct {
	Role        string `json:"role"`
	EngineClass string `json:"engine_class"`
	CustomGraph string `json:"custom_graph"`
	Execution   string `json:"execution"`
	L1Treatment string `json:"l1_treatment"`
}

type agentModelPolicyJSON struct {
	EngineClass      string `json:"engine_class"`
	RequiredProvider string `json:"required_provider"`
	SelectionRule    string `json:"selection_rule"`
}

type listCapabilitiesJSON struct {
	Skills           []skillSummaryJSON     `json:"skills"`
	MCPServers       []mcpserverSummaryJSON `json:"mcp_servers"`
	Providers        []providerSummaryJSON  `json:"providers"`
	Runtimes         []runtimeSummaryJSON   `json:"runtimes"`
	AgentGraphPolicy []agentGraphPolicyJSON `json:"agent_graph_policy"`
	AgentModelPolicy []agentModelPolicyJSON `json:"agent_model_policy"`
}

// listCapabilities implements tf_list_capabilities using each capability
// package's existing list read path. Skills use the same legacy skill
// namespace read as the platform skills API; MCP servers, providers, and
// runtimes use their stores' list methods.
func (d *ReadToolsDispatcher) listCapabilities(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	out := listCapabilitiesJSON{
		Skills:     []skillSummaryJSON{},
		MCPServers: []mcpserverSummaryJSON{},
		Providers:  []providerSummaryJSON{},
		Runtimes:   []runtimeSummaryJSON{},
		AgentGraphPolicy: []agentGraphPolicyJSON{
			{
				Role: "avatar", EngineClass: "loom", CustomGraph: "forbidden",
				Execution: "standard_tool_loop", L1Treatment: "team_workflow_lead_nodes",
			},
			{
				Role: "worker", EngineClass: "loom", CustomGraph: "supported",
				Execution: "declarative_or_standard", L1Treatment: "declarative_graph_required",
			},
			{
				Role: "worker", EngineClass: "cli", CustomGraph: "forbidden",
				Execution: "runtime_cli", L1Treatment: "explicit_waiver_required",
			},
		},
		AgentModelPolicy: []agentModelPolicyJSON{
			{
				EngineClass: "loom", RequiredProvider: "resolvable_provider_or_runtime_default",
				SelectionRule: "select an enabled provider model when available; otherwise leave model_ref empty so runtime-backed Loom inference uses the assigned authenticated runtime default",
			},
			{
				EngineClass: "codex_or_opencode", RequiredProvider: "system/openai_or_authenticated_runtime_default",
				SelectionRule: "select a model listed by system/openai when available; otherwise leave model_ref empty and use the assigned runtime's authenticated CLI default",
			},
			{
				EngineClass: "claude", RequiredProvider: "system/anthropic_or_authenticated_runtime_default",
				SelectionRule: "select a model listed by system/anthropic when available; otherwise leave model_ref empty and use the assigned runtime's authenticated CLI default",
			},
		},
	}

	if d.deps.Skills != nil {
		skills, err := d.listSkills(ctx)
		if err != nil {
			return toolError(call.ID, err.Error()), nil
		}
		out.Skills = skills
	}

	if d.deps.MCPs != nil {
		servers, err := d.deps.MCPs.ListMetadata(ctx, d.workspaceID)
		if err != nil {
			return toolError(call.ID, err.Error()), nil
		}
		for _, server := range servers {
			out.MCPServers = append(out.MCPServers, mcpserverSummaryJSON{
				ID:                 server.ID,
				Slug:               server.Slug,
				DisplayName:        server.DisplayName,
				Transport:          string(server.Transport),
				Enabled:            server.Enabled,
				Status:             server.Status,
				FunctionalRevision: server.FunctionalRevision,
				ProtocolVersion:    server.ProtocolVersion,
				LastError:          server.LastError,
				ToolCount:          server.ToolCount,
				AgentCount:         server.AgentCount,
				LastProbedAt:       server.LastProbedAt,
			})
		}
	}

	if d.deps.Providers != nil {
		providers, err := d.deps.Providers.ListMetadata(ctx, d.workspaceID)
		if err != nil {
			return toolError(call.ID, err.Error()), nil
		}
		for _, provider := range providers {
			out.Providers = append(out.Providers, providerSummaryJSON{
				ID:               provider.ID,
				Name:             provider.Name,
				BaseURL:          provider.BaseURL,
				Models:           provider.Models,
				JSONObjectMode:   provider.JSONObjectMode,
				Enabled:          provider.Enabled,
				LatestRevision:   provider.LatestRevision,
				SourceKind:       provider.SourceKind,
				SourceProviderID: provider.SourceProviderID,
			})
		}
	}

	if d.deps.Runtimes != nil {
		runtimes, err := d.deps.Runtimes.List(ctx, d.workspaceID)
		if err != nil {
			return toolError(call.ID, err.Error()), nil
		}
		for _, runtime := range runtimes {
			out.Runtimes = append(out.Runtimes, runtimeSummaryJSON{
				ID:                 runtime.ID,
				Name:               runtime.Name,
				Engines:            runtime.Engines,
				Enabled:            runtime.Enabled,
				Online:             runtime.Online,
				FunctionalRevision: runtime.FunctionalRevision,
				LastHeartbeatAt:    runtime.LastHeartbeatAt,
			})
		}
	}
	return toolJSON(call.ID, out)
}

// listSkills reads the legacy skill namespace exactly like the platform
// skills list API: List keys under skill:<workspace>, then Get each record.
func (d *ReadToolsDispatcher) listSkills(ctx context.Context) ([]skillSummaryJSON, error) {
	namespace := skillNamespacePrefix + d.workspaceID
	keys, err := d.deps.Skills.List(ctx, namespace, "")
	if err != nil {
		return nil, err
	}
	skills := make([]skillSummaryJSON, 0, len(keys))
	for _, key := range keys {
		data, err := d.deps.Skills.Get(ctx, namespace, key)
		if err != nil {
			continue
		}
		var skill skillSummaryJSON
		if json.Unmarshal(data, &skill) == nil {
			skills = append(skills, skill)
		}
	}
	return skills, nil
}
