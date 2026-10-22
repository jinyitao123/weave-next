package api

import (
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestDevelopmentMemberReasoningEffort(t *testing.T) {
	configure := func(engine, effort string) (*registry.AgentRecord, error) {
		record := &registry.AgentRecord{Name: "member"}
		err := configureDevelopmentMember(record, teamMemberAgentConfiguration{DisplayName: "成员", Engine: engine, ReasoningEffort: effort}, teamMemberRelationshipDraft{}, false, nil)
		return record, err
	}
	if record, err := configure("loom", "high"); err != nil || record.RequestProfile == nil || record.RequestProfile.ReasoningEffort != "high" {
		t.Fatalf("chosen effort: %+v %v", record.RequestProfile, err)
	}
	if record, err := configure("loom", ""); err != nil || record.RequestProfile != nil {
		t.Fatalf("unset effort stored a profile: %+v %v", record.RequestProfile, err)
	}
	if _, err := configure("loom", "extreme"); err == nil {
		t.Fatal("an unknown effort was accepted")
	}
	if _, err := configure("claude", "low"); err == nil {
		t.Fatal("a CLI member accepted a reasoning effort it would never send")
	}
}
