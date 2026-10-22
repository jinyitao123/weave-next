package workflow_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func TestOutputLimitFrozenPrimaryAndFallbackRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "output-limit", UserID: "alice"})
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('output-limit','output-limit','Output limit')`); err != nil {
		t.Fatal(err)
	}
	bodies := make(chan map[string]any, 8)
	var failPrimary atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies <- body
		if body["model"] == "primary" && failPrimary.Load() {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	store := credentials.New(pool, []byte("0123456789abcdef0123456789abcdef"))
	cfg := llmrouter.ProviderConfig{ID: "primary", Name: "Primary", Protocol: frozen.ProtocolOpenAICompletions, BaseURL: server.URL, ChatPath: "/v1/chat/completions", OutputLimitField: frozen.OutputLimitMaxCompletionTokens, Models: []string{"primary"}, APIKey: "synthetic-private-key", CredentialScope: frozen.CredentialScopeUser, CredentialUserID: "alice"}
	first, err := store.UpsertRevision(ctx, "output-limit", cfg)
	if err != nil {
		t.Fatal(err)
	}
	freeze := func(model string) frozen.FrozenModelBinding {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		binding, err := credentials.ResolveModelRevisionTx(ctx, tx, "output-limit", model)
		if err != nil {
			t.Fatal(err)
		}
		return binding
	}
	primary := freeze("primary")
	if primary.OutputLimitField != cfg.OutputLimitField {
		t.Fatal("declaration lost while freezing")
	}
	same, err := store.UpsertRevision(ctx, "output-limit", cfg)
	if err != nil || same.Advanced {
		t.Fatalf("same declaration advanced: %v", err)
	}
	cfg.OutputLimitField = frozen.OutputLimitMaxTokens
	changed, err := store.UpsertRevision(ctx, "output-limit", cfg)
	if err != nil || !changed.Advanced || changed.Revision.ContentHash == first.Revision.ContentHash {
		t.Fatalf("declaration not hashed: %v", err)
	}
	newPrimary := freeze("primary")
	if newPrimary.OutputLimitField != cfg.OutputLimitField || newPrimary.ContentHash == primary.ContentHash {
		t.Fatal("new declaration not frozen")
	}
	cfg.ID, cfg.Models = "fallback", []string{"fallback"}
	if _, err := store.UpsertRevision(ctx, "output-limit", cfg); err != nil {
		t.Fatal(err)
	}
	fallback := freeze("fallback")
	raw, err := json.Marshal(primary)
	if err != nil || strings.Contains(string(raw), cfg.APIKey) {
		t.Fatal("unsafe binding")
	}
	var restored frozen.FrozenModelBinding
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	resolver := nativeProfileResolver{pool: pool, store: store}
	opts, closer, err := workflow.NewRuntimeHostFactory().Build(ctx, frozen.FrozenExecutionBundle{Agent: frozen.FrozenAgentRecord{Model: "primary"}, PrimaryModel: restored, FallbackModels: []frozen.FrozenModelBinding{fallback}}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	request := contract.ChatRequest{Model: "primary", MaxTokens: 257, Messages: []contract.Message{{Role: "user", Content: "private prompt"}}}
	if response, err := opts.LLM.Chat(ctx, request); err != nil || response.Content != "done" {
		t.Fatalf("old frozen primary: %v", err)
	}
	check := func(model, present, absent string) {
		t.Helper()
		body := <-bodies
		if body["model"] != model || body[present] != float64(257) || body[absent] != nil {
			t.Fatalf("frozen wire changed: %v", body)
		}
	}
	check("primary", "max_completion_tokens", "max_tokens")
	failPrimary.Store(true)
	stream, err := opts.LLM.Stream(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
	check("primary", "max_completion_tokens", "max_tokens")
	check("fallback", "max_tokens", "max_completion_tokens")
	for _, field := range []string{"unknown", "max_tokens"} {
		bad := restored
		bad.OutputLimitField = field
		if field == "unknown" {
			if _, err := frozen.HashDTO(bad, frozen.PreorderFrozenModelBinding); err == nil {
				t.Fatal("invalid declaration hashed")
			}
		} else {
			hash, err := frozen.HashDTO(bad, frozen.PreorderFrozenModelBinding)
			if err != nil || hash == restored.ContentHash {
				t.Fatal("declaration absent from frozen digest")
			}
		}
	}
	// The original functional revision remains readable after its head changes.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	old, err := credentials.GetProviderRevisionTx(ctx, tx, "output-limit", "primary", first.Revision.Revision)
	if err != nil || old.OutputLimitField != restored.OutputLimitField || old.ContentHash != first.Revision.ContentHash {
		t.Fatalf("old revision changed: %v", err)
	}
}
