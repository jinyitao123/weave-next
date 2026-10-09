package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
	"github.com/jinyitao123/weave/internal/kernel/runtimeagent"
	"github.com/jinyitao123/weave/internal/kernel/runtimehost"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

const (
	defaultConcurrency      = 2
	defaultClaimWait        = 25
	defaultTimeoutSeconds   = 600
	codexChatGPTAuthMode    = "chatgpt"
	claudeOAuthAuthMode     = "oauth"
	codexLoginProbeTimeout  = 10 * time.Second
	claudeLoginProbeTimeout = 10 * time.Second
)

type runEngineFunc func(context.Context, string, engine.RunSpec) (engine.RunResult, error)

type daemonConfig struct {
	server             string
	token              string
	workspacesRoot     string
	concurrency        int
	httpClient         *http.Client
	runEngine          runEngineFunc
	detectedEngines    []string
	engineCapabilities []runtimeprotocol.EngineCapability
	renewInterval      time.Duration
	heartbeatInterval  time.Duration
	minBackoff         time.Duration
	maxBackoff         time.Duration
}

type service struct {
	subjectGuard       *subjectGuard
	credentials        ClaimCredentialResolver
	client             *runtimeClient
	server             string
	workspacesRoot     string
	concurrency        int
	runEngine          runEngineFunc
	detectedEngines    []string
	engineCapabilities []runtimeprotocol.EngineCapability
	// redetect is set when engines come from this host rather than a test
	// fixture; capabilityMu guards the engine fields it refreshes.
	redetect          bool
	publicEventsReady bool
	capabilityMu      sync.RWMutex
	claimWaitSeconds  int
	renewInterval     time.Duration
	heartbeatInterval time.Duration
	minBackoff        time.Duration
	maxBackoff        time.Duration
	active            atomic.Int32
	publicSpool       *publicSpool
	resultSpool       *resultSpool
}

// Main parses daemon flags and runs until SIGINT or SIGTERM.
func Main(args []string) error {
	cfg, err := parseFlags(args)
	if err != nil {
		return err
	}
	d, err := newDaemon(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return d.Run(ctx)
}

func parseFlags(args []string) (daemonConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return daemonConfig{}, fmt.Errorf("runtime: resolve home directory: %w", err)
	}
	cfg := daemonConfig{}
	flags := flag.NewFlagSet("weave runtime", flag.ContinueOnError)
	flags.StringVar(&cfg.server, "server", "", "Weave server base URL (required)")
	flags.StringVar(&cfg.token, "runtime-token", "", "runtime bearer token (or WEAVE_RUNTIME_TOKEN)")
	flags.StringVar(&cfg.workspacesRoot, "workspaces-root", filepath.Join(home, ".weave", "runtime-workspaces"), "runtime workspace root")
	flags.IntVar(&cfg.concurrency, "concurrency", defaultConcurrency, "number of concurrent task claims")
	if err := flags.Parse(args); err != nil {
		return daemonConfig{}, err
	}
	if cfg.server == "" {
		return daemonConfig{}, errors.New("runtime: --server is required")
	}
	if cfg.token == "" {
		cfg.token = os.Getenv("WEAVE_RUNTIME_TOKEN")
	}
	if cfg.token == "" {
		return daemonConfig{}, errors.New("runtime: --runtime-token or WEAVE_RUNTIME_TOKEN is required")
	}
	if cfg.concurrency < 1 {
		return daemonConfig{}, errors.New("runtime: --concurrency must be at least 1")
	}
	return cfg, nil
}

