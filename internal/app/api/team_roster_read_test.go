package api

import (
	"encoding/json"
	"testing"

	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestTeamRosterDoesNotReportRegisteredLeadAsDisabled(t *testing.T) {
	rosters := aggregateTeamRosters(
		[]org.Team{{ID: "team", LeadAvatarID: "lead"}, {ID: "missing", LeadAvatarID: "unknown"}},
		[]registry.AgentRecord{{ID: "lead", Name: "leader", Role: "avatar"}, {ID: "worker", Name: "reviewer", Role: "worker"}},
		[]registry.TeamWorker{{TeamID: "team", WorkerAgentID: "worker", Enabled: false}},
	)
	data, err := json.Marshal(rosters)
	if err != nil {
		t.Fatal(err)
	}
	var result []struct {
		Lead *struct {
			Enabled bool `json:"enabled"`
		} `json:"lead"`
		Workers []struct {
			Enabled bool `json:"enabled"`
		} `json:"workers"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result[0].Lead == nil || !result[0].Lead.Enabled {
		t.Fatalf("registered team lead reported unavailable: %s", data)
	}
	if len(result[0].Workers) != 1 || result[0].Workers[0].Enabled {
		t.Fatalf("disabled worker membership was changed: %s", data)
	}
	if result[1].Lead != nil {
		t.Fatalf("missing lead was fabricated: %s", data)
	}
}
