package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/labstack/echo/v4"
)

type externalIdentityVerifierFunc func(context.Context, string) (ExternalIdentity, error)

func (f externalIdentityVerifierFunc) Verify(ctx context.Context, token string) (ExternalIdentity, error) {
	return f(ctx, token)
}

func TestForgeUserInfoVerifierMapsStableSubjectAndConfiguredRole(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer forge-token" {
			t.Fatalf("authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub": "forge-user-1", "email": "developer@example.test", "name": "Developer",
		})
	}))
	defer upstream.Close()

	verifier := NewForgeUserInfoVerifier(upstream.URL, "workspace-1", []string{"forge-user-1"}, upstream.Client())
	identity, err := verifier.Verify(context.Background(), "forge-token")
	if err != nil {
		t.Fatal(err)
	}
	if identity.UserID == "" || identity.Subject != "forge-user-1" || identity.Organization != "workspace-1" || identity.Role != "developer" {
		t.Fatalf("unexpected identity: %#v", identity)
	}
}

func TestExternalIdentityExchangeIssuesShortLivedWeaveSession(t *testing.T) {
	e := echo.New()
	s := &Server{
		Echo:   e,
		Config: &config.Config{JWTSecret: "test-secret"},
		ExternalIdentity: externalIdentityVerifierFunc(func(_ context.Context, token string) (ExternalIdentity, error) {
			if token != "forge-token" {
				t.Fatalf("token = %q", token)
			}
			return ExternalIdentity{Subject: "forge-user-1", UserID: "ext-user-1", Organization: "workspace-1", Role: "developer"}, nil
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
		Token     string `json:"token"`
		ExpiresIn int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	claims := &Claims{}
	parsed, err := jwt.ParseWithClaims(response.Token, claims, func(*jwt.Token) (any, error) { return []byte("test-secret"), nil }, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !parsed.Valid {
		t.Fatalf("invalid issued token: %v", err)
	}
	if claims.IdentitySource != "external" || claims.TenantID != "workspace-1" || claims.UserID != "ext-user-1" || firstClaimRole(claims.Roles) != "developer" {
		t.Fatalf("unexpected claims: %#v", claims)
	}
	if response.ExpiresIn != 600 {
		t.Fatalf("expiresIn = %d", response.ExpiresIn)
	}
}
