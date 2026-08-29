package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/pgstore"
	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/app/attachments"
	"github.com/jinyitao123/weave/internal/app/chatrequest"
	"github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/jinyitao123/weave/internal/app/ownermem"
	"github.com/jinyitao123/weave/internal/app/projects"
	"github.com/jinyitao123/weave/internal/app/schedules"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/app/webui"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/fanout"
	"github.com/jinyitao123/weave/internal/base/realtime"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/base/teamrun"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/kernel/audit"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/memory"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimellm"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	importskills "github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/teamcompiler"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// Server holds all shared dependencies for the HTTP API.
type Server struct {
	Echo                      *echo.Echo
	Store                     loom.Store
	Registry                  *registry.AgentRegistry
	Descriptors               *compiler.DescriptorRegistry
	TeamAssembler             teamcompiler.TeamInteractionAssembler // optional test seam; nil uses the production assembler
	Models                    *llmrouter.Resolver
	Config                    *config.Config
	Embedders                 *memory.EmbedderResolver // nil if PG pool unavailable — resolves workspace-scoped memory services
	StoreExt                  *storeext.PGExt          // nil if Store is not PGStore
	Fanout                    *fanout.Store            // nil if PG pool unavailable
	FanoutReconciler          *fanout.Reconciler       // nil if fan-out completion is unavailable
	Tasks                     *taskqueue.Store         // nil if PG pool unavailable
	Runtimes                  *runtimes.Store          // nil if PG pool unavailable
	LocalExec                 mcphost.RemoteEngineExecutor
	RemoteExec                mcphost.RemoteEngineExecutor
	engineExecMu              sync.Mutex
	TaskWorker                *taskqueue.Worker // nil if Tasks is nil
	AgentSchedules            *schedule.Store   // nil if PG pool unavailable
	ScheduleTransactions      ScheduleTransactionBeginner
	WorkflowScheduleAdmission WorkflowScheduleAdmissionService
	WorkflowScheduleStepHook  func(context.Context, WorkflowScheduleStage) error
	RunLifecycleHook          loomruntime.RunLifecycleHook
	Snapshots                 *snapshot.Store                     // nil if PG pool unavailable
	TeamReader                *teamReader                         // nil if team-aware read dependencies are unavailable
	AgentRunReader            loomruntime.AgentRunLifecycleReader // nil if PG pool unavailable
	ScheduleStore             *schedules.Store                    // nil if PG pool unavailable
	UserStore                 *users.Store                        // nil if PG pool unavailable
	KeyStore                  *apikeys.Store                      // nil if PG pool unavailable
	OrgStore                  *org.Store                          // nil if PG pool unavailable
	Projects                  *projects.Store                     // nil if PG pool unavailable
	Attachments               *attachments.Store                  // nil if PG pool unavailable
	ChatRequests              *chatrequest.Store                  // nil if PG pool unavailable
	Workflow                  *workflow.Store                     // nil if PG pool unavailable
	TeamBuild                 *teambuild.Store                    // nil if PG pool unavailable
	TeamBuildOrchestrator     TeamBuildExecutionService           // nil until the production meta-team controller is configured
	TeamTemplates             TeamTemplateService                 // nil until the template fast path is configured
	TeamEvaluations           TeamEvaluationService               // nil until post-template evaluation is configured
	TeamForgeDrafts           *teamforge.DraftRegistry            // shared in-memory draft registry; nil disables teamforge wiring
	Pool                      *pgxpool.Pool                       // nil if PG pool unavailable
	TeamWorkers               *registry.TeamWorkerRepository      // nil if PG pool unavailable
	DeliveryTargets           *delivery.Store                     // nil if WEAVE_SECRET_KEY is not configured
	Deliverables              *deliverable.Store                  // nil if PG pool unavailable
	SkillImporter             *importskills.Importer              // nil if PG pool unavailable
	Skills                    *importskills.Store                 // nil if PG pool unavailable
	Audit                     *audit.Store                        // nil if PG pool unavailable
	Credentials               *credentials.Store                  // nil if WEAVE_SECRET_KEY is not configured
	SystemProviders           credentials.SystemProviderSource
	MCPRegistry               *mcpregistry.Store     // nil if WEAVE_SECRET_KEY is not configured
	MCPResolver               mcphost.AccessResolver // optional override; defaults to MCPRegistry-backed resolver
	Conversations             *conversation.Store    // nil if PG pool unavailable
	OwnerMem                  OwnerMemoryStore       // nil if PG pool unavailable
	Hub                       *realtime.Hub
	sessionExecutionWorkers   *sessionExecutionWorkers
	teamRunWorkers            *teamrun.Workers
	teamRunCancel             *teamrun.CancelService
	teamRunHumanResume        *teamrun.HumanResumeService
	teamRunHumanTasks         *teamrun.HumanTaskReader
	workflowFanoutReconciler  *fanout.WorkflowReconcilerWorker
}

func (s *Server) engineExecutorFor(remote bool) mcphost.RemoteEngineExecutor {
	s.engineExecMu.Lock()
	defer s.engineExecMu.Unlock()

	var workspacesRoot, oneapiBase, boundaryBase, oneapiKey string
	if s.Config != nil {
		workspacesRoot = s.Config.WorkspacesRoot
		oneapiBase = s.Config.OneAPIBase
		boundaryBase = s.Config.MCPBoundaryBase
		oneapiKey = s.Config.OneAPIKey
	}
	if remote {
		if s.RemoteExec == nil && s.Tasks != nil && s.Runtimes != nil {
			s.RemoteExec = runtimes.NewExecutor(s.Tasks, s.Runtimes, oneapiBase, oneapiKey)
		}
		return s.RemoteExec
	}
	if s.LocalExec == nil {
		s.LocalExec = runtimes.NewLocalExecutor(workspacesRoot, oneapiBase, boundaryBase, oneapiKey)
	}
	return s.LocalExec
}

// teamRunCLIExecutor keeps published TeamWorkflow execution on the runtime
// path frozen into each CLI AgentRecord. The local executor would silently
// ignore runtime_id and run every worker inside the server container.
func (s *Server) teamRunCLIExecutor() mcphost.RemoteEngineExecutor {
	return s.engineExecutorFor(true)
}

