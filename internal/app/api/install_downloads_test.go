package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestCuaDriverDownloadsCarryTheirChecksumAndVersion(t *testing.T) {
	dir := t.TempDir()
	archive := []byte("universal archive")
	for name, content := range map[string][]byte{"cua-driver-darwin-universal.tar.gz": archive, "VERSION": []byte("0.35.2\n"), "LICENSE": []byte("MIT License")} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("WEAVE_CUA_DRIVER_DIST_DIR", dir)
	e := echo.New()
	s := &Server{}
	e.GET("/v1/downloads/cua-driver/:os/:arch", s.handleDownloadCuaDriver)
	get := func(path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder
	}

	sum := sha256.Sum256(archive)
	for _, arch := range []string{"arm64", "amd64"} {
		got := get("/v1/downloads/cua-driver/darwin/" + arch)
		if got.Code != http.StatusOK || got.Body.String() != string(archive) {
			t.Fatalf("darwin/%s: %d %q", arch, got.Code, got.Body.String())
		}
		if got.Header().Get(downloadDigestHeader) != hex.EncodeToString(sum[:]) || got.Header().Get(cuaDriverVersionHeader) != "0.35.2" {
			t.Fatalf("darwin/%s headers: %v", arch, got.Header())
		}
	}
	if got := get("/v1/downloads/cua-driver/license/notice"); got.Code != http.StatusOK || got.Body.String() != "MIT License" {
		t.Fatalf("license: %d %q", got.Code, got.Body.String())
	}
	if got := get("/v1/downloads/cua-driver/windows/amd64"); got.Code != http.StatusNotFound {
		t.Fatalf("missing windows archive: %d", got.Code)
	}
	if got := get("/v1/downloads/cua-driver/linux/amd64"); got.Code != http.StatusBadRequest {
		t.Fatalf("linux: %d", got.Code)
	}
}

func TestRuntimeDownloadCarriesItsChecksum(t *testing.T) {
	dir := t.TempDir()
	binary := []byte("runtime binary")
	if err := os.WriteFile(filepath.Join(dir, "weave-runtime-windows-amd64.exe"), binary, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEAVE_RUNTIME_DIST_DIR", dir)
	e := echo.New()
	e.GET("/v1/downloads/runtime/:os/:arch", (&Server{}).handleDownloadRuntime)
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/downloads/runtime/windows/amd64", nil))
	sum := sha256.Sum256(binary)
	if recorder.Code != http.StatusOK || recorder.Header().Get(downloadDigestHeader) != hex.EncodeToString(sum[:]) {
		t.Fatalf("runtime download: %d %v", recorder.Code, recorder.Header())
	}
}
