package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/labstack/echo/v4"
)

const externalSessionTTL = 8 * time.Hour

const forgeTeamDeveloperPermissionSet = "weave_team_developer"

type ExternalIdentity struct {
	Issuer             string
	Subject            string
	Email              string
	Name               string
	Organization       string
	NativeOrganization string
	AccessRole         string
	PermissionSets     []string
}

type ExternalIdentityVerifier interface {
	Verify(context.Context, string) (ExternalIdentity, error)
}

type ForgeSessionVerifier struct {
	endpoint         *url.URL
	defaultWorkspace string
	client           *http.Client
}

func NewForgeSessionVerifier(rawURL, defaultWorkspace string, client *http.Client) *ForgeSessionVerifier {
	endpoint, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil {
		endpoint = nil
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &ForgeSessionVerifier{endpoint: endpoint, defaultWorkspace: strings.TrimSpace(defaultWorkspace), client: client}
}

func (v *ForgeSessionVerifier) Verify(ctx context.Context, bearer string) (ExternalIdentity, error) {
	if v == nil || v.endpoint == nil || strings.TrimSpace(bearer) == "" {
		return ExternalIdentity{}, errors.New("external identity is not configured")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.endpoint.String(), nil)
	if err != nil {
		return ExternalIdentity{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+bearer)
	response, err := v.client.Do(request)
	if err != nil {
		return ExternalIdentity{}, fmt.Errorf("verify external identity: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return ExternalIdentity{}, fmt.Errorf("verify external identity: status %d", response.StatusCode)
	}
	var body struct {
		Sub          string `json:"sub"`
		ID           string `json:"id"`
		Email        string `json:"email"`
		Name         string `json:"name"`
		Organization struct {
			ID string `json:"id"`
		} `json:"organization"`
		User struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"user"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
		return ExternalIdentity{}, errors.New("invalid external identity response")
	}
	subject := strings.TrimSpace(body.Sub)
	if subject == "" {
		subject = strings.TrimSpace(body.ID)
	}
	if subject == "" {
		subject = strings.TrimSpace(body.User.ID)
	}
	email := strings.TrimSpace(body.Email)
	if email == "" {
		email = strings.TrimSpace(body.User.Email)
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = strings.TrimSpace(body.User.Name)
	}
	workspace := strings.TrimSpace(body.Organization.ID)
	if workspace == "" {
		workspace = v.defaultWorkspace
	}
	if subject == "" || workspace == "" {
		return ExternalIdentity{}, errors.New("external identity is missing subject or workspace")
	}
	sessionEndpoint := *v.endpoint
	sessionEndpoint.Path, sessionEndpoint.RawPath, sessionEndpoint.RawQuery, sessionEndpoint.Fragment = "/api/v1/auth/get-session", "", "", ""
	sessionRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, sessionEndpoint.String(), nil)
	if err != nil {
		return ExternalIdentity{}, err
	}
	sessionRequest.Header.Set("Authorization", "Bearer "+bearer)
	sessionResponse, err := v.client.Do(sessionRequest)
	if err != nil {
		return ExternalIdentity{}, errors.New("native session could not be verified")
	}
	defer sessionResponse.Body.Close()
	if sessionResponse.StatusCode != http.StatusOK {
		return ExternalIdentity{}, errors.New("native session is unavailable")
	}
	var nativeSession struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Session struct {
			UserID               string `json:"userId"`
			ActiveOrganizationID string `json:"activeOrganizationId"`
		} `json:"session"`
	}
	if json.NewDecoder(io.LimitReader(sessionResponse.Body, 1<<20)).Decode(&nativeSession) != nil || nativeSession.User.ID != subject || (nativeSession.Session.UserID != "" && nativeSession.Session.UserID != subject) || nativeSession.Session.ActiveOrganizationID == "" {
		return ExternalIdentity{}, errors.New("native session identity or organization differs")
	}
	permissionEndpoint := *v.endpoint
	permissionEndpoint.Path = "/api/v1/auth/me/permissions"
	permissionEndpoint.RawPath = ""
	permissionEndpoint.RawQuery = ""
	permissionEndpoint.Fragment = ""
	permissionRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, permissionEndpoint.String(), nil)
	if err != nil {
		return ExternalIdentity{}, err
	}
	permissionRequest.Header.Set("Accept", "application/json")
	permissionRequest.Header.Set("Authorization", "Bearer "+bearer)
	permissionResponse, err := v.client.Do(permissionRequest)
	if err != nil {
		return ExternalIdentity{}, fmt.Errorf("verify Forge permissions: %w", err)
	}
	defer permissionResponse.Body.Close()
	if permissionResponse.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(permissionResponse.Body, 64<<10))
		return ExternalIdentity{}, fmt.Errorf("verify Forge permissions: status %d", permissionResponse.StatusCode)
	}
	var permissionBody struct {
		Authenticated  bool     `json:"authenticated"`
		PermissionSets []string `json:"permissionSets"`
	}
	if err := json.NewDecoder(io.LimitReader(permissionResponse.Body, 1<<20)).Decode(&permissionBody); err != nil || !permissionBody.Authenticated {
		return ExternalIdentity{}, errors.New("invalid Forge permission response")
	}
	accessRole := "member"
	for _, permissionSet := range permissionBody.PermissionSets {
		switch strings.TrimSpace(permissionSet) {
		case "admin_full_access":
			accessRole = "admin"
		case forgeTeamDeveloperPermissionSet:
			if accessRole != "admin" {
				accessRole = "developer"
			}
		}
	}
	return ExternalIdentity{
		Issuer: v.endpoint.Scheme + "://" + v.endpoint.Host, Subject: subject,
		Email: email, Name: name,
		Organization: workspace, NativeOrganization: nativeSession.Session.ActiveOrganizationID, AccessRole: accessRole, PermissionSets: permissionBody.PermissionSets,
	}, nil
}

