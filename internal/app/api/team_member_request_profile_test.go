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
	timed := func(engine string, firstChunk, idle int64) (*registry.AgentRecord, error) {
		record := &registry.AgentRecord{Name: "member"}
		err := configureDevelopmentMember(record, teamMemberAgentConfiguration{DisplayName: "成员", Engine: engine,
			FirstChunkTimeoutMS: firstChunk, IdleTimeoutMS: idle}, teamMemberRelationshipDraft{}, false, nil)
		return record, err
	}
	if record, err := timed("loom", 30000, 10000); err != nil || record.RequestProfile == nil ||
		record.RequestProfile.FirstChunkTimeoutMS != 30000 || record.RequestProfile.IdleTimeoutMS != 10000 {
		t.Fatalf("wait budgets: %+v %v", record.RequestProfile, err)
	}
	if _, err := timed("loom", -5, 0); err == nil {
		t.Fatal("a negative wait budget was accepted")
	}
	if _, err := timed("codex", 30000, 0); err == nil {
		t.Fatal("a CLI member accepted a wait budget it would never apply")
	}
	called := func(firstChunk, idle, call int64) (*registry.AgentRecord, error) {
		record := &registry.AgentRecord{Name: "member"}
		err := configureDevelopmentMember(record, teamMemberAgentConfiguration{DisplayName: "成员", Engine: "loom",
			FirstChunkTimeoutMS: firstChunk, IdleTimeoutMS: idle, CallTimeoutMS: call}, teamMemberRelationshipDraft{}, false, nil)
		return record, err
	}
	if record, err := called(30000, 15000, 120000); err != nil || record.RequestProfile == nil || record.RequestProfile.CallTimeoutMS != 120000 {
		t.Fatalf("call budget: %+v %v", record.RequestProfile, err)
	}
	// A stage wait as long as the whole call, or any budget as long as the
	// member step limit, would never be the one to end a call.
	for _, tc := range [][3]int64{{120000, 0, 120000}, {0, 180000, 120000}, {2700000, 0, 0}, {0, 0, 3000000}} {
		if _, err := called(tc[0], tc[1], tc[2]); err == nil {
			t.Fatalf("budgets %v were accepted although one can never fire", tc)
		}
	}
}
