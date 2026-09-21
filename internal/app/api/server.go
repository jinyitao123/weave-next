package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/app/kernelbindings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/pgstore"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/app/attachments"
	appcapabilities "github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/app/chatrequest"
	"github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/jinyitao123/weave/internal/app/ownermem"
	"github.com/jinyitao123/weave/internal/app/projects"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/kernel/audit"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/executionport"
	"github.com/jinyitao123/weave/internal/kernel/fanout"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/memory"
	"github.com/jinyitao123/weave/internal/kernel/publication"

	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimellm"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	importskills "github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamcompiler"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflowhealth"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// Server holds all shared dependencies for the HTTP API.
type Server struct {
	Echo                      *echo.Echo
	Store                     loom.Store
	Registry                  *agentcatalog.AgentRegistry
	Descriptors               *compiler.DescriptorRegistry
	TeamAssembler             teamcompiler.TeamInteractionAssembler // optional test seam; nil uses the production assembler
	Models                    *llmrouter.Resolver
	Config                    *config.Config
	ExternalIdentity          ExternalIdentityVerifier
	ExternalIdentityBinder    externalIdentityBinder
	Embedders                 *memory.EmbedderResolver // nil if PG pool unavailable — resolves workspace-scoped memory services
	StoreExt                  *storeext.PGExt          // nil if Store is not PGStore
	Fanout                    *fanout.Store            // nil if PG pool unavailable
	FanoutReconciler          *fanout.Reconciler       // nil if fan-out completion is unavailable
	Tasks                     *taskqueue.Store         // nil if PG pool unavailable
	Runtimes                  *runtimes.Store          // nil if PG pool unavailable
	RemoteExec                executionport.RemoteEngineExecutor
	TaskWorker                *taskqueue.Worker // nil if Tasks is nil
	AgentSchedules            *schedule.Store   // nil if PG pool unavailable
	ScheduleTransactions      ScheduleTransactionBeginner
	WorkflowScheduleAdmission WorkflowScheduleAdmissionService
	WorkflowScheduleStepHook  func(context.Context, WorkflowScheduleStage) error
	RunLifecycleHook          loomruntime.RunLifecycleHook
	Snapshots                 *snapshot.Store                     // nil if PG pool unavailable
	TeamReader                *teamReader                         // nil if team-aware read dependencies are unavailable
	AgentRunReader            loomruntime.AgentRunLifecycleReader // nil if PG pool unavailable
	UserStore                 *users.Store                        // nil if PG pool unavailable
	KeyStore                  *apikeys.Store                      // nil if PG pool unavailable
	OrgStore                  *orgstore.Store                     // nil if PG pool unavailable
	Projects                  *projects.Store                     // nil if PG pool unavailable
	Attachments               *attachments.Store                  // nil if PG pool unavailable
	ChatRequests              *chatrequest.Store                  // nil if PG pool unavailable
	WorkflowArtifacts         *workflow.ArtifactStore
	Workflow                  *workflowcatalog.Store               // nil if PG pool unavailable
	KernelPublication         publication.Service                  // nil until the process-owned kernel publication unit is configured
	ProductPublication        *teamconstruction.ProductPublication // nil until product activation is bound to kernel receipts
	PublicationAuthority      *teamconstruction.PublicationAuthority
	WorkflowHealth            *workflowhealth.Store              // nil if PG pool unavailable
	TeamBuild                 *teambuild.Store                   // nil if PG pool unavailable
	TeamBuildOrchestrator     TeamBuildExecutionService          // nil until the production meta-team controller is configured
	TeamTemplates             TeamTemplateService                // nil until the template fast path is configured
	TeamEvaluations           TeamEvaluationService              // nil until post-template evaluation is configured
	TeamForgeDrafts           *teamforge.DraftRegistry           // durable build draft registry; nil disables teamforge wiring
	Pool                      *pgxpool.Pool                      // nil if PG pool unavailable
	TeamWorkers               *agentcatalog.TeamWorkerRepository // nil if PG pool unavailable
	DeliveryTargets           *delivery.Store                    // nil if WEAVE_SECRET_KEY is not configured
	Deliverables              *deliverable.Store                 // nil if PG pool unavailable
	SkillImporter             *importskills.Importer             // nil if PG pool unavailable
	Skills                    *importskills.Store                // nil if PG pool unavailable
	Audit                     *audit.Store                       // nil if PG pool unavailable
	Credentials               *credentials.Store                 // nil if WEAVE_SECRET_KEY is not configured
	Capabilities              *appcapabilities.Service           // nil if PG persistence is unavailable
	CapabilityAccess          *appcapabilities.AccessStore
	SystemProviders           credentials.SystemProviderSource
	MCPRegistry               *mcpregistry.Store     // nil if WEAVE_SECRET_KEY is not configured
	MCPResolver               mcphost.AccessResolver // optional override; defaults to MCPRegistry-backed resolver
	Conversations             *conversation.Store    // nil if PG pool unavailable
	OwnerMem                  OwnerMemoryStore       // nil if PG pool unavailable
	sessionExecutionWorkers   *sessionExecutionWorkers
	teamRunWorkers            *teamrun.Workers
	teamRunCancel             *teamrun.CancelService
	teamRunStageRetry         *teamrun.StageRetryService
	teamRunHumanResume        *teamrun.HumanResumeService
	teamRunHumanTasks         *teamrun.HumanTaskReader
	teamRunCorrections        *teamrun.CorrectionStore
	teamRunCorrectionResume   *teamrun.CorrectionResumeService
	teamRunActivities         *teamrun.PGActivityStore
	workflowFanoutReconciler  *fanout.WorkflowReconcilerWorker
	workflowHealthWorkers     *workflowHealthWorkers
}

