package teamforge

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

// Deps aggregates the narrow read-only store interfaces backing the
// teamforge tools. Production implementations are the existing stores; the
// narrow shapes prevent tools from accidentally reaching write methods and
// make unit tests trivial.
type Deps struct {
	// Agents lists workspace agents and reads exact immutable versions.
	Agents AgentReader
	// Teams lists workspace teams (the existing team read path).
	Teams TeamReader
	// Roster reads the complete team-worker roster of one team.
	Roster RosterReader
	// DispatchRules reads one team's free-collaboration dispatch rules.
	DispatchRules DispatchRulesReader
	// Workflows reads workflows, versions, and exact version contents.
	Workflows WorkflowReader
	// Skills reads the legacy skill namespace exactly like the API list path.
	Skills NamespaceReader
	// MCPs lists workspace MCP servers with availability state.
	MCPs MCPServerReader
	// Providers lists workspace provider credentials metadata.
	Providers ProviderReader
	// Runtimes lists workspace runtimes with online state.
	Runtimes RuntimeReader
	// Runs reads run evidence from the audit:<workspace> namespace exactly
	// like the platform runs read path.
	Runs NamespaceReader
	// Tasks reads durable task queue records.
	Tasks TaskReader
	// Deliverables reads immutable final deliverables.
	Deliverables DeliverableReader
}

// AgentReader is satisfied by *registry.AgentRegistry.
type AgentReader interface {
	List(ctx context.Context, workspaceID string) ([]registry.AgentRecord, error)
	GetVersion(ctx context.Context, workspaceID, agentID string, version int) (*registry.AgentRecord, error)
}

// TeamReader is satisfied by *orgstore.Store.
type TeamReader interface {
	ListTeams(ctx context.Context, workspaceID string) ([]org.Team, error)
}

// BusinessTeamLister lists only non-platform teams: the "__" built-in prefix
// is excluded at the query layer so callers physically cannot enumerate
// platform teams. Satisfied by *orgstore.Store.
type BusinessTeamLister interface {
	ListBusinessTeams(ctx context.Context, workspaceID string) ([]org.Team, error)
}

// RosterReader is satisfied by *registry.TeamWorkerRepository.
type RosterReader interface {
	ListByTeam(ctx context.Context, workspaceID, teamID string) ([]registry.TeamWorker, error)
}

// DispatchRulesReader is satisfied by *orgstore.Store.
type DispatchRulesReader interface {
	GetTeamDispatchRules(ctx context.Context, workspaceID, teamID string) (org.TeamDispatchRules, error)
}

// WorkflowReader is satisfied by *workflow.Store.
type WorkflowReader interface {
	Get(ctx context.Context, workspaceID, workflowID string) (*workflow.TeamWorkflow, error)
	GetVersion(ctx context.Context, workspaceID, workflowID string, version int) (*workflow.TeamWorkflowVersion, error)
	ListByTeam(ctx context.Context, workspaceID, teamID string) ([]workflow.TeamWorkflow, error)
	ListVersionsByWorkflows(ctx context.Context, workspaceID string, workflowIDs []string) ([]workflow.TeamWorkflowVersion, error)
}

// NamespaceReader is the read-only slice of loom.Store (Get/List) used by
// the skill and run evidence namespaces; *pgstore.PGStore satisfies it.
type NamespaceReader interface {
	Get(ctx context.Context, ns, key string) ([]byte, error)
	List(ctx context.Context, ns, prefix string) ([]string, error)
}

// MCPServerReader is satisfied by *mcpregistry.Store.
type MCPServerReader interface {
	ListMetadata(ctx context.Context, workspaceID string) ([]mcpregistry.ServerMetadata, error)
}

// ProviderReader is satisfied by *credentials.Store.
type ProviderReader interface {
	ListMetadata(ctx context.Context, workspaceID string) ([]credentials.ProviderHead, error)
}

