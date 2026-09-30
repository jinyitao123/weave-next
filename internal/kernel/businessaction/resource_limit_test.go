package businessaction

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

func delegatedResourcesForTest(t *testing.T, inputRevisionID string, files int) []byte {
	t.Helper()
	resources := []delegatedResource{
		{Type: "dispatch-input", ID: inputRevisionID, SHA256: strings.Repeat("a", 64)},
		recordResourceForTest("sales_contract", "record-a"),
	}
	for index := 0; index < files; index++ {
		resources = append(resources, delegatedResource{
			Type: "forge-file", ID: fmt.Sprintf("file-%02d", index), Name: fmt.Sprintf("材料%02d.md", index),
			MediaType: "text/markdown", Bytes: 100, SHA256: strings.Repeat("c", 64),
		})
	}
	raw, err := json.Marshal(resources)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The runtime accepts the largest handoff registration allows and refuses one
// more, so the two sides agree through frozen.MaxDelegatedFiles.
func TestDelegatedResourcesAcceptExactlyTheRegistrationMaximum(t *testing.T) {
	const input = "input-revision-1"
	decoded, err := decodeDelegatedResources(delegatedResourcesForTest(t, input, frozen.MaxDelegatedFiles), input)
	if err != nil {
		t.Fatalf("a handoff with the maximum %d files could not be decoded: %v", frozen.MaxDelegatedFiles, err)
	}
	if want := frozen.MaxDelegatedFiles + 1; len(decoded) != want { // the input entry is not returned
		t.Fatalf("decoded %d resources, want %d files plus the record", len(decoded), want)
	}
	if _, err := decodeDelegatedResources(delegatedResourcesForTest(t, input, frozen.MaxDelegatedFiles+1), input); err == nil {
		t.Fatalf("a handoff with %d files was decoded although it exceeds the shared limit", frozen.MaxDelegatedFiles+1)
	}
}