// llmFor returns the workspace-scoped LLM snapshot for one request. The
// snapshot is reused within the request (compile, streaming adapter, hooks,
// agent-as-tool) so every consumer sees the same provider set.
func (s *Server) llmFor(ctx context.Context, tenant string) (contract.LLM, error) {
	if s.Models == nil {
		return nil, errors.New("model resolver not configured")
	}
	return s.Models.ForWorkspace(ctx, tenant)
}

// runtimeLLMForNode binds one immutable Loom node identity to the runtime
// selected for the enclosing turn. Only the inference transport is replaced;
// callers still compile and execute the original Loom graph and tool surface.
func (s *Server) runtimeLLMForNode(
	tenant string,
	identity, runtimeBinding *registry.AgentRecord,
	scope execution.Scope,
	runSnapshotID string,
) (contract.LLM, error) {
	if identity == nil || runtimeBinding == nil {
		return nil, errors.New("runtime LLM binding is incomplete")
	}
	bound := *identity
	bound.Engine = runtimeBinding.Engine
	bound.Model = ""
	bound.RuntimeID = runtimeBinding.RuntimeID
	bound.RuntimePoolID = runtimeBinding.RuntimePoolID
	bound.RuntimePolicyMode = runtimeBinding.RuntimePolicyMode
	return runtimellm.New(
		s.engineExecutorFor(true), tenant, &bound,
		execution.AgentExecutionStamp{
			AgentID: bound.ID, AgentVersion: bound.Version, ExecutionScope: scope,
			RunSnapshotID: runSnapshotID,
		},
	)
}

// memoryFor resolves the workspace memory service on the soft paths
// (chat/sse/jobs/resume/topology/preview/sub-agents): resolution failures are
// only logged and degrade to "memory not configured", matching the existing
// memSvc == nil semantics.
func (s *Server) memoryFor(ctx context.Context, tenant string) *memory.Service {
	if s.Embedders == nil {
		return nil
	}
	svc, err := s.Embedders.ForWorkspace(ctx, tenant)
	if err != nil {
		slog.Warn("embedder resolution failed", "workspace", tenant, "error", err)
		return nil
	}
	return svc
}

// memoryForStrict resolves the workspace memory service for the memory CRUD
// API: resolution errors surface to the caller (500), while an unconfigured
// embedder still returns (nil, nil) so handlers keep their existing 501.
func (s *Server) memoryForStrict(ctx context.Context, tenant string) (*memory.Service, error) {
	if s.Embedders == nil {
		return nil, nil
	}
	return s.Embedders.ForWorkspace(ctx, tenant)
}

