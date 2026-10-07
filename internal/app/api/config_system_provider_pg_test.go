package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/labstack/echo/v4"
)

type systemCatalogFixture struct {
	server *Server
	pool   *pgxpool.Pool
	router *llmrouter.Router
	http   *httptest.Server
}

func newSystemCatalogFixture(t *testing.T) *systemCatalogFixture {
	t.Helper()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	router := llmrouter.New("fixture-model")
	for _, id := range []string{"fixture", "unmirrored"} {
		router.RegisterProvider(llmrouter.ProviderConfig{
			ID: id, Name: "Server " + id, BaseURL: "https://models.example.test",
			APIKey: "catalog-test-secret-" + id, Models: []string{"fixture-model"},
		})
	}
	server := &Server{
		Echo: echo.New(), Config: &config.Config{JWTSecret: "catalog-test-jwt", DisableLocalLogin: true},
		Pool: pool, UserStore: users.NewStore(pool), KeyStore: apikeys.NewStore(pool),
		Credentials: credentials.New(pool, bytes.Repeat([]byte{'k'}, 32)), SystemProviders: router,
	}
	server.registerRoutes()
	httpServer := httptest.NewServer(server.Echo)
	t.Cleanup(httpServer.Close)
	return &systemCatalogFixture{server: server, pool: pool, router: router, http: httpServer}
}

func (f *systemCatalogFixture) account(t *testing.T, workspace, subject, role string) (*users.User, string) {
	t.Helper()
	user, err := f.server.UserStore.BindExternalInOrganizationFromOrigin(t.Context(), "urn:test:catalog", "http://forge.example.test", subject, workspace, subject+"@example.test", subject, workspace)
	if err != nil {
		t.Fatal(err)
	}
	// The production auth middleware validates this issued Forge-session JWT
	// against the real account store; no authentication middleware is mocked.
	token, err := f.server.signJWTFor(workspace, user.ID, []string{role}, "forge", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return user, token
}

func (f *systemCatalogFixture) request(t *testing.T, method, path, token, body string, expected int) []byte {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, f.http.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := f.http.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != expected {
		t.Fatalf("%s %s returned %d, want %d: %s", method, path, response.StatusCode, expected, data)
	}
	return data
}

func (f *systemCatalogFixture) mirror(t *testing.T, token string) credentials.SystemProviderMirrorResult {
	t.Helper()
	data := f.request(t, http.MethodPost, "/v1/providers/system/fixture/mirror", token, `{"reason":"catalog integration test"}`, http.StatusCreated)
	var result credentials.SystemProviderMirrorResult
	if err := json.Unmarshal(data, &result); err != nil || result.ProviderID != "system/fixture" || result.Revision < 1 {
		t.Fatalf("unrecognized mirror receipt: %+v, %v", result, err)
	}
	return result
}

func (f *systemCatalogFixture) catalog(t *testing.T, token, path string) map[string]systemProviderSummaryJSON {
	t.Helper()
	data := f.request(t, http.MethodGet, path, token, "", http.StatusOK)
	for _, forbidden := range []string{"api_key", "cipher", "catalog-test-secret", credentials.MaskedKey} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatal("system catalog exposed credential material or a credential field")
		}
	}
	var rows []systemProviderSummaryJSON
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	result := make(map[string]systemProviderSummaryJSON, len(rows))
	for _, row := range rows {
		result[row.ID] = row
	}
	return result
}

func TestSystemProviderCatalogMirrorReadbackRealPG(t *testing.T) {
	f := newSystemCatalogFixture(t)
	_, admin := f.account(t, "workspace-a", "admin-a", "admin")
	before := f.catalog(t, admin, "/v1/providers/system")
	if before["fixture"].Mirrored {
		t.Fatal("a process provider was reported as already mirrored")
	}
	receipt := f.mirror(t, admin)
	after := f.catalog(t, admin, "/v1/providers/system")
	if actual := after["fixture"]; !actual.Mirrored || actual.MirroredAs != receipt.ProviderID || actual.MirrorRevision != receipt.Revision {
		t.Fatalf("committed system mirror is absent from the current workspace catalog: %+v", actual)
	}
	if after["unmirrored"].Mirrored || len(after) != 2 {
		t.Fatal("the catalog added a mirror without a corresponding stored revision")
	}
	// Catalog metadata must not open the secret slot even when its ciphertext
	// cannot be decrypted. This change does not repair or read that secret.
	if _, err := f.pool.Exec(t.Context(), `UPDATE weave_provider_credentials SET api_key_cipher='not-a-valid-ciphertext' WHERE workspace_id='workspace-a' AND id='system/fixture'`); err != nil {
		t.Fatal(err)
	}
	if actual := f.catalog(t, admin, "/v1/providers/system")["fixture"]; !actual.Mirrored || actual.MirrorRevision != receipt.Revision {
		t.Fatal("metadata reading unexpectedly required credential decryption")
	}
}

