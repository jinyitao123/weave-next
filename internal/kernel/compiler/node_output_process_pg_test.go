package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

const (
	workbenchProcessPhaseEnv   = "WEAVE_WORKBENCH_PG_PHASE"
	workbenchProcessSchemaEnv  = "WEAVE_WORKBENCH_PG_SCHEMA"
	workbenchProcessGraphEnv   = "WEAVE_WORKBENCH_PG_GRAPH"
	workbenchProcessRunEnv     = "WEAVE_WORKBENCH_PG_RUN"
	workbenchProcessCounterEnv = "WEAVE_WORKBENCH_PG_COUNTER"
)

func TestWorkbenchResultCorrectionPersistsAcrossProcesses(t *testing.T) {
	if phase := os.Getenv(workbenchProcessPhaseEnv); phase != "" {
		runWorkbenchProcessPhase(t, phase)
		return
	}
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if !isDedicatedWorkbenchTestDatabase(databaseURL) {
		t.Skip("process recovery regression is restricted to the dedicated local PostgreSQL test cluster")
	}

	pool := testutil.PostgresPool(t)
	searchPath := pool.Config().ConnConfig.RuntimeParams["search_path"]
	if searchPath == "" {
		t.Fatal("isolated PostgreSQL schema is unavailable")
	}
	if _, err := pool.Exec(t.Context(), `CREATE TABLE workbench_action_counter (
		counter_key text PRIMARY KEY, calls integer NOT NULL, status text NOT NULL
	)`); err != nil {
		t.Fatal("create isolated action counter")
	}
	if _, err := pool.Exec(t.Context(), `CREATE TABLE workbench_process_stage (
		counter_key text NOT NULL, phase text NOT NULL, pid bigint NOT NULL,
		PRIMARY KEY(counter_key,phase)
	)`); err != nil {
		t.Fatal("create isolated subprocess evidence table")
	}
	graphName, runID, counterKey := t.Name(), uuid.NewString(), uuid.NewString()
	if !isWorkbenchTestSchema(searchPath) {
		t.Fatal("PostgreSQL helper did not create a uniquely named isolated schema")
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO workbench_action_counter(counter_key,calls,status) VALUES($1,0,'not_started')`, counterKey); err != nil {
		t.Fatal("seed isolated action counter")
	}

	for _, stage := range []struct {
		phase          string
		wantCorrection bool
		wantCalls      int
		wantStatus     string
	}{
		{phase: "action", wantCalls: 1, wantStatus: "succeeded"},
		{phase: "overlong", wantCorrection: true, wantCalls: 1, wantStatus: "succeeded"},
		{phase: "replay", wantCorrection: true, wantCalls: 1, wantStatus: "succeeded"},
		{phase: "complete", wantCalls: 1, wantStatus: "succeeded"},
	} {
		runWorkbenchProcess(t, stage.phase, searchPath, graphName, runID, counterKey)
		assertWorkbenchChildPID(t, pool, counterKey, stage.phase)
		assertWorkbenchPGCheckpoint(t, pool, graphName, runID, stage.wantCorrection)
		assertWorkbenchExternalAction(t, pool, counterKey, stage.wantCalls, stage.wantStatus)
	}
}

func runWorkbenchProcess(t *testing.T, phase, schema, graphName, runID, counterKey string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWorkbenchResultCorrectionPersistsAcrossProcesses$", "-test.timeout=2m")
	env := make([]string, 0, len(os.Environ())+5)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == workbenchProcessPhaseEnv || key == workbenchProcessSchemaEnv || key == workbenchProcessGraphEnv || key == workbenchProcessRunEnv || key == workbenchProcessCounterEnv {
			continue
		}
		env = append(env, entry)
	}
	command.Env = append(env,
		workbenchProcessPhaseEnv+"="+phase,
		workbenchProcessSchemaEnv+"="+schema,
		workbenchProcessGraphEnv+"="+graphName,
		workbenchProcessRunEnv+"="+runID,
		workbenchProcessCounterEnv+"="+counterKey,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("phase %q subprocess failed: %v\n%s", phase, err, strings.TrimSpace(string(output)))
	}
}

func runWorkbenchProcessPhase(t *testing.T, phase string) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if !isDedicatedWorkbenchTestDatabase(databaseURL) {
		t.Fatal("child process is outside the dedicated local PostgreSQL test cluster")
	}
	schema := os.Getenv(workbenchProcessSchemaEnv)
	graphName := os.Getenv(workbenchProcessGraphEnv)
	runID := os.Getenv(workbenchProcessRunEnv)
	counterKey := os.Getenv(workbenchProcessCounterEnv)
	if schema == "" || graphName == "" || runID == "" || counterKey == "" {
		t.Fatal("child process fixture identity is incomplete")
	}
	if !isWorkbenchTestSchema(schema) {
		t.Fatal("child process fixture is outside the random isolated schema")
	}

	databaseURL = workbenchDatabaseURL(t, schema)
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal("open isolated action ledger")
	}
	defer pool.Close()
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatal("connect isolated action ledger")
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO workbench_process_stage(counter_key,phase,pid) VALUES($1,$2,$3)`, counterKey, phase, os.Getpid()); err != nil {
		t.Fatal("record subprocess identity")
	}
	store := openWorkbenchPGStore(t, schema)
	defer store.Close()
	tools := &pgWorkbenchActionDispatcher{pool: pool, counterKey: counterKey}
	llm := &schemaTestLLM{responses: workbenchProcessResponses(t, phase)}
	graph := newWorkbenchControlledGraph(graphName, llm, tools)
	checkCalls := 0
	check := func(ctx context.Context, _ string) (bool, string, error) {
		checkCalls++
		var calls int
		var status string
		if err := pool.QueryRow(ctx, `SELECT calls,status FROM workbench_action_counter WHERE counter_key=$1`, counterKey).Scan(&calls, &status); err != nil {
			return false, "", err
		}
		if calls == 0 {
			return false, "required action receipt missing", nil
		}
		if calls != 1 || status != "succeeded" {
			return false, "required action receipt is not uniquely confirmed", nil
		}
		return true, "", nil
	}
	ctx := WithNodeCompletionCheck(
		WithWorkbenchResultOutput(WithNodeOutputSchema(t.Context(), machine.WorkbenchResultSchemaV1())),
		"persisted-action-receipt-v1", check,
	)

	var result *loom.RunResult
	if phase == "action" {
		input := schemaInput()
		input["__run_id"] = runID
		result, err = graph.Run(ctx, input, store)
	} else {
		delta := prepareWorkbenchProcessResume(t, store, graphName, runID, "grant-"+phase)
		result, err = graph.Resume(ctx, runID, delta, store)
	}
	if err != nil || result == nil {
		t.Fatalf("phase %q run failed: %v", phase, err)
	}

	switch phase {
	case "action":
		if !result.Yielded || checkCalls != 0 {
			t.Fatalf("initial action did not pause: yielded=%v checks=%d", result.Yielded, checkCalls)
		}
		assertWorkbenchExternalAction(t, pool, counterKey, 1, "succeeded")
	case "overlong":
		if !result.Yielded || checkCalls != 0 || !hasWorkbenchCorrectionMarker(result.State) {
			t.Fatalf("overlong output did not persist correction state: yielded=%v checks=%d", result.Yielded, checkCalls)
		}
	case "replay":
		if !result.Yielded || checkCalls != 0 || !hasWorkbenchCorrectionMarker(result.State) {
			t.Fatalf("replay phase lost correction state: yielded=%v checks=%d", result.Yielded, checkCalls)
		}
		messages, ok := result.State["__toolloop_msgs"].([]contract.Message)
		if !ok || !messagesContain(messages, "Tools are unavailable while correcting") {
			t.Fatal("new process did not block the replayed tool call")
		}
		assertWorkbenchExternalAction(t, pool, counterKey, 1, "succeeded")
	case "complete":
		valid := workbenchProcessValidOutput(t)
		if result.Yielded || result.State["output"] != valid || checkCalls != 1 || hasWorkbenchCorrectionMarker(result.State) {
			t.Fatalf("valid result did not complete: yielded=%v checks=%d", result.Yielded, checkCalls)
		}
		if len(llm.requests) != 1 || !messagesContain(llm.requests[0].Messages, "Tools are unavailable while correcting") {
			t.Fatal("final process did not restore the blocked tool result from PostgreSQL")
		}
		assertWorkbenchExternalAction(t, pool, counterKey, 1, "succeeded")
	default:
		t.Fatalf("unknown child phase %q", phase)
	}
}

