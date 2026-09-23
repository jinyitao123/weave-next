package businessaction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

func recordResourceForTest(objectName, id string) delegatedResource {
	raw, _ := json.Marshal(map[string]string{"id": id, "object_name": objectName})
	hash := sha256.Sum256(raw)
	return delegatedResource{Type: "forge-record", ID: id, ObjectName: objectName, SHA256: hex.EncodeToString(hash[:])}
}

func TestFrozenRecordIsInjectedAndCannotBeSubstituted(t *testing.T) {
	for _, object := range []string{"sales_contract", "sales_quote", "lead"} {
		t.Run(object, func(t *testing.T) {
			catalog := map[string]actionMetadata{object + ".submit": {Name: "submit", ObjectName: object, RequiresRecord: true}}
			for _, args := range []string{`{}`, `{"recordId":"record-a"}`, `{"recordId":"record-b"}`, `{"recordId":null}`} {
				host := &captureHost{}
				d, err := newDispatcherWithResources(host, []string{"forge:action:" + object + ".submit"}, catalog, []delegatedResource{recordResourceForTest(object, "record-a")})
				if err != nil {
					t.Fatal(err)
				}
				tools, _ := d.ListTools(t.Context())
				result, err := d.Dispatch(t.Context(), contract.ToolCall{ID: "call", Name: tools[0].Name, Args: args})
				if err != nil {
					t.Fatal(err)
				}
				if args == `{}` || args == `{"recordId":"record-a"}` {
					var actual map[string]any
					if result.IsError || json.Unmarshal([]byte(host.call.Args), &actual) != nil || actual["recordId"] != "record-a" || actual["objectName"] != object {
						t.Fatalf("result=%+v upstream=%s", result, host.call.Args)
					}
				} else if !result.IsError || host.call.Name != "" {
					t.Fatalf("substitution reached upstream: %+v", host.call)
				}
			}
		})
	}
}

func TestRequiredRecordFailsClosedBeforeToolsAreExposed(t *testing.T) {
	for _, resources := range [][]delegatedResource{
		nil,
		{recordResourceForTest("another_object", "a")},
		{recordResourceForTest("sales_contract", "a"), recordResourceForTest("sales_contract", "b")},
		{{Type: "forge-record", ID: "a", ObjectName: "sales_contract", SHA256: "changed"}},
	} {
		if _, err := newDispatcherWithResources(&captureHost{}, []string{"forge:action:sales_contract.ContractSubmit"}, contractSubmitCatalog(), resources); err == nil {
			t.Fatalf("accepted invalid scope: %+v", resources)
		}
	}
}

func TestRecordlessActionDoesNotAcceptUnboundRecord(t *testing.T) {
	for _, resources := range [][]delegatedResource{nil, {recordResourceForTest("lead", "lead-a")}} {
		host := &captureHost{}
		d, err := newDispatcherWithResources(host, []string{"forge:action:lead.create"}, map[string]actionMetadata{"lead.create": {Name: "create", ObjectName: "lead"}}, resources)
		if err != nil {
			t.Fatal(err)
		}
		tools, _ := d.ListTools(t.Context())
		result, err := d.Dispatch(t.Context(), contract.ToolCall{ID: "call", Name: tools[0].Name, Args: `{"recordId":"lead-a"}`})
		if err != nil || !result.IsError || host.call.Name != "" {
			t.Fatalf("result=%+v call=%+v err=%v", result, host.call, err)
		}
		result, err = d.Dispatch(t.Context(), contract.ToolCall{ID: "call", Name: tools[0].Name, Args: `{}`})
		if err != nil || result.IsError || host.call.Name != "run_action" {
			t.Fatalf("recordless action rejected: %+v %v", result, err)
		}
		var upstream map[string]any
		if err := json.Unmarshal([]byte(host.call.Args), &upstream); err != nil || upstream["recordId"] != "" {
			t.Fatalf("recordless action received a record binding: %+v %v", upstream, err)
		}
	}
}

func TestStoredRecordBindingIsRecoveredWithTheInput(t *testing.T) {
	input := delegatedResource{Type: "dispatch-input", ID: "input-a", SHA256: "input-hash"}
	record := recordResourceForTest("sales_quote", "quote-a")
	raw, _ := json.Marshal([]delegatedResource{input, record})
	got, err := decodeDelegatedResources(raw, "input-a")
	if err != nil || len(got) != 1 || got[0] != record {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	record.ID = "quote-b"
	raw, _ = json.Marshal([]delegatedResource{input, record})
	if _, err = decodeDelegatedResources(raw, "input-a"); err == nil {
		t.Fatal("accepted altered stored record")
	}
}
