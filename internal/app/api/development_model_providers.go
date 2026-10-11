package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
)

// The models a workspace shares come from providers: an address, the model
// names it serves and a key. Developers and administrators add or replace one
// here, so a team can be given a model without leaving the place it is being
// built in. The key is sealed on arrival and is never read back by any route.

type developmentModelProvider struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	BaseURL string   `json:"base_url"`
	Models  []string `json:"models"`
	// A provider that came with the deployment is switched on by an
	// administrator and is not changed here.
	Deployment bool `json:"deployment"`
}

var developmentModelProviderID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (s *Server) handleListDevelopmentModelProviders(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return workflowError(c, http.StatusServiceUnavailable, "development_model_provider_unavailable", "组织模型暂时无法读取")
	}
	rows, err := pool.Query(c.Request().Context(), `SELECT id,name,base_url,models,source_kind FROM weave_provider_credentials
		WHERE workspace_id=$1 AND credential_scope='workspace_service' AND enabled AND revoked_at IS NULL AND deleted_at IS NULL AND latest_revision > 0
		ORDER BY name,id`, getTenant(c))
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	defer rows.Close()
	providers := []developmentModelProvider{}
	for rows.Next() {
		var provider developmentModelProvider
		var source string
		if err := rows.Scan(&provider.ID, &provider.Name, &provider.BaseURL, &provider.Models, &source); err != nil {
			return workflowStoreFailure(c, err)
		}
		provider.Deployment = source != "workspace"
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return workflowStoreFailure(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"version": "1", "providers": providers})
}

// handlePutDevelopmentModelProvider adds a shared provider or replaces one
// that was added here. Leaving the key out keeps the one already stored.
func (s *Server) handlePutDevelopmentModelProvider(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil || s.Credentials == nil {
		return workflowError(c, http.StatusServiceUnavailable, "development_model_provider_unavailable", "组织模型暂时无法配置")
	}
	invalid := func(message string) error {
		return workflowError(c, http.StatusUnprocessableEntity, "model_provider_invalid", message)
	}
	var request struct {
		Name    string   `json:"name"`
		BaseURL string   `json:"base_url"`
		APIKey  string   `json:"api_key"`
		Models  []string `json:"models"`
	}
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	id, name, key := c.Param("id"), strings.TrimSpace(request.Name), strings.TrimSpace(request.APIKey)
	if !developmentModelProviderID.MatchString(id) {
		return invalid("供应方标识只能用小写字母、数字和连字符，以字母或数字开头，不超过 63 个字符")
	}
	if name == "" || utf8.RuneCountInString(name) > 80 {
		return invalid("请给出供应方名称（80 字以内）")
	}
	if len(key) > 4096 || strings.ContainsAny(key, " \t\r\n") {
		return invalid("密钥格式无效")
	}
	seen, models := map[string]bool{}, []string{}
	for _, model := range request.Models {
		model = strings.TrimSpace(model)
		if model == "" || utf8.RuneCountInString(model) > 200 || strings.ContainsAny(model, " \t\r\n") {
			return invalid("模型名称不能为空、不能含空白，且不超过 200 个字符")
		}
		if !seen[model] {
			seen[model] = true
			models = append(models, model)
		}
	}
	if len(models) < 1 || len(models) > 32 {
		return invalid("请给出 1 至 32 个模型名称")
	}
	sort.Strings(models)

	ctx, tenant := c.Request().Context(), getTenant(c)
	var scope, source string
	err := pool.QueryRow(ctx, `SELECT credential_scope,source_kind FROM weave_provider_credentials WHERE workspace_id=$1 AND id=$2`, tenant, id).Scan(&scope, &source)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if key == "" {
			return invalid("新增模型供应方需要密钥")
		}
	case err != nil:
		return workflowStoreFailure(c, err)
	case scope != string(frozen.CredentialScopeWorkspaceService) || source != "workspace":
		// A personal provider or one that came with the deployment keeps its identity.
		return workflowError(c, http.StatusConflict, "model_provider_conflict", "这个供应方标识已被占用，请换一个")
	}
	// A model is served by one shared provider, or the choice at run time would be ambiguous.
	var taken, by string
	err = pool.QueryRow(ctx, `SELECT model,name FROM weave_provider_credentials, unnest(models) AS model
		WHERE workspace_id=$1 AND id<>$2 AND credential_scope='workspace_service' AND source_kind='workspace'
		  AND enabled AND revoked_at IS NULL AND deleted_at IS NULL AND model=ANY($3) ORDER BY model LIMIT 1`, tenant, id, models).Scan(&taken, &by)
	if err == nil {
		return workflowError(c, http.StatusConflict, "model_already_provided", "模型“"+taken+"”已由“"+by+"”提供；一个模型只能由一个组织供应方提供")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return workflowStoreFailure(c, err)
	}

	service := "workspace-provider:" + id
	authorized := credentials.WithServiceReferenceAuthorization(ctx, func(_ context.Context, subject execution.Subject, ref frozen.CredentialReference) error {
		if subject.UserID == "" || subject.WorkspaceID != tenant || ref.WorkspaceID != tenant ||
			ref.Scope != frozen.CredentialScopeWorkspaceService || ref.Kind != frozen.CredentialProviderAPIKey ||
			ref.ResourceID != id || ref.ServiceID != service {
			return execution.ErrSubjectMismatch
		}
		return nil
	})
	if _, err = s.Credentials.UpsertRevision(authorized, tenant, llmrouter.ProviderConfig{
		CredentialScope: frozen.CredentialScopeWorkspaceService, CredentialServiceID: service,
		ID: id, Name: name, BaseURL: strings.TrimSpace(request.BaseURL), APIKey: key, Models: models,
	}); err != nil {
		var coded *credentials.Error
		if errors.As(err, &coded) && errors.Is(coded, credentials.ErrProviderRevisionRequired) {
			return invalid("供应方地址须是完整的 http 或 https 地址，不带账号、查询参数和片段")
		}
		if errors.As(err, &coded) {
			return workflowError(c, http.StatusConflict, "model_provider_conflict", "这个供应方现在不能修改")
		}
		return workflowStoreFailure(c, err)
	}
	if s.Models != nil {
		s.Models.Invalidate(tenant)
	}
	slog.Info("workspace model provider configured", "workspace", tenant, "provider", id, "actor", getUserID(c), "key_replaced", key != "")
	return c.JSON(http.StatusOK, map[string]any{"id": id, "name": name, "models": models, "key_replaced": key != ""})
}