func (s *Server) engineExecutor() executionport.RemoteEngineExecutor { return s.RemoteExec }

// teamRunCLIExecutor keeps published TeamWorkflow execution on the runtime
// path frozen into each CLI AgentRecord. The local executor would silently
// ignore runtime_id and run every worker inside the server container.
func (s *Server) teamRunCLIExecutor() executionport.RemoteEngineExecutor {
	return s.engineExecutor()
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
		s.engineExecutor(), tenant, &bound,
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
	var agentRegistry *agentcatalog.AgentRegistry
	if ps, ok := store.(*pgstore.PGStore); ok {
		agentRegistry = kernelbindings.NewRegistry(ps.Pool())
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
		Echo:             e,
		Store:            store,
		Registry:         agentRegistry,
		Models:           models,
		Config:           cfg,
		ExternalIdentity: NewForgeSessionVerifier(cfg.ForgeSessionURL, cfg.ForgeDefaultWorkspace, nil),
	}

	// Initialize platform store extensions if PGStore is available.
	if ps, ok := store.(*pgstore.PGStore); ok {
		s.StoreExt = storeext.New(ps.Pool())
		s.Pool = ps.Pool()
		capabilityStore := appcapabilities.NewPGStore(ps.Pool())
		s.Capabilities = appcapabilities.NewService(capabilityStore, capabilityStore)
		s.CapabilityAccess = appcapabilities.NewAccessStore(ps.Pool())
		s.TeamWorkers = agentcatalog.NewTeamWorkerRepository(ps.Pool())
		s.ChatRequests = chatrequest.New(ps.Pool(), chatrequest.RealClock{})
		s.Attachments = attachments.New(ps.Pool())
		s.OwnerMem = ownermem.New(ps.Pool(), ownermem.RealClock{})
		s.WorkflowArtifacts = workflow.NewArtifactStore(ps.Pool(), workflow.RealClock{})
		s.Workflow = workflowcatalog.New(ps.Pool(), workflow.RealClock{}, s.WorkflowArtifacts)
		healthStore, healthErr := workflowhealth.New(ps.Pool(), workflowhealth.Policy{
			WindowSize: cfg.HealthWindowSize, MinSamples: cfg.HealthMinSamples,
			WarningFailureRate: cfg.HealthWarningFailureRate, WarningSlowRate: cfg.HealthWarningSlowRate,
			SlowRunThreshold: time.Duration(cfg.HealthSlowRunSeconds) * time.Second,
		})
		if healthErr != nil {
			slog.Error("workflow health initialization failed", "error", healthErr)
		} else {
			s.WorkflowHealth = healthStore
			s.workflowHealthWorkers = &workflowHealthWorkers{store: healthStore}
		}
		s.TeamBuild = teambuild.New(ps.Pool(), teambuild.RealClock{})
		s.TeamForgeDrafts = teamforge.NewDraftRegistry(s.TeamBuild)
		s.WorkflowScheduleAdmission = NewWorkflowScheduleAdmissionService(s.Workflow, s.WorkflowArtifacts)
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
	s.Echo.GET("/v1/ready", s.handleReady)
	s.Echo.GET("/install.sh", s.handleInstallScript)
	s.Echo.GET("/install.ps1", s.handleInstallScript)
	s.Echo.GET("/v1/downloads/runtime/:os/:arch", s.handleDownloadRuntime)
	s.Echo.POST("/v1/auth/token", s.handleIssueToken)
	s.Echo.POST("/v1/auth/login", s.handleLogin)
	s.Echo.POST("/v1/auth/external/exchange", s.handleExternalIdentityExchange)
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

	// Developer capability contract endpoints. Execution is admitted here;
	// runtime scheduling is intentionally a separate follow-up integration.
	capabilityAPI := s.Echo.Group("/v1", s.capabilityAuthentication())
	capabilityAPI.POST("/capabilities/drafts", s.handleSaveCapabilityDraft, requireCapabilityAccess("manage"))
	capabilityAPI.GET("/capabilities/drafts", s.handleListCapabilityDrafts, requireCapabilityAccess("manage"))
	capabilityAPI.POST("/capabilities/generate", s.handleGenerateCapability, requireCapabilityAccess("manage"))
	capabilityAPI.POST("/capabilities/:capabilityID/debug", s.handleDebugCapability, requireCapabilityAccess("manage"))
	capabilityAPI.POST("/capabilities/:capabilityID/versions/:revision/publish", s.handlePublishCapability, requireCapabilityAccess("manage"))
	capabilityAPI.POST("/capabilities/:capabilityID/versions/:revision/invocations", s.handleInvokeCapability, requireCapabilityAccess("invoke"))
	capabilityAPI.GET("/capability-invocations", s.handleListCapabilityInvocations, requireCapabilityAccess("read"))
	capabilityAPI.GET("/invocations/:invocationID", s.handleGetCapabilityInvocation, requireCapabilityAccess("read"))
	capabilityAPI.GET("/invocations/:invocationID/events", s.handleGetCapabilityInvocationEvents, requireCapabilityAccess("read"))
	capabilityAPI.POST("/invocations/:invocationID/resume", s.handleResumeCapabilityInvocation, requireCapabilityAccess("invoke"))
	capabilityAPI.POST("/invocations/:invocationID/cancel", s.handleCancelCapabilityInvocation, requireCapabilityAccess("cancel"))
	capabilityAPI.GET("/capability-quotas", s.handleCapabilityQuota, requireCapabilityAccess("manage"))
	capabilityAPI.PUT("/capability-quotas", s.handleCapabilityQuota, requireCapabilityAccess("manage"))
	capabilityAPI.GET("/capability-apps", s.handleCapabilityApps, requireCapabilityAccess("manage"))
	capabilityAPI.POST("/capability-apps/actions", s.handleCapabilityAppAction, requireCapabilityAccess("manage"))

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
	auth.GET("/workspace/members", s.handleListMembers, orgScope)
	auth.POST("/workspace/members", s.handleAddMember, RequireAnyRole("admin", "owner"), orgScope)
	auth.DELETE("/workspace/members/:userID", s.handleRemoveMember, RequireAnyRole("admin", "owner"), orgScope)
	auth.GET("/deliverables", s.handleListFinalDeliverables, chatScope)
	auth.GET("/deliverables/:id", s.handleGetFinalDeliverable, chatScope)
	auth.GET("/deliverables/:id/content", s.handleDownloadFinalDeliverable, chatScope)
	auth.GET("/teams", s.handleListTeams, orgScope)
	auth.POST("/teams", s.handleCreateTeam, RequireAnyRole("developer", "admin"), orgScope)
	auth.POST("/teams:from-template", s.handleCreateTeamFromTemplate, RequireRole("admin"), orgScope)
	auth.POST("/teams/:id/evaluations", s.handleEvaluateTeam, RequireRole("admin"), orgScope)
	auth.GET("/team-templates/samples", s.handleListTeamTemplateSamples, orgScope)
	auth.GET("/teams/:id", s.handleGetTeam, orgScope)
	auth.GET("/teams/:id/members/:agent/config-draft", s.handleGetTeamMemberConfigDraft, RequireAnyRole("developer", "admin", "owner"), orgScope)
	auth.PUT("/teams/:id/members/:agent/config-draft", s.handlePutTeamMemberConfigDraft, RequireAnyRole("developer", "admin", "owner"), orgScope)
	auth.GET("/teams/:id/dispatch-rules", s.handleGetTeamDispatchRules, orgScope)
	auth.PUT("/teams/:id/dispatch-rules", s.handlePutTeamDispatchRules, RequireAnyRole("admin", "owner"), orgScope)
	auth.POST("/teams/:id/dispatch", s.handleDispatchTeam, orgScope, chatScope)
	auth.PUT("/game-decision-bindings/:key_id", s.handleBindGameDecision, RequireRole("admin"), adminScope)
	auth.POST("/game-decisions", s.handleAdmitGameDecision, RequireScope("game_decisions"))
	auth.GET("/game-decisions/:decision_id", s.handleReadGameDecision, RequireScope("game_decisions"))
	auth.POST("/game-decisions:cancel", s.handleCancelGameDecision, RequireScope("game_decisions"))
	auth.POST("/workbench/dispatch-inputs", s.handleRegisterDispatchInput, orgScope, chatScope)
	auth.POST("/workbench/dispatch-inputs/:input_revision_id/reconcile", s.handleReconcileDispatchInput, orgScope, chatScope)
	auth.PUT("/teams/:id/roster", s.handleUpdateTeamRoster, RequireAnyRole("admin", "owner"), orgScope)
	auth.GET("/teams/:id/workers/:worker/revocation-impact", s.handleGetTeamWorkerRevocationImpact, orgScope)
	auth.PUT("/teams/:id", s.handleRenameTeam, RequireRole("admin"), orgScope)
	auth.PUT("/teams/:id/profile", s.handleUpdateTeamProfile, RequireAnyRole("developer", "admin", "owner"), orgScope)
	auth.POST("/teams/:id/workers", s.handleCreateTeamWorker, RequireAnyRole("developer", "admin", "owner"), orgScope)
	auth.DELETE("/teams/:id/workers/:worker", s.handleDeleteTeamWorker, RequireAnyRole("developer", "admin", "owner"), orgScope)
	auth.DELETE("/teams/:id", s.handleDeleteTeam, RequireRole("admin"), orgScope)
	auth.POST("/teams/:id/workflows", s.handleCreateWorkflow, RequireAnyRole("admin", "owner"), orgScope)
	auth.GET("/teams/:id/workflows", s.handleListTeamWorkflows, orgScope)
	auth.GET("/workflows/:id", s.handleGetWorkflow, orgScope)
	auth.GET("/workflows/:id/versions/:version", s.handleGetWorkflowVersion, orgScope)
	auth.POST("/workflows/:id/drafts", s.handleCreateWorkflowDraft, RequireAnyRole("admin", "owner"), orgScope)
	auth.PUT("/workflows/:id/versions/:version", s.handleUpdateWorkflowDraft, RequireAnyRole("admin", "owner"), orgScope)
	auth.POST("/workflows/:id/versions/:version/publish", s.handlePublishWorkflowVersion, RequireAnyRole("admin", "owner"), orgScope)
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
	auth.GET("/agent-execution-settings", s.handleListAgentExecutionSettings, orgScope)
	auth.GET("/runtimes", s.handleListRuntimes, orgScope)
	auth.POST("/runtimes", s.handleCreateRuntime, RequireAnyRole("admin", "owner"), orgScope)
	auth.PUT("/runtimes/:id", s.handleRenameRuntime, RequireAnyRole("admin", "owner"), orgScope)
	auth.DELETE("/runtimes/:id", s.handleDeleteRuntime, RequireAnyRole("admin", "owner"), orgScope)
	runtimeAPI := s.Echo.Group("/v1/runtime", s.runtimeAuthMiddleware())
	runtimeAPI.POST("/hello", s.handleRuntimeHello)
	runtimeAPI.POST("/heartbeat", s.handleRuntimeHeartbeat)
	runtimeAPI.POST("/claim", s.handleRuntimeClaim)
	runtimeAPI.POST("/tasks/:id/renew", s.handleRuntimeTaskRenew)
	runtimeAPI.POST("/tasks/:id/complete", s.handleRuntimeTaskComplete)
	runtimeAPI.POST("/tasks/:id/stopped", s.handleRuntimeTaskStopped)
	runtimeAPI.POST("/tasks/:id/events", s.handleRuntimeTaskEvents)
	runtimeAPI.GET("/tasks/:id/attachments/:aid", s.handleRuntimeTaskAttachment)
	// Task-scoped MCP gateway: a remote loom daemon dials one of these per MCP
	// server index; auth is the runtime lease, the record is the frozen task
	// snapshot, and upstream URLs/headers never leave the server.
	s.Echo.Any("/v1/runtime/tasks/:id/mcp/:idx", s.handleRuntimeTaskMCP, s.taskMCPAuthMiddleware())
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
	auth.GET("/agents/:name/memory-slots", s.handleGetMemorySlots, agentsScope)
	auth.PUT("/agents/:name/memory-slots", s.handlePutMemorySlots, RequireRole("admin"), adminScope)
	auth.GET("/agents/:name/memory-profile", s.handleGetMemoryProfile, RequireAnyRole("admin", "owner"), memoryScope)
	auth.POST("/agents", s.handleCreateAgent, agentsScope)
	auth.PUT("/agents/:name", s.handleUpdateAgent, agentsScope)
	auth.PUT("/agents/:name/execution", s.handleConfigureAgentExecution, RequireAnyRole("admin", "owner"), orgScope)
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

	// Execution MCP registry and maintenance.
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
	auth.GET("/chat-requests/:id", s.handleGetChatRequest, chatScope)
	auth.POST("/resume", s.handleResume, chatScope)
	auth.GET("/human-tasks", s.handleListHumanTasks, runsScope)
	auth.GET("/human-tasks/:run_id", s.handleGetHumanTask, runsScope)
	auth.POST("/human-tasks/:run_id/complete", s.handleCompleteHumanTask, runsScope)

	// Runs.
	auth.GET("/runs", s.handleListRuns, runsScope)
	auth.GET("/runs/:id", s.handleGetRun, runsScope)
	auth.GET("/runs/:id/activity", s.handleGetRunActivity, runsScope)
	auth.GET("/runs/:id/delivery", s.handleGetRunDelivery, runsScope)
	auth.POST("/runs/:id/delivery/recheck", s.handleRecheckRunDelivery, runsScope)
	auth.GET("/runs/:id/delivery/verifications/:verification_id", s.handleGetRunVerification, runsScope)
	auth.POST("/runs/:id/stop", s.handleStopRun, runsScope)
	auth.POST("/runs/:id/stages/:node_id/retry", s.handleRetryRunStage, runsScope)
	auth.GET("/runs/:id/corrections", s.handleListRunCorrections, runsScope)
	auth.POST("/runs/:id/corrections", s.handleRequestRunCorrection, runsScope)
	auth.POST("/runs/:id/corrections/:correction_id/confirm", s.handleConfirmRunCorrection, runsScope)
	auth.GET("/runs/:id/trace", s.handleGetRunTrace, runsScope)
	auth.GET("/runs/:id/state", s.handleGetRunState, runsScope)
	auth.GET("/runs/:id/checkpoints", s.handleGetRunCheckpoints, runsScope)

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

	// 数字员工定时上班（agent 调度）。

	// Memory.
	auth.GET("/agents/:name/memories", s.handleListMemories, memoryScope)
	auth.POST("/agents/:name/memories", s.handleCreateMemory, memoryScope)
	auth.DELETE("/agents/:name/memories/:id", s.handleDeleteMemory, memoryScope)
	auth.POST("/agents/:name/memories/search", s.handleSearchMemories, memoryScope)

}

// Start runs the HTTP server.
func (s *Server) Start() error {
	s.reconcileOrphanedChatRequests()
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
	if s.workflowHealthWorkers != nil {
		s.workflowHealthWorkers.Start()
		defer s.workflowHealthWorkers.Stop()
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
	runStore.Transactions = pool
	checkpointStore := teamrun.NewPGCheckpointStore()
	correctionStore := &teamrun.CorrectionStore{Transactions: pool, Runs: runStore}
	activityStore := &teamrun.PGActivityStore{Transactions: pool}
	consumer := &teamrun.Consumer{
		Transactions: pool,
		Snapshots:    s.Snapshots,
		Runs:         runStore,
		Tasks:        s.Tasks,
	}
	runtime := &teamrun.WorkflowSerialRuntime{
		Artifacts: s.WorkflowArtifacts,
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
		Corrections:    correctionStore,
		Activities:     activityStore,
	}
	memberRunner, err := loomruntime.NewMemberRunner(kernelbindings.WithTerminalActivity(storeext.New(pool)))
	if err != nil {
		panic(fmt.Sprintf("configure frozen member runner: %v", err))
	}
	runtime.Members = memberRunner
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
		MemberBudgets:     loomruntime.MemberBudgetCoordinator{},
		Tasks:             s.Tasks,
		Consumer:          consumer,
		Transactions:      pool,
		Runs:              runStore,
		Checkpoints:       checkpointStore,
		Runtime:           runtime,
		Fanout:            teamrun.FanoutCoordinatorAdapter{Coordinator: coordinator},
		Corrections:       correctionStore,
		RuntimeRecords:    s.StoreExt,
		HeartbeatInterval: temporaryTeamRunTaskHeartbeat,
	}
	s.teamRunWorkers = &teamrun.Workers{
		Executor: executor,
		// Four durable claims are enough to make ordinary multi-runtime fanout
		// genuinely concurrent without introducing an unbounded worker pool.
		ExecutorConcurrency: 4,
		CancelGrace: &teamrun.CancelGraceSweeper{
			Tasks:        s.Tasks,
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
		Tasks:        s.Tasks,
	}
	s.teamRunStageRetry = &teamrun.StageRetryService{
		MemberBudgets: loomruntime.MemberBudgetCoordinator{},
		Transactions:  pool, Runs: runStore, Checkpoints: checkpointStore, Tasks: s.Tasks,
	}
	s.teamRunHumanResume = &teamrun.HumanResumeService{
		Transactions: pool,
		Runs:         runStore,
		Checkpoints:  checkpointStore,
		Tasks:        s.Tasks,
	}
	s.teamRunHumanTasks = &teamrun.HumanTaskReader{Pool: pool}
	s.teamRunCorrections = correctionStore
	s.teamRunActivities = activityStore
	s.teamRunCorrectionResume = &teamrun.CorrectionResumeService{
		Transactions: pool, Runs: runStore, Corrections: correctionStore,
		Checkpoints: checkpointStore, Tasks: s.Tasks,
	}
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
