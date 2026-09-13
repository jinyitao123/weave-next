package runtimeagent

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

//go:embed testdata/full-definition.json
var completeDefinition []byte

func fullClaim(raw []byte) runtimeprotocol.ExecutionClaim {
	now := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	return runtimeprotocol.ExecutionClaim{
		SchemaVersion: runtimeprotocol.ClaimSchemaV1, TaskID: "task", WorkspaceID: "workspace",
		Subject: execution.Subject{WorkspaceID: "workspace", UserID: "user"}, ClaimEpoch: 4,
		LeaseIssuedAt: now, LeaseExpiresAt: now.Add(time.Minute),
		Agent:   runtimeprotocol.AgentIdentity{ID: "agent", Name: "worker", Version: 7, ExecutionScope: execution.ScopeLegacyOrchestrator},
		Request: runtimeprotocol.ExecutionRequest{SchemaVersion: runtimeprotocol.RequestSchemaV1, Engine: "loom", FrozenAgent: raw, FrozenAgentHash: fmt.Sprintf("%x", sha256.Sum256(raw))},
	}
}

func TestDecodePreservesCompleteFrozenDefinition(t *testing.T) {
	claim := fullClaim(completeDefinition)
	record, err := Decode(claim)
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]any
	if err = json.Unmarshal(completeDefinition, &expected); err != nil {
		t.Fatal(err)
	}
	// The fixture explicitly exercises every serializable record field. New
	// execution fields require a deliberate fixture update rather than a smaller
	// Host DTO silently dropping them at the repository boundary.
	schema := reflect.TypeOf(registry.AgentRecord{})
	for i := 0; i < schema.NumField(); i++ {
		field := schema.Field(i)
		key := strings.Split(field.Tag.Get("json"), ",")[0]
		if key == "-" {
			continue
		}
		if key == "" {
			key = field.Name
		}
		if _, ok := expected[key]; !ok {
			t.Errorf("frozen fixture omits field %s", key)
		}
	}
	actualJSON, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]any
	if err = json.Unmarshal(actualJSON, &actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("frozen execution definition changed\nwant: %s\ngot: %s", completeDefinition, actualJSON)
	}
	if record.RuntimePolicyMode != "" || record.RuntimePoolID != "" {
		t.Fatal("local runtime policy leaked into frozen definition")
	}
	record.MCPServers[0].Headers["X-Task"] = "changed"
	record.Spec.Skills[0].Body = "changed"
	again, err := Decode(claim)
	if err != nil {
		t.Fatal(err)
	}
	if again.MCPServers[0].Headers["X-Task"] != "frozen-test" || again.Spec.Skills[0].Body != "exact frozen skill body" {
		t.Fatal("decoded record mutation changed the frozen claim")
	}
}

func TestDecodeRejectsMismatchedFrozenIdentity(t *testing.T) {
	for _, field := range []string{"workspace_id", "id", "name", "version"} {
		t.Run(field, func(t *testing.T) {
			var fields map[string]any
			if err := json.Unmarshal(completeDefinition, &fields); err != nil {
				t.Fatal(err)
			}
			if field == "version" {
				fields[field] = 8
			} else {
				fields[field] = "other"
			}
			raw, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Decode(fullClaim(raw)); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
				t.Fatalf("mismatched %s accepted: %v", field, err)
			}
		})
	}
	claim := fullClaim(completeDefinition)
	claim.Request.FrozenAgentHash = "different"
	if _, err := Decode(claim); err == nil {
		t.Fatal("definition with invalid hash accepted")
	}
	claim = fullClaim(completeDefinition)
	claim.Subject.WorkspaceID = "other"
	if _, err := Decode(claim); err == nil {
		t.Fatal("foreign execution subject accepted")
	}
}
