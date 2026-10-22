package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

// The default protocol must not move revision hashes written before
// protocols existed; any other protocol is part of the functional identity.
func TestProviderRevisionHashKeepsLegacyOpenAIHash(t *testing.T) {
	cfg := llmrouter.ProviderConfig{Name: "n", BaseURL: "https://models.invalid", Models: []string{"a", "b"}}
	legacy, err := json.Marshal(struct {
		Name           string   `json:"name"`
		BaseURL        string   `json:"base_url"`
		Models         []string `json:"models"`
		JSONObjectMode bool     `json:"json_object_mode"`
	}{cfg.Name, cfg.BaseURL, cfg.Models, cfg.JSONObjectMode})
	if err != nil {
		t.Fatal(err)
	}
	want, err := frozen.HashCanonicalJSON(legacy)
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{"", llmrouter.ProtocolOpenAI} {
		cfg.Protocol = protocol
		if got, err := hashProviderRevision(cfg); err != nil || got != want {
			t.Fatalf("protocol %q moved the legacy hash", protocol)
		}
	}
	cfg.Protocol = llmrouter.ProtocolSystemOne
	if got, _ := hashProviderRevision(cfg); got == want {
		t.Fatal("systemone protocol is not part of the revision hash")
	}
}

// Migration 0196 classifies existing rows by ciphertext shape only:
// NOT (length = 40 AND ends with "==") means a key is stored.
func TestEmptyKeyCiphertextShapeMatchesMigrationBackfill(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	stored := func(cipher string) bool { return !(len(cipher) == 40 && strings.HasSuffix(cipher, "==")) }
	for size := 0; size < 64; size++ {
		cipher, err := secret.Seal(key, []byte(strings.Repeat("k", size)))
		if err != nil {
			t.Fatal(err)
		}
		if stored(cipher) != (size > 0) {
			t.Fatalf("a %d-byte key is misclassified by the backfill (%d characters)", size, len(cipher))
		}
	}
}