// RuntimeReader is satisfied by *runtimes.Store.
type RuntimeReader interface {
	List(ctx context.Context, workspaceID string) ([]runtimes.Runtime, error)
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

// WriteDeps contains product asset commands. Transaction ownership stays
// with the command implementation, outside the builder tool surface.
type WriteDeps struct {
	// Agents commits the model binding check and immutable agent version atomically.
	Agents AgentWriter
	// AgentLoad reads the current record for patch merging and the
	// create-v1 existence guard.
	AgentLoad AgentLoader
	// Runtimes reads one workspace runtime for the engine binding rule.
	Runtimes RuntimeGetter
	// Teams creates one active team aggregate (lead + initial roster) through
	// the platform team-creation path.
	Teams TeamCreator
	// TeamDesign updates only the objective/scenario/success contract for an
	// existing team during optimize-mode compilation.
	TeamDesign TeamDesignUpdater
	// Roster applies one complete team-roster CAS command through the
	// platform roster command writer (never a raw row write).
	Roster RosterCommander
	// DispatchRules writes one team's free-collaboration dispatch rules.
	DispatchRules DispatchRuleWriter
	// Workflows persists workflow drafts through the workflow store's own
	// Create/CreateDraft/UpdateDraft (CAS) paths.
	Workflows WorkflowWriter
}

// AgentWriteRequest carries the already authorized and assembled asset command.
type AgentWriteRequest struct {
	WorkspaceID   string
	Record        registry.AgentRecord
	InternalGraph bool
}

// AgentWriteResult is returned only after the command committed successfully.
type AgentWriteResult struct {
	Record registry.AgentRecord
	JSON   json.RawMessage
}

// AgentWriter preserves provider validation and version creation as one operation.
type AgentWriter interface {
	CommitAgent(context.Context, AgentWriteRequest) (AgentWriteResult, error)
}

// AgentLoader is satisfied by *registry.AgentRegistry.Get.
type AgentLoader interface {
	Get(ctx context.Context, workspaceID, name string) (*registry.AgentRecord, error)
	// List resolves a caller-supplied stable agent ID that Get (name-keyed)
	// cannot resolve. *registry.AgentRegistry satisfies both methods.
	List(ctx context.Context, workspaceID string) ([]registry.AgentRecord, error)
}

// RuntimeGetter is satisfied by *runtimes.Store.Get.
type RuntimeGetter interface {
	Get(ctx context.Context, workspaceID, id string) (*runtimes.Runtime, error)
}

// TeamCreator is satisfied by *orgstore.Store.CreateActiveTeam.
type TeamCreator interface {
	CreateActiveTeam(
		ctx context.Context,
		workspaceID string,
		input org.CreateActiveTeamInput,
	) (org.CreateActiveTeamResult, error)
}

// TeamDesignUpdater is satisfied by *orgstore.Store.UpdateTeamDesign.
type TeamDesignUpdater interface {
	UpdateTeamDesign(
		ctx context.Context,
		workspaceID, teamID string,
		input org.UpdateTeamDesignInput,
	) (org.Team, error)
}

// RosterCommander is satisfied by *registry.AgentRegistry.ApplyTeamRosterCommand.
type RosterCommander interface {
	ApplyTeamRosterCommand(
		ctx context.Context,
		command registry.TeamRosterCommand,
	) (*registry.TeamRosterResult, error)
}

// DispatchRuleWriter is satisfied by *orgstore.Store.PutTeamDispatchRules.
type DispatchRuleWriter interface {
	PutTeamDispatchRules(
		ctx context.Context,
		workspaceID string,
		rules org.TeamDispatchRules,
	) error
}

// WorkflowWriter is the narrow write surface of the team workflow store used
// by the workflow write tools: create workflow+v1 draft, clone a published
// version into the next draft, and CAS-update one mutable draft.
type WorkflowWriter interface {
	Create(
		ctx context.Context,
		workflow *workflow.TeamWorkflow,
		initial workflow.DraftInput,
	) (*workflow.TeamWorkflowVersion, error)
	CreateDraft(
		ctx context.Context,
		workspaceID, workflowID, createdBy string,
	) (*workflow.TeamWorkflowVersion, error)
	UpdateDraft(
		ctx context.Context,
		workspaceID, workflowID string,
		version int,
		expectedUpdatedAt time.Time,
		input workflow.DraftInput,
	) (*workflow.TeamWorkflowVersion, error)
}