func newDaemon(cfg daemonConfig) (*service, error) {
	client, err := newRuntimeClient(cfg.server, cfg.token, cfg.httpClient)
	if err != nil {
		return nil, err
	}
	if cfg.workspacesRoot == "" {
		return nil, errors.New("runtime: workspace root is required")
	}
	if cfg.concurrency < 1 {
		return nil, errors.New("runtime: concurrency must be at least 1")
	}
	if cfg.runEngine == nil {
		cfg.runEngine = runEngine
	}
	redetect := cfg.detectedEngines == nil
	if redetect {
		cfg.detectedEngines = detectEngines()
		cfg.engineCapabilities = detectEngineCapabilities(context.Background(), cfg.detectedEngines)
	}
	if cfg.renewInterval <= 0 {
		cfg.renewInterval = 20 * time.Second
	}
	if cfg.heartbeatInterval <= 0 {
		cfg.heartbeatInterval = 30 * time.Second
	}
	if cfg.minBackoff <= 0 {
		cfg.minBackoff = time.Second
	}
	if cfg.maxBackoff <= 0 {
		cfg.maxBackoff = 30 * time.Second
	}
	if cfg.maxBackoff < cfg.minBackoff {
		cfg.maxBackoff = cfg.minBackoff
	}
	spool, spoolErr := newPublicSpool(cfg.workspacesRoot, cfg.token)
	if spoolErr != nil {
		slog.Warn("runtime public progress unavailable; using completion updates", "error", spoolErr)
	}
	markPublicEvents(cfg.engineCapabilities, spoolErr == nil)
	results, err := newResultSpool(cfg.workspacesRoot, cfg.token)
	if err != nil {
		return nil, fmt.Errorf("initialize durable runtime results: %w", err)
	}
	return &service{
		publicSpool:        spool,
		resultSpool:        results,
		client:             client,
		server:             cfg.server,
		workspacesRoot:     cfg.workspacesRoot,
		concurrency:        cfg.concurrency,
		runEngine:          cfg.runEngine,
		detectedEngines:    cfg.detectedEngines,
		engineCapabilities: cfg.engineCapabilities,
		redetect:           redetect,
		publicEventsReady:  spoolErr == nil,
		claimWaitSeconds:   defaultClaimWait,
		renewInterval:      cfg.renewInterval,
		heartbeatInterval:  cfg.heartbeatInterval,
		minBackoff:         cfg.minBackoff,
		maxBackoff:         cfg.maxBackoff,
	}, nil
}

func (d *service) Run(ctx context.Context) error {
	if !d.helloUntilConnected(ctx) {
		return nil
	}

	var workers sync.WaitGroup
	workers.Add(d.concurrency + 3)
	go func() { defer workers.Done(); d.resultRecoveryLoop(ctx) }()
	go func() { defer workers.Done(); d.publicEventsLoop(ctx) }()
	go func() {
		defer workers.Done()
		d.heartbeatLoop(ctx)
	}()
	for range d.concurrency {
		go func() {
			defer workers.Done()
			d.claimLoop(ctx)
		}()
	}
	<-ctx.Done()
	workers.Wait()
	return nil
}

func (d *service) helloUntilConnected(ctx context.Context) bool {
	backoff := newBackoff(d.minBackoff, d.maxBackoff)
	for {
		engines, capabilities := d.engines()
		err := d.client.hello(ctx, engines, capabilities, d.concurrency)
		if err == nil {
			slog.Info("runtime daemon connected", "server", d.server, "engines", engines)
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		slog.Warn("runtime hello failed; retrying", "error", err)
		if !waitFor(ctx, backoff.next()) {
			return false
		}
	}
}

func (d *service) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(d.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.heartbeatUntilConnected(ctx)
		}
	}
}

func (d *service) heartbeatUntilConnected(ctx context.Context) {
	backoff := newBackoff(d.minBackoff, d.maxBackoff)
	for {
		if probe, err := d.client.heartbeat(ctx, int(d.active.Load())); err == nil {
			if probe {
				d.probe(ctx)
			}
			return
		} else if ctx.Err() == nil {
			slog.Warn("runtime heartbeat failed; retrying", "error", err)
		}
		if !waitFor(ctx, backoff.next()) {
			return
		}
	}
}

// probe re-detects the installed engines and reports them with a fresh
// hello, which also clears the request on the server.
func (d *service) probe(ctx context.Context) {
	if d.redetect {
		engines := detectEngines()
		capabilities := detectEngineCapabilities(ctx, engines)
		markPublicEvents(capabilities, d.publicEventsReady)
		d.capabilityMu.Lock()
		d.detectedEngines, d.engineCapabilities = engines, capabilities
		d.capabilityMu.Unlock()
	}
	engines, capabilities := d.engines()
	if err := d.client.hello(ctx, engines, capabilities, d.concurrency); err != nil {
		slog.Warn("runtime probe report failed", "error", err)
		return
	}
	slog.Info("runtime engines re-detected", "engines", engines)
}

func (d *service) engines() ([]string, []runtimeprotocol.EngineCapability) {
	d.capabilityMu.RLock()
	defer d.capabilityMu.RUnlock()
	return append([]string(nil), d.detectedEngines...), append([]runtimeprotocol.EngineCapability(nil), d.engineCapabilities...)
}

func markPublicEvents(capabilities []runtimeprotocol.EngineCapability, ready bool) {
	for index := range capabilities {
		capabilities[index].PublicEvents = engine.PublishesPublicEvents(capabilities[index].Engine) && ready
	}
}

