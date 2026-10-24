package credentials

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
)

// Declarations are part of a provider revision's identity once made, and
// leave revisions without them on their existing hash.
func TestProviderRevisionHashCoversModelParametersOnlyWhenDeclared(t *testing.T) {
	cfg := llmrouter.ProviderConfig{ID: "p", Name: "n", BaseURL: "https://models.invalid", Models: []string{"a", "b"}}
	plain, err := hashProviderRevision(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ModelParameters = map[string]frozen.ModelParameters{}
	if empty, _ := hashProviderRevision(cfg); empty != plain {
		t.Fatal("an empty declaration moved the revision hash")
	}
	cfg.ModelParameters = map[string]frozen.ModelParameters{"a": {ReasoningEfforts: []string{"low"}}}
	if declared, _ := hashProviderRevision(cfg); declared == plain {
		t.Fatal("a declaration is not part of the revision hash")
	}
}

func TestProviderModelParametersNormalize(t *testing.T) {
	normalized, err := normalizeProviderConfig("ws", llmrouter.ProviderConfig{ID: "p", Name: "n", BaseURL: "https://models.invalid",
		Models:          []string{"b", "a"},
		ModelParameters: map[string]frozen.ModelParameters{"a": {Thinking: []string{"disabled", "enabled"}}, "b": {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized.ModelParameters["a"].Thinking, []string{"enabled", "disabled"}) {
		t.Fatalf("declaration not normalized: %+v", normalized.ModelParameters)
	}
	for name, cfg := range map[string]llmrouter.ProviderConfig{
		"a model not served": {ID: "p", Name: "n", BaseURL: "https://models.invalid", Models: []string{"a"},
			ModelParameters: map[string]frozen.ModelParameters{"z": {}}},
		"a systemone provider": {ID: "p", Name: "n", BaseURL: "https://models.invalid", Models: []string{"a"}, Protocol: llmrouter.ProtocolSystemOne,
			ModelParameters: map[string]frozen.ModelParameters{"a": {}}},
		"an invalid value": {ID: "p", Name: "n", BaseURL: "https://models.invalid", Models: []string{"a"},
			ModelParameters: map[string]frozen.ModelParameters{"a": {ReasoningEfforts: []string{"extreme"}}}},
	} {
		if _, err := normalizeProviderConfig("ws", cfg); !errors.Is(err, ErrProviderRevisionRequired) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A declaration made on a provider is kept by its revision and frozen into the
// binding of exactly the model it names; changing it advances the revision.
func TestProviderModelParametersReachTheFrozenBindingRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('workspace','workspace','workspace')`); err != nil {
		t.Fatal(err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	alice := execution.WithSubject(ctx, execution.Subject{WorkspaceID: "workspace", UserID: "alice"})
	declared := frozen.ModelParameters{ReasoningEfforts: []string{"low", "high"}, Temperature: &frozen.ParameterRange{Min: 0, Max: 1}}
	config := func(parameters map[string]frozen.ModelParameters) llmrouter.ProviderConfig {
		return llmrouter.ProviderConfig{ID: "nimble", Name: "Nimble", BaseURL: "http://models.invalid:11434",
			Models: []string{"plain", "tuned"}, ModelParameters: parameters,
			CredentialScope: frozen.CredentialScopeUser, CredentialUserID: "alice"}
	}
	binding := func(model string) frozen.FrozenModelBinding {
		t.Helper()
		tx, err := pool.Begin(alice)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(alice)
		value, err := ResolveModelRevisionTx(alice, tx, "workspace", model)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}

	if _, err := store.UpsertRevision(alice, "workspace", config(nil)); err != nil {
		t.Fatal(err)
	}
	if got := binding("tuned"); got.Parameters != nil {
		t.Fatalf("an undeclared model froze a declaration: %+v", got.Parameters)
	}
	result, err := store.UpsertRevision(alice, "workspace", config(map[string]frozen.ModelParameters{"tuned": declared}))
	if err != nil || !result.Advanced || result.Revision.Revision != 2 {
		t.Fatalf("declaring advanced = %+v %v", result, err)
	}
	if got := binding("tuned"); got.Parameters == nil || !reflect.DeepEqual(*got.Parameters, declared) || got.ProviderRevision != 2 {
		t.Fatalf("tuned binding = %+v", got)
	}
	if got := binding("plain"); got.Parameters != nil {
		t.Fatalf("the declaration leaked to another model: %+v", got.Parameters)
	}
	heads, err := store.ListMetadata(alice, "workspace")
	if err != nil || len(heads) != 1 || !reflect.DeepEqual(heads[0].ModelParameters["tuned"], declared) {
		t.Fatalf("head = %+v %v", heads, err)
	}
	revision, err := store.GetRevision(alice, "workspace", "nimble", 2)
	if err != nil || !reflect.DeepEqual(revision.ModelParameters["tuned"], declared) {
		t.Fatalf("revision = %+v %v", revision, err)
	}
	if again, err := store.UpsertRevision(alice, "workspace", config(map[string]frozen.ModelParameters{"tuned": declared})); err != nil || again.Advanced {
		t.Fatalf("the same declaration advanced: %+v %v", again, err)
	}
}