// NewServer creates a new API server with all dependencies wired.
func NewServer(cfg *config.Config, store loom.Store, models *llmrouter.Resolver) *Server {
	e := echo.New()
	e.HideBanner = true
	var agentRegistry *registry.AgentRegistry
	if ps, ok := store.(*pgstore.PGStore); ok {
		agentRegistry = registry.New(ps.Pool())
	}

	e.Use(middleware.Recover())
	e.Use(middleware.RequestID())
	e.Use(middleware.Logger())
	// CORS: use configured origins, default to same-origin in production.
	corsOrigins := []string{}
	if cfg.CORSOrigins != "" {
		corsOrigins = strings.Split(cfg.CORSOrigins, ",")
	}
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: corsOrigins,
		AllowMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders: []string{"Authorization", "Content-Type"},
	}))

	s := &Server{
		Echo:     e,
		Store:    store,
		Registry: agentRegistry,
		Models:   models,
		Config:   cfg,
	}

	// Initialize platform store extensions if PGStore is available.
	if ps, ok := store.(*pgstore.PGStore); ok {
		s.StoreExt = storeext.New(ps.Pool())
		s.Pool = ps.Pool()
		s.TeamWorkers = registry.NewTeamWorkerRepository(ps.Pool())
		s.TeamForgeDrafts = teamforge.NewDraftRegistry()
		s.ChatRequests = chatrequest.New(ps.Pool(), chatrequest.RealClock{})
		s.Attachments = attachments.New(ps.Pool())
		s.OwnerMem = ownermem.New(ps.Pool(), ownermem.RealClock{})
		s.Workflow = workflow.New(ps.Pool(), workflow.RealClock{})
		s.TeamBuild = teambuild.New(ps.Pool(), teambuild.RealClock{})
		s.WorkflowScheduleAdmission = NewWorkflowScheduleAdmissionService(s.Workflow)
		s.ScheduleTransactions = ps.Pool()
		s.Snapshots = snapshot.NewStore(ps.Pool())
		expectedRuns, err := loomruntime.NewExpectedRunRegistry(s.StoreExt)
		lifecycleReader, lifecycleErr := loomruntime.NewPGRunLifecycleReader(ps.Pool(), nil)
		if lifecycleErr == nil {
			s.AgentRunReader = lifecycleReader
		}
		if err == nil && lifecycleErr == nil {
			s.TeamReader = &teamReader{
				expected: snapshotRegistryTeamExpectedSource{
					snapshots: s.Snapshots,
					registry:  expectedRuns,
				},
				terminals: pgTeamTerminalLoader{store: s.StoreExt},
				lifecycle: lifecycleReader,
			}
		} else {
			slog.Error(
				"team reader initialization failed",
				"registry_error",
				err,
				"lifecycle_error",
				lifecycleErr,
			)
		}
		s.SkillImporter = importskills.NewImporter(
			ps.Pool(), importskills.New(ps.Pool()), importskills.NewPGLegacyReader(ps.Pool()),
		)
		s.sessionExecutionWorkers = newSessionExecutionWorkers(s)
	}

	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	// Public endpoints (no auth).
	s.Echo.GET("/v1/health", s.handleHealth)
	s.Echo.GET("/install.sh", s.handleInstallScript)
	s.Echo.GET("/install.ps1", s.handleInstallScript)
	s.Echo.GET("/v1/downloads/runtime/:os/:arch", s.handleDownloadRuntime)
	s.Echo.POST("/v1/auth/token", s.handleIssueToken)
	s.Echo.POST("/v1/auth/login", s.handleLogin)
	s.Echo.Any("/v1/mcp-boundary/:tenant/:agent/:idx", s.handleMCPBoundary)
	s.Echo.Any("/v1/mcp-gateway/:workspace/:agent/:serverID", s.handleMCPGateway)

	// Lazy getter for KeyStore (set after route registration in main.go).
	keyStoreGetter := func() *apikeys.Store { return s.KeyStore }
	userStoreGetter := func() *users.Store { return s.UserStore }

	// Register endpoint — uses optional auth (first user bootstrap needs no auth, subsequent need admin).
	s.Echo.POST("/v1/auth/register", s.handleRegister,
		OptionalAuthMiddleware(s.Config.JWTSecret, keyStoreGetter, userStoreGetter), RequireScope("admin"))

	// Authenticated endpoints.
	auth := s.Echo.Group("/v1", AuthMiddleware(s.Config.JWTSecret, keyStoreGetter, userStoreGetter))
	adminScope := RequireScope("admin")
	agentsScope := RequireScope("agents")
	chatScope := RequireScope("chat")
	runsScope := RequireScope("runs")
	memoryScope := RequireScope("memory")
	orgScope := RequireScope("org")

	// Auth: refresh & me.
	auth.POST("/auth/refresh", s.handleRefresh)
	auth.GET("/auth/me", s.handleMe)
	auth.PUT("/auth/me", s.handleUpdateMe)
	auth.PUT("/auth/me/password", s.handleChangeMyPassword)

	// User management (admin or owner).
	auth.GET("/users", s.handleListUsers, RequireAnyRole("admin", "owner"), adminScope)
	auth.GET("/users/:id", s.handleGetUser, RequireAnyRole("admin", "owner"), adminScope)
	auth.PUT("/users/:id", s.handleUpdateUser, RequireAnyRole("admin", "owner"), adminScope)
	auth.DELETE("/users/:id", s.handleDeleteUser, RequireAnyRole("admin", "owner"), adminScope)

	// API Key management (admin only).
	auth.POST("/auth/api-keys", s.handleCreateAPIKey, RequireRole("admin"), adminScope)
	auth.GET("/auth/api-keys", s.handleListAPIKeys, RequireRole("admin"), adminScope)
	auth.DELETE("/auth/api-keys/:id", s.handleDeleteAPIKey, RequireRole("admin"), adminScope)

	// Organization.
	auth.GET("/workspace", s.handleGetWorkspace, orgScope)
	auth.GET("/features", s.handleGetFeatures, orgScope)
	auth.GET("/workspace/members", s.handleListMembers, orgScope)
	auth.POST("/workspace/members", s.handleAddMember, RequireAnyRole("admin", "owner"), orgScope)
	auth.DELETE("/workspace/members/:userID", s.handleRemoveMember, RequireAnyRole("admin", "owner"), orgScope)
	auth.GET("/projects", s.handleListProjects, orgScope)
	auth.POST("/projects", s.handleCreateProject, orgScope)
	auth.POST("/projects/ensure-unclassified", s.handleEnsureUnclassifiedProject, orgScope)
	auth.GET("/projects/:id", s.handleGetProject, orgScope)
	auth.PUT("/projects/:id", s.handleUpdateProject, orgScope)
	auth.DELETE("/projects/:id", s.handleArchiveProject, orgScope)
	auth.POST("/projects/:id/restore", s.handleRestoreProject, orgScope)
	auth.POST("/projects/:id/move", s.handleMoveProject, orgScope)
	auth.GET("/projects/:id/collaborators", s.handleListProjectCollaborators, orgScope)
	auth.POST("/projects/:id/collaborators", s.handleAddProjectCollaborator, RequireAnyRole("admin", "owner"), orgScope)
	auth.DELETE("/projects/:id/collaborators/:teamId", s.handleRemoveProjectCollaborator, RequireAnyRole("admin", "owner"), orgScope)
	auth.GET("/projects/:id/resources", s.handleListProjectResources, orgScope)
	auth.POST("/projects/:id/resources", s.handleCreateProjectResource, orgScope)
	auth.DELETE("/projects/:id/resources/:resourceID", s.handleDeleteProjectResource, orgScope)
	auth.GET("/projects/:id/memories", s.handleListProjectMemories, memoryScope)
	auth.POST("/projects/:id/memories", s.handleCreateProjectMemory, memoryScope)
	auth.DELETE("/projects/:id/memories/:memoryID", s.handleDeleteProjectMemory, memoryScope)
	auth.POST("/projects/:id/memories/search", s.handleSearchProjectMemories, memoryScope)
	auth.GET("/deliverables", s.handleListFinalDeliverables, chatScope)
	auth.GET("/deliverables/:id", s.handleGetFinalDeliverable, chatScope)
	auth.GET("/deliverables/:id/content", s.handleDownloadFinalDeliverable, chatScope)
	auth.GET("/conversations/:id/deliverable", s.handleGetConversationDeliverable, chatScope)
	auth.GET("/teams", s.handleListTeams, orgScope)
	auth.GET("/team-creation-options", s.handleGetTeamCreationOptions, orgScope)
	auth.POST("/teams", s.handleCreateTeam, RequireRole("admin"), orgScope)
	auth.POST("/teams:from-template", s.handleCreateTeamFromTemplate, RequireRole("admin"), orgScope)
	auth.POST("/teams/:id/evaluations", s.handleEvaluateTeam, RequireRole("admin"), orgScope)
	auth.GET("/team-templates/samples", s.handleListTeamTemplateSamples, orgScope)
	auth.GET("/teams/:id", s.handleGetTeam, orgScope)
	auth.GET("/teams/:id/dispatch-rules", s.handleGetTeamDispatchRules, orgScope)
	auth.PUT("/teams/:id/dispatch-rules", s.handlePutTeamDispatchRules, RequireAnyRole("admin", "owner"), orgScope)
	auth.PUT("/teams/:id/roster", s.handleUpdateTeamRoster, RequireAnyRole("admin", "owner"), orgScope)
	auth.GET("/teams/:id/workers/:worker/revocation-impact", s.handleGetTeamWorkerRevocationImpact, orgScope)
	auth.PUT("/teams/:id", s.handleRenameTeam, RequireRole("admin"), orgScope)
	auth.DELETE("/teams/:id", s.handleDeleteTeam, RequireRole("admin"), orgScope)
	auth.POST("/teams/:id/workflows", s.handleCreateWorkflow, RequireAnyRole("admin", "owner"), orgScope)
	auth.GET("/teams/:id/workflows", s.handleListTeamWorkflows, orgScope)
	auth.GET("/workflows/:id", s.handleGetWorkflow, orgScope)
	auth.GET("/workflows/:id/versions/:version", s.handleGetWorkflowVersion, orgScope)
	auth.POST("/workflows/:id/drafts", s.handleCreateWorkflowDraft, RequireAnyRole("admin", "owner"), orgScope)
	auth.PUT("/workflows/:id/versions/:version", s.handleUpdateWorkflowDraft, RequireAnyRole("admin", "owner"), orgScope)
	auth.POST("/workflows/:id/versions/:version/publish", s.handlePublishWorkflowVersion, RequireAnyRole("admin", "owner"), orgScope)
	auth.POST("/workflows/:id/run", s.handleRunWorkflow, RequireAnyRole("admin", "owner"), orgScope)
	auth.POST("/internal/workflows/:id/run", s.handleRunWorkflow, RequireAnyRole("admin", "owner"), orgScope)
	auth.POST("/internal/team-build-runs", s.handleCreateTeamBuildRun, RequireRole("admin"), orgScope)
	auth.GET("/internal/team-build-runs", s.handleListBuildRuns, orgScope)
	auth.PUT("/team-build-runs/:id/blueprint", s.handlePlanTeamBlueprint, RequireRole("admin"), orgScope)
	auth.PUT("/internal/team-build-runs/:id/drafts", s.handleUpdateTeamBuildRunDrafts, RequireRole("admin"), orgScope)
	auth.POST("/internal/team-build-runs/:id/authorize", s.handleAuthorizeBuildRun, RequireRole("admin"), orgScope)
	auth.POST("/internal/team-build-runs/:id/submit", s.handleSubmitBuildRun, RequireRole("admin"), orgScope)
	auth.POST("/internal/team-build-runs/:id/execute", s.handleExecuteTeamBuildRun, RequireRole("admin"), orgScope)
	auth.POST("/internal/team-build-runs/:id/cancel", s.handleCancelBuildRun, RequireRole("admin"), orgScope)
	auth.POST("/internal/team-build-runs/:id/rollback", s.handleRollbackBuildRun, RequireRole("admin"), orgScope)
	auth.GET("/internal/team-build-runs/:id/progress", s.handleGetBuildRunProgress, orgScope)
	auth.GET("/internal/team-build-runs/:id/rounds", s.handleListBuildRunRounds, orgScope)
	auth.GET("/internal/team-build-runs/:id/rounds/:n/report", s.handleGetBuildRunRoundReport, orgScope)
	auth.GET("/internal/team-build-runs/:id/usage", s.handleGetBuildRunUsage, orgScope)
	auth.GET("/internal/team-build-runs/:id", s.handleGetBuildRun, orgScope)
	auth.POST("/internal/team-build-runs/candidate-runs", s.handleCandidateTestRun, RequireRole("admin"), orgScope)
	auth.POST("/internal/team-build-runs/publish", s.handleCandidatePublish, RequireRole("admin"), orgScope)
	auth.POST("/workflows/:id/versions/:version/validate", s.handleValidateWorkflowVersion, orgScope)
	auth.PUT("/workflows/:id/versions/:version/admission", s.handlePutWorkflowAdmission, RequireAnyRole("admin", "owner"), orgScope)
	auth.GET("/workflows/:id/versions/:version/admission/audit", s.handleListWorkflowAdmissionAudit, RequireAnyRole("admin", "owner"), orgScope)
	auth.DELETE("/workflows/:id", s.handleArchiveWorkflow, RequireAnyRole("admin", "owner"), orgScope)
	auth.GET("/workflows/:id/versions/:version/dependencies", s.handleGetWorkflowVersionDependencies, orgScope)
	auth.GET("/workflows/:id/versions/:version/admission", s.handleGetWorkflowVersionAdmission, orgScope)

	// Revisioned outbound delivery targets (admin only).
	auth.GET("/delivery-targets", s.handleListDeliveryTargets, RequireRole("admin"), adminScope)
	auth.POST("/delivery-targets", s.handleCreateDeliveryTarget, RequireRole("admin"), adminScope)
	auth.GET("/delivery-targets/:id", s.handleGetDeliveryTarget, RequireRole("admin"), adminScope)
	auth.PUT("/delivery-targets/:id", s.handleUpdateDeliveryTarget, RequireRole("admin"), adminScope)
	auth.GET("/delivery-targets/:id/revisions/:revision", s.handleGetDeliveryTargetRevision, RequireRole("admin"), adminScope)
	auth.POST("/delivery-targets/:id/rotate-headers", s.handleRotateDeliveryTargetHeaders, RequireRole("admin"), adminScope)
	auth.POST("/delivery-targets/:id/disable", s.handleDisableDeliveryTarget, RequireRole("admin"), adminScope)
	auth.POST("/delivery-targets/:id/revoke", s.handleRevokeDeliveryTarget, RequireRole("admin"), adminScope)
	auth.DELETE("/delivery-targets/:id", s.handleDeleteDeliveryTarget, RequireRole("admin"), adminScope)

	// Remote engine runtimes.
	auth.GET("/runtimes", s.handleListRuntimes, orgScope)
	auth.POST("/runtimes", s.handleCreateRuntime, orgScope)
	auth.PUT("/runtimes/:id", s.handleRenameRuntime, RequireAnyRole("admin", "owner"), orgScope)
	auth.DELETE("/runtimes/:id", s.handleDeleteRuntime, orgScope)
	runtimeAPI := s.Echo.Group("/v1/runtime", s.runtimeAuthMiddleware())
	runtimeAPI.POST("/hello", s.handleRuntimeHello)
	runtimeAPI.POST("/heartbeat", s.handleRuntimeHeartbeat)
	runtimeAPI.POST("/claim", s.handleRuntimeClaim)
	runtimeAPI.POST("/tasks/:id/renew", s.handleRuntimeTaskRenew)
	runtimeAPI.POST("/tasks/:id/complete", s.handleRuntimeTaskComplete)
	runtimeAPI.POST("/tasks/:id/fail", s.handleRuntimeTaskFail)
	runtimeAPI.GET("/tasks/:id/attachments/:aid", s.handleRuntimeTaskAttachment)
	// Task-scoped MCP gateway: a remote loom daemon dials one of these per MCP
	// server index; auth is the runtime lease, the record is the frozen task
	// snapshot, and upstream URLs/headers never leave the server.
	runtimeAPI.Any("/tasks/:id/mcp/:idx", s.handleRuntimeTaskMCP)
	// Task-scoped LLM proxy: the same remote loom daemon proxies each model
	// call (and its stream) back through the server so provider keys stay
	// server-side; the daemon may only reach models this task's agent is
	// configured for.
	runtimeAPI.POST("/tasks/:id/llm/chat", s.handleRuntimeTaskLLMChat)
	runtimeAPI.POST("/tasks/:id/llm/stream", s.handleRuntimeTaskLLMStream)

	// Agent CRUD.
	auth.GET("/agents", s.handleListAgents, agentsScope)
	auth.GET("/agents/:name", s.handleGetAgent, agentsScope)
	auth.GET("/agents/:name/team-memberships", s.handleGetAgentTeamMemberships, orgScope)
	auth.GET("/agents/:name/run-summary", s.handleGetAgentRunSummary, orgScope)
	auth.GET("/agents/:name/channels", s.handleListChannels, agentsScope)
	auth.POST("/agents/:name/channels", s.handleCreateChannel, agentsScope)
	auth.POST("/agents/:name/channels/reorder", s.handleReorderChannels, agentsScope)
	auth.PATCH("/agents/:name/channels/:id", s.handleRenameChannel, agentsScope)
	auth.DELETE("/agents/:name/channels/:id", s.handleDeleteChannel, agentsScope)
	auth.GET("/agents/:name/memory-slots", s.handleGetMemorySlots, agentsScope)
	auth.PUT("/agents/:name/memory-slots", s.handlePutMemorySlots, RequireRole("admin"), adminScope)
	auth.GET("/agents/:name/memory-profile", s.handleGetMemoryProfile, RequireAnyRole("admin", "owner"), memoryScope)
	auth.POST("/agents", s.handleCreateAgent, agentsScope)
	auth.PUT("/agents/:name", s.handleUpdateAgent, agentsScope)
	auth.DELETE("/agents/:name", s.handleDeleteAgent, agentsScope)
	auth.GET("/agents/:name/managed", s.handleListManaged, agentsScope)
	auth.POST("/agents/:name/managed", s.handleLinkManages, RequireRole("admin"), agentsScope)
	auth.DELETE("/agents/:name/managed/:worker", s.handleUnlinkManages, RequireRole("admin"), agentsScope)
	auth.GET("/agent-links", s.handleListAgentLinks, agentsScope)
	auth.POST("/agent-links", s.handleCreateAgentLink, RequireRole("admin"), agentsScope)
	auth.PATCH("/agent-links/:id", s.handleUpdateAgentLink, RequireRole("admin"), agentsScope)
	auth.DELETE("/agent-links/:id", s.handleDeleteAgentLink, RequireRole("admin"), agentsScope)
	auth.POST("/agents/cleanup-orphans", s.handleCleanupOrphanWorkers, RequireRole("admin"), agentsScope)
	auth.POST("/agents/:name/preview-prompt", s.handlePreviewPrompt, agentsScope)
	auth.GET("/agents/:name/topology", s.handleTopology, agentsScope)
	auth.POST("/agents/upload", s.handleUploadAgent, agentsScope)
	auth.POST("/agents/import/preview", s.handleImportPreview, agentsScope)

	// MCP proxy (for Console to list tools from internal MCP servers).
	auth.POST("/mcp/tools", s.handleMCPListTools, agentsScope)
	auth.GET("/mcp-servers", s.handleListMCPServers, agentsScope)
	auth.POST("/mcp-servers", s.handleCreateMCPServer, RequireRole("admin"), adminScope)
	auth.GET("/mcp-servers/:id", s.handleGetMCPServer, agentsScope)
	auth.PUT("/mcp-servers/:id", s.handleUpdateMCPServer, RequireRole("admin"), adminScope)
	auth.DELETE("/mcp-servers/:id", s.handleDeleteMCPServer, RequireRole("admin"), adminScope)
	auth.POST("/mcp-servers/:id/probe", s.handleProbeMCPServer, RequireRole("admin"), adminScope)
	auth.GET("/mcp-servers/:id/tools", s.handleGetMCPServerTools, agentsScope)
	auth.POST("/attachments", s.handleUploadAttachment, agentsScope)
	auth.GET("/attachments", s.handleListAttachments, agentsScope)
	auth.GET("/attachments/:id", s.handleGetAttachment, agentsScope)

	// Skills (reusable prompt modules).
	auth.GET("/skills", s.handleListSkills, agentsScope)
	auth.GET("/skills/:id", s.handleGetSkill, agentsScope)
	auth.POST("/skills", s.handleCreateSkill, agentsScope)
	auth.PUT("/skills/:id", s.handleUpdateSkill, agentsScope)
	auth.DELETE("/skills/:id", s.handleDeleteSkill, agentsScope)
	auth.POST("/skills/:id/import-legacy", s.handleImportLegacySkill, RequireRole("admin"), adminScope)

	// Chat & Resume.
	auth.POST("/chat", s.handleChat, chatScope)
	auth.GET("/chat-requests/:id", s.handleGetChatRequest, chatScope)
	auth.GET("/events", s.handleEvents, chatScope)
	auth.POST("/resume", s.handleResume, chatScope)
	auth.GET("/conversations", s.handleListConversations, chatScope)
	auth.PATCH("/conversations/:id", s.handleRenameConversation, chatScope)
	auth.GET("/conversations/:id/messages", s.handleListConversationMessages, chatScope)
	auth.GET("/conversations/:id/chat-request", s.handleGetConversationChatRequest, chatScope)
	auth.PATCH("/conversations/:id/messages/:message_id/assistant-execution-segments", s.handleUpdateAssistantExecutionSegments, chatScope)
	auth.GET("/conversations/:id/threads", s.handleListThreads, chatScope)
	auth.POST("/conversations/:id/threads", s.handleCreateThread, chatScope)
	auth.POST("/conversations/:id/read", s.handleMarkConversationRead, chatScope)
	auth.GET("/inbox/unread", s.handleListInboxUnread, chatScope)
	auth.GET("/human-tasks", s.handleListHumanTasks, runsScope)
	auth.GET("/human-tasks/:run_id", s.handleGetHumanTask, runsScope)
	auth.POST("/human-tasks/:run_id/complete", s.handleCompleteHumanTask, runsScope)
	auth.POST("/messages/:id/flag", s.handleFlagMessage, chatScope)
	auth.DELETE("/messages/:id/flag", s.handleUnflagMessage, chatScope)
	auth.POST("/messages/:id/deliverable", s.handlePromoteMessageDeliverable, chatScope)
	auth.GET("/flags", s.handleListFlags, chatScope)
	auth.GET("/flags/count", s.handleCountFlags, chatScope)

	// Sessions.
	auth.GET("/sessions", s.handleListSessions, chatScope)
	auth.GET("/sessions/:id", s.handleGetSession, chatScope)
	auth.DELETE("/sessions/:id", s.handleDeleteSession, chatScope)

	// Runs.
	auth.GET("/runs", s.handleListRuns, runsScope)
	auth.GET("/runs/:id", s.handleGetRun, runsScope)
	auth.GET("/runs/:id/trace", s.handleGetRunTrace, runsScope)
	auth.GET("/runs/:id/state", s.handleGetRunState, runsScope)
	auth.GET("/runs/:id/checkpoints", s.handleGetRunCheckpoints, runsScope)
	auth.POST("/runs/:id/fork", s.handleForkRun, runsScope)
	auth.GET("/task-groups", s.handleListTaskGroups, runsScope)
	auth.GET("/task-groups/:id", s.handleGetTaskGroup, runsScope)

	// Usage.
	auth.GET("/usage", s.handleGetUsage, runsScope)

	// Providers (LLM configuration).
	auth.GET("/providers", s.handleListProviders, adminScope)
	auth.POST("/providers", s.handleAddProvider, RequireRole("admin"), adminScope)
	auth.GET("/providers/system", s.handleListSystemProviders, RequireRole("admin"), adminScope)
	auth.POST("/providers/system/:id/mirror", s.handleMirrorSystemProvider, RequireRole("admin"), adminScope)
	auth.PUT("/providers/:id", s.handleUpdateProvider, RequireRole("admin"), adminScope)
	auth.DELETE("/providers/:id", s.handleDeleteProvider, RequireRole("admin"), adminScope)

	// Embedding provider.
	auth.GET("/embedder", s.handleGetEmbedder, adminScope)
	auth.PUT("/embedder", s.handleUpdateEmbedder, RequireRole("admin"), adminScope)
	auth.DELETE("/embedder", s.handleDeleteEmbedder, RequireRole("admin"), adminScope)

	// Task ledger (the legacy /jobs route paths are retained).
	auth.GET("/jobs", s.handleListJobs, runsScope)
	auth.GET("/jobs/:id", s.handleGetJob, runsScope)
	auth.POST("/jobs/:id/cancel", s.handleCancelJob, runsScope)

	// Data sources (MCP server aggregation).
	auth.GET("/sources", s.handleListSources, adminScope)

	// Connector schedules.
	auth.GET("/schedules", s.handleListSchedules, adminScope)
	auth.GET("/schedules/one", s.handleGetSchedule, adminScope)
	auth.PUT("/schedules", s.handleUpsertSchedule, adminScope)
	auth.DELETE("/schedules", s.handleDeleteSchedule, adminScope)

	// 数字员工定时上班（agent 调度）。
	auth.GET("/agent-schedules", s.handleListAgentSchedules, agentsScope)
	auth.PUT("/agent-schedules", s.handleUpsertAgentSchedule, agentsScope)
	auth.DELETE("/agent-schedules", s.handleDeleteAgentSchedule, agentsScope)

	// Memory.
	auth.GET("/agents/:name/memories", s.handleListMemories, memoryScope)
	auth.POST("/agents/:name/memories", s.handleCreateMemory, memoryScope)
	auth.DELETE("/agents/:name/memories/:id", s.handleDeleteMemory, memoryScope)
	auth.POST("/agents/:name/memories/search", s.handleSearchMemories, memoryScope)

	// Keep the UI fallback after every API route so it never shadows /v1.
	s.registerWebUIRoutes()
}

