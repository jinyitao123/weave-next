package api

import (
	"net/http"
	"testing"

	"github.com/jinyitao123/weave/internal/app/capabilities"
)

func TestCapabilityWorkflowAndInlineInvocationsShareWorkspaceQuotaRealPG(t *testing.T) {
	t.Run("inline activity blocks workflow admission and replay uses no new slot", func(t *testing.T) {
		f := newCapabilityWorkflowContractFixture(t)
		f.publishInlineCapability(t, "inline")
		f.publishCapability(t, "workflow")
		app, key := f.application(t, "inline first quota app")
		f.grant(t, app.ID, "inline", true)
		f.grant(t, app.ID, "workflow", true)
		if _, err := capabilities.NewPGStore(f.pool).SetQuota(t.Context(), "ws", capabilities.Quota{
			MaxStepsPerInvocation: 100, MaxActiveInvocations: 1,
		}); err != nil {
			t.Fatal(err)
		}

		inlinePath := workflowInvocationPath("inline", 1)
		inlineRequest := map[string]any{"request_id": "quota-inline-first", "input": map[string]any{"document": "Report"}}
		first := f.call(t, key, http.MethodPost, inlinePath, inlineRequest)
		if first.status != http.StatusAccepted {
			t.Fatalf("initial inline invocation status=%d body=%+v", first.status, first.body)
		}
		replay := f.call(t, key, http.MethodPost, inlinePath, inlineRequest)
		if replay.status != http.StatusAccepted || !replay.boolField(t, "replayed") {
			t.Fatalf("exact inline replay consumed a new slot: status=%d body=%+v", replay.status, replay.body)
		}
		blocked := f.call(t, key, http.MethodPost, workflowInvocationPath("workflow", 1), map[string]any{
			"request_id": "quota-workflow-blocked", "input": map[string]any{"document": "Report"},
		})
		if blocked.status != http.StatusTooManyRequests || blocked.stringField(t, "code") != "capability_quota_exceeded" {
			t.Fatalf("workflow admission ignored active inline call: status=%d body=%+v", blocked.status, blocked.body)
		}
		inlineID := replay.stringField(t, "invocation_id")
		stopped := f.call(t, key, http.MethodPost, "/v1/invocations/"+inlineID+"/cancel", nil)
		if stopped.status != http.StatusOK {
			t.Fatalf("cancel inline invocation to release quota status=%d body=%+v", stopped.status, stopped.body)
		}
		accepted := f.call(t, key, http.MethodPost, workflowInvocationPath("workflow", 1), map[string]any{
			"request_id": "quota-workflow-after-inline-cancel", "input": map[string]any{"document": "Report"},
		})
		if accepted.status != http.StatusAccepted {
			t.Fatalf("workflow admission did not use released quota: status=%d body=%+v", accepted.status, accepted.body)
		}
	})

	t.Run("workflow activity blocks inline admission", func(t *testing.T) {
		f := newCapabilityWorkflowContractFixture(t)
		f.publishInlineCapability(t, "inline")
		f.publishCapability(t, "workflow")
		app, key := f.application(t, "workflow first quota app")
		f.grant(t, app.ID, "inline", true)
		f.grant(t, app.ID, "workflow", true)
		if _, err := capabilities.NewPGStore(f.pool).SetQuota(t.Context(), "ws", capabilities.Quota{
			MaxStepsPerInvocation: 100, MaxActiveInvocations: 1,
		}); err != nil {
			t.Fatal(err)
		}

		workflowRequest := map[string]any{
			"request_id": "quota-workflow-first", "input": map[string]any{"document": "Report"},
		}
		workflow := f.call(t, key, http.MethodPost, workflowInvocationPath("workflow", 1), workflowRequest)
		if workflow.status != http.StatusAccepted {
			t.Fatalf("initial workflow invocation status=%d body=%+v", workflow.status, workflow.body)
		}
		replay := f.call(t, key, http.MethodPost, workflowInvocationPath("workflow", 1), workflowRequest)
		if replay.status != http.StatusAccepted || !replay.boolField(t, "replayed") ||
			replay.stringField(t, "run_id") != workflow.stringField(t, "run_id") {
			t.Fatalf("exact workflow replay consumed a new quota slot: status=%d body=%+v", replay.status, replay.body)
		}
		blocked := f.call(t, key, http.MethodPost, workflowInvocationPath("inline", 1), map[string]any{
			"request_id": "quota-inline-blocked", "input": map[string]any{"document": "Report"},
		})
		if blocked.status != http.StatusTooManyRequests || blocked.stringField(t, "code") != "capability_quota_exceeded" {
			t.Fatalf("inline admission ignored active workflow call: status=%d body=%+v", blocked.status, blocked.body)
		}
	})
}
