package fileartifact

import "testing"

func TestMemberExportRejectsUnsafeOrAmbiguousFiles(t *testing.T) {
	for _, raw := range []string{
		`{"weave_member_artifacts_v1":[{"path":"../x","content_type":"text/plain","content":"x"}]}`,
		`{"weave_member_artifacts_v1":[{"path":"x","content_type":"text/plain","content":"a"},{"path":"x","content_type":"text/plain","content":"b"}]}`,
		`{"weave_member_artifacts_v1":"not-files"}`,
	} {
		if _, present, err := DecodeMemberReceipt(raw); !present || err == nil {
			t.Fatalf("invalid export accepted: %s", raw)
		}
	}
	if _, present, err := DecodeMemberReceipt(`{"artifacts":[{"path":"x"}]}`); present || err != nil {
		t.Fatal("ordinary tool text treated as delivery")
	}
}