func TestSystemProviderCatalogKeepsRoleScopeAndWorkspaceBoundariesRealPG(t *testing.T) {
	f := newSystemCatalogFixture(t)
	adminUser, admin := f.account(t, "workspace-a", "admin-a", "admin")
	_, otherAdmin := f.account(t, "workspace-b", "admin-b", "admin")
	f.mirror(t, admin)
	if other := f.catalog(t, otherAdmin, "/v1/providers/system?workspace_id=workspace-a")["fixture"]; other.Mirrored || other.MirroredAs != "" || other.MirrorRevision != 0 {
		t.Fatal("another workspace's mirror was revealed through a caller-supplied workspace")
	}
	for _, role := range []string{"member", "developer"} {
		_, token := f.account(t, "workspace-a", role, role)
		f.request(t, http.MethodGet, "/v1/providers/system", token, "", http.StatusForbidden)
		f.request(t, http.MethodPost, "/v1/providers/system/fixture/mirror", token, `{}`, http.StatusForbidden)
	}
	for _, token := range []string{"", "invalid"} {
		f.request(t, http.MethodGet, "/v1/providers/system", token, "", http.StatusUnauthorized)
	}
	_, limitedKey, err := f.server.KeyStore.Create(t.Context(), "workspace-a", "catalog-no-admin-scope", "admin", adminUser.ID, []string{"runs"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.request(t, http.MethodGet, "/v1/providers/system", limitedKey, "", http.StatusForbidden)
	f.request(t, http.MethodPost, "/v1/providers/system/fixture/mirror", limitedKey, `{}`, http.StatusForbidden)
	// A valid account from A with a signed-but-mismatched B workspace must
	// still fail at the real account lookup, before catalog authorization.
	wrongWorkspace, err := f.server.signJWTFor("workspace-b", adminUser.ID, []string{"admin"}, "forge", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.request(t, http.MethodGet, "/v1/providers/system", wrongWorkspace, "", http.StatusUnauthorized)
}

func TestSystemProviderCatalogDoesNotGrantGeneralCredentialReadsRealPG(t *testing.T) {
	f := newSystemCatalogFixture(t)
	adminUser, admin := f.account(t, "workspace-a", "admin-a", "admin")
	f.mirror(t, admin)
	f.request(t, http.MethodPost, "/v1/providers", admin,
		`{"id":"personal","name":"Personal fixture","base_url":"https://models.example.test","api_key":"personal-test-only","models":["personal-model"]}`, http.StatusCreated)
	service := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "workspace-a", ServiceID: "other-service"})
	if err := f.server.Credentials.Upsert(service, "workspace-a", llmrouter.ProviderConfig{
		ID: "other-service-provider", Name: "Other service", BaseURL: "https://models.example.test", APIKey: "other-test-only", Models: []string{"other-model"},
		CredentialScope: frozen.CredentialScopeWorkspaceService, CredentialServiceID: "other-service",
	}); err != nil {
		t.Fatal(err)
	}
	if rows := f.catalog(t, admin, "/v1/providers/system"); len(rows) != 2 || !rows["fixture"].Mirrored {
		t.Fatal("catalog either hid its committed mirror or included non-system providers")
	}
	var personal []credentials.ProviderHead
	if err := json.Unmarshal(f.request(t, http.MethodGet, "/v1/providers", admin, "", http.StatusOK), &personal); err != nil || len(personal) != 1 || personal[0].ID != "personal" {
		t.Fatalf("ordinary provider metadata access changed: count=%d err=%v", len(personal), err)
	}
	actor := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace-a", UserID: adminUser.ID})
	if _, err := f.server.Credentials.GetHead(actor, "workspace-a", "system/fixture"); err == nil {
		t.Fatal("the catalog permit escaped into an ordinary credential read")
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	ref := frozen.CredentialReference{SchemaVersion: 1, WorkspaceID: "workspace-a", Scope: frozen.CredentialScopeWorkspaceService,
		ServiceID: "system-provider:fixture", Kind: frozen.CredentialProviderAPIKey, ResourceID: "system/fixture", Slot: "api_key"}
	if _, err := f.server.Credentials.ResolveProviderAPIKeyTx(actor, tx, ref); err == nil {
		t.Fatal("the system catalog granted a service secret to the user")
	}
}

func TestSystemProviderCatalogRejectsClosedAndMismatchedMirrorMetadataRealPG(t *testing.T) {
	f := newSystemCatalogFixture(t)
	_, admin := f.account(t, "workspace-a", "admin-a", "admin")
	f.mirror(t, admin)
	for _, update := range []string{
		`enabled=false`,
		`enabled=true, revoked_at=now()`,
		`revoked_at=NULL, deleted_at=now()`,
		`deleted_at=NULL, source_provider_id='unmirrored'`,
	} {
		if _, err := f.pool.Exec(t.Context(), `UPDATE weave_provider_credentials SET `+update+` WHERE workspace_id='workspace-a' AND id='system/fixture'`); err != nil {
			t.Fatal(err)
		}
		for _, row := range f.catalog(t, admin, "/v1/providers/system") {
			if row.Mirrored || row.MirroredAs != "" || row.MirrorRevision != 0 {
				t.Fatalf("closed or mismatched mirror was reported: %s", update)
			}
		}
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE weave_provider_credentials SET source_provider_id='fixture' WHERE workspace_id='workspace-a' AND id='system/fixture'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE weave_provider_credentials SET credential_service_id='system-provider:unmirrored' WHERE workspace_id='workspace-a' AND id='system/fixture'`); err == nil {
		t.Fatal("the existing immutable credential subject guard was lost")
	}
	if !f.catalog(t, admin, "/v1/providers/system")["fixture"].Mirrored {
		t.Fatal("a refused subject rewrite changed the original mirror")
	}
}
