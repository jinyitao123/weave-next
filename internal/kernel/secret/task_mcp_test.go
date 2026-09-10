package secret

import (
	"strings"
	"testing"
)

func TestTaskMCPTokenBindsAllClaimDimensions(t *testing.T) {
	t.Setenv("WEAVE_SECRET_KEY_FILE", "")
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("ab", 32))
	claims := TaskMCPClaims{WorkspaceID: "ws", TaskID: "task", RuntimeID: "runtime", ServerIndex: 1, BindingDigest: "digest", ClaimEpoch: 2}
	token, err := SignTaskMCPToken(claims)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyTaskMCPToken(token)
	if err != nil || got != claims {
		t.Fatalf("roundtrip failed: %v", err)
	}
	for _, invalid := range []string{token + "x", strings.Replace(token, "tmcp1.", "rtk_", 1), strings.Repeat("x", 5000), BoundaryToken("ws", "worker", 1), MCPGatewayToken("ws", "worker", "server")} {
		if _, err := VerifyTaskMCPToken(invalid); err == nil {
			t.Fatal("accepted invalid or foreign token")
		}
	}
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("cd", 32))
	if _, err := VerifyTaskMCPToken(token); err == nil {
		t.Fatal("accepted revoked signing key")
	}
}
