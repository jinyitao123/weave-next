package api

import (
	"github.com/golang-jwt/jwt/v5"
	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/labstack/echo/v4"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWorkbenchHostRequiresVerifiedUserIdentity(t *testing.T) {
	e := echo.New()
	host := &apikeys.APIKey{ID: "host", TenantID: "ws", Role: "admin", OwnerUserID: "owner"}
	makeContext := func(user, secret, workspace string) echo.Context {
		c := e.NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
		claims := Claims{TenantID: workspace, UserID: user, Roles: []string{"user"}, RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
		signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
		if err != nil {
			t.Fatal(err)
		}
		c.Request().Header.Set("X-Weave-User-Authorization", "Bearer "+signed)
		c.Request().Header.Set("X-Weave-Actor-ID", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		setAPIKeyContext(c, host)
		return c
	}
	first, second := makeContext("alice", "secret", "ws"), makeContext("bob", "secret", "ws")
	for _, c := range []echo.Context{first, second} {
		if err := bindDelegatedUser(c, "secret", nil, host); err != nil {
			t.Fatal(err)
		}
	}
	if getUserID(first) == getUserID(second) || capabilityApplicationID(first) == capabilityApplicationID(second) {
		t.Fatal("verified users share application identity")
	}
	subject, err := execution.RequireSubject(first.Request().Context(), "ws")
	if err != nil || subject.UserID != "alice" {
		t.Fatalf("subject=%+v err=%v", subject, err)
	}
	for _, c := range []echo.Context{makeContext("alice", "forged", "ws"), makeContext("alice", "secret", "other")} {
		if err := bindDelegatedUser(c, "secret", nil, host); err == nil {
			t.Fatal("invalid delegated proof accepted")
		}
	}
	ordinary := *host
	ordinary.Role = "user"
	if err := bindDelegatedUser(makeContext("alice", "secret", "ws"), "secret", nil, &ordinary); err == nil {
		t.Fatal("non-Host credential delegated identity")
	}
}

func TestUntrustedWorkbenchActorHeaderCannotImpersonateUser(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
	c.Request().Header.Set("X-Weave-Actor-ID", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	setAPIKeyContext(c, &apikeys.APIKey{ID: "key", TenantID: "ws", Role: "admin", OwnerUserID: "owner"})
	if getUserID(c) != "owner" || capabilityApplicationID(c) != "key" {
		t.Fatal("arbitrary header overrode authenticated actor")
	}
}