func (s *Server) registerWebUIRoutes() {
	uiFS, _ := webui.FS()
	indexHTML, _ := fs.ReadFile(uiFS, "index.html")
	indexHTMLGzip := gzipWebUIBytes(indexHTML)
	fileServer := http.FileServer(http.FS(uiFS))

	handler := func(c echo.Context) error {
		requestPath := c.Request().URL.Path
		if requestPath == "/v1" || strings.HasPrefix(requestPath, "/v1/") {
			return echo.ErrNotFound
		}

		filePath := strings.TrimPrefix(requestPath, "/")
		if info, err := fs.Stat(uiFS, filePath); err == nil && !info.IsDir() {
			if strings.HasPrefix(filePath, "assets/") {
				c.Response().Header().Set(echo.HeaderCacheControl, "public, max-age=31536000, immutable")
			}
			if acceptsGzip(c.Request().Header.Get(echo.HeaderAcceptEncoding)) && shouldGzipWebUIFile(filePath) {
				body, err := fs.ReadFile(uiFS, filePath)
				if err == nil {
					c.Response().Header().Set(echo.HeaderContentEncoding, "gzip")
					c.Response().Header().Add(echo.HeaderVary, "Accept-Encoding")
					return c.Blob(http.StatusOK, webUIContentType(filePath, body), gzipWebUIBytes(body))
				}
			}
			fileServer.ServeHTTP(c.Response(), c.Request())
			return nil
		}

		if acceptsGzip(c.Request().Header.Get(echo.HeaderAcceptEncoding)) {
			c.Response().Header().Set(echo.HeaderContentEncoding, "gzip")
			c.Response().Header().Add(echo.HeaderVary, "Accept-Encoding")
			return c.Blob(http.StatusOK, "text/html; charset=utf-8", indexHTMLGzip)
		}
		return c.Blob(http.StatusOK, "text/html; charset=utf-8", indexHTML)
	}

	s.Echo.GET("/*", handler)
	s.Echo.HEAD("/*", handler)
}

