package runtimes

import (
	"context"
	"errors"
	"sort"
	"strings"
)

const (
	SelectionExplicit = "explicit"
	SelectionAuto     = "auto"

	ReasonExplicitEligible = "explicit_runtime_eligible"
	ReasonAgentDefault     = "agent_default_eligible"
	ReasonMostRecent       = "most_recent_eligible"
)

var (
	ErrRuntimeSelectionUnavailable = errors.New("runtime selection unavailable")
	ErrNoEligibleRuntime           = errors.New("no eligible runtime")
)

// CandidateFact is the server-observed capability state used for selection.
type CandidateFact struct {
	RuntimeID         string   `json:"runtime_id"`
	Name              string   `json:"name"`
	Engines           []string `json:"engines"`
	RuntimeRevision   int64    `json:"runtime_revision"`
	PoolID            string   `json:"pool_id,omitempty"`
	HealthStatus      string   `json:"health_status"`
	TotalSlots        int      `json:"total_slots"`
	ActiveSlots       int      `json:"active_slots"`
	Enabled           bool     `json:"enabled"`
	Online            bool     `json:"online"`
	Eligible          bool     `json:"eligible"`
	UnavailableReason string   `json:"unavailable_reason,omitempty"`
}

// Assignment is the explainable Runtime decision for one admission.
type Assignment struct {
	RuntimeID       string          `json:"runtime_id,omitempty"`
	RuntimeRevision int64           `json:"runtime_revision,omitempty"`
	Mode            string          `json:"mode"`
	ReasonCode      string          `json:"reason_code"`
	Engine          string          `json:"engine"`
	PoolID          string          `json:"pool_id,omitempty"`
	CapabilityFacts []CandidateFact `json:"capability_facts"`
}

// Select chooses one eligible Runtime without mutating Agent configuration.
func (s *Store) Select(
	ctx context.Context,
	workspaceID, engine, requestedRuntimeID, preferredRuntimeID string,
) (Assignment, error) {
	if s == nil {
		return Assignment{}, ErrRuntimeSelectionUnavailable
	}
	engine = strings.TrimSpace(engine)
	requestedRuntimeID = strings.TrimSpace(requestedRuntimeID)
	preferredRuntimeID = strings.TrimSpace(preferredRuntimeID)
	stored, err := s.List(ctx, workspaceID)
	if err != nil {
		return Assignment{}, err
	}
	sort.SliceStable(stored, func(i, j int) bool {
		leftHealthy, rightHealthy := stored[i].HealthStatus == "healthy", stored[j].HealthStatus == "healthy"
		if leftHealthy != rightHealthy {
			return leftHealthy
		}
		leftAvailable := stored[i].ActiveSlots < stored[i].TotalSlots
		rightAvailable := stored[j].ActiveSlots < stored[j].TotalSlots
		if leftAvailable != rightAvailable {
			return leftAvailable
		}
		left, right := stored[i].LastHeartbeatAt, stored[j].LastHeartbeatAt
		if left != nil && right != nil && !left.Equal(*right) {
			return left.After(*right)
		}
		if left != nil && right == nil {
			return true
		}
		if left == nil && right != nil {
			return false
		}
		return stored[i].ID < stored[j].ID
	})

	facts := make([]CandidateFact, 0, len(stored))
	eligible := make([]Runtime, 0, len(stored))
	for _, runtime := range stored {
		fact := CandidateFact{
			RuntimeID: runtime.ID, Name: runtime.Name,
			Engines:         append([]string(nil), runtime.Engines...),
			RuntimeRevision: runtime.FunctionalRevision,
			PoolID:          runtime.PoolID, HealthStatus: runtime.HealthStatus,
			TotalSlots: runtime.TotalSlots, ActiveSlots: runtime.ActiveSlots,
			Enabled: runtime.Enabled, Online: runtime.Online,
		}
		switch {
		case !runtime.Enabled:
			fact.UnavailableReason = "runtime_disabled"
		case runtime.RevokedAt != nil:
			fact.UnavailableReason = "runtime_revoked"
		case !runtime.Online:
			fact.UnavailableReason = "runtime_offline"
		case runtime.HealthStatus == "quarantined":
			fact.UnavailableReason = "runtime_quarantined"
		case !containsEngine(runtime.Engines, engine):
			fact.UnavailableReason = "engine_unavailable"
		default:
			fact.Eligible = true
			eligible = append(eligible, runtime)
		}
		facts = append(facts, fact)
	}

	if requestedRuntimeID != "" {
		for _, runtime := range eligible {
			if runtime.ID == requestedRuntimeID {
				return runtimeAssignment(runtime, engine, SelectionExplicit, ReasonExplicitEligible, facts), nil
			}
		}
		return Assignment{}, ErrRuntimeSelectionUnavailable
	}
	if preferredRuntimeID != "" {
		for _, runtime := range eligible {
			if runtime.ID == preferredRuntimeID {
				return runtimeAssignment(runtime, engine, SelectionAuto, ReasonAgentDefault, facts), nil
			}
		}
	}
	if len(eligible) == 0 {
		return Assignment{}, ErrNoEligibleRuntime
	}
	return runtimeAssignment(eligible[0], engine, SelectionAuto, ReasonMostRecent, facts), nil
}

func runtimeAssignment(
	runtime Runtime,
	engine, mode, reason string,
	facts []CandidateFact,
) Assignment {
	return Assignment{
		RuntimeID: runtime.ID, RuntimeRevision: runtime.FunctionalRevision,
		Mode: mode, ReasonCode: reason, Engine: engine, PoolID: runtime.PoolID,
		CapabilityFacts: append([]CandidateFact(nil), facts...),
	}
}

func containsEngine(engines []string, expected string) bool {
	for _, engine := range engines {
		if engine == expected {
			return true
		}
	}
	return false
}
