package teamforge

// Team write tools (plan §10.2.3 "Team 与 Roster"). tf_create_team creates
// one active team aggregate through the platform team-creation path
// (org.Store.CreateActiveTeam: team + lead relation + enabled initial
// workers atomically); tf_set_roster submits a complete roster CAS command
// through registry.AgentRegistry.ApplyTeamRosterCommand — the platform's
// only roster writer — never a raw row write. Both tools are receipt-gated
// on AssetRef{Kind: "team"} and audited through the shared write skeleton;
// free-collaboration dispatch rules ride along on both calls.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/build/teambuild"
)

const (
	// ToolCreateTeam creates one active team with its initial roster.
	ToolCreateTeam = "tf_create_team"
	// ToolSetRoster submits one complete roster CAS command (and optional
	// dispatch rules) for an existing team.
	ToolSetRoster = "tf_set_roster"
)

// TeamWriteToolsDispatcher exposes the two team write tools. The workspace,
// calling agent, and BuildAuthorizationReceipt are fixed at construction
// time; every call is receipt-gated on AssetRef{Kind: "team"}.
type TeamWriteToolsDispatcher struct {
	gate              *WriteGate
	deps              Deps
	writeDeps         WriteDeps
	createTeamEnabled bool
	tools             []contract.ToolDef
}

// NewTeamWriteTools creates the team write dispatcher for one workspace, one
// calling agent, and one build authorization receipt. deps supplies the
// team read path (used to resolve names/IDs and CAS tokens); writeDeps
// carries the team creator, roster command writer, and dispatch-rule writer.
func NewTeamWriteTools(
	workspaceID, agentName string,
	receipt teambuild.BuildAuthorizationReceipt,
	validator ReceiptValidator,
	audit AuditRecorder,
	deps Deps,
	writeDeps WriteDeps,
) *TeamWriteToolsDispatcher {
	return NewTeamWriteToolsMode(
		workspaceID, agentName, receipt, validator, audit, deps, writeDeps, true,
	)
}

// NewTeamWriteToolsMode is the mode-aware constructor (ticket T15C): when
// allowCreateTeam is false the dispatcher exposes only tf_set_roster, so an
// optimize build run never sees tf_create_team. The roster command remains
// fully receipt-gated and audited.
func NewTeamWriteToolsMode(
	workspaceID, agentName string,
	receipt teambuild.BuildAuthorizationReceipt,
	validator ReceiptValidator,
	audit AuditRecorder,
	deps Deps,
	writeDeps WriteDeps,
	allowCreateTeam bool,
) *TeamWriteToolsDispatcher {
	d := &TeamWriteToolsDispatcher{
		gate:              newWriteGate(workspaceID, agentName, receipt, validator, audit, writeDeps),
		deps:              deps,
		writeDeps:         writeDeps,
		createTeamEnabled: allowCreateTeam,
	}
	d.tools = make([]contract.ToolDef, 0, 2)
	if d.createTeamEnabled {
		d.tools = append(d.tools, contract.ToolDef{
			Name: ToolCreateTeam,
			Description: "创建活动团队：原子地创建 Team + lead 关系 + 启用初始 roster（org.CreateActiveTeam），可选写入派工规则。" +
				"Create an active team aggregate (team + lead relation + enabled initial roster) and optionally set dispatch rules.",
			InputSchema: createTeamSchema,
			ReadOnly:    false,
		})
	}
	d.tools = append(d.tools, contract.ToolDef{
		Name: ToolSetRoster,
		Description: "提交完整 Roster CAS command（AgentRegistry.ApplyTeamRosterCommand，基于团队 updated_at 的乐观锁 + 幂等键），" +
			"可选同时更新派工规则；绝不绕过平台 roster 写路径。幂等键默认 team-forge:<build_run_id>:round:<round_no>:roster，" +
			"每轮独立。Submit one complete roster CAS command (idempotent, CAS on team updated_at) and optionally dispatch rules; " +
			"the roster never bypasses the platform command writer.",
		InputSchema: setRosterSchema,
		ReadOnly:    false,
	})
	return d
}

// ListTools returns the two team write tools.
func (d *TeamWriteToolsDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	return d.tools, nil
}

