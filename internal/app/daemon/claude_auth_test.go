package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The decision fields Claude Code 2.1.285 reports on Windows for claude.ai
// subscription login.
const claudeAuthSubscriptionStatus = `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}`

const (
	claudeAuthFakeStatusEnv = "WEAVE_TEST_FAKE_CLAUDE_AUTH_STATUS"
	claudeAuthFakeModeEnv   = "WEAVE_TEST_FAKE_CLAUDE_AUTH_MODE"
)

// The test binary doubles as the claude executable for the process-level
// probes, so they run the same way on Windows and POSIX without a shell script.
// It prints the configured status, then exits 0, exits 1 ("fail") or hangs
// ("hang").
func init() {
	status, ok := os.LookupEnv(claudeAuthFakeStatusEnv)
	if !ok {
		return
	}
	if len(os.Args) != 3 || os.Args[1] != "auth" || os.Args[2] != "status" {
		fmt.Fprintf(os.Stderr, "fake claude: unexpected arguments %q\n", os.Args[1:])
		os.Exit(2)
	}
	fmt.Print(status)
	switch os.Getenv(claudeAuthFakeModeEnv) {
	case "fail":
		os.Exit(1)
	case "hang":
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func TestClaudeAuthStatusIsFirstPartyOAuth(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{name: "claude.ai subscription", output: claudeAuthSubscriptionStatus, want: true},
		{name: "oauth token", output: `{"loggedIn":true,"authMethod":"oauth_token","apiProvider":"firstParty"}`, want: true},
		{name: "indented CRLF output with extra fields", output: "{\r\n  \"loggedIn\": true,\r\n  \"authMethod\": \"claude.ai\",\r\n  \"apiProvider\": \"firstParty\",\r\n  \"email\": \"user@example.com\"\r\n}\r\n", want: true},
		{name: "api key", output: `{"loggedIn":true,"authMethod":"api_key","apiProvider":"firstParty"}`, want: false},
		{name: "weave mode name is not a claude method", output: `{"loggedIn":true,"authMethod":"oauth","apiProvider":"firstParty"}`, want: false},
		{name: "method compared exactly", output: `{"loggedIn":true,"authMethod":"Claude.ai","apiProvider":"firstParty"}`, want: false},
		{name: "missing method", output: `{"loggedIn":true,"apiProvider":"firstParty"}`, want: false},
		{name: "bedrock provider", output: `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"bedrock"}`, want: false},
		{name: "vertex provider", output: `{"loggedIn":true,"authMethod":"oauth_token","apiProvider":"vertex"}`, want: false},
		{name: "foundry provider", output: `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"foundry"}`, want: false},
		{name: "missing provider", output: `{"loggedIn":true,"authMethod":"claude.ai"}`, want: false},
		{name: "logged out", output: `{"loggedIn":false,"authMethod":"claude.ai","apiProvider":"firstParty"}`, want: false},
		{name: "loggedIn is not a boolean", output: `{"loggedIn":"true","authMethod":"claude.ai","apiProvider":"firstParty"}`, want: false},
		{name: "plain text", output: "Logged in using Claude.ai\n", want: false},
		{name: "empty output", output: "", want: false},
		{name: "json null", output: "null", want: false},
		{name: "truncated json", output: `{"loggedIn":true,"authMethod":"claude.ai"`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeAuthStatusIsFirstPartyOAuth([]byte(tt.output)); got != tt.want {
				t.Fatalf("claudeAuthStatusIsFirstPartyOAuth(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

func TestClaudeLoggedInWithFirstPartyOAuthProbesAuthStatus(t *testing.T) {
	cli := claudeAuthFakeCLI(t)
	tests := []struct {
		name   string
		status string
		mode   string
		want   bool
	}{
		{name: "claude.ai subscription", status: claudeAuthSubscriptionStatus, want: true},
		{name: "oauth token", status: `{"loggedIn":true,"authMethod":"oauth_token","apiProvider":"firstParty"}`, want: true},
		{name: "api key", status: `{"loggedIn":true,"authMethod":"api_key","apiProvider":"firstParty"}`, want: false},
		{name: "accepted status from failed command", status: claudeAuthSubscriptionStatus, mode: "fail", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(claudeAuthFakeStatusEnv, tt.status)
			t.Setenv(claudeAuthFakeModeEnv, tt.mode)
			if got := claudeLoggedInWithFirstPartyOAuth(context.Background(), cli); got != tt.want {
				t.Fatalf("claudeLoggedInWithFirstPartyOAuth() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClaudeLoggedInWithFirstPartyOAuthRejectsHungCommand(t *testing.T) {
	cli := claudeAuthFakeCLI(t)
	t.Setenv(claudeAuthFakeStatusEnv, claudeAuthSubscriptionStatus)
	t.Setenv(claudeAuthFakeModeEnv, "hang")
	// claudeLoginProbeTimeout is a constant; the probe inherits this shorter
	// caller deadline, so the test does not wait the full probe timeout.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	if claudeLoggedInWithFirstPartyOAuth(ctx, cli) {
		t.Fatal("hung auth status was accepted")
	}
	if elapsed := time.Since(started); elapsed >= claudeLoginProbeTimeout {
		t.Fatalf("probe returned after %v, want it stopped at the caller deadline", elapsed)
	}
}

func TestClaudeLoggedInWithFirstPartyOAuthRejectsMissingCommand(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "claude")
	if claudeLoggedInWithFirstPartyOAuth(context.Background(), missing) {
		t.Fatal("missing claude executable was accepted")
	}
}

func claudeAuthFakeCLI(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	return exe
}
