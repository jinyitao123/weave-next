package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/app/api"
)

const checkSecretKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func checkEnvironment(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DATABASE_URL", "postgres://fixture.invalid/weave")
	t.Setenv("JWT_SECRET", "check-jwt-secret")
	t.Setenv("WEAVE_LOGIN_KEY", "check-login-key")
	t.Setenv("WEAVE_DEV_MODE", "")
	t.Setenv("WEAVE_WORKSPACES_ROOT", dir)
	t.Setenv("WEAVE_SECRET_KEY", "")
	t.Setenv("WEAVE_SECRET_KEY_FILE", "")
	t.Setenv("WEAVE_ORGANIZATION_CONNECTION_FILE", "")
	t.Setenv("WEAVE_TEAMRUN_DRAIN_TIMEOUT", "")
	t.Setenv("WEAVE_HEALTH_SLOW_RUN_OVERRIDES", "")
	return dir
}

func runCheck(t *testing.T) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	handled, code := dispatchEarlyCommand([]string{"config-check"}, &stdout, &stderr)
	if !handled {
		t.Fatal("config-check was not handled before server startup")
	}
	output := stdout.String() + stderr.String()
	for _, secret := range []string{checkSecretKey, "check-jwt-secret", "check-login-key"} {
		if strings.Contains(output, secret) {
			t.Fatalf("config-check printed a secret: %s", output)
		}
	}
	return code, output
}

// The base deployment needs no optional file; each optional file is checked
// only once enabled, and a broken one is named with its cause (#102).
func TestConfigCheckChecksOnlyWhatIsEnabled(t *testing.T) {
	dir := checkEnvironment(t)
	t.Setenv("WEAVE_SECRET_KEY", checkSecretKey)
	code, output := runCheck(t)
	if code != 0 || !strings.Contains(output, "off  organization connection") || !strings.Contains(output, "read from WEAVE_SECRET_KEY") {
		t.Fatalf("base deployment: code %d\n%s", code, output)
	}

	keyFile := filepath.Join(dir, "secret-key")
	if err := os.WriteFile(keyFile, []byte(checkSecretKey+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEAVE_SECRET_KEY_FILE", keyFile)
	if code, output := runCheck(t); code != 1 || !strings.Contains(output, "ambiguous") {
		t.Fatalf("key in both places: code %d\n%s", code, output)
	}
	t.Setenv("WEAVE_SECRET_KEY", "")
	connection := filepath.Join(dir, "organization-connection.json")
	t.Setenv("WEAVE_ORGANIZATION_CONNECTION_FILE", connection)
	if code, output := runCheck(t); code != 1 || !strings.Contains(output, "does not exist") || !strings.Contains(output, "read from WEAVE_SECRET_KEY_FILE") {
		t.Fatalf("missing connection file: code %d\n%s", code, output)
	}
	if err := os.Mkdir(connection, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, output := runCheck(t); code != 1 || !strings.Contains(output, "is a directory") {
		t.Fatalf("connection path created as a directory: code %d\n%s", code, output)
	}
	if err := os.Remove(connection); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(connection, []byte(`{"version":1,"forgeOrigin":"https://forge.example.test/app","weaveOrigin":"https://weave.example.test"}`), 0o444); err != nil {
		t.Fatal(err)
	}
	if code, output := runCheck(t); code != 1 || !strings.Contains(output, "not a valid organization connection") {
		t.Fatalf("origin with a path: code %d\n%s", code, output)
	}
	if err := os.Remove(connection); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(connection, []byte(`{"version":1,"forgeOrigin":"https://forge.example.test","weaveOrigin":"https://weave.example.test"}`), 0o444); err != nil {
		t.Fatal(err)
	}
	if code, output := runCheck(t); code != 0 || !strings.Contains(output, "ok   organization connection") {
		t.Fatalf("valid files: code %d\n%s", code, output)
	}
}

func TestConfigCheckReportsAMissingLoginKey(t *testing.T) {
	checkEnvironment(t)
	t.Setenv("WEAVE_SECRET_KEY", checkSecretKey)
	t.Setenv("WEAVE_LOGIN_KEY", "")
	if code, output := runCheck(t); code != 1 || !strings.Contains(output, "fail server configuration") || !strings.Contains(output, "WEAVE_LOGIN_KEY is required") {
		t.Fatalf("missing login key: code %d\n%s", code, output)
	}
}

// The example shipped next to the Compose overrides must itself be valid.
func TestShippedOrganizationConnectionExampleIsValid(t *testing.T) {
	if err := api.CheckOrganizationConnectionFile(filepath.Join("..", "..", "deploy", "compose", "organization-connection.example.json")); err != nil {
		t.Fatal(err)
	}
}

// A drain timeout the server would ignore is reported before startup (#107).
func TestConfigCheckReportsTimingSettings(t *testing.T) {
	checkEnvironment(t)
	t.Setenv("WEAVE_SECRET_KEY", checkSecretKey)
	if code, output := runCheck(t); code != 0 || !strings.Contains(output, "ok   shutdown drain: WEAVE_TEAMRUN_DRAIN_TIMEOUT unset") || !strings.Contains(output, "ok   slow-run alert: 600s") {
		t.Fatalf("defaults: code %d\n%s", code, output)
	}
	t.Setenv("WEAVE_TEAMRUN_DRAIN_TIMEOUT", "30s")
	if code, output := runCheck(t); code != 0 || !strings.Contains(output, "waits up to 30s") {
		t.Fatalf("drain 30s: code %d\n%s", code, output)
	}
	t.Setenv("WEAVE_TEAMRUN_DRAIN_TIMEOUT", "30")
	if code, output := runCheck(t); code != 1 || !strings.Contains(output, "fail shutdown drain") {
		t.Fatalf("drain without a unit: code %d\n%s", code, output)
	}
}

// Per-workflow slow-run thresholds are listed, and a malformed list fails the
// check as it would fail the server's start (#107).
func TestConfigCheckListsSlowRunOverrides(t *testing.T) {
	checkEnvironment(t)
	t.Setenv("WEAVE_SECRET_KEY", checkSecretKey)
	t.Setenv("WEAVE_HEALTH_SLOW_RUN_OVERRIDES", "report=30m,chat=2m")
	if code, output := runCheck(t); code != 0 || !strings.Contains(output, "ok   slow-run alert overrides: chat 2m0s, report 30m0s") {
		t.Fatalf("overrides: code %d\n%s", code, output)
	}
	t.Setenv("WEAVE_HEALTH_SLOW_RUN_OVERRIDES", "chat=120")
	if code, output := runCheck(t); code != 1 || !strings.Contains(output, "WEAVE_HEALTH_SLOW_RUN_OVERRIDES must list") {
		t.Fatalf("malformed overrides: code %d\n%s", code, output)
	}
}
