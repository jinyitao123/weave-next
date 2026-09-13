package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jinyitao123/loom/pgstore"
	"github.com/jinyitao123/weave/internal/app/api"
	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/app/attachments"
	"github.com/jinyitao123/weave/internal/app/cli"
	"github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/jinyitao123/weave/internal/app/daemon"
	"github.com/jinyitao123/weave/internal/app/deliveryverify"
	"github.com/jinyitao123/weave/internal/app/metateam"
	"github.com/jinyitao123/weave/internal/app/projects"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/app/teamevaluations"
	"github.com/jinyitao123/weave/internal/app/teamtemplates"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/kernel/fanout"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teameval"
	"github.com/jinyitao123/weave/internal/build/teamorch"
	"github.com/jinyitao123/weave/internal/kernel/audit"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/declarative"
	"github.com/jinyitao123/weave/internal/kernel/declarative/designprompt"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/memory"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/secret"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

var buildCommit = "unknown"
var buildVersion = "development"

func csvEnvOrDefault(name string, fallback []string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return append([]string(nil), fallback...)
	}
	models := make([]string, 0)
	seen := make(map[string]struct{})
	for _, value := range strings.Split(raw, ",") {
		model := strings.TrimSpace(value)
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	if len(models) == 0 {
		return append([]string(nil), fallback...)
	}
	return models
}

type teamBuildExecutionAdapter struct {
	service *teamorch.AsyncService
}

func (a teamBuildExecutionAdapter) Submit(
	ctx context.Context, workspaceID, buildRunID string,
) (api.TeamBuildExecutionSubmission, error) {
	result, err := a.service.Submit(ctx, workspaceID, buildRunID)
	if err != nil {
		return api.TeamBuildExecutionSubmission{}, err
	}
	return api.TeamBuildExecutionSubmission{
		WorkspaceID: result.WorkspaceID,
		BuildRunID:  result.BuildRunID,
		Status:      result.Status,
	}, nil
}

func (a teamBuildExecutionAdapter) Start() { a.service.Start() }
func (a teamBuildExecutionAdapter) Stop()  { a.service.Stop() }

type teamTemplateExecutionAdapter struct {
	service api.TeamBuildExecutionService
}

func (a teamTemplateExecutionAdapter) Submit(ctx context.Context, workspaceID, buildRunID string) error {
	_, err := a.service.Submit(ctx, workspaceID, buildRunID)
	return err
}

func registerFrozenDescriptors() error {
	if err := compiler.RegisterDescriptor(compiler.NewStandardFrozenDescriptor()); err != nil {
		return err
	}
	if err := compiler.RegisterDescriptor(compiler.NewStandardFrozenToolsDescriptor()); err != nil {
		return err
	}
	if err := compiler.RegisterDescriptor(compiler.NewStandardFrozenCLIToolsDescriptor()); err != nil {
		return err
	}
	return compiler.RegisterDescriptor(declarative.NewFrozenDescriptor())
}

