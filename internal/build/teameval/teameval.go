// Package teameval implements the team-forge hard-gate evaluator (plan §8.1
// + §13.1 L1 capability points, config side and evidence side). Given a
// workspace, a TeamBuildRun, and its target team, GateEvaluator reads the
// frozen stores and the shared validators and produces one structured
// GateResult per stable gate code. It never writes and never reads builder
// self-reports (评测语境隔离, plan §4.4); every finding carries an evidence
// reference (asset ID + version, or run id).
//
// The package is also the single owner of the shared employee-graph
// validator (ValidateEmployeeGraph) and the team-workflow machine.Validate
// assembly (ValidateWorkflowDraft) — teamforge consumes both through this
// package so no second implementation exists.
package teameval

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teameval/gatecodes"
	"github.com/jinyitao123/weave/internal/kernel/audit"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// TriggerConfig and GraphDefinition alias the machine workflow shapes so
// evaluator call sites stay terse.
type (
	TriggerConfig   = machine.TriggerConfig
	GraphDefinition = machine.GraphDefinition
)

// Stable hard-gate codes. These are the machine contract between the
// evaluator and EvaluationReport consumers: messages and details may evolve,
// codes must not change without an explicit compatibility decision.
const (
	GateAgentRolesExist       = gatecodes.AgentRolesExist
	GateTeamShape             = gatecodes.TeamShape
	GateWorkerKinds           = gatecodes.WorkerKinds
	GateWorkflowRefs          = gatecodes.WorkflowRefs
	GateNoCrossEmployee       = gatecodes.NoCrossEmployee
	GateGraphSchema           = gatecodes.GraphSchema
	GateDepsFreezable         = gatecodes.DepsFreezable
	GateDepsPinned            = gatecodes.DepsPinned
	GateEngineGraphMatch      = gatecodes.EngineGraphMatch
	GateScopeCompliance       = gatecodes.ScopeCompliance
	GateL1WorkerCapabilities  = gatecodes.L1WorkerCapabilities
	GateL1FlowCapabilities    = gatecodes.L1FlowCapabilities
	GateRunTerminalConsistent = gatecodes.RunTerminalConsistent
	GateNoGovernanceViolation = gatecodes.NoGovernanceViolation
)

// AllGateCodes is the stable, ordered list of every gate code the evaluator
// emits. Order is the report order.
var AllGateCodes = []string{
	GateAgentRolesExist,
	GateTeamShape,
	GateWorkerKinds,
	GateWorkflowRefs,
	GateNoCrossEmployee,
	GateGraphSchema,
	GateDepsFreezable,
	GateDepsPinned,
	GateEngineGraphMatch,
	GateScopeCompliance,
	GateL1WorkerCapabilities,
	GateL1FlowCapabilities,
	GateRunTerminalConsistent,
	GateNoGovernanceViolation,
}

// Status is one gate outcome: pass blocks nothing, warn does not block but
// enters the report, fail blocks the build round.
type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	StatusWarn Status = "warn"
)

// GateResult is the evidence-backed outcome of one hard gate. Code is the
// stable gate code, Status the classification, Evidence a concrete reference
// (asset ID+version, run id, task id, audit id, ...) and Detail the finding.
type GateResult struct {
	Gate         string `json:"gate"`
	Code         string `json:"code"`
	Status       Status `json:"status"`
	Evidence     string `json:"evidence"`
	Detail       string `json:"detail"`
	WaiverReason string `json:"waiver_reason,omitempty"`
}

// ToHardGateResult maps one result onto the persisted EvaluationReport
// shape (teambuild.HardGateResult): warn is not blocking, so only fail maps
// to Passed=false.
func (r GateResult) ToHardGateResult() teambuild.HardGateResult {
	return teambuild.HardGateResult{
		GateID:       r.Code,
		Passed:       r.Status != StatusFail,
		EvidenceRef:  r.Evidence,
		Status:       string(r.Status),
		Detail:       r.Detail,
		Floor:        teambuild.IsContractFloorGate(r.Code),
		WaiverReason: r.WaiverReason,
	}
}

// Pass returns the all-green result shape for one gate code.
func Pass(code, evidence, detail string) GateResult {
	return GateResult{Gate: code, Code: code, Status: StatusPass, Evidence: evidence, Detail: detail}
}

// Fail returns the blocking result shape for one gate code.
func Fail(code, evidence, detail string) GateResult {
	return GateResult{Gate: code, Code: code, Status: StatusFail, Evidence: evidence, Detail: detail}
}

