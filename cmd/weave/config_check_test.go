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
