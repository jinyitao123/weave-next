package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/config"
)

func TestServiceCredentialRoutesRemainSeparateFromUserAuthenticationRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	workspaceID := "capability-api-" + uuid.NewString()
	admin, err := users.NewStore(pool).Create(ctx, workspaceID, "admin", "password", "Admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	store := capabilities.New(pool, capabilities.RealClock{})
	app, err := store.CreateApp(ctx, capabilities.CreateAppRequest{
		WorkspaceID: workspaceID, Name: "service-" + uuid.NewString(),
		MaxConcurrentInvocations: 1, CreatedBy: admin.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := store.CreateCredential(ctx, capabilities.CreateCredentialRequest{
		WorkspaceID: workspaceID, AppID: app.ID, Name: "route-test",
		Scopes: []string{"invoke", "read", "cancel"}, CreatedBy: admin.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&config.Config{JWTSecret: "jwt-test"}, teamDispatchPoolStore{pool: pool}, nil)
	server.Capabilities = &capabilities.InvocationService{Store: store}

	requestBody, _ := json.Marshal(map[string]any{
		"request_id": "route-request", "input": map[string]any{"order_id": "42"},
	})
	request := httptest.NewRequest(http.MethodPost,
		"/v1/capabilities/cap-1/versions/1/invocations", bytes.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer "+raw)
	recorder := httptest.NewRecorder()
	server.Echo.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden ||
		!bytes.Contains(recorder.Body.Bytes(), []byte(`"capability_not_available"`)) {
		t.Fatalf("service credential route status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/invocations/inv-unknown", nil)
	request.Header.Set("Authorization", "Bearer "+raw)
	recorder = httptest.NewRecorder()
	server.Echo.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound ||
		!bytes.Contains(recorder.Body.Bytes(), []byte(`"invocation_not_found"`)) {
		t.Fatalf("service read route status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/invocations/inv-unknown", nil)
	request.Header.Set("Authorization", "Bearer invalid-user-token")
	recorder = httptest.NewRecorder()
	server.Echo.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("user token reached service route: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
