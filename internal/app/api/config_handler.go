package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/admissionfence"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/labstack/echo/v4"
)

const credentialKeyNotConfigured = "credential encryption key not configured"

// --- Provider CRUD ---
//
// Handlers persist to the credentials store only; the workspace snapshot is
// rebuilt lazily via Models.Invalidate — no process-global Router mutation.

func (s *Server) handleListProviders(c echo.Context) error {
	if s.Credentials == nil {
		return credentialStoreUnavailable(c)
	}
	providers, err := s.Credentials.ListMetadata(c.Request().Context(), getTenant(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, providers)
}

func (s *Server) handleAddProvider(c echo.Context) error {
	if s.Credentials == nil {
		return credentialStoreUnavailable(c)
	}
	var req llmrouter.ProviderConfig
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if req.ID == "" {
		req.ID = req.Name
	}
	tenant := getTenant(c)
	if err := bindUserProviderOwner(c.Request().Context(), tenant, &req); err != nil {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "user credential identity is required"})
	}

	if _, err := s.Credentials.UpsertRevision(c.Request().Context(), tenant, req); err != nil {
		var coded *credentials.Error
		if errors.As(err, &coded) {
			return providerStoreError(c, coded)
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to persist provider: " + err.Error()})
	}
	if s.Models != nil {
		s.Models.Invalidate(tenant)
	}
	return c.JSON(http.StatusCreated, map[string]string{"status": "created"})
}

