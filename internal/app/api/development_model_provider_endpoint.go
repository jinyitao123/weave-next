package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
)

// A provider's requests go to base_url + chat_path exactly. These routes let a
// configuration page show that address before saving, and try it afterwards
// through the client runs use, so a wrong address is found before a team is
// published.

type providerEndpointJSON struct {
	BaseURL  string `json:"base_url"`
	ChatPath string `json:"chat_path"`
	ChatURL  string `json:"chat_url"`
}

func providerEndpointView(endpoint frozen.ProviderEndpoint) providerEndpointJSON {
	return providerEndpointJSON{BaseURL: endpoint.BaseURL, ChatPath: endpoint.ChatPath, ChatURL: endpoint.URL()}
}

var providerEndpointMessages = map[string]string{
	frozen.EndpointBaseURLInvalid:       "供应方地址须是完整的 http 或 https 地址，不带账号、查询参数、片段和空路径段",
	frozen.EndpointChatPathInvalid:      "接口路径须以 / 开头，只含路径段，不带查询参数和结尾的 /",
	frozen.EndpointInBaseURL:            "供应方地址里已经包含接口路径。请把服务地址和接口路径分开填写",
	frozen.EndpointDuplicatePathSegment: "供应方地址和接口路径拼接后会出现重复的路径段，请求会发到错误的地址",
}

// providerEndpointRefusal answers a refused endpoint with its reason and, when
// one exists, the split to confirm instead; nothing is rewritten silently.
func providerEndpointRefusal(c echo.Context, err error) error {
	var refused *frozen.EndpointError
	if !errors.As(err, &refused) {
		return workflowError(c, http.StatusUnprocessableEntity, "model_provider_invalid", "供应方地址无效")
	}
	body := map[string]any{"code": "model_provider_endpoint_invalid", "reason": refused.Reason, "error": providerEndpointMessages[refused.Reason]}
	if refused.Suggestion != nil {
		body["suggestion"] = providerEndpointView(*refused.Suggestion)
	}
	return c.JSON(http.StatusUnprocessableEntity, body)
}

// requestedProviderEndpoint is the endpoint a provider form asks for. An
// omitted chat path records the address the provider has always reached, so
// existing callers keep their address and now see it.
func requestedProviderEndpoint(protocol, baseURL string, chatPath *string) (frozen.ProviderEndpoint, error) {
	if chatPath == nil {
		legacy := frozen.LegacyProviderEndpoint(protocol, baseURL)
		return frozen.NormalizeProviderEndpoint(protocol, legacy.BaseURL, legacy.ChatPath)
	}
	return frozen.NormalizeProviderEndpoint(protocol, baseURL, *chatPath)
}

// handleResolveDevelopmentModelEndpoint shows the address a configuration
// would send requests to, or why it is refused. It stores nothing.
func (s *Server) handleResolveDevelopmentModelEndpoint(c echo.Context) error {
	var request struct {
		Protocol string  `json:"protocol"`
		BaseURL  string  `json:"base_url"`
		ChatPath *string `json:"chat_path"`
	}
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	protocol, err := llmrouter.NormalizeProtocol(request.Protocol)
	if err != nil {
		return workflowError(c, http.StatusBadRequest, "provider_protocol_invalid", "协议只能是 openai 或 systemone")
	}
	endpoint, err := requestedProviderEndpoint(protocol, request.BaseURL, request.ChatPath)
	if err != nil {
		return providerEndpointRefusal(c, err)
	}
	return c.JSON(http.StatusOK, providerEndpointView(endpoint))
}

var providerProbeMessages = map[string]string{
	llmrouter.ProbeOK:               "连接成功，模型已回复",
	llmrouter.ProbeUnreachable:      "连不上这个地址：请检查域名、端口和网络",
	llmrouter.ProbeTimeout:          "等待回复超时：请检查地址和网络，或稍后再试",
	llmrouter.ProbeUnauthorized:     "地址可以访问，但密钥被拒绝：请检查密钥和它的权限",
	llmrouter.ProbeEndpointNotFound: "服务存在，但这个接口路径不存在：请检查供应方地址和接口路径",
	llmrouter.ProbeRequestRejected:  "接口存在，但拒绝了请求：请检查模型名称和接口协议",
	llmrouter.ProbeRateLimited:      "接口存在，但当前被限流或额度不足",
	llmrouter.ProbeProviderError:    "供应方服务出错，请稍后再试",
	llmrouter.ProbeInvalidResponse:  "收到的回复不符合所选接口协议：请检查接口协议和接口路径",
}

const providerProbeTimeout = 30 * time.Second

// handleProbeDevelopmentModelProvider sends one minimal request to a saved
// shared provider and reports where it went and what kind of answer came back,
// without returning keys, prompts or provider bodies.
func (s *Server) handleProbeDevelopmentModelProvider(c echo.Context) error {
	if s.GetPool() == nil || s.Credentials == nil {
		return workflowError(c, http.StatusServiceUnavailable, "development_model_provider_unavailable", "组织模型暂时无法读取")
	}
	var request struct {
		Model string `json:"model"`
	}
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	id, tenant := c.Param("id"), getTenant(c)
	if !developmentModelProviderID.MatchString(id) {
		return workflowError(c, http.StatusNotFound, "model_provider_not_found", "没有这个模型供应方")
	}
	service := "workspace-provider:" + id
	authorized := credentials.WithServiceReferenceAuthorization(c.Request().Context(), func(_ context.Context, subject execution.Subject, ref frozen.CredentialReference) error {
		if subject.UserID == "" || subject.WorkspaceID != tenant || ref.WorkspaceID != tenant ||
			ref.Scope != frozen.CredentialScopeWorkspaceService || ref.Kind != frozen.CredentialProviderAPIKey ||
			ref.ResourceID != id || ref.ServiceID != service {
			return execution.ErrSubjectMismatch
		}
		return nil
	})
	provider, err := s.Credentials.GetProvider(authorized, tenant, id)
	if err != nil {
		if errors.Is(err, credentials.ErrCredentialUnavailable) || errors.Is(err, execution.ErrSubjectMismatch) {
			return workflowError(c, http.StatusNotFound, "model_provider_not_found", "没有这个模型供应方")
		}
		return workflowStoreFailure(c, err)
	}
	model := strings.TrimSpace(request.Model)
	if model == "" && len(provider.Models) > 0 {
		model = provider.Models[0]
	}
	known := false
	for _, served := range provider.Models {
		known = known || served == model
	}
	if !known {
		return workflowError(c, http.StatusUnprocessableEntity, "model_provider_invalid", "这个供应方不提供模型“"+model+"”")
	}
	result := llmrouter.ProbeProvider(c.Request().Context(), provider, model, providerProbeTimeout)
	slog.Info("workspace model provider probed", "workspace", tenant, "provider", id, "actor", getUserID(c), "category", result.Category, "status", result.Status)
	return c.JSON(http.StatusOK, map[string]any{
		"ok": result.Category == llmrouter.ProbeOK, "chat_url": result.ChatURL, "model": result.Model,
		"category": result.Category, "status": result.Status, "latency_ms": result.LatencyMS,
		"message": providerProbeMessages[result.Category],
	})
}
