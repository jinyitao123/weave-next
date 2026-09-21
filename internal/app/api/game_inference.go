package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
)

const gameInferenceSemantics = `The v3 inference field is a deterministic card-counting aid computed from own_hand plus public_history. remaining_by_rank is the total still hidden outside this seat, not a claim about a particular opponent. seat_estimates and candidate_metrics are bounded estimates, not facts. Compare finish_distance, response_risk, control_retention, break_cost and team_assist when choosing; prefer useful team outcomes without treating a probability as certainty. Never claim to know another seat's cards.`

var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type gameInferenceSnapshot struct {
	Version                    string                         `json:"version"`
	Visibility                 string                         `json:"visibility"`
	ObservedCardCount          int                            `json:"observed_card_count"`
	UnknownCardCount           int                            `json:"unknown_card_count"`
	PublicHighControlRemaining int                            `json:"public_high_control_remaining"`
	HighControlUnknown         int                            `json:"high_control_unknown"`
	RemainingByRank            map[string]int                 `json:"remaining_by_rank"`
	SeatEstimates              map[string]gameSeatEstimate    `json:"seat_estimates"`
	OwnHandShape               gameHandShape                  `json:"own_hand_shape"`
	CandidateMetrics           map[string]gameCandidateMetric `json:"candidate_metrics"`
	ChosenMetric               *gameCandidateMetric           `json:"chosen_metric,omitempty"`
	PolicyHash                 string                         `json:"policy_hash"`
	PolicyPublication          *gameInferencePublication      `json:"policy_publication"`
	ExplanationThresholds      gameInferenceThresholds        `json:"explanation_thresholds"`
}

type gameInferenceThresholds struct {
	ResponseRiskMedium float64 `json:"response_risk_medium"`
	ResponseRiskHigh   float64 `json:"response_risk_high"`
	BreakCostMedium    float64 `json:"break_cost_medium"`
	BreakCostHigh      float64 `json:"break_cost_high"`
	TeamAssistLow      float64 `json:"team_assist_low"`
	TeamAssistHigh     float64 `json:"team_assist_high"`
}

type gameInferencePublication struct {
	ProjectID    string `json:"project_id"`
	OntologyID   string `json:"ontology_id"`
	Version      string `json:"version"`
	PublishID    string `json:"publish_id"`
	DocumentHash string `json:"document_hash"`
}

type gameSeatEstimate struct {
	HandCount      int     `json:"hand_count"`
	EstimatedTurns int     `json:"estimated_turns"`
	BombRisk       float64 `json:"bomb_risk"`
}

type gameHandShape struct {
	FormationScore float64 `json:"formation_score"`
	LooseCards     int     `json:"loose_cards"`
	MaxShed        int     `json:"max_shed"`
	EstimatedTurns int     `json:"estimated_turns"`
}

type gameCandidateMetric struct {
	CandidateID      string  `json:"candidate_id"`
	CardsShed        int     `json:"cards_shed"`
	FinishDistance   int     `json:"finish_distance"`
	ResponseRisk     float64 `json:"response_risk"`
	ControlRetention float64 `json:"control_retention"`
	BreakCost        float64 `json:"break_cost"`
	TeamAssist       float64 `json:"team_assist"`
}

