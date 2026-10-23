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
	"github.com/jinyitao123/weave/internal/app/deliveryverify"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/kernel/audit"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
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
	"github.com/jinyitao123/weave/internal/kernel/publication"

	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/secret"
	importskills "github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflowhealth"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// Server holds all shared dependencies for the HTTP API.
type Server struct {
	Feishu                    *feishuClient
	feishuApps                feishuAppCache
	Echo                      *echo.Echo
	Store                     loom.Store
	Registry                  *agentcatalog.AgentRegistry
	Descriptors               *compiler.DescriptorRegistry
	Models                    *llmrouter.Resolver
	Config                    *config.Config
	ExternalIdentity          ExternalIdentityVerifier
	ExternalIdentityBinder    externalIdentityBinder
	StoreExt                  *storeext.PGExt  // nil if Store is not PGStore
	Fanout                    *fanout.Store    // nil if PG pool unavailable
	Tasks                     *taskqueue.Store // nil if PG pool unavailable
	Runtimes                  *runtimes.Store  // nil if PG pool unavailable
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
	Attachments               *attachments.Store                  // nil if PG pool unavailable
	WorkflowArtifacts         *workflow.ArtifactStore
	Workflow                  *workflowcatalog.Store               // nil if PG pool unavailable
	KernelPublication         publication.Service                  // nil until the process-owned kernel publication unit is configured
	ProductPublication        *teamconstruction.ProductPublication // nil until product activation is bound to kernel receipts
	PublicationAuthority      *teamconstruction.PublicationAuthority
	WorkflowHealth            *workflowhealth.Store              // nil if PG pool unavailable
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
	BusinessDelegations       *businessaction.Store  // task-scoped Forge identity and action boundary
	mcpLimiter                mcpRateLimiter
	deviceEnrollLimiter       mcpRateLimiter
	teamRunWorkers            *teamrun.Workers
	modelActivityBuffer       *teamrun.BufferedActivityRecorder
	ModelGateway              *llmrouter.ModelGateway
	teamRunCancel             *teamrun.CancelService
	teamRunStageRetry         *teamrun.StageRetryService
	teamRunHumanResume        *teamrun.HumanResumeService
	teamRunHumanTasks         *teamrun.HumanTaskReader
	teamRunCorrections        *teamrun.CorrectionStore
	teamRunCorrectionResume   *teamrun.CorrectionResumeService
	teamRunActivities         *teamrun.PGActivityStore
	workflowFanoutReconciler  *fanout.WorkflowReconcilerWorker
	workflowHealthWorkers     *workflowHealthWorkers
	employeeRunEventWorker    *employeeRunEventWorker
	taskDelegationRevoker     *taskDelegationRevoker
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
		AllowHeaders: []string{"Authorization", "Content-Type", forgeDelegationHeader, adminRequestHeader},
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
		s.Capabilities = appcapabilities.NewService(capabilityStore)
		s.CapabilityAccess = appcapabilities.NewAccessStore(ps.Pool())
		s.TeamWorkers = agentcatalog.NewTeamWorkerRepository(ps.Pool())
		s.Attachments = attachments.New(ps.Pool())
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
	}

	s.Feishu = feishuFromEnv()
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	s.Echo.Pre(adminSessionCookieMiddleware)
	s.registerAdminConsole()

	// Public endpoints (no auth).
	s.Echo.POST("/v1/integrations/feishu/events", s.handleFeishuEvent)
	s.Echo.POST("/v1/integrations/feishu/events/:callback", s.handleFeishuWorkspaceEvent)
	s.Echo.GET("/v1/health", s.handleHealth)
	s.Echo.GET("/v1/ready", s.handleReady)
	s.Echo.GET("/install.sh", s.handleInstallScript)
	s.Echo.GET("/install.ps1", s.handleInstallScript)
	s.Echo.GET("/v1/downloads/runtime/:os/:arch", s.handleDownloadRuntime)
	s.Echo.GET("/v1/downloads/cua-driver/:os/:arch", s.handleDownloadCuaDriver)
	// The one-time enrollment code is the credential of this exchange.
	s.Echo.POST("/v1/runtime/device-enrollments", s.handleDeviceEnrollmentExchange)
	s.Echo.POST("/v1/auth/external/exchange", s.handleExternalIdentityExchange)
	s.Echo.Any("/v1/mcp-boundary/:tenant/:agent/:idx", s.handleMCPBoundary)
	s.Echo.Any("/v1/mcp-gateway/:workspace/:agent/:serverID", s.handleMCPGateway)

	// Lazy getter for KeyStore (set after route registration in main.go).
	keyStoreGetter := func() *apikeys.Store { return s.KeyStore }
	userStoreGetter := func() *users.Store { return s.UserStore }

	// Authenticated endpoints.
	if s.Config.LoginKeyDefault {
		slog.Warn("the login key is the default \"admin\"; set WEAVE_LOGIN_KEY for this deployment")
	}
	auth := authenticatedRouteGroup(s.Echo, "/v1", s.mcpSessionMiddleware(), s.loginKeyMiddleware(), AuthMiddleware(s.Config.JWTSecret, keyStoreGetter, userStoreGetter))
	auth.GET("/integrations/feishu/binding", s.handleFeishuBinding)
	auth.POST("/integrations/feishu/link-code", s.handleFeishuLinkCode)
	auth.DELETE("/integrations/feishu/binding", s.handleFeishuUnlink)
	auth.POST("/mcp", s.handleMCPEntry)
	auth.POST("/devices/enrollments", s.handleCreateDeviceEnrollment)
	auth.GET("/devices", s.handleListDevices)
	auth.POST("/devices/:id/sessions", s.handleOpenDeviceSession)
	auth.GET("/device-sessions/:id", s.handleReadDeviceSession)
	auth.DELETE("/device-sessions/:id", s.handleCloseDeviceSession)
	auth.POST("/device-sessions/:id/ops", s.handleDeviceOperation)
	s.registerMCPSignIn(auth)
	consoleAccess := RequireAtLeast(roleDeveloper)

	// Developer and operator console.
	auth.GET("/admin/organization-connection", s.handleOrganizationConnection, RequireAtLeast(roleAdmin))
	auth.GET("/admin/session", s.handleGetAdminSession, consoleAccess)
	auth.GET("/admin/tasks", s.handleListAdminTasks, consoleAccess)
	auth.GET("/admin/tasks/:id", s.handleListAdminTasks, consoleAccess)
	auth.POST("/admin/tasks", s.handleSubmitAdminTask, consoleAccess)
	auth.GET("/admin/tasks/:id/evidence", s.handleExportAdminTaskEvidence, consoleAccess)
	auth.GET("/admin/tasks/:id/outputs/:output", s.handleGetAdminTaskOutput, consoleAccess)
	auth.GET("/admin/tasks/:id/logs", s.handleGetAdminTaskLogs, consoleAccess)
	auth.GET("/admin/tasks/:id/stream", s.handleAdminTaskStream, consoleAccess)
	auth.GET("/admin/tasks/:id/queue", s.handleGetAdminTaskQueue, consoleAccess)
	auth.GET("/admin/metrics", s.handleGetAdminMetrics, consoleAccess)

	// Capability contract endpoints: each published capability runs one
	// already-published workflow.
	// The deployment login key signs in as the administrator here as on every
	// other route; an application credential never matches it and keeps only
	// its own scopes, which never include manage.
	capabilityAPI := authenticatedRouteGroup(s.Echo, "/v1", s.loginKeyMiddleware(), s.capabilityAuthentication())
	capabilityAPI.POST("/capabilities/drafts", s.handleSaveCapabilityDraft, requireCapabilityAccess("manage"))
	capabilityAPI.GET("/capabilities/drafts", s.handleListCapabilityDrafts, requireCapabilityAccess("manage"))
	capabilityAPI.POST("/capabilities/:capabilityID/versions/:revision/publish", s.handlePublishCapability, requireCapabilityAccess("manage"))
	capabilityAPI.POST("/capabilities/:capabilityID/versions/:revision/invocations", s.handleInvokeCapability, requireCapabilityAccess("invoke"))
	capabilityAPI.GET("/capability-invocations", s.handleListCapabilityInvocations, requireCapabilityAccess("read"))
	capabilityAPI.GET("/invocations/:invocationID", s.handleGetCapabilityInvocation, requireCapabilityAccess("read"))
	capabilityAPI.GET("/invocations/:invocationID/events", s.handleGetCapabilityInvocationEvents, requireCapabilityAccess("read"))
	capabilityAPI.POST("/invocations/:invocationID/cancel", s.handleCancelCapabilityInvocation, requireCapabilityAccess("cancel"))
	capabilityAPI.GET("/capability-quotas", s.handleCapabilityQuota, requireCapabilityAccess("manage"))
	capabilityAPI.PUT("/capability-quotas", s.handleCapabilityQuota, requireCapabilityAccess("manage"))
	capabilityAPI.GET("/capability-apps", s.handleCapabilityApps, requireCapabilityAccess("manage"))
	capabilityAPI.POST("/capability-apps/actions", s.handleCapabilityAppAction, requireCapabilityAccess("manage"))

	// Current identity self-read supports installation and operator checks.
	auth.GET("/auth/me", s.handleMe)

	// API Key management (admin only).
	auth.POST("/auth/api-keys", s.handleCreateAPIKey, RequireAtLeast(roleAdmin))
	auth.GET("/auth/api-keys", s.handleListAPIKeys, RequireAtLeast(roleAdmin))
	auth.DELETE("/auth/api-keys/:id", s.handleDeleteAPIKey, RequireAtLeast(roleAdmin))

	// Organization.
	auth.GET("/deliverables", s.handleListFinalDeliverables)
	auth.GET("/deliverables/:id", s.handleGetFinalDeliverable)
	auth.GET("/deliverables/:id/content", s.handleDownloadFinalDeliverable)
	auth.GET("/teams", s.handleListTeams)
	auth.POST("/teams", s.handleCreateTeam, RequireAtLeast(roleDeveloper))
	auth.GET("/teams/:id", s.handleGetTeam)
	auth.GET("/teams/:id/members/:agent/config-draft", s.handleGetTeamMemberConfigDraft, RequireAtLeast(roleDeveloper))
	auth.PUT("/teams/:id/members/:agent/config-draft", s.handlePutTeamMemberConfigDraft, RequireAtLeast(roleDeveloper))
	auth.POST("/teams/:id/members/:agent/config-draft/apply", s.handleApplyTeamMemberConfigDraft, RequireAtLeast(roleDeveloper))
	auth.GET("/development/business-capabilities", s.handleGetBusinessCapabilities, RequireAtLeast(roleDeveloper))
	auth.POST("/development/business-capabilities/refresh", s.handleRefreshBusinessCapabilities, RequireAtLeast(roleDeveloper))
	auth.GET("/integrations/feishu/app", s.handleGetFeishuApp, RequireAtLeast(roleDeveloper))
	auth.PUT("/integrations/feishu/app", s.handlePutFeishuApp, RequireAtLeast(roleAdmin))
	auth.DELETE("/integrations/feishu/app", s.handleDeleteFeishuApp, RequireAtLeast(roleAdmin))
	auth.POST("/integrations/feishu/app/check", s.handleCheckFeishuApp, RequireAtLeast(roleAdmin))
	auth.GET("/teams/:id/feishu-access", s.handleGetFeishuTeamAccess, RequireAtLeast(roleDeveloper))
	auth.PUT("/teams/:id/feishu-access", s.handlePutFeishuTeamAccess, RequireAtLeast(roleDeveloper))
	auth.GET("/teams/:id/development", s.handleGetTeamDevelopment, RequireAtLeast(roleDeveloper))
	auth.PUT("/teams/:id/development", s.handleSaveTeamDevelopment, RequireAtLeast(roleDeveloper))
	auth.POST("/teams/:id/development/rebase", s.handleRebaseTeamDevelopment, RequireAtLeast(roleDeveloper))
	auth.POST("/teams/:id/development/operations", s.handleApplyTeamDevelopmentOperations, RequireAtLeast(roleDeveloper))
	auth.POST("/development/teams", s.handleCreateDevelopmentTeam, RequireAtLeast(roleDeveloper))
	auth.POST("/development/teams/import", s.handleImportDevelopmentTeam, RequireAtLeast(roleDeveloper))
	auth.GET("/teams/:id/development/export", s.handleExportTeamDevelopment, RequireAtLeast(roleDeveloper))
	auth.GET("/development/operations", s.handleListDevelopmentOperations, RequireAtLeast(roleDeveloper))
	auth.GET("/development/operations/:topic", s.handleGetDevelopmentOperationTopic, RequireAtLeast(roleDeveloper))
	auth.GET("/development/workspace-operations", s.handleListWorkspaceOperations)
	auth.GET("/development/workspace-operations/:topic", s.handleGetWorkspaceOperationTopic)
	auth.POST("/development/workspace-operations", s.handleApplyWorkspaceOperation)
	auth.POST("/teams/:id/development/trials", s.handleTrialTeamDevelopment, RequireAtLeast(roleDeveloper))
	auth.POST("/teams/:id/development/publish", s.handlePublishTeamDevelopment, RequireAtLeast(roleDeveloper))
	auth.GET("/teams/:id/development/trials/:request/input", s.handleDevelopmentTrialInput, RequireAtLeast(roleDeveloper))
	auth.GET("/runs/:id/trial-outputs", s.handleListDevelopmentTrialOutputs, RequireAtLeast(roleDeveloper))
	auth.GET("/runs/:id/trial-outputs/:output", s.handleGetDevelopmentTrialOutput, RequireAtLeast(roleDeveloper))
	auth.DELETE("/teams/:id/development/team", s.handleArchiveDevelopmentTeam, RequireAtLeast(roleDeveloper))
	auth.GET("/development/member-tools", s.handleDevelopmentMemberTools, RequireAtLeast(roleDeveloper))
	auth.GET("/development/model-catalog", s.handleDevelopmentModelCatalog, RequireAtLeast(roleDeveloper))
	auth.GET("/development/model-providers", s.handleListDevelopmentModelProviders, RequireAtLeast(roleDeveloper))
	auth.PUT("/development/model-providers/:id", s.handlePutDevelopmentModelProvider, RequireAtLeast(roleDeveloper))
	auth.POST("/development/model-providers/endpoint", s.handleResolveDevelopmentModelEndpoint, RequireAtLeast(roleDeveloper))
	auth.POST("/development/model-providers/:id/probe", s.handleProbeDevelopmentModelProvider, RequireAtLeast(roleDeveloper))
	auth.DELETE("/development/model-providers/:id", s.handleDeleteDevelopmentModelProvider, RequireAtLeast(roleAdmin))
	auth.GET("/teams/:id/dispatch-rules", s.handleGetTeamDispatchRules)
	auth.PUT("/teams/:id/dispatch-rules", s.handlePutTeamDispatchRules, RequireAtLeast(roleAdmin))
	auth.POST("/teams/:id/dispatch", s.handleDispatchTeam)
	auth.POST("/workbench/dispatch-inputs/prepare", s.handlePrepareDispatchInput)
	auth.POST("/workbench/dispatch-inputs/:input_revision_id/authorization", s.handleRenewDispatchAuthorization)
	auth.POST("/workbench/dispatch-inputs", s.handleRegisterDispatchInput)
	auth.POST("/workbench/dispatch-inputs/:input_revision_id/reconcile", s.handleReconcileDispatchInput)
	auth.PUT("/teams/:id/roster", s.handleUpdateTeamRoster, RequireAtLeast(roleAdmin))
	auth.GET("/teams/:id/workers/:worker/revocation-impact", s.handleGetTeamWorkerRevocationImpact)
	auth.PUT("/teams/:id", s.handleRenameTeam, RequireAtLeast(roleAdmin))
	auth.PUT("/teams/:id/profile", s.handleUpdateTeamProfile, RequireAtLeast(roleDeveloper))
	auth.POST("/teams/:id/workers", s.handleCreateTeamWorker, RequireAtLeast(roleDeveloper))
	auth.DELETE("/teams/:id/workers/:worker", s.handleDeleteTeamWorker, RequireAtLeast(roleDeveloper))
	auth.DELETE("/teams/:id", s.handleDeleteTeam, RequireAtLeast(roleAdmin))
	auth.POST("/teams/:id/workflows", s.handleCreateWorkflow, RequireAtLeast(roleDeveloper))
	auth.GET("/teams/:id/workflows", s.handleListTeamWorkflows)
	auth.GET("/workflows/:id", s.handleGetWorkflow)
	auth.GET("/workflows/:id/versions/:version", s.handleGetWorkflowVersion)
	auth.POST("/workflows/:id/drafts", s.handleCreateWorkflowDraft, RequireAtLeast(roleDeveloper))
	auth.PUT("/workflows/:id/versions/:version", s.handleUpdateWorkflowDraft, RequireAtLeast(roleDeveloper))
	auth.POST("/workflows/:id/versions/:version/publish", s.handlePublishWorkflowVersion, RequireAtLeast(roleDeveloper))
	auth.POST("/workflows/:id/versions/:version/validate", s.handleValidateWorkflowVersion)
	auth.PUT("/workflows/:id/versions/:version/admission", s.handlePutWorkflowAdmission, RequireAtLeast(roleAdmin))
	auth.GET("/workflows/:id/versions/:version/admission/audit", s.handleListWorkflowAdmissionAudit, RequireAtLeast(roleAdmin))
	auth.DELETE("/workflows/:id", s.handleArchiveWorkflow, RequireAtLeast(roleDeveloper))
	auth.GET("/workflows/:id/versions/:version/dependencies", s.handleGetWorkflowVersionDependencies)
	auth.GET("/workflows/:id/versions/:version/admission", s.handleGetWorkflowVersionAdmission)

	// Revisioned outbound delivery targets (admin only).
	// Delivery-target management is closed (W10, close first then delete); the
	// handlers stay until the deletion trigger in the remediation plan is met.

	// Remote engine runtimes.
	auth.GET("/agent-execution-settings", s.handleListAgentExecutionSettings)
	auth.GET("/runtimes", s.handleListRuntimes)
	auth.POST("/runtimes", s.handleCreateRuntime, RequireAtLeast(roleAdmin))
	auth.GET("/runtimes/:id", s.handleGetRuntime)
	auth.PUT("/runtimes/:id", s.handleRenameRuntime, RequireAtLeast(roleAdmin))
	auth.POST("/runtimes/:id/token", s.handleRotateRuntimeToken, RequireAtLeast(roleAdmin))
	auth.PUT("/runtimes/:id/paused", s.handleSetRuntimePaused, RequireAtLeast(roleAdmin))
	auth.POST("/runtimes/:id/probe", s.handleProbeRuntime, RequireAtLeast(roleAdmin))
	auth.DELETE("/runtimes/:id", s.handleDeleteRuntime, RequireAtLeast(roleAdmin))
	runtimeAPI := authenticatedRouteGroup(s.Echo, "/v1/runtime", s.runtimeAuthMiddleware())
	runtimeAPI.POST("/hello", s.handleRuntimeHello)
	runtimeAPI.POST("/heartbeat", s.handleRuntimeHeartbeat)
	runtimeAPI.POST("/claim", s.handleRuntimeClaim)
	runtimeAPI.POST("/tasks/:id/renew", s.handleRuntimeTaskRenew)
	runtimeAPI.POST("/tasks/:id/device/next", s.handleDeviceNext)
	runtimeAPI.POST("/tasks/:id/device/ops/:op", s.handleDeviceOperationResult)
	runtimeAPI.POST("/tasks/:id/device/end", s.handleDeviceSessionEnd)
	runtimeAPI.POST("/tasks/:id/complete", s.handleRuntimeTaskComplete)
	runtimeAPI.POST("/tasks/:id/stopped", s.handleRuntimeTaskStopped)
	runtimeAPI.POST("/tasks/:id/events", s.handleRuntimeTaskEvents)
	runtimeAPI.POST("/tasks/:id/logs", s.handleRuntimeTaskLogs)
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
	auth.GET("/agents", s.handleListAgents)
	auth.GET("/agents/:name", s.handleGetAgent)
	auth.GET("/agents/:name/team-memberships", s.handleGetAgentTeamMemberships)
	auth.GET("/agents/:name/run-summary", s.handleGetAgentRunSummary)
	auth.POST("/agents", s.handleCreateAgent, RequireAtLeast(roleDeveloper))
	auth.PUT("/agents/:name", s.handleUpdateAgent, RequireAtLeast(roleDeveloper))
	auth.PUT("/agents/:name/execution", s.handleConfigureAgentExecution, RequireAtLeast(roleAdmin))
	auth.DELETE("/agents/:name", s.handleDeleteAgent, RequireAtLeast(roleDeveloper))
	auth.GET("/agents/:name/managed", s.handleListManaged)
	auth.POST("/agents/:name/managed", s.handleLinkManages, RequireAtLeast(roleAdmin))
	auth.DELETE("/agents/:name/managed/:worker", s.handleUnlinkManages, RequireAtLeast(roleAdmin))
	auth.GET("/agent-links", s.handleListAgentLinks)
	auth.POST("/agent-links", s.handleCreateAgentLink, RequireAtLeast(roleAdmin))
	auth.PATCH("/agent-links/:id", s.handleUpdateAgentLink, RequireAtLeast(roleAdmin))
	auth.DELETE("/agent-links/:id", s.handleDeleteAgentLink, RequireAtLeast(roleAdmin))
	auth.POST("/agents/cleanup-orphans", s.handleCleanupOrphanWorkers, RequireAtLeast(roleAdmin))
	auth.POST("/agents/:name/preview-prompt", s.handlePreviewPrompt)
	auth.GET("/agents/:name/topology", s.handleTopology)
	auth.POST("/agents/upload", s.handleUploadAgent, RequireAtLeast(roleDeveloper))
	auth.POST("/agents/import/preview", s.handleImportPreview, RequireAtLeast(roleDeveloper))

	// Execution MCP registry and maintenance.
	auth.GET("/mcp-servers", s.handleListMCPServers)
	auth.POST("/mcp-servers", s.handleCreateMCPServer, RequireAtLeast(roleAdmin))
	auth.GET("/mcp-servers/:id", s.handleGetMCPServer)
	auth.PUT("/mcp-servers/:id", s.handleUpdateMCPServer, RequireAtLeast(roleAdmin))
	auth.DELETE("/mcp-servers/:id", s.handleDeleteMCPServer, RequireAtLeast(roleAdmin))
	auth.POST("/mcp-servers/:id/probe", s.handleProbeMCPServer, RequireAtLeast(roleAdmin))
	auth.GET("/mcp-servers/:id/tools", s.handleGetMCPServerTools)
	auth.POST("/attachments", s.handleUploadAttachment)
	auth.GET("/attachments", s.handleListAttachments)
	auth.GET("/attachments/:id", s.handleGetAttachment)

	// Skills (reusable prompt modules).
	auth.GET("/skills", s.handleListSkills)
	auth.GET("/skills/:id", s.handleGetSkill)
	auth.POST("/skills", s.handleCreateSkill, RequireAtLeast(roleDeveloper))
	auth.PUT("/skills/:id", s.handleUpdateSkill, RequireAtLeast(roleDeveloper))
	auth.DELETE("/skills/:id", s.handleDeleteSkill, RequireAtLeast(roleDeveloper))
	auth.POST("/skills/:id/import-legacy", s.handleImportLegacySkill, RequireAtLeast(roleAdmin))

	// Human tasks.
	auth.GET("/human-tasks", s.handleListHumanTasks)
	auth.GET("/human-tasks/:run_id", s.handleGetHumanTask)
	auth.POST("/human-tasks/:run_id/complete", s.handleCompleteHumanTask)

	// Runs.
	auth.GET("/runs", s.handleListRuns)
	auth.GET("/runs/:id/workbench-context", s.handleGetWorkbenchRunContext)
	auth.POST("/workbench/runs/lookup", s.handleLookupWorkbenchRuns)
	auth.GET("/runs/:id", s.handleGetRun)
	auth.GET("/runs/:id/activity", s.handleGetRunActivity)
	auth.GET("/runs/:id/delivery", s.handleGetRunDelivery)
	auth.POST("/runs/:id/delivery/recheck", s.handleRecheckRunDelivery)
	auth.GET("/runs/:id/delivery/verifications/:verification_id", s.handleGetRunVerification)
	auth.POST("/runs/:id/stop", s.handleStopRun)
	auth.POST("/runs/:id/stages/:node_id/retry", s.handleRetryRunStage)
	auth.GET("/runs/:id/corrections", s.handleListRunCorrections)
	auth.POST("/runs/:id/corrections", s.handleRequestRunCorrection)
	auth.POST("/runs/:id/corrections/:correction_id/confirm", s.handleConfirmRunCorrection)
	auth.GET("/runs/:id/trace", s.handleGetRunTrace)
	auth.GET("/runs/:id/state", s.handleGetRunState)
	auth.GET("/runs/:id/checkpoints", s.handleGetRunCheckpoints)

	// Usage.
	auth.GET("/usage", s.handleGetUsage)

	// Providers (LLM configuration).
	auth.GET("/providers", s.handleListProviders)
	auth.POST("/providers", s.handleAddProvider, RequireAtLeast(roleAdmin))
	auth.GET("/providers/system", s.handleListSystemProviders, RequireAtLeast(roleAdmin))
	auth.POST("/providers/system/:id/mirror", s.handleMirrorSystemProvider, RequireAtLeast(roleAdmin))
	auth.PUT("/providers/:id", s.handleUpdateProvider, RequireAtLeast(roleAdmin))
	auth.DELETE("/providers/:id", s.handleDeleteProvider, RequireAtLeast(roleAdmin))

	// Data sources (MCP server aggregation).

	// 数字员工定时上班（agent 调度）。

	// Task ledger (the legacy /jobs route paths are retained).
	auth.GET("/jobs", s.handleListJobs)
	auth.GET("/jobs/:id", s.handleGetJob)
	auth.POST("/jobs/:id/cancel", s.handleCancelJob)
}