func workbenchProcessResponses(t *testing.T, phase string) []contract.ChatResponse {
	t.Helper()
	switch phase {
	case "action":
		return []contract.ChatResponse{{StopReason: "tool_calls", ToolCalls: []contract.ToolCall{{ID: "write-1", Name: "submit_material", Args: `{}`}}}}
	case "overlong":
		return []contract.ChatResponse{{StopReason: "stop", Content: workbenchOutput(t, strings.Repeat("界", 1033))}}
	case "replay":
		return []contract.ChatResponse{{StopReason: "tool_calls", ToolCalls: []contract.ToolCall{{ID: "write-2", Name: "submit_material", Args: `{}`}}}}
	case "complete":
		return []contract.ChatResponse{{StopReason: "stop", Content: workbenchProcessValidOutput(t)}}
	default:
		t.Fatalf("unknown phase %q", phase)
		return nil
	}
}

func prepareWorkbenchProcessResume(t *testing.T, store loom.Store, graphName, runID, grantID string) loom.State {
	t.Helper()
	raw, err := store.Get(t.Context(), "checkpoint:"+graphName, runID)
	if err != nil {
		t.Fatal("read PostgreSQL checkpoint")
	}
	var checkpoint struct {
		State loom.State `json:"state"`
	}
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatal("decode PostgreSQL checkpoint")
	}
	outcome, present, err := stdlib.ReadToolLoopOutcome(checkpoint.State)
	if err != nil || !present {
		t.Fatalf("read controlled pause: present=%v err=%v", present, err)
	}
	seq, ok := checkpointSequence(checkpoint.State["__checkpoint_seq"])
	if !ok {
		t.Fatal("checkpoint sequence is missing")
	}
	token, ok := checkpoint.State["__yield_token"].(string)
	if !ok || token == "" {
		t.Fatal("checkpoint yield token is missing")
	}
	delta, err := stdlib.PrepareToolLoopResume(checkpoint.State, stdlib.ToolLoopResumeGrant{
		ID: grantID, ExpectedRunID: runID, ExpectedCheckpointSeq: seq, ExpectedYieldToken: token,
		ExpectedSlice: outcome.Slice, AuthorizedTotalRounds: 4,
	})
	if err != nil {
		t.Fatal("prepare PostgreSQL checkpoint resume")
	}
	return delta
}