func main() {
	if handled, exitCode := dispatchEarlyCommand(os.Args[1:], os.Stdout, os.Stderr); handled {
		os.Exit(exitCode)
	}

	if err := registerFrozenDescriptors(); err != nil {
		slog.Error("failed to register frozen graph descriptors", "error", err)
		os.Exit(1)
	}

	api.SetBuildCommit(buildCommit)
	api.SetBuildVersion(buildVersion)

	// "runtime" turns this binary into a remote runtime worker. "daemon" stays
	// as a hidden alias so existing scripts keep working.
	if len(os.Args) > 1 && (os.Args[1] == "runtime" || os.Args[1] == "daemon") {
		if err := daemon.Main(os.Args[2:]); err != nil {
			slog.Error("runtime stopped", "error", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	secretKey, err := secret.KeyFromEnv()
	if err != nil {
		slog.Error("failed to load credential encryption key", "error", err)
		os.Exit(1)
	}

	store, err := pgstore.New(cfg.DatabaseURL)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	// Run migrations.
	if err := store.Migrate(context.Background()); err != nil {
		slog.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}
	if err := db.Migrate(context.Background(), store.Pool()); err != nil {
		slog.Error("failed to run platform migrations", "error", err)
		os.Exit(1)
	}

	// Create LLM router (providers configured via API at runtime).
	defaultModel := os.Getenv("DEFAULT_MODEL")
	if defaultModel == "" {
		defaultModel = "deepseek-v4-flash"
	}
	router := llmrouter.New(defaultModel)

	// Register default providers from environment if keys are set.
	// 物理 attempt 超时：LLM_ATTEMPT_TIMEOUT_SECONDS 覆盖，缺省 900s。
	// 只约束单次模型调用，多轮工具循环
	// 的每一轮各占一次 attempt，不会整体累计超时。
	attemptTimeoutSeconds := llmrouter.DefaultAttemptTimeoutSeconds
	if raw := os.Getenv("LLM_ATTEMPT_TIMEOUT_SECONDS"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			attemptTimeoutSeconds = parsed
		} else {
			slog.Warn("invalid LLM_ATTEMPT_TIMEOUT_SECONDS, keeping default",
				"value", raw, "default_seconds", attemptTimeoutSeconds)
		}
	}
	if key := os.Getenv("DEEPSEEK_API_KEY"); key != "" {
		// DeepSeek V4 的 thinking 默认开启，但携带 tools 时必须显式禁用
		// （M1 fail-safe：thinking+tools 需回传 reasoning_content，本阶段不做
		// reasoning 连续性）。无 tools 时的默认档位可用 DEEPSEEK_THINKING_MODE
		// 覆盖（默认 "enabled"，保留无 tools 请求的 thinking 能力）。
		thinkingMode := os.Getenv("DEEPSEEK_THINKING_MODE")
		if thinkingMode == "" {
			thinkingMode = "enabled"
		}
		router.RegisterProvider(llmrouter.ProviderConfig{
			ID:                       "deepseek",
			Name:                     "DeepSeek",
			BaseURL:                  "https://api.deepseek.com",
			APIKey:                   key,
			Models:                   []string{"deepseek-v4-flash", "deepseek-v4-pro"},
			JSONObjectMode:           true, // DeepSeek doesn't support json_schema, use json_object
			ThinkingDefaultMode:      thinkingMode,
			ThinkingDisableWithTools: true,
			AttemptTimeoutSeconds:    attemptTimeoutSeconds,
		})
	}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		router.RegisterProvider(llmrouter.ProviderConfig{
			ID:                    "anthropic",
			Name:                  "Anthropic",
			BaseURL:               "https://api.anthropic.com/v1",
			APIKey:                key,
			Models:                []string{"claude-sonnet-4-20250514"},
			AttemptTimeoutSeconds: attemptTimeoutSeconds,
		})
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		baseURL := os.Getenv("OPENAI_BASE_URL")
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		router.RegisterProvider(llmrouter.ProviderConfig{
			ID:      "openai",
			Name:    "OpenAI",
			BaseURL: baseURL,
			APIKey:  key,
			Models: csvEnvOrDefault("OPENAI_MODELS", []string{
				"gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano",
				"gpt-5.2", "gpt-5.1-codex", "gpt-5-codex",
				"gpt-5.3-codex", "gpt-5.2-codex", "codex-mini-latest",
			}),
			AttemptTimeoutSeconds: attemptTimeoutSeconds,
		})
	}

	// Register custom graph types.
	declarative.Register()

	// The env-provider router above is the system namespace; workspace
	// providers are resolved per request through the resolver.
	models := llmrouter.NewResolver(router)
	srv := api.NewServer(cfg, store, models)
	descriptors := compiler.NewDescriptorRegistry()
	if err := descriptors.Register(compiler.NewStandardFrozenDescriptor()); err != nil {
		slog.Error("failed to register server standard graph descriptor", "error", err)
		os.Exit(1)
	}
	if err := descriptors.Register(compiler.NewStandardFrozenToolsDescriptor()); err != nil {
		slog.Error("failed to register server standard tools graph descriptor", "error", err)
		os.Exit(1)
	}
	if err := descriptors.Register(compiler.NewStandardFrozenCLIToolsDescriptor()); err != nil {
		slog.Error("failed to register server standard tools graph descriptor", "error", err)
		os.Exit(1)
	}
	if err := descriptors.Register(declarative.NewFrozenDescriptor()); err != nil {
		slog.Error("failed to register server declarative graph descriptor", "error", err)
		os.Exit(1)
	}
	srv.Descriptors = descriptors
	srv.SystemProviders = router

	// Seed system agents.
	designprompt.EnsureDesigner(srv.Registry, "default")

	// Initialize user and API key stores if PG pool is available.
	if pool := srv.GetPool(); pool != nil {
		srv.OrgStore = org.NewStore(pool)
		if srv.TeamBuild != nil && srv.Registry != nil && srv.Workflow != nil {
			srv.TeamBuild.SetBaselineSources(srv.OrgStore, srv.Registry, srv.Workflow)
		}
		designprompt.EnsureDesignStudio(srv.Registry, srv.OrgStore, "default")
		if err := metateam.EnsureMetaTeamIfEnabled(
			context.Background(), srv.Registry, srv.OrgStore, "default", cfg.MetaTeamEnabled,
		); err != nil {
			slog.Warn("failed to seed meta team", "error", err)
		}

		userStore := users.NewStore(pool)
		srv.UserStore = userStore

		// Workspace-level memory: EMBEDDER_URL is the runtime system fallback;
		// workspace embedders are resolved per request through the resolver.
		var systemEmb *memory.EmbedderSettings
		if cfg.EmbedderURL != "" {
			systemEmb = &memory.EmbedderSettings{
				BaseURL:   cfg.EmbedderURL,
				APIKey:    cfg.EmbedderKey,
				Model:     cfg.EmbedderModel,
				Dimension: cfg.EmbedderDimension,
			}
		}
		srv.Embedders = memory.NewEmbedderResolver(pool, systemEmb)

		// Seed admin user from environment variables (only when missing,
		// so password changes survive container rebuilds).
		if cfg.AdminUser != "" && cfg.AdminPass != "" {
			if _, err := userStore.GetByUsername(context.Background(), "default", cfg.AdminUser); err == nil {
				slog.Info("admin user already exists, skipping seed", "username", cfg.AdminUser)
			} else if _, err := userStore.Upsert(context.Background(), "default", cfg.AdminUser, cfg.AdminPass, cfg.AdminUser, "admin"); err != nil {
				slog.Warn("failed to seed admin user", "error", err)
			} else {
				slog.Info("admin user seeded", "username", cfg.AdminUser)
			}
		}

		srv.KeyStore = apikeys.NewStore(pool)
		srv.Skills = skills.New(pool)
		srv.Runtimes = runtimes.NewStore(pool)
		srv.Projects = projects.New(pool, projects.RealClock{})
		srv.Attachments = attachments.New(pool)
		srv.Deliverables = deliveryverify.NewStore(pool)
		srv.Audit = audit.New(pool, audit.RealClock{})
		srv.Conversations = conversation.New(pool, conversation.RealClock{})
		srv.Credentials = credentials.New(pool, secretKey)
		srv.DeliveryTargets = delivery.New(pool, secretKey)
		models.SetSource(srv.Credentials)
		srv.Embedders.SetSource(api.CredentialEmbedderSource{Store: srv.Credentials})
		srv.MCPRegistry = mcpregistry.New(pool, secretKey)

		// Upgrade diagnostics: name the agents/workspaces still relying on the
		// old "any workspace's credentials serve everyone" behavior.
		go srv.WarnLegacyCrossWorkspaceConfig(context.Background())
	}

	// Initialize the durable task queue if PG pool is available.
	if pool := srv.GetPool(); pool != nil {
		taskStore := taskqueue.New(pool, taskqueue.RealClock{}, 60*time.Second)
		srv.Tasks = taskStore
		srv.Fanout = fanout.New(pool, fanout.RealClock{})
		srv.FanoutReconciler = fanout.NewReconciler(
			srv.Fanout, taskStore, srv.Conversations,
		)
		if recovered, err := taskStore.RecoverStale(context.Background()); err != nil {
			slog.Error("failed to recover stale tasks at startup", "error", err)
		} else if recovered > 0 {
			slog.Info("recovered stale tasks at startup", "count", recovered)
		}

		recoveryCtx, stopRecovery := context.WithCancel(context.Background())
		defer stopRecovery()
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-recoveryCtx.Done():
					return
				case <-ticker.C:
					recovered, err := taskStore.RecoverStale(recoveryCtx)
					if err != nil {
						slog.Error("failed to recover stale tasks", "error", err)
					} else if recovered > 0 {
						slog.Info("recovered stale tasks", "count", recovered)
					}
					if err := srv.FanoutReconciler.SweepDeadlines(recoveryCtx, time.Now()); err != nil {
						slog.Error("failed to sweep fan-out deadlines", "error", err)
					}
				}
			}
		}()

		srv.TaskWorker = taskqueue.NewWorker(taskStore, srv, 4)
		srv.TaskWorker.SetOnLegTerminal(func(ctx context.Context, workspaceID, groupID string) {
			_ = srv.FanoutReconciler.RefreshCard(ctx, workspaceID, groupID)
			_ = srv.FanoutReconciler.ReconcileGroup(ctx, workspaceID, groupID, false)
		})
		srv.TaskWorker.Start()
		defer srv.TaskWorker.Stop()
	}

	// Initialize schedule store if PG pool is available.
	if pool := srv.GetPool(); pool != nil {
		srv.AgentSchedules = schedule.New(pool, schedule.RealClock{})
	}

	srv.ConfigureTeamRunWorkers()

	// The meta-team round controller is a platform service, not a test-only
	// helper or a client-side conversation convention. It reuses the same
	// stores, routed LLM, candidate runtime, and role-scoped teamforge tools as
	// the rest of the server. Missing optional secure stores keep the service
	// unavailable without weakening its dependency checks.
	if phases, err := teamconstruction.NewPhases(teamconstruction.Dependencies{
		Pool: store.Pool(), Store: store, Build: srv.TeamBuild,
		Agents: srv.Registry, TeamWorkers: srv.TeamWorkers, Teams: srv.OrgStore,
		Workflows: srv.Workflow, MCPs: srv.MCPRegistry, Providers: srv.Credentials,
		Runtimes: srv.Runtimes, Tasks: srv.Tasks,
		Deliverables: srv.Deliverables, Snapshots: srv.Snapshots, Audit: srv.Audit,
		Delivery: srv.DeliveryTargets, Skills: srv.Skills, Schedules: srv.AgentSchedules,
		Descriptors: srv.Descriptors, Fanout: srv.Fanout, Drafts: srv.TeamForgeDrafts,
		CLIExecutor: srv.RemoteExec,
		LLMResolver: models,
	}); err != nil {
		slog.Warn("team build orchestrator unavailable", "error", err)
	} else {
		controller := teamorch.NewController(srv.TeamBuild, phases, nil)
		controller.RevisionPlanner = phases
		synchronous := teamorch.NewService(
			controller, phases,
		)
		queue := teamorch.NewExecutionQueue(store.Pool(), srv.TeamBuild, nil)
		async := teamorch.NewAsyncService(queue, synchronous)

		srv.TeamBuildOrchestrator = teamBuildExecutionAdapter{
			service: async,
		}
		srv.TeamTemplates = teamtemplates.New(
			teamtemplates.NewPGIdempotencyStore(srv.Pool),
			srv.TeamBuild,
			teamTemplateExecutionAdapter{service: srv.TeamBuildOrchestrator},
			teamtemplates.Options{Catalog: teamtemplates.NewStaticCatalog(), DefaultModel: defaultModel, RuntimeSelector: srv.Runtimes, Policy: teambuild.TemplateAuthorizationPolicy{
				AutoBudgetThresholdUSD: cfg.TemplateAutoMaxCostUSD,
				DailyBudgetUSD:         cfg.TemplateDailyBudgetUSD,
				MonthlyBudgetUSD:       cfg.TemplateMonthlyBudgetUSD,
				MaxConcurrent:          cfg.TemplateMaxConcurrent,
			}},
		)
		srv.TeamEvaluations = teamevaluations.New(
			teamevaluations.NewPGIdempotencyStore(srv.Pool),
			srv.TeamBuild,
			teamTemplateExecutionAdapter{service: srv.TeamBuildOrchestrator},
			teamevaluations.Options{
				OrgStore: srv.OrgStore, Registry: srv.Registry, Workflows: srv.Workflow,
				DeclarativeValidator: func(
					ctx context.Context,
					workspaceID, teamID string,
					trigger machine.TriggerConfig,
					graph machine.GraphDefinition,
				) (machine.Report, error) {
					return teameval.ValidateWorkflowForTeam(ctx, teameval.WorkflowValidateDeps{
						Teams: srv.OrgStore, Roster: srv.TeamWorkers,
						Agents: srv.Registry, Workflows: srv.Workflow,
					}, workspaceID, teamID, trigger, graph)
				},
			},
		)
	}

	// Every due team workflow schedule is first written to the durable task ledger. The
	// worker then executes it with the same lease and restart recovery as chats.
	if srv.AgentSchedules != nil && srv.Tasks != nil {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				if err := srv.SweepSchedules(context.Background(), time.Now()); err != nil {
					slog.Error("failed to sweep team workflow schedules", "error", err)
				}
			}
		}()
	}

	// Graceful shutdown.
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		slog.Info("shutting down...")
		srv.Echo.Close()
	}()

	if cfg.DevMode {
		slog.Info("dev mode enabled — /v1/auth/token endpoint active")
	}
	slog.Info("starting weave server", "port", cfg.Port)
	if err := srv.Start(); err != nil {
		slog.Info("server stopped", "reason", err)
	}
}

func dispatchEarlyCommand(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) > 0 && args[0] == "bootstrap" {
		return true, runBootstrapCommand(args[1:], stdout, stderr)
	}
	return cli.Dispatch(args, stdout, stderr)
}