// Start runs the HTTP server.
func (s *Server) Start() error {
	if s.modelActivityBuffer != nil {
		defer s.modelActivityBuffer.Close()
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
	if s.employeeRunEventWorker != nil {
		s.employeeRunEventWorker.Start()
		defer s.employeeRunEventWorker.Stop()
	}
	return s.Echo.Start(":" + s.Config.Port)
}

const teamRunWorkersDisableEnv = "WEAVE_TEAMRUN_WORKERS_DISABLED"

// Deployment policy pending (OQ-5). These process-local values are temporary
// worker policy and are not part of the TeamRun ABI.
const (
	temporaryTeamRunWorkerPollInterval = 2 * time.Second
	temporaryTeamRunWorkerBatchSize    = 32
	// Running local members observe durable cancellation on this heartbeat.
	// Keep detection within the ordinary caller's cancellation confirmation window.
	temporaryTeamRunTaskHeartbeat = 5 * time.Second
)

const (
	teamRunExecutorConcurrencyEnv     = "WEAVE_TEAMRUN_EXECUTOR_CONCURRENCY"
	defaultTeamRunExecutorConcurrency = 4
	maxTeamRunExecutorConcurrency     = 4096
)

// teamRunExecutorConcurrency bounds this process's durable workflow claims.
// Each claim polls and renews its own lease, so the bound also sizes the
// queue's SQL load. Unset means the default; any other value outside the
// range is an error, so the process refuses to start with a capacity nobody
// chose.
func teamRunExecutorConcurrency() (int, error) {
	raw := strings.TrimSpace(os.Getenv(teamRunExecutorConcurrencyEnv))
	if raw == "" {
		return defaultTeamRunExecutorConcurrency, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > maxTeamRunExecutorConcurrency {
		return 0, fmt.Errorf("%s=%q: want an integer in 1..%d", teamRunExecutorConcurrencyEnv, raw, maxTeamRunExecutorConcurrency)
	}
	return value, nil
}

func teamRunWorkersDisabled() bool {
	value := strings.TrimSpace(os.Getenv(teamRunWorkersDisableEnv))
	if value == "" {
		return false
	}
	disabled, err := strconv.ParseBool(value)
	return err == nil && disabled
}

// ConfigureTeamRunWorkers wires the no-session schedule executor after main
// has installed task, runtime, and credential stores. It fails only on
// invalid worker configuration.
func (s *Server) ConfigureTeamRunWorkers() error {
	pool := s.GetPool()
	if pool == nil || s.Tasks == nil || s.Snapshots == nil || s.Workflow == nil {
		return nil
	}
	concurrency, err := teamRunExecutorConcurrency()
	if err != nil {
		return err
	}
	s.employeeRunEventWorker = newEmployeeRunEventWorker(pool)
	s.employeeRunEventWorker.FeishuServer = s
	s.taskDelegationRevoker = newTaskDelegationRevoker(pool)
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
	var businessDelegations *businessaction.Store
	if key, keyErr := secret.KeyFromEnv(); keyErr == nil {
		businessDelegations = businessaction.NewStore(pool, s.Tasks, key)
		for index := range key {
			key[index] = 0
		}
	}
	materialReads := businessDelegations
	if materialReads == nil {
		materialReads = businessaction.NewStore(pool, s.Tasks, nil)
	}
	s.BusinessDelegations = businessDelegations
	runtime := &teamrun.WorkflowSerialRuntime{
		BusinessReceiptReader: deliveryverify.BusinessReceiptReader(pool),
		CredentialAuthorizer: func(ctx context.Context, run teamrun.TeamRun, task *taskqueue.Task, subject execution.Subject, ref frozen.CredentialReference) error {
			service := s.workflowCapabilityInvocations()
			if service == nil {
				return frozen.ErrCredentialSubjectDenied
			}
			return service.AuthorizeRuntimeCredential(ctx, run, task, subject, ref)
		},
		AuthorizationRetry: func(ctx context.Context, proof execution.AuthorizationRefusal) (bool, error) {
			if businessDelegations == nil {
				return false, nil
			}
			return businessDelegations.CanRetryAuthorization(ctx, proof)
		},
		Artifacts: s.WorkflowArtifacts,
		Loader: &workflow.RuntimeLoader{
			Registry: s.Descriptors, CLIExecutor: s.teamRunCLIExecutor(),
		},
		HostFactory: businessaction.Factory{Inner: workflow.NewRuntimeHostFactoryWithGateway(s.ModelGateway), Store: businessDelegations, MaterialStore: materialReads},
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
		OutputRecorder: &developmentTrialWorkflowOutputRecorder{pool: pool, fallback: s.Deliverables},
		Corrections:    correctionStore,
		Activities:     activityStore,
	}
	if os.Getenv("WEAVE_TEAMRUN_OBSERVATION_WRITES") == "buffered" {
		s.modelActivityBuffer = teamrun.NewBufferedActivityRecorder(activityStore)
		runtime.Activities = s.modelActivityBuffer
		runtime.ModelObservationOptions = workflow.ModelObservationOptions{CompactPhases: true, SlowAfter: 30 * time.Second}
	}
	memberRunner, err := loomruntime.NewMemberRunner(storeext.New(pool))
	if err != nil {
		panic(fmt.Sprintf("configure frozen member runner: %v", err))
	}
	runtime.Members = memberRunner
	if s.Config != nil && s.Config.ToolProtocolProbe != nil {
		probe := s.Config.ToolProtocolProbe
		memberRunner.ProtocolProbe = &loomruntime.ModelProtocolProbePolicy{WorkspaceID: probe.WorkspaceID, WorkflowID: probe.WorkflowID,
			RunID: probe.RunID, ExpiresAt: probe.ExpiresAt, MaxModelCalls: probe.MaxModelCalls}
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
		Executor:     executor,
		Pooled:       os.Getenv("WEAVE_TEAMRUN_EXECUTOR") == "pooled",
		DrainTimeout: teamRunDrainTimeout(),
		// Four durable claims are enough to make ordinary multi-runtime fanout
		// genuinely concurrent without introducing an unbounded worker pool.
		// Deployments with many concurrent pure-model members raise it explicitly.
		ExecutorConcurrency: concurrency,
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
		StoppedWorkflows: &teamrun.StoppedWorkflowSweeper{
			Tasks:        s.Tasks,
			Transactions: pool,
			Runs:         runStore,
			Executor:     executor,
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
	return nil
}

// teamRunCapacity reports this process's workflow execution capacity as
// configured: Running is false when this process does not run the workers
// (they are disabled or not wired), whatever other executor processes run.
type teamRunCapacity struct {
	ExecutorConcurrency int    `json:"executor_concurrency"`
	ExecutorMode        string `json:"executor_mode"`
	Running             bool   `json:"running"`
}

func (s *Server) teamRunCapacity() teamRunCapacity {
	if s.teamRunWorkers == nil {
		return teamRunCapacity{}
	}
	mode := "legacy"
	if s.teamRunWorkers.Pooled {
		mode = "pooled"
	}
	return teamRunCapacity{
		ExecutorConcurrency: s.teamRunWorkers.ExecutorConcurrency,
		ExecutorMode:        mode,
		Running:             !teamRunWorkersDisabled(),
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
	terminalSink, err := s.rootTerminalSink()
	if err != nil {
		return nil, fmt.Errorf("prepare terminal sink for sub-agent %q: %w", agentName, err)
	}
	// Prepare child without sub-agent resolution (prevent infinite recursion).
	childOpts := compiler.CompileOpts{
		Store:       s.Store,
		AgentRunner: s.compilerAgentRunner(tenant, llm),
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
