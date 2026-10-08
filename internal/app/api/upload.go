package api

import (
	"archive/zip"
	"crypto/sha256"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/labstack/echo/v4"
)

type importedFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type skippedArchiveEntry struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type importedAgentPackage struct {
	Spec          *stdlib.AgentSpec
	Root          string
	Files         []importedFile
	Skipped       []skippedArchiveEntry
	ArchiveSHA256 string
}

type importPackageError struct {
	Status  int
	Message string
}

func (e *importPackageError) Error() string { return e.Message }

const (
	maxUploadSize    = 50 << 20  // 50 MB
	maxExtractedSize = 200 << 20 // 200 MB (zip bomb protection)
)

func (s *Server) handleUploadAgent(c echo.Context) error {
	tenant := getTenant(c)
	previewToken := c.FormValue("preview_token")
	if previewToken == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "preview_token is required; run import preview first"})
	}
	payload, payloadJSON, tokenErr := s.parseImportPreviewToken(previewToken)
	if tokenErr != nil {
		return c.JSON(tokenErr.Status, map[string]string{"error": tokenErr.Message})
	}

	file, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "file is required"})
	}
	pkg, cleanup, importErr := importAgentPackage(file)
	if importErr != nil {
		return c.JSON(importErr.Status, map[string]string{"error": importErr.Message})
	}
	defer cleanup()

	// Derive agent name from directory or use form field.
	name := c.FormValue("name")
	if name == "" {
		name = filepath.Base(file.Filename)
		name = name[:len(name)-len(filepath.Ext(name))]
	}

	model := c.FormValue("model")
	if model == "" {
		model = "deepseek-flash"
	}

	rec := &registry.AgentRecord{
		Name:  name,
		Model: model,
		Spec:  *pkg.Spec,
	}
	if tokenErr := validateImportPayload(payload, tenant, pkg.ArchiveSHA256, name, model); tokenErr != nil {
		return c.JSON(tokenErr.Status, map[string]string{"error": tokenErr.Message})
	}
	pool, err := s.importTokenPool()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	ctx := c.Request().Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if tokenErr := lockImportToken(ctx, tx, payload, payloadJSON, tenant); tokenErr != nil {
		return c.JSON(tokenErr.Status, map[string]string{"error": tokenErr.Message})
	}
	if err := s.Registry.PutTx(ctx, tx, tenant, rec); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if err := consumeImportToken(ctx, tx, payload.ID); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if err := tx.Commit(ctx); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusCreated, rec)
}

func importAgentPackage(file *multipart.FileHeader) (*importedAgentPackage, func(), *importPackageError) {
	if file.Size > maxUploadSize {
		return nil, nil, &importPackageError{http.StatusBadRequest, fmt.Sprintf("file too large: %d bytes (max %d)", file.Size, maxUploadSize)}
	}
	if !strings.HasSuffix(strings.ToLower(file.Filename), ".zip") {
		return nil, nil, &importPackageError{http.StatusBadRequest, "only .zip files are accepted"}
	}
	src, err := file.Open()
	if err != nil {
		return nil, nil, &importPackageError{http.StatusInternalServerError, "cannot open file"}
	}
	defer src.Close()
	tmpFile, err := os.CreateTemp("", "agent-upload-*.zip")
	if err != nil {
		return nil, nil, &importPackageError{http.StatusInternalServerError, "cannot create temp file"}
	}
	cleanupFile := func() { tmpFile.Close(); os.Remove(tmpFile.Name()) }
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmpFile, hash), io.LimitReader(src, maxUploadSize+1))
	if err != nil {
		cleanupFile()
		return nil, nil, &importPackageError{http.StatusInternalServerError, "cannot write temp file"}
	}
	if written > maxUploadSize {
		cleanupFile()
		return nil, nil, &importPackageError{http.StatusBadRequest, fmt.Sprintf("file too large: %d bytes (max %d)", written, maxUploadSize)}
	}
	if err := tmpFile.Close(); err != nil {
		cleanupFile()
		return nil, nil, &importPackageError{http.StatusInternalServerError, "cannot write temp file"}
	}
	tmpDir, err := os.MkdirTemp("", "agent-upload-*")
	if err != nil {
		cleanupFile()
		return nil, nil, &importPackageError{http.StatusInternalServerError, "cannot create temp dir"}
	}
	cleanup := func() { cleanupFile(); os.RemoveAll(tmpDir) }
	files, skipped, err := extractZipWithManifest(tmpFile.Name(), tmpDir)
	if err != nil {
		cleanup()
		return nil, nil, &importPackageError{http.StatusBadRequest, "invalid zip: " + err.Error()}
	}
	spec, err := stdlib.LoadFromDirectory(tmpDir)
	if err != nil {
		cleanup()
		return nil, nil, &importPackageError{http.StatusBadRequest, "invalid agent spec: " + err.Error()}
	}
	return &importedAgentPackage{Spec: spec, Root: tmpDir, Files: files, Skipped: skipped, ArchiveSHA256: fmt.Sprintf("%x", hash.Sum(nil))}, cleanup, nil
}

func extractZipWithManifest(zipPath, destDir string) ([]importedFile, []skippedArchiveEntry, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, nil, err
	}
	defer r.Close()

	var totalDeclared, totalExtracted int64
	var files []importedFile
	var skipped []skippedArchiveEntry
	seen := make(map[string]struct{})

	for _, f := range r.File {
		cleanName := filepath.Clean(filepath.FromSlash(f.Name))
		fpath := filepath.Join(destDir, cleanName)
		if cleanName == ".." || strings.HasPrefix(cleanName, ".."+string(os.PathSeparator)) || filepath.IsAbs(cleanName) || !filepath.HasPrefix(fpath, filepath.Clean(destDir)+string(os.PathSeparator)) {
			skipped = append(skipped, skippedArchiveEntry{f.Name, "path traversal"})
			continue
		}
		if _, ok := seen[cleanName]; ok {
			skipped = append(skipped, skippedArchiveEntry{f.Name, "duplicate path"})
			continue
		}
		seen[cleanName] = struct{}{}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(fpath, 0755); err != nil {
				return nil, nil, err
			}
			continue
		}
		files = append(files, importedFile{Path: filepath.ToSlash(cleanName), Size: int64(f.UncompressedSize64)})

		totalDeclared += int64(f.UncompressedSize64)
		if totalDeclared > maxExtractedSize {
			return nil, nil, fmt.Errorf("extracted size exceeds limit (%d bytes)", maxExtractedSize)
		}

		if err := os.MkdirAll(filepath.Dir(fpath), 0755); err != nil {
			return nil, nil, err
		}

		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return nil, nil, err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return nil, nil, err
		}

		remaining := int64(maxExtractedSize) - totalExtracted
		written, err := io.Copy(outFile, io.LimitReader(rc, remaining+1))
		rc.Close()
		outFile.Close()
		if err != nil {
			return nil, nil, err
		}
		if written > remaining {
			return nil, nil, fmt.Errorf("extracted size exceeds limit (%d bytes)", maxExtractedSize)
		}
		if written > int64(f.UncompressedSize64) {
			return nil, nil, fmt.Errorf("file %q exceeded declared size", f.Name)
		}
		totalExtracted += written
	}
	return files, skipped, nil
}
