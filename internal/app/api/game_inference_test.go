package api

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

func validV3GameInput() map[string]any {
	definition := json.RawMessage(`{"id":"balanced","version":"1","name":"均衡","objective":"team_finish","preferences":[{"when":"always","prefer":"shed_more_cards","weight":45}]}`)
	hash := sha256.Sum256(definition)
	remaining := map[string]int{}
	for rank := 2; rank <= 14; rank++ {
		remaining[fmt.Sprint(rank)] = 8
	}
	remaining["3"] = 7
	remaining["15"] = 2
	remaining["16"] = 2
	input := gameTestInput("room-1")
	input["schema_version"] = "guandan-decision-v3"
	input["control_revision"] = 0
	input["published_version"] = "V260911.18:publication-inference"
	input["own_hand"] = []any{map[string]any{"id": "0-S-3", "rank": 3, "suit": "S"}}
	input["remaining_counts"] = map[string]int{"south": 1, "east": 27, "north": 27, "west": 27}
	input["legal_candidates"] = []any{map[string]any{"candidate_id": "candidate-a", "cards": []string{"0-S-3"}}}
	input["strategy_snapshot"] = map[string]any{"source": "local_ontology_candidate", "catalog_version": "local-1", "content_hash": fmt.Sprintf("%x", hash[:]), "definition": definition}
	input["inference"] = map[string]any{
		"version": "guandan-card-inference-v1", "visibility": "acting_seat_hand_and_public_events_only",
		"policy_hash":            fmt.Sprintf("%064d", 1),
		"policy_publication":     map[string]any{"project_id": "project", "ontology_id": "hea5bbnj18", "version": "V260911.18", "publish_id": "publication-inference", "document_hash": fmt.Sprintf("%064d", 2)},
		"explanation_thresholds": map[string]any{"response_risk_medium": .38, "response_risk_high": .68, "break_cost_medium": .2, "break_cost_high": .55, "team_assist_low": .25, "team_assist_high": .68},
		"observed_card_count":    0, "unknown_card_count": 107,
		"public_high_control_remaining": 12, "high_control_unknown": 11,
		"remaining_by_rank": remaining,
		"seat_estimates": map[string]any{
			"south": map[string]any{"hand_count": 1, "estimated_turns": 1, "bomb_risk": 0},
			"east":  map[string]any{"hand_count": 27, "estimated_turns": 6, "bomb_risk": .2},
			"north": map[string]any{"hand_count": 27, "estimated_turns": 6, "bomb_risk": .2},
			"west":  map[string]any{"hand_count": 27, "estimated_turns": 6, "bomb_risk": .2},
		},
		"own_hand_shape": map[string]any{"formation_score": 1, "loose_cards": 0, "max_shed": 1, "estimated_turns": 1},
		"candidate_metrics": map[string]any{
			"candidate-a": map[string]any{"candidate_id": "candidate-a", "cards_shed": 1, "finish_distance": 0, "response_risk": .5, "control_retention": .5, "break_cost": 0, "team_assist": 1},
		},
	}
	return input
}

func TestGameInferenceV3Contract(t *testing.T) {
	valid := validV3GameInput()
	raw, _ := json.Marshal(valid)
	if _, err := validateGameInput(raw); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"missing":           func(v map[string]any) { delete(v, "inference") },
		"missing-control":   func(v map[string]any) { delete(v, "control_revision") },
		"v2-extra":          func(v map[string]any) { v["schema_version"] = "guandan-decision-v2" },
		"private-expansion": func(v map[string]any) { v["inference"].(map[string]any)["other_hands"] = map[string]any{} },
		"rank-total": func(v map[string]any) {
			v["inference"].(map[string]any)["remaining_by_rank"].(map[string]any)["3"] = float64(8)
		},
		"seat-count": func(v map[string]any) {
			v["inference"].(map[string]any)["seat_estimates"].(map[string]any)["east"].(map[string]any)["hand_count"] = float64(26)
		},
		"candidate-gap": func(v map[string]any) {
			delete(v["inference"].(map[string]any)["candidate_metrics"].(map[string]any), "candidate-a")
		},
		"probability": func(v map[string]any) {
			v["inference"].(map[string]any)["candidate_metrics"].(map[string]any)["candidate-a"].(map[string]any)["response_risk"] = 1.2
		},
		"preselected": func(v map[string]any) {
			v["inference"].(map[string]any)["chosen_metric"] = map[string]any{"candidate_id": "candidate-a"}
		},
		"unpublished-policy": func(v map[string]any) {
			delete(v["inference"].(map[string]any), "policy_publication")
		},
		"invalid-policy-hash": func(v map[string]any) {
			v["inference"].(map[string]any)["policy_hash"] = fmt.Sprintf("%064s", "z")
		},
		"changed-threshold": func(v map[string]any) {
			v["inference"].(map[string]any)["explanation_thresholds"].(map[string]any)["response_risk_high"] = .7
		},
	} {
		t.Run(name, func(t *testing.T) {
			var candidate map[string]any
			_ = json.Unmarshal(raw, &candidate)
			mutate(candidate)
			encoded, _ := json.Marshal(candidate)
			if _, err := validateGameInput(encoded); err == nil {
				t.Fatal("invalid inference accepted")
			}
		})
	}
}
