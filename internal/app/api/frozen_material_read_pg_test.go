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
		"forge-file-A": []byte("%PDF-1.7\nRAW-ORIGINAL-A"),
		"forge-file-B": []byte("%PDF-1.7\nRAW-ORIGINAL-B"),
	}
	requestPaths := map[string]string{
		"forge-file-A": "/api/v1/workbench/materials/forge-file-A/original",
		"forge-file-B": "/api/v1/approvals/requests/approval-request-B/workbench-history/files/forge-file-B/original",
	}
	var forgeReads atomic.Int32
	var forgeRequests atomic.Int32
	var failOriginal atomic.Bool
	forge := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer fixture-token" ||
			request.Header.Get("Accept-Encoding") != "identity" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		var fileID string
		for id, path := range requestPaths {
			if request.URL.Path == path {
				fileID = id
				break
			}
		}
		content, ok := fileBytes[fileID]
		if !ok {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		forgeRequests.Add(1)
		fileSHA := dispatchInputDigest(content)
		if request.Header.Get("If-Match") != `"`+fileSHA+`"` {
			writer.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		if failOriginal.Load() {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		forgeReads.Add(1)
		writer.Header().Set("Content-Type", "application/pdf")
		writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		writer.Header().Set("ETag", `"`+fileSHA+`"`)
		writer.Header().Set("X-Content-SHA256", fileSHA)
		_, _ = writer.Write(content)
	}))
	defer forge.Close()
	server.ExternalIdentity = externalIdentityVerifierFunc(func(_ context.Context, token string) (ExternalIdentity, error) {
		if token != "fixture-token" {
			return ExternalIdentity{}, fmt.Errorf("unexpected token")
		}
		return ExternalIdentity{Issuer: forge.URL, BaseURL: forge.URL, Subject: "forge-user", Organization: "ws"}, nil
	})
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id)
		VALUES($1,'forge-user','ws','user')`, forge.URL); err != nil {
		t.Fatal(err)
	}

	makeTask := func(materialID, name string, content []byte, extracted, status string, limitations []string) (string, string) {
		t.Helper()
		if limitations == nil {
			limitations = []string{}
		}
		fileSHA := dispatchInputDigest(content)
		extractionBytes := []byte(extracted)
		extractionSHA := dispatchInputDigest(extractionBytes)
		coverage := map[string]any{"pdfPageCount": 1, "pdfTextPageCount": 1, "pdfPagesWithoutText": []int{}}
		if status == "partial" {
			coverage = map[string]any{"pdfPageCount": 2, "pdfTextPageCount": 1, "pdfPagesWithoutText": []int{2}}
		}
		task, err := json.Marshal(map[string]any{
			"goal":             "按本次冻结材料核对正文",
			"materialHandling": "只依据当前冻结材料及其提取状态；partial须说明未读内容。",
			"materials": []any{map[string]any{
				"materialId": materialID, "name": name, "mediaType": "application/pdf",
				"bytes": len(content), "sha256": fileSHA,
				"extraction": map[string]any{
					"status": status, "mediaType": "text/plain; charset=utf-8", "bytes": len(extractionBytes),
					"sha256": extractionSHA, "sourceSha256": fileSHA, "content": extracted,
					"extractor": "pdfjs-dist", "coverage": coverage, "limitations": limitations,
				},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(task), fileSHA
	}
	registerAndDispatch := func(session, materialID, name, fileID, extracted, status string, limitations []string) (workflowManualRunResponse, string, string) {
		t.Helper()
		original := fileBytes[fileID]
		task, fileSHA := makeTask(materialID, name, original, extracted, status, limitations)
		registration := dispatchInputRegistrationFixture(session, task, "")
		version := 1
		registration.WorkflowID, registration.WorkflowVersion = "flow", &version
		registration.Resources = []dispatchInputResource{{
			Type: "forge-file", SourceKind: "owner", MaterialID: materialID, ID: fileID, Name: name,
			MediaType: "application/pdf", Bytes: int64(len(original)), SHA256: fileSHA,
		}}
		if fileID == "forge-file-B" {
			registration.Resources[0].SourceKind = "approval"
			registration.Resources[0].RequestID = "approval-request-B"
		}
		body, err := json.Marshal(registration)
		if err != nil {
			t.Fatal(err)
		}
		c, recorder := dispatchInputTestContext(body, "/v1/workbench/dispatch-inputs", "ws", "user")
		setTestForgeTaskDelegation(c.Request().Header, "fixture-token")
		err = server.handleRegisterDispatchInput(c)
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
			strings.Contains(executionTask, "approval-request-B") || strings.Contains(executionTask, `"sourceKind"`) ||
			!strings.Contains(executionTask, materialID) || !strings.Contains(executionTask, fileSHA) ||
			!strings.Contains(executionTask, `"status":"`+status+`"`) ||
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
	runA, hashA, executionTaskA := registerAndDispatch("material-session-A", materialAID, "材料A.pdf", "forge-file-A", "A-ONLY UTF8提取正文。"+strings.Repeat("补充A。", 6), "partial", []string{"page-without-text"})
	runB, hashB, executionTaskB := registerAndDispatch("material-session-B", materialBID, "材料B.pdf", "forge-file-B", "B-SECRET提取正文不得外泄", "complete", nil)
	runCtxA := startRun(runA, "material-worker-A", executionTaskA)
	runCtxB := startRun(runB, "material-worker-B", executionTaskB)
	if forgeReads.Load() != 2 {
		t.Fatalf("expected one registration-time digest check per original file, got %d", forgeReads.Load())
	}

	readStore := businessaction.NewStore(pool, server.Tasks, []byte(strings.Repeat("\x22", 32)))
	dispatcher, err := readStore.MaterialReadDispatcher(runCtxA)
	if err != nil || dispatcher == nil {
		t.Fatalf("material read dispatcher=%T err=%v", dispatcher, err)
	}
	tools, err := dispatcher.ListTools(runCtxA)
	if err != nil || len(tools) != 1 || tools[0].Name != "read_frozen_material" || !tools[0].ReadOnly {
		t.Fatalf("unexpected Loom material tools=%+v err=%v", tools, err)
	}
	var readA struct {
		Status                     string   `json:"status"`
		Source                     string   `json:"source"`
		OriginalVerificationStatus string   `json:"originalVerificationStatus"`
		OriginalVerificationCode   string   `json:"originalVerificationCode"`
		MaterialID                 string   `json:"materialId"`
		SHA256                     string   `json:"sha256"`
		Extractor                  string   `json:"extractor"`
		Content                    string   `json:"content"`
		Limitations                []string `json:"limitations"`
		Offset                     int      `json:"offset"`
		NextOffset                 *int     `json:"nextOffset"`
		HasMore                    bool     `json:"hasMore"`
	}
	callAt := func(target context.Context, materialID, digest string, maxBytes int, offset *int) (*contract.ToolResult, error) {
		values := map[string]any{"materialId": materialID, "sha256": digest, "maxBytes": maxBytes}
		if offset != nil {
			values["offset"] = *offset
		}
		args, err := json.Marshal(values)
		if err != nil {
			return nil, err
		}
		return dispatcher.Dispatch(target, contract.ToolCall{ID: "read-material", Name: "read_frozen_material", Args: string(args)})
	}
	call := func(materialID, digest string, maxBytes int) (*contract.ToolResult, error) {
		return callAt(runCtxA, materialID, digest, maxBytes, nil)
	}
	resultA, err := call(materialAID, hashA, 64)
	if err != nil || resultA.IsError {
		t.Fatalf("read current frozen material result=%+v err=%v", resultA, err)
	}
	if err := json.Unmarshal([]byte(resultA.Content), &readA); err != nil {
		t.Fatal(err)
	}
	if readA.Status != "partial" || readA.Source != "workbench-extraction" || readA.OriginalVerificationStatus != "verified" ||
		readA.MaterialID != materialAID || readA.SHA256 != hashA || readA.Extractor != "pdfjs-dist" ||
		!strings.Contains(readA.Content, "A-ONLY UTF8提取正文") || strings.Contains(readA.Content, "B-SECRET") ||
		strings.Contains(readA.Content, "RAW-ORIGINAL-A") || len(readA.Limitations) != 1 || !readA.HasMore ||
		readA.NextOffset == nil || *readA.NextOffset <= readA.Offset {
		t.Fatalf("current UTF-8 material excerpt lost scope/status: %+v ForgeReads=%d ForgeRequests=%d", readA, forgeReads.Load(), forgeRequests.Load())
	}
	failOriginal.Store(true)
	unavailableOriginal, err := callAt(runCtxA, materialAID, hashA, 64, readA.NextOffset)
	if err != nil || unavailableOriginal.IsError {
		t.Fatalf("unavailable original endpoint should preserve extracted text: result=%+v err=%v", unavailableOriginal, err)
	}
	var fallback struct {
		Status                     string `json:"status"`
		OriginalVerificationStatus string `json:"originalVerificationStatus"`
		Content                    string `json:"content"`
	}
	if err := json.Unmarshal([]byte(unavailableOriginal.Content), &fallback); err != nil || fallback.Status != "partial" ||
		fallback.OriginalVerificationStatus != "unavailable" || fallback.Content == "" || strings.Contains(fallback.Content, "RAW-ORIGINAL-A") {
		t.Fatalf("Forge outage hid or mislabeled the text extraction: %+v err=%v", fallback, err)
	}
	failOriginal.Store(false)
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
	wrongEmployeeCtx := execution.WithSubject(runCtxA, execution.Subject{WorkspaceID: "ws", UserID: "other-user"})
	wrongEmployeeResult, err := dispatcher.Dispatch(wrongEmployeeCtx, contract.ToolCall{ID: "wrong-employee", Name: "read_frozen_material", Args: string(mustMaterialReadArgs(t, materialAID, hashA, 64))})
	if err != nil || wrongEmployeeResult.IsError {
		t.Fatalf("non-owner read should return unavailable: result=%+v err=%v", wrongEmployeeResult, err)
	}
	var wrongEmployeeRead struct {
		Status  string `json:"status"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(wrongEmployeeResult.Content), &wrongEmployeeRead); err != nil ||
		wrongEmployeeRead.Status != "unavailable" || wrongEmployeeRead.Content != "" {
		t.Fatalf("non-owner read received material text: %+v err=%v", wrongEmployeeRead, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_task_business_delegations
		SET expires_at=issued_at+interval '1 millisecond'
		WHERE workspace_id='ws' AND input_revision_id=$1`, runA.InputRevisionID); err != nil {
		t.Fatal(err)
	}
	expiredResult, err := dispatcher.Dispatch(runCtxA, contract.ToolCall{ID: "expired", Name: "read_frozen_material", Args: string(mustMaterialReadArgs(t, materialAID, hashA, 64))})
	if err != nil || expiredResult.IsError {
		t.Fatalf("expired delegation should return unavailable: result=%+v err=%v", expiredResult, err)
	}
	var expiredRead struct {
		Status                     string `json:"status"`
		OriginalVerificationStatus string `json:"originalVerificationStatus"`
		Content                    string `json:"content"`
	}
	if err := json.Unmarshal([]byte(expiredResult.Content), &expiredRead); err != nil || expiredRead.Status != "unavailable" ||
		expiredRead.OriginalVerificationStatus != "unavailable" || expiredRead.Content != "" {
		t.Fatalf("expired delegation exposed material: %+v err=%v", expiredRead, err)
	}
	if forgeReads.Load() != 3 {
		t.Fatalf("cross-run, wrong-user, or expired scope triggered Forge reads: GET count=%d", forgeReads.Load())
	}
	dispatcherB, err := readStore.MaterialReadDispatcher(runCtxB)
	if err != nil || dispatcherB == nil {
		t.Fatalf("approval material read dispatcher=%T err=%v", dispatcherB, err)
	}
	argsB, _ := json.Marshal(map[string]any{"materialId": materialBID, "sha256": hashB, "maxBytes": 64})
	resultB, err = dispatcherB.Dispatch(runCtxB, contract.ToolCall{ID: "read-approval-material", Name: "read_frozen_material", Args: string(argsB)})
	if err != nil || resultB.IsError {
		t.Fatalf("read frozen approval material result=%+v err=%v", resultB, err)
	}
	if err := json.Unmarshal([]byte(resultB.Content), &readA); err != nil {
		t.Fatal(err)
	}
	if readA.Status != "complete" || readA.OriginalVerificationStatus != "verified" ||
		!strings.Contains(readA.Content, "B-SECRET提取正文") || strings.Contains(readA.Content, "RAW-ORIGINAL-B") {
		t.Fatalf("approval route did not preserve extraction-only model content and original verification: %+v", readA)
	}
	if forgeReads.Load() != 4 || forgeRequests.Load() != 5 {
		t.Fatalf("expected two registrations, two successful source reads, one transient failure, and no rejected-scope requests: successful=%d requests=%d", forgeReads.Load(), forgeRequests.Load())
	}
}

func mustMaterialReadArgs(t *testing.T, materialID, digest string, maxBytes int) []byte {
	t.Helper()
	args, err := json.Marshal(map[string]any{"materialId": materialID, "sha256": digest, "maxBytes": maxBytes})
	if err != nil {
		t.Fatal(err)
	}
	return args
}