// Dispatch gates the call on the receipt and call envelope, executes the
// matching team tool, and records the outcome in the audit trail. Every
// failure is an IsError result, never a Go error.
func (d *TeamWriteToolsDispatcher) Dispatch(
	ctx context.Context,
	call contract.ToolCall,
) (result *contract.ToolResult, err error) {
	defer func() {
		d.gate.recordAudit(ctx, call, result, err)
	}()

	if !d.toolRegistered(call.Name) {
		if call.Name == ToolCreateTeam && !d.createTeamEnabled {
			return toolError(call.ID, "tf_create_team is not available: optimize build runs cannot create a new team"), nil
		}
		return toolError(call.ID, fmt.Sprintf("unknown tool %q", call.Name)), nil
	}

	switch call.Name {
	case ToolCreateTeam:
		return d.createTeam(ctx, call)
	case ToolSetRoster:
		return d.setRoster(ctx, call)
	default:
		return toolError(call.ID, fmt.Sprintf("unknown tool %q", call.Name)), nil
	}
}

func (d *TeamWriteToolsDispatcher) toolRegistered(name string) bool {
	for _, tool := range d.tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

var _ contract.ToolDispatcher = (*TeamWriteToolsDispatcher)(nil)

// --- call shapes ---

type teamWorkerInput struct {
	WorkerAgentID      string   `json:"worker_agent_id"`
	Duty               string   `json:"duty"`
	WhenToUse          string   `json:"when_to_use"`
	ContextInstruction string   `json:"context_instruction"`
	AllowedKinds       []string `json:"allowed_kinds"`
	DefaultKind        string   `json:"default_kind"`
	ResultRequirement  string   `json:"result_requirement"`
}

type dispatchRulesInput struct {
	LegTimeoutSec    int `json:"leg_timeout_sec"`
	GroupDeadlineSec int `json:"group_deadline_sec"`
	Quorum           int `json:"quorum"`
}

type createTeamInput struct {
	BuildRunID      string              `json:"build_run_id"`
	Name            string              `json:"name"`
	Objective       string              `json:"objective"`
	PrimaryScenario string              `json:"primary_scenario"`
	SuccessCriteria string              `json:"success_criteria"`
	LeadAvatarID    string              `json:"lead_avatar_id"`
	Workers         []teamWorkerInput   `json:"workers"`
	DispatchRules   *dispatchRulesInput `json:"dispatch_rules,omitempty"`
}

func (d *TeamWriteToolsDispatcher) createTeam(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	var input createTeamInput
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return toolError(call.ID, "invalid input: "+err.Error()), nil
	}
	runID, err := d.gate.resolveBuildRunID(call.Args)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	input.BuildRunID = runID
	if strings.TrimSpace(input.Name) == "" {
		return toolError(call.ID, "name is required"), nil
	}
	input.Name = strings.TrimSpace(input.Name)
	if err := d.gate.authorizeRef(ctx, teambuild.AssetRef{Kind: "team", Name: input.Name}); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if err := d.gate.requireBuildRunContext(call.Args); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if d.writeDeps.Teams == nil {
		return toolError(call.ID, "team creation is unavailable"), nil
	}

	createInput := org.CreateActiveTeamInput{
		Name:            input.Name,
		Objective:       input.Objective,
		PrimaryScenario: input.PrimaryScenario,
		SuccessCriteria: input.SuccessCriteria,
		LeadAvatarID:    strings.TrimSpace(input.LeadAvatarID),
		DesiredStatus:   "building",
		Workers:         make([]org.InitialTeamWorker, len(input.Workers)),
	}
	for i, worker := range input.Workers {
		createInput.Workers[i] = org.InitialTeamWorker{
			WorkerAgentID:      strings.TrimSpace(worker.WorkerAgentID),
			Duty:               worker.Duty,
			WhenToUse:          worker.WhenToUse,
			ContextInstruction: worker.ContextInstruction,
			AllowedKinds:       worker.AllowedKinds,
			DefaultKind:        worker.DefaultKind,
			ResultRequirement:  worker.ResultRequirement,
		}
	}
	result, err := d.writeDeps.Teams.CreateActiveTeam(ctx, d.gate.workspaceID, createInput)
	if err != nil {
		return toolError(call.ID, fmt.Sprintf("create team: %v", err)), nil
	}

	var rules *org.TeamDispatchRules
	if input.DispatchRules != nil {
		if d.writeDeps.DispatchRules == nil {
			return toolError(call.ID, "dispatch-rule write is unavailable"), nil
		}
		rules = &org.TeamDispatchRules{
			TeamID:           result.Team.ID,
			LegTimeoutSec:    input.DispatchRules.LegTimeoutSec,
			GroupDeadlineSec: input.DispatchRules.GroupDeadlineSec,
			Quorum:           input.DispatchRules.Quorum,
		}
		if err := d.writeDeps.DispatchRules.PutTeamDispatchRules(ctx, d.gate.workspaceID, *rules); err != nil {
			return toolError(call.ID, fmt.Sprintf("set dispatch rules: %v", err)), nil
		}
	}
	return toolJSON(call.ID, map[string]any{
		"build_run_id":   input.BuildRunID,
		"team":           result.Team,
		"workers":        result.Workers,
		"dispatch_rules": rules,
	})
}

