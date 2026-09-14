package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/localruntime"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/labstack/echo/v4"
)

type managedRuntimeFixture struct {
	server *Server
	ctx    context.Context
	root   string
	record *registry.AgentRecord
}

func newManagedRuntimeFixture(t *testing.T) managedRuntimeFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX CLI fixture")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('managed','managed','managed');
 INSERT INTO weave_users(id,tenant_id,username,password) VALUES('alice','managed','alice','unused'),('bob','managed','bob','unused');
 INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('managed-agent','managed','worker','worker','{}');
 INSERT INTO weave_agent_versions(agent_id,workspace_id,version,spec) VALUES('managed-agent','managed',1,'{}');`); err != nil {
		t.Fatal(err)
	}
	pc := pool.Config()
	pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	prepared, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(prepared.Close)
	root := t.TempDir()
	script := filepath.Join(t.TempDir(), "codex-managed-fixture")
	contents := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli fixture'; exit 0; fi
test "$ONEAPI_API_KEY" = "$WEAVE_ACTOR_USER_ID-key" || exit 31
test -z "$SERVER_PRIVATE_API_KEY" || exit 32
prompt=$(cat)
printf 'run\n' >> "$HOME/executions"
mkdir -p outputs
printf '%s' "$WEAVE_ACTOR_USER_ID" > outputs/report.md
printf '%s\n' '{"type":"item.completed","item":{"id":"message","type":"agent_message","text":"Saved [report](outputs/report.md)."}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":4,"reasoning_output_tokens":0}}'
if [ "$prompt" = block ]; then while true; do sleep 1; done; fi
`
	if err := os.WriteFile(script, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEAVE_ENGINE_CODEX_PATH", script)
	t.Setenv("WEAVE_ENGINE_CLAUDE_PATH", "/unavailable/claude")
	t.Setenv("WEAVE_ENGINE_OPENCODE_PATH", "/unavailable/opencode")
	t.Setenv("SERVER_PRIVATE_API_KEY", "not-inherited")
	credentialStore := credentials.New(prepared, []byte("0123456789abcdef0123456789abcdef"))
	for _, user := range []string{"alice", "bob"} {
		subject := execution.Subject{WorkspaceID: "managed", UserID: user}
		if err := credentialStore.Upsert(execution.WithSubject(ctx, subject), "managed", llmrouter.ProviderConfig{ID: user, Name: user, BaseURL: "https://provider.invalid/v1", APIKey: user + "-key", Models: []string{"fixture"}, CredentialScope: frozen.CredentialScopeUser, CredentialUserID: user}); err != nil {
			t.Fatal(err)
		}
	}
	models := llmrouter.NewResolver(llmrouter.New("fixture"))
	models.SetSource(credentialStore)
	s := &Server{Echo: echo.New(), Pool: prepared, Config: &config.Config{JWTSecret: "fixture", WorkspacesRoot: root, LocalRuntimeEnabled: true}, Tasks: taskqueue.New(prepared, nil, 5*time.Second), Runtimes: runtimes.NewStore(prepared), Models: models, Credentials: credentialStore}
	api := s.Echo.Group("/v1/runtime", s.runtimeAuthMiddleware())
	api.POST("/hello", s.handleRuntimeHello)
	api.POST("/heartbeat", s.handleRuntimeHeartbeat)
	api.POST("/claim", s.handleRuntimeClaim)
	api.POST("/tasks/:id/renew", s.handleRuntimeTaskRenew)
	api.POST("/tasks/:id/complete", s.handleRuntimeTaskComplete)
	api.POST("/tasks/:id/stopped", s.handleRuntimeTaskStopped)
	api.POST("/tasks/:id/events", s.handleRuntimeTaskEvents)
	return managedRuntimeFixture{server: s, ctx: ctx, root: root, record: &registry.AgentRecord{WorkspaceID: "managed", ID: "managed-agent", Name: "worker", Version: 1, Engine: engine.Codex, Model: "fixture"}}
}

