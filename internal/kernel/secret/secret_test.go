package secret

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyFromEnv(t *testing.T) {
	want := []byte("0123456789abcdef0123456789abcdef")
	hexKey := hex.EncodeToString(want)
	base64Key := base64.StdEncoding.EncodeToString(want)

	tests := []struct {
		name       string
		envValue   string
		fileValue  string
		wantError  string
		useMissing bool
		useDir     bool
	}{
		{name: "environment hex", envValue: hexKey},
		{name: "environment base64", envValue: base64Key},
		{name: "file hex", fileValue: hexKey + "\n"},
		{name: "file base64", fileValue: "  " + base64Key + "\n"},
		{name: "both sources", envValue: hexKey, fileValue: hexKey, wantError: "ambiguous"},
		{name: "missing file", useMissing: true, wantError: keyFileEnv},
		{name: "directory", useDir: true, wantError: "regular file"},
		{name: "malformed", envValue: "not-a-key", wantError: "64-character hex or base64"},
		{name: "malformed file", fileValue: "not-a-key", wantError: keyFileEnv},
		{name: "neither source", wantError: "not configured"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(keyEnv, tc.envValue)
			t.Setenv(keyFileEnv, "")
			if tc.fileValue != "" {
				path := filepath.Join(t.TempDir(), "weave-secret")
				if err := os.WriteFile(path, []byte(tc.fileValue), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv(keyFileEnv, path)
			}
			if tc.useMissing {
				t.Setenv(keyFileEnv, filepath.Join(t.TempDir(), "missing"))
			}
			if tc.useDir {
				t.Setenv(keyFileEnv, t.TempDir())
			}

			got, err := KeyFromEnv()
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("KeyFromEnv() error = %v, want containing %q", err, tc.wantError)
				}
				if strings.Contains(err.Error(), hexKey) || strings.Contains(err.Error(), base64Key) {
					t.Fatalf("KeyFromEnv() leaked secret in error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("KeyFromEnv() = %x, want %x", got, want)
			}
		})
	}
}