func acceptsGzip(acceptEncoding string) bool {
	for _, part := range strings.Split(acceptEncoding, ",") {
		coding := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if strings.EqualFold(coding, "gzip") {
			return true
		}
	}
	return false
}

func shouldGzipWebUIFile(filePath string) bool {
	switch strings.ToLower(path.Ext(filePath)) {
	case ".css", ".html", ".js", ".json", ".map", ".svg", ".txt", ".xml":
		return true
	default:
		return false
	}
}

func webUIContentType(filePath string, body []byte) string {
	if contentType := mime.TypeByExtension(path.Ext(filePath)); contentType != "" {
		return contentType
	}
	return http.DetectContentType(body)
}

func gzipWebUIBytes(body []byte) []byte {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	_, _ = writer.Write(body)
	_ = writer.Close()
	return buffer.Bytes()
}

// Start runs the HTTP server.
func (s *Server) Start() error {
	s.reconcileOrphanedChatRequests()
	if worker, ok := s.TeamBuildOrchestrator.(interface {
		Start()
		Stop()
	}); ok {
		worker.Start()
		defer worker.Stop()
	}
	if s.sessionExecutionWorkers != nil && !sessionExecutionWorkersDisabled() {
		s.sessionExecutionWorkers.Start()
		defer s.sessionExecutionWorkers.Stop()
	}
	if s.teamRunWorkers != nil && !teamRunWorkersDisabled() {
		s.teamRunWorkers.Start()
		defer s.teamRunWorkers.Stop()
	}
	if s.workflowFanoutReconciler != nil && !teamRunWorkersDisabled() {
		s.workflowFanoutReconciler.Start()
		defer s.workflowFanoutReconciler.Stop()
	}
	return s.Echo.Start(":" + s.Config.Port)
}

