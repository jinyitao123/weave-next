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
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/secret"
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
	engineCapabilities []runtimes.EngineCapability
	renewInterval      time.Duration
	heartbeatInterval  time.Duration
	minBackoff         time.Duration
	maxBackoff         time.Duration
}

type service struct {
	client             *runtimeClient
	server             string
	workspacesRoot     string
	concurrency        int
	runEngine          runEngineFunc
	detectedEngines    []string
	engineCapabilities []runtimes.EngineCapability
	claimWaitSeconds   int
	renewInterval      time.Duration
	heartbeatInterval  time.Duration
	minBackoff         time.Duration
	maxBackoff         time.Duration
	active             atomic.Int32
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
	return d.run(ctx)
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
	if cfg.detectedEngines == nil {
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
	return &service{
		client:             client,
		server:             cfg.server,
		workspacesRoot:     cfg.workspacesRoot,
		concurrency:        cfg.concurrency,
		runEngine:          cfg.runEngine,
		detectedEngines:    cfg.detectedEngines,
		engineCapabilities: cfg.engineCapabilities,
		claimWaitSeconds:   defaultClaimWait,
		renewInterval:      cfg.renewInterval,
		heartbeatInterval:  cfg.heartbeatInterval,
		minBackoff:         cfg.minBackoff,
		maxBackoff:         cfg.maxBackoff,
	}, nil
}

func (d *service) run(ctx context.Context) error {
	if !d.helloUntilConnected(ctx) {
		return nil
	}

	var workers sync.WaitGroup
	workers.Add(d.concurrency + 1)
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
		err := d.client.hello(ctx, d.detectedEngines, d.engineCapabilities, d.concurrency)
		if err == nil {
			slog.Info("runtime daemon connected", "server", d.server, "engines", d.detectedEngines)
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
		if err := d.client.heartbeat(ctx, int(d.active.Load())); err == nil {
			return
		} else if ctx.Err() == nil {
			slog.Warn("runtime heartbeat failed; retrying", "error", err)
		}
		if !waitFor(ctx, backoff.next()) {
			return
		}
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

func (d *service) processTask(ctx context.Context, task *taskqueue.Task) {
	taskCtx, cancelTask := context.WithCancel(ctx)
	leaseLost := &atomic.Bool{}
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		d.renewLoop(taskCtx, cancelTask, task.ID, leaseLost)
	}()

	result, runErr := d.executeTask(taskCtx, task)
	if leaseLost.Load() || ctx.Err() != nil {
		cancelTask()
		<-renewDone
		return
	}

	var report func(context.Context) error
	if runErr != nil && result.Status == "" {
		report = func(reportCtx context.Context) error {
			return d.client.fail(reportCtx, task.ID, runErr.Error())
		}
	} else {
		report = func(reportCtx context.Context) error {
			return d.client.complete(reportCtx, task.ID, result)
		}
	}
	if errors.Is(d.reportUntilAccepted(taskCtx, report), errLeaseLost) {
		leaseLost.Store(true)
	}
	cancelTask()
	<-renewDone
}

func (d *service) renewLoop(ctx context.Context, cancel context.CancelFunc, taskID string, leaseLost *atomic.Bool) {
	backoff := newBackoff(d.minBackoff, d.maxBackoff)
	delay := d.renewInterval
	for waitFor(ctx, delay) {
		err := d.client.renew(ctx, taskID)
		switch {
		case err == nil:
			backoff.reset()
			delay = d.renewInterval
		case errors.Is(err, errLeaseLost):
			leaseLost.Store(true)
			cancel()
			return
		case ctx.Err() != nil:
			return
		default:
			slog.Warn("runtime task renewal failed; retrying", "task_id", taskID, "error", err)
			delay = backoff.next()
		}
	}
}

func (d *service) reportUntilAccepted(ctx context.Context, report func(context.Context) error) error {
	backoff := newBackoff(d.minBackoff, d.maxBackoff)
	for {
		err := report(ctx)
		if err == nil || errors.Is(err, errLeaseLost) || ctx.Err() != nil {
			return err
		}
		slog.Warn("runtime task result failed; retrying", "error", err)
		if !waitFor(ctx, backoff.next()) {
			return ctx.Err()
		}
	}
}

func (d *service) executeTask(ctx context.Context, task *taskqueue.Task) (runtimes.EngineExecResult, error) {
	var request runtimes.EngineExecRequest
	if err := json.Unmarshal(task.Payload, &request); err != nil {
		return runtimes.EngineExecResult{}, fmt.Errorf("runtime: decode task payload: %w", err)
	}
	if !engine.IsCLIEngine(request.Engine) {
		return runtimes.EngineExecResult{}, fmt.Errorf("runtime: unsupported CLI engine %q", request.Engine)
	}
	if _, err := agentExecutionStampForTask(task, request); err != nil {
		return runtimes.EngineExecResult{}, err
	}

	workDir, runEnv, err := execenv.Materialize(d.workspacesRoot, request.Record, request.Prompt, nil)
	if err != nil {
		return runtimes.EngineExecResult{}, err
	}
	if len(request.Attachments) > 0 {
		downloadDir, err := os.MkdirTemp(workDir, ".weave-attachments-")
		if err != nil {
			return runtimes.EngineExecResult{}, fmt.Errorf("runtime: create attachment temp directory: %w", err)
		}
		defer os.RemoveAll(downloadDir)

		attachments := make([]execenv.Attachment, 0, len(request.Attachments))
		for _, attachment := range request.Attachments {
			local, err := os.CreateTemp(downloadDir, "attachment-")
			if err != nil {
				return runtimes.EngineExecResult{}, fmt.Errorf("runtime: create attachment temp file: %w", err)
			}
			downloadErr := d.client.downloadAttachment(ctx, task.ID, attachment.ID, local)
			closeErr := local.Close()
			if downloadErr != nil {
				return runtimes.EngineExecResult{}, downloadErr
			}
			if closeErr != nil {
				return runtimes.EngineExecResult{}, fmt.Errorf("runtime: close attachment temp file: %w", closeErr)
			}
			attachments = append(attachments, execenv.Attachment{Filename: attachment.Filename, Path: local.Name()})
		}
		workDir, runEnv, err = execenv.Materialize(d.workspacesRoot, request.Record, request.Prompt, attachments)
		if err != nil {
			return runtimes.EngineExecResult{}, err
		}
	}

	cliAuthMode := d.detectCLIAuthMode(ctx, request.Engine)
	oneAPIBase, oneAPIKey := runtimeProviderConfig(request)
	if err := execenv.WriteEngineConfigWithAuthMode(request.Engine, workDir, request.Record, oneAPIBase, d.server, oneAPIKey, cliAuthMode); err != nil {
		return runtimes.EngineExecResult{}, err
	}
	if runEnv == nil {
		runEnv = make(map[string]string)
	}
	for key, value := range request.Env {
		runEnv[key] = value
	}
	mergeRuntimeProviderEnv(runEnv, oneAPIBase, oneAPIKey)
	for idx := range request.Record.MCPServers {
		if token := secret.BoundaryToken(task.WorkspaceID, request.Record.Name, idx); token != "" {
			runEnv[fmt.Sprintf("WEAVE_MCP_BOUNDARY_TOKEN_%d", idx)] = token
		}
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
	outputsBefore := runtimes.SnapshotOutputArtifacts(workDir)
	timeoutSeconds := request.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultTimeoutSeconds
	}
	result, err := d.runEngine(ctx, request.Engine, engine.RunSpec{
		WorkDir:       workDir,
		Prompt:        request.Prompt,
		Model:         request.Model,
		Env:           runEnv,
		Timeout:       time.Duration(timeoutSeconds) * time.Second,
		EngineVersion: d.engineVersion(request.Engine),
		OutputSchema:  request.OutputSchema,
	})
	if result.Status == "completed" {
		result.Artifacts = runtimes.CollectOutputArtifactsSince(workDir, outputsBefore)
	}
	execResult := runtimes.CLIEngineExecResult(result)
	if err != nil {
		return execResult, err
	}
	if result.Status != "completed" {
		message := result.Err
		if message == "" {
			message = fmt.Sprintf("engine run ended with status %q", result.Status)
		}
		return execResult, errors.New(message)
	}
	return execResult, nil
}

func (d *service) engineVersion(name string) string {
	for _, capability := range d.engineCapabilities {
		if capability.Engine == name {
			return capability.BinaryVersion
		}
	}
	return "unavailable"
}

func runtimeProviderConfig(request runtimes.EngineExecRequest) (baseURL, apiKey string) {
	baseURL = firstNonEmpty(request.OneAPIBase, os.Getenv("OPENAI_BASE_URL"))
	apiKey = firstNonEmpty(request.OneAPIKey, os.Getenv("OPENAI_API_KEY"), os.Getenv("ONEAPI_API_KEY"))
	return baseURL, apiKey
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

func agentExecutionStampForTask(
	task *taskqueue.Task,
	request runtimes.EngineExecRequest,
) (execution.AgentExecutionStamp, error) {
	if task == nil {
		return execution.AgentExecutionStamp{}, errors.New("runtime: task is required")
	}
	if task.IdentityKind != taskqueue.IdentityAgent {
		return execution.AgentExecutionStamp{}, fmt.Errorf(
			"runtime: task identity kind %q is not an agent", task.IdentityKind,
		)
	}
	if task.IdentitySchemaVersion != 2 {
		return execution.AgentExecutionStamp{}, fmt.Errorf(
			"runtime: unsupported agent identity schema %d", task.IdentitySchemaVersion,
		)
	}
	if task.WorkspaceID == "" || task.Agent == "" ||
		task.AgentID == "" || task.AgentVersion < 1 ||
		!task.ExecutionScope.Valid() {
		return execution.AgentExecutionStamp{}, errors.New(
			"runtime: invalid schema-two agent execution identity",
		)
	}
	if task.WorkflowID != "" || task.WorkflowVersion != 0 {
		return execution.AgentExecutionStamp{}, errors.New(
			"runtime: agent task contains workflow identity",
		)
	}
	teamScoped := task.ExecutionScope == execution.ScopeTeamFreeCollab ||
		task.ExecutionScope == execution.ScopeTeamWorkerLeaf
	if teamScoped != (task.RunSnapshotID != "") {
		return execution.AgentExecutionStamp{}, errors.New(
			"runtime: team agent task requires exact run snapshot identity",
		)
	}
	if request.Record == nil {
		return execution.AgentExecutionStamp{}, errors.New(
			"runtime: task missing agent record",
		)
	}
	if request.Record.WorkspaceID != task.WorkspaceID ||
		request.Record.Name != task.Agent ||
		request.Record.ID != task.AgentID ||
		request.Record.Version != task.AgentVersion {
		return execution.AgentExecutionStamp{}, errors.New(
			"runtime: task identity does not match payload agent record",
		)
	}
	return execution.AgentExecutionStamp{
		AgentID:        task.AgentID,
		AgentVersion:   task.AgentVersion,
		ExecutionScope: task.ExecutionScope,
		RunSnapshotID:  task.RunSnapshotID,
	}, nil
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
	var status struct {
		LoggedIn    bool   `json:"loggedIn"`
		AuthMethod  string `json:"authMethod"`
		APIProvider string `json:"apiProvider"`
	}
	if json.Unmarshal(output, &status) != nil {
		return false
	}
	return status.LoggedIn && status.AuthMethod == "oauth_token" && status.APIProvider == "firstParty"
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

func detectEngineCapabilities(ctx context.Context, detected []string) []runtimes.EngineCapability {
	capabilities := make([]runtimes.EngineCapability, 0, len(detected))
	for _, name := range detected {
		configuredPath := config.ResolveEngineCLIPath(name)
		binaryPath, err := exec.LookPath(configuredPath)
		if err != nil {
			continue
		}
		authMode := runtimes.AuthModeProvider
		endpointClass := "configured_provider"
		switch name {
		case engine.Codex:
			if codexLoggedInWithChatGPT(ctx, binaryPath) {
				authMode = runtimes.AuthModeChatGPT
				endpointClass = "openai_subscription"
			}
		case engine.Claude:
			if claudeLoggedInWithFirstPartyOAuth(ctx, binaryPath) {
				authMode = runtimes.AuthModeOAuth
				endpointClass = "anthropic_first_party"
			}
		}
		capabilities = append(capabilities, runtimes.EngineCapability{
			Engine: name, BinaryPath: binaryPath,
			BinaryVersion: engine.BinaryVersion(ctx, binaryPath),
			AuthMode:      authMode, ProtocolVersion: engineProtocolVersion(name),
			EndpointClass: endpointClass,
		})
	}
	return capabilities
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
