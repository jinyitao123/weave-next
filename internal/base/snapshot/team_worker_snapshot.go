package snapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
)

const TeamWorkerSnapshotSchemaVersion = 1

var snapshotHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type FrozenWorkerRoleProof struct {
	Role                  string `json:"role"`
	AgentContentHash      string `json:"agent_content_hash"`
	CapabilitySchema      int    `json:"capability_schema"`
	CapabilityContentHash string `json:"capability_content_hash"`
}

// FrozenTeamWorker is the strict free-collaboration roster DTO persisted in a
// schema-v2 TeamRunSnapshot. It binds authorization facts to one exact version.
type FrozenTeamWorker struct {
	SchemaVersion      int                   `json:"schema_version"`
	WorkerAgentID      string                `json:"worker_agent_id"`
	WorkerAgentVersion int                   `json:"worker_agent_version"`
	Name               string                `json:"name"`
	Duty               string                `json:"duty"`
	WhenToUse          string                `json:"when_to_use"`
	ContextInstruction string                `json:"context_instruction"`
	AllowedKinds       []string              `json:"allowed_kinds"`
	DefaultKind        string                `json:"default_kind"`
	ResultRequirement  string                `json:"result_requirement"`
	EnabledAtSnapshot  bool                  `json:"enabled_at_snapshot"`
	RoleProof          FrozenWorkerRoleProof `json:"role_proof"`
}

func EncodeTeamWorkerSnapshot(workers []FrozenTeamWorker) (json.RawMessage, json.RawMessage, error) {
	cloned := append([]FrozenTeamWorker{}, workers...)
	sort.Slice(cloned, func(i, j int) bool { return cloned[i].WorkerAgentID < cloned[j].WorkerAgentID })
	versions := make(map[string]int, len(cloned))
	for index := range cloned {
		if err := validateFrozenTeamWorker(cloned[index]); err != nil {
			return nil, nil, err
		}
		if _, duplicate := versions[cloned[index].WorkerAgentID]; duplicate {
			return nil, nil, errors.New("duplicate worker_agent_id")
		}
		versions[cloned[index].WorkerAgentID] = cloned[index].WorkerAgentVersion
	}
	roster, err := json.Marshal(cloned)
	if err != nil {
		return nil, nil, err
	}
	workerVersions, err := json.Marshal(versions)
	if err != nil {
		return nil, nil, err
	}
	return roster, workerVersions, nil
}

func DecodeTeamWorkerSnapshot(raw json.RawMessage) ([]FrozenTeamWorker, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var workers []FrozenTeamWorker
	if err := decoder.Decode(&workers); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("team worker snapshot contains trailing JSON")
	}
	seen := make(map[string]struct{}, len(workers))
	for index := range workers {
		if err := validateFrozenTeamWorker(workers[index]); err != nil {
			return nil, fmt.Errorf("worker %d: %w", index, err)
		}
		if _, duplicate := seen[workers[index].WorkerAgentID]; duplicate {
			return nil, errors.New("duplicate worker_agent_id")
		}
		seen[workers[index].WorkerAgentID] = struct{}{}
	}
	return workers, nil
}

func validateFrozenTeamWorker(worker FrozenTeamWorker) error {
	if worker.SchemaVersion != TeamWorkerSnapshotSchemaVersion ||
		worker.WorkerAgentID == "" || worker.WorkerAgentVersion < 1 || worker.Name == "" {
		return errors.New("invalid frozen worker identity")
	}
	allowed := make(map[string]struct{}, len(worker.AllowedKinds))
	for _, kind := range worker.AllowedKinds {
		if kind != "consult" && kind != "dispatch" && kind != "handoff" {
			return fmt.Errorf("invalid interaction kind %q", kind)
		}
		if _, duplicate := allowed[kind]; duplicate {
			return fmt.Errorf("duplicate interaction kind %q", kind)
		}
		allowed[kind] = struct{}{}
	}
	if len(allowed) == 0 {
		return errors.New("allowed_kinds must not be empty")
	}
	if _, ok := allowed[worker.DefaultKind]; !ok {
		return errors.New("default_kind must belong to allowed_kinds")
	}
	proof := worker.RoleProof
	if proof.Role != "worker" || proof.CapabilitySchema < 2 ||
		!snapshotHashPattern.MatchString(proof.AgentContentHash) ||
		!snapshotHashPattern.MatchString(proof.CapabilityContentHash) {
		return errors.New("invalid frozen worker role proof")
	}
	return nil
}
