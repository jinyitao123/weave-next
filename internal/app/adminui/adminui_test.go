package adminui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func serve(t *testing.T, handler http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
	return recorder
}

func TestConsoleServesBuildAndClientRoutes(t *testing.T) {
	handler := NewHandler(fstest.MapFS{
		"index.html":      {Data: []byte("<!doctype html><title>Weave</title>")},
		"assets/app-1.js": {Data: []byte("console.log(1)")},
		"favicon.svg":     {Data: []byte("<svg/>")},
	}, Options{ConnectSources: []string{"https://forge.example"}})

	root := serve(t, handler, http.MethodGet, "/admin/")
	if root.Code != http.StatusOK || !strings.Contains(root.Body.String(), "<title>Weave</title>") {
		t.Fatalf("root = %d %q", root.Code, root.Body.String())
	}
	if policy := root.Header().Get("Content-Security-Policy"); !strings.Contains(policy, "connect-src 'self' https://forge.example") || !strings.Contains(policy, "frame-ancestors 'none'") {
		t.Fatalf("policy = %q", policy)
	}
	route := serve(t, handler, http.MethodGet, "/admin/tasks/readable-task")
	if route.Code != http.StatusOK || route.Header().Get("Cache-Control") != "no-cache" || !strings.Contains(route.Body.String(), "<title>Weave</title>") {
		t.Fatalf("client route = %d %q", route.Code, route.Body.String())
	}
	asset := serve(t, handler, http.MethodGet, "/admin/assets/app-1.js")
	if asset.Code != http.StatusOK || !strings.Contains(asset.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset = %d %q", asset.Code, asset.Header().Get("Cache-Control"))
	}
	if missing := serve(t, handler, http.MethodGet, "/admin/assets/missing.js"); missing.Code != http.StatusNotFound {
		t.Fatalf("missing asset = %d", missing.Code)
	}
	if redirect := serve(t, handler, http.MethodGet, "/admin"); redirect.Code != http.StatusMovedPermanently || redirect.Header().Get("Location") != "/admin/" {
		t.Fatalf("bare prefix = %d %q", redirect.Code, redirect.Header().Get("Location"))
	}
	if post := serve(t, handler, http.MethodPost, "/admin/"); post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post = %d", post.Code)
	}
	if other := serve(t, handler, http.MethodGet, "/administrator"); other.Code != http.StatusNotFound {
		t.Fatalf("foreign prefix = %d", other.Code)
	}
	if escape := serve(t, handler, http.MethodGet, "/admin/../../go.mod"); escape.Code == http.StatusOK && !strings.Contains(escape.Body.String(), "<title>Weave</title>") {
		t.Fatalf("path escape served %q", escape.Body.String())
	}
}

func TestConsoleReportsMissingBuild(t *testing.T) {
	handler := NewHandler(fstest.MapFS{".gitkeep": {Data: []byte{}}}, Options{})
	recorder := serve(t, handler, http.MethodGet, "/admin/")
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing build = %d %q", recorder.Code, recorder.Header().Get("Cache-Control"))
	}
}

func TestEmbeddedHandlerAlwaysBuilds(t *testing.T) {
	// The committed placeholder keeps go:embed valid before the console is built.
	if Handler(Options{}) == nil {
		t.Fatal("handler is nil")
	}
}
