package teamforge

// Assembly decisions for meta-team conversation wiring. Compiler-v2 roles are
// always read-only while a TeamBuildRun is live; the platform ChangeSet
// executor owns formal writes. The explicitly named legacy decision function
// retains the former direct-write surface for compatibility callers.

import (
	"strings"

	"github.com/jinyitao123/weave/internal/build/teambuild"
)

// Guide agent names are build protocol identities, not ownership of their
// workspace AgentRecords. The application metateam package assembles those
// records; build code must not import upward to discover the protocol names.
const (
	TeamArchitectAgentName  = "__team_architect"
	ConfigEngineerAgentName = teambuild.ConfigEngineerResourceName
	GraphDesignerAgentName  = teambuild.GraphDesignerResourceName
	EvalDebuggerAgentName   = "__eval_debugger"
)

// ToolSet names the teamforge dispatcher groups one agent may receive for one
// build-run state. Each group maps to exactly one dispatcher:
//   - Read           → NewReadTools (the seven tf_* read tools)
//   - AgentWrite     → NewWriteTools (tf_create_agent, tf_update_agent)
//   - TeamWrite      → NewTeamWriteTools (tf_create_team, tf_set_roster)
//   - TeamRosterWrite → NewTeamWriteToolsMode with create disabled (tf_set_roster only)
//   - GraphWrite     → NewGraphWriteTools (tf_graph_*)
//   - WorkflowWrite  → NewWorkflowWriteTools (tf_wf_*)
type ToolSet struct {
	Read            bool
	AgentWrite      bool
	TeamWrite       bool
	TeamRosterWrite bool
	GraphWrite      bool
	WorkflowWrite   bool
}

// IsZero reports whether no teamforge dispatcher group is selected.
func (set ToolSet) IsZero() bool {
	return !set.Read && !set.AgentWrite && !set.TeamWrite && !set.TeamRosterWrite &&
		!set.GraphWrite && !set.WorkflowWrite
}

// HasWrites reports whether the decision grants at least one write group.
func (set ToolSet) HasWrites() bool {
	return set.AgentWrite || set.TeamWrite || set.TeamRosterWrite ||
		set.GraphWrite || set.WorkflowWrite
}

// DecideToolSet returns the compiler-v2 teamforge dispatcher groups one agent
// receives in one build-run state. Non-meta-team agents always get the zero
// set, so ordinary PlatformTools behavior is unchanged.
func DecideToolSet(agentName, runStatus string) ToolSet {
	return DecideToolSetForMode(agentName, runStatus, teambuild.ModeCreate)
}

// DecideToolSetForMode keeps every compiler-v2 meta-team role read-only during
// a live run. mode is retained in the API because callers already assemble it
// from the frozen brief and legacy direct wiring still needs that dimension.
func DecideToolSetForMode(agentName, runStatus, mode string) ToolSet {
	_ = mode
	if !isMetaTeamAgent(agentName) {
		return ToolSet{}
	}
	switch runStatus {
	case teambuild.StatusPlanning, teambuild.StatusAuthorized,
		teambuild.StatusRoundRunning, teambuild.StatusPublishing:
		return ToolSet{Read: true}
	default:
		return ToolSet{}
	}
}

// DecideLegacyDirectToolSetForMode preserves the pre-compiler-v2 direct-write
// surface behind an explicit compatibility name. New meta-team wiring must use
// DecideToolSetForMode so construction stays in the ChangeSet executor.
func DecideLegacyDirectToolSetForMode(agentName, runStatus, mode string) ToolSet {
	var writes ToolSet
	switch agentName {
	case ConfigEngineerAgentName:
		// 配置工程师 holds the agent assembly and team/roster write sets
		// (plan §10.2.1 and §10.2.3 team entry).
		writes = ToolSet{AgentWrite: true}
		if mode == teambuild.ModeOptimize {
			// Optimize targets an existing team: creating a new team is never
			// part of the loop, so tf_create_team stays unexposed and only the
			// complete-roster command remains.
			writes.TeamRosterWrite = true
		} else {
			writes.TeamWrite = true
		}
	case GraphDesignerAgentName:
		// 图设计师 holds the employee internal-graph and team-workflow write
		// sets (plan §10.2.2 and §10.2.3 workflow entry).
		writes = ToolSet{GraphWrite: true, WorkflowWrite: true}
	case TeamArchitectAgentName, EvalDebuggerAgentName:
		// 团队架构师 and 评测调试师 never hold configuration write tools
		// (plan §4.1/§4.4); they stay read-only in every phase.
		writes = ToolSet{}
	default:
		return ToolSet{}
	}

	switch runStatus {
	case teambuild.StatusPlanning:
		// Discovery phase: every role receives the read-only environment set.
		return ToolSet{Read: true}
	case teambuild.StatusAuthorized, teambuild.StatusRoundRunning, teambuild.StatusPublishing:
		writes.Read = true
		return writes
	default:
		// Terminal (passed/blocked/cancelled) or unknown status: the task's
		// permission window is closed, so no teamforge tools are attached.
		return ToolSet{}
	}
}

func isMetaTeamAgent(agentName string) bool {
	switch agentName {
	case TeamArchitectAgentName, ConfigEngineerAgentName,
		GraphDesignerAgentName, EvalDebuggerAgentName:
		return true
	default:
		return false
	}
}

