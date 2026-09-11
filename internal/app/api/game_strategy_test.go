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
	publication := map[string]any{"project_id": "project-test", "ontology_id": "ontology-test", "version": "V260911.16", "publish_id": "publication-test", "document_hash": fmt.Sprintf("%x", h[:])}
	snapshot["source"] = "ontology_published"
	snapshot["publication"] = publication
	valid["published_version"] = "V260911.16:publication-test"
	raw, _ = json.Marshal(valid)
	if _, err := validateGameInput(raw); err != nil {
		t.Fatalf("published policy rejected: %v", err)
	}
	for name, change := range map[string]func(map[string]any){
		"missing-publication":         func(v map[string]any) { delete(v["strategy_snapshot"].(map[string]any), "publication") },
		"publication-drift":           func(v map[string]any) { v["published_version"] = "V260911.15:other" },
		"candidate-publication-claim": func(v map[string]any) { v["strategy_snapshot"].(map[string]any)["source"] = "local_ontology_candidate" },
		"publication-unknown-field": func(v map[string]any) {
			v["strategy_snapshot"].(map[string]any)["publication"].(map[string]any)["tools"] = []string{"*"}
		},
		"bad-publication-hash": func(v map[string]any) {
			v["strategy_snapshot"].(map[string]any)["publication"].(map[string]any)["document_hash"] = "bad"
		},
	} {
		t.Run(name, func(t *testing.T) {
			var v map[string]any
			_ = json.Unmarshal(raw, &v)
			change(v)
			b, _ := json.Marshal(v)
			if _, err := validateGameInput(b); err == nil {
				t.Fatal("invalid publication accepted")
			}
		})
	}
}
