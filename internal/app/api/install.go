package api

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	defaultRuntimeDistDir   = "dist/runtime"
	defaultCuaDriverDistDir = "dist/cua-driver"
	// downloadDigestHeader carries the SHA-256 the install scripts check
	// before installing anything.
	downloadDigestHeader   = "X-Weave-SHA256"
	cuaDriverVersionHeader = "X-Weave-Cua-Driver-Version"
)

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
	return serveDistFile(c, distDir("WEAVE_RUNTIME_DIST_DIR", defaultRuntimeDistDir), filename, "runtime binary not found")
}

// handleDownloadCuaDriver serves the cua-driver release this deployment pins
// (MIT, Cua AI, Inc.). The image build places the release archives, their
// VERSION and the upstream LICENSE in the directory; "license/notice" serves
// the latter for the install scripts to keep beside the driver.
func (s *Server) handleDownloadCuaDriver(c echo.Context) error {
	dir := distDir("WEAVE_CUA_DRIVER_DIST_DIR", defaultCuaDriverDistDir)
	if version, err := os.ReadFile(filepath.Join(dir, "VERSION")); err == nil {
		c.Response().Header().Set(cuaDriverVersionHeader, strings.TrimSpace(string(version)))
	}
	if c.Param("os") == "license" && c.Param("arch") == "notice" {
		return serveDistFile(c, dir, "LICENSE", "cua-driver license not found")
	}
	filename := cuaDriverArchive(c.Param("os"), c.Param("arch"))
	if filename == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "unsupported platform"})
	}
	return serveDistFile(c, dir, filename, "cua-driver archive not found")
}

// cuaDriverArchive names the archive for a platform. macOS uses the
// universal archive, which carries CuaDriver.app: system permissions are
// granted to the app bundle.
func cuaDriverArchive(osName, arch string) string {
	switch osName + "/" + arch {
	case "darwin/amd64", "darwin/arm64":
		return "cua-driver-darwin-universal.tar.gz"
	case "windows/amd64":
		return "cua-driver-windows-x86_64.zip"
	case "windows/arm64":
		return "cua-driver-windows-arm64.zip"
	}
	return ""
}

func distDir(env, fallback string) string {
	if dir := os.Getenv(env); dir != "" {
		return dir
	}
	return fallback
}

func serveDistFile(c echo.Context, dir, filename, missing string) error {
	path := filepath.Join(dir, filename)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return c.JSON(http.StatusNotFound, map[string]string{"error": missing})
	}
	digest, err := distDigests.of(path, info)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "download unavailable"})
	}
	c.Response().Header().Set(downloadDigestHeader, digest)
	c.Response().Header().Set(echo.HeaderContentType, "application/octet-stream")
	return c.Attachment(path, filename)
}

// distDigests remembers each file's SHA-256 until the file changes, so a large
// archive is hashed once rather than on every download.
var distDigests = &fileDigests{entries: map[string]fileDigest{}}

type fileDigest struct {
	size     int64
	modified time.Time
	sum      string
}

type fileDigests struct {
	mu      sync.Mutex
	entries map[string]fileDigest
}

func (d *fileDigests) of(path string, info os.FileInfo) (string, error) {
	d.mu.Lock()
	entry, known := d.entries[path]
	d.mu.Unlock()
	if known && entry.size == info.Size() && entry.modified.Equal(info.ModTime()) {
		return entry.sum, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	d.mu.Lock()
	d.entries[path] = fileDigest{size: info.Size(), modified: info.ModTime(), sum: sum}
	d.mu.Unlock()
	return sum, nil
}

func supportedRuntimePlatform(osName, arch string) bool {
	switch osName + "/" + arch {
	case "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64":
		return true
	}
	return false
}