// orphanedChatRequestStaleAfter is how long a chat request may stay in
// 'running' before startup reconciliation treats it as orphaned (its handler
// died without writing terminal state, e.g. the server was restarted mid-run).
const orphanedChatRequestStaleAfter = 15 * time.Minute

// reconcileOrphanedChatRequests finalizes chat requests whose stream handler
// died without writing terminal state, so a restart cannot leave them stuck in
// 'running' forever.
func (s *Server) reconcileOrphanedChatRequests() {
	if s.ChatRequests == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reconciled, err := s.ChatRequests.FailOrphaned(ctx, orphanedChatRequestStaleAfter)
	if err != nil {
		slog.Warn("failed to reconcile orphaned chat requests", "error", err)
		return
	}
	if reconciled > 0 {
		slog.Info("reconciled orphaned chat requests", "count", reconciled)
	}
}

const teamRunWorkersDisableEnv = "WEAVE_TEAMRUN_WORKERS_DISABLED"

// Deployment policy pending (OQ-5). These process-local values are temporary
// worker policy and are not part of the TeamRun ABI.
const (
	temporaryTeamRunWorkerPollInterval = 2 * time.Second
	temporaryTeamRunWorkerBatchSize    = 32
	temporaryTeamRunTaskHeartbeat      = 20 * time.Second
)