// TargetedBlueprintPatchContext is the typed, evaluation-produced authority
// for a graph expert to revise an existing custom_spec. Free-form user text is
// deliberately absent: it cannot authorize graph planning. This pure contract
// is not connected to production in Task 1; Task 6 may pass it from typed
// evaluator output. API chat input must never construct it.
type TargetedBlueprintPatchContext struct {
	DiagnosisType string
	CustomSpecRef string
	TargetPaths   []string
}

// AssembleMetaPlanningState derives the graph expert's trusted routing state
// from the frozen run and an optional typed patch. A planning blueprint run
// opens only the bounded declarative_v1 route; the architect's delegation
// carries topology intent but cannot widen the server-side spec validator or
// authorize custom/extension behavior. The returned target paths own their
// backing storage so later caller mutation cannot change assembled state.
func AssembleMetaPlanningState(
	run teambuild.TeamBuildRun,
	patch *TargetedBlueprintPatchContext,
) map[string]any {
	state := map[string]any{
		"meta_graph_planning_allowed":     false,
		"meta_graph_planning_route":       "",
		"meta_graph_declarative_planning": false,
		"meta_graph_template_match":       false,
		"meta_graph_block_reason":         "",
		"meta_workflow_build_mode":        "",
		"meta_template_gap":               "",
		"meta_template_gap_confirmed":     false,
		"meta_brief_hash":                 run.BriefHash,
		"meta_confirmed_by":               run.ConfirmedBy,
		"meta_targeted_patch_paths":       []string{},
		"meta_targeted_custom_spec_ref":   "",
	}
	if strings.TrimSpace(run.BriefHash) == "" {
		state["meta_graph_block_reason"] = "missing_brief_hash"
		return state
	}

	mode := run.Brief.EffectiveWorkflowBuildMode()
	gap := strings.TrimSpace(run.Brief.TemplateGap)
	confirmed := strings.TrimSpace(run.ConfirmedBy) != ""
	planning := run.Status == teambuild.StatusPlanning
	state["meta_workflow_build_mode"] = mode
	state["meta_template_gap"] = gap

	if patch != nil {
		state["meta_targeted_custom_spec_ref"] = strings.TrimSpace(patch.CustomSpecRef)

		if patch.DiagnosisType == "runtime_infrastructure_failure" {
			state["meta_graph_block_reason"] = "runtime_patch_forbidden"
			return state
		}
		if patch.DiagnosisType != "business_quality_failure" ||
			strings.TrimSpace(patch.CustomSpecRef) == "" || len(patch.TargetPaths) == 0 {
			state["meta_graph_block_reason"] = "invalid_targeted_patch"
			return state
		}
		paths := make([]string, 0, len(patch.TargetPaths))
		seen := make(map[string]struct{}, len(patch.TargetPaths))
		for _, rawPath := range patch.TargetPaths {
			path := strings.TrimSpace(rawPath)
			suffix := strings.TrimPrefix(path, "custom_spec.")
			firstSegment, _, _ := strings.Cut(suffix, ".")
			if !strings.HasPrefix(path, "custom_spec.") || strings.TrimSpace(firstSegment) == "" {
				state["meta_graph_block_reason"] = "invalid_targeted_patch"
				return state
			}
			if _, duplicate := seen[path]; duplicate {
				state["meta_graph_block_reason"] = "invalid_targeted_patch"
				return state
			}
			seen[path] = struct{}{}
			paths = append(paths, path)
		}
		state["meta_targeted_patch_paths"] = append([]string(nil), paths...)
		if run.Status != teambuild.StatusAuthorized && run.Status != teambuild.StatusRoundRunning {
			state["meta_graph_block_reason"] = "targeted_patch_status_forbidden"
			return state
		}
		if !confirmed {
			state["meta_graph_block_reason"] = "unconfirmed_targeted_patch"
			return state
		}
		state["meta_graph_planning_allowed"] = true
		state["meta_graph_planning_route"] = "targeted_patch"
		state["meta_template_gap_confirmed"] = mode == teambuild.WorkflowBuildModeCustom && gap != ""
		return state
	}

	if planning && mode == teambuild.WorkflowBuildModeBlueprint && gap == "" {
		// The frozen brief says the platform-native planning channel is in use,
		// but it cannot decide whether one of the four fixed templates expresses
		// the business topology. The architect makes that semantic decision and
		// only delegates on a miss; the graph designer then owns one bounded
		// declarative_v1 submission whose server-side validator remains decisive.
		state["meta_graph_planning_allowed"] = true
		state["meta_graph_planning_route"] = "declarative_v1"
		state["meta_graph_declarative_planning"] = true
		return state
	}
	if !planning {
		state["meta_graph_block_reason"] = "initial_planning_status_forbidden"
		return state
	}
	if mode == teambuild.WorkflowBuildModeCustom && gap != "" {
		// TeamBuildRun currently stores the requested gap but no distinct admin
		// confirmation of that gap. BriefHash authenticates bytes, not authority.
		// Task 2/4 must add and persist that authority before this path opens.
		state["meta_graph_block_reason"] = "template_gap_confirmation_unavailable"
		return state
	}
	if mode != teambuild.WorkflowBuildModeCustom || gap == "" {
		state["meta_graph_block_reason"] = "unconfirmed_template_gap"
		return state
	}
	return state
}