func checkpointSequence(value any) (int64, bool) {
	switch sequence := value.(type) {
	case int:
		return int64(sequence), true
	case int64:
		return sequence, true
	case float64:
		return int64(sequence), sequence >= 1
	default:
		return 0, false
	}
}

func workbenchProcessValidOutput(t *testing.T) string {
	t.Helper()
	summary := strings.Repeat("中", 998) + "😀🙂"
	if utf8.RuneCountInString(summary) != 1000 {
		t.Fatal("final fixture is not exactly 1000 Unicode code points")
	}
	return workbenchOutput(t, summary)
}

func hasWorkbenchCorrectionMarker(state loom.State) bool {
	marker, _ := state[workbenchResultCorrectionStateKey].(string)
	return strings.HasPrefix(marker, "workbench_result_v1:")
}

func assertWorkbenchExternalAction(t *testing.T, pool *pgxpool.Pool, counterKey string, wantCalls int, wantStatus string) {
	t.Helper()
	var calls int
	var status string
	if err := pool.QueryRow(t.Context(), `SELECT calls,status FROM workbench_action_counter WHERE counter_key=$1`, counterKey).Scan(&calls, &status); err != nil {
		t.Fatal("read isolated external action ledger")
	}
	if calls != wantCalls || status != wantStatus {
		t.Fatalf("external action calls=%d status=%q want calls=%d status=%q", calls, status, wantCalls, wantStatus)
	}
}

func assertWorkbenchChildPID(t *testing.T, pool *pgxpool.Pool, counterKey, phase string) {
	t.Helper()
	var pid int64
	if err := pool.QueryRow(t.Context(), `SELECT pid FROM workbench_process_stage WHERE counter_key=$1 AND phase=$2`, counterKey, phase).Scan(&pid); err != nil {
		t.Fatal("child process identity was not persisted")
	}
	if pid == int64(os.Getpid()) {
		t.Fatal("phase ran in the parent process")
	}
	var distinct int
	if err := pool.QueryRow(t.Context(), `SELECT count(DISTINCT pid) FROM workbench_process_stage WHERE counter_key=$1`, counterKey).Scan(&distinct); err != nil {
		t.Fatal("could not verify distinct subprocess identities")
	}
	var stages int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM workbench_process_stage WHERE counter_key=$1`, counterKey).Scan(&stages); err != nil {
		t.Fatal("could not count subprocess stages")
	}
	if distinct != stages {
		t.Fatalf("phases reused a process id: distinct=%d phases=%d", distinct, stages)
	}
}

type pgWorkbenchActionDispatcher struct {
	pool       *pgxpool.Pool
	counterKey string
}

func (*pgWorkbenchActionDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{{Name: "submit_material", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}

func (d *pgWorkbenchActionDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE workbench_action_counter SET calls=calls+1,status='succeeded' WHERE counter_key=$1`, d.counterKey)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, errors.New("external action counter row is missing")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &contract.ToolResult{CallID: call.ID, Content: `{"status":"succeeded"}`}, nil
}

func isDedicatedWorkbenchTestDatabase(databaseURL string) bool {
	connection, err := url.Parse(databaseURL)
	if err != nil || connection.User == nil {
		return false
	}
	_, hasPassword := connection.User.Password()
	host := connection.Hostname()
	return (host == "127.0.0.1" || host == "localhost" || host == "::1") && connection.Port() == "55439" &&
		connection.Path == "/postgres" && connection.User.Username() == "postgres" && !hasPassword
}

func isWorkbenchTestSchema(schema string) bool {
	if strings.HasPrefix(schema, `"`) && strings.HasSuffix(schema, `"`) {
		schema = strings.TrimSuffix(strings.TrimPrefix(schema, `"`), `"`)
	}
	if !strings.HasPrefix(schema, "test_") {
		return false
	}
	_, err := uuid.Parse(strings.TrimPrefix(schema, "test_"))
	return err == nil
}

func workbenchDatabaseURL(t *testing.T, schema string) string {
	t.Helper()
	connection, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || !isDedicatedWorkbenchTestDatabase(os.Getenv("TEST_DATABASE_URL")) {
		t.Fatal("PostgreSQL URL is outside the dedicated local test cluster")
	}
	query := connection.Query()
	query.Set("search_path", schema)
	connection.RawQuery = query.Encode()
	return connection.String()
}
