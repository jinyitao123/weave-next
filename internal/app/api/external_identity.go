package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/labstack/echo/v4"
)

const externalSessionTTL = 8 * time.Hour

const forgeTeamDeveloperPermissionSet = "weave_team_developer"

type ExternalIdentity struct {
	// Issuer is Forge's stable identity source; it never changes with the
	// address Weave uses to reach Forge (decision 002).
	Issuer string
	// BaseURL is the Forge network address for follow-up calls.
	BaseURL        string
	Subject        string
	Email          string
	Name           string
	Organization   string
	AccessRole     string
	PermissionSets []string
}

type ExternalIdentityVerifier interface {
	Verify(context.Context, string) (ExternalIdentity, error)
}

type ForgeSessionVerifier struct {
	endpoint         *url.URL
	defaultWorkspace string
	client           *http.Client

	issuerMu sync.Mutex
	issuer   string
}

var forgeIssuerPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:\S{1,240}$`)

func (v *ForgeSessionVerifier) baseURL() string {
	return v.endpoint.Scheme + "://" + v.endpoint.Host
}

// stableIssuer reads Forge's configured identity source once. A failed read is
// not cached, and nothing falls back to the network address: binding by
// address is exactly what the stable issuer replaces.
func (v *ForgeSessionVerifier) stableIssuer(ctx context.Context) (string, error) {
	v.issuerMu.Lock()
	defer v.issuerMu.Unlock()
	if v.issuer != "" {
		return v.issuer, nil
	}
	sourceURL := *v.endpoint
	sourceURL.Path, sourceURL.RawPath, sourceURL.RawQuery, sourceURL.Fragment = "/api/v1/workbench/identity-source", "", "", ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL.String(), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/json")
	response, err := v.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("read Forge identity source: %w", err)
	}
	defer response.Body.Close()
	var body struct {
		Version string `json:"version"`
		Issuer  string `json:"issuer"`
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return "", fmt.Errorf("read Forge identity source: status %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body); err != nil ||
		body.Version != "1" || !forgeIssuerPattern.MatchString(strings.TrimSpace(body.Issuer)) {
		return "", errors.New("invalid Forge identity source")
	}
	v.issuer = strings.TrimSpace(body.Issuer)
	return v.issuer, nil
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
	issuer, err := v.stableIssuer(ctx)
	if err != nil {
		return ExternalIdentity{}, err
	}
	return ExternalIdentity{
		Issuer: issuer, BaseURL: v.baseURL(), Subject: subject,
		Email: email, Name: name,
		Organization: workspace, AccessRole: accessRole, PermissionSets: permissionBody.PermissionSets,
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
	user, err := binder.BindExternal(c.Request().Context(), identity.Issuer, identity.Subject, identity.Organization, identity.Email, identity.Name)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "account binding failed"})
	}
	accessRole := identity.AccessRole
	if accessRole != "developer" && accessRole != "admin" {
		accessRole = "member"
	}
	token, err := s.signJWTWithPermissionSets(user.TenantID, user.ID, []string{accessRole}, "forge", identity.PermissionSets, externalSessionTTL)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "could not issue product session"})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"token": token, "tokenType": "Bearer", "expiresIn": int(externalSessionTTL.Seconds()),
		"subject":      map[string]string{"id": user.ID, "externalId": identity.Subject, "email": identity.Email, "name": user.DisplayName},
		"organization": map[string]string{"id": user.TenantID},
		"permissions":  productPermissions(accessRole),
		"issuer":       identity.Issuer,
	})
}
