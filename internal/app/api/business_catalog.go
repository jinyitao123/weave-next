package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
)

// The catalog is read with the developer's own Forge session, once per verified
// exchange, because the console holds no Forge credential. Only definitions are
// kept; the token is used for this one request and never stored or logged.
const (
	businessCatalogPath    = "/api/v1/workbench/business-actions/catalog"
	businessCatalogTimeout = 8 * time.Second
	businessCatalogMaxSize = 1 << 20
	businessCatalogMaxRows = 500
	businessCatalogMaxArgs = 64
)

type catalogParameter struct {
	Name        string   `json:"name"`
	Label       string   `json:"label,omitempty"`
	Type        string   `json:"type"`
	Multiple    bool     `json:"multiple,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

type catalogCapability struct {
	ID                     string             `json:"id"`
	Name                   string             `json:"name"`
	Description            string             `json:"description,omitempty"`
	Effect                 string             `json:"effect"`
	ExecutionMode          string             `json:"executionMode,omitempty"`
	ResourceType           string             `json:"resourceType,omitempty"`
	ObjectName             string             `json:"objectName,omitempty"`
	ActionName             string             `json:"actionName,omitempty"`
	RequiresRecord         bool               `json:"requiresRecord,omitempty"`
	RequiresConfirmation   bool               `json:"requiresConfirmation,omitempty"`
	RequiresEmployeeIntent bool               `json:"requiresEmployeeIntent,omitempty"`
	Status                 string             `json:"status"`
	UnavailableReason      string             `json:"unavailableReason,omitempty"`
	Params                 []catalogParameter `json:"params,omitempty"`
}

type forgeCatalog struct {
	Version      string              `json:"version"`
	Capabilities []catalogCapability `json:"capabilities"`
}

var errBusinessCatalogRejected = errors.New("business capability catalog response rejected")

// normalize keeps only recognised, bounded values so an unexpected catalog can
// neither bloat the snapshot nor smuggle fields into the console.
func (c *forgeCatalog) normalize() error {
	if strings.TrimSpace(c.Version) == "" || len(c.Version) > 64 || len(c.Capabilities) > businessCatalogMaxRows {
		return errBusinessCatalogRejected
	}
	seen := make(map[string]struct{}, len(c.Capabilities))
	for index := range c.Capabilities {
		item := &c.Capabilities[index]
		item.ID, item.Name = strings.TrimSpace(item.ID), strings.TrimSpace(item.Name)
		if item.ID == "" || len(item.ID) > 256 || item.Name == "" || len(item.Name) > 200 || len(item.Description) > 4000 || len(item.Params) > businessCatalogMaxArgs {
			return errBusinessCatalogRejected
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return errBusinessCatalogRejected
		}
		seen[item.ID] = struct{}{}
		if item.Effect != "read" && item.Effect != "write" {
			return errBusinessCatalogRejected
		}
		// A catalog that predates executionMode is team-delegable.
		if item.ExecutionMode == "" {
			item.ExecutionMode = "team_delegable"
		}
		if item.ExecutionMode != "employee_only" && item.ExecutionMode != "team_delegable" {
			return errBusinessCatalogRejected
		}
		if item.Status != "available" && item.Status != "unavailable" {
			return errBusinessCatalogRejected
		}
		for _, parameter := range item.Params {
			if strings.TrimSpace(parameter.Name) == "" || len(parameter.Name) > 128 {
				return errBusinessCatalogRejected
			}
		}
	}
	if c.Capabilities == nil {
		c.Capabilities = []catalogCapability{}
	}
	return nil
}

func (s *Server) readForgeBusinessCatalog(ctx context.Context, baseURL, bearer string) (forgeCatalog, error) {
	var catalog forgeCatalog
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || strings.TrimSpace(bearer) == "" {
		return catalog, errors.New("business capability catalog source is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, businessCatalogTimeout)
	defer cancel()
	endpoint := *base
	endpoint.Path, endpoint.RawPath, endpoint.RawQuery, endpoint.Fragment = businessCatalogPath, "", "", ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return catalog, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+bearer)
	client := &http.Client{Timeout: businessCatalogTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return catalog, errors.New("business capability catalog is unreachable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return catalog, errors.New("business capability catalog was refused")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, businessCatalogMaxSize+1))
	if err != nil || len(body) > businessCatalogMaxSize {
		return catalog, errBusinessCatalogRejected
	}
	if json.Unmarshal(body, &catalog) != nil {
		return catalog, errBusinessCatalogRejected
	}
	return catalog, catalog.normalize()
}

// refreshBusinessCatalog replaces the workspace snapshot. A failed read keeps
// the previous snapshot.
func (s *Server) refreshBusinessCatalog(ctx context.Context, baseURL, bearer, workspace, actor string) error {
	pool := s.GetPool()
	if pool == nil || workspace == "" || actor == "" {
		return errors.New("business capability catalog storage is unavailable")
	}
	catalog, err := s.readForgeBusinessCatalog(ctx, baseURL, bearer)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(catalog.Capabilities)
	if err != nil {
		return err
	}
	if _, err = pool.Exec(ctx, `INSERT INTO weave_business_capability_catalog(workspace_id,version,capabilities,entries,fetched_by,fetched_at)
 VALUES($1,$2,$3::jsonb,$4,$5,now())
 ON CONFLICT(workspace_id) DO UPDATE SET version=EXCLUDED.version,capabilities=EXCLUDED.capabilities,entries=EXCLUDED.entries,fetched_by=EXCLUDED.fetched_by,fetched_at=EXCLUDED.fetched_at`,
		workspace, catalog.Version, string(raw), len(catalog.Capabilities), actor); err != nil {
		return err
	}
	slog.Info("business capability catalog snapshot saved", "workspace", workspace, "actor", actor, "entries", len(catalog.Capabilities), "version", catalog.Version)
	return nil
}

// captureBusinessCatalog is the sign-in hook: best effort, detached from the
// request so a slow or failing Forge never delays or fails the sign-in.
func (s *Server) captureBusinessCatalog(baseURL, bearer, workspace, actor, role string) {
	if role != "developer" && role != "admin" && role != "owner" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), businessCatalogTimeout+2*time.Second)
		defer cancel()
		if err := s.refreshBusinessCatalog(ctx, baseURL, bearer, workspace, actor); err != nil {
			slog.Warn("business capability catalog snapshot not updated", "workspace", workspace, "reason", err.Error())
		}
	}()
}

// handleRefreshBusinessCapabilities re-reads the catalog on request. The
// console keeps no Forge credential, so the developer proves the same Forge
// account once more and that one session is used for this read only.
func (s *Server) handleRefreshBusinessCapabilities(c echo.Context) error {
	var request struct {
		ForgeToken string `json:"forge_token"`
	}
	if err := c.Bind(&request); err != nil || strings.TrimSpace(request.ForgeToken) == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "请重新验证 Forge 账号"})
	}
	bearer, ctx := strings.TrimSpace(request.ForgeToken), c.Request().Context()
	identity, user, role, failure := s.bindVerifiedExternalIdentity(ctx, bearer)
	if failure != nil {
		if failure.Status == http.StatusUnauthorized {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Forge 账号验证失败"})
		}
		return c.JSON(failure.Status, map[string]string{"error": failure.Message})
	}
	if user.ID != getUserID(c) || user.TenantID != getTenant(c) {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "验证的不是当前登录的账号"})
	}
	if role != "developer" && role != "admin" {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "当前账号不能读取业务动作目录"})
	}
	if err := s.refreshBusinessCatalog(ctx, identity.BaseURL, bearer, user.TenantID, user.ID); err != nil {
		slog.Warn("business capability catalog refresh failed", "workspace", user.TenantID, "reason", err.Error())
		return c.JSON(http.StatusBadGateway, map[string]string{"error": "没有读到业务动作目录，已保留上一份"})
	}
	return s.handleGetBusinessCapabilities(c)
}

type businessCapabilitiesView struct {
	Available    bool                `json:"available"`
	Version      string              `json:"version,omitempty"`
	FetchedAt    *time.Time          `json:"fetchedAt,omitempty"`
	FetchedBy    string              `json:"fetchedBy,omitempty"`
	Capabilities []catalogCapability `json:"capabilities"`
}

func (s *Server) handleGetBusinessCapabilities(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	view := businessCapabilitiesView{Capabilities: []catalogCapability{}}
	var raw []byte
	var fetchedAt time.Time
	err := pool.QueryRow(c.Request().Context(), `SELECT snapshot.version,snapshot.capabilities,snapshot.fetched_at,COALESCE(NULLIF(u.display_name,''),u.username,'')
 FROM weave_business_capability_catalog snapshot LEFT JOIN weave_users u ON u.id=snapshot.fetched_by
 WHERE snapshot.workspace_id=$1`, getTenant(c)).Scan(&view.Version, &raw, &fetchedAt, &view.FetchedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusOK, view)
	}
	if err != nil || json.Unmarshal(raw, &view.Capabilities) != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	view.Available, view.FetchedAt = true, &fetchedAt
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, view)
}