func teamRunWorkersDisabled() bool {
	value := strings.TrimSpace(os.Getenv(teamRunWorkersDisableEnv))
	if value == "" {
		return false
	}
	disabled, err := strconv.ParseBool(value)
	return err == nil && disabled
}

// ConfigureTeamRunWorkers wires the no-session schedule executor after main
// has installed task, runtime, and credential stores.
func (s *Server) ConfigureTeamRunWorkers() {
	pool := s.GetPool()
	if pool == nil || s.Tasks == nil || s.Snapshots == nil || s.Workflow == nil {
		return
	}
	runStore := teamrun.NewPGStore()
	checkpointStore := teamrun.NewPGCheckpointStore()
	consumer := &teamrun.Consumer{
		Transactions: pool,
		Snapshots:    s.Snapshots,
		Runs:         runStore,
		Tasks:        s.Tasks,
	}
	runtime := &teamrun.WorkflowSerialRuntime{
		Artifacts: s.Workflow,
		Loader: &workflow.RuntimeLoader{
			Registry: s.Descriptors, CLIExecutor: s.teamRunCLIExecutor(),
		},
		HostFactory: workflow.NewRuntimeHostFactory(),
		CredentialResolvers: func(
			workspaceID string,
		) (workflow.RuntimeCredentialResolver, error) {
			if s.Credentials == nil || s.MCPRegistry == nil ||
				s.Runtimes == nil || s.DeliveryTargets == nil {
				return nil, credentials.ErrCredentialUnavailable
			}
			return credentials.NewPoolCredentialResolver(
				pool,
				workspaceID,
				credentials.TxCredentialSources{
					ProviderGate:    s.Credentials.ValidateReferenceTx,
					ProviderResolve: s.Credentials.ResolveProviderAPIKeyTx,
					MCPGate:         s.MCPRegistry.ValidateReferenceTx,
					MCPResolve:      s.MCPRegistry.ResolveMCPAccessTx,
					RuntimeGate:     s.Runtimes.ValidateReferenceTx,
					RuntimeResolve:  s.Runtimes.ResolveRuntimeAccessTx,
					DeliveryGate:    s.DeliveryTargets.ValidateReferenceTx,
					DeliveryResolve: s.DeliveryTargets.ResolveDeliveryAccessTx,
				},
			)
		},
		Transactions:   pool,
		Runs:           runStore,
		Checkpoints:    checkpointStore,
		Tasks:          s.Tasks,
		Snapshots:      s.Snapshots,
		OutputRecorder: s.Deliverables,
	}
	checkpointReader := &teamrun.FanoutCheckpointReader{
		Transactions: pool, Runs: runStore, Checkpoints: checkpointStore,
	}
	coordinator := fanout.NewWorkflowCoordinator(pool, s.Fanout, checkpointReader)
	creatorLeases := &teamrun.FanoutCreatorLeaseReader{
		Transactions: pool, Records: s.StoreExt,
	}
	resumer := &teamrun.FanoutParentRunResumer{
		Transactions: pool, Fanout: s.Fanout, Runs: runStore,
		Checkpoints: checkpointStore, Tasks: s.Tasks, Records: s.StoreExt,
	}
	coordinator.CreatorLeases = creatorLeases
	coordinator.Resumer = resumer
	coordinator.Synthesis = fanout.DurableLateSynthesisScheduler{
		Transactions: pool,
		Store:        s.Fanout,
		Tasks:        s.Tasks,
		Builder:      completionTaskBuilder{Server: s},
	}
	coordinator.Tasks = s.Tasks
	executor := &teamrun.Executor{
		Tasks:             s.Tasks,
		Consumer:          consumer,
		Transactions:      pool,
		Runs:              runStore,
		Checkpoints:       checkpointStore,
		Runtime:           runtime,
		Fanout:            teamrun.FanoutCoordinatorAdapter{Coordinator: coordinator},
		RuntimeRecords:    s.StoreExt,
		HeartbeatInterval: temporaryTeamRunTaskHeartbeat,
	}
	s.teamRunWorkers = &teamrun.Workers{
		Executor: executor,
		CancelGrace: &teamrun.CancelGraceSweeper{
			Transactions: pool,
			Runs:         runStore,
			BatchSize:    temporaryTeamRunWorkerBatchSize,
		},
		TimerWake: &teamrun.TimerWakeSweeper{
			Transactions: pool,
			Runs:         runStore,
			Executor:     executor,
			BatchSize:    temporaryTeamRunWorkerBatchSize,
		},
		HumanTimeout: &teamrun.HumanTimeoutSweeper{
			Transactions: pool,
			Runs:         runStore,
			Checkpoints:  checkpointStore,
			Tasks:        s.Tasks,
			BatchSize:    temporaryTeamRunWorkerBatchSize,
		},
		PollInterval: temporaryTeamRunWorkerPollInterval,
	}
	s.teamRunCancel = &teamrun.CancelService{
		Transactions: pool,
		Runs:         runStore,
	}
	s.teamRunHumanResume = &teamrun.HumanResumeService{
		Transactions: pool,
		Runs:         runStore,
		Checkpoints:  checkpointStore,
		Tasks:        s.Tasks,
	}
	s.teamRunHumanTasks = &teamrun.HumanTaskReader{Pool: pool}
	s.workflowFanoutReconciler = &fanout.WorkflowReconcilerWorker{
		Transactions: pool, Store: s.Fanout, Coordinator: coordinator,
		BatchSize:    temporaryTeamRunWorkerBatchSize,
		PollInterval: temporaryTeamRunWorkerPollInterval,
	}
}

