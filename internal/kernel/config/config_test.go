package config

import "testing"

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

func TestLoadDisableLocalLogin(t *testing.T) {
	base := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://example")
		t.Setenv("JWT_SECRET", "secret")
		t.Setenv("WEAVE_WORKSPACES_ROOT", t.TempDir())
	}
	t.Run("defaults to closed", func(t *testing.T) {
		base(t)
		t.Setenv("WEAVE_DISABLE_LOCAL_LOGIN", "")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.DisableLocalLogin {
			t.Fatal("local login is open by default; the desktop uses the Forge identity exchange")
		}
	})
	t.Run("explicit false re-enables", func(t *testing.T) {
		base(t)
		t.Setenv("WEAVE_DISABLE_LOCAL_LOGIN", "false")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DisableLocalLogin {
			t.Fatal("explicit false did not re-enable local login")
		}
	})
	t.Run("rejects an invalid value", func(t *testing.T) {
		base(t)
		t.Setenv("WEAVE_DISABLE_LOCAL_LOGIN", "sometimes")
		if _, err := Load(); err == nil {
			t.Fatal("Load() with an invalid WEAVE_DISABLE_LOCAL_LOGIN error = nil")
		}
	})
}
