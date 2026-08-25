package teamcompiler

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"sort"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

type assembler struct {
	routeKey func(string, string) string
}

func NewTeamInteractionAssembler() TeamInteractionAssembler {
	return &assembler{routeKey: RouteKey}
}

// RouteKey returns the sole canonical handoff route encoding.
func RouteKey(runSnapshotID, workerAgentID string) string {
	snapshotHash := sha256.Sum256([]byte(runSnapshotID))
	return "tw1." + base64.RawURLEncoding.EncodeToString(snapshotHash[:]) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(workerAgentID))
}

func (a *assembler) Assemble(_ context.Context, in TeamInteractionInput) (TeamInteractionCatalog, error) {
	if in.WorkspaceID == "" || in.TeamID == "" || in.RunID == "" || in.RunSnapshotID == "" ||
		in.Lead.AgentID == "" || in.Lead.AgentVersion < 1 {
		return TeamInteractionCatalog{}, ErrTeamCompileInputInvalid
	}

	enabled := make([]FrozenTeamWorker, 0, len(in.Workers))
	workerIDs := make(map[string]struct{}, len(in.Workers))
	for _, worker := range in.Workers {
		if worker.WorkerAgentID == "" {
			return TeamInteractionCatalog{}, ErrTeamCompileInputInvalid
		}
		if _, duplicate := workerIDs[worker.WorkerAgentID]; duplicate {
			return TeamInteractionCatalog{}, ErrTeamCompileInputInvalid
		}
		workerIDs[worker.WorkerAgentID] = struct{}{}
		if !worker.EnabledAtSnapshot {
			continue
		}
		if worker.WorkerAgentVersion < 1 {
			return TeamInteractionCatalog{}, ErrTeamCompileInputInvalid
		}
		if err := validateKinds(worker.AllowedKinds, worker.DefaultKind); err != nil {
			return TeamInteractionCatalog{}, err
		}
		if err := validateRoleProofShape(worker.RoleProof); err != nil {
			return TeamInteractionCatalog{}, err
		}
		enabled = append(enabled, worker)
	}

	sort.Slice(enabled, func(i, j int) bool {
		return enabled[i].WorkerAgentID < enabled[j].WorkerAgentID
	})
	catalog := TeamInteractionCatalog{
		WorkspaceID: in.WorkspaceID, TeamID: in.TeamID, RunID: in.RunID,
		RunSnapshotID: in.RunSnapshotID, LeadAgentID: in.Lead.AgentID,
		Consult: map[string]AuthorizedWorker{}, Dispatch: map[string]AuthorizedWorker{},
		Handoff: map[string]HandoffRoute{},
	}
	routeOwners := make(map[string]HandoffRoute, len(enabled))
	for _, worker := range enabled {
		authorized := authorizedWorker(worker)
		kinds := canonicalKinds(worker.AllowedKinds)
		for _, kind := range kinds {
			switch kind {
			case InteractionConsult:
				catalog.Consult[worker.WorkerAgentID] = authorized
			case InteractionDispatch:
				catalog.Dispatch[worker.WorkerAgentID] = authorized
			case InteractionHandoff:
				routeKey := a.routeKey(in.RunSnapshotID, worker.WorkerAgentID)
				route := HandoffRoute{
					RouteKey: routeKey, WorkerAgentID: worker.WorkerAgentID,
					WorkerAgentVersion: worker.WorkerAgentVersion, DisplayName: worker.Name,
				}
				if prior, collision := routeOwners[routeKey]; collision &&
					(prior.WorkerAgentID != route.WorkerAgentID ||
						prior.WorkerAgentVersion != route.WorkerAgentVersion) {
					return TeamInteractionCatalog{}, ErrTeamInteractionRouteCollision
				}
				routeOwners[routeKey] = route
				catalog.Handoff[routeKey] = route
			}
		}
	}
	return catalog, nil
}