// Warn returns the non-blocking result shape for one gate code.
func Warn(code, evidence, detail string) GateResult {
	return GateResult{Gate: code, Code: code, Status: StatusWarn, Evidence: evidence, Detail: detail}
}

// Deps aggregates the narrow read-only store surfaces backing GateEvaluator.
// Production implementations are the existing stores; the narrow shapes
// prevent the evaluator from reaching write methods and make integration
// tests trivial.
type Deps struct {
	// Pool begins the read-only transactions that carry exact dependency
	// resolution (credentials.ResolveModelRevisionTx etc.).
	Pool *pgxpool.Pool
	// BuildRuns reads the TeamBuildRun control record (asset scope).
	BuildRuns BuildRunReader
	// Models resolves model bindings to exact provider revisions (F2).
	Models ModelResolver
	// Teams lists workspace teams.
	Teams TeamReader
	// Roster reads the complete team-worker roster of one team.
	Roster RosterReader
	// Agents lists workspace agents and reads exact immutable versions.
	Agents AgentReader
	// Workflows lists a team's workflows and reads exact versions.
	Workflows WorkflowListReader
	// MCPs reads workspace MCP servers with availability state.
	MCPs MCPServerReader
	// Runtimes reads one workspace runtime with online state.
	Runtimes RuntimeReader
	// SkillsNamespace reads the legacy skill namespace (skill:<workspace>)
	// exactly like the platform skills API.
	SkillsNamespace NamespaceReader
	// Snapshots reads immutable team-run snapshots.
	Snapshots SnapshotReader
	// Tasks reads durable task queue records.
	Tasks TaskReader
	// Deliverables reads immutable final deliverables.
	Deliverables DeliverableReader
	// Audit reads MCP tool invocation audit records.
	Audit AuditReader
	// Runs reads run evidence from the audit:<workspace> namespace exactly
	// like the platform runs read path.
	Runs NamespaceReader
}

// BuildRunReader is satisfied by *teambuild.Store.
type BuildRunReader interface {
	GetBuildRun(ctx context.Context, workspaceID, buildRunID string) (teambuild.TeamBuildRun, error)
}

// WorkflowListReader extends the shared workflow read surface with the team
// listing and exact version reads the evaluator needs.
type WorkflowListReader interface {
	WorkflowReader
	ListByTeam(ctx context.Context, workspaceID, teamID string) ([]workflow.TeamWorkflow, error)
	GetVersion(ctx context.Context, workspaceID, workflowID string, version int) (*workflow.TeamWorkflowVersion, error)
	ListVersionsByWorkflows(ctx context.Context, workspaceID string, workflowIDs []string) ([]workflow.TeamWorkflowVersion, error)
}

// MCPServerReader is satisfied by *mcpregistry.Store.
type MCPServerReader interface {
	Get(ctx context.Context, workspaceID, id string) (mcpregistry.ServerView, error)
	List(ctx context.Context, workspaceID string) ([]mcpregistry.ServerView, error)
}

// RuntimeReader is satisfied by *runtimes.Store.
type RuntimeReader interface {
	Get(ctx context.Context, workspaceID, id string) (*runtimes.Runtime, error)
}

// NamespaceReader is the read-only slice of loom.Store (Get/List) used by
// the legacy skill namespace and the run evidence namespace;
// *pgstore.PGStore satisfies it.
type NamespaceReader interface {
	Get(ctx context.Context, ns, key string) ([]byte, error)
	List(ctx context.Context, ns, prefix string) ([]string, error)
}

// SnapshotReader is satisfied by *snapshot.Store.
type SnapshotReader interface {
	GetByRunID(ctx context.Context, workspaceID, runID string) (*snapshot.TeamRunSnapshot, error)
	ListByTeam(ctx context.Context, workspaceID, teamID string) ([]snapshot.TeamRunSnapshot, error)
}

// TaskReader is satisfied by *taskqueue.Store.
type TaskReader interface {
	Get(ctx context.Context, workspaceID, id string) (*taskqueue.Task, error)
	List(ctx context.Context, workspaceID string, limit, offset int) ([]taskqueue.Task, int, error)
}

// DeliverableReader is satisfied by *deliverable.Store.
type DeliverableReader interface {
	Get(ctx context.Context, workspaceID, id string) (deliverable.FinalDeliverable, error)
	List(ctx context.Context, workspaceID string, filter deliverable.ListFilter) ([]deliverable.FinalDeliverable, error)
}