// resolveSubAgent prepares a child agent as a host-managed nested Step.
// Used by orchestrator agents that have sub_agents configured.
func (s *Server) resolveSubAgent(tenant, agentName string) (loom.Step, error) {
	ctx := context.Background()
	rec, err := s.Registry.Get(ctx, tenant, agentName)
	if err != nil {
		return nil, fmt.Errorf("sub-agent %q not found: %w", agentName, err)
	}
	llm, err := s.llmFor(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("resolve models for sub-agent %q: %w", agentName, err)
	}
	tools := s.buildAuditedMCPDispatcher(rec, tenant, "")
	memSvc := s.memoryFor(ctx, tenant)
	terminalSink, err := s.rootTerminalSink()
	if err != nil {
		return nil, fmt.Errorf("prepare terminal sink for sub-agent %q: %w", agentName, err)
	}
	// Prepare child without sub-agent resolution (prevent infinite recursion).
	childOpts := compiler.CompileOpts{
		Store:       s.Store,
		AgentRunner: s.compilerAgentRunner(tenant, "", llm, memSvc, nil),
	}
	if cfg := registry.EffectiveMemoryConfig(rec); memSvc != nil && cfg.Enabled {
		childOpts.MemoryService = memSvc
		if cfg.TopK > 0 {
			childOpts.MemoryTopK = cfg.TopK
		}
		childOpts.AutoRemember = cfg.AutoRemember
		childOpts.MemoryScope = cfg.Scope
	}
	prepared, err := loomruntime.Prepare(loomruntime.RunRequest{
		Tenant: tenant,
		Agent:  rec,
		Stamp: &execution.AgentExecutionStamp{
			AgentID:        rec.ID,
			AgentVersion:   rec.Version,
			ExecutionScope: execution.ScopeLegacyOrchestrator,
			LegacyScope:    false,
		},
		Dependencies: loomruntime.Dependencies{
			LLM:                llm,
			Tools:              tools,
			Store:              s.Store,
			TerminalSink:       terminalSink,
			LifecycleHook:      s.RunLifecycleHook,
			SkillVersionReader: s.Skills,
			CompileOpts:        childOpts,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("prepare sub-agent %q: %w", agentName, err)
	}
	step, err := prepared.NestedStep(loomruntime.NestedStepOptions{
		ParentMergeConfig: loom.DefaultMergeConfig(),
	})
	if err != nil {
		return nil, fmt.Errorf("prepare nested step for sub-agent %q: %w", agentName, err)
	}
	return step, nil
}

// GetPool returns the pgxpool.Pool from the underlying PGStore, or nil.
func (s *Server) GetPool() *pgxpool.Pool {
	if ps, ok := s.Store.(*pgstore.PGStore); ok {
		return ps.Pool()
	}
	if ps, ok := s.Store.(interface{ Pool() *pgxpool.Pool }); ok {
		return ps.Pool()
	}
	return nil
}
