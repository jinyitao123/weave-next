package config

import (
	"fmt"
	"math"
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

	// Team template automatic authorization limits.
	TemplateAutoMaxCostUSD   float64 // WEAVE_TEMPLATE_AUTO_MAX_COST_USD, default 5
	TemplateDailyBudgetUSD   float64 // WEAVE_TEMPLATE_DAILY_BUDGET_USD, default 25
	TemplateMonthlyBudgetUSD float64 // WEAVE_TEMPLATE_MONTHLY_BUDGET_USD, default 250
	TemplateMaxConcurrent    int     // WEAVE_TEMPLATE_MAX_CONCURRENT, default 2

	// Optional built-in meta-team conversation guide. Disabling it preserves
	// stored assets and history while freezing all new runs.
	MetaTeamEnabled bool // WEAVE_METATEAM_ENABLED, default true
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	dim, _ := strconv.Atoi(envOr("EMBEDDER_DIMENSION", "1536"))
	templateAutoMaxCost, err := positiveFloatEnv("WEAVE_TEMPLATE_AUTO_MAX_COST_USD", 5)
	if err != nil {
		return nil, err
	}
	templateDailyBudget, err := positiveFloatEnv("WEAVE_TEMPLATE_DAILY_BUDGET_USD", 25)
	if err != nil {
		return nil, err
	}
	templateMonthlyBudget, err := positiveFloatEnv("WEAVE_TEMPLATE_MONTHLY_BUDGET_USD", 250)
	if err != nil {
		return nil, err
	}
	templateMaxConcurrent, err := positiveIntEnv("WEAVE_TEMPLATE_MAX_CONCURRENT", 2)
	if err != nil {
		return nil, err
	}
	metaTeamEnabled, err := boolEnv("WEAVE_METATEAM_ENABLED", true)
	if err != nil {
		return nil, err
	}
	if templateDailyBudget < templateAutoMaxCost {
		return nil, fmt.Errorf("WEAVE_TEMPLATE_DAILY_BUDGET_USD must be at least WEAVE_TEMPLATE_AUTO_MAX_COST_USD")
	}
	if templateMonthlyBudget < templateDailyBudget {
		return nil, fmt.Errorf("WEAVE_TEMPLATE_MONTHLY_BUDGET_USD must be at least WEAVE_TEMPLATE_DAILY_BUDGET_USD")
	}
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
		Port:                     port,
		DatabaseURL:              os.Getenv("DATABASE_URL"),
		JWTSecret:                os.Getenv("JWT_SECRET"),
		LogLevel:                 envOr("LOG_LEVEL", "info"),
		DevMode:                  devMode,
		AdminUser:                os.Getenv("WEAVE_ADMIN_USER"),
		AdminPass:                os.Getenv("WEAVE_ADMIN_PASS"),
		CORSOrigins:              corsOrigins,
		MCPBoundaryBase:          envOr("WEAVE_MCP_BOUNDARY_BASE", "http://127.0.0.1:"+port),
		WorkspacesRoot:           workspacesRoot,
		OneAPIBase:               os.Getenv("OPENAI_BASE_URL"),
		OneAPIKey:                os.Getenv("OPENAI_API_KEY"),
		EmbedderURL:              os.Getenv("EMBEDDER_URL"),
		EmbedderKey:              os.Getenv("EMBEDDER_API_KEY"),
		EmbedderModel:            envOr("EMBEDDER_MODEL", "text-embedding-3-small"),
		EmbedderDimension:        dim,
		TemplateAutoMaxCostUSD:   templateAutoMaxCost,
		TemplateDailyBudgetUSD:   templateDailyBudget,
		TemplateMonthlyBudgetUSD: templateMonthlyBudget,
		TemplateMaxConcurrent:    templateMaxConcurrent,
		MetaTeamEnabled:          metaTeamEnabled,
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("JWT_SECRET is required")
	}

	return cfg, nil
}

func boolEnv(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return value, nil
}

func positiveFloatEnv(key string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("%s must be a positive number", key)
	}
	return value, nil
}

func positiveIntEnv(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return value, nil
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
