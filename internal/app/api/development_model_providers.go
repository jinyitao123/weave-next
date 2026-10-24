package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
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

// The models a workspace shares come from providers: an address, the wire
// protocol, the model names it serves and an optional key. Developers and
// administrators add or replace one here, so a team can be given a model
// without leaving the place it is being built in. The key is sealed on
// arrival and is never read back by any route; listings say only whether one
// is stored.

type developmentModelProvider struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	BaseURL string   `json:"base_url"`
	Models  []string `json:"models"`
	// ChatPath is the recorded path, empty for a provider saved before paths
	// were recorded; ChatURL is the address requests go to either way.
	ChatPath string `json:"chat_path"`
	ChatURL  string `json:"chat_url"`
	// OutputLimitField is the Chat Completions field the output limit is sent
	// as; empty means max_tokens.
	OutputLimitField string `json:"output_limit_field"`
	// ModelParameters declares per model ID the request values members may
	// set; a model left out has no declaration.
	ModelParameters map[string]frozen.ModelParameters `json:"model_parameters"`
	// Protocol is "openai" (chat completions) or "systemone" (decision models).
	Protocol       string `json:"protocol"`
	HasAPIKey      bool   `json:"has_api_key"`
	JSONObjectMode bool   `json:"json_object_mode"`
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
	rows, err := pool.Query(c.Request().Context(), `SELECT id,name,base_url,models,source_kind,protocol,api_key_set,json_object_mode,chat_path,output_limit_field,model_parameters FROM weave_provider_credentials
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
		var parameters []byte
		if err := rows.Scan(&provider.ID, &provider.Name, &provider.BaseURL, &provider.Models, &source, &provider.Protocol, &provider.HasAPIKey, &provider.JSONObjectMode, &provider.ChatPath, &provider.OutputLimitField, &parameters); err != nil {
			return workflowStoreFailure(c, err)
		}
		if err := json.Unmarshal(parameters, &provider.ModelParameters); err != nil {
			return workflowStoreFailure(c, err)
		}
		provider.ChatURL = frozen.ResolveProviderEndpoint(provider.Protocol, provider.BaseURL, provider.ChatPath).URL()
		provider.Deployment = source != "workspace"
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return workflowStoreFailure(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"version": "1", "providers": providers})
}

// handlePutDevelopmentModelProvider adds a shared provider or replaces one
// that was added here. The key is optional: leaving api_key out (or sending
// the masked placeholder) keeps the stored key, a value replaces it, and an
// explicit "" clears it. protocol is "openai" (default) or "systemone".
// chat_path is appended to base_url to form the request address; omitted, a
// new provider records the address its protocol's client has always used and
// an existing one keeps what it has.
func (s *Server) handlePutDevelopmentModelProvider(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil || s.Credentials == nil {
		return workflowError(c, http.StatusServiceUnavailable, "development_model_provider_unavailable", "组织模型暂时无法配置")
	}
	invalid := func(message string) error {
		return workflowError(c, http.StatusUnprocessableEntity, "model_provider_invalid", message)
	}
	var request struct {
		Name           string   `json:"name"`
		BaseURL        string   `json:"base_url"`
		ChatPath       *string  `json:"chat_path"`
		Protocol       string   `json:"protocol"`
		APIKey         *string  `json:"api_key"`
		Models         []string `json:"models"`
		JSONObjectMode *bool    `json:"json_object_mode"`
		// OutputLimitField is max_tokens or max_completion_tokens; omitted keeps
		// the current declaration and "" clears it.
		OutputLimitField *string `json:"output_limit_field"`
		// ModelParameters declares per model ID the request values members may
		// set; omitted keeps the current declarations and {} clears them.
		ModelParameters *map[string]frozen.ModelParameters `json:"model_parameters"`
	}
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	id, name := c.Param("id"), strings.TrimSpace(request.Name)
	if !developmentModelProviderID.MatchString(id) {
		return invalid("供应方标识只能用小写字母、数字和连字符，以字母或数字开头，不超过 63 个字符")
	}
	if name == "" || utf8.RuneCountInString(name) > 80 {
		return invalid("请给出供应方名称（80 字以内）")
	}
	if _, err := llmrouter.NormalizeProtocol(request.Protocol); err != nil {
		return workflowError(c, http.StatusBadRequest, "provider_protocol_invalid", "协议只能是 openai 或 systemone")
	}
	key, clearKey, masked := "", false, false
	if request.APIKey != nil {
		switch raw := *request.APIKey; {
		case raw == "":
			clearKey = true
		case raw == credentials.MaskedKey:
			masked = true // keeps the stored key; never a new key
		default:
			key = strings.TrimSpace(raw)
			if key == "" || key != raw || len(key) > 4096 || strings.ContainsAny(key, " \t\r\n") {
				return invalid("密钥格式无效")
			}
		}
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
	tx, err := pool.Begin(ctx)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize shared model claims across server processes before checking or saving.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "development-model-providers:"+tenant); err != nil {
		return workflowStoreFailure(c, err)
	}
	// An omitted protocol or output mode keeps the provider's current one, as
	// an omitted key does; a new provider starts from OpenAI chat with strict
	// JSON Schema output.
	var scope, source, currentProtocol, currentChatPath, currentOutputLimit string
	var currentJSONObjectMode bool
	var currentParameters []byte
	err = tx.QueryRow(ctx, `SELECT credential_scope,source_kind,protocol,json_object_mode,chat_path,output_limit_field,model_parameters FROM weave_provider_credentials WHERE workspace_id=$1 AND id=$2`,
		tenant, id).Scan(&scope, &source, &currentProtocol, &currentJSONObjectMode, &currentChatPath, &currentOutputLimit, &currentParameters)
	existing := err == nil
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// A new provider may have no key (local and decision-model servers),
		// but the masked placeholder is not a key it could keep.
		if masked {
			return invalid("密钥格式无效")
		}
	case err != nil:
		return workflowStoreFailure(c, err)
	case scope != string(frozen.CredentialScopeWorkspaceService) || source != "workspace":
		// A personal provider or one that came with the deployment keeps its identity.
		return workflowError(c, http.StatusConflict, "model_provider_conflict", "这个供应方标识已被占用，请换一个")
	}
	protocolInput := request.Protocol
	if protocolInput == "" {
		protocolInput = currentProtocol
	}
	protocol, err := llmrouter.NormalizeProtocol(protocolInput)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	jsonObjectMode := currentJSONObjectMode
	if request.JSONObjectMode != nil {
		jsonObjectMode = *request.JSONObjectMode
	}
	outputLimit := currentOutputLimit
	if request.OutputLimitField != nil {
		outputLimit = strings.TrimSpace(*request.OutputLimitField)
	}
	if protocol == llmrouter.ProtocolSystemOne {
		outputLimit = "" // only the OpenAI-compatible client has a field to choose
	}
	if err := frozen.ValidateOutputLimitField("", outputLimit); err != nil {
		return invalid("输出上限字段只能是 max_tokens 或 max_completion_tokens")
	}
	parameters, refusal := requestedModelParameters(request.ModelParameters, currentParameters, models, protocol)
	if refusal != "" {
		return invalid(refusal)
	}
	chatPath := request.ChatPath
	if chatPath == nil && currentChatPath != "" {
		chatPath = &currentChatPath
	}
	endpoint := frozen.ProviderEndpoint{BaseURL: strings.TrimSpace(request.BaseURL)}
	if chatPath != nil || !existing {
		// An existing provider saved before paths were recorded keeps that form,
		// and its address, until a path is given.
		if endpoint, err = requestedProviderEndpoint(protocol, request.BaseURL, chatPath); err != nil {
			return providerEndpointRefusal(c, err)
		}
	}
	// A model is served by one shared provider, or the choice at run time would be ambiguous.
	var taken, by string
	err = tx.QueryRow(ctx, `SELECT model,name FROM weave_provider_credentials, unnest(models) AS model
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
	var options []credentials.UpsertOption
	if clearKey {
		options = append(options, credentials.ClearAPIKey())
	}
	result, err := s.Credentials.UpsertRevisionTx(authorized, tx, tenant, llmrouter.ProviderConfig{
		CredentialScope: frozen.CredentialScopeWorkspaceService, CredentialServiceID: service,
		ID: id, Name: name, BaseURL: endpoint.BaseURL, ChatPath: endpoint.ChatPath, OutputLimitField: outputLimit, Protocol: protocol, APIKey: key, Models: models,
		JSONObjectMode: jsonObjectMode, ModelParameters: parameters,
	}, options...)
	if err != nil {
		var coded *credentials.Error
		if errors.As(err, &coded) && errors.Is(coded, credentials.ErrProviderRevisionRequired) {
			return invalid("供应方地址须是完整的 http 或 https 地址，不带账号、查询参数和片段")
		}
		if errors.As(err, &coded) {
			return workflowError(c, http.StatusConflict, "model_provider_conflict", "这个供应方现在不能修改")
		}
		return workflowStoreFailure(c, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return workflowStoreFailure(c, err)
	}
	if s.Models != nil {
		s.Models.Invalidate(tenant)
	}
	slog.Info("workspace model provider configured", "workspace", tenant, "provider", id, "actor", getUserID(c),
		"protocol", protocol, "key_replaced", key != "", "key_cleared", clearKey)
	return c.JSON(http.StatusOK, map[string]any{
		"id": id, "name": name, "protocol": protocol, "models": models, "json_object_mode": jsonObjectMode,
		"base_url": endpoint.BaseURL, "chat_path": endpoint.ChatPath, "chat_url": frozen.ResolveProviderEndpoint(protocol, endpoint.BaseURL, endpoint.ChatPath).URL(),
		"output_limit_field": outputLimit, "model_parameters": modelParametersView(parameters),
		"key_replaced": key != "", "key_cleared": clearKey, "has_api_key": result.Head.APIKeySet,
	})
}

// requestedModelParameters resolves the declarations a provider update saves.
// Omitted keeps the current ones for the models still served; sent ones must
// name served models and be valid. The refusal is a message for the developer.
func requestedModelParameters(requested *map[string]frozen.ModelParameters, current []byte, models []string, protocol string) (map[string]frozen.ModelParameters, string) {
	values := map[string]frozen.ModelParameters{}
	if requested == nil {
		if len(current) > 0 {
			if err := json.Unmarshal(current, &values); err != nil {
				return nil, "已保存的模型参数声明无法读取"
			}
		}
		for model := range values {
			if !slices.Contains(models, model) {
				delete(values, model) // a model no longer served takes its declaration with it
			}
		}
	} else if *requested != nil {
		values = *requested
	}
	if len(values) == 0 {
		return nil, ""
	}
	if protocol != llmrouter.ProtocolOpenAI {
		return nil, "只有 OpenAI 兼容协议的供应方可以声明模型参数"
	}
	normalized := make(map[string]frozen.ModelParameters, len(values))
	for model, declared := range values {
		if !slices.Contains(models, model) {
			return nil, "模型参数声明了供应方未提供的模型“" + model + "”"
		}
		value, err := frozen.NormalizeModelParameters(declared)
		if err != nil {
			return nil, "模型“" + model + "”的参数声明无效：" + err.Error()
		}
		normalized[model] = value
	}
	return normalized, ""
}

func modelParametersView(values map[string]frozen.ModelParameters) map[string]frozen.ModelParameters {
	if values == nil {
		return map[string]frozen.ModelParameters{}
	}
	return values
}
