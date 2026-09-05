package api

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/labstack/echo/v4"
)

func (s *Server) handleListFinalDeliverables(c echo.Context) error {
	if s.Deliverables == nil {
		return finalDeliverableUnavailable(c)
	}
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	items, err := s.Deliverables.List(c.Request().Context(), getTenant(c), deliverable.ListFilter{
		ProjectID:      c.QueryParam("project_id"),
		ConversationID: c.QueryParam("conversation_id"),
		RunID:          c.QueryParam("run_id"),
		Limit:          limit,
		Offset:         offset,
	})
	if err != nil {
		return finalDeliverableFailure(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"deliverables": items})
}

func (s *Server) handleGetFinalDeliverable(c echo.Context) error {
	if s.Deliverables == nil {
		return finalDeliverableUnavailable(c)
	}
	item, err := s.Deliverables.Get(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		return finalDeliverableFailure(c, err)
	}
	if c.QueryParam("path") != "" || c.QueryParam("offset") != "" || c.QueryParam("limit") != "" {
		var content any
		decoder := json.NewDecoder(strings.NewReader(item.Content))
		decoder.UseNumber()
		if err := decoder.Decode(&content); err != nil {
			content = item.Content
		}
		return writeSelectedJSONResponse(c, content, "deliverable")
	}
	return c.JSON(http.StatusOK, item)
}

func (s *Server) handleDownloadFinalDeliverable(c echo.Context) error {
	if s.Deliverables == nil {
		return finalDeliverableUnavailable(c)
	}
	item, err := s.Deliverables.Get(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		return finalDeliverableFailure(c, err)
	}
	filename := workflowArtifactFilename(item)
	if filename != "" {
		c.Response().Header().Set(echo.HeaderContentDisposition, mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
		return c.Blob(http.StatusOK, item.ContentType+"; charset=utf-8", []byte(item.Content))
	}
	extension := "md"
	switch {
	case strings.HasPrefix(item.ContentType, "image/svg+xml") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(item.Content)), "<svg"):
		extension = "svg"
	case deliverable.LooksLikeHTMLDocument(item.Content) || strings.HasPrefix(item.ContentType, "text/html"):
		extension = "html"
	case strings.HasPrefix(item.ContentType, "application/x-ndjson") || strings.HasPrefix(item.ContentType, "application/jsonl"):
		extension = "jsonl"
	case strings.HasPrefix(item.ContentType, "application/json"):
		extension = "json"
	case strings.HasPrefix(item.ContentType, "text/plain"):
		extension = "txt"
	}
	filename = "weave-deliverable-" + item.ID + "." + extension
	c.Response().Header().Set(echo.HeaderContentDisposition, mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	return c.Blob(http.StatusOK, item.ContentType+"; charset=utf-8", []byte(item.Content))
}

func workflowArtifactFilename(item deliverable.FinalDeliverable) string {
	var metadata struct {
		Filename string `json:"filename"`
	}
	if json.Unmarshal(item.Metadata, &metadata) != nil {
		return ""
	}
	filename := path.Base(strings.ReplaceAll(strings.TrimSpace(metadata.Filename), "\\", "/"))
	if filename == "." || filename == "/" || filename == "" {
		return ""
	}
	return strings.ReplaceAll(strings.ReplaceAll(filename, "\r", ""), "\n", "")
}

func finalDeliverableUnavailable(c echo.Context) error {
	return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "final_deliverables_unavailable"})
}

func finalDeliverableFailure(c echo.Context, err error) error {
	if errors.Is(err, deliverable.ErrNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "final_deliverable_not_found"})
	}
	return c.JSON(http.StatusInternalServerError, map[string]string{"error": "final_deliverable_read_failed"})
}
