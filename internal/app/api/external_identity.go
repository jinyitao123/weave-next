package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

const externalSessionTTL = 8 * time.Hour

type ExternalIdentity struct {
	Subject      string
	UserID       string
	Email        string
	Name         string
	Organization string
	Role         string
}

type ExternalIdentityVerifier interface {
	Verify(context.Context, string) (ExternalIdentity, error)
}

type ForgeUserInfoVerifier struct {
	endpoint         *url.URL
	defaultWorkspace string
	developers       map[string]struct{}
	client           *http.Client
}

func NewForgeUserInfoVerifier(rawURL, defaultWorkspace string, developerSubjects []string, client *http.Client) *ForgeUserInfoVerifier {
	endpoint, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil {
		endpoint = nil
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	developers := make(map[string]struct{}, len(developerSubjects))
	for _, subject := range developerSubjects {
		if subject = strings.TrimSpace(subject); subject != "" {
			developers[subject] = struct{}{}
		}
	}
	return &ForgeUserInfoVerifier{endpoint: endpoint, defaultWorkspace: strings.TrimSpace(defaultWorkspace), developers: developers, client: client}
}

func (v *ForgeUserInfoVerifier) Verify(ctx context.Context, bearer string) (ExternalIdentity, error) {
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
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
		return ExternalIdentity{}, errors.New("invalid external identity response")
	}
	subject := strings.TrimSpace(body.Sub)
	if subject == "" {
		subject = strings.TrimSpace(body.ID)
	}
	workspace := strings.TrimSpace(body.Organization.ID)
	if workspace == "" {
		workspace = v.defaultWorkspace
	}
	if subject == "" || workspace == "" {
		return ExternalIdentity{}, errors.New("external identity is missing subject or workspace")
	}
	digest := sha256.Sum256([]byte(v.endpoint.Host + "\x00" + subject))
	role := "member"
	if _, ok := v.developers[subject]; ok {
		role = "developer"
	}
	return ExternalIdentity{
		Subject: subject, UserID: "ext_" + hex.EncodeToString(digest[:16]),
		Email: strings.TrimSpace(body.Email), Name: strings.TrimSpace(body.Name),
		Organization: workspace, Role: role,
	}, nil
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
	token, err := s.signJWTFor(identity.Organization, identity.UserID, []string{identity.Role}, "external", externalSessionTTL)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "could not issue product session"})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"token": token, "tokenType": "Bearer", "expiresIn": int(externalSessionTTL.Seconds()),
		"subject":      map[string]string{"id": identity.UserID, "externalId": identity.Subject, "email": identity.Email, "name": identity.Name},
		"organization": map[string]string{"id": identity.Organization},
	})
}
