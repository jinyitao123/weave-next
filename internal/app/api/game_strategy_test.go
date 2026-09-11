package api

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

func TestGameStrategyV2Contract(t *testing.T) {
	// Field order is the canonical definition order in the application contract.
	definition := json.RawMessage(`{"id":"balanced","version":"1","name":"均衡","objective":"team_finish","preferences":[{"when":"always","prefer":"shed_more_cards","weight":45}]}`)
	h := sha256.Sum256(definition)
	snapshot := map[string]any{"source": "local_ontology_candidate", "catalog_version": "local-1", "content_hash": fmt.Sprintf("%x", h[:]), "definition": definition}
	valid := gameTestInput("room-1")
	valid["schema_version"] = "guandan-decision-v2"
	valid["strategy_snapshot"] = snapshot
	raw, _ := json.Marshal(valid)
	if _, err := validateGameInput(raw); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(map[string]any){
		"v1-extra":      func(v map[string]any) { v["schema_version"] = "guandan-decision-v1" },
		"v2-missing":    func(v map[string]any) { delete(v, "strategy_snapshot") },
		"mismatched-id": func(v map[string]any) { v["strategy"] = "team_first" },
		"hash":          func(v map[string]any) { v["strategy_snapshot"].(map[string]any)["content_hash"] = "bad" },
		"unsupported-preference": func(v map[string]any) {
			d := v["strategy_snapshot"].(map[string]any)["definition"].(map[string]any)
			d["preferences"].([]any)[0].(map[string]any)["prefer"] = "read_other_hands"
		},
		"tool-expansion": func(v map[string]any) { v["strategy_snapshot"].(map[string]any)["tools"] = []string{"*"} },
	} {
		t.Run(name, func(t *testing.T) {
			var v map[string]any
			_ = json.Unmarshal(raw, &v)
			change(v)
			b, _ := json.Marshal(v)
			if _, err := validateGameInput(b); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
}