func (d *service) claimLoop(ctx context.Context) {
	backoff := newBackoff(d.minBackoff, d.maxBackoff)
	for ctx.Err() == nil {
		task, err := d.client.claim(ctx, d.claimWaitSeconds)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("runtime claim failed; retrying", "error", err)
			if !waitFor(ctx, backoff.next()) {
				return
			}
			continue
		}
		backoff.reset()
		if task == nil {
			continue
		}
		d.active.Add(1)
		d.processTask(ctx, task)
		d.active.Add(-1)
	}
}

func (d *service) processTask(ctx context.Context, task *runtimeprotocol.ExecutionClaim) {
	ctx = withTaskProof(ctx, task.Subject, task.ClaimEpoch)
	taskCtx, cancelTask := context.WithCancel(ctx)
	leaseLost := &atomic.Bool{}
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		d.renewLoop(taskCtx, cancelTask, task, leaseLost)
	}()

	// Engine adapters return only after their process group has exited.
	result, runErr := d.executeTask(taskCtx, task)
	// A daemon shutdown cancels the process context even though the user did
	// not cancel this task. Report that boundary as an infrastructure failure
	// so the parent workflow can park at its durable checkpoint and offer an
	// explicit continuation. Completing a synthetic timeout result would lose
	// the process-interruption identity and terminalize the parent as work
	// failure instead.
	if ctx.Err() != nil && runErr != nil && result.Status != "completed" {
		if result.TaskID == "" {
			result = failedExecutionReceipt(task, "runtime_process_interrupted: daemon shutdown")
		} else {
			result.Status = "failed"
			result.Error = "runtime_process_interrupted: daemon shutdown"
		}
	} else if runErr != nil && result.Status == "" {
		result = failedExecutionReceipt(task, runErr.Error())
	}
	journal := resultJournal{TaskID: task.TaskID, Result: result, Subject: task.Subject, ClaimEpoch: task.ClaimEpoch}
	saved := false
	if d.resultSpool != nil {
		if err := d.resultSpool.save(journal); err != nil {
			slog.Error("runtime result could not be journaled", "task_id", task.TaskID, "error", err)
		} else {
			saved = true
			defer d.resultSpool.release(task.TaskID)
		}
	}
	accepted := false
	if !leaseLost.Load() {
		reportCtx := taskCtx
		stopReport := func() {}
		if ctx.Err() != nil {
			reportCtx, stopReport = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		}
		report := func(reportCtx context.Context) error {
			return d.client.complete(reportCtx, result)
		}
		reportErr := d.reportUntilAccepted(reportCtx, report)
		if isRejectedRuntimeResult(reportErr) {
			slog.Error("runtime result rejected", "task_id", task.TaskID, "error", reportErr)
			if saved {
				d.resultSpool.retain(task.TaskID, "rejected")
				saved = false
			}
			rejected := failedExecutionReceipt(task, "runtime_result_rejected: result validation failed; inspect runtime log")
			reportErr = d.reportUntilAccepted(reportCtx, func(ctx context.Context) error {
				return d.client.complete(ctx, rejected)
			})
		}
		stopReport()
		accepted = reportErr == nil
		if accepted && saved {
			_ = d.resultSpool.acknowledge(task.TaskID)
		}
		if errors.Is(reportErr, errLeaseLost) {
			leaseLost.Store(true)
		}
	}
	cancelTask()
	<-renewDone
	if !accepted && (leaseLost.Load() || ctx.Err() != nil && !saved) {
		ackCtx := ctx
		if ctx.Err() != nil {
			var cancel context.CancelFunc
			ackCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
		}
		stopErr := d.reportUntilAccepted(ackCtx, func(reportCtx context.Context) error {
			return d.client.stopped(reportCtx, task, &result)
		})
		if stopErr == nil && saved {
			d.resultSpool.retain(task.TaskID, "stopped")
		}
	}
}

func failedExecutionReceipt(task *runtimeprotocol.ExecutionClaim, message string) runtimeprotocol.ExecutionReceipt {
	return runtimeprotocol.ExecutionReceipt{Versioned: runtimeprotocol.NewVersioned(), SchemaVersion: runtimeprotocol.ReceiptSchemaV1,
		TaskID: task.TaskID, ClaimEpoch: task.ClaimEpoch, Subject: task.Subject, Status: "failed", Error: message}
}