func (s *Server) handleUpdateProvider(c echo.Context) error {
	if s.Credentials == nil {
		return credentialStoreUnavailable(c)
	}
	id := c.Param("id")
	var req llmrouter.ProviderConfig
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	req.ID = id
	if req.Name == "" {
		req.Name = id
	}
	tenant := getTenant(c)
	if err := bindUserProviderOwner(c.Request().Context(), tenant, &req); err != nil {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "user credential identity is required"})
	}

	if _, err := s.Credentials.UpsertRevision(c.Request().Context(), tenant, req); err != nil {
		var coded *credentials.Error
		if errors.As(err, &coded) {
			return providerStoreError(c, coded)
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to persist provider: " + err.Error()})
	}
	if s.Models != nil {
		s.Models.Invalidate(tenant)
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteProvider(c echo.Context) error {
	if s.Credentials == nil {
		return credentialStoreUnavailable(c)
	}
	id := c.Param("id")
	tenant := getTenant(c)
	head, err := s.Credentials.GetHead(c.Request().Context(), tenant, id)
	if err != nil {
		return providerStoreError(c, credentials.ErrCredentialUnavailable)
	}
	ref := frozen.CredentialReference{
		SchemaVersion: frozen.FrozenSchemaVersion, WorkspaceID: tenant,
		Kind: frozen.CredentialProviderAPIKey, ResourceID: id, Slot: "api_key",
		Scope: head.CredentialScope, UserID: head.CredentialUserID, ServiceID: head.CredentialServiceID,
	}
	resource := admissionfence.Credential(ref, head.LatestRevision)
	return s.applyExternalAccessChange(c, "credential.delete", id,
		map[string]any{"provider_id": id, "credential_revision": head.LatestRevision},
		[]admissionfence.Resource{resource}, nil, []admissionfence.Resource{admissionfence.CredentialResource(ref)},
		func(ctx context.Context) (json.RawMessage, error) {
			if err := s.Credentials.Delete(ctx, tenant, id); err != nil {
				return nil, err
			}
			if s.Models != nil {
				s.Models.Invalidate(tenant)
			}
			return json.RawMessage(`{}`), nil
		}, http.StatusNoContent)
}

type mirrorSystemProviderRequest struct {
	Reason string `json:"reason"`
}

// systemProviderSummaryJSON is the safe read model for one process-configured
// provider. It never carries credentials; mirrored/mirrored_as describe the
// current workspace's mirror state so the console can offer the action only
// where it is meaningful.
type systemProviderSummaryJSON struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	BaseURL        string   `json:"base_url"`
	Models         []string `json:"models"`
	Mirrored       bool     `json:"mirrored"`
	MirroredAs     string   `json:"mirrored_as,omitempty"`
	MirrorRevision int64    `json:"mirror_revision,omitempty"`
}

func (s *Server) handleListSystemProviders(c echo.Context) error {
	if s.SystemProviders == nil {
		return providerStoreError(c, credentials.ErrProviderRevisionRequired)
	}
	lister, ok := s.SystemProviders.(credentials.SystemProviderLister)
	if !ok {
		return providerStoreError(c, credentials.ErrProviderRevisionRequired)
	}
	mirrored := map[string]credentials.ProviderHead{}
	if s.Credentials != nil {
		heads, err := s.Credentials.ListMetadata(c.Request().Context(), getTenant(c))
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		for _, head := range heads {
			if head.SourceKind == "system_mirror" && head.SourceProviderID != nil {
				mirrored[*head.SourceProviderID] = head
			}
		}
	}
	configs := lister.ListProviders()
	out := make([]systemProviderSummaryJSON, 0, len(configs))
	for _, cfg := range configs {
		summary := systemProviderSummaryJSON{
			ID:      cfg.ID,
			Name:    cfg.Name,
			BaseURL: cfg.BaseURL,
			Models:  append([]string(nil), cfg.Models...),
		}
		if head, exists := mirrored[cfg.ID]; exists {
			summary.Mirrored = true
			summary.MirroredAs = head.ID
			summary.MirrorRevision = head.LatestRevision
		}
		out = append(out, summary)
	}
	return c.JSON(http.StatusOK, out)
}

func (s *Server) handleMirrorSystemProvider(c echo.Context) error {
	if s.Credentials == nil {
		return credentialStoreUnavailable(c)
	}
	if s.SystemProviders == nil {
		return providerStoreError(c, credentials.ErrProviderRevisionRequired)
	}
	var request mirrorSystemProviderRequest
	decoder := json.NewDecoder(c.Request().Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}

	tenant := getTenant(c)
	systemProviderID := c.Param("id")
	ctx := credentials.WithServiceReferenceAuthorization(
		c.Request().Context(),
		func(_ context.Context, subject execution.Subject, ref frozen.CredentialReference) error {
			if subject.UserID == "" || subject.WorkspaceID != tenant ||
				ref.WorkspaceID != tenant ||
				ref.Scope != frozen.CredentialScopeWorkspaceService ||
				ref.ResourceID != "system/"+systemProviderID ||
				ref.ServiceID != "system-provider:"+systemProviderID {
				return execution.ErrSubjectMismatch
			}
			return nil
		},
	)
	result, err := s.Credentials.MirrorSystemProvider(
		ctx,
		tenant,
		getUserID(c),
		systemProviderID,
		s.SystemProviders,
	)
	if err != nil {
		var coded *credentials.Error
		if errors.As(err, &coded) {
			return providerStoreError(c, coded)
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to mirror system provider",
		})
	}
	if s.Models != nil {
		s.Models.Invalidate(tenant)
	}
	status := http.StatusOK
	if result.Outcome == credentials.MirrorOutcomeCreated {
		status = http.StatusCreated
	}
	return c.JSON(status, result)
}

func bindUserProviderOwner(ctx context.Context, workspaceID string, config *llmrouter.ProviderConfig) error {
	subject, err := execution.RequireSubject(ctx, workspaceID)
	if err != nil || subject.UserID == "" {
		return execution.ErrSubjectMismatch
	}
	config.CredentialScope = frozen.CredentialScopeUser
	config.CredentialUserID = subject.UserID
	config.CredentialServiceID = ""
	return nil
}

func providerStoreError(c echo.Context, coded *credentials.Error) error {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(coded, credentials.ErrProviderRevisionRequired):
		status = http.StatusBadRequest
	case errors.Is(coded, credentials.ErrCredentialUnavailable):
		status = http.StatusConflict
	}
	return c.JSON(status, map[string]string{"code": coded.Code(), "error": coded.Code()})
}

func credentialStoreUnavailable(c echo.Context) error {
	return c.JSON(http.StatusInternalServerError, map[string]string{"error": credentialKeyNotConfigured})
}
