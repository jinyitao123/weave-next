package frozen

import "testing"

func TestRequestProfileNormalizesAndValidates(t *testing.T) {
	base := FrozenAgentRecord{SchemaVersion: 1, WorkspaceID: "ws", AgentID: "a", AgentVersion: 1, Name: "a",
		Profiles: map[string]FrozenAgentProfile{}, OutputSchema: []byte("null"), FactoryInput: []byte("{}")}
	plain, err := Canonicalize(base, PreorderFrozenAgentRecord)
	if err != nil {
		t.Fatal(err)
	}
	empty := base
	empty.RequestProfile = &FrozenRequestProfile{}
	if encoded, err := Canonicalize(empty, PreorderFrozenAgentRecord); err != nil || string(encoded) != string(plain) {
		t.Fatalf("an empty request profile changed the canonical record: %v\n%s\n%s", err, encoded, plain)
	}
	chosen := base
	chosen.RequestProfile = &FrozenRequestProfile{ReasoningEffort: "low"}
	encoded, err := Canonicalize(chosen, PreorderFrozenAgentRecord)
	if err != nil || string(encoded) == string(plain) {
		t.Fatalf("a chosen reasoning effort did not reach the canonical record: %v", err)
	}
	invalid := base
	invalid.RequestProfile = &FrozenRequestProfile{ReasoningEffort: "extreme"}
	if _, err := Canonicalize(invalid, PreorderFrozenAgentRecord); err == nil {
		t.Fatal("an unknown reasoning effort was frozen")
	}
}

func TestRequestProfileWaitTimeoutsAreBounded(t *testing.T) {
	for _, tc := range []struct {
		profile FrozenRequestProfile
		valid   bool
	}{
		{FrozenRequestProfile{FirstChunkTimeoutMS: 30000, IdleTimeoutMS: 15000}, true},
		{FrozenRequestProfile{FirstChunkTimeoutMS: MaxWaitTimeoutMS}, true},
		{FrozenRequestProfile{FirstChunkTimeoutMS: -1}, false},
		{FrozenRequestProfile{IdleTimeoutMS: MaxWaitTimeoutMS + 1}, false},
	} {
		if err := ValidateRequestProfile(tc.profile); (err == nil) != tc.valid {
			t.Errorf("%+v: valid=%v, error %v", tc.profile, tc.valid, err)
		}
	}
}
