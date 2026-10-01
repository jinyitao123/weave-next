package config

import "testing"

func TestLoadTemplateAuthorizationDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("WEAVE_WORKSPACES_ROOT", t.TempDir())
	for _, key := range []string{
		"WEAVE_TEMPLATE_AUTO_MAX_COST_USD", "WEAVE_TEMPLATE_DAILY_BUDGET_USD",
		"WEAVE_TEMPLATE_MONTHLY_BUDGET_USD", "WEAVE_TEMPLATE_MAX_CONCURRENT",
	} {
		t.Setenv(key, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.TemplateAutoMaxCostUSD != 5 || cfg.TemplateDailyBudgetUSD != 25 ||
		cfg.TemplateMonthlyBudgetUSD != 250 || cfg.TemplateMaxConcurrent != 2 {
		t.Fatalf("template authorization defaults = %#v", cfg)
	}
}

func TestLoadRejectsInvalidTemplateAuthorizationLimits(t *testing.T) {
	cases := []struct {
		name, key, value string
	}{
		{name: "zero threshold", key: "WEAVE_TEMPLATE_AUTO_MAX_COST_USD", value: "0"},
		{name: "invalid daily", key: "WEAVE_TEMPLATE_DAILY_BUDGET_USD", value: "invalid"},
		{name: "negative monthly", key: "WEAVE_TEMPLATE_MONTHLY_BUDGET_USD", value: "-1"},
		{name: "zero concurrency", key: "WEAVE_TEMPLATE_MAX_CONCURRENT", value: "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example")
			t.Setenv("JWT_SECRET", "secret")
			t.Setenv("WEAVE_WORKSPACES_ROOT", t.TempDir())
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("Load() error = nil")
			}
		})
	}
}

func TestLoadMetaTeamEnabled(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("WEAVE_WORKSPACES_ROOT", t.TempDir())
	t.Setenv("WEAVE_METATEAM_ENABLED", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MetaTeamEnabled {
		t.Fatal("MetaTeamEnabled default = true, want false (retired)")
	}
	t.Setenv("WEAVE_METATEAM_ENABLED", "true")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MetaTeamEnabled {
		t.Fatal("MetaTeamEnabled = false, want true when explicitly enabled")
	}
	t.Setenv("WEAVE_METATEAM_ENABLED", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("Load() invalid WEAVE_METATEAM_ENABLED error = nil")
	}
}

func TestLoadWorkflowHealthDefaultsAndBounds(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("WEAVE_WORKSPACES_ROOT", t.TempDir())
	for _, key := range []string{
		"WEAVE_HEALTH_WINDOW_SIZE", "WEAVE_HEALTH_MIN_SAMPLES", "WEAVE_HEALTH_WARNING_FAILURE_RATE",
		"WEAVE_HEALTH_WARNING_SLOW_RATE", "WEAVE_HEALTH_SLOW_RUN_SECONDS",
	} {
		t.Setenv(key, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HealthWindowSize != 20 || cfg.HealthMinSamples != 3 || cfg.HealthWarningFailureRate != 0.25 ||
		cfg.HealthWarningSlowRate != 0.5 || cfg.HealthSlowRunSeconds != 600 {
		t.Fatalf("workflow health defaults = %#v", cfg)
	}
	t.Setenv("WEAVE_HEALTH_MIN_SAMPLES", "21")
	if _, err := Load(); err == nil {
		t.Fatal("min samples greater than window accepted")
	}
	t.Setenv("WEAVE_HEALTH_MIN_SAMPLES", "3")
	t.Setenv("WEAVE_HEALTH_WARNING_FAILURE_RATE", "1.1")
	if _, err := Load(); err == nil {
		t.Fatal("failure warning rate greater than one accepted")
	}
}

func TestLoadRetiredCapabilityFlags(t *testing.T) {
	base := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://example")
		t.Setenv("JWT_SECRET", "secret")
		t.Setenv("WEAVE_WORKSPACES_ROOT", t.TempDir())
	}
	t.Run("defaults retire legacy APIs and local login", func(t *testing.T) {
		base(t)
		t.Setenv("WEAVE_RETIRE_LEGACY_PLATFORM_APIS", "")
		t.Setenv("WEAVE_DISABLE_LOCAL_LOGIN", "")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.RetireLegacyPlatformAPIs || !cfg.DisableLocalLogin {
			t.Fatalf("defaults = retire %v, disable login %v; want both true", cfg.RetireLegacyPlatformAPIs, cfg.DisableLocalLogin)
		}
	})
	t.Run("explicit false re-enables", func(t *testing.T) {
		base(t)
		t.Setenv("WEAVE_RETIRE_LEGACY_PLATFORM_APIS", "false")
		t.Setenv("WEAVE_DISABLE_LOCAL_LOGIN", "false")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.RetireLegacyPlatformAPIs || cfg.DisableLocalLogin {
			t.Fatalf("explicit false = retire %v, disable login %v; want both false", cfg.RetireLegacyPlatformAPIs, cfg.DisableLocalLogin)
		}
	})
	for _, key := range []string{"WEAVE_RETIRE_LEGACY_PLATFORM_APIS", "WEAVE_DISABLE_LOCAL_LOGIN"} {
		t.Run("rejects invalid "+key, func(t *testing.T) {
			base(t)
			t.Setenv(key, "sometimes")
			if _, err := Load(); err == nil {
				t.Fatalf("Load() with invalid %s error = nil", key)
			}
		})
	}
}
