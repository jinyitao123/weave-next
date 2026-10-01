package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

type dispatchInputHTTPFixture struct {
	server *httptest.Server
	log    *bytes.Buffer
	token  string
	lines  int
}

func newDispatchInputHTTPFixture(t *testing.T, handler *Server) *dispatchInputHTTPFixture {
	t.Helper()
	const secret = "dispatch-input-route-test-secret"
	e := echo.New()
	log := &bytes.Buffer{}
	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
		Output: log,
		Format: "${status} ${method} ${uri}\n",
	}))
	e.POST("/v1/workbench/dispatch-inputs", handler.handleRegisterDispatchInput,
		AuthMiddleware(secret, nil, nil), RequireScope("org"), RequireScope("chat"))
	handler.Echo = e
	server := httptest.NewServer(e)
	claims := Claims{
		TenantID: "ws", UserID: "user", Roles: []string{"admin"},
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return &dispatchInputHTTPFixture{server: server, log: log, token: token}
}

func (f *dispatchInputHTTPFixture) post(t *testing.T, request dispatchInputRegistration, forgeToken string) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/workbench/dispatch-inputs", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+f.token)
	httpRequest.Header.Set("Content-Type", "application/json")
	if forgeToken != "" {
		httpRequest.Header.Set(forgeDelegationHeader, "Bearer "+forgeToken)
	}
	response, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	f.lines++
	assertDispatchInputHTTPLogStatus(t, f.log, f.lines, response.StatusCode)
	return response.StatusCode, responseBody
}

func assertDispatchInputHTTPLogStatus(t *testing.T, output *bytes.Buffer, expectedLines, expectedStatus int) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) == 0 || len(lines) == 1 && lines[0] == "" {
		t.Fatal("HTTP route emitted no access log line")
	}
	if len(lines) != expectedLines {
		t.Fatalf("HTTP route wrote %d access log lines, want %d: %q", len(lines), expectedLines, output.String())
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 3 || fields[0] != fmt.Sprint(expectedStatus) {
		t.Fatalf("HTTP status=%d but latest access log is %q", expectedStatus, lines[len(lines)-1])
	}
}

func assertDispatchInputHTTPError(t *testing.T, status int, body []byte, wantStatus int, wantCode string) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("HTTP status=%d want=%d body=%s", status, wantStatus, body)
	}
	var payload struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&payload); err != nil {
		t.Fatalf("error response is not JSON: %v body=%s", err, body)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("error response contains another JSON value: second decode=%v body=%s", err, body)
	}
	if payload.Code != wantCode || payload.Error == "" {
		t.Fatalf("error response=%+v want code=%q body=%s", payload, wantCode, body)
	}
}

type dispatchInputWriteCounts struct {
	inputs            int64
	delegations       int64
	admissionRequests int64
	admissionReceipts int64
	tasks             int64
	runSnapshots      int64
}

func dispatchInputCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) dispatchInputWriteCounts {
	t.Helper()
	var counts dispatchInputWriteCounts
	err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM weave_dispatch_input_revisions),
		(SELECT count(*) FROM weave_task_business_delegations),
		(SELECT count(*) FROM weave_workflow_admission_requests),
		(SELECT count(*) FROM weave_workflow_version_admission_receipts),
		(SELECT count(*) FROM weave_task_queue),
		(SELECT count(*) FROM weave_team_run_snapshots)`).Scan(
		&counts.inputs, &counts.delegations, &counts.admissionRequests,
		&counts.admissionReceipts, &counts.tasks, &counts.runSnapshots,
	)
	if err != nil {
		t.Fatal(err)
	}
	return counts
}

func assertDispatchInputNoWrites(t *testing.T, before, after dispatchInputWriteCounts) {
	t.Helper()
	if before != after {
		t.Fatalf("rejected registration wrote input, delegation, admission, task, or run data: before=%+v after=%+v", before, after)
	}
}

func dispatchInputDelegationRequest(session string, material dispatchInputResource) dispatchInputRegistration {
	request := dispatchInputRegistrationFixture(session, "复核这份冻结的合同原件", "")
	version := 1
	request.WorkflowID = "flow"
	request.WorkflowVersion = &version
	request.AuthorizedBusinessCapabilityIDs = &[]string{}
	request.BusinessRecord = &dispatchBusinessRecord{ObjectName: "sales_contract", RecordID: "contract-a"}
	if material.ID != "" {
		request.Resources = []dispatchInputResource{material}
	}
	return request
}

func TestDispatchInputDelegationPreparationRejectionsWriteOnceOverHTTPRealPG(t *testing.T) {
	t.Setenv("WEAVE_SECRET_KEY_FILE", "")
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("11", 32))
	server, pool := newTeamDispatchTestServer(t)
	ctx := t.Context()
	materialBytes := []byte("%PDF-1.7\ncontract original")
	materialSHA := dispatchInputDigest(materialBytes)
	material := dispatchInputResource{
		Type: "forge-file", SourceKind: "owner", MaterialID: strings.Repeat("a", 24),
		ID: "contract-original", Name: "contract.pdf", MediaType: "application/pdf",
		Bytes: int64(len(materialBytes)), SHA256: materialSHA,
	}
	authority := newTaskGrantHTTPFixture(t)
	authority.files[material.ID] = materialBytes
	authority.rejectFiles.Store(true)
	server.Config = &config.Config{ForgeSessionURL: authority.server.URL + "/api/v1/auth/me"}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id,native_organization)
		VALUES($1,'forge-user','ws','user','native-org'),($1,'forge-other-user','ws','user-other','native-org')`, "forge:task-delegation-test"); err != nil {
		t.Fatal(err)
	}
	server.ExternalIdentity = externalIdentityVerifierFunc(func(context.Context, string) (ExternalIdentity, error) {
		t.Fatal("task delegation attempted to revalidate a general employee session")
		return ExternalIdentity{}, errors.New("unreachable")
	})
	route := newDispatchInputHTTPFixture(t, server)
	request := dispatchInputDelegationRequest("http-rejections", material)
	withoutFile := request
	withoutFile.Resources = nil
	authority.add(t, "valid", "forge-user", "native-org", 1, scopeForRegistration(request))
	authority.add(t, "valid-no-file", "forge-user", "native-org", 1, scopeForRegistration(withoutFile))
	authority.add(t, "organization-mismatch", "forge-user", "other-native-org", 1, scopeForRegistration(withoutFile))
	authority.add(t, "employee-mismatch", "forge-other-user", "native-org", 1, scopeForRegistration(withoutFile))
	authority.add(t, "missing-identity", "forge-missing-user", "native-org", 1, scopeForRegistration(withoutFile))
	baseline := dispatchInputCounts(t, ctx, pool)
	assertRejected := func(request dispatchInputRegistration, forgeToken string, wantStatus int, wantCode string) {
		t.Helper()
		status, body := route.post(t, request, forgeToken)
		assertDispatchInputHTTPError(t, status, body, wantStatus, wantCode)
		assertDispatchInputNoWrites(t, baseline, dispatchInputCounts(t, ctx, pool))
	}

	assertRejected(request, "", http.StatusUnauthorized, "business_delegation_required")
	assertRejected(request, "invalid", http.StatusUnauthorized, "business_delegation_invalid")
	assertRejected(withoutFile, "organization-mismatch", http.StatusForbidden, "business_delegation_identity_mismatch")
	assertRejected(withoutFile, "missing-identity", http.StatusForbidden, "business_delegation_identity_mismatch")
	assertRejected(withoutFile, "employee-mismatch", http.StatusForbidden, "business_delegation_identity_mismatch")

	t.Setenv("WEAVE_SECRET_KEY", "")
	assertRejected(withoutFile, "valid-no-file", http.StatusServiceUnavailable, "business_delegation_unavailable")
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("11", 32))

	assertRejected(request, "valid", http.StatusUnprocessableEntity, "business_resource_invalid")
	if lastIfMatch, _ := authority.lastIfMatch.Load().(string); lastIfMatch != `"`+materialSHA+`"` {
		t.Fatalf("Forge received If-Match %q; expected quoted SHA-256 ETag", lastIfMatch)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE weave_external_identities RENAME TO weave_external_identities_unavailable`); err != nil {
		t.Fatal(err)
	}
	assertRejected(withoutFile, "valid-no-file", http.StatusInternalServerError, "workflow_store_failed")
}

func TestDispatchInputLegalForgeOriginalRegistersAndRecoversLegacyInputOverHTTPRealPG(t *testing.T) {
	t.Setenv("WEAVE_SECRET_KEY_FILE", "")
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("22", 32))
	server, pool := newTeamDispatchTestServer(t)
	ctx := t.Context()
	materialBytes := []byte("%PDF-1.7\nlegally accessible contract")
	materialSHA := dispatchInputDigest(materialBytes)
	material := dispatchInputResource{
		Type: "forge-file", SourceKind: "owner", MaterialID: strings.Repeat("b", 24),
		ID: "contract-original", Name: "contract.pdf", MediaType: "application/pdf",
		Bytes: int64(len(materialBytes)), SHA256: materialSHA,
	}
	authority := newTaskGrantHTTPFixture(t)
	authority.files[material.ID] = materialBytes
	authority.rejectFiles.Store(true)
	server.Config = &config.Config{ForgeSessionURL: authority.server.URL + "/api/v1/auth/me"}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id,native_organization)
		VALUES($1,'forge-user','ws','user','native-org')`, "forge:task-delegation-test"); err != nil {
		t.Fatal(err)
	}
	server.ExternalIdentity = externalIdentityVerifierFunc(func(context.Context, string) (ExternalIdentity, error) {
		t.Fatal("task delegation attempted to revalidate a general employee session")
		return ExternalIdentity{}, errors.New("unreachable")
	})
	route := newDispatchInputHTTPFixture(t, server)
	request := dispatchInputDelegationRequest("legacy-http-session", material)
	authority.add(t, "valid", "forge-user", "native-org", 1, scopeForRegistration(request))
	baseline := dispatchInputCounts(t, ctx, pool)

	status, body := route.post(t, request, "valid")
	assertDispatchInputHTTPError(t, status, body, http.StatusUnprocessableEntity, "business_resource_invalid")
	assertDispatchInputNoWrites(t, baseline, dispatchInputCounts(t, ctx, pool))

	// Recreate the exact durable state left by the old bug: the failed request's
	// registration fingerprint and input revision exist, while its delegation
	// was never saved. The retry below must restore the delegation on that same
	// registration and return the same receipt.
	legacyReceipt := seedLegacyDispatchInputWithoutDelegation(t, ctx, pool, request)
	request.InputRevisionID = legacyReceipt.InputRevisionID
	authority.add(t, "valid", "forge-user", "native-org", 1, scopeForRegistration(request))
	legacyCounts := dispatchInputCounts(t, ctx, pool)
	if legacyCounts.inputs != baseline.inputs+1 || legacyCounts.delegations != baseline.delegations {
		t.Fatalf("legacy fixture does not match failed historical registration: %+v", legacyCounts)
	}
	// The same issuer now accepts the quoted frozen ETag, matching the live API.
	authority.rejectFiles.Store(false)
	status, body = route.post(t, request, "valid")
	if status != http.StatusOK {
		t.Fatalf("legacy recovery status=%d body=%s", status, body)
	}
	var recovered dispatchInputReceipt
	if err := json.Unmarshal(body, &recovered); err != nil || recovered != legacyReceipt {
		t.Fatalf("legacy retry receipt=%+v want=%+v err=%v", recovered, legacyReceipt, err)
	}
	if lastIfMatch, _ := authority.lastIfMatch.Load().(string); lastIfMatch != `"`+materialSHA+`"` {
		t.Fatalf("Forge received If-Match %q; expected quoted SHA-256 ETag", lastIfMatch)
	}
	counts := dispatchInputCounts(t, ctx, pool)
	if counts.inputs != legacyCounts.inputs || counts.delegations != legacyCounts.delegations+1 || counts.admissionRequests != baseline.admissionRequests || counts.admissionReceipts != baseline.admissionReceipts {
		t.Fatalf("legacy retry did not restore only its delegation: %+v", counts)
	}

	newRequest := dispatchInputDelegationRequest("legal-http-session", material)
	newRequest.Task = "再核对另一份固定合同原件"
	newRequest.SourceMessages[0].SHA256 = dispatchInputDigest([]byte(newRequest.Task))
	authority.add(t, "valid-new", "forge-user", "native-org", 2, scopeForRegistration(newRequest))
	status, body = route.post(t, newRequest, "valid-new")
	if status != http.StatusCreated {
		t.Fatalf("valid Forge resource registration status=%d body=%s", status, body)
	}
	var created dispatchInputReceipt
	if err := json.Unmarshal(body, &created); err != nil || created.InputRevisionID == "" {
		t.Fatalf("valid registration receipt=%+v err=%v body=%s", created, err, body)
	}
	if lastIfMatch, _ := authority.lastIfMatch.Load().(string); lastIfMatch != `"`+materialSHA+`"` {
		t.Fatalf("Forge received If-Match %q; expected quoted SHA-256 ETag", lastIfMatch)
	}
	counts = dispatchInputCounts(t, ctx, pool)
	if counts.inputs != legacyCounts.inputs+1 || counts.delegations != legacyCounts.delegations+2 || counts.admissionRequests != baseline.admissionRequests || counts.admissionReceipts != baseline.admissionReceipts || counts.tasks != baseline.tasks || counts.runSnapshots != baseline.runSnapshots {
		t.Fatalf("legal registration wrote unexpected data: %+v", counts)
	}
}

