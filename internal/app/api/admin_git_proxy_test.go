package api

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

func pktLine(text string) string { return fmt.Sprintf("%04x%s", len(text)+4, text) }

func TestAllowOnlyRefUpdateChecksEveryCommand(t *testing.T) {
	zero, sha := strings.Repeat("0", 40), strings.Repeat("a", 40)
	allowed := "refs/heads/weave/20261007-0900-abcd"
	own := pktLine(zero+" "+sha+" "+allowed+"\x00report-status side-band-64k\n") + "0000PACKDATA"
	body, err := allowOnlyRefUpdate(strings.NewReader(own), allowed)
	if err != nil {
		t.Fatal(err)
	}
	if forwarded, _ := io.ReadAll(body); string(forwarded) != own {
		t.Fatalf("forwarded body changed: %q", forwarded)
	}
	for name, request := range map[string]string{
		"second ref":  pktLine(zero+" "+sha+" "+allowed+"\x00report-status\n") + pktLine(zero+" "+sha+" refs/heads/main\n") + "0000",
		"delete main": pktLine(sha+" "+zero+" refs/heads/main\x00report-status\n") + "0000",
		"tag":         pktLine(zero+" "+sha+" refs/tags/v1\n") + "0000",
		"no command":  "0000",
		"malformed":   "zzzz",
		"truncated":   "00ff" + zero,
	} {
		if _, err := allowOnlyRefUpdate(strings.NewReader(request), allowed); err == nil {
			t.Errorf("%s was allowed", name)
		}
	}
	if !gitProxyRepository("https://git.example.com/team/app.git") || gitProxyRepository("git@example.com:team/app.git") || gitProxyRepository("https://user@example.com/app.git") {
		t.Fatal("proxy repository rule changed")
	}
}