func (d *service) renewLoop(ctx context.Context, cancel context.CancelFunc, task *runtimeprotocol.ExecutionClaim, leaseLost *atomic.Bool) {
	ttl := time.Minute
	if !task.LeaseExpiresAt.IsZero() && !task.LeaseIssuedAt.IsZero() {
		ttl = task.LeaseExpiresAt.Sub(task.LeaseIssuedAt)
	}
	// Stop locally before the last confirmed lease can expire on the server.
	// The per-request deadline also bounds a connection which silently hangs.
	margin := min(ttl/10, 2*time.Second)
	deadline := time.Now().Add(ttl - margin)
	if !task.LeaseExpiresAt.IsZero() && task.LeaseExpiresAt.Add(-margin).Before(deadline) {
		deadline = task.LeaseExpiresAt.Add(-margin)
	}
	backoff := newBackoff(d.minBackoff, d.maxBackoff)
	delay := d.renewInterval
	for {
		leaseCtx, endLease := context.WithDeadline(ctx, deadline)
		if !waitFor(leaseCtx, delay) {
			endLease()
			if ctx.Err() == nil {
				leaseLost.Store(true)
				cancel()
			}
			return
		}
		started := time.Now()
		err := d.client.renew(leaseCtx, task)
		endLease()
		switch {
		case err == nil:
			deadline = started.Add(ttl - margin)
			backoff.reset()
			delay = d.renewInterval
		case errors.Is(err, errLeaseLost), !time.Now().Before(deadline):
			leaseLost.Store(true)
			cancel()
			return
		case ctx.Err() != nil:
			return
		default:
			slog.Warn("runtime task renewal failed; retrying", "task_id", task.TaskID, "error", err)
			delay = backoff.next()
		}
	}
}

func (d *service) reportUntilAccepted(ctx context.Context, report func(context.Context) error) error {
	backoff := newBackoff(d.minBackoff, d.maxBackoff)
	for {
		err := report(ctx)
		if err == nil || errors.Is(err, errLeaseLost) || isRejectedRuntimeResult(err) || ctx.Err() != nil {
			return err
		}
		slog.Warn("runtime task result failed; retrying", "error", err)
		if !waitFor(ctx, backoff.next()) {
			return ctx.Err()
		}
	}
}

func isRejectedRuntimeResult(err error) bool {
	var status *httpStatusError
	return errors.As(err, &status) && (status.code == 400 || status.code == 413 || status.code == 422)
}

