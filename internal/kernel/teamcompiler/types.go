package teamcompiler

import (
	"context"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

type TeamRunMode string

const (
	TeamRunModeFixedWorkflow     TeamRunMode = "fixed_workflow"
	TeamRunModeFreeCollaboration TeamRunMode = "free_collaboration"
)

type InteractionKind string

const (
	InteractionConsult  InteractionKind = "consult"
	InteractionDispatch InteractionKind = "dispatch"
	InteractionHandoff  InteractionKind = "handoff"
)

type FrozenLeadRef struct {
	AgentID      string
	AgentVersion int64
	Name         string
}

type FrozenWorkerRoleProof struct {
	Role                  string
	AgentContentHash      string
	CapabilitySchema      int
	CapabilityContentHash string
}

type FrozenTeamWorker struct {
	WorkerAgentID      string
	WorkerAgentVersion int64
	Name               string
	Duty               string
	WhenToUse          string
	ContextInstruction string
	AllowedKinds       []InteractionKind
	DefaultKind        InteractionKind
	ResultRequirement  string
	EnabledAtSnapshot  bool
	RoleProof          FrozenWorkerRoleProof
}

type TeamInteractionInput struct {
	WorkspaceID   string
	TeamID        string
	RunID         string
	RunSnapshotID string
	Lead          FrozenLeadRef
	Workers       []FrozenTeamWorker
}

type AuthorizedWorker struct {
	WorkerAgentID      string
	WorkerAgentVersion int64
	DisplayName        string
	Duty               string
	WhenToUse          string
	ContextInstruction string
	ResultRequirement  string
	DefaultKind        InteractionKind
}

type HandoffRoute struct {
	RouteKey           string
	WorkerAgentID      string
	WorkerAgentVersion int64
	DisplayName        string
}

type TeamInteractionCatalog struct {
	WorkspaceID   string
	TeamID        string
	RunID         string
	RunSnapshotID string
	LeadAgentID   string
	Consult       map[string]AuthorizedWorker
	Dispatch      map[string]AuthorizedWorker
	Handoff       map[string]HandoffRoute
}

type InteractionAuthorizationRequest struct {
	WorkspaceID   string
	TeamID        string
	RunID         string
	RunSnapshotID string
	WorkerAgentID string
	RouteKey      string
	Kind          InteractionKind
}

type TeamInteractionAssembler interface {
	Assemble(context.Context, TeamInteractionInput) (TeamInteractionCatalog, error)
	AuthorizeInteraction(context.Context, TeamInteractionCatalog, InteractionAuthorizationRequest) (AuthorizedWorker, error)
}

type InteractionFailureRecord struct {
	FailureClass string
	FailureCause string
}

type TeamInteractionRecorder interface {
	RecordInteractionFailure(context.Context, TeamInteractionCatalog, InteractionAuthorizationRequest, InteractionFailureRecord) error
}

type LockedWorkerRef struct {
	WorkspaceID        string
	RunSnapshotID      string
	WorkerAgentID      string
	WorkerAgentVersion int64
}

type LockedWorkerRunner interface {
	StepFor(context.Context, LockedWorkerRef) (loom.Step, error)
}

// FrozenWorkflowGraph is the host-owned fixed topology consumed by
// CompileTeam. Worker identity is always an exact stable ID and version.
type FrozenWorkflowGraph struct {
	Entry string               `json:"entry"`
	Steps []FrozenWorkflowStep `json:"steps"`
}

type FrozenWorkflowStep struct {
	ID                 string `json:"id"`
	WorkerAgentID      string `json:"worker_agent_id"`
	WorkerAgentVersion int64  `json:"worker_agent_version"`
	Next               string `json:"next"`
}

type TeamCompileInput struct {
	WorkspaceID           string
	TeamID                string
	RunID                 string
	RunSnapshotID         string
	Mode                  TeamRunMode
	Lead                  FrozenLeadRef
	Workers               []FrozenTeamWorker
	Workflow              *FrozenWorkflowGraph
	RequestContext        map[string]any
	WorkerRunner          LockedWorkerRunner
	LLM                   contract.LLM
	Tools                 contract.ToolDispatcher
	ToolHooks             []contract.ToolHook
	BeforeStepHooks       []loom.StepHook
	AfterStepHooks        []loom.StepHook
	ContextEnrichmentStep loom.Step
	CheckpointStore       loom.Store
}

// TeamResumeInput intentionally has no Workers or catalog field. Resume can
// only recover the frozen catalog written by the original CompileTeam call.
type TeamResumeInput struct {
	WorkspaceID           string
	TeamID                string
	RunID                 string
	RunSnapshotID         string
	Mode                  TeamRunMode
	Workflow              *FrozenWorkflowGraph
	RequestContext        map[string]any
	WorkerRunner          LockedWorkerRunner
	LLM                   contract.LLM
	Tools                 contract.ToolDispatcher
	ToolHooks             []contract.ToolHook
	BeforeStepHooks       []loom.StepHook
	AfterStepHooks        []loom.StepHook
	ContextEnrichmentStep loom.Step
	CheckpointStore       loom.Store
}

type TeamCompileManifest struct {
	Mode               TeamRunMode            `json:"mode"`
	Catalog            TeamInteractionCatalog `json:"catalog"`
	CatalogContentHash string                 `json:"catalog_content_hash"`
}

type TeamCompiler interface {
	CompileTeam(context.Context, TeamCompileInput) (*loom.Graph, TeamCompileManifest, error)
}

type TeamResumer interface {
	ResumeTeam(context.Context, TeamResumeInput) (*loom.Graph, TeamCompileManifest, error)
}

// FrozenWorkerClosure contains every fact the locked runner may consult.
// There is deliberately no Registry, latest resolver, or name resolver.
type FrozenWorkerClosure struct {
	WorkspaceID   string
	RunSnapshotID string
	Workers       []FrozenTeamWorker
	Bundles       []frozen.FrozenExecutionBundle
	BuildOpts     compiler.FrozenBuildOpts
}

const (
	CodeTeamCompileInputInvalid         = "team_compile_input_invalid"
	CodeTeamCompileIdentityMismatch     = "team_compile_identity_mismatch"
	CodeTeamInteractionKindInvalid      = "team_interaction_kind_invalid"
	CodeTeamInteractionUnauthorized     = "team_interaction_unauthorized"
	CodeTeamInteractionCatalogMismatch  = "team_interaction_catalog_mismatch"
	CodeTeamInteractionRouteCollision   = "team_interaction_route_collision"
	CodeTeamInteractionSnapshotMismatch = "team_interaction_snapshot_mismatch"
	CodeTeamWorkerVersionUnavailable    = "team_worker_version_unavailable"
	CodeTeamWorkerRoleIncompatible      = "team_worker_role_incompatible"
	CodeTeamWorkerGraphIncompatible     = "team_worker_graph_incompatible"
	CodeTeamWorkerFactoryIncompatible   = "team_worker_factory_incompatible"
	CodeTeamFrozenDependencyUndeclared  = "team_frozen_dependency_undeclared"
	CodeTeamLiveResolutionForbidden     = "team_live_resolution_forbidden"
	CodeTeamHandoffTargetInvalid        = "team_handoff_target_invalid"
	CodeTeamHandoffUnavailable          = "team_handoff_unavailable"
)

var (
	ErrTeamCompileInputInvalid         = &Error{code: CodeTeamCompileInputInvalid}
	ErrTeamCompileIdentityMismatch     = &Error{code: CodeTeamCompileIdentityMismatch}
	ErrTeamInteractionKindInvalid      = &Error{code: CodeTeamInteractionKindInvalid}
	ErrTeamInteractionUnauthorized     = &Error{code: CodeTeamInteractionUnauthorized}
	ErrTeamInteractionCatalogMismatch  = &Error{code: CodeTeamInteractionCatalogMismatch}
	ErrTeamInteractionRouteCollision   = &Error{code: CodeTeamInteractionRouteCollision}
	ErrTeamInteractionSnapshotMismatch = &Error{code: CodeTeamInteractionSnapshotMismatch}
	ErrTeamWorkerVersionUnavailable    = &Error{code: CodeTeamWorkerVersionUnavailable}
	ErrTeamWorkerRoleIncompatible      = &Error{code: CodeTeamWorkerRoleIncompatible}
	ErrTeamWorkerGraphIncompatible     = &Error{code: CodeTeamWorkerGraphIncompatible}
	ErrTeamWorkerFactoryIncompatible   = &Error{code: CodeTeamWorkerFactoryIncompatible}
	ErrTeamFrozenDependencyUndeclared  = &Error{code: CodeTeamFrozenDependencyUndeclared}
	ErrTeamLiveResolutionForbidden     = &Error{code: CodeTeamLiveResolutionForbidden}
	ErrTeamHandoffTargetInvalid        = &Error{code: CodeTeamHandoffTargetInvalid}
	ErrTeamHandoffUnavailable          = &Error{code: CodeTeamHandoffUnavailable}
)

type Error struct {
	code  string
	cause error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *Error) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *Error) Is(target error) bool {
	if e == nil || target == nil {
		return false
	}
	targetError, ok := target.(*Error)
	return ok && targetError != nil && targetError.code != "" && targetError.code == e.code
}

func codedError(code string, cause error) error {
	return &Error{code: code, cause: cause}
}

var _ compiler.CodedError = (*Error)(nil)