func (a *assembler) AuthorizeInteraction(
	_ context.Context,
	catalog TeamInteractionCatalog,
	req InteractionAuthorizationRequest,
) (AuthorizedWorker, error) {
	if catalog.WorkspaceID != req.WorkspaceID || catalog.TeamID != req.TeamID ||
		catalog.RunID != req.RunID || catalog.RunSnapshotID != req.RunSnapshotID {
		return AuthorizedWorker{}, ErrTeamInteractionUnauthorized
	}
	switch req.Kind {
	case InteractionConsult:
		if req.RouteKey != "" {
			return AuthorizedWorker{}, ErrTeamInteractionUnauthorized
		}
		worker, ok := catalog.Consult[req.WorkerAgentID]
		if !ok || worker.WorkerAgentID != req.WorkerAgentID {
			return AuthorizedWorker{}, ErrTeamInteractionUnauthorized
		}
		return worker, nil
	case InteractionDispatch:
		if req.RouteKey != "" {
			return AuthorizedWorker{}, ErrTeamInteractionUnauthorized
		}
		worker, ok := catalog.Dispatch[req.WorkerAgentID]
		if !ok || worker.WorkerAgentID != req.WorkerAgentID {
			return AuthorizedWorker{}, ErrTeamInteractionUnauthorized
		}
		return worker, nil
	case InteractionHandoff:
		route, ok := catalog.Handoff[req.RouteKey]
		if !ok || route.RouteKey != req.RouteKey || route.WorkerAgentID != req.WorkerAgentID {
			return AuthorizedWorker{}, ErrTeamInteractionUnauthorized
		}
		return AuthorizedWorker{
			WorkerAgentID: route.WorkerAgentID, WorkerAgentVersion: route.WorkerAgentVersion,
			DisplayName: route.DisplayName, DefaultKind: InteractionHandoff,
		}, nil
	default:
		return AuthorizedWorker{}, ErrTeamInteractionUnauthorized
	}
}

func CatalogContentHash(catalog TeamInteractionCatalog) (string, error) {
	hash, err := frozen.HashDTO(catalogForFrozen(catalog), frozen.PreorderTeamInteractionCatalog)
	if err != nil {
		return "", codedError(CodeTeamInteractionCatalogMismatch, err)
	}
	return hash, nil
}

func ValidateCatalogContentHash(catalog TeamInteractionCatalog, expected string) error {
	actual, err := CatalogContentHash(catalog)
	if err != nil {
		return err
	}
	if len(expected) != len(actual) || subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
		return ErrTeamInteractionCatalogMismatch
	}
	return nil
}

func ValidateWorkerRoleProof(worker FrozenTeamWorker, bundle frozen.FrozenExecutionBundle) error {
	if err := validateRoleProofShape(worker.RoleProof); err != nil {
		return err
	}
	if worker.WorkerAgentID != bundle.Agent.AgentID ||
		worker.WorkerAgentVersion != bundle.Agent.AgentVersion ||
		bundle.Agent.Role != "worker" || bundle.Capability.Role != "worker" ||
		bundle.Capability.SchemaVersion != worker.RoleProof.CapabilitySchema {
		return ErrTeamWorkerRoleIncompatible
	}
	agentHash, err := frozen.HashDTO(bundle.Agent, frozen.PreorderFrozenAgentRecord)
	if err != nil {
		return codedError(CodeTeamWorkerRoleIncompatible, err)
	}
	capabilityHash, err := frozen.HashDTO(bundle.Capability, frozen.PreorderCapabilityManifest)
	if err != nil {
		return codedError(CodeTeamWorkerRoleIncompatible, err)
	}
	if !equalHash(agentHash, bundle.Capability.AgentContentHash) ||
		!equalHash(agentHash, worker.RoleProof.AgentContentHash) ||
		!equalHash(capabilityHash, worker.RoleProof.CapabilityContentHash) {
		return ErrTeamWorkerRoleIncompatible
	}
	return nil
}