func (d *service) executeTask(ctx context.Context, task *runtimeprotocol.ExecutionClaim) (runtimeprotocol.ExecutionReceipt, error) {
	ctx, err := execution.BindSubject(ctx, task.Subject)
	if err != nil {
		return runtimeprotocol.ExecutionReceipt{}, err
	}
	ctx = withTaskProof(ctx, task.Subject, task.ClaimEpoch)
	request := task.Request
	if request.Engine == runtimeprotocol.EngineLoom {
		return d.executeLoomTask(ctx, task, request)
	}
	if !engine.IsCLIEngine(request.Engine) {
		return runtimeprotocol.ExecutionReceipt{}, fmt.Errorf("runtime: unsupported CLI engine %q", request.Engine)
	}
	if _, err := agentExecutionStampForTask(task, request); err != nil {
		return runtimeprotocol.ExecutionReceipt{}, err
	}
	record, err := runtimeagent.Decode(*task)
	if err != nil {
		return runtimeprotocol.ExecutionReceipt{}, err
	}
	taskTargets, err := runtimeTaskMCPTargets(d.server, task, request)
	if err != nil {
		return runtimeprotocol.ExecutionReceipt{}, err
	}
	workspaceRoot := d.workspacesRoot
	var isolation *engine.ProcessIsolation
	var managedEnv map[string]string
	if d.subjectGuard != nil {
		subjectRoot, policy, guardErr := d.subjectGuard.bind(task.Subject)
		if guardErr != nil {
			return runtimeprotocol.ExecutionReceipt{}, guardErr
		}
		resolved, resolveErr := d.credentials(ctx, task)
		if resolveErr != nil {
			return runtimeprotocol.ExecutionReceipt{}, resolveErr
		}
		managedEnv, err = managedEnvironment(subjectRoot, resolved)
		if err != nil {
			return runtimeprotocol.ExecutionReceipt{}, err
		}
		if filepath.Base(task.TaskID) != task.TaskID || task.TaskID == "." || task.TaskID == ".." {
			return runtimeprotocol.ExecutionReceipt{}, errors.New("invalid invocation identity")
		}
		workspaceRoot, isolation = filepath.Join(subjectRoot, ".invocations", task.TaskID), policy
	}
	if d.subjectGuard == nil && request.NodeID != "" && task.RunSnapshotID != "" {
		if filepath.Base(task.TaskID) != task.TaskID || task.TaskID == "." || task.TaskID == ".." {
			return runtimeprotocol.ExecutionReceipt{}, fmt.Errorf("invalid invocation identity")
		}
		workspaceRoot = filepath.Join(workspaceRoot, ".invocations", task.TaskID)
	}
	workDir, runEnv, err := execenv.Materialize(ctx, workspaceRoot, record, request.Prompt, nil)
	if err != nil {
		return runtimeprotocol.ExecutionReceipt{}, err
	}
	if len(request.Attachments) > 0 {
		downloadDir, err := os.MkdirTemp(workDir, ".weave-attachments-")
		if err != nil {
			return runtimeprotocol.ExecutionReceipt{}, fmt.Errorf("runtime: create attachment temp directory: %w", err)
		}
		defer os.RemoveAll(downloadDir)
		attachments := make([]execspec.Attachment, 0, len(request.Attachments))
		for _, attachment := range request.Attachments {
			local, err := os.CreateTemp(downloadDir, "attachment-")
			if err != nil {
				return runtimeprotocol.ExecutionReceipt{}, fmt.Errorf("runtime: create attachment temp file: %w", err)
			}
			downloadErr := d.client.downloadAttachment(ctx, task.TaskID, attachment.ID, local)
			closeErr := local.Close()
			if downloadErr != nil {
				return runtimeprotocol.ExecutionReceipt{}, downloadErr
			}
			if closeErr != nil {
				return runtimeprotocol.ExecutionReceipt{}, fmt.Errorf("runtime: close attachment temp file: %w", closeErr)
			}
			attachments = append(attachments, execspec.Attachment{Filename: attachment.Filename, Path: local.Name()})
		}
		workDir, runEnv, err = execenv.Materialize(ctx, workspaceRoot, record, request.Prompt, attachments)
		if err != nil {
			return runtimeprotocol.ExecutionReceipt{}, err
		}
	}
	cliAuthMode := "subject_provider"
	oneAPIBase, oneAPIKey := managedEnv["OPENAI_BASE_URL"], managedEnv["OPENAI_API_KEY"]
	if d.subjectGuard == nil {
		cliAuthMode = d.detectCLIAuthMode(ctx, request.Engine)
		oneAPIBase, oneAPIKey = runtimeProviderConfig(request)
	}
	if err := validateRuntimeProviderConfig(request.Engine, cliAuthMode, oneAPIKey); err != nil {
		return runtimeprotocol.ExecutionReceipt{}, err
	}
	if err := execenv.WriteEngineConfigWithAuthMode(request.Engine, workDir, record, oneAPIBase, d.server, oneAPIKey, cliAuthMode, taskTargets...); err != nil {
		return runtimeprotocol.ExecutionReceipt{}, err
	}
	if runEnv == nil {
		runEnv = make(map[string]string)
	}
	mergeRuntimeProviderEnv(runEnv, oneAPIBase, oneAPIKey)
	for key, value := range managedEnv {
		runEnv[key] = value
	}
	for idx, target := range taskTargets {
		runEnv[fmt.Sprintf("WEAVE_MCP_BOUNDARY_TOKEN_%d", idx)] = target.Token
	}
	if cliAuthMode == codexChatGPTAuthMode && request.Engine == engine.Codex {
		runEnv["WEAVE_CODEX_AUTH_MODE"] = codexChatGPTAuthMode
		delete(runEnv, "OPENAI_BASE_URL")
		delete(runEnv, "OPENAI_API_KEY")
		delete(runEnv, "ONEAPI_API_KEY")
	}
	if cliAuthMode == claudeOAuthAuthMode && request.Engine == engine.Claude {
		runEnv["WEAVE_CLAUDE_AUTH_MODE"] = claudeOAuthAuthMode
		for _, key := range claudeInjectedAuthEnvKeys {
			delete(runEnv, key)
		}
	}
	if err := materializeInputFiles(workDir, request.InputFiles); err != nil {
		return runtimeprotocol.ExecutionReceipt{}, fmt.Errorf("materialize upstream files: %w", err)
	}
	prompt := request.Prompt
	logs := startTaskLogs(ctx, d.client, task)
	defer logs.close()
	var code *codeSession
	if request.Code != nil {
		remote := codeRemote{}
		if request.Code.Proxy {
			remote = proxiedCodeRemote(d.client.baseURL, d.client.token, task)
		}
		code, err = prepareCodeWorkspace(ctx, d.workspacesRoot, workDir, *request.Code, remote, request.NodeID, logs)
		if err != nil {
			return runtimeprotocol.ExecutionReceipt{}, fmt.Errorf("prepare code workspace: %w", err)
		}
		prompt = code.prompt(prompt)
	}
	for key, value := range task.Subject.Environment() {
		runEnv[key] = value
	}
	outputsBefore := runtimehost.SnapshotOutputArtifacts(workDir)
	timeoutSeconds := request.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultTimeoutSeconds
	}
	publish, finishProgress := d.publicEventCapture(task, engine.PublishesPublicEvents(request.Engine) && request.NodeID != "" && task.RunSnapshotID != "")
	if task.DeadlineAt != nil {
		var stop context.CancelFunc
		ctx, stop = context.WithDeadline(ctx, *task.DeadlineAt)
		defer stop()
	}
	result, err := d.runEngine(ctx, request.Engine, engine.RunSpec{
		DisableTools: deniesAllTools(record.Permissions.Deny),
		Subject:      task.Subject,
		Isolation:    isolation,
		OnPublicEvent: func(event engine.Event, truncated bool) {
			if publish != nil {
				publish(event, truncated)
			}
			logs.event(event)
		},
		MCPServers:    engineTaskMCPServers(taskTargets),
		WorkDir:       workDir,
		Prompt:        prompt,
		Model:         request.Model,
		Env:           runEnv,
		Timeout:       time.Duration(timeoutSeconds) * time.Second,
		EngineVersion: d.engineVersion(request.Engine),
		OutputSchema:  request.OutputSchema,
	})
	finishProgress(&result)
	err = errors.Join(err, runtimehost.CollectRunOutputArtifacts(workDir, outputsBefore, &result))
	if code != nil && err == nil && result.Status == "completed" {
		err = code.finish(ctx, &result)
	}
	receipt := runtimeprotocol.ExecutionReceipt{Versioned: runtimeprotocol.NewVersioned(), SchemaVersion: runtimeprotocol.ReceiptSchemaV1,
		TaskID: task.TaskID, Subject: task.Subject, ClaimEpoch: task.ClaimEpoch, SessionID: result.SessionID,
		ArtifactCollection: result.ArtifactCollection, Output: result.Output, RetrySafeBeforeExecution: result.RetrySafeBeforeExecution,
		ReportedModels: append([]string(nil), result.ReportedModels...), Status: result.Status, Error: result.Err, UsageReceipt: result.Usage,
		Diagnostics: append([]engine.Diagnostic(nil), result.Diagnostics...), Events: append([]engine.Event(nil), result.Events...), Artifacts: append([]engine.Artifact(nil), result.Artifacts...)}
	if err != nil {
		return receipt, err
	}
	if result.Status != "completed" {
		message := result.Err
		if message == "" {
			message = fmt.Sprintf("engine run ended with status %q", result.Status)
		}
		return receipt, errors.New(message)
	}
	return receipt, nil
}

