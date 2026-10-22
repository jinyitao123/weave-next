package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

// taskEvidenceExport is a read-only record of one task for people who were not
// watching it: what was asked, which exact code versions each stage handed
// over, the Host-run verification evidence and the rule-based verdict. It
// carries no credentials and no member prompts.
type taskEvidenceExport struct {
	Format     string           `json:"format"`
	Version    int              `json:"version"`
	ExportedAt time.Time        `json:"exported_at"`
	Content    taskEvidenceBody `json:"content"`
	// ContentSHA256 covers the JSON encoding of Content as written in this file.
	ContentSHA256 string `json:"content_sha256"`
}

type taskEvidenceBody struct {
	Task adminTask `json:"task"`
}

const adminTaskOutputLimit = 256 << 10

// handleGetAdminTaskOutput returns what one step of a run produced, to anyone
// who may read the run. Reading someone else's result is recorded like any
// other view of their run.
func (s *Server) handleGetAdminTaskOutput(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil || s.Deliverables == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "task_output_unavailable"})
	}
	runID := c.Param("id")
	tasks, err := s.queryAdminTasks(c, pool, adminTaskQuery{RunID: runID})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_output_failed"})
	}
	if len(tasks) == 0 {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "task_not_found"})
	}
	item, err := s.Deliverables.Get(c.Request().Context(), getTenant(c), c.Param("output"))
	if err != nil || item.RunID != runID {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "task_output_not_found"})
	}
	if !tasks[0].Mine {
		s.auditRunView(c, runID, "output")
	}
	content := item.Content
	if len(content) > adminTaskOutputLimit {
		content = strings.ToValidUTF8(content[:adminTaskOutputLimit], "")
	}
	return c.JSON(http.StatusOK, map[string]any{
		"id": item.ID, "title": item.Title, "content_type": item.ContentType,
		"content": content, "content_bytes": len(item.Content), "truncated": len(content) < len(item.Content),
	})
}

func (s *Server) handleExportAdminTaskEvidence(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "task_evidence_unavailable"})
	}
	runID := c.Param("id")
	tasks, err := s.queryAdminTasks(c, pool, adminTaskQuery{RunID: runID})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_evidence_failed"})
	}
	if len(tasks) == 0 {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "task_not_found"})
	}
	if !tasks[0].Mine {
		s.auditRunView(c, runID, "evidence")
	}
	body := taskEvidenceBody{Task: tasks[0]}
	content, err := json.Marshal(body)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_evidence_failed"})
	}
	sum := sha256.Sum256(content)
	export := taskEvidenceExport{Format: "weave-task-evidence", Version: 1, ExportedAt: time.Now().UTC(), Content: body, ContentSHA256: hex.EncodeToString(sum[:])}
	encoded, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "task_evidence_failed"})
	}
	name := "weave-evidence-" + tasks[0].CreatedAt.UTC().Format("20060102-150405") + ".json"
	c.Response().Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.Blob(http.StatusOK, "application/json; charset=utf-8", encoded)
}
