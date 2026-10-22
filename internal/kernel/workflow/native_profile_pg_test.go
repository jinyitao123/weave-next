package workflow_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func TestNativeProfilePersistsFreezesAndRoutesRealPG(t *testing.T) {
	for _, protocol := range []string{frozen.ProtocolAnthropicMessages, frozen.ProtocolOpenAIResponses} {
		t.Run(protocol, func(t *testing.T) {
			pool := testutil.PostgresPool(t)
			ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "native", UserID: "alice"})
			if err := db.Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('native','native','Native')`); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if protocol == frozen.ProtocolAnthropicMessages {
					if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "synthetic-key" {
						t.Error("frozen native protocol or authorized key lost")
					}
					w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2,\"cache_read_input_tokens\":0}}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"done\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n\n"))
				} else {
					if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer synthetic-key" {
						t.Error("frozen native protocol or authorized key lost")
					}
					w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"input_tokens_details\":{\"cached_tokens\":0}}}}\n\n"))
				}
			}))
			defer server.Close()
			store := credentials.New(pool, []byte("0123456789abcdef0123456789abcdef"))
			path := "/v1/responses"
			if protocol == frozen.ProtocolAnthropicMessages {
				path = "/v1/messages"
			}
			cfg := llmrouter.ProviderConfig{Protocol: protocol, ID: "native", Name: "Native", BaseURL: server.URL, ChatPath: path, Models: []string{"native-model"}, APIKey: "synthetic-key", AttemptTimeoutSeconds: 7, CredentialScope: frozen.CredentialScopeUser, CredentialUserID: "alice"}
			first, err := store.UpsertRevision(ctx, "native", cfg)
			if err != nil {
				t.Fatal(err)
			}
			same, err := store.UpsertRevision(ctx, "native", cfg)
			if err != nil || same.Advanced || same.Revision.Revision != first.Revision.Revision {
				t.Fatalf("idempotent revision changed: %v", err)
			}
			listed, err := credentials.New(pool, []byte("0123456789abcdef0123456789abcdef")).List(ctx, "native")
			if err != nil || len(listed) != 1 || listed[0].Protocol != protocol || listed[0].AttemptTimeoutSeconds != 7 {
				t.Fatalf("profile not persisted: %v", err)
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := credentials.ResolveModelRevisionTx(ctx, tx, "native", "native-model")
			tx.Rollback(ctx)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(binding)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), cfg.APIKey) {
				t.Fatal("secret leaked to frozen publication")
			}
			var restored frozen.FrozenModelBinding
			if err = json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			cfg.Protocol = frozen.ProtocolOpenAICompletions
			cfg.ChatPath = "/v1/chat/completions"
			cfg.BaseURL = server.URL + "/changed"
			cfg.APIKey = ""
			cfg.AttemptTimeoutSeconds = 8
			changed, err := store.UpsertRevision(ctx, "native", cfg)
			if err != nil || !changed.Advanced || changed.Revision.ContentHash == first.Revision.ContentHash {
				t.Fatalf("functional revision did not advance: %v", err)
			}
			resolver := nativeProfileResolver{pool: pool, store: store}
			opts, closer, err := workflow.NewRuntimeHostFactory().Build(ctx, frozen.FrozenExecutionBundle{Agent: frozen.FrozenAgentRecord{Model: "native-model"}, PrimaryModel: restored}, resolver)
			if err != nil {
				t.Fatal(err)
			}
			defer closer.Close()
			resp, err := opts.LLM.Chat(ctx, contract.ChatRequest{Model: "native-model", Messages: []contract.Message{{Role: "user", Content: "check frozen route"}}})
			if err != nil || resp.Content != "done" || !resp.Usage.CacheReadReported || calls.Load() != 1 {
				t.Fatalf("frozen route failed: %+v, %v", resp, err)
			}
			mutated := restored
			mutated.AttemptTimeoutSeconds++
			mutated.ContentHash = ""
			changedHash, hashErr := frozen.HashDTO(mutated, frozen.PreorderFrozenModelBinding)
			if hashErr != nil || changedHash == restored.ContentHash {
				t.Fatal("request timeout not covered by frozen digest")
			}
			bad := restored
			bad.Protocol = "unknown"
			if _, err := frozen.HashDTO(bad, frozen.PreorderFrozenModelBinding); err == nil {
				t.Fatal("unknown protocol accepted in frozen hash")
			}
			bob := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "native", UserID: "bob"})
			if _, err := resolver.Resolve(bob, credentials.ResolveRequest{Reference: restored.CredentialRef}); err == nil {
				t.Fatal("another actor resolved the provider credential")
			}
		})
	}
}

type nativeProfileResolver struct {
	pool  *pgxpool.Pool
	store *credentials.Store
}

func (r nativeProfileResolver) Validate(ctx context.Context, ref frozen.CredentialReference) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	return r.store.ValidateReferenceTx(ctx, tx, ref)
}
func (r nativeProfileResolver) Resolve(ctx context.Context, req credentials.ResolveRequest) (credentials.SecretMaterial, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return credentials.SecretMaterial{}, err
	}
	defer tx.Rollback(ctx)
	key, err := r.store.ResolveProviderAPIKeyTx(ctx, tx, req.Reference)
	if err != nil {
		return credentials.SecretMaterial{}, err
	}
	return credentials.NewSecretMaterial([]byte(key), nil, nil), nil
}