func seedLegacyDispatchInputWithoutDelegation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, request dispatchInputRegistration) dispatchInputReceipt {
	t.Helper()
	normalized := request
	normalized.Mode = teamDispatchModeWorkflow
	rawRegistration, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := json.Marshal(normalized.SourceMessages)
	if err != nil {
		t.Fatal(err)
	}
	receipt := dispatchInputReceipt{
		InputRevisionID: uuid.NewString(), ClientRequestID: uuid.NewString(),
		TaskSHA256: dispatchInputDigest([]byte(normalized.Task)),
	}
	_, err = pool.Exec(ctx, `INSERT INTO weave_dispatch_input_revisions
		(workspace_id,user_id,workbench_session_id,input_revision_id,registration_id,registration_sha256,
		 source_messages,task,task_sha256,team_id,mode,workflow_id,workflow_version,project_id,client_request_id,
		 execution_task,revision_kind,root_input_revision_id)
		VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10,$11,$12,$13,$14,$15,$8,'initial',$4)`,
		"ws", "user", normalized.WorkbenchSessionID, receipt.InputRevisionID, normalized.RegistrationID,
		dispatchInputDigest(rawRegistration), string(sources), normalized.Task, receipt.TaskSHA256,
		normalized.TeamID, normalized.Mode, normalized.WorkflowID, *normalized.WorkflowVersion,
		normalized.ProjectID, receipt.ClientRequestID,
	)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}
