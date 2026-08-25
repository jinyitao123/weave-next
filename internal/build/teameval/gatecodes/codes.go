// Package gatecodes owns the stable machine codes shared by the team-build
// contract floor and the evaluator. It deliberately contains no evaluator or
// teambuild types, avoiding a package cycle between those two owners.
package gatecodes

const (
	AgentRolesExist       = "gate_agent_roles_exist"
	TeamShape             = "gate_team_shape"
	WorkerKinds           = "gate_worker_kinds"
	WorkflowRefs          = "gate_workflow_refs"
	NoCrossEmployee       = "gate_no_cross_employee"
	GraphSchema           = "gate_graph_schema"
	DepsFreezable         = "gate_deps_freezable"
	DepsPinned            = "gate_deps_pinned"
	EngineGraphMatch      = "gate_engine_graph_match"
	ScopeCompliance       = "gate_scope_compliance"
	L1WorkerCapabilities  = "gate_l1_worker_capabilities"
	L1FlowCapabilities    = "gate_l1_flow_capabilities"
	RunTerminalConsistent = "gate_run_terminal_consistent"
	NoGovernanceViolation = "gate_no_governance_violation"
)