func TestProviderProtocolAndOptionalKeyRoundTripRealPG(t *testing.T) {
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
	config := func(protocol, key string) llmrouter.ProviderConfig {
		return llmrouter.ProviderConfig{ID: "nimble", Name: "Nimble", BaseURL: "http://models.invalid:11434", Protocol: protocol, APIKey: key,
			Models: []string{"nimble:latest"}, CredentialScope: frozen.CredentialScopeUser, CredentialUserID: "alice"}
	}
	resolveKey := func() string {
		t.Helper()
		tx, err := pool.Begin(alice)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(alice)
		key, err := store.ResolveProviderAPIKeyTx(alice, tx, providerConfigReference("workspace", config("", "")))
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	binding := func() frozen.FrozenModelBinding {
		t.Helper()
		tx, err := pool.Begin(alice)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(alice)
		value, err := ResolveModelRevisionTx(alice, tx, "workspace", "nimble:latest")
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	metadata := func() ProviderHead {
		t.Helper()
		heads, err := store.ListMetadata(alice, "workspace")
		if err != nil || len(heads) != 1 {
			t.Fatalf("metadata: %+v %v", heads, err)
		}
		return heads[0]
	}

	// A keyless systemone provider is created, listed as "no key" and frozen with its protocol.
	created, err := store.UpsertRevision(alice, "workspace", config(llmrouter.ProtocolSystemOne, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !created.Advanced || created.Revision.Revision != 1 || created.Revision.Protocol != llmrouter.ProtocolSystemOne ||
		created.Head.Protocol != llmrouter.ProtocolSystemOne || created.Head.APIKeySet || created.Head.APIKey != "" {
		t.Fatalf("created = %+v", created)
	}
	head := metadata()
	encoded, _ := json.Marshal(head)
	if head.Protocol != llmrouter.ProtocolSystemOne || head.APIKeySet || strings.Contains(string(encoded), `"api_key"`) ||
		!strings.Contains(string(encoded), `"has_api_key":false`) || !strings.Contains(string(encoded), `"protocol":"systemone"`) {
		t.Fatalf("keyless provider listed as %s", encoded)
	}
	configs, err := store.List(alice, "workspace")
	if err != nil || len(configs) != 1 || configs[0].Protocol != llmrouter.ProtocolSystemOne || configs[0].APIKey != "" {
		t.Fatalf("decrypted list = %+v %v", configs, err)
	}
	revision, err := store.GetRevision(alice, "workspace", "nimble", 1)
	if err != nil || revision.Protocol != llmrouter.ProtocolSystemOne {
		t.Fatalf("revision readback = %+v %v", revision, err)
	}
	if got := binding(); got.Protocol != frozen.ModelProtocolSystemOne || got.ProviderRevision != 1 {
		t.Fatalf("frozen binding = %+v", got)
	}
	if key := resolveKey(); key != "" {
		t.Fatal("keyless provider resolved a key")
	}

	// Setting, keeping and clearing a key never creates a functional revision.
	result, err := store.UpsertRevision(alice, "workspace", config(llmrouter.ProtocolSystemOne, "nimble-test-key"))
	if err != nil || result.Advanced || !result.Head.APIKeySet || result.Head.APIKey != MaskedKey {
		t.Fatalf("set key = %+v %v", result, err)
	}
	if head := metadata(); !head.APIKeySet || head.APIKey != MaskedKey {
		t.Fatalf("keyed provider listed as %+v", head)
	}
	for _, kept := range []string{"", MaskedKey} {
		if result, err := store.UpsertRevision(alice, "workspace", config(llmrouter.ProtocolSystemOne, kept)); err != nil || !result.Head.APIKeySet {
			t.Fatalf("key %q did not keep the stored key: %+v %v", kept, result, err)
		}
		if key := resolveKey(); key != "nimble-test-key" {
			t.Fatal("stored key changed without a replacement")
		}
	}
	if _, err := store.UpsertRevision(alice, "workspace", config(llmrouter.ProtocolSystemOne, "other-key"), ClearAPIKey()); !errors.Is(err, ErrProviderRevisionRequired) {
		t.Fatalf("replace and clear together accepted: %v", err)
	}
	result, err = store.UpsertRevision(alice, "workspace", config(llmrouter.ProtocolSystemOne, ""), ClearAPIKey())
	if err != nil || result.Advanced || result.Head.APIKeySet || result.Head.APIKey != "" {
		t.Fatalf("clear key = %+v %v", result, err)
	}
	if key := resolveKey(); key != "" || metadata().APIKeySet {
		t.Fatal("cleared key is still stored")
	}

	// Changing the protocol is a new revision; openai freezes as the default.
	result, err = store.UpsertRevision(alice, "workspace", config("", ""))
	if err != nil || !result.Advanced || result.Revision.Revision != 2 || result.Revision.Protocol != llmrouter.ProtocolOpenAI {
		t.Fatalf("protocol change = %+v %v", result, err)
	}
	if got := binding(); got.Protocol != "" || got.ProviderRevision != 2 {
		t.Fatalf("openai binding = %+v", got)
	}
	first, err := store.GetRevision(alice, "workspace", "nimble", 1)
	if err != nil || first.Protocol != llmrouter.ProtocolSystemOne || first.ContentHash == result.Revision.ContentHash {
		t.Fatalf("immutable first revision changed: %+v %v", first, err)
	}

	// Unknown protocols are rejected by the store and by the schema.
	if _, err := store.UpsertRevision(alice, "workspace", config("grpc", "")); !errors.Is(err, ErrProviderRevisionRequired) {
		t.Fatalf("unknown protocol accepted: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_provider_credentials SET protocol='grpc' WHERE workspace_id='workspace' AND id='nimble'`); err == nil {
		t.Fatal("database accepted an unknown protocol")
	}
}