func validateGameInference(raw json.RawMessage, input map[string]json.RawMessage) error {
	var snapshot gameInferenceSnapshot
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return fmt.Errorf("invalid inference snapshot: %w", err)
	}
	if decoder.Decode(new(any)) == nil {
		return fmt.Errorf("inference snapshot must be one object")
	}
	if snapshot.Version != "guandan-card-inference-v1" || snapshot.Visibility != "acting_seat_hand_and_public_events_only" || snapshot.ChosenMetric != nil {
		return fmt.Errorf("invalid inference identity or visibility")
	}
	publication := snapshot.PolicyPublication
	var publishedVersion string
	_ = json.Unmarshal(input["published_version"], &publishedVersion)
	if publication == nil || publication.ProjectID == "" || publication.OntologyID == "" || publication.Version+":"+publication.PublishID != publishedVersion || !sha256HexPattern.MatchString(publication.DocumentHash) || !sha256HexPattern.MatchString(snapshot.PolicyHash) {
		return fmt.Errorf("invalid inference policy publication")
	}
	t := snapshot.ExplanationThresholds
	if t.ResponseRiskMedium != .38 || t.ResponseRiskHigh != .68 || t.BreakCostMedium != .2 || t.BreakCostHigh != .55 || t.TeamAssistLow != .25 || t.TeamAssistHigh != .68 {
		return fmt.Errorf("invalid inference explanation thresholds")
	}
	var ownHand []struct {
		Rank int `json:"rank"`
	}
	var history []struct {
		Cards []string `json:"cards"`
	}
	var remainingCounts map[string]int
	var candidates []struct {
		ID    string   `json:"candidate_id"`
		Cards []string `json:"cards"`
	}
	if json.Unmarshal(input["own_hand"], &ownHand) != nil || json.Unmarshal(input["public_history"], &history) != nil || json.Unmarshal(input["remaining_counts"], &remainingCounts) != nil || json.Unmarshal(input["legal_candidates"], &candidates) != nil {
		return fmt.Errorf("inference evidence cannot be verified")
	}
	observed := 0
	for _, action := range history {
		observed += len(action.Cards)
	}
	if snapshot.ObservedCardCount != observed || snapshot.UnknownCardCount != 108-len(ownHand)-observed || snapshot.UnknownCardCount < 0 || snapshot.UnknownCardCount > 108 {
		return fmt.Errorf("inference evidence counts mismatch")
	}
	rankTotal := 0
	for rank := 2; rank <= 16; rank++ {
		value, ok := snapshot.RemainingByRank[strconv.Itoa(rank)]
		limit := 8
		if rank >= 15 {
			limit = 2
		}
		if !ok || value < 0 || value > limit {
			return fmt.Errorf("invalid remaining rank count")
		}
		rankTotal += value
	}
	if len(snapshot.RemainingByRank) != 15 || rankTotal != snapshot.UnknownCardCount || snapshot.PublicHighControlRemaining < 0 || snapshot.PublicHighControlRemaining > 12 || snapshot.HighControlUnknown < 0 || snapshot.HighControlUnknown > 12 {
		return fmt.Errorf("invalid card-counting totals")
	}
	if len(snapshot.SeatEstimates) != 4 {
		return fmt.Errorf("all seat estimates are required")
	}
	for _, seat := range []string{"south", "east", "north", "west"} {
		estimate, ok := snapshot.SeatEstimates[seat]
		if !ok || estimate.HandCount != remainingCounts[seat] || estimate.HandCount < 0 || estimate.HandCount > 27 || estimate.EstimatedTurns < 0 || estimate.EstimatedTurns > 27 || !probability(estimate.BombRisk) {
			return fmt.Errorf("invalid seat estimate")
		}
	}
	shape := snapshot.OwnHandShape
	if !probability(shape.FormationScore) || shape.LooseCards < 0 || shape.LooseCards > len(ownHand) || shape.MaxShed < 1 || shape.MaxShed > len(ownHand) || shape.EstimatedTurns < 0 || shape.EstimatedTurns > len(ownHand) {
		return fmt.Errorf("invalid hand-shape estimate")
	}
	if len(snapshot.CandidateMetrics) != len(candidates) {
		return fmt.Errorf("candidate metrics must cover the frozen set")
	}
	for _, candidate := range candidates {
		metric, ok := snapshot.CandidateMetrics[candidate.ID]
		if !ok || metric.CandidateID != candidate.ID || metric.CardsShed != len(candidate.Cards) || metric.CardsShed < 0 || metric.CardsShed > len(ownHand) || metric.FinishDistance < 0 || metric.FinishDistance > len(ownHand) || !probability(metric.ResponseRisk) || !probability(metric.ControlRetention) || !probability(metric.BreakCost) || !probability(metric.TeamAssist) || math.Abs(metric.ResponseRisk+metric.ControlRetention-1) > .011 {
			return fmt.Errorf("invalid candidate metric")
		}
	}
	return nil
}

func probability(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}
