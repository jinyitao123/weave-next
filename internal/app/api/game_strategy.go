package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
)

const gameStrategySemantics = `If strategy_snapshot is present, use its definition as soft decision preferences within the legal candidates, not as game legality. team_finish favors the team's finish order; self_finish favors emptying your own hand. Higher weights mean greater preference, not probabilities. Conditions: always; can_finish_hand means a legal candidate empties your hand; teammate_controls_table means the last non-pass public action belongs to teammate_seat; opponent_low_cards means either opponent has at most two remaining cards. Preferences: finish_hand, shed_more_cards, preserve_bombs (avoid spending a bomb unless beneficial), pass, take_control (play a non-pass candidate). Compare applicable preferences with the public situation; do not infer other players' hidden cards. Briefly mention the relevant strategy preference in rationale. Legacy v1 input has no structured policy. ` + gameInferenceSemantics

// Narrow, versioned game-policy data. These enums never grant tools or change
// workflow permissions; the application remains the authority on legal moves.
func validateGameStrategy(raw, strategy, publication json.RawMessage) error {
	var snapshot struct {
		Publication *struct {
			ProjectID    string `json:"project_id"`
			OntologyID   string `json:"ontology_id"`
			Version      string `json:"version"`
			PublishID    string `json:"publish_id"`
			DocumentHash string `json:"document_hash"`
		} `json:"publication,omitempty"`
		Source         string `json:"source"`
		CatalogVersion string `json:"catalog_version"`
		ContentHash    string `json:"content_hash"`
		Definition     struct {
			ID          string `json:"id"`
			Version     string `json:"version"`
			Name        string `json:"name"`
			Objective   string `json:"objective"`
			Preferences []struct {
				When   string `json:"when"`
				Prefer string `json:"prefer"`
				Weight int    `json:"weight"`
			} `json:"preferences"`
		} `json:"definition"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&snapshot); err != nil {
		return fmt.Errorf("invalid strategy snapshot: %w", err)
	}
	id := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
	s := snapshot.Definition
	var selected string
	if json.Unmarshal(strategy, &selected) != nil || selected != s.ID || !id.MatchString(snapshot.CatalogVersion) || !id.MatchString(s.ID) || !id.MatchString(s.Version) || s.Name == "" || len(s.Name) > 96 || (s.Objective != "team_finish" && s.Objective != "self_finish") || len(s.Preferences) < 1 || len(s.Preferences) > 12 {
		return fmt.Errorf("invalid strategy identity or objective")
	}
	switch snapshot.Source {
	case "local_ontology_candidate":
		if snapshot.Publication != nil {
			return fmt.Errorf("local candidate cannot claim publication")
		}
	case "ontology_published":
		p := snapshot.Publication
		var effective string
		if p == nil || !id.MatchString(p.ProjectID) || !id.MatchString(p.OntologyID) || !id.MatchString(p.PublishID) || !regexp.MustCompile(`^V[0-9]+\.[0-9]+$`).MatchString(p.Version) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(p.DocumentHash) || json.Unmarshal(publication, &effective) != nil || effective != p.Version+":"+p.PublishID {
			return fmt.Errorf("published strategy requires matching publication provenance")
		}
	default:
		return fmt.Errorf("unsupported strategy source")
	}
	seen := map[string]bool{}
	for _, p := range s.Preferences {
		when := p.When == "always" || p.When == "can_finish_hand" || p.When == "teammate_controls_table" || p.When == "opponent_low_cards"
		prefer := p.Prefer == "finish_hand" || p.Prefer == "shed_more_cards" || p.Prefer == "preserve_bombs" || p.Prefer == "pass" || p.Prefer == "take_control"
		key := p.When + ":" + p.Prefer
		if !when || !prefer || p.Weight < 1 || p.Weight > 100 || seen[key] {
			return fmt.Errorf("unsupported strategy preference")
		}
		seen[key] = true
	}
	canonical, _ := json.Marshal(s)
	h := sha256.Sum256(canonical)
	if snapshot.ContentHash != fmt.Sprintf("%x", h[:]) {
		return fmt.Errorf("strategy content hash mismatch")
	}
	return nil
}