// AuditReader is satisfied by *audit.Store.
type AuditReader interface {
	List(ctx context.Context, workspaceID, agent string, limit, offset int) ([]audit.Entry, error)
}

// ModelResolver matches the signature of the free function
// credentials.ResolveModelRevisionTx; ModelResolverFunc binds it.
type ModelResolver interface {
	ResolveModelRevisionTx(
		ctx context.Context,
		tx pgx.Tx,
		workspaceID, modelID string,
	) (frozen.FrozenModelBinding, error)
}

// ModelResolverFunc adapts credentials.ResolveModelRevisionTx (a package
// function) to the narrow resolver interface.
type ModelResolverFunc func(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, modelID string,
) (frozen.FrozenModelBinding, error)

// ResolveModelRevisionTx delegates to the wrapped function.
func (f ModelResolverFunc) ResolveModelRevisionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, modelID string,
) (frozen.FrozenModelBinding, error) {
	return f(ctx, tx, workspaceID, modelID)
}

// GateEvaluator evaluates the hard gates of one team + build run. It is a
// pure read path: construction is cheap and Evaluate never mutates state.
type GateEvaluator struct {
	deps Deps
}

// NewGateEvaluator creates a hard-gate evaluator over the given stores.
func NewGateEvaluator(deps Deps) *GateEvaluator {
	return &GateEvaluator{deps: deps}
}

// EvaluatedTeam is the loaded configuration snapshot one evaluation reads:
// the exact team, lead version, roster workers with their exact agent
// versions, and the team's workflows with their evaluated versions.
type EvaluatedTeam struct {
	BuildRun  teambuild.TeamBuildRun
	Team      *org.Team
	Lead      *registry.AgentRecord
	LeadErr   string
	Workers   []EvaluatedWorker
	Workflows []EvaluatedWorkflow
}

// EvaluatedWorker is one roster slot bound to its exact agent record.
type EvaluatedWorker struct {
	TeamWorker registry.TeamWorker
	Agent      *registry.AgentRecord
	Err        string
}

// EvaluatedWorkflow is one team workflow bound to the evaluated version
// (published when available, otherwise the latest draft) and its decoded
// machine shape.
type EvaluatedWorkflow struct {
	Workflow workflow.TeamWorkflow
	Version  *workflow.TeamWorkflowVersion
	Trigger  TriggerShape
	Graph    GraphShape
}

// TriggerShape carries the decoded trigger and its decode report.
type TriggerShape struct {
	Trigger   TriggerConfig
	DecodeErr string
}

// GraphShape carries the decoded graph and its decode report.
type GraphShape struct {
	Graph     GraphDefinition
	DecodeErr string
}

// Evaluate loads the team + build run from the stores and runs every gate.
// The returned slice always contains one result per stable gate code, in
// AllGateCodes order.
func (e *GateEvaluator) Evaluate(
	ctx context.Context,
	workspaceID, buildRunID, teamID string,
) ([]GateResult, error) {
	loaded, err := e.loadEvaluation(ctx, workspaceID, buildRunID, teamID)
	if err != nil {
		return nil, err
	}

	runners := []func(context.Context, EvaluatedTeam) GateResult{
		e.gateAgentRolesExist,
		e.gateTeamShape,
		e.gateWorkerKinds,
		e.gateWorkflowRefs,
		e.gateNoCrossEmployee,
		e.gateGraphSchema,
		e.gateDepsFreezable,
		e.gateDepsPinned,
		e.gateEngineGraphMatch,
		e.gateScopeCompliance,
		e.gateL1WorkerCapabilities,
		e.gateL1FlowCapabilities,
		e.gateRunTerminalConsistent,
		e.gateNoGovernanceViolation,
	}
	results := make([]GateResult, 0, len(runners))
	for _, run := range runners {
		results = append(results, applyGateWaiver(run(ctx, loaded), loaded.BuildRun.Contract))
	}
	return results, nil
}

func applyGateWaiver(result GateResult, contract teambuild.EvaluationContract) GateResult {
	if result.Code == GateScopeCompliance || result.Code == GateNoGovernanceViolation {
		return result
	}
	for _, waiver := range contract.WaivedGates {
		if waiver.GateID != result.Code {
			continue
		}
		result.Status = StatusWarn
		result.WaiverReason = waiver.Reason
		if result.Detail == "" {
			result.Detail = fmt.Sprintf("waived: %s", waiver.Reason)
		} else {
			result.Detail = fmt.Sprintf("%s; waived: %s", result.Detail, waiver.Reason)
		}
		break
	}
	return result
}