type setRosterInput struct {
	BuildRunID     string              `json:"build_run_id"`
	RoundNo        int                 `json:"round_no"`
	TeamID         string              `json:"team_id"`
	IdempotencyKey string              `json:"idempotency_key,omitempty"`
	LeadAgentID    string              `json:"lead_agent_id"`
	Workers        []teamWorkerInput   `json:"workers"`
	DispatchRules  *dispatchRulesInput `json:"dispatch_rules,omitempty"`
}

func (d *TeamWriteToolsDispatcher) setRoster(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	var input setRosterInput
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return toolError(call.ID, "invalid input: "+err.Error()), nil
	}
	runID, err := d.gate.resolveBuildRunID(call.Args)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	input.BuildRunID = runID
	if input.RoundNo < 1 {
		return toolError(call.ID, "round_no is required and must be positive"), nil
	}
	if strings.TrimSpace(input.TeamID) == "" {
		return toolError(call.ID, "team_id is required"), nil
	}
	input.TeamID = strings.TrimSpace(input.TeamID)
	if strings.TrimSpace(input.LeadAgentID) == "" {
		return toolError(call.ID, "lead_agent_id is required"), nil
	}
	input.LeadAgentID = strings.TrimSpace(input.LeadAgentID)
	if err := d.gate.requireBuildRunContext(call.Args); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if d.deps.Teams == nil {
		return toolError(call.ID, "team read is unavailable"), nil
	}
	teams, err := d.deps.Teams.ListTeams(ctx, d.gate.workspaceID)
	if err != nil {
		return toolError(call.ID, fmt.Sprintf("list teams: %v", err)), nil
	}
	team, found := findTeamByID(teams, input.TeamID)
	if !found {
		return toolError(call.ID, "team not found"), nil
	}
	// The receipt scope matches create-mode assets by name prefix and
	// optimize-mode assets by explicit ref; pass both identities when known.
	if err := d.gate.authorizeRef(ctx, teambuild.AssetRef{Kind: "team", ID: team.ID, Name: team.Name}); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if d.writeDeps.Roster == nil {
		return toolError(call.ID, "roster command writer is unavailable"), nil
	}

	idempotencyKey := strings.TrimSpace(input.IdempotencyKey)
	if idempotencyKey == "" {
		// The roster key is per round (ticket T15C): a later round rewrites
		// the complete roster with a different key, so the round-1 command
		// never conflicts with round 2's replay under the same build run.
		idempotencyKey = fmt.Sprintf(
			"team-forge:%s:round:%d:roster",
			input.BuildRunID,
			input.RoundNo,
		)
	}
	command := registry.TeamRosterCommand{
		WorkspaceID:       d.gate.workspaceID,
		TeamID:            team.ID,
		IdempotencyKey:    idempotencyKey,
		ExpectedUpdatedAt: team.UpdatedAt,
		DesiredTeamStatus: "active",
		LeadAgentID:       input.LeadAgentID,
		Workers:           make([]registry.TeamRosterWorkerInput, len(input.Workers)),
		OperatorID:        d.gate.agent,
		Reason:            "team forge build run " + input.BuildRunID,
	}
	for i, worker := range input.Workers {
		command.Workers[i] = registry.TeamRosterWorkerInput{
			WorkerAgentID:      strings.TrimSpace(worker.WorkerAgentID),
			Duty:               worker.Duty,
			WhenToUse:          worker.WhenToUse,
			ContextInstruction: worker.ContextInstruction,
			AllowedKinds:       worker.AllowedKinds,
			DefaultKind:        worker.DefaultKind,
			ResultRequirement:  worker.ResultRequirement,
			Enabled:            true,
		}
	}
	result, err := d.writeDeps.Roster.ApplyTeamRosterCommand(ctx, command)
	if err != nil {
		return toolError(call.ID, fmt.Sprintf("apply team roster command: %v", err)), nil
	}

	var rules *org.TeamDispatchRules
	if input.DispatchRules != nil {
		if d.writeDeps.DispatchRules == nil {
			return toolError(call.ID, "dispatch-rule write is unavailable"), nil
		}
		rules = &org.TeamDispatchRules{
			TeamID:           team.ID,
			LegTimeoutSec:    input.DispatchRules.LegTimeoutSec,
			GroupDeadlineSec: input.DispatchRules.GroupDeadlineSec,
			Quorum:           input.DispatchRules.Quorum,
		}
		if err := d.writeDeps.DispatchRules.PutTeamDispatchRules(ctx, d.gate.workspaceID, *rules); err != nil {
			return toolError(call.ID, fmt.Sprintf("set dispatch rules: %v", err)), nil
		}
	}
	return toolJSON(call.ID, map[string]any{
		"build_run_id":    input.BuildRunID,
		"team_id":         team.ID,
		"team_name":       team.Name,
		"idempotency_key": idempotencyKey,
		"roster":          result,
		"dispatch_rules":  rules,
	})
}