func (f managedRuntimeFixture) control(t *testing.T, user, id string, duration ...time.Duration) (context.Context, *taskqueue.Task) {
	t.Helper()
	subject := execution.Subject{WorkspaceID: "managed", UserID: user}
	ctx := execution.WithSubject(f.ctx, subject)
	limit := 30 * time.Second
	if len(duration) > 0 {
		limit = duration[0]
	}
	deadline := time.Now().Add(limit)
	task := &taskqueue.Task{ID: id, WorkspaceID: "managed", Agent: "worker", AgentID: "managed-agent", AgentVersion: 1, IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeLegacyOrchestrator, Source: "test", Kind: "control", Payload: json.RawMessage(`{}`), DeadlineAt: &deadline}
	if err := f.server.Tasks.Enqueue(ctx, task); err != nil {
		t.Fatal(err)
	}
	claimed, err := f.server.Tasks.Claim(ctx, "owner-"+id, taskqueue.ClaimFilter{Kind: "control", WorkspaceID: "managed", IdentityKind: taskqueue.IdentityAgent})
	if err != nil || claimed == nil {
		t.Fatalf("claim control: %v", err)
	}
	bound, err := taskqueue.BindTaskExecution(ctx, claimed, f.server.Tasks)
	if err != nil {
		t.Fatal(err)
	}
	return execution.WithInvocationID(bound, "invoke-"+id), claimed
}

func (f managedRuntimeFixture) execute(ctx context.Context, host *localruntime.Manager, prompt string) (engine.RunResult, error) {
	return host.ExecRemote(ctx, "managed", f.record, execution.AgentExecutionStamp{AgentID: f.record.ID, AgentVersion: 1, ExecutionScope: execution.ScopeLegacyOrchestrator}, prompt, nil)
}

