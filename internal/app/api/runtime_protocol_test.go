package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func TestRuntimeMCPReferenceContextDelegatesOnlyExactUserBinding(t *testing.T) {
	alice := execution.Subject{WorkspaceID: "ws", UserID: "alice"}
	expected := frozen.CredentialReference{
		SchemaVersion: frozen.FrozenSchemaVersion,
		WorkspaceID:   "ws",
		Scope:         frozen.CredentialScopeWorkspaceService,
		ServiceID:     "mcp:server",
		Kind:          frozen.CredentialMCPServerAccess,
		ResourceID:    "server",
		Slot:          "access",
	}
	ctx := runtimeMCPReferenceContext(execution.WithSubject(context.Background(), alice), alice, expected)
	if err := frozen.AuthorizeCredentialReference(ctx, expected); err != nil {
		t.Fatalf("exact task user binding denied: %v", err)
	}

	changed := expected
	changed.ResourceID = "other"
	changed.ServiceID = "mcp:other"
	if err := frozen.AuthorizeCredentialReference(ctx, changed); err == nil {
		t.Fatal("different binding inherited task authorization")
	}

	bob := execution.Subject{WorkspaceID: "ws", UserID: "bob"}
	bobContext := runtimeMCPReferenceContext(execution.WithSubject(context.Background(), bob), alice, expected)
	if err := frozen.AuthorizeCredentialReference(bobContext, expected); err == nil {
		t.Fatal("different user inherited task authorization")
	}

	otherWorkspace := expected
	otherWorkspace.WorkspaceID = "other"
	if err := frozen.AuthorizeCredentialReference(ctx, otherWorkspace); err == nil {
		t.Fatal("different workspace inherited task authorization")
	}
}

func runtimeReceiptForTask(task *taskqueue.Task, result runtimes.EngineExecResult) runtimeprotocol.ExecutionReceipt {
	var receipt runtimeprotocol.ExecutionReceipt
	encoded, _ := json.Marshal(result)
	_ = json.Unmarshal(encoded, &receipt)
	receipt.Versioned = runtimeprotocol.NewVersioned()
	receipt.SchemaVersion = runtimeprotocol.ReceiptSchemaV1
	receipt.TaskID = task.ID
	receipt.ClaimEpoch = task.ClaimEpoch
	receipt.Subject = task.Subject
	return receipt
}

func setRuntimeTaskProof(request *http.Request, task *taskqueue.Task) {
	request.Header.Set(runtimeprotocol.HeaderVersion, runtimeprotocol.ProtocolVersion)
	request.Header.Set("X-Weave-Task-Epoch", strconv.FormatInt(task.ClaimEpoch, 10))
	request.Header.Set("X-Weave-Task-Subject", task.Subject.Digest())
}
