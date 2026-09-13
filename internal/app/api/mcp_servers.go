package api

import (
	"errors"
	"net/http"

	"github.com/jinyitao123/weave/internal/kernel/mcpprobe"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/labstack/echo/v4"
)

const mcpRegistryKeyUnavailable = "MCP registry encryption key is unavailable"

func (s *Server) handleListMCPServers(c echo.Context) error {
	if s.MCPRegistry == nil {
		return mcpRegistryUnavailable(c)
	}
	servers, err := s.MCPRegistry.List(c.Request().Context(), getTenant(c))
	if err != nil {
		return handleMCPRegistryError(c, err)
	}
	views, err := projectMCPServers(c.Request().Context(), s.Registry, getTenant(c), servers)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "工具使用情况暂时不可用，请稍后重试"})
	}
	return c.JSON(http.StatusOK, views)
}

func (s *Server) handleCreateMCPServer(c echo.Context) error {
	if s.MCPRegistry == nil {
		return mcpRegistryUnavailable(c)
	}
	req, err := bindMCPServerRequest(c)
	if err != nil {
		return handleMCPRequestError(c, err)
	}
	server, err := s.MCPRegistry.Create(
		c.Request().Context(), getTenant(c), getUserID(c), req,
	)
	if err != nil {
		return handleMCPRegistryError(c, err)
	}
	return s.writeMCPServerProductView(c, http.StatusCreated, server)
}

func (s *Server) handleGetMCPServer(c echo.Context) error {
	if s.MCPRegistry == nil {
		return mcpRegistryUnavailable(c)
	}
	server, err := s.MCPRegistry.Get(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		return handleMCPRegistryError(c, err)
	}
	return s.writeMCPServerProductView(c, http.StatusOK, server)
}

func (s *Server) handleUpdateMCPServer(c echo.Context) error {
	if s.MCPRegistry == nil {
		return mcpRegistryUnavailable(c)
	}
	req, err := bindMCPServerRequest(c)
	if err != nil {
		return handleMCPRequestError(c, err)
	}
	server, err := s.MCPRegistry.Update(
		c.Request().Context(), getTenant(c), c.Param("id"), req,
	)
	if err != nil {
		return handleMCPRegistryError(c, err)
	}
	return s.writeMCPServerProductView(c, http.StatusOK, server)
}

func (s *Server) handleDeleteMCPServer(c echo.Context) error {
	if s.MCPRegistry == nil {
		return mcpRegistryUnavailable(c)
	}
	err := s.MCPRegistry.Delete(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		return handleMCPRegistryError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleProbeMCPServer(c echo.Context) error {
	if s.MCPRegistry == nil {
		return mcpRegistryUnavailable(c)
	}
	result, err := mcpprobe.New(s.MCPRegistry).Probe(
		c.Request().Context(), getTenant(c), c.Param("id"),
	)
	if err != nil {
		switch {
		case errors.Is(err, mcpprobe.ErrProbeFailed):
			return c.JSON(http.StatusBadGateway, map[string]string{"error": err.Error()})
		default:
			return handleMCPRegistryError(c, err)
		}
	}
	return s.writeMCPCatalogProductView(c, result)
}

func (s *Server) handleGetMCPServerTools(c echo.Context) error {
	if s.MCPRegistry == nil {
		return mcpRegistryUnavailable(c)
	}
	result, err := s.MCPRegistry.Catalog(
		c.Request().Context(), getTenant(c), c.Param("id"),
	)
	if err != nil {
		return handleMCPRegistryError(c, err)
	}
	return s.writeMCPCatalogProductView(c, result)
}

func bindMCPServerRequest(c echo.Context) (mcpregistry.UpsertServerRequest, error) {
	var req mcpregistry.UpsertServerRequest
	if err := c.Bind(&req); err != nil {
		return mcpregistry.UpsertServerRequest{}, mcpRequestBodyError{}
	}
	if err := mcpregistry.ValidateUpsertServerRequest(req); err != nil {
		return mcpregistry.UpsertServerRequest{}, err
	}
	return req, nil
}

type mcpRequestBodyError struct{}

func (mcpRequestBodyError) Error() string { return "invalid request body" }
func (mcpRequestBodyError) Code() string  { return mcpregistry.CodeInvalidRequest }

func handleMCPRequestError(c echo.Context, err error) error {
	var bodyErr mcpRequestBodyError
	if errors.As(err, &bodyErr) {
		return writeMCPError(c, http.StatusBadRequest, err, err.Error())
	}
	return handleMCPRegistryError(c, err)
}

func handleMCPRegistryError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, mcpregistry.ErrNotFound):
		return writeMCPError(c, http.StatusNotFound, err, "MCP server not found")
	case errors.Is(err, mcpregistry.ErrConflict), errors.Is(err, mcpregistry.ErrInUse), errors.Is(err, mcpregistry.ErrClosed):
		return writeMCPError(c, http.StatusConflict, err, err.Error())
	case errors.Is(err, mcpregistry.ErrUnsupportedTransport), errors.Is(err, mcpregistry.ErrInvalidRequest):
		return writeMCPError(c, http.StatusUnprocessableEntity, err, err.Error())
	case errors.Is(err, mcpregistry.ErrKeyUnavailable):
		return writeMCPError(c, http.StatusServiceUnavailable, err, mcpRegistryKeyUnavailable)
	default:
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "MCP registry operation failed"})
	}
}

func writeMCPError(c echo.Context, status int, err error, detail string) error {
	payload := map[string]string{"error": detail}
	var coded interface{ Code() string }
	if errors.As(err, &coded) && coded.Code() != "" {
		payload["code"] = coded.Code()
	}
	return c.JSON(status, payload)
}

func mcpRegistryUnavailable(c echo.Context) error {
	return c.JSON(http.StatusServiceUnavailable, map[string]string{
		"code":  mcpregistry.CodeKeyUnavailable,
		"error": mcpRegistryKeyUnavailable,
	})
}
