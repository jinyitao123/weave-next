package api

import "testing"

func TestPlatformInternalAgentGateMatchesOnlyTheReservedPrefix(t *testing.T) {
	for _, name := range []string{"__graph_designer", "__another_platform_agent", "__"} {
		if !isPlatformInternalAgent(name) {
			t.Errorf("isPlatformInternalAgent(%q) = false", name)
		}
	}
	for _, name := range []string{"ordinary-agent", "a__b", "_single", "", "team__architect"} {
		if isPlatformInternalAgent(name) {
			t.Errorf("isPlatformInternalAgent(%q) = true", name)
		}
	}
}