func deniesAllTools(denied []string) bool {
	for _, value := range denied {
		if value == "*" {
			return true
		}
	}
	return false
}

func (d *service) engineVersion(name string) string {
	_, capabilities := d.engines()
	for _, capability := range capabilities {
		if capability.Engine == name {
			return capability.BinaryVersion
		}
	}
	return "unavailable"
}

func runtimeProviderConfig(request runtimeprotocol.ExecutionRequest) (baseURL, apiKey string) {
	baseURL = os.Getenv("OPENAI_BASE_URL")
	apiKey = firstNonEmpty(os.Getenv("OPENAI_API_KEY"), os.Getenv("ONEAPI_API_KEY"))
	return baseURL, apiKey
}

func validateRuntimeProviderConfig(engineName, authMode, apiKey string) error {
	if engineName == engine.Codex &&
		!strings.EqualFold(strings.TrimSpace(authMode), codexChatGPTAuthMode) && strings.TrimSpace(apiKey) == "" {
		return errors.New("runtime_credentials_missing: ONEAPI_API_KEY")
	}
	return nil
}

func mergeRuntimeProviderEnv(env map[string]string, baseURL, apiKey string) {
	if baseURL != "" {
		env["OPENAI_BASE_URL"] = baseURL
	}
	if apiKey != "" {
		env["OPENAI_API_KEY"] = apiKey
		env["ONEAPI_API_KEY"] = apiKey
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func agentExecutionStampForTask(task *runtimeprotocol.ExecutionClaim, request runtimeprotocol.ExecutionRequest) (execution.AgentExecutionStamp, error) {
	if task == nil {
		return execution.AgentExecutionStamp{}, errors.New("runtime: claim is required")
	}
	if err := task.Validate(); err != nil {
		return execution.AgentExecutionStamp{}, err
	}
	if request.SchemaVersion != task.Request.SchemaVersion || request.FrozenAgentHash != task.Request.FrozenAgentHash {
		return execution.AgentExecutionStamp{}, errors.New("runtime: execution request does not match claim")
	}
	teamScoped := task.Agent.ExecutionScope == execution.ScopeTeamFreeCollab || task.Agent.ExecutionScope == execution.ScopeTeamWorkerLeaf
	if teamScoped != (task.RunSnapshotID != "") {
		return execution.AgentExecutionStamp{}, errors.New("runtime: team agent claim requires exact run snapshot identity")
	}
	if _, err := runtimeagent.Decode(*task); err != nil {
		return execution.AgentExecutionStamp{}, err
	}
	return execution.AgentExecutionStamp{AgentID: task.Agent.ID, AgentVersion: task.Agent.Version,
		ExecutionScope: task.Agent.ExecutionScope, RunSnapshotID: task.RunSnapshotID}, nil
}

func (d *service) detectCLIAuthMode(ctx context.Context, engineName string) string {
	switch engineName {
	case engine.Codex:
		if codexLoggedInWithChatGPT(ctx, config.ResolveEngineCLIPath(engine.Codex)) {
			return codexChatGPTAuthMode
		}
	case engine.Claude:
		if claudeLoggedInWithFirstPartyOAuth(ctx, config.ResolveEngineCLIPath(engine.Claude)) {
			return claudeOAuthAuthMode
		}
	}
	return ""
}

var claudeInjectedAuthEnvKeys = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ONEAPI_API_KEY",
	"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
}

func claudeLoggedInWithFirstPartyOAuth(ctx context.Context, cliPath string) bool {
	if cliPath == "" {
		cliPath = engine.Claude
	}
	checkCtx, cancel := context.WithTimeout(ctx, claudeLoginProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, cliPath, "auth", "status")
	cmd.Env = envWithExecutableDir(envWithoutKeys(os.Environ(), claudeInjectedAuthEnvKeys...), cliPath)
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return claudeAuthStatusIsFirstPartyOAuth(output)
}

// claudeAuthStatusIsFirstPartyOAuth accepts `claude auth status` output only for
// a logged-in first-party account whose method is on the allowlist. Claude Code
// 2.1.285 reports subscription login as claude.ai. API keys, third-party
// providers and unknown methods are rejected.
func claudeAuthStatusIsFirstPartyOAuth(output []byte) bool {
	var status struct {
		LoggedIn    bool   `json:"loggedIn"`
		AuthMethod  string `json:"authMethod"`
		APIProvider string `json:"apiProvider"`
	}
	if json.Unmarshal(output, &status) != nil {
		return false
	}
	if !status.LoggedIn || status.APIProvider != "firstParty" {
		return false
	}
	switch status.AuthMethod {
	case "claude.ai", "oauth_token":
		return true
	default:
		return false
	}
}

func envWithoutKeys(in []string, keys ...string) []string {
	blocked := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		blocked[key] = struct{}{}
	}
	out := make([]string, 0, len(in))
	for _, entry := range in {
		key, _, ok := strings.Cut(entry, "=")
		if _, remove := blocked[key]; ok && remove {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func codexLoggedInWithChatGPT(ctx context.Context, cliPath string) bool {
	if cliPath == "" {
		cliPath = engine.Codex
	}
	// Homebrew's Codex launcher must start Node and read the host credential
	// store. Three seconds proved too short under normal machine contention and
	// caused a false "not logged in" result followed by an API-key fallback.
	// Keep the probe bounded while allowing a realistic cold start.
	checkCtx, cancel := context.WithTimeout(ctx, codexLoginProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, cliPath, "login", "status")
	// Homebrew installs Codex as a /usr/bin/env node launcher. A launchd
	// runtime commonly has no /opt/homebrew/bin in PATH even when cliPath is
	// the absolute Codex path. Match the real engine execution environment by
	// making the CLI directory available before probing the host login.
	cmd.Env = envWithExecutableDir(envWithoutCodexHome(os.Environ()), cliPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "Logged in using ChatGPT")
}

func envWithExecutableDir(in []string, executablePath string) []string {
	dir := filepath.Dir(strings.TrimSpace(executablePath))
	if dir == "" || dir == "." {
		return in
	}
	out := append([]string(nil), in...)
	for i, entry := range out {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key != "PATH" {
			continue
		}
		for _, existing := range filepath.SplitList(value) {
			if existing == dir {
				return out
			}
		}
		if value == "" {
			out[i] = "PATH=" + dir
		} else {
			out[i] = "PATH=" + dir + string(os.PathListSeparator) + value
		}
		return out
	}
	return append(out, "PATH="+dir)
}

func envWithoutCodexHome(in []string) []string {
	out := make([]string, 0, len(in))
	for _, entry := range in {
		key, _, ok := strings.Cut(entry, "=")
		if ok && key == "CODEX_HOME" {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func runEngine(ctx context.Context, engineName string, spec engine.RunSpec) (engine.RunResult, error) {
	backend, err := engine.New(engineName, config.ResolveEngineCLIPath(engineName))
	if err != nil {
		return engine.RunResult{}, err
	}
	return backend.Run(ctx, spec)
}

func detectEngines() []string {
	engines := make([]string, 0, 3)
	for _, name := range []string{engine.OpenCode, engine.Codex, engine.Claude} {
		if _, err := exec.LookPath(config.ResolveEngineCLIPath(name)); err == nil {
			engines = append(engines, name)
		}
	}
	return engines
}

func detectEngineCapabilities(ctx context.Context, detected []string) []runtimeprotocol.EngineCapability {
	capabilities := make([]runtimeprotocol.EngineCapability, 0, len(detected))
	for _, name := range detected {
		configuredPath := config.ResolveEngineCLIPath(name)
		binaryPath, err := exec.LookPath(configuredPath)
		if err != nil {
			continue
		}
		authMode := runtimeprotocol.AuthModeProvider
		endpointClass := "configured_provider"
		switch name {
		case engine.Codex:
			if codexLoggedInWithChatGPT(ctx, binaryPath) {
				authMode = runtimeprotocol.AuthModeChatGPT
				endpointClass = "openai_subscription"
			}
		case engine.Claude:
			if claudeLoggedInWithFirstPartyOAuth(ctx, binaryPath) {
				authMode = runtimeprotocol.AuthModeOAuth
				endpointClass = "anthropic_first_party"
			}
		}
		capability := runtimeprotocol.EngineCapability{
			Engine: name, BinaryPath: binaryPath, SubjectIsolation: runtimeprotocol.SubjectIsolationSingleUser,
			BinaryVersion: engine.BinaryVersion(ctx, binaryPath),
			AuthMode:      authMode, ProtocolVersion: engineProtocolVersion(name),
			EndpointClass: endpointClass,
		}
		describeEngineConfiguration(&capability)
		describeEngineAvailability(&capability)
		capabilities = append(capabilities, capability)
	}
	return capabilities
}

func describeEngineAvailability(capability *runtimeprotocol.EngineCapability) {
	capability.Availability = runtimeprotocol.EngineAvailabilityUnknown
	capability.UnavailableReason = ""
	if capability.AuthMode == runtimeprotocol.AuthModeChatGPT || capability.AuthMode == runtimeprotocol.AuthModeOAuth {
		capability.Availability = runtimeprotocol.EngineAvailabilityReady
		return
	}
	if capability.Engine != engine.Codex || capability.AuthMode != runtimeprotocol.AuthModeProvider {
		return
	}
	if firstNonEmpty(os.Getenv("OPENAI_API_KEY"), os.Getenv("ONEAPI_API_KEY")) == "" {
		capability.Availability = runtimeprotocol.EngineAvailabilityUnavailable
		capability.UnavailableReason = "provider_credentials_missing"
		return
	}
	capability.Availability = runtimeprotocol.EngineAvailabilityReady
}

func engineProtocolVersion(name string) string {
	switch name {
	case engine.Codex:
		return "codex-jsonl-v1"
	case engine.Claude:
		return "claude-stream-json-v1"
	case engine.OpenCode:
		return "opencode-json-v1"
	default:
		return "unknown"
	}
}

type exponentialBackoff struct {
	min     time.Duration
	max     time.Duration
	current time.Duration
}

func newBackoff(minimum, maximum time.Duration) *exponentialBackoff {
	return &exponentialBackoff{min: minimum, max: maximum, current: minimum}
}

func (b *exponentialBackoff) next() time.Duration {
	delay := b.current
	if b.current < b.max {
		b.current *= 2
		if b.current > b.max {
			b.current = b.max
		}
	}
	return delay
}

func (b *exponentialBackoff) reset() {
	b.current = b.min
}

func waitFor(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
