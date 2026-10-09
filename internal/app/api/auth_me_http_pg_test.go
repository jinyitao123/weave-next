package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/config"
)

func TestCurrentIdentitySelfReadHTTPRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	server := routeHTTPServer(&config.Config{JWTSecret: "identity-http-fixture"})
	server.UserStore, server.KeyStore = users.NewStore(pool), apikeys.NewStore(pool)
	host := httptest.NewServer(server.Echo)
	t.Cleanup(host.Close)
	request := func(t *testing.T, method, path, token string, status int) map[string]string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, host.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := host.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != status {
			t.Fatalf("%s %s returned %d, want %d", method, path, response.StatusCode, status)
		}
		var result map[string]string
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, token := range []string{"", "invalid", "wv_sk_missing"} {
		request(t, http.MethodGet, "/v1/auth/me", token, http.StatusUnauthorized)
	}
	for _, role := range []string{"admin", "member"} {
		t.Run(role, func(t *testing.T) {
			workspace := "identity-" + role
			user, err := server.UserStore.BindExternalInOrganizationFromOrigin(t.Context(), "urn:test:identity", "http://forge.example.test", role, workspace, role+"@example.test", role, workspace)
			if err != nil {
				t.Fatal(err)
			}
			key, rawKey, err := server.KeyStore.Create(t.Context(), workspace, "operator", role, user.ID, []string{"admin"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			// The key fixes both the effective role and workspace; query fields
			// cannot select another identity. An exact DTO also excludes secrets.
			actual := request(t, http.MethodGet, "/v1/auth/me?tenant_id=other&role=admin&user_id=other", rawKey, http.StatusOK)
			want := map[string]string{"id": user.ID, "tenant_id": workspace, "role": role, "source": "apikey", "api_key_id": key.ID}
			if !reflect.DeepEqual(actual, want) {
				t.Fatal("API key identity differs from its authenticated principal")
			}
			if role == "member" {
				request(t, http.MethodGet, "/v1/auth/api-keys", rawKey, http.StatusForbidden)
			}
			// Forge roles come from the verified session, not the account's
			// initial stored role. Self-read cannot mint or refresh a session.
			token, err := server.signJWTFor(workspace, user.ID, []string{role}, "forge", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			actual = request(t, http.MethodGet, "/v1/auth/me", token, http.StatusOK)
			want = map[string]string{"id": user.ID, "tenant_id": workspace, "role": role, "source": "jwt"}
			if !reflect.DeepEqual(actual, want) {
				t.Fatal("Forge session identity differs from its authenticated principal")
			}
			if role == "member" {
				request(t, http.MethodGet, "/v1/auth/api-keys", token, http.StatusForbidden)
			}
			for _, route := range []string{"/v1/auth/login", "/v1/auth/register", "/v1/auth/refresh"} {
				request(t, http.MethodPost, route, rawKey, http.StatusNotFound)
			}
			request(t, http.MethodPut, "/v1/auth/me", token, http.StatusNotFound)
			if err := server.KeyStore.Delete(t.Context(), workspace, key.ID); err != nil {
				t.Fatal(err)
			}
			request(t, http.MethodGet, "/v1/auth/me", rawKey, http.StatusUnauthorized)
		})
	}
}
