package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/labstack/echo/v4"
)

type externalIdentityVerifierFunc func(context.Context, string) (ExternalIdentity, error)

func (f externalIdentityVerifierFunc) Verify(ctx context.Context, token string) (ExternalIdentity, error) {
	return f(ctx, token)
}

type externalIdentityBinderFunc func(context.Context, string, string, string, string, string) (*users.User, error)

func (f externalIdentityBinderFunc) BindExternal(ctx context.Context, issuer, subject, workspace, email, name string) (*users.User, error) {
	return f(ctx, issuer, subject, workspace, email, name)
}

func TestForgeSessionVerifierReadsForgeAccount(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer forge-token" {
			t.Fatalf("authorization = %q", got)
		}
		if r.URL.Path == "/api/v1/auth/me/permissions" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authenticated": true, "permissionSets": []string{forgeTeamDeveloperPermissionSet},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user": map[string]string{"id": "forge-user-1", "email": "developer@example.test", "name": "Developer"},
		})
	}))
	defer upstream.Close()

	verifier := NewForgeSessionVerifier(upstream.URL, "workspace-1", upstream.Client())
	identity, err := verifier.Verify(context.Background(), "forge-token")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Issuer != upstream.URL || identity.Subject != "forge-user-1" || identity.Organization != "workspace-1" || identity.Email != "developer@example.test" || identity.AccessRole != "developer" {
		t.Fatalf("unexpected identity: %#v", identity)
	}
}

func TestForgeSessionVerifierMapsForgeAdminAndMemberAccess(t *testing.T) {
	for _, test := range []struct {
		name           string
		permissionSets []string
		wantRole       string
	}{
		{name: "ordinary employee", permissionSets: []string{"member_default"}, wantRole: "member"},
		{name: "Forge platform administrator", permissionSets: []string{"admin_full_access"}, wantRole: "admin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/auth/me/permissions" {
					_ = json.NewEncoder(w).Encode(map[string]any{"authenticated": true, "permissionSets": test.permissionSets})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"user": map[string]string{"id": "forge-user-1"}})
			}))
			defer upstream.Close()

			identity, err := NewForgeSessionVerifier(upstream.URL, "workspace-1", upstream.Client()).Verify(context.Background(), "forge-token")
			if err != nil {
				t.Fatal(err)
			}
			if identity.AccessRole != test.wantRole {
				t.Fatalf("role = %q, want %q", identity.AccessRole, test.wantRole)
			}
		})
	}
}

func TestExternalIdentityExchangeBindsAccountAndIssuesWeaveSession(t *testing.T) {
	e := echo.New()
	s := &Server{
		Echo:   e,
		Config: &config.Config{JWTSecret: "test-secret"},
		ExternalIdentity: externalIdentityVerifierFunc(func(_ context.Context, token string) (ExternalIdentity, error) {
			if token != "forge-token" {
				t.Fatalf("token = %q", token)
			}
			return ExternalIdentity{Issuer: "https://forge.example.test", Subject: "forge-user-1", Email: "developer@example.test", Name: "Developer", Organization: "workspace-1", AccessRole: "developer"}, nil
		}),
		ExternalIdentityBinder: externalIdentityBinderFunc(func(_ context.Context, issuer, subject, workspace, email, name string) (*users.User, error) {
			if issuer != "https://forge.example.test" || subject != "forge-user-1" || workspace != "workspace-1" || email != "developer@example.test" || name != "Developer" {
				t.Fatalf("unexpected binding: %q %q %q %q %q", issuer, subject, workspace, email, name)
			}
			return &users.User{ID: "ext-user-1", TenantID: "workspace-1", DisplayName: "Developer", Role: "developer"}, nil
		}),
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/external/exchange", nil)
	request.Header.Set("Authorization", "Bearer forge-token")
	recorder := httptest.NewRecorder()
	if err := s.handleExternalIdentityExchange(e.NewContext(request, recorder)); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Token       string   `json:"token"`
		ExpiresIn   int      `json:"expiresIn"`
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	claims := &Claims{}
	parsed, err := jwt.ParseWithClaims(response.Token, claims, func(*jwt.Token) (any, error) { return []byte("test-secret"), nil }, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !parsed.Valid {
		t.Fatalf("invalid issued token: %v", err)
	}
	if claims.IdentitySource != "forge" || claims.TenantID != "workspace-1" || claims.UserID != "ext-user-1" || firstClaimRole(claims.Roles) != "developer" {
		t.Fatalf("unexpected claims: %#v", claims)
	}
	if response.ExpiresIn != 28800 {
		t.Fatalf("expiresIn = %d", response.ExpiresIn)
	}
	if len(response.Permissions) != 2 || response.Permissions[1] != "teams:develop" {
		t.Fatalf("permissions = %#v", response.Permissions)
	}
}
