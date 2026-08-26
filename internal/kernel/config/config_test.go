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
	if !cfg.MetaTeamEnabled {
		t.Fatal("MetaTeamEnabled default = false, want true")
	}
	t.Setenv("WEAVE_METATEAM_ENABLED", "false")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MetaTeamEnabled {
		t.Fatal("MetaTeamEnabled = true, want false")
	}
	t.Setenv("WEAVE_METATEAM_ENABLED", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("Load() invalid WEAVE_METATEAM_ENABLED error = nil")
	}
}
