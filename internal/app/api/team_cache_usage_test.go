package api

import (
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"testing"
)

func TestTeamCacheUsageProjectionRetainsKnownSubtotalsAndMissingCoverage(t *testing.T) {
	known := loomruntime.TerminalUsage{InputTokens: 100, CacheReadTokens: 80, CacheWriteTokens: 10, CacheReadComplete: true, CacheWriteComplete: true}
	total, agents, err := foldTeamContributions([]teamUsageContribution{{RunID: "a", Agent: "worker", Usage: known}, {RunID: "b", Agent: "worker", Usage: known}})
	if err != nil || total.InputTokens != 200 || total.CacheReadTokens != 160 || total.CacheWriteTokens != 20 || !total.CacheReadComplete || len(agents) != 1 || agents[0].Usage.CacheReadTokens != 160 || !agents[0].Usage.CacheWriteComplete {
		t.Fatalf("team cache projection: %+v %+v %v", total, agents, err)
	}
	partial, _, err := foldTeamContributions([]teamUsageContribution{{RunID: "a", Agent: "worker", Usage: known}, {RunID: "b", Agent: "worker", Usage: loomruntime.TerminalUsage{InputTokens: 100}}})
	if err != nil || partial.CacheReadTokens != 80 || partial.CacheReadComplete || partial.CacheWriteComplete {
		t.Fatalf("partial team usage became complete: %+v %v", partial, err)
	}
}