func productPermissions(role string) []string {
	permissions := []string{"teams:use"}
	if role == "developer" || role == "admin" {
		permissions = append(permissions, "teams:develop")
	}
	if role == "admin" {
		permissions = append(permissions, "teams:admin")
	}
	return permissions
}

type externalIdentityBinder interface {
	BindExternal(context.Context, string, string, string, string, string) (*users.User, error)
}

type nativeExternalIdentityBinder interface {
	BindExternalInOrganization(context.Context, string, string, string, string, string, string) (*users.User, error)
}

func (s *Server) handleExternalIdentityExchange(c echo.Context) error {
	authorization := c.Request().Header.Get("Authorization")
	bearer := strings.TrimPrefix(authorization, "Bearer ")
	if bearer == authorization || bearer == "" {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "missing external identity token"})
	}
	if s.ExternalIdentity == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "external identity is not configured"})
	}
	identity, err := s.ExternalIdentity.Verify(c.Request().Context(), bearer)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "external identity verification failed"})
	}
	binder := s.ExternalIdentityBinder
	if binder == nil && s.UserStore != nil {
		binder = s.UserStore
	}
	if binder == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "account binding is not configured"})
	}
	nativeBinder, supported := binder.(nativeExternalIdentityBinder)
	if !supported || identity.NativeOrganization == "" {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "native organization binding is unavailable"})
	}
	user, err := nativeBinder.BindExternalInOrganization(c.Request().Context(), identity.Issuer, identity.Subject, identity.Organization, identity.Email, identity.Name, identity.NativeOrganization)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "account binding failed"})
	}
	accessRole := identity.AccessRole
	if accessRole != "developer" && accessRole != "admin" {
		accessRole = "member"
	}
	token, err := s.signJWTFor(user.TenantID, user.ID, []string{accessRole}, "forge", externalSessionTTL)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "could not issue product session"})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"token": token, "tokenType": "Bearer", "expiresIn": int(externalSessionTTL.Seconds()),
		"subject":      map[string]string{"id": user.ID, "externalId": identity.Subject, "email": identity.Email, "name": user.DisplayName},
		"organization": map[string]string{"id": user.TenantID},
		"permissions":  productPermissions(accessRole),
	})
}
