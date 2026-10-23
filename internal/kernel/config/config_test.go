package config

import (
	"strings"
	"testing"
)

func TestLoadWorkflowHealthDefaultsAndBounds(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("WEAVE_LOGIN_KEY", "deployment-login-key")
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

func TestLoadBooleanDefaultsAndOverrides(t *testing.T) {
	for _, setting := range []struct {
		name         string
		defaultValue bool
		value        func(*Config) bool
	}{
		{"WEAVE_LOCAL_RUNTIME_ENABLED", false, func(c *Config) bool { return c.LocalRuntimeEnabled }},
	} {
		for _, input := range []string{"", "true", "false", "sometimes"} {
			t.Run(setting.name+"/"+input, func(t *testing.T) {
				t.Setenv("DATABASE_URL", "postgres://example")
				t.Setenv("JWT_SECRET", "secret")
				t.Setenv("WEAVE_LOGIN_KEY", "deployment-login-key")
				t.Setenv("WEAVE_WORKSPACES_ROOT", t.TempDir())
				t.Setenv(setting.name, input)
				cfg, err := Load()
				if input == "sometimes" {
					if err == nil {
						t.Fatal("invalid boolean configuration was accepted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				want := setting.defaultValue
				if input != "" {
					want = input == "true"
				}
				if setting.value(cfg) != want {
					t.Fatalf("%s=%q: got %t, want %t", setting.name, input, setting.value(cfg), want)
				}
			})
		}
	}
}

// A deployment never starts with the well-known login key by accident: only
// development mode falls back to it, and errors never echo a key.
func TestLoadRequiresDeploymentLoginKey(t *testing.T) {
	base := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://example")
		t.Setenv("JWT_SECRET", "secret")
		t.Setenv("WEAVE_WORKSPACES_ROOT", t.TempDir())
		t.Setenv("WEAVE_DEV_MODE", "")
	}
	for name, key := range map[string]string{"unset": "", "blank": "   ", "development default": "admin"} {
		t.Run(name, func(t *testing.T) {
			base(t)
			t.Setenv("WEAVE_LOGIN_KEY", key)
			cfg, err := Load()
			if err == nil {
				t.Fatalf("started with login key %q: %+v", key, cfg)
			}
			if strings.Contains(err.Error(), `"admin"`) || (strings.TrimSpace(key) != "" && strings.Contains(err.Error(), key)) {
				t.Fatalf("error echoes a key: %v", err)
			}
		})
	}
	t.Run("explicit", func(t *testing.T) {
		base(t)
		t.Setenv("WEAVE_LOGIN_KEY", "  set-at-deployment  ")
		cfg, err := Load()
		if err != nil || cfg.LoginKey != "set-at-deployment" || cfg.LoginKeyDefault {
			t.Fatalf("explicit key = %+v, %v", cfg, err)
		}
	})
	t.Run("development mode", func(t *testing.T) {
		base(t)
		t.Setenv("WEAVE_DEV_MODE", "true")
		t.Setenv("WEAVE_LOGIN_KEY", "")
		cfg, err := Load()
		if err != nil || cfg.LoginKey != "admin" || !cfg.LoginKeyDefault {
			t.Fatalf("development default = %+v, %v", cfg, err)
		}
	})
}
