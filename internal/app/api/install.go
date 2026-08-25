package api

import (
	"embed"
	"net/http"
	"os"
	"path/filepath"

	"github.com/labstack/echo/v4"
)

const defaultRuntimeDistDir = "dist/runtime"

//go:embed install_assets/install.sh install_assets/install.ps1
var installAssets embed.FS

func (s *Server) handleInstallScript(c echo.Context) error {
	asset := "install_assets/" + filepath.Base(c.Path())
	content, err := installAssets.ReadFile(asset)
	if err != nil {
		return err
	}
	return c.Blob(http.StatusOK, "text/plain; charset=utf-8", content)
}

func (s *Server) handleDownloadRuntime(c echo.Context) error {
	osName := c.Param("os")
	arch := c.Param("arch")
	if !supportedRuntimePlatform(osName, arch) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "unsupported platform"})
	}

	filename := "weave-runtime-" + osName + "-" + arch
	if osName == "windows" {
		filename += ".exe"
	}
	distDir := os.Getenv("WEAVE_RUNTIME_DIST_DIR")
	if distDir == "" {
		distDir = defaultRuntimeDistDir
	}
	path := filepath.Join(distDir, filename)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "runtime binary not found"})
	}

	c.Response().Header().Set(echo.HeaderContentType, "application/octet-stream")
	return c.Attachment(path, filename)
}

func supportedRuntimePlatform(osName, arch string) bool {
	switch osName + "/" + arch {
	case "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64":
		return true
	}
	return false
}
