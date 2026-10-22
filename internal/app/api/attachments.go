package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	attachmentcatalog "github.com/jinyitao123/weave/internal/app/attachments"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/labstack/echo/v4"
)

func (s *Server) handleUploadAttachment(c echo.Context) error {
	if s.Config == nil || s.Config.WorkspacesRoot == "" {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "attachments storage not configured"})
	}
	if s.Attachments == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "attachments catalog not configured"})
	}
	file, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "file is required"})
	}
	if file.Size > maxUploadSize {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "file too large"})
	}
	filename := filepath.Base(file.Filename)
	if filename == "" || filename == "." || filename == ".." || strings.ContainsAny(filename, `/\`) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid filename"})
	}
	id := uuid.NewString()
	dir := filepath.Join(s.Config.WorkspacesRoot, getTenant(c), "_attachments", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "cannot create attachment directory"})
	}

	src, err := file.Open()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "cannot open file"})
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, filename), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "cannot store attachment"})
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(dst, hash), io.LimitReader(src, maxUploadSize+1))
	closeErr := dst.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.RemoveAll(dir)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "cannot store attachment"})
	}
	if size > maxUploadSize {
		_ = os.RemoveAll(dir)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "file too large"})
	}
	attachment, err := s.Attachments.Create(c.Request().Context(), attachmentcatalog.Attachment{
		ID: id, WorkspaceID: getTenant(c), Filename: filename, SizeBytes: size,
		SHA256: hex.EncodeToString(hash.Sum(nil)), ContentType: file.Header.Get("Content-Type"),
		CreatedBy: getUserID(c),
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "cannot catalog attachment"})
	}

	return c.JSON(http.StatusCreated, attachment)
}

func (s *Server) handleListAttachments(c echo.Context) error {
	if s.Attachments == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "attachments catalog not configured"})
	}
	items, err := s.Attachments.List(c.Request().Context(), getTenant(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "cannot list attachments"})
	}
	return c.JSON(http.StatusOK, map[string]any{"attachments": items})
}

func (s *Server) handleGetAttachment(c echo.Context) error {
	if s.Attachments == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "attachments catalog not configured"})
	}
	item, err := s.Attachments.Get(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		if errors.Is(err, attachmentcatalog.ErrNotFound) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "attachment_not_found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "cannot read attachment"})
	}
	return c.JSON(http.StatusOK, item)
}

// ResolveAttachment resolves one uploaded attachment within a workspace.
func ResolveAttachment(root, tenant, id string) (taskqueue.Attachment, error) {
	if root == "" {
		return taskqueue.Attachment{}, fmt.Errorf("attachments storage not configured")
	}
	if _, err := uuid.Parse(id); err != nil {
		return taskqueue.Attachment{}, fmt.Errorf("invalid attachment id")
	}

	root, err := filepath.Abs(root)
	if err != nil {
		return taskqueue.Attachment{}, fmt.Errorf("resolve attachments root: %w", err)
	}
	workspaceRoot := filepath.Clean(filepath.Join(root, tenant, "_attachments"))
	if !pathWithin(root, workspaceRoot) {
		return taskqueue.Attachment{}, fmt.Errorf("invalid workspace path")
	}
	dir := filepath.Clean(filepath.Join(workspaceRoot, id))
	if !pathWithin(workspaceRoot, dir) {
		return taskqueue.Attachment{}, fmt.Errorf("invalid attachment path")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return taskqueue.Attachment{}, fmt.Errorf("read attachment: %w", err)
	}
	if len(entries) != 1 {
		return taskqueue.Attachment{}, fmt.Errorf("attachment directory contains %d entries", len(entries))
	}
	filename := entries[0].Name()
	path := filepath.Clean(filepath.Join(dir, filename))
	if !pathWithin(dir, path) {
		return taskqueue.Attachment{}, fmt.Errorf("invalid attachment file path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return taskqueue.Attachment{}, fmt.Errorf("inspect attachment: %w", err)
	}
	if !info.Mode().IsRegular() {
		return taskqueue.Attachment{}, fmt.Errorf("attachment is not a regular file")
	}

	return taskqueue.Attachment{ID: id, Filename: filename, Path: path}, nil
}

func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

type messageAttachment struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
}

type userMessageMetadata struct {
	Attachments []messageAttachment `json:"attachments"`
}
