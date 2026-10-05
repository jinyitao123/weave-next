package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/labstack/echo/v4"
)

func TestDispatchInputSourceMessagesRejectAmbiguousProvenance(t *testing.T) {
	zero, one, negative := int64(0), int64(1), int64(-1)
	valid := dispatchInputSourceMessage{MessageID: "user-1", EventSeq: &zero, SHA256: strings.Repeat("a", 64)}
	if !validDispatchInputSourceMessages([]dispatchInputSourceMessage{valid, {MessageID: "user-2", EventSeq: &one, SHA256: strings.Repeat("b", 64)}}) {
		t.Fatal("valid ordered user source references rejected")
	}
	for _, test := range []struct {
		name     string
		messages []dispatchInputSourceMessage
	}{
		{"empty", nil},
		{"missing_sequence", []dispatchInputSourceMessage{{MessageID: "user-1", SHA256: valid.SHA256}}},
		{"negative_sequence", []dispatchInputSourceMessage{{MessageID: "user-1", EventSeq: &negative, SHA256: valid.SHA256}}},
		{"duplicate_sequence", []dispatchInputSourceMessage{valid, {MessageID: "user-2", EventSeq: &zero, SHA256: valid.SHA256}}},
		{"duplicate_message", []dispatchInputSourceMessage{valid, {MessageID: "user-1", EventSeq: &one, SHA256: valid.SHA256}}},
		{"invalid_hash", []dispatchInputSourceMessage{{MessageID: "user-1", EventSeq: &zero, SHA256: strings.Repeat("z", 64)}}},
		{"short_hash", []dispatchInputSourceMessage{{MessageID: "user-1", EventSeq: &zero, SHA256: "ab"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if validDispatchInputSourceMessages(test.messages) {
				t.Fatal("ambiguous source references accepted")
			}
		})
	}
}

func TestDispatchInputFrozenResourceBudgetMatchesMaterialContract(t *testing.T) {
	resource := dispatchInputResource{
		Type: "forge-file", SourceKind: "owner", ID: "file-a", Name: "材料.pdf", MaterialID: strings.Repeat("a", 24),
		MediaType: "application/pdf", Bytes: dispatchInputResourceMaxBytes, SHA256: strings.Repeat("a", 64),
	}
	if !validDispatchInputResources([]dispatchInputResource{resource}) {
		t.Fatal("accepted contract-sized original was rejected")
	}
	tooLarge := resource
	tooLarge.Bytes++
	if validDispatchInputResources([]dispatchInputResource{tooLarge}) {
		t.Fatal("accepted an original larger than 2 MiB")
	}
	missingMaterialID := resource
	missingMaterialID.MaterialID = ""
	if validDispatchInputResources([]dispatchInputResource{missingMaterialID}) {
		t.Fatal("accepted a binary source without its frozen material ID")
	}
	textWithoutMaterialID := dispatchInputResource{
		Type: "forge-file", ID: "file-text", Name: "材料.txt", MediaType: "text/plain",
		Bytes: 12, SHA256: strings.Repeat("b", 64),
	}
	if !validDispatchInputResources([]dispatchInputResource{textWithoutMaterialID}) {
		t.Fatal("rejected the optional material ID for a text resource")
	}
	approval := resource
	approval.SourceKind, approval.RequestID = "approval", "request-a"
	if !validDispatchInputResources([]dispatchInputResource{approval}) {
		t.Fatal("rejected a frozen approval source binding")
	}
	approval.RequestID = ""
	if validDispatchInputResources([]dispatchInputResource{approval}) {
		t.Fatal("accepted an approval source without its request ID")
	}
	ownerWithRequest := resource
	ownerWithRequest.RequestID = "request-a"
	if validDispatchInputResources([]dispatchInputResource{ownerWithRequest}) {
		t.Fatal("accepted an owner source with an approval request ID")
	}
	missingSource := resource
	missingSource.SourceKind = ""
	if validDispatchInputResources([]dispatchInputResource{missingSource}) {
		t.Fatal("accepted a binary material without a frozen source route")
	}
	resources := make([]dispatchInputResource, 4)
	for index := range resources {
		resources[index] = dispatchInputResource{
			Type: "forge-file", ID: fmt.Sprintf("file-%d", index), Name: "材料.txt",
			Bytes: dispatchInputResourceMaxBytes, SHA256: strings.Repeat("b", 64),
		}
	}
	if !validDispatchInputResources(resources) {
		t.Fatal("rejected the 8 MiB aggregate original limit")
	}
	resources = append(resources, dispatchInputResource{
		Type: "forge-file", ID: "file-extra", Name: "额外材料.txt", Bytes: 1, SHA256: strings.Repeat("c", 64),
	})
	if validDispatchInputResources(resources) {
		t.Fatal("accepted originals above the 8 MiB aggregate limit")
	}
	smallReferences := make([]dispatchInputResource, 9)
	for index := range smallReferences {
		smallReferences[index] = dispatchInputResource{
			Type: "forge-file", ID: fmt.Sprintf("small-file-%d", index), Name: fmt.Sprintf("材料-%d.txt", index),
			Bytes: 1, SHA256: fmt.Sprintf("%064x", index+1),
		}
	}
	if !validDispatchInputResources(smallReferences) {
		t.Fatal("rejected nine small file references below the unchanged aggregate byte limit")
	}
}

func TestAuthorizedBusinessActionsRequireAnExplicitPublishedSubset(t *testing.T) {
	actions := []string{
		"forge:action:sales_contract.ContractSubmit",
		"forge:action:sales_contract.RequestRevision",
	}
	published := publishedActionScope{actions: actions, receiptChecked: true}
	if _, err := authorizedBusinessActions(published, nil); err == nil {
		t.Fatal("missing task scope was accepted for a workflow with business actions")
	}
	empty := []string{}
	if got, err := authorizedBusinessActions(published, &empty); err != nil || len(got) != 0 {
		t.Fatalf("material-only scope=%v err=%v", got, err)
	}
	requested := []string{actions[1], actions[0]}
	got, err := authorizedBusinessActions(published, &requested)
	if err != nil || len(got) != 2 || got[0] != actions[0] || got[1] != actions[1] {
		t.Fatalf("authorized scope=%v err=%v", got, err)
	}
	outside := []string{"forge:action:sales_contract.Delete"}
	if _, err := authorizedBusinessActions(published, &outside); err == nil {
		t.Fatal("action outside the published workflow was accepted")
	}
	duplicate := []string{actions[0], actions[0]}
	if _, err := authorizedBusinessActions(published, &duplicate); err == nil {
		t.Fatal("duplicate action scope was accepted")
	}
}

func TestAuthorizedBusinessActionsRequireDeclaredReceiptCheck(t *testing.T) {
	unchecked := publishedActionScope{actions: []string{"forge:action:crm_lead.ConvertLead"}, executable: map[string]bool{"forge:action:crm_lead.ConvertLead": true}}
	requested := []string{"forge:action:crm_lead.ConvertLead"}
	if _, err := authorizedBusinessActions(unchecked, &requested); !errors.Is(err, errBusinessCompletionCheckRequired) {
		t.Fatalf("an unchecked published version authorized an action: %v", err)
	}
	inert := publishedActionScope{actions: unchecked.actions, executable: map[string]bool{}}
	if got, err := authorizedBusinessActions(inert, &requested); err != nil || len(got) != 1 {
		t.Fatalf("an action no graph member can execute = %v, %v", got, err)
	}
	readOnly := []string{}
	if got, err := authorizedBusinessActions(unchecked, &readOnly); err != nil || len(got) != 0 {
		t.Fatalf("read-only dispatch to an unchecked version = %v, %v", got, err)
	}
}

func TestAuthorizedBusinessActionsAllowOmissionWhenWorkflowHasNone(t *testing.T) {
	got, err := authorizedBusinessActions(publishedActionScope{}, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("scope=%v err=%v", got, err)
	}
}

func TestEmptyBusinessActionScopeDoesNotRequireDelegationWithoutResources(t *testing.T) {
	if !ensurePreparedActions(nil, nil) {
		t.Fatal("empty business action scope unexpectedly required a delegation")
	}
	if ensurePreparedActions(nil, []string{"forge:action:sales_contract.ContractSubmit"}) {
		t.Fatal("non-empty business action scope was accepted without a delegation")
	}
}

func TestBoundDispatchWirePreservesOmissionAndExactText(t *testing.T) {
	input := dispatchInputRevision{Task: " \n甲：“引号”\n", TeamID: "team", Mode: "workflow", WorkflowID: "flow", WorkflowVersion: 1}
	input.ClientRequestID = "fixed-key"
	for _, test := range []struct {
		name string
		body string
		want bool
	}{
		{"omitted", `{}`, true},
		{"exact", `{"task":" \n甲：“引号”\n"}`, true},
		{"trailing_newline_lost", `{"task":" \n甲：“引号”"}`, false},
		{"empty_assertion", `{"task":""}`, false},
		{"wrong_nonce", `{"client_request_id":"another-key"}`, false},
		{"empty_workflow_assertion", `{"workflow_id":""}`, false},
		{"different_version", `{"workflow_version":2}`, false},
		{"conversation_substitution", `{"conversation_id":"old-conversation"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var wire teamDispatchWireRequest
			if err := json.Unmarshal([]byte(test.body), &wire); err != nil {
				t.Fatal(err)
			}
			if got := wire.matchesInput("team", input); got != test.want {
				t.Fatalf("match=%v want=%v", got, test.want)
			}
		})
	}
	for _, body := range []string{`{"task":null}`, `{"client_request_id":null}`, `{"workflow_version":null}`} {
		var wire teamDispatchWireRequest
		if json.Unmarshal([]byte(body), &wire) == nil {
			t.Fatalf("explicit null was treated as omission: %s", body)
		}
	}
}

func TestBusinessRecordBindingRejectsAmbiguousIdentity(t *testing.T) {
	if !validDispatchBusinessRecord(nil) || !validDispatchBusinessRecord(&dispatchBusinessRecord{ObjectName: "sales_quote", RecordID: "quote-a"}) {
		t.Fatal("valid binding rejected")
	}
	for _, record := range []dispatchBusinessRecord{
		{ObjectName: "sales_quote"}, {ObjectName: "sys_user", RecordID: "a"},
		{ObjectName: " sales_quote", RecordID: "a"}, {ObjectName: "sales_quote", RecordID: " a "},
		{ObjectName: "sales_quote", RecordID: "a\x00b"},
	} {
		if validDispatchBusinessRecord(&record) {
			t.Fatalf("invalid binding accepted: %+v", record)
		}
	}
}

func dispatchInputFilesForTest(count int) []dispatchInputResource {
	files := make([]dispatchInputResource, count)
	for index := range files {
		files[index] = dispatchInputResource{
			Type: "forge-file", ID: fmt.Sprintf("file-%02d", index), Name: fmt.Sprintf("材料%02d.md", index),
			MediaType: "text/markdown", Bytes: 100, SHA256: strings.Repeat("c", 64),
		}
	}
	return files
}

// Registration and the runtime share frozen.MaxDelegatedFiles: the largest set
// registration accepts is exactly what the runtime can start.
func TestDispatchInputResourceCountMatchesTheRuntimeLimit(t *testing.T) {
	if !validDispatchInputResources(dispatchInputFilesForTest(frozen.MaxDelegatedFiles)) {
		t.Fatalf("%d files, the documented maximum, were rejected", frozen.MaxDelegatedFiles)
	}
	if validDispatchInputResources(dispatchInputFilesForTest(frozen.MaxDelegatedFiles + 1)) {
		t.Fatalf("%d files were accepted although the runtime cannot start them", frozen.MaxDelegatedFiles+1)
	}
}

// An oversized handoff is refused with a specific code before any state is
// written, instead of being accepted and then failing when the run starts.
func TestDispatchInputRegistrationRejectsTooManyFilesWithASpecificCodeRealPG(t *testing.T) {
	server, pool := newTeamDispatchTestServer(t)
	files := dispatchInputFilesForTest(frozen.MaxDelegatedFiles + 1)
	body, err := json.Marshal(map[string]any{
		"registration_id": uuid.NewString(), "workbench_session_id": "session", "team_id": "team",
		"task": "检查", "resources": files,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/workbench/dispatch-inputs", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	c := echo.New().NewContext(request, recorder)
	c.Set("tenant", "ws")
	c.Set("user_id", "user")
	if err := server.handleRegisterDispatchInput(c); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "dispatch_input_too_many_resources") {
		t.Fatalf("status=%d body=%s, want 409 dispatch_input_too_many_resources", recorder.Code, recorder.Body.String())
	}
	var stored int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_dispatch_input_revisions`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("a rejected handoff left %d input revisions err=%v", stored, err)
	}
}
