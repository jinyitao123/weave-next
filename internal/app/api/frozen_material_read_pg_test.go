package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

func TestLoomMaterialReadStaysWithinFrozenRunInputRealPG(t *testing.T) {
	t.Setenv("WEAVE_SECRET_KEY_FILE", "")
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("22", 32))
	server, pool := newTeamDispatchTestServer(t)
	fileBytes := map[string][]byte{
		"forge-file-A": []byte("UTF-8 original source A"),
		"forge-file-B": []byte("UTF-8 original source B"),
	}
	var forgeReads atomic.Int32
	forge := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer fixture-token" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		fileID := strings.TrimPrefix(request.URL.Path, "/api/v1/storage/files/")
		content, ok := fileBytes[fileID]
		if !ok {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		forgeReads.Add(1)
		_, _ = writer.Write(content)
	}))
	defer forge.Close()
	server.ExternalIdentity = externalIdentityVerifierFunc(func(_ context.Context, token string) (ExternalIdentity, error) {
		if token != "fixture-token" {
			return ExternalIdentity{}, fmt.Errorf("unexpected token")
		}
		return ExternalIdentity{Issuer: forge.URL, Subject: "forge-user", Organization: "ws"}, nil
	})
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id)
		VALUES($1,'forge-user','ws','user')`, forge.URL); err != nil {
		t.Fatal(err)
	}

	makeTask := func(materialID, name string, content []byte, extracted string) (string, string) {
		t.Helper()
		fileSHA := dispatchInputDigest(content)
		extractionBytes := []byte(extracted)
		extractionSHA := dispatchInputDigest(extractionBytes)
		task, err := json.Marshal(map[string]any{
			"goal":             "按本次冻结材料核对正文",
			"materialHandling": "只依据当前冻结材料及其提取状态；partial须说明未读内容。",
			"materials": []any{map[string]any{
				"materialId": materialID, "name": name, "mediaType": "text/plain",
				"bytes": len(content), "sha256": fileSHA,
				"extraction": map[string]any{
					"status": "complete", "mediaType": "text/plain; charset=utf-8", "bytes": len(extractionBytes),
					"sha256": extractionSHA, "sourceSha256": fileSHA, "content": extracted,
					"extractor": "utf8", "coverage": map[string]any{}, "limitations": []string{},
				},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(task), fileSHA
	}
	registerAndDispatch := func(session, materialID, name, fileID, extracted string) (workflowManualRunResponse, string, string) {
		t.Helper()
		original := fileBytes[fileID]
		task, fileSHA := makeTask(materialID, name, original, extracted)
		registration := dispatchInputRegistrationFixture(session, task, "")
		version := 1
		registration.WorkflowID, registration.WorkflowVersion = "flow", &version
		registration.Resources = []dispatchInputResource{{
			Type: "forge-file", MaterialID: materialID, ID: fileID, Name: name,
			MediaType: "text/plain", Bytes: int64(len(original)), SHA256: fileSHA,
		}}
		recorder, err := registerInputForTest(server, registration)
		if err != nil || recorder.Code != http.StatusCreated {
			t.Fatalf("register input status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
		}
		var input dispatchInputReceipt
		if err := json.Unmarshal(recorder.Body.Bytes(), &input); err != nil {
			t.Fatal(err)
		}
		dispatchRecorder, err := boundDispatchForTest(server, map[string]any{
			"input_revision_id": input.InputRevisionID, "client_request_id": input.ClientRequestID,
		}, "user")
		if err != nil || dispatchRecorder.Code != http.StatusCreated {
			t.Fatalf("dispatch status=%d body=%s err=%v", dispatchRecorder.Code, dispatchRecorder.Body.String(), err)
		}
		var run workflowManualRunResponse
		if err := json.Unmarshal(dispatchRecorder.Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		var storedTask, storedTaskSHA, executionTask string
		if err := pool.QueryRow(t.Context(), `SELECT task,task_sha256,execution_task FROM weave_dispatch_input_revisions
			WHERE workspace_id='ws' AND input_revision_id=$1`, input.InputRevisionID).
			Scan(&storedTask, &storedTaskSHA, &executionTask); err != nil {
			t.Fatal(err)
		}
		if storedTask != task || storedTaskSHA != input.TaskSHA256 || storedTaskSHA != dispatchInputDigest([]byte(storedTask)) ||
			!strings.Contains(storedTask, extracted) || strings.Contains(executionTask, extracted) ||
			!strings.Contains(executionTask, materialID) || !strings.Contains(executionTask, fileSHA) ||
			!strings.Contains(executionTask, `"status":"complete"`) ||
			!strings.Contains(executionTask, `"sourceSha256":"`+fileSHA+`"`) {
			t.Fatalf("input identity or Loom projection changed: rawHash=%s receiptHash=%s executionTask=%s",
				storedTaskSHA, input.TaskSHA256, executionTask)
		}
		return run, fileSHA, executionTask
	}
	startRun := func(run workflowManualRunResponse, workerID, expectedExecutionTask string) context.Context {
		t.Helper()
		ctx := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user"})
		claimed, err := server.Tasks.Claim(ctx, workerID, taskqueue.ClaimFilter{
			Kind: "team_workflow", WorkspaceID: "ws", IdentityKind: taskqueue.IdentityTeamWorkflow,
		})
		if err != nil || claimed == nil || claimed.ID != run.TaskID {
			t.Fatalf("claim task=%+v runTask=%s err=%v", claimed, run.TaskID, err)
		}
		var queuedTask string
		if err := json.Unmarshal(claimed.Payload, &queuedTask); err != nil || queuedTask != expectedExecutionTask {
			t.Fatalf("queued Loom task differs from frozen runtime projection: got=%q want=%q err=%v", queuedTask, expectedExecutionTask, err)
		}
		runs := teamrun.NewPGStore()
		runs.Transactions = pool
		consumer := &teamrun.Consumer{Transactions: pool, Snapshots: server.Snapshots, Runs: runs, Tasks: server.Tasks}
		queued, err := consumer.ConsumeClaimed(ctx, claimed, workerID)
		if err != nil || queued.RunID != run.RunID || queued.Status != teamrun.StatusQueued {
			t.Fatalf("establish run=%+v err=%v", queued, err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = runs.ClaimRunningTx(ctx, tx, teamrun.ClaimRequest{
			WorkspaceID: "ws", RunID: queued.RunID, ExpectedStatus: teamrun.StatusQueued,
			ExpectedTeamRunGeneration: queued.Generation, ExpectedExecutionLeaseEpoch: queued.ExecutionLeaseEpoch,
			ExpectedResumeGeneration: queued.ResumeGeneration, ExecutorID: workerID,
			IdempotencyKey: "material-read-claim:" + workerID, Actor: workerID, Source: "test", OccurredAt: time.Now().UTC(),
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		bound, err := taskqueue.BindTaskExecution(ctx, claimed, server.Tasks)
		if err != nil {
			t.Fatal(err)
		}
		return bound
	}

	materialAID, materialBID := "aaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbb"
	runA, hashA, executionTaskA := registerAndDispatch("material-session-A", materialAID, "材料A.txt", "forge-file-A", "A-ONLY UTF8正文。"+strings.Repeat("补充A。", 6))
	runB, hashB, executionTaskB := registerAndDispatch("material-session-B", materialBID, "材料B.txt", "forge-file-B", "B-SECRET正文不得外泄")
	runCtxA := startRun(runA, "material-worker-A", executionTaskA)
	startRun(runB, "material-worker-B", executionTaskB)
	if forgeReads.Load() != 2 {
		t.Fatalf("expected one registration-time digest check per original file, got %d", forgeReads.Load())
	}

	readStore := businessaction.NewStore(pool, server.Tasks, []byte(strings.Repeat("22", 32)))
	dispatcher, err := readStore.MaterialReadDispatcher(runCtxA)
	if err != nil || dispatcher == nil {
		t.Fatalf("material read dispatcher=%T err=%v", dispatcher, err)
	}
	tools, err := dispatcher.ListTools(runCtxA)
	if err != nil || len(tools) != 1 || tools[0].Name != "read_frozen_material" || !tools[0].ReadOnly {
		t.Fatalf("unexpected Loom material tools=%+v err=%v", tools, err)
	}
	var readA struct {
		Status           string   `json:"status"`
		Source           string   `json:"source"`
		BinaryReadStatus string   `json:"binaryReadStatus"`
		MaterialID       string   `json:"materialId"`
		SHA256           string   `json:"sha256"`
		Extractor        string   `json:"extractor"`
		Content          string   `json:"content"`
		Limitations      []string `json:"limitations"`
		Offset           int      `json:"offset"`
		NextOffset       *int     `json:"nextOffset"`
		HasMore          bool     `json:"hasMore"`
	}
	call := func(materialID, digest string, maxBytes int) (*contract.ToolResult, error) {
		args, err := json.Marshal(map[string]any{"materialId": materialID, "sha256": digest, "maxBytes": maxBytes})
		if err != nil {
			return nil, err
		}
		return dispatcher.Dispatch(runCtxA, contract.ToolCall{ID: "read-material", Name: "read_frozen_material", Args: string(args)})
	}
	resultA, err := call(materialAID, hashA, 64)
	if err != nil || resultA.IsError {
		t.Fatalf("read current frozen material result=%+v err=%v", resultA, err)
	}
	if err := json.Unmarshal([]byte(resultA.Content), &readA); err != nil {
		t.Fatal(err)
	}
	if readA.Status != "complete" || readA.Source != "workbench-extraction" || readA.BinaryReadStatus != "unavailable" ||
		readA.MaterialID != materialAID || readA.SHA256 != hashA || readA.Extractor != "utf8" ||
		!strings.Contains(readA.Content, "A-ONLY UTF8正文") || strings.Contains(readA.Content, "B-SECRET") ||
		len(readA.Limitations) != 0 || !readA.HasMore || readA.NextOffset == nil || *readA.NextOffset <= readA.Offset {
		t.Fatalf("current UTF-8 material excerpt lost scope/status: %+v", readA)
	}
	resultB, err := call(materialBID, hashB, 64)
	if err != nil || resultB.IsError {
		t.Fatalf("cross-input read should return unavailable: result=%+v err=%v", resultB, err)
	}
	var readB struct {
		Status  string `json:"status"`
		Reason  string `json:"reason"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(resultB.Content), &readB); err != nil {
		t.Fatal(err)
	}
	if readB.Status != "unavailable" || readB.Reason != "material_not_in_current_run" || strings.Contains(readB.Content, "B-SECRET") {
		t.Fatalf("input A exposed input B: %+v", readB)
	}
	if forgeReads.Load() != 2 {
		t.Fatalf("Loom material reads must not fetch Forge binary files; Forge GET count=%d", forgeReads.Load())
	}
}
