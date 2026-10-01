package businessaction

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

func TestTaskCurrentRequiresStrictVersionActiveAndScopeHash(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any)
		valid  bool
	}{
		{"valid", func(map[string]any) {}, true},
		{"24 hour lifetime", func(body map[string]any) { body["expires_at"] = body["issued_at"].(time.Time).Add(24 * time.Hour) }, true},
		{"over 24 hour lifetime", func(body map[string]any) {
			body["expires_at"] = body["issued_at"].(time.Time).Add(24*time.Hour + time.Second)
		}, false},
		{"numeric version", func(body map[string]any) { body["version"] = 1 }, false},
		{"inactive", func(body map[string]any) { body["active"] = false }, false},
		{"missing active", func(body map[string]any) { delete(body, "active") }, false},
		{"missing stable identity source", func(body map[string]any) { delete(body, "identity_issuer") }, false},
		{"network URL as stable identity source", func(body map[string]any) { body["identity_issuer"] = "https://forge.example" }, false},
		{"general employee token", func(body map[string]any) { body["token_type"] = "employee_session" }, false},
		{"changed scope digest", func(body map[string]any) { body["scope_sha256"] = "wrong" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			scope := TaskDelegationScope{InputRevisionID: "input", RegistrationID: "registration", TaskSHA256: "task", WorkflowID: "workflow", WorkflowVersion: 1, AllowedActions: []string{}, Resources: []TaskDelegationResource{}}
			raw, _ := json.Marshal(scope)
			digest, _ := frozen.HashCanonicalJSON(raw)
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != TaskDelegationPath+"/current" {
					t.Error("current used ordinary auth or MCP")
				}
				body := map[string]any{"version": "1", "active": true, "token_type": "forge_task", "issuer": server.URL, "identity_issuer": "forge:workbench-test", "subject": map[string]string{"id": "native-user", "organization_id": "native-org"}, "grant_id": "grant", "generation": 1, "issued_at": time.Now().UTC(), "expires_at": time.Now().UTC().Add(20 * time.Minute), "scope_sha256": digest, "scope": scope}
				test.change(body)
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			_, err := ReadTaskDelegationGrant(context.Background(), server.URL, []byte("task-token"))
			if (err == nil) != test.valid {
				t.Fatalf("current validation valid=%v err=%v", test.valid, err)
			}
		})
	}
}
func TestOnlyTrustedCurrentAuthorizationCodesCarryNoEffectRefusal(t *testing.T) {
	for _, code := range []string{"FORGE_TASK_DELEGATION_EXPIRED", "FORGE_TASK_PARENT_REVOKED", "FORGE_TASK_DELEGATION_REPLACED", "FORGE_TASK_DELEGATION_REVOKED"} {
		body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "no_effect": true, "phase": "authorization"}})
		if err := taskAuthorizationRefusal(401, body); !errors.Is(err, ErrDelegationExpired) {
			t.Fatalf("trusted authorization refusal lost code %s", code)
		}
	}
	for _, test := range []struct {
		status int
		code   string
	}{
		{401, "FORGE_TASK_SUBJECT_INACTIVE"},
		{403, "FORGE_TASK_ORGANIZATION_FORBIDDEN"},
		{403, "FORGE_TASK_CANCELLED"},
	} {
		body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": test.code, "no_effect": true, "phase": "authorization"}})
		err := taskAuthorizationRefusal(test.status, body)
		var refusal *taskAuthorizationRefusalError
		if !errors.As(err, &refusal) || refusal.nonRenewableReason() != test.code || errors.Is(err, ErrDelegationExpired) {
			t.Fatalf("trusted denial must remain explicit and non-renewable: status=%d code=%s err=%v", test.status, test.code, err)
		}
	}
	for _, test := range []struct {
		status int
		body   string
	}{
		{401, `{"error":{"code":"FORGE_TASK_DELEGATION_INVALID","no_effect":true,"phase":"authorization"}}`},
		{401, `{"error":{"code":"FORGE_TASK_DELEGATION_EXPIRED","phase":"authorization"}}`},
		{401, `{"error":{"code":"FORGE_TASK_DELEGATION_EXPIRED","no_effect":true,"phase":"execution"}}`},
		{503, `{"error":{"code":"FORGE_TASK_DELEGATION_EXPIRED","no_effect":true,"phase":"authorization"}}`},
		{401, `{"message":"authorization expired, no effects"}`},
		{401, `{"error":{"code":"FORGE_TASK_SUBJECT_INACTIVE","phase":"authorization","no_effect":false}}`},
		{401, `{"error":{"code":"FORGE_TASK_SUBJECT_INACTIVE","no_effect":true,"phase":"execution"}}`},
		{403, `{"error":{"code":"FORGE_TASK_ORGANIZATION_FORBIDDEN","no_effect":false,"phase":"authorization"}}`},
		{403, `{"error":{"code":"FORGE_TASK_ORGANIZATION_FORBIDDEN","no_effect":true,"phase":"execution"}}`},
		{403, `{"error":{"code":"FORGE_TASK_DELEGATION_EXPIRED","no_effect":true,"phase":"authorization"}}`},
		{401, `{"error":{"code":"FORGE_TASK_ORGANIZATION_FORBIDDEN","no_effect":true,"phase":"authorization"}}`},
		{403, `{"error":{"code":"FORGE_TASK_SUBJECT_INACTIVE","no_effect":true,"phase":"authorization"}}`},
		{401, `{"error":{"code":"FORGE_TASK_CANCELLED","no_effect":true,"phase":"authorization"}}`},
	} {
		if err := taskAuthorizationRefusal(test.status, []byte(test.body)); err != nil {
			t.Fatalf("unproved refusal became safe replay: %s", test.body)
		}
	}
}