func validateKinds(kinds []InteractionKind, defaultKind InteractionKind) error {
	if len(kinds) == 0 {
		return ErrTeamInteractionKindInvalid
	}
	seen := make(map[InteractionKind]struct{}, len(kinds))
	for _, kind := range kinds {
		if !validInteractionKind(kind) {
			return ErrTeamInteractionKindInvalid
		}
		if _, duplicate := seen[kind]; duplicate {
			return ErrTeamInteractionKindInvalid
		}
		seen[kind] = struct{}{}
	}
	if _, ok := seen[defaultKind]; !ok {
		return ErrTeamInteractionKindInvalid
	}
	return nil
}

func validInteractionKind(kind InteractionKind) bool {
	switch kind {
	case InteractionConsult, InteractionDispatch, InteractionHandoff:
		return true
	default:
		return false
	}
}

func canonicalKinds(kinds []InteractionKind) []InteractionKind {
	ordered := make([]InteractionKind, len(kinds))
	copy(ordered, kinds)
	sort.Slice(ordered, func(i, j int) bool {
		return kindOrder(ordered[i]) < kindOrder(ordered[j])
	})
	return ordered
}

func kindOrder(kind InteractionKind) int {
	switch kind {
	case InteractionConsult:
		return 0
	case InteractionDispatch:
		return 1
	default:
		return 2
	}
}

func authorizedWorker(worker FrozenTeamWorker) AuthorizedWorker {
	return AuthorizedWorker{
		WorkerAgentID: worker.WorkerAgentID, WorkerAgentVersion: worker.WorkerAgentVersion,
		DisplayName: worker.Name, Duty: worker.Duty, WhenToUse: worker.WhenToUse,
		ContextInstruction: worker.ContextInstruction, ResultRequirement: worker.ResultRequirement,
		DefaultKind: worker.DefaultKind,
	}
}

func catalogForFrozen(catalog TeamInteractionCatalog) frozen.TeamInteractionCatalog {
	result := frozen.TeamInteractionCatalog{
		WorkspaceID: catalog.WorkspaceID, TeamID: catalog.TeamID, RunID: catalog.RunID,
		RunSnapshotID: catalog.RunSnapshotID, LeadAgentID: catalog.LeadAgentID,
		Consult:  map[string]frozen.TeamInteractionAuthorizedWorker{},
		Dispatch: map[string]frozen.TeamInteractionAuthorizedWorker{},
		Handoff:  map[string]frozen.TeamInteractionHandoffRoute{},
	}
	for key, worker := range catalog.Consult {
		result.Consult[key] = frozenWorker(worker)
	}
	for key, worker := range catalog.Dispatch {
		result.Dispatch[key] = frozenWorker(worker)
	}
	for key, route := range catalog.Handoff {
		result.Handoff[key] = frozen.TeamInteractionHandoffRoute{
			RouteKey: route.RouteKey, WorkerAgentID: route.WorkerAgentID,
			WorkerAgentVersion: route.WorkerAgentVersion, DisplayName: route.DisplayName,
		}
	}
	return result
}

func frozenWorker(worker AuthorizedWorker) frozen.TeamInteractionAuthorizedWorker {
	return frozen.TeamInteractionAuthorizedWorker{
		WorkerAgentID: worker.WorkerAgentID, WorkerAgentVersion: worker.WorkerAgentVersion,
		DisplayName: worker.DisplayName, Duty: worker.Duty, WhenToUse: worker.WhenToUse,
		ContextInstruction: worker.ContextInstruction, ResultRequirement: worker.ResultRequirement,
		DefaultKind: string(worker.DefaultKind),
	}
}

func validateRoleProofShape(proof FrozenWorkerRoleProof) error {
	if proof.Role != "worker" || proof.CapabilitySchema != frozen.CapabilityManifestSchemaVersion ||
		!validHash(proof.AgentContentHash) || !validHash(proof.CapabilityContentHash) {
		return ErrTeamWorkerRoleIncompatible
	}
	return nil
}

func validHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, char := range value {
		if (char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') {
			continue
		}
		return false
	}
	return true
}

func equalHash(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