func TestManagedLocalRuntimeTwoUsersAndRestartRealPG(t *testing.T) {
	f := newManagedRuntimeFixture(t)
	host, err := f.server.ConfigureRuntimeExecution(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if host != nil {
			_ = host.Close()
		}
	}()
	registered, err := f.server.Runtimes.List(f.ctx, "managed")
	if err != nil || len(registered) != 1 {
		t.Fatalf("registrations: %v %v", registered, err)
	}
	if registered[0].EngineCapabilities[engine.Codex].SubjectIsolation != runtimeprotocol.SubjectIsolationStrong {
		t.Skip("host accurately declines strong multi-user isolation")
	}
	var first context.Context
	for _, user := range []string{"alice", "bob"} {
		ctx, parent := f.control(t, user, "control-"+user)
		if first == nil {
			first = ctx
		}
		result, err := f.execute(ctx, host, "report")
		if err != nil || result.Status != "completed" || len(result.Artifacts) != 1 || result.Artifacts[0].Content != user || result.Usage == nil {
			t.Fatalf("%s execution: %+v %v", user, result, err)
		}
		if len(result.Attempts) != 1 {
			t.Fatalf("physical attempts: %+v", result.Attempts)
		}
		child, err := f.server.Tasks.Get(ctx, "managed", result.Attempts[0].AttemptID)
		if err != nil || child.ParentTaskID != parent.ID || child.Subject.UserID != user || !child.DeadlineAt.Equal(*parent.DeadlineAt) || child.PhysicalUsage.InputTokens != 10 {
			t.Fatalf("physical lineage/usage: %+v %v", child, err)
		}
		foreign := execution.WithSubject(f.ctx, execution.Subject{WorkspaceID: "managed", UserID: map[string]string{"alice": "bob", "bob": "alice"}[user]})
		if _, err := f.server.Tasks.Get(foreign, "managed", child.ID); err == nil {
			t.Fatal("another user read completed evidence")
		}
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	host = nil
	host, err = f.server.ConfigureRuntimeExecution(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.server.Runtimes.List(f.ctx, "managed")
	if err != nil || len(after) != 1 || after[0].ID != registered[0].ID {
		t.Fatal("restart replaced runtime identity")
	}
	result, err := f.execute(first, host, "report")
	if err != nil || len(result.Attempts) != 1 {
		t.Fatalf("reconcile receipt: %+v %v", result, err)
	}
	var count int
	if err := f.server.Pool.QueryRow(f.ctx, `SELECT count(*) FROM weave_task_queue WHERE kind='engine_exec'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("restart replayed execution: %d %v", count, err)
	}
}

func awaitManaged(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("managed runtime did not reach expected state")
}

func TestManagedLocalRuntimeCancelledUsageSurvivesUnavailableStopAndRestartRealPG(t *testing.T) {
	f := newManagedRuntimeFixture(t)
	var unavailable atomic.Bool
	unavailable.Store(true)
	var evidenceMu sync.Mutex
	var evidence []byte
	var headers http.Header
	var endpoint string
	f.server.Echo.Pre(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if strings.HasSuffix(c.Request().URL.Path, "/stopped") && unavailable.Load() {
				body, err := io.ReadAll(c.Request().Body)
				if err != nil {
					return err
				}
				evidenceMu.Lock()
				evidence, headers, endpoint = body, c.Request().Header.Clone(), c.Request().URL.Path
				evidenceMu.Unlock()
				return c.NoContent(http.StatusServiceUnavailable)
			}
			return next(c)
		}
	})
	host, err := f.server.ConfigureRuntimeExecution(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if host != nil {
			_ = host.Close()
		}
	}()
	registered, regErr := f.server.Runtimes.List(f.ctx, "managed")
	if regErr != nil {
		t.Fatal(regErr)
	}
	t.Logf("capabilities: %+v", registered[0].EngineCapabilities)
	ctx, parent := f.control(t, "alice", "cancel-parent")
	done := make(chan error, 1)
	go func() { _, err := f.execute(ctx, host, "block"); done <- err }()
	// Actual CLI output proves execution and token use before cancellation.
	awaitManaged(t, func() bool {
		matches, _ := filepath.Glob(filepath.Join(f.root, ".local-runtime", "*", ".subjects", "*", "home", "executions"))
		return len(matches) == 1
	})
	if changed, err := f.server.Tasks.RequestCancelTask(ctx, "managed", parent.ID); err != nil || !changed {
		t.Fatalf("cancel: %v %v", changed, err)
	}
	awaitManaged(t, func() bool { evidenceMu.Lock(); defer evidenceMu.Unlock(); return len(evidence) > 0 })
	// Stop endpoint is unreachable. The process has exited but the platform must
	// retain an unknown stop outcome and the exact durable receipt for retry.
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	host = nil
	var childID, status string
	if err := f.server.Pool.QueryRow(f.ctx, `SELECT id,status FROM weave_task_queue WHERE kind='engine_exec'`).Scan(&childID, &status); err != nil || status != taskqueue.StatusCancelRequested {
		t.Fatalf("unknown stop outcome: %s %v", status, err)
	}
	pending, _ := filepath.Glob(filepath.Join(f.root, ".local-runtime", "*", ".weave-results", "*.pending.json"))
	if len(pending) != 1 {
		t.Fatalf("missing durable receipt: %v", pending)
	}
	saved, err := os.ReadFile(pending[0])
	if err != nil {
		t.Fatal(err)
	}
	var journal struct {
		Result runtimeprotocol.ExecutionReceipt `json:"result"`
	}
	if err := json.Unmarshal(saved, &journal); err != nil {
		t.Fatal(err)
	}
	if journal.Result.UsageReceipt == nil || journal.Result.UsageReceipt.InputTokens != 10 {
		t.Fatalf("cancel lost observed usage: %+v", journal.Result)
	}
	unavailable.Store(false)
	host, err = f.server.ConfigureRuntimeExecution(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	awaitManaged(t, func() bool {
		task, err := f.server.Tasks.Get(ctx, "managed", childID)
		return err == nil && task.Status == taskqueue.StatusCancelled && task.PhysicalUsage.InputTokens == 10
	})
	evidenceMu.Lock()
	original := append([]byte(nil), evidence...)
	originalHeaders := headers.Clone()
	target := endpoint
	evidenceMu.Unlock()
	var stop runtimeprotocol.StoppedReceipt
	if err := json.Unmarshal(original, &stop); err != nil {
		t.Fatal(err)
	}
	if stop.ReceiptID != journal.Result.Identity() || stop.ResultDigest != journal.Result.Digest() {
		t.Fatal("restart changed receipt identity or digest")
	}
	send := func(body []byte) int {
		request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
		request.Header = originalHeaders.Clone()
		response := httptest.NewRecorder()
		f.server.Echo.ServeHTTP(response, request)
		return response.Code
	}
	if code := send(original); code != http.StatusNoContent {
		t.Fatalf("duplicate stop rejected: %d", code)
	}
	for _, alter := range []func(*runtimeprotocol.StoppedReceipt){
		func(s *runtimeprotocol.StoppedReceipt) { s.Result.Subject.UserID = "bob" },
		func(s *runtimeprotocol.StoppedReceipt) { s.Result.ClaimEpoch++ },
		func(s *runtimeprotocol.StoppedReceipt) { s.ResultDigest = strings.Repeat("0", 64) },
		func(s *runtimeprotocol.StoppedReceipt) {
			s.Result.UsageReceipt.InputTokens = 999
			s.ResultDigest = s.Result.Digest()
		},
	} {
		var invalid runtimeprotocol.StoppedReceipt
		_ = json.Unmarshal(original, &invalid)
		alter(&invalid)
		body, _ := json.Marshal(invalid)
		if code := send(body); code < 400 {
			t.Fatalf("tampered evidence accepted: %d", code)
		}
	}
	var count int
	var inputTokens int64
	if err := f.server.Pool.QueryRow(f.ctx, `SELECT count(*) FROM weave_task_stop_receipts WHERE task_id=$1`, childID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate evidence: %d %v", count, err)
	}
	if err := f.server.Pool.QueryRow(f.ctx, `SELECT (physical_usage->>'input_tokens')::bigint FROM weave_task_queue WHERE id=$1`, childID).Scan(&inputTokens); err != nil || inputTokens != 10 {
		t.Fatalf("duplicate/altered usage: %d %v", inputTokens, err)
	}
	runs, _ := filepath.Glob(filepath.Join(f.root, ".local-runtime", "*", ".subjects", "*", "home", "executions"))
	if len(runs) != 1 {
		t.Fatalf("execution homes: %v", runs)
	}
	contents, _ := os.ReadFile(runs[0])
	if string(contents) != "run\n" {
		t.Fatalf("restart re-executed CLI: %q", contents)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("caller did not observe cancellation")
	}
}

func TestManagedLocalRuntimeAbsoluteDeadlineAndMissingCredentialsRealPG(t *testing.T) {
	f := newManagedRuntimeFixture(t)
	host, err := f.server.ConfigureRuntimeExecution(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	ctx, parent := f.control(t, "alice", "deadline-parent", 1500*time.Millisecond)
	deadline := *parent.DeadlineAt
	_, err = f.execute(ctx, host, "block")
	if err == nil {
		t.Fatal("CLI ignored absolute task deadline")
	}
	var raw []byte
	var status string
	var storedDeadline time.Time
	if err := f.server.Pool.QueryRow(f.ctx, `SELECT result->>'status',deadline_at,physical_usage FROM weave_task_queue WHERE kind='engine_exec'`).Scan(&status, &storedDeadline, &raw); err != nil {
		t.Fatal(err)
	}
	if status != "timeout" || !storedDeadline.Equal(deadline.Truncate(time.Microsecond)) {
		t.Fatalf("deadline outcome changed: %s %v", status, storedDeadline)
	}
	var usage execution.TerminalUsage
	if err := json.Unmarshal(raw, &usage); err != nil || usage.InputTokens != 10 {
		t.Fatalf("deadline lost physical usage: %+v %v", usage, err)
	}
	bob, _ := f.control(t, "bob", "no-provider-parent")
	if err := f.server.Credentials.Delete(bob, "managed", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.execute(bob, host, "report"); err == nil {
		t.Fatal("missing user provider silently used another user's credentials")
	}
	runs, _ := filepath.Glob(filepath.Join(f.root, ".local-runtime", "*", ".subjects", "*", "home", "executions"))
	if len(runs) != 1 {
		t.Fatalf("unconfigured user launched CLI: %v", runs)
	}
}
