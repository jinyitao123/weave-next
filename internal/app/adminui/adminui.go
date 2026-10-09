// Package adminui serves the compiled Weave admin console from the server
// binary. The console source lives in web/admin; its build output is copied
// into dist before the Go build. A binary built without it still starts and
// answers console requests with an explicit "not built" response.
package adminui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Prefix is the URL path the console is served under.
const Prefix = "/admin"

//go:embed all:dist
var embedded embed.FS

// Options carries the browser policy that depends on deployment.
type Options struct {
	// ConnectSources are extra origins the console may call from the browser,
	// such as the Forge sign-in origin. Same-origin Weave calls are always allowed.
	ConnectSources []string
}

// Handler serves the embedded console build.
func Handler(options Options) http.Handler {
	files, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic("adminui: embedded dist directory is missing")
	}
	return NewHandler(files, options)
}

// NewHandler serves a console build from files. Paths without a file extension
// fall back to index.html so client routes can be opened directly.
func NewHandler(files fs.FS, options Options) http.Handler {
	policy := contentSecurityPolicy(options.ConnectSources)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, Prefix)
		if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) {
			http.NotFound(w, r)
			return
		}
		if rest == "" {
			http.Redirect(w, r, Prefix+"/", http.StatusMovedPermanently)
			return
		}
		header := w.Header()
		header.Set("Content-Security-Policy", policy)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "same-origin")
		header.Set("X-Frame-Options", "DENY")

		name := strings.TrimPrefix(path.Clean(rest), "/")
		if name != "" && name != "." && isFile(files, name) {
			if strings.HasPrefix(name, "assets/") {
				header.Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				header.Set("Cache-Control", "no-cache")
			}
			http.ServeFileFS(w, r, files, name)
			return
		}
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		if !isFile(files, "index.html") {
			header.Set("Cache-Control", "no-store")
			http.Error(w, "Weave 管理端未随本次构建打包", http.StatusServiceUnavailable)
			return
		}
		header.Set("Cache-Control", "no-cache")
		// Serve the shell by content rather than by name so ServeFileFS does not
		// redirect a client route such as /admin/tasks to its directory form.
		index, err := fs.ReadFile(files, "index.html")
		if err != nil {
			http.Error(w, "Weave 管理端读取失败", http.StatusInternalServerError)
			return
		}
		header.Set("Content-Type", "text/html; charset=utf-8")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(index)
	})
}

func isFile(files fs.FS, name string) bool {
	info, err := fs.Stat(files, name)
	return err == nil && !info.IsDir()
}

func contentSecurityPolicy(connectSources []string) string {
	connect := []string{"'self'"}
	for _, source := range connectSources {
		if source = strings.TrimSpace(source); source != "" {
			connect = append(connect, source)
		}
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src " + strings.Join(connect, " "),
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'self'",
	}, "; ")
}