// --- tool schemas ---

var teamWorkerSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"worker_agent_id": {"type": "string", "description": "Stable agent ID returned by tf_list_agents"},
		"duty": {"type": "string", "description": "Roster duty label"},
		"when_to_use": {"type": "string", "description": "When this worker should be dispatched"},
		"context_instruction": {"type": "string"},
		"allowed_kinds": {"type": "array", "items": {"type": "string", "enum": ["consult", "dispatch", "handoff"]}},
		"default_kind": {"type": "string", "enum": ["consult", "dispatch", "handoff"]},
		"result_requirement": {"type": "string"}
	},
	"required": ["worker_agent_id", "duty", "when_to_use", "allowed_kinds", "default_kind"],
	"additionalProperties": false
}`)

var dispatchRulesSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"leg_timeout_sec": {"type": "integer"},
		"group_deadline_sec": {"type": "integer"},
		"quorum": {"type": "integer"}
	},
	"additionalProperties": false
}`)

var createTeamSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"build_run_id": {"type": "string", "description": "可选；省略时自动绑定当前授权 run"},
		"name": {"type": "string", "description": "Team name; must sit inside the build authorization scope"},
		"objective": {"type": "string"},
		"primary_scenario": {"type": "string"},
		"success_criteria": {"type": "string"},
		"lead_avatar_id": {"type": "string", "description": "Stable agent ID of an avatar in this workspace"},
		"workers": {"type": "array", "items": ` + string(teamWorkerSchema) + `},
		"dispatch_rules": ` + string(dispatchRulesSchema) + `
	},
	"required": ["name", "lead_avatar_id", "workers"],
	"additionalProperties": false
}`)

var setRosterSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"build_run_id": {"type": "string", "description": "可选；省略时自动绑定当前授权 run"},
		"round_no": {"type": "integer", "description": "Current build round (1-based); part of the default roster idempotency key"},
		"team_id": {"type": "string", "description": "Team ID from tf_create_team"},
		"idempotency_key": {"type": "string", "description": "Optional roster command idempotency key (defaults to team-forge:<build_run_id>:round:<round_no>:roster)"},
		"lead_agent_id": {"type": "string", "description": "Stable lead avatar agent ID"},
		"workers": {"type": "array", "items": ` + string(teamWorkerSchema) + `},
		"dispatch_rules": ` + string(dispatchRulesSchema) + `
	},
	"required": ["round_no", "team_id", "lead_agent_id", "workers"],
	"additionalProperties": false
}`)
