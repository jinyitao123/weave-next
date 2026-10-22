package api

import (
	"context"
	"net/http"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/labstack/echo/v4"
)

// Shared provider deletion reuses the credential revocation fence and retains
// immutable revisions. The organization role check is at the registered route;
// this grant is restricted to this workspace's exact shared provider.
func (s *Server) handleDeleteDevelopmentModelProvider(c echo.Context) error {
	if s.Credentials == nil || s.GetPool() == nil {
		return credentialStoreUnavailable(c)
	}
	id, workspace := c.Param("id"), getTenant(c)
	var scope, source, service string
	err := s.GetPool().QueryRow(c.Request().Context(), `SELECT credential_scope,source_kind,credential_service_id
		FROM weave_provider_credentials WHERE workspace_id=$1 AND id=$2`, workspace, id).Scan(&scope, &source, &service)
	if err != nil || scope != string(frozen.CredentialScopeWorkspaceService) || source != "workspace" || service != "workspace-provider:"+id {
		return workflowError(c, http.StatusNotFound, "model_provider_not_found", "这个供应方不存在或不能在这里删除")
	}
	ctx := credentials.WithServiceReferenceAuthorization(c.Request().Context(), func(_ context.Context, subject execution.Subject, ref frozen.CredentialReference) error {
		if subject.UserID != "" && subject.WorkspaceID == workspace && ref.WorkspaceID == workspace &&
			ref.Scope == frozen.CredentialScopeWorkspaceService && ref.Kind == frozen.CredentialProviderAPIKey && ref.ResourceID == id && ref.ServiceID == service {
			return nil
		}
		return execution.ErrSubjectMismatch
	})
	c.SetRequest(c.Request().WithContext(ctx))
	return s.handleDeleteProvider(c)
}
