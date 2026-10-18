package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/labstack/echo/v4"
)

const externalSessionTTL = 14 * 24 * time.Hour

const forgeTeamDeveloperPermissionSet = "weave_team_developer"

type ExternalIdentity struct {
	// Issuer is Forge's stable deployment identity source. It is not a URL.
	Issuer string
	// BaseURL is the current network address used only for Forge requests.
	BaseURL            string
	Subject            string
	Email              string
	Name               string
	Organization       string
	NativeOrganization string
	AccessRole         string
	PermissionSets     []string
}

var forgeIdentityIssuerPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:\S{1,240}$`)

func validForgeIdentityIssuer(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 || !forgeIdentityIssuerPattern.MatchString(value) {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" {
		return false
	}
	// A deployment identity is a stable URI, never the mutable service address.
	return (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == ""
}

type ExternalIdentityVerifier interface {
	Verify(context.Context, string) (ExternalIdentity, error)
}

type ForgeSessionVerifier struct {
	endpoint         *url.URL
	defaultWorkspace string
	client           *http.Client
}

type forgeIdentityFailure struct {
	status int
	code   string
}

func (e *forgeIdentityFailure) Error() string    { return e.code }
func forgeFailure(status int, code string) error { return &forgeIdentityFailure{status, code} }
func forgeResponseFailure(status int) error {
	if status == http.StatusUnauthorized {
		return forgeFailure(status, "forge_session_invalid")
	}
	if status == http.StatusForbidden {
		return forgeFailure(status, "forge_account_forbidden")
	}
	return forgeFailure(http.StatusServiceUnavailable, "forge_identity_unavailable")
}

func NewForgeSessionVerifier(rawURL, defaultWorkspace string, client *http.Client) *ForgeSessionVerifier {
	endpoint, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil {
		endpoint = nil
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	configuredClient := *client
	configuredClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ForgeSessionVerifier{endpoint: endpoint, defaultWorkspace: strings.TrimSpace(defaultWorkspace), client: &configuredClient}
}

func (v *ForgeSessionVerifier) Verify(ctx context.Context, bearer string) (ExternalIdentity, error) {
	if v == nil || v.endpoint == nil {
		return ExternalIdentity{}, forgeFailure(http.StatusServiceUnavailable, "external identity is not configured")
	}
	if strings.TrimSpace(bearer) == "" {
		return ExternalIdentity{}, forgeFailure(http.StatusUnauthorized, "forge_session_invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.endpoint.String(), nil)
	if err != nil {
		return ExternalIdentity{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+bearer)
	response, err := v.client.Do(request)
	if err != nil {
		return ExternalIdentity{}, forgeFailure(http.StatusServiceUnavailable, "forge_identity_unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return ExternalIdentity{}, forgeResponseFailure(response.StatusCode)
	}
	var body struct {
		Sub   string `json:"sub"`
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
		User  struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"user"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
		return ExternalIdentity{}, forgeFailure(http.StatusServiceUnavailable, "forge_identity_response_invalid")
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
	if subject == "" {
		return ExternalIdentity{}, forgeFailure(http.StatusUnauthorized, "forge_session_invalid")
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
		return ExternalIdentity{}, forgeFailure(http.StatusServiceUnavailable, "forge_identity_unavailable")
	}
	defer sessionResponse.Body.Close()
	if sessionResponse.StatusCode != http.StatusOK {
		return ExternalIdentity{}, forgeResponseFailure(sessionResponse.StatusCode)
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
	if json.NewDecoder(io.LimitReader(sessionResponse.Body, 1<<20)).Decode(&nativeSession) != nil {
		return ExternalIdentity{}, forgeFailure(http.StatusServiceUnavailable, "forge_identity_response_invalid")
	}
	if nativeSession.User.ID != subject || (nativeSession.Session.UserID != "" && nativeSession.Session.UserID != subject) {
		return ExternalIdentity{}, forgeFailure(http.StatusUnauthorized, "forge_session_invalid")
	}
	if strings.TrimSpace(nativeSession.Session.ActiveOrganizationID) == "" {
		return ExternalIdentity{}, forgeFailure(http.StatusForbidden, "forge_organization_required")
	}
	workspace := v.defaultWorkspace
	if workspace == "" {
		workspace = nativeSession.Session.ActiveOrganizationID
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
		return ExternalIdentity{}, forgeFailure(http.StatusServiceUnavailable, "forge_identity_unavailable")
	}
	defer permissionResponse.Body.Close()
	if permissionResponse.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(permissionResponse.Body, 64<<10))
		return ExternalIdentity{}, forgeResponseFailure(permissionResponse.StatusCode)
	}
	var permissionBody struct {
		Authenticated  bool     `json:"authenticated"`
		PermissionSets []string `json:"permissionSets"`
	}
	if err := json.NewDecoder(io.LimitReader(permissionResponse.Body, 1<<20)).Decode(&permissionBody); err != nil {
		return ExternalIdentity{}, forgeFailure(http.StatusServiceUnavailable, "forge_identity_response_invalid")
	}
	if !permissionBody.Authenticated {
		return ExternalIdentity{}, forgeFailure(http.StatusForbidden, "forge_account_forbidden")
	}
	identitySourceURL := *v.endpoint
	identitySourceURL.Path, identitySourceURL.RawPath, identitySourceURL.RawQuery, identitySourceURL.Fragment = "/api/v1/workbench/identity-source", "", "", ""
	identitySourceRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, identitySourceURL.String(), nil)
	if err != nil {
		return ExternalIdentity{}, err
	}
	identitySourceRequest.Header.Set("Accept", "application/json")
	identitySourceResponse, err := v.client.Do(identitySourceRequest)
	if err != nil {
		return ExternalIdentity{}, forgeFailure(http.StatusServiceUnavailable, "forge_identity_source_unavailable")
	}
	defer identitySourceResponse.Body.Close()
	if identitySourceResponse.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(identitySourceResponse.Body, 64<<10))
		return ExternalIdentity{}, forgeFailure(http.StatusServiceUnavailable, "forge_identity_source_unavailable")
	}
	var identitySource struct {
		Version string `json:"version"`
		Issuer  string `json:"issuer"`
	}
	if json.NewDecoder(io.LimitReader(identitySourceResponse.Body, 64<<10)).Decode(&identitySource) != nil ||
		identitySource.Version != "1" || !validForgeIdentityIssuer(identitySource.Issuer) {
		return ExternalIdentity{}, forgeFailure(http.StatusServiceUnavailable, "forge_identity_source_unavailable")
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
		Issuer: strings.TrimSpace(identitySource.Issuer), BaseURL: v.endpoint.Scheme + "://" + v.endpoint.Host, Subject: subject,
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

type stableNativeExternalIdentityBinder interface {
	BindExternalInOrganizationFromOrigin(context.Context, string, string, string, string, string, string, string) (*users.User, error)
}

// externalIdentityExchange is one verified, bound external identity and the
// product session issued for it.
type externalIdentityExchange struct {
	Identity   ExternalIdentity
	User       *users.User
	AccessRole string
	Token      string
}

// exchangeFailure keeps the exchange endpoint's established status codes and
// messages so every caller reports the same refusal.
type exchangeFailure struct {
	Status  int
	Message string
}

// bindVerifiedExternalIdentity proves a Forge session and resolves the Weave
// account bound to it, without issuing anything.
func (s *Server) bindVerifiedExternalIdentity(ctx context.Context, bearer string) (ExternalIdentity, *users.User, string, *exchangeFailure) {
	if s.ExternalIdentity == nil {
		return ExternalIdentity{}, nil, "", &exchangeFailure{http.StatusServiceUnavailable, "external identity is not configured"}
	}
	identity, err := s.ExternalIdentity.Verify(ctx, bearer)
	if err != nil {
		var failure *forgeIdentityFailure
		if errors.As(err, &failure) {
			if s.Echo != nil {
				s.Echo.Logger.Warnf("Forge identity verification: %s", failure.code)
			}
			return identity, nil, "", &exchangeFailure{failure.status, failure.code}
		}
		return identity, nil, "", &exchangeFailure{http.StatusUnauthorized, "external identity verification failed"}
	}
	binder := s.ExternalIdentityBinder
	if binder == nil && s.UserStore != nil {
		binder = s.UserStore
	}
	if binder == nil {
		return identity, nil, "", &exchangeFailure{http.StatusServiceUnavailable, "account binding is not configured"}
	}
	if identity.NativeOrganization == "" || identity.Issuer == "" || identity.BaseURL == "" {
		return identity, nil, "", &exchangeFailure{http.StatusForbidden, "native organization binding is unavailable"}
	}
	var user *users.User
	if stableBinder, supported := binder.(stableNativeExternalIdentityBinder); supported {
		user, err = stableBinder.BindExternalInOrganizationFromOrigin(ctx, identity.Issuer, identity.BaseURL, identity.Subject, identity.Organization, identity.Email, identity.Name, identity.NativeOrganization)
	} else if nativeBinder, supported := binder.(nativeExternalIdentityBinder); supported {
		user, err = nativeBinder.BindExternalInOrganization(ctx, identity.Issuer, identity.Subject, identity.Organization, identity.Email, identity.Name, identity.NativeOrganization)
	} else {
		return identity, nil, "", &exchangeFailure{http.StatusForbidden, "native organization binding is unavailable"}
	}
	if err != nil {
		return identity, nil, "", &exchangeFailure{http.StatusForbidden, "account binding failed"}
	}
	accessRole := identity.AccessRole
	if accessRole != "developer" && accessRole != "admin" {
		accessRole = "member"
	}
	return identity, user, accessRole, nil
}

func (s *Server) exchangeExternalIdentity(ctx context.Context, bearer string) (*externalIdentityExchange, *exchangeFailure) {
	identity, user, accessRole, failure := s.bindVerifiedExternalIdentity(ctx, bearer)
	if failure != nil {
		return nil, failure
	}
	token, err := s.signJWTWithPermissionSets(user.TenantID, user.ID, []string{accessRole}, "forge", identity.PermissionSets, externalSessionTTL)
	if err != nil {
		return nil, &exchangeFailure{http.StatusInternalServerError, "could not issue product session"}
	}
	// Developers keep the console's business action choices current from the
	// session they just proved; the token is not kept.
	s.captureBusinessCatalog(identity.BaseURL, bearer, user.TenantID, user.ID, accessRole)
	return &externalIdentityExchange{Identity: identity, User: user, AccessRole: accessRole, Token: token}, nil
}

func (s *Server) handleExternalIdentityExchange(c echo.Context) error {
	authorization := c.Request().Header.Get("Authorization")
	bearer := strings.TrimPrefix(authorization, "Bearer ")
	if bearer == authorization || bearer == "" {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "missing external identity token"})
	}
	exchanged, failure := s.exchangeExternalIdentity(c.Request().Context(), bearer)
	if failure != nil {
		return c.JSON(failure.Status, map[string]string{"error": failure.Message})
	}
	identity, user := exchanged.Identity, exchanged.User
	return c.JSON(http.StatusOK, map[string]any{
		"token": exchanged.Token, "tokenType": "Bearer", "expiresIn": int(externalSessionTTL.Seconds()),
		"subject":      map[string]string{"id": user.ID, "externalId": identity.Subject, "email": identity.Email, "name": user.DisplayName},
		"organization": map[string]string{"id": user.TenantID},
		"permissions":  productPermissions(exchanged.AccessRole),
		"issuer":       identity.Issuer,
	})
}
