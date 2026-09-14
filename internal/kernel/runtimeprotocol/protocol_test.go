package runtimeprotocol

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
)

func TestV1ClaimAndReceiptRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	subject := execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"}
	frozenAgent := json.RawMessage(`{"id":"agent-1","version":7}`)
	claim := ExecutionClaim{
		SchemaVersion: ClaimSchemaV1, TaskID: "task-1", WorkspaceID: subject.WorkspaceID,
		Subject: subject, ClaimEpoch: 3, LeaseIssuedAt: now, LeaseExpiresAt: now.Add(time.Minute),
		Agent:   AgentIdentity{ID: "agent-1", Version: 7, Name: "reviewer", ExecutionScope: execution.ScopeTeamWorkerLeaf},
		Request: ExecutionRequest{SchemaVersion: RequestSchemaV1, Engine: "codex", Prompt: "review", FrozenAgent: frozenAgent, FrozenAgentHash: fmt.Sprintf("%x", sha256.Sum256(frozenAgent))},
	}
	response := ClaimResponse{Versioned: NewVersioned(), Claim: &claim}
	wire, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ClaimResponse
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Versioned.Validate(); err != nil {
		t.Fatal(err)
	}
	if decoded.Claim == nil {
		t.Fatal("claim was omitted")
	}
	if err := decoded.Claim.Validate(); err != nil {
		t.Fatal(err)
	}
	receipt := ExecutionReceipt{Versioned: NewVersioned(), SchemaVersion: ReceiptSchemaV1, TaskID: claim.TaskID, ClaimEpoch: claim.ClaimEpoch, Subject: subject, Status: "completed", Output: "done"}
	if err := receipt.ValidateFor(*decoded.Claim); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownVersionsFailClosed(t *testing.T) {
	if err := (Versioned{ProtocolVersion: "weave.runtime/v2"}).Validate(); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("protocol error=%v", err)
	}
	claim := ExecutionClaim{SchemaVersion: 2, Request: ExecutionRequest{SchemaVersion: RequestSchemaV1}}
	if err := claim.Validate(); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("claim error=%v", err)
	}
	receipt := ExecutionReceipt{Versioned: NewVersioned(), SchemaVersion: 2}
	if err := receipt.ValidateFor(ExecutionClaim{}); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("receipt error=%v", err)
	}
}

func TestReceiptCannotChangePhysicalOwner(t *testing.T) {
	subject := execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"}
	claim := ExecutionClaim{TaskID: "task-1", ClaimEpoch: 4, Subject: subject}
	receipt := ExecutionReceipt{Versioned: NewVersioned(), SchemaVersion: ReceiptSchemaV1, TaskID: claim.TaskID, ClaimEpoch: 5, Subject: subject, Status: "completed"}
	if err := receipt.ValidateFor(claim); err == nil {
		t.Fatal("receipt from another physical attempt was accepted")
	}
}

func TestStoppedReceiptPinsVersionIdentityAndObservedEvidence(t *testing.T) {
	subject := execution.Subject{WorkspaceID: "workspace", UserID: "alice"}
	claim := ExecutionClaim{TaskID: "physical-one", ClaimEpoch: 2, Subject: subject}
	result := ExecutionReceipt{Versioned: NewVersioned(), SchemaVersion: ReceiptSchemaV1, TaskID: claim.TaskID, ClaimEpoch: claim.ClaimEpoch, Subject: subject, Status: "timeout", Error: "cancelled", Output: "partial"}
	stop := StoppedReceipt{Versioned: NewVersioned(), SchemaVersion: ReceiptSchemaV1, TaskID: claim.TaskID, ClaimEpoch: claim.ClaimEpoch, Subject: subject, ReceiptID: result.Identity(), ResultDigest: result.Digest(), Result: &result}
	if err := stop.ValidateFor(claim); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(stop)
	for _, alter := range []func(*StoppedReceipt){
		func(s *StoppedReceipt) { s.ProtocolVersion = "weave.runtime/v2" },
		func(s *StoppedReceipt) { s.Result.Subject.UserID = "bob" },
		func(s *StoppedReceipt) { s.Result.ClaimEpoch++ },
		func(s *StoppedReceipt) { s.Result.Output = "changed" },
		func(s *StoppedReceipt) { s.ReceiptID = "another" },
		func(s *StoppedReceipt) { s.Result = nil },
	} {
		var invalid StoppedReceipt
		_ = json.Unmarshal(encoded, &invalid)
		alter(&invalid)
		if err := invalid.ValidateFor(claim); err == nil {
			t.Fatal("unbound stop evidence accepted")
		}
	}
}
