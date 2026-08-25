package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds all platform configuration, loaded from environment variables.
type Config struct {
	Port        string // HTTP listen port, default "8080"
	DatabaseURL string // PostgreSQL connection string
	JWTSecret   string // HS256 signing key
	LogLevel    string // "debug", "info", "warn", "error"

	// Auth settings.
	DevMode   bool   // WEAVE_DEV_MODE — enables /v1/auth/token (no-credential token endpoint)
	AdminUser string // WEAVE_ADMIN_USER — seed admin username on startup
	AdminPass string // WEAVE_ADMIN_PASS — seed admin password on startup

	// CORS settings.
	CORSOrigins string // CORS_ORIGINS — comma-separated allowed origins; "*" for dev (default when DevMode)

	// MCP boundary settings.
	MCPBoundaryBase string // WEAVE_MCP_BOUNDARY_BASE, default http://127.0.0.1:<Port>

	// External engine execution settings.
	WorkspacesRoot string // WEAVE_WORKSPACES_ROOT, default ~/.weave/workspaces
	OneAPIBase     string // OPENAI_BASE_URL
	OneAPIKey      string // OPENAI_API_KEY

	// Embedder settings (optional — enables memory features).
	EmbedderURL       string // EMBEDDER_URL, e.g. "https://api.openai.com"
	EmbedderKey       string // EMBEDDER_API_KEY
	EmbedderModel     string // EMBEDDER_MODEL, default "text-embedding-3-small"
	EmbedderDimension int    // EMBEDDER_DIMENSION, default 1536
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	dim, _ := strconv.Atoi(envOr("EMBEDDER_DIMENSION", "1536"))
	port := envOr("PORT", "8080")
	workspacesRoot := os.Getenv("WEAVE_WORKSPACES_ROOT")
	if workspacesRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home directory: %w", err)
		}
		workspacesRoot = filepath.Join(home, ".weave", "workspaces")
	}
	devMode := os.Getenv("WEAVE_DEV_MODE") == "true" ||
		os.Getenv("JWT_SECRET") == "dev-secret-change-in-prod"
	corsOrigins := os.Getenv("CORS_ORIGINS")
	if corsOrigins == "" {
		if devMode {
			corsOrigins = "*"
		} else {
			corsOrigins = "" // empty means no CORS (same-origin only)
		}
	}

	cfg := &Config{
		Port:              port,
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		JWTSecret:         os.Getenv("JWT_SECRET"),
		LogLevel:          envOr("LOG_LEVEL", "info"),
		DevMode:           devMode,
		AdminUser:         os.Getenv("WEAVE_ADMIN_USER"),
		AdminPass:         os.Getenv("WEAVE_ADMIN_PASS"),
		CORSOrigins:       corsOrigins,
		MCPBoundaryBase:   envOr("WEAVE_MCP_BOUNDARY_BASE", "http://127.0.0.1:"+port),
		WorkspacesRoot:    workspacesRoot,
		OneAPIBase:        os.Getenv("OPENAI_BASE_URL"),
		OneAPIKey:         os.Getenv("OPENAI_API_KEY"),
		EmbedderURL:       os.Getenv("EMBEDDER_URL"),
		EmbedderKey:       os.Getenv("EMBEDDER_API_KEY"),
		EmbedderModel:     envOr("EMBEDDER_MODEL", "text-embedding-3-small"),
		EmbedderDimension: dim,
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("JWT_SECRET is required")
	}

	return cfg, nil
}

// ResolveEngineCLIPath resolves an external engine executable. An explicit
// WEAVE_ENGINE_<ENGINE>_PATH override wins, followed by PATH lookup. Returning
// the bare engine name preserves exec's normal not-found error at run time.
func ResolveEngineCLIPath(engine string) string {
	if path := os.Getenv("WEAVE_ENGINE_" + strings.ToUpper(engine) + "_PATH"); path != "" {
		return path
	}
	if path, err := exec.LookPath(engine); err == nil {
		return path
	}
	return engine
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
