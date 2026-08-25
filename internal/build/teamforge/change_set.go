package teamforge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/build/teambuild"
)

const (
	ChangeSetSchemaVersionV1  = 1
	EmptyCreateBaselineHashV1 = "12c94376e5c16470d9d1fe0e7b50a652610d9e2c62716f3d38f7c74eac5a4f3f"
)

// ChangeOperationTypeV1 is deliberately closed. A model may produce a
// TeamBlueprintV1, but only this compiler can turn it into persistence work.
type ChangeOperationTypeV1 string

const (
	OperationAgentCreate       ChangeOperationTypeV1 = "agent_create"
	OperationAgentUpdate       ChangeOperationTypeV1 = "agent_update"
	OperationTeamCreate        ChangeOperationTypeV1 = "team_create"
	OperationTeamUpdate        ChangeOperationTypeV1 = "team_update"
	OperationRosterSet         ChangeOperationTypeV1 = "roster_set"
	OperationAgentGraphCompile ChangeOperationTypeV1 = "agent_graph_compile"
	OperationWorkflowCompile   ChangeOperationTypeV1 = "workflow_compile"
	OperationCandidateRun      ChangeOperationTypeV1 = "candidate_run"
	OperationPublish           ChangeOperationTypeV1 = "publish"
)

const (
	CompilerAgentV1         = "platform.agent.v1"
	CompilerTeamV1          = "platform.team.v1"
	CompilerRosterV1        = "platform.roster.v1"
	CompilerAgentGraphV1    = "platform.agent_graph.v1"
	CompilerWorkflowV1      = "platform.workflow_blueprint.v1"
	CompilerDeclarativeV1   = "platform.workflow_declarative.v1"
	CompilerCustomFlowV1    = "platform.workflow_custom_ref.v1"
	CompilerCandidateV1     = "platform.candidate.v1"
	CompilerPublishV1       = "platform.publish.v1"
	VerifyAgentPersisted    = "agent_version_persisted"
	VerifyTeamPersisted     = "team_version_persisted"
	VerifyRosterExact       = "roster_exact"
	VerifyAgentGraphValid   = "agent_graph_machine_valid"
	VerifyWorkflowValid     = "workflow_machine_valid"
	VerifyCandidateTerminal = "candidate_terminal_consistent"
	VerifyPublishedVersion  = "published_version_exact"
)

var operationContractV1 = map[ChangeOperationTypeV1]struct {
	compiler     string
	verification []string
}{
	OperationAgentCreate:       {CompilerAgentV1, []string{VerifyAgentPersisted}},
	OperationAgentUpdate:       {CompilerAgentV1, []string{VerifyAgentPersisted}},
	OperationTeamCreate:        {CompilerTeamV1, []string{VerifyTeamPersisted}},
	OperationTeamUpdate:        {CompilerTeamV1, []string{VerifyTeamPersisted}},
	OperationRosterSet:         {CompilerRosterV1, []string{VerifyRosterExact}},
	OperationAgentGraphCompile: {CompilerAgentGraphV1, []string{VerifyAgentGraphValid}},
	OperationWorkflowCompile:   {CompilerWorkflowV1, []string{VerifyWorkflowValid}},
	OperationCandidateRun:      {CompilerCandidateV1, []string{VerifyCandidateTerminal}},
	OperationPublish:           {CompilerPublishV1, []string{VerifyPublishedVersion}},
}

// TeamBuildBaselineV1 is the frozen, read-only current state used by the
// compiler. It contains semantic snapshots rather than mutable store handles.
type TeamBuildBaselineV1 struct {
	SchemaVersion      int                 `json:"schema_version"`
	SourceSnapshotHash string              `json:"source_snapshot_hash"`
	Mode               string              `json:"mode"`
	TeamID             string              `json:"team_id,omitempty"`
	Team               *BaselineTeamV1     `json:"team,omitempty"`
	Members            []BaselineMemberV1  `json:"members"`
	Roster             *BaselineRosterV1   `json:"roster,omitempty"`
	Workflow           *BaselineWorkflowV1 `json:"workflow,omitempty"`
}

type BaselineTeamV1 struct {
	Target  string `json:"target"`
	Version int64  `json:"version"`
	Purpose string `json:"purpose"`
	LeadRef string `json:"lead_ref"`
}

type BaselineMemberV1 struct {
	StableRef      string                      `json:"stable_ref"`
	Target         string                      `json:"target"`
	Version        int64                       `json:"version"`
	Desired        teambuild.BlueprintMemberV1 `json:"desired"`
	GraphInputHash string                      `json:"graph_input_hash,omitempty"`
}

type BaselineRosterV1 struct {
	Version    int64    `json:"version"`
	LeadRef    string   `json:"lead_ref"`
	MemberRefs []string `json:"member_refs"`
	InputHash  string   `json:"input_hash,omitempty"`
}

type BaselineWorkflowV1 struct {
	Target    string `json:"target"`
	Version   int64  `json:"version"`
	InputHash string `json:"input_hash"`
}

// EmptyCreateBaselineV1 returns the single canonical no-existing-assets
// baseline used to bind create-mode authorization.
func EmptyCreateBaselineV1() TeamBuildBaselineV1 {
	return TeamBuildBaselineV1{
		SchemaVersion:      ChangeSetSchemaVersionV1,
		SourceSnapshotHash: EmptyCreateBaselineHashV1,
		Mode:               teambuild.ModeCreate,
		Members:            []BaselineMemberV1{},
	}
}

// AuthorizedBaselineV1 projects the immutable baseline stored on an
// authorized build run into the compiler's semantic baseline. Revision
// recompilation always calls this projection instead of reading mutable live
// assets.
func AuthorizedBaselineV1(run teambuild.TeamBuildRun, blueprint teambuild.TeamBlueprintV1) (TeamBuildBaselineV1, error) {
	if run.Mode == teambuild.ModeCreate {
		if run.Baseline != nil {
			return TeamBuildBaselineV1{}, errors.New("create run must not have a baseline snapshot")
		}
		return EmptyCreateBaselineV1(), nil
	}
	if run.Mode != teambuild.ModeOptimize || run.Baseline == nil {
		return TeamBuildBaselineV1{}, errors.New("optimize run requires its authorization-bound baseline snapshot")
	}
	snapshot := run.Baseline
	if snapshot.ContentHash == "" || snapshot.Team.TeamID != blueprint.TeamID {
		return TeamBuildBaselineV1{}, errors.New("authorization-bound baseline does not match blueprint team")
	}
	pinsByName := make(map[string]teambuild.BaselineAgentPin, len(snapshot.AgentPins))
	for _, pin := range snapshot.AgentPins {
		if _, duplicate := pinsByName[pin.Name]; duplicate {
			return TeamBuildBaselineV1{}, errors.New("authorization-bound baseline has duplicate agent names")
		}
		pinsByName[pin.Name] = pin
	}
	stableByAgentID := make(map[string]string, len(blueprint.Members))
	members := make([]BaselineMemberV1, 0, len(blueprint.Members))
	for _, member := range blueprint.Members {
		pin, ok := pinsByName[member.Name]
		if !ok {
			return TeamBuildBaselineV1{}, fmt.Errorf("authorization-bound baseline has no agent pin for %q", member.Name)
		}
		if _, duplicate := stableByAgentID[pin.AgentID]; duplicate {
			return TeamBuildBaselineV1{}, errors.New("multiple blueprint members resolve to one baseline agent")
		}
		current := member
		policy, err := pin.ExecutionPolicy()
		if err != nil {
			return TeamBuildBaselineV1{}, fmt.Errorf("authorization-bound baseline agent %q: %w", pin.AgentID, err)
		}
		current.ExecutionPolicy = policy
		if model := strings.TrimSpace(pin.Model); model != "" {
			current.ModelRef = model
		}
		stableByAgentID[pin.AgentID] = member.StableRef
		members = append(members, BaselineMemberV1{
			StableRef: member.StableRef, Target: pin.AgentID, Version: int64(pin.Version), Desired: current,
		})
	}
	if len(stableByAgentID) != len(snapshot.AgentPins) {
		return TeamBuildBaselineV1{}, errors.New("blueprint members do not exactly cover authorization-bound agent pins")
	}
	leadRef, ok := stableByAgentID[snapshot.Team.LeadAvatarID]
	if !ok || leadRef != blueprint.LeadRef {
		return TeamBuildBaselineV1{}, errors.New("blueprint lead does not match authorization-bound baseline")
	}
	rosterRefs := make([]string, 0, len(snapshot.Roster))
	for _, entry := range snapshot.Roster {
		if !entry.Enabled {
			continue
		}
		stableRef, exists := stableByAgentID[entry.AgentID]
		if !exists {
			return TeamBuildBaselineV1{}, errors.New("authorization-bound roster contains an agent outside the blueprint")
		}
		rosterRefs = append(rosterRefs, stableRef)
	}
	sort.Strings(rosterRefs)
	if len(rosterRefs) != len(blueprint.Members) {
		return TeamBuildBaselineV1{}, errors.New("blueprint members do not exactly cover authorization-bound roster")
	}
	rosterInputHash, err := RosterContractHashFromSnapshotV1(snapshot.Roster, stableByAgentID)
	if err != nil {
		return TeamBuildBaselineV1{}, err
	}
	published := make([]teambuild.BaselineWorkflowRef, 0, len(snapshot.Workflows))
	for _, workflow := range snapshot.Workflows {
		if workflow.Published != nil {
			published = append(published, workflow)
		}
	}
	if len(published) > 1 {
		return TeamBuildBaselineV1{}, errors.New("authorization-bound baseline requires at most one published workflow")
	}
	var workflowBaseline *BaselineWorkflowV1
	if len(published) == 1 {
		workflow := published[0]
		workflowBaseline = &BaselineWorkflowV1{Target: workflow.WorkflowID, Version: int64(workflow.Published.Version), InputHash: workflow.Published.ContentHash}
	}
	return TeamBuildBaselineV1{
		SchemaVersion: ChangeSetSchemaVersionV1, SourceSnapshotHash: snapshot.ContentHash,
		Mode: teambuild.ModeOptimize, TeamID: snapshot.Team.TeamID,
		Team:    &BaselineTeamV1{Target: snapshot.Team.TeamID, Version: 1, Purpose: snapshot.Team.Objective, LeadRef: leadRef},
		Members: members, Roster: &BaselineRosterV1{Version: 1, LeadRef: leadRef, MemberRefs: rosterRefs, InputHash: rosterInputHash},
		Workflow: workflowBaseline,
	}, nil
}

// ChangeSetV1 is an immutable compiler output. Execution and CAS persistence
// intentionally live outside this document contract.
type ChangeSetV1 struct {
	SchemaVersion int                 `json:"schema_version"`
	ChangeSetID   string              `json:"change_set_id"`
	BaselineHash  string              `json:"baseline_hash"`
	BlueprintHash string              `json:"blueprint_hash"`
	Operations    []ChangeOperationV1 `json:"operations"`
}

type ChangeOperationV1 struct {
	OperationID     string                `json:"operation_id"`
	Type            ChangeOperationTypeV1 `json:"type"`
	Target          string                `json:"target"`
	ExpectedVersion *int64                `json:"expected_version,omitempty"`
	Input           json.RawMessage       `json:"input"`
	InputHash       string                `json:"input_hash"`
	DependsOn       []string              `json:"depends_on"`
	Compiler        string                `json:"compiler"`
	Verification    []string              `json:"verification"`
	RollbackRef     string                `json:"rollback_ref"`
}

type teamOperationInputV1 struct {
	Mode    string `json:"mode"`
	TeamID  string `json:"team_id,omitempty"`
	Name    string `json:"name,omitempty"`
	Purpose string `json:"purpose"`
	LeadRef string `json:"lead_ref"`
}

type rosterOperationInputV1 struct {
	TeamTarget         string   `json:"team_target"`
	LeadRef            string   `json:"lead_ref"`
	MemberRefs         []string `json:"member_refs"`
	RosterContractHash string   `json:"roster_contract_hash"`
}

type rosterWorkerContractV1 struct {
	StableRef          string   `json:"stable_ref"`
	Duty               string   `json:"duty"`
	WhenToUse          string   `json:"when_to_use"`
	ContextInstruction string   `json:"context_instruction"`
	AllowedKinds       []string `json:"allowed_kinds"`
	DefaultKind        string   `json:"default_kind"`
	ResultRequirement  string   `json:"result_requirement"`
}

type rosterContractV1 struct {
	Workers []rosterWorkerContractV1 `json:"workers"`
}

// RosterContractHashV1 content-addresses every worker relation field derived
// from a Blueprint. This prevents a result_requirement-only revision from
// incorrectly carrying forward an old roster_set operation.
func RosterContractHashV1(blueprint teambuild.TeamBlueprintV1) (string, error) {
	workers := make([]rosterWorkerContractV1, 0, len(blueprint.Members))
	for _, member := range blueprint.Members {
		if member.Role != teambuild.BlueprintMemberRoleWorker {
			continue
		}
		workers = append(workers, rosterWorkerContractV1{
			StableRef:          member.StableRef,
			Duty:               strings.Join(sortedNormalized(member.Responsibilities), "; "),
			WhenToUse:          "Dispatch when the team needs: " + strings.Join(sortedNormalized(member.Capabilities), ", "),
			ContextInstruction: compiledMemberPrompt(blueprint.Purpose, member),
			AllowedKinds:       []string{"consult", "dispatch"}, DefaultKind: "dispatch",
			ResultRequirement: resultRequirement(blueprint, member.StableRef),
		})
	}
	sort.Slice(workers, func(i, j int) bool { return workers[i].StableRef < workers[j].StableRef })
	return hashCanonical(rosterContractV1{Workers: workers})
}

// RosterContractHashFromSnapshotV1 projects the authorization snapshot's
// exact relation fields into the same stable-ref contract used above.
func RosterContractHashFromSnapshotV1(
	roster []teambuild.BaselineRosterEntry,
	stableByAgentID map[string]string,
) (string, error) {
	workers := make([]rosterWorkerContractV1, 0, len(roster))
	for _, entry := range roster {
		if !entry.Enabled || entry.Role != "worker" {
			continue
		}
		stableRef := strings.TrimSpace(stableByAgentID[entry.AgentID])
		if stableRef == "" {
			return "", fmt.Errorf("authorization-bound roster agent %q has no stable ref", entry.AgentID)
		}
		allowedKinds := append([]string(nil), entry.AllowedKinds...)
		sort.Strings(allowedKinds)
		workers = append(workers, rosterWorkerContractV1{
			StableRef: stableRef, Duty: entry.Duty, WhenToUse: entry.WhenToUse,
			ContextInstruction: entry.ContextInstruction, AllowedKinds: allowedKinds,
			DefaultKind: entry.DefaultKind, ResultRequirement: entry.ResultRequirement,
		})
	}
	sort.Slice(workers, func(i, j int) bool { return workers[i].StableRef < workers[j].StableRef })
	return hashCanonical(rosterContractV1{Workers: workers})
}

type agentOperationInputV1 struct {
	StableRef string                      `json:"stable_ref"`
	Desired   teambuild.BlueprintMemberV1 `json:"desired"`
}

type agentGraphOperationInputV1 struct {
	StableRef        string `json:"stable_ref"`
	InternalGraphRef string `json:"internal_graph_ref"`
}

type workflowTemplateOperationInputV1 struct {
	Mode         string                     `json:"mode"`
	Blueprint    workflowLogicalBlueprintV1 `json:"blueprint"`
	CompiledHash string                     `json:"compiled_hash"`
}

type workflowLogicalBlueprintV1 struct {
	Template        WorkflowBlueprintTemplate `json:"template"`
	LeadInstruction string                    `json:"lead_instruction"`
	PrimaryRef      string                    `json:"primary_ref,omitempty"`
	ReviewerRef     string                    `json:"reviewer_ref,omitempty"`
	ParallelRefs    []string                  `json:"parallel_refs,omitempty"`
	FinalizerRef    string                    `json:"finalizer_ref,omitempty"`
	MaxIterations   *int64                    `json:"max_iterations,omitempty"`
	Requirements    map[string]string         `json:"requirements"`
}

// BindWorkflowTemplateOperationV1 resolves stable Blueprint member refs to
// persisted Agent IDs/versions immediately before workflow persistence.
func BindWorkflowTemplateOperationV1(raw json.RawMessage, resolve func(string) (string, int64, error)) (WorkflowBlueprint, string, error) {
	var input workflowTemplateOperationInputV1
	if err := json.Unmarshal(raw, &input); err != nil {
		return WorkflowBlueprint{}, "", err
	}
	if input.Mode != teambuild.BlueprintWorkflowTemplate || resolve == nil {
		return WorkflowBlueprint{}, "", errors.New("workflow template operation is invalid")
	}
	bind := func(ref string) (*WorkflowBlueprintWorker, error) {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return nil, nil
		}
		id, version, err := resolve(ref)
		if err != nil {
			return nil, err
		}
		return &WorkflowBlueprintWorker{AgentID: id, AgentVersion: version, ResultRequirement: input.Blueprint.Requirements[ref]}, nil
	}
	blueprint := WorkflowBlueprint{Template: input.Blueprint.Template, LeadInstruction: input.Blueprint.LeadInstruction, MaxIterations: input.Blueprint.MaxIterations}
	var err error
	if blueprint.Primary, err = bind(input.Blueprint.PrimaryRef); err != nil {
		return WorkflowBlueprint{}, "", err
	}
	if blueprint.Reviewer, err = bind(input.Blueprint.ReviewerRef); err != nil {
		return WorkflowBlueprint{}, "", err
	}
	for _, ref := range input.Blueprint.ParallelRefs {
		worker, bindErr := bind(ref)
		if bindErr != nil {
			return WorkflowBlueprint{}, "", bindErr
		}
		blueprint.ParallelWorkers = append(blueprint.ParallelWorkers, *worker)
	}
	if blueprint.Finalizer, err = bind(input.Blueprint.FinalizerRef); err != nil {
		return WorkflowBlueprint{}, "", err
	}
	compiled, problems := CompileWorkflowBlueprint(blueprint)
	if len(problems) != 0 {
		return WorkflowBlueprint{}, "", fmt.Errorf("late-bound workflow compilation failed: %+v", problems)
	}
	hash, err := hashCanonical(struct {
		Trigger any `json:"trigger"`
		Graph   any `json:"graph"`
	}{compiled.Trigger, compiled.Graph})
	return blueprint, hash, err
}

type workflowCustomOperationInputV1 struct {
	Mode          string `json:"mode"`
	CustomSpecRef string `json:"custom_spec_ref"`
	AuthorityRef  string `json:"authority_ref"`
	ScopeHash     string `json:"scope_hash"`
}

type workflowDeclarativeOperationInputV1 struct {
	Mode           string                          `json:"mode"`
	SourceSpecHash string                          `json:"source_spec_hash,omitempty"`
	SpecHash       string                          `json:"spec_hash"`
	CompiledHash   string                          `json:"compiled_hash"`
	FrozenSpec     FrozenDeclarativeWorkflowSpecV1 `json:"frozen_spec"`
}

type candidateOperationInputV1 struct {
	TeamTarget string `json:"team_target"`
}

type publishOperationInputV1 struct {
	TeamTarget string `json:"team_target"`
}

// CompileChangeSetV1 deterministically diffs a frozen baseline against one
// valid template/custom blueprint. Declarative graph specs must first pass the
// platform-owned freeze path and use CompileDeclarativeChangeSetV1.
func CompileChangeSetV1(baseline TeamBuildBaselineV1, blueprint teambuild.TeamBlueprintV1) (ChangeSetV1, error) {
	if blueprint.Workflow.Mode == teambuild.BlueprintWorkflowDeclarativeV1 {
		return ChangeSetV1{}, errors.New("declarative_v1 workflow requires CompileDeclarativeChangeSetV1 with a frozen spec")
	}
	return compileChangeSetV1(baseline, blueprint, nil, true)
}

// CompileTemplateInstantiateChangeSetV1 produces only deterministic asset
// materialization operations. Candidate evaluation and publication remain the
// unchanged compiler_v1 path used by a later explicit evaluation build.
func CompileTemplateInstantiateChangeSetV1(
	baseline TeamBuildBaselineV1,
	blueprint teambuild.TeamBlueprintV1,
) (ChangeSetV1, error) {
	if blueprint.Mode != teambuild.ModeCreate {
		return ChangeSetV1{}, errors.New("template_instantiate ChangeSet requires create mode")
	}
	if blueprint.Workflow.Mode != teambuild.BlueprintWorkflowTemplate {
		return ChangeSetV1{}, errors.New("template_instantiate initially requires a built-in template workflow")
	}
	return compileChangeSetV1(baseline, blueprint, nil, false)
}

// CompileTemplateInstantiateDeclarativeChangeSetV1 compiles a frozen
// declarative_v1 graph for the template fast path. It preserves the ordinary
// declarative freeze and BuildRun bindings while omitting only the deferred
// candidate and publish operations.
func CompileTemplateInstantiateDeclarativeChangeSetV1(
	baseline TeamBuildBaselineV1,
	blueprint teambuild.TeamBlueprintV1,
	frozen FrozenDeclarativeWorkflowSpecV1,
) (ChangeSetV1, error) {
	if blueprint.Mode != teambuild.ModeCreate {
		return ChangeSetV1{}, errors.New("template_instantiate declarative ChangeSet requires create mode")
	}
	if blueprint.Workflow.Mode != teambuild.BlueprintWorkflowDeclarativeV1 {
		return ChangeSetV1{}, errors.New("template_instantiate declarative ChangeSet requires declarative_v1 workflow mode")
	}
	if err := validateFrozenDeclarativeWorkflowSpecV1(frozen); err != nil {
		return ChangeSetV1{}, err
	}
	if frozen.SpecHash != strings.TrimSpace(blueprint.Workflow.DeclarativeSpecHash) {
		return ChangeSetV1{}, errors.New("frozen declarative_v1 spec hash does not match Blueprint")
	}
	if frozen.BuildBinding.BaselineHash != strings.TrimSpace(baseline.SourceSnapshotHash) {
		return ChangeSetV1{}, errors.New("frozen declarative_v1 spec baseline does not match ChangeSet baseline")
	}
	return compileChangeSetV1(baseline, blueprint, &frozen, false)
}

// CompileDeclarativeChangeSetV1 compiles a platform-frozen declarative_v1
// graph into the ordinary workflow_compile ChangeSet operation. It does not
// connect the phase-2 executor/materializer.
func CompileDeclarativeChangeSetV1(
	baseline TeamBuildBaselineV1,
	blueprint teambuild.TeamBlueprintV1,
	frozen FrozenDeclarativeWorkflowSpecV1,
) (ChangeSetV1, error) {
	if blueprint.Workflow.Mode != teambuild.BlueprintWorkflowDeclarativeV1 {
		return ChangeSetV1{}, errors.New("declarative ChangeSet requires workflow mode declarative_v1")
	}
	if err := validateFrozenDeclarativeWorkflowSpecV1(frozen); err != nil {
		return ChangeSetV1{}, err
	}
	if frozen.SpecHash != strings.TrimSpace(blueprint.Workflow.DeclarativeSpecHash) {
		return ChangeSetV1{}, errors.New("frozen declarative_v1 spec hash does not match Blueprint")
	}
	if frozen.BuildBinding.BaselineHash != strings.TrimSpace(baseline.SourceSnapshotHash) {
		return ChangeSetV1{}, errors.New("frozen declarative_v1 spec baseline does not match ChangeSet baseline")
	}
	return compileChangeSetV1(baseline, blueprint, &frozen, true)
}

func compileChangeSetV1(
	baseline TeamBuildBaselineV1,
	blueprint teambuild.TeamBlueprintV1,
	declarative *FrozenDeclarativeWorkflowSpecV1,
	includeCandidatePublish bool,
) (ChangeSetV1, error) {
	if err := teambuild.ValidateTeamBlueprintV1(blueprint); err != nil {
		return ChangeSetV1{}, err
	}
	if err := validateBaselineIdentityV1(baseline, blueprint); err != nil {
		return ChangeSetV1{}, err
	}
	baselineHash := strings.TrimSpace(baseline.SourceSnapshotHash)
	blueprintHash, err := blueprint.BlueprintHash()
	if err != nil {
		return ChangeSetV1{}, err
	}

	compiler := changeSetCompilerV1{
		baseline:                baseline,
		blueprint:               blueprint,
		declarative:             declarative,
		includeCandidatePublish: includeCandidatePublish,
		memberVersions:          make(map[string]int64, len(blueprint.Members)),
		memberTargets:           make(map[string]string, len(blueprint.Members)),
		memberMutated:           make(map[string]bool, len(blueprint.Members)),
	}
	if err := compiler.compile(); err != nil {
		return ChangeSetV1{}, err
	}
	changeSet := ChangeSetV1{
		SchemaVersion: ChangeSetSchemaVersionV1,
		BaselineHash:  baselineHash,
		BlueprintHash: blueprintHash,
		Operations:    compiler.operations,
	}
	id, err := contentAddressChangeSetV1(changeSet)
	if err != nil {
		return ChangeSetV1{}, err
	}
	changeSet.ChangeSetID = id
	var validateErr error
	if includeCandidatePublish {
		validateErr = ValidateChangeSetV1(changeSet)
	} else {
		validateErr = ValidateTemplateInstantiateChangeSetV1(changeSet)
	}
	if validateErr != nil {
		return ChangeSetV1{}, fmt.Errorf("compiled change set invalid: %w", validateErr)
	}
	return changeSet, nil
}

type changeSetCompilerV1 struct {
	baseline                TeamBuildBaselineV1
	blueprint               teambuild.TeamBlueprintV1
	declarative             *FrozenDeclarativeWorkflowSpecV1
	includeCandidatePublish bool
	operations              []ChangeOperationV1
	memberVersions          map[string]int64
	memberTargets           map[string]string
	memberMutated           map[string]bool
}

func (c *changeSetCompilerV1) compile() error {
	baselineMembers := make(map[string]BaselineMemberV1, len(c.baseline.Members))
	for _, member := range c.baseline.Members {
		baselineMembers[strings.TrimSpace(member.StableRef)] = member
	}
	members := append([]teambuild.BlueprintMemberV1(nil), c.blueprint.Members...)
	sort.Slice(members, func(i, j int) bool {
		return strings.TrimSpace(members[i].StableRef) < strings.TrimSpace(members[j].StableRef)
	})
	memberMutationIDs := make([]string, 0, len(members))
	for _, member := range members {
		stableRef := strings.TrimSpace(member.StableRef)
		current, exists := baselineMembers[stableRef]
		if member.ManagementMode == teambuild.BlueprintManagementPreserveExisting && !exists {
			return fmt.Errorf("preserve_existing member %q requires a frozen baseline entry", stableRef)
		}
		operationTarget := strings.TrimSpace(member.Name)
		resolvedTarget := operationTarget
		version := int64(1)
		var mutationID string
		if member.ManagementMode == teambuild.BlueprintManagementPreserveExisting {
			resolvedTarget = strings.TrimSpace(current.Target)
			version = current.Version
		} else if exists {
			resolvedTarget = strings.TrimSpace(current.Target)
			version = current.Version
			equal, err := semanticEqual(normalizeChangeMember(current.Desired), normalizeChangeMember(member))
			if err != nil {
				return err
			}
			if !equal {
				op, err := newChangeOperationV1(OperationAgentUpdate, operationTarget, int64Ptr(current.Version),
					agentOperationInputV1{StableRef: stableRef, Desired: normalizeChangeMember(member)}, nil, "version:"+fmt.Sprint(current.Version))
				if err != nil {
					return err
				}
				c.operations = append(c.operations, op)
				mutationID = op.OperationID
				version = current.Version + 1
			}
		} else {
			op, err := newChangeOperationV1(OperationAgentCreate, operationTarget, nil,
				agentOperationInputV1{StableRef: stableRef, Desired: normalizeChangeMember(member)}, nil, "delete:"+operationTarget)
			if err != nil {
				return err
			}
			c.operations = append(c.operations, op)
			mutationID = op.OperationID
		}
		if mutationID != "" {
			memberMutationIDs = append(memberMutationIDs, mutationID)
			c.memberMutated[stableRef] = true
		}
		if member.ManagementMode == teambuild.BlueprintManagementManaged && member.ExecutionPolicy.ExecutionMode == teambuild.BlueprintExecutionInternalGraph {
			graphInput := agentGraphOperationInputV1{StableRef: stableRef, InternalGraphRef: strings.TrimSpace(member.ExecutionPolicy.InternalGraphRef)}
			graphHash, _, err := canonicalInput(graphInput)
			if err != nil {
				return err
			}
			if !exists || current.GraphInputHash != graphHash {
				dependencies := nonEmptyStrings(mutationID)
				op, err := newChangeOperationV1(OperationAgentGraphCompile, operationTarget, int64Ptr(version), graphInput, dependencies, "agent-version:"+fmt.Sprint(version))
				if err != nil {
					return err
				}
				c.operations = append(c.operations, op)
				memberMutationIDs = append(memberMutationIDs, op.OperationID)
				c.memberMutated[stableRef] = true
				version++
			}
		}
		c.memberTargets[stableRef] = resolvedTarget
		c.memberVersions[stableRef] = version
	}

	teamTarget := normalizeChangeText(c.blueprint.NewTeamName)
	if c.blueprint.Mode == teambuild.ModeOptimize {
		teamTarget = strings.TrimSpace(c.blueprint.TeamID)
	}
	teamVersion := int64(1)
	var teamMutationID string
	teamInput := teamOperationInputV1{
		Mode: c.blueprint.Mode, TeamID: strings.TrimSpace(c.blueprint.TeamID), Name: normalizeChangeText(c.blueprint.NewTeamName),
		Purpose: normalizeChangeText(c.blueprint.Purpose), LeadRef: strings.TrimSpace(c.blueprint.LeadRef),
	}
	if c.baseline.Team == nil {
		op, err := newChangeOperationV1(OperationTeamCreate, teamTarget, nil, teamInput, memberMutationIDs, "delete:"+teamTarget)
		if err != nil {
			return err
		}
		c.operations = append(c.operations, op)
		teamMutationID = op.OperationID
	} else {
		teamTarget = strings.TrimSpace(c.baseline.Team.Target)
		teamVersion = c.baseline.Team.Version
		currentTeam := teamOperationInputV1{
			Mode: c.blueprint.Mode, TeamID: strings.TrimSpace(c.blueprint.TeamID),
			Purpose: normalizeChangeText(c.baseline.Team.Purpose), LeadRef: strings.TrimSpace(c.baseline.Team.LeadRef),
		}
		equal, err := semanticEqual(currentTeam, teamInput)
		if err != nil {
			return err
		}
		if !equal {
			op, err := newChangeOperationV1(OperationTeamUpdate, teamTarget, int64Ptr(teamVersion), teamInput, memberMutationIDs, "version:"+fmt.Sprint(teamVersion))
			if err != nil {
				return err
			}
			c.operations = append(c.operations, op)
			teamMutationID = op.OperationID
			teamVersion++
		}
	}

	desiredRefs := make([]string, 0, len(members))
	for _, member := range members {
		desiredRefs = append(desiredRefs, strings.TrimSpace(member.StableRef))
	}
	rosterContractHash, err := RosterContractHashV1(c.blueprint)
	if err != nil {
		return err
	}
	rosterChanged := c.baseline.Roster == nil || strings.TrimSpace(c.baseline.Roster.LeadRef) != strings.TrimSpace(c.blueprint.LeadRef) ||
		!equalSortedStrings(c.baseline.Roster.MemberRefs, desiredRefs) || c.baseline.Roster.InputHash != rosterContractHash
	var rosterMutationID string
	if rosterChanged {
		dependencies := append([]string(nil), memberMutationIDs...)
		if teamMutationID != "" {
			dependencies = append(dependencies, teamMutationID)
		}
		var expected *int64
		rollback := "roster:none"
		if c.baseline.Roster != nil {
			expected = int64Ptr(c.baseline.Roster.Version)
			rollback = "roster-version:" + fmt.Sprint(c.baseline.Roster.Version)
		}
		op, err := newChangeOperationV1(OperationRosterSet, teamTarget, expected,
			rosterOperationInputV1{TeamTarget: teamTarget, LeadRef: strings.TrimSpace(c.blueprint.LeadRef), MemberRefs: desiredRefs, RosterContractHash: rosterContractHash},
			dependencies, rollback)
		if err != nil {
			return err
		}
		c.operations = append(c.operations, op)
		rosterMutationID = op.OperationID
	}

	workflowInput, compilerName, workflowHash, err := c.compileWorkflowInput()
	if err != nil {
		return err
	}
	workflowTarget := normalizeChangeText(c.blueprint.NewTeamName) + "-workflow"
	if c.blueprint.Mode == teambuild.ModeOptimize {
		workflowTarget = teambuild.FirstOptimizeWorkflowID(c.blueprint.TeamID)
	}
	workflowChanged := c.baseline.Workflow == nil || c.baseline.Workflow.InputHash != workflowHash
	if c.baseline.Workflow != nil {
		workflowTarget = strings.TrimSpace(c.baseline.Workflow.Target)
	}
	if workflowChanged {
		dependencies := append([]string(nil), memberMutationIDs...)
		dependencies = append(dependencies, nonEmptyStrings(rosterMutationID, teamMutationID)...)
		dependencies = uniqueStrings(dependencies)
		var expected *int64
		rollback := "workflow:none"
		if c.baseline.Workflow != nil {
			expected = int64Ptr(c.baseline.Workflow.Version)
			rollback = "workflow-version:" + fmt.Sprint(c.baseline.Workflow.Version)
		}
		op, err := newChangeOperationWithCompilerV1(OperationWorkflowCompile, workflowTarget, expected, workflowInput, dependencies, rollback, compilerName)
		if err != nil {
			return err
		}
		c.operations = append(c.operations, op)
	}
	if !c.includeCandidatePublish {
		return nil
	}

	changeIDs := make([]string, 0, len(c.operations))
	for _, operation := range c.operations {
		changeIDs = append(changeIDs, operation.OperationID)
	}
	candidate, err := newChangeOperationV1(OperationCandidateRun, "candidate/"+teamIdentitySegment(c.blueprint), nil,
		candidateOperationInputV1{TeamTarget: teamTarget}, changeIDs, "none")
	if err != nil {
		return err
	}
	c.operations = append(c.operations, candidate)
	publish, err := newChangeOperationV1(OperationPublish, teamTarget, int64Ptr(teamVersion),
		publishOperationInputV1{TeamTarget: teamTarget}, []string{candidate.OperationID}, "team-version:"+fmt.Sprint(teamVersion))
	if err != nil {
		return err
	}
	c.operations = append(c.operations, publish)
	return nil
}

func (c *changeSetCompilerV1) compileWorkflowInput() (any, string, string, error) {
	workflow := c.blueprint.Workflow
	if workflow.Mode == teambuild.BlueprintWorkflowDeclarativeV1 {
		if c.declarative == nil {
			return nil, "", "", errors.New("declarative_v1 workflow is missing its frozen spec")
		}
		frozen, err := c.rebindDeclarativeWorkerVersions()
		if err != nil {
			return nil, "", "", err
		}
		compiledHash, err := hashCanonical(struct {
			Trigger json.RawMessage `json:"trigger_config"`
			Graph   json.RawMessage `json:"graph_definition"`
		}{frozen.TriggerConfig, frozen.GraphDefinition})
		if err != nil {
			return nil, "", "", err
		}
		input := workflowDeclarativeOperationInputV1{
			Mode:           teambuild.BlueprintWorkflowDeclarativeV1,
			SourceSpecHash: c.declarative.SpecHash,
			SpecHash:       frozen.SpecHash, CompiledHash: compiledHash,
			FrozenSpec: frozen,
		}
		workflowHash, _, err := canonicalInput(input)
		return input, CompilerDeclarativeV1, workflowHash, err
	}
	if workflow.Mode == teambuild.BlueprintWorkflowCustom {
		fact := workflow.TemplateGapAuthorization
		input := workflowCustomOperationInputV1{
			Mode: teambuild.BlueprintWorkflowCustom, CustomSpecRef: strings.TrimSpace(workflow.CustomSpecRef),
			AuthorityRef: strings.TrimSpace(fact.AuthorityRef), ScopeHash: strings.TrimSpace(fact.ScopeHash),
		}
		hash, _, err := canonicalInput(input)
		return input, CompilerCustomFlowV1, hash, err
	}
	params := workflow.TemplateParameters
	resolve := func(ref string) WorkflowBlueprintWorker {
		ref = strings.TrimSpace(ref)
		return WorkflowBlueprintWorker{
			AgentID: ref, AgentVersion: 1,
			ResultRequirement: params.ResultRequirements[ref],
		}
	}
	compiledInput := WorkflowBlueprint{
		Template: WorkflowBlueprintTemplate(workflow.Template), LeadInstruction: params.LeadInstruction,
	}
	if strings.TrimSpace(params.PrimaryRef) != "" {
		value := resolve(params.PrimaryRef)
		compiledInput.Primary = &value
	}
	if strings.TrimSpace(params.ReviewerRef) != "" {
		value := resolve(params.ReviewerRef)
		compiledInput.Reviewer = &value
	}
	for _, ref := range params.ParallelWorkerRefs {
		compiledInput.ParallelWorkers = append(compiledInput.ParallelWorkers, resolve(ref))
	}
	if strings.TrimSpace(params.FinalizerRef) != "" {
		value := resolve(params.FinalizerRef)
		compiledInput.Finalizer = &value
	}
	if params.MaxIterations != nil {
		value := int64(*params.MaxIterations)
		compiledInput.MaxIterations = &value
	}
	compiled, problems := CompileWorkflowBlueprint(compiledInput)
	if len(problems) != 0 {
		return nil, "", "", fmt.Errorf("workflow blueprint compilation failed: %+v", problems)
	}
	compiledHash, err := hashCanonical(struct {
		Trigger any `json:"trigger"`
		Graph   any `json:"graph"`
	}{compiled.Trigger, compiled.Graph})
	if err != nil {
		return nil, "", "", err
	}
	logical := workflowLogicalBlueprintV1{
		Template: WorkflowBlueprintTemplate(workflow.Template), LeadInstruction: params.LeadInstruction,
		PrimaryRef: strings.TrimSpace(params.PrimaryRef), ReviewerRef: strings.TrimSpace(params.ReviewerRef),
		ParallelRefs: sortedTrimmed(params.ParallelWorkerRefs), FinalizerRef: strings.TrimSpace(params.FinalizerRef),
		MaxIterations: compiledInput.MaxIterations, Requirements: params.ResultRequirements,
	}
	input := workflowTemplateOperationInputV1{Mode: teambuild.BlueprintWorkflowTemplate, Blueprint: logical, CompiledHash: compiledHash}
	workflowHash, _, err := canonicalInput(input)
	return input, CompilerWorkflowV1, workflowHash, err
}

func (c *changeSetCompilerV1) rebindDeclarativeWorkerVersions() (FrozenDeclarativeWorkflowSpecV1, error) {
	frozen := *c.declarative
	frozen.WorkerBindings = append([]DeclarativeWorkerBindingV1(nil), c.declarative.WorkerBindings...)
	changed := false
	for index := range frozen.WorkerBindings {
		binding := &frozen.WorkerBindings[index]
		if !c.memberMutated[binding.StableRef] {
			continue
		}
		target, targetOK := c.memberTargets[binding.StableRef]
		version, versionOK := c.memberVersions[binding.StableRef]
		if !targetOK || !versionOK || strings.TrimSpace(target) == "" || version < 1 {
			return FrozenDeclarativeWorkflowSpecV1{}, fmt.Errorf("declarative worker %q has no compiled AgentVersion", binding.StableRef)
		}
		binding.AgentID = target
		binding.AgentVersion = version
		changed = true
	}
	if !changed {
		return frozen, nil
	}
	compiled, err := CompileDeclarativeWorkflowSpecV1(frozen.Spec, frozen.WorkerBindings)
	if err != nil {
		return FrozenDeclarativeWorkflowSpecV1{}, fmt.Errorf("rebind declarative worker versions: %w", err)
	}
	frozen.TriggerConfig, frozen.GraphDefinition, err = encodeWorkflowDraftJSON(compiled.Trigger, compiled.Graph)
	if err != nil {
		return FrozenDeclarativeWorkflowSpecV1{}, fmt.Errorf("encode rebound declarative workflow: %w", err)
	}
	frozen.SpecHash, err = frozenDeclarativeSpecHashV1(frozen)
	if err != nil {
		return FrozenDeclarativeWorkflowSpecV1{}, fmt.Errorf("hash rebound declarative workflow: %w", err)
	}
	if err := validateFrozenDeclarativeWorkflowSpecV1(frozen); err != nil {
		return FrozenDeclarativeWorkflowSpecV1{}, err
	}
	return frozen, nil
}

func validateBaselineIdentityV1(baseline TeamBuildBaselineV1, blueprint teambuild.TeamBlueprintV1) error {
	if baseline.SchemaVersion != ChangeSetSchemaVersionV1 {
		return errors.New("baseline schema_version must be 1")
	}
	if strings.TrimSpace(baseline.Mode) != strings.TrimSpace(blueprint.Mode) {
		return errors.New("baseline mode does not match blueprint mode")
	}
	switch blueprint.Mode {
	case teambuild.ModeCreate:
		if baseline.SourceSnapshotHash != EmptyCreateBaselineHashV1 {
			return errors.New("create baseline must bind the canonical empty snapshot hash")
		}
		if strings.TrimSpace(baseline.TeamID) != "" || baseline.Team != nil || baseline.Roster != nil || baseline.Workflow != nil || len(baseline.Members) != 0 {
			return errors.New("create baseline must not claim existing assets")
		}
	case teambuild.ModeOptimize:
		if !isSHA256(baseline.SourceSnapshotHash) {
			return errors.New("optimize baseline requires the official source snapshot hash")
		}
		if strings.TrimSpace(baseline.TeamID) == "" || strings.TrimSpace(baseline.TeamID) != strings.TrimSpace(blueprint.TeamID) {
			return errors.New("optimize baseline team_id must match blueprint team_id")
		}
		if baseline.Team == nil {
			return errors.New("optimize baseline requires the target team snapshot")
		}
	}
	seen := make(map[string]bool, len(baseline.Members))
	for _, member := range baseline.Members {
		ref := strings.TrimSpace(member.StableRef)
		if ref == "" || seen[ref] {
			return errors.New("baseline member stable_ref must be non-blank and unique")
		}
		seen[ref] = true
		if strings.TrimSpace(member.Target) == "" || member.Version < 1 {
			return fmt.Errorf("baseline member %s requires target and positive version", ref)
		}
		if strings.TrimSpace(member.Desired.StableRef) != ref {
			return fmt.Errorf("baseline member %s desired stable_ref mismatch", ref)
		}
		if member.GraphInputHash != "" && !isSHA256(member.GraphInputHash) {
			return fmt.Errorf("baseline member %s graph_input_hash must be canonical sha256", ref)
		}
	}
	if baseline.Team != nil && (strings.TrimSpace(baseline.Team.Target) == "" || baseline.Team.Version < 1) {
		return errors.New("baseline team requires target and positive version")
	}
	if baseline.Roster != nil && baseline.Roster.Version < 1 {
		return errors.New("baseline roster requires positive version")
	}
	if baseline.Roster != nil && baseline.Roster.InputHash != "" && !isSHA256(baseline.Roster.InputHash) {
		return errors.New("baseline roster input_hash must be canonical sha256")
	}
	if baseline.Workflow != nil {
		if strings.TrimSpace(baseline.Workflow.Target) == "" || baseline.Workflow.Version < 1 || !isSHA256(baseline.Workflow.InputHash) {
			return errors.New("baseline workflow requires target, positive version, and canonical input_hash")
		}
	}
	return nil
}

// CanonicalHash returns the content hash used to bind the ChangeSet to its
// frozen baseline. Order-insensitive baseline collections are normalized.
func (baseline TeamBuildBaselineV1) CanonicalHash() (string, error) {
	normalized := baseline
	normalized.SourceSnapshotHash = strings.TrimSpace(baseline.SourceSnapshotHash)
	normalized.Mode = strings.TrimSpace(baseline.Mode)
	normalized.TeamID = strings.TrimSpace(baseline.TeamID)
	if baseline.Team != nil {
		team := *baseline.Team
		team.Target = strings.TrimSpace(team.Target)
		team.Purpose = normalizeChangeText(team.Purpose)
		team.LeadRef = strings.TrimSpace(team.LeadRef)
		normalized.Team = &team
	}
	normalized.Members = make([]BaselineMemberV1, len(baseline.Members))
	copy(normalized.Members, baseline.Members)
	for index := range normalized.Members {
		normalized.Members[index].StableRef = strings.TrimSpace(normalized.Members[index].StableRef)
		normalized.Members[index].Target = strings.TrimSpace(normalized.Members[index].Target)
		normalized.Members[index].Desired = normalizeChangeMember(normalized.Members[index].Desired)
		normalized.Members[index].GraphInputHash = strings.TrimSpace(normalized.Members[index].GraphInputHash)
	}
	sort.Slice(normalized.Members, func(i, j int) bool { return normalized.Members[i].StableRef < normalized.Members[j].StableRef })
	if baseline.Roster != nil {
		roster := *baseline.Roster
		roster.LeadRef = strings.TrimSpace(roster.LeadRef)
		roster.MemberRefs = sortedTrimmed(roster.MemberRefs)
		normalized.Roster = &roster
	}
	if baseline.Workflow != nil {
		workflow := *baseline.Workflow
		workflow.Target = strings.TrimSpace(workflow.Target)
		workflow.InputHash = strings.TrimSpace(workflow.InputHash)
		normalized.Workflow = &workflow
	}
	return hashCanonical(normalized)
}

// CanonicalBytes returns the stable JSON representation of a valid change
// set. Operations retain deterministic topological compiler order.
func (changeSet ChangeSetV1) CanonicalBytes() ([]byte, error) {
	return changeSet.canonicalBytes(ValidateChangeSetV1)
}

// TemplateInstantiateCanonicalBytes returns the stable representation of a
// materialization-only ChangeSet.
func (changeSet ChangeSetV1) TemplateInstantiateCanonicalBytes() ([]byte, error) {
	return changeSet.canonicalBytes(ValidateTemplateInstantiateChangeSetV1)
}

func (changeSet ChangeSetV1) canonicalBytes(validate func(ChangeSetV1) error) ([]byte, error) {
	if err := validate(changeSet); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(changeSet)
	if err != nil {
		return nil, err
	}
	return frozen.CanonicalizeJSON(raw)
}

func (changeSet ChangeSetV1) CanonicalHash() (string, error) {
	bytes, err := changeSet.CanonicalBytes()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:]), nil
}

// TemplateInstantiateCanonicalHash hashes the complete materialization-only
// ChangeSet document for blueprint revision binding.
func (changeSet ChangeSetV1) TemplateInstantiateCanonicalHash() (string, error) {
	bytes, err := changeSet.TemplateInstantiateCanonicalBytes()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:]), nil
}

func ValidateChangeSetV1(changeSet ChangeSetV1) error {
	return validateChangeSetV1(changeSet, true)
}

// ValidateTemplateInstantiateChangeSetV1 accepts the same closed operation
// contracts and DAG rules while requiring candidate_run and publish to be
// completely absent.
func ValidateTemplateInstantiateChangeSetV1(changeSet ChangeSetV1) error {
	return validateChangeSetV1(changeSet, false)
}

func validateChangeSetV1(changeSet ChangeSetV1, requireCandidatePublish bool) error {
	if changeSet.SchemaVersion != ChangeSetSchemaVersionV1 {
		return errors.New("change set schema_version must be 1")
	}
	if !isSHA256(changeSet.ChangeSetID) || !isSHA256(changeSet.BaselineHash) || !isSHA256(changeSet.BlueprintHash) {
		return errors.New("change_set_id, baseline_hash, and blueprint_hash must be canonical sha256 values")
	}
	if len(changeSet.Operations) == 0 {
		return errors.New("change set operations are required")
	}
	byID := make(map[string]int, len(changeSet.Operations))
	for index, operation := range changeSet.Operations {
		if strings.TrimSpace(operation.OperationID) == "" {
			return fmt.Errorf("operation %d has blank operation_id", index)
		}
		if _, exists := byID[operation.OperationID]; exists {
			return fmt.Errorf("duplicate operation_id %q", operation.OperationID)
		}
		byID[operation.OperationID] = index
		contract, known := operationContractV1[operation.Type]
		if !known {
			return fmt.Errorf("operation %s has unknown type %q", operation.OperationID, operation.Type)
		}
		expectedCompiler := contract.compiler
		if operation.Type == OperationWorkflowCompile &&
			(operation.Compiler == CompilerCustomFlowV1 || operation.Compiler == CompilerDeclarativeV1) {
			expectedCompiler = operation.Compiler
		}
		if operation.Compiler != expectedCompiler {
			return fmt.Errorf("operation %s compiler is not allowed for type %s", operation.OperationID, operation.Type)
		}
		if !equalStringSlices(operation.Verification, contract.verification) {
			return fmt.Errorf("operation %s verification is not the platform contract", operation.OperationID)
		}
		if strings.TrimSpace(operation.Target) == "" || !isSHA256(operation.InputHash) || strings.TrimSpace(operation.RollbackRef) == "" {
			return fmt.Errorf("operation %s requires target, canonical input_hash, and rollback_ref", operation.OperationID)
		}
		if len(operation.Input) == 0 || !json.Valid(operation.Input) {
			return fmt.Errorf("operation %s input must be valid JSON", operation.OperationID)
		}
		canonicalHash, canonicalBytes, err := canonicalRawJSON(operation.Input)
		if err != nil || canonicalHash != operation.InputHash || !bytesEqual(canonicalBytes, operation.Input) {
			return fmt.Errorf("operation %s input is not canonical or does not match input_hash", operation.OperationID)
		}
		expectedID, err := contentAddressOperationV1(operation)
		if err != nil || expectedID != operation.OperationID {
			return fmt.Errorf("operation %s is not content-addressed", operation.OperationID)
		}
	}
	for index, operation := range changeSet.Operations {
		seenDeps := map[string]bool{}
		for _, dependency := range operation.DependsOn {
			if dependency == operation.OperationID {
				return fmt.Errorf("operation %s depends on itself", operation.OperationID)
			}
			depIndex, exists := byID[dependency]
			if !exists {
				return fmt.Errorf("operation %s has missing dependency %s", operation.OperationID, dependency)
			}
			if depIndex >= index {
				return fmt.Errorf("operation %s dependency %s is not earlier in stable topological order", operation.OperationID, dependency)
			}
			if seenDeps[dependency] {
				return fmt.Errorf("operation %s duplicates dependency %s", operation.OperationID, dependency)
			}
			seenDeps[dependency] = true
		}
		if !sort.StringsAreSorted(operation.DependsOn) {
			return fmt.Errorf("operation %s dependencies are not stably sorted", operation.OperationID)
		}
	}
	candidateIndex := -1
	publishIndex := -1
	candidateCount := 0
	publishCount := 0
	for index, operation := range changeSet.Operations {
		switch operation.Type {
		case OperationCandidateRun:
			candidateIndex = index
			candidateCount++
		case OperationPublish:
			publishIndex = index
			publishCount++
		}
	}
	if !requireCandidatePublish {
		if candidateCount != 0 || publishCount != 0 {
			return errors.New("template_instantiate change set forbids candidate_run and publish")
		}
		expectedChangeSetID, err := contentAddressChangeSetV1(changeSet)
		if err != nil || expectedChangeSetID != changeSet.ChangeSetID {
			return errors.New("change_set_id is not content-addressed")
		}
		return nil
	}
	if candidateCount != 1 || publishCount != 1 {
		return errors.New("change set requires exactly one candidate_run and one publish")
	}
	if publishIndex != len(changeSet.Operations)-1 || candidateIndex != publishIndex-1 {
		return errors.New("candidate_run and publish must terminate stable operation order")
	}
	candidate := changeSet.Operations[candidateIndex]
	wantChanges := make([]string, 0, candidateIndex)
	for _, operation := range changeSet.Operations[:candidateIndex] {
		wantChanges = append(wantChanges, operation.OperationID)
	}
	sort.Strings(wantChanges)
	if !equalStringSlices(candidate.DependsOn, wantChanges) {
		return errors.New("candidate_run must depend on all changes")
	}
	publish := changeSet.Operations[publishIndex]
	if len(publish.DependsOn) != 1 || publish.DependsOn[0] != candidate.OperationID {
		return errors.New("publish must depend only on candidate_run")
	}
	expectedChangeSetID, err := contentAddressChangeSetV1(changeSet)
	if err != nil || expectedChangeSetID != changeSet.ChangeSetID {
		return errors.New("change_set_id is not content-addressed")
	}
	return nil
}

func newChangeOperationV1(typ ChangeOperationTypeV1, target string, expected *int64, input any, depends []string, rollback string) (ChangeOperationV1, error) {
	contract, ok := operationContractV1[typ]
	if !ok {
		return ChangeOperationV1{}, fmt.Errorf("unknown operation type %q", typ)
	}
	return newChangeOperationWithCompilerV1(typ, target, expected, input, depends, rollback, contract.compiler)
}

func newChangeOperationWithCompilerV1(typ ChangeOperationTypeV1, target string, expected *int64, input any, depends []string, rollback, compiler string) (ChangeOperationV1, error) {
	contract := operationContractV1[typ]
	inputHash, inputBytes, err := canonicalInput(input)
	if err != nil {
		return ChangeOperationV1{}, err
	}
	dependencies := append([]string{}, depends...)
	sort.Strings(dependencies)
	verification := append([]string(nil), contract.verification...)
	operation := ChangeOperationV1{
		Type: typ, Target: target, ExpectedVersion: expected, Input: inputBytes, InputHash: inputHash,
		DependsOn: dependencies, Compiler: compiler, Verification: verification, RollbackRef: rollback,
	}
	id, err := contentAddressOperationV1(operation)
	if err != nil {
		return ChangeOperationV1{}, err
	}
	operation.OperationID = id
	return operation, nil
}

func contentAddressOperationV1(operation ChangeOperationV1) (string, error) {
	return hashCanonical(struct {
		Type            ChangeOperationTypeV1 `json:"type"`
		Target          string                `json:"target"`
		ExpectedVersion *int64                `json:"expected_version,omitempty"`
		InputHash       string                `json:"input_hash"`
		DependsOn       []string              `json:"depends_on"`
		Compiler        string                `json:"compiler"`
		Verification    []string              `json:"verification"`
		RollbackRef     string                `json:"rollback_ref"`
	}{operation.Type, operation.Target, operation.ExpectedVersion, operation.InputHash, operation.DependsOn,
		operation.Compiler, operation.Verification, operation.RollbackRef})
}

func contentAddressChangeSetV1(changeSet ChangeSetV1) (string, error) {
	return hashCanonical(struct {
		SchemaVersion int                 `json:"schema_version"`
		BaselineHash  string              `json:"baseline_hash"`
		BlueprintHash string              `json:"blueprint_hash"`
		Operations    []ChangeOperationV1 `json:"operations"`
	}{changeSet.SchemaVersion, changeSet.BaselineHash, changeSet.BlueprintHash, changeSet.Operations})
}

func canonicalInput(value any) (string, json.RawMessage, error) {
	bytes, err := json.Marshal(value)
	if err != nil {
		return "", nil, err
	}
	hash, canonical, err := canonicalRawJSON(bytes)
	return hash, json.RawMessage(canonical), err
}

func canonicalRawJSON(raw json.RawMessage) (string, []byte, error) {
	bytes, err := frozen.CanonicalizeJSON(raw)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:]), bytes, nil
}

func hashCanonical(value any) (string, error) {
	bytes, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return frozen.HashCanonicalJSON(bytes)
}

func semanticEqual(first, second any) (bool, error) {
	firstHash, err := hashCanonical(first)
	if err != nil {
		return false, err
	}
	secondHash, err := hashCanonical(second)
	return firstHash == secondHash, err
}

func teamIdentitySegment(blueprint teambuild.TeamBlueprintV1) string {
	if blueprint.Mode == teambuild.ModeOptimize {
		return strings.TrimSpace(blueprint.TeamID)
	}
	name := strings.ToLower(normalizeChangeText(blueprint.NewTeamName))
	name = strings.ReplaceAll(name, " ", "-")
	return name
}

func normalizeChangeText(value string) string { return strings.Join(strings.Fields(value), " ") }

func sortedTrimmed(values []string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = strings.TrimSpace(value)
	}
	sort.Strings(result)
	return result
}

func equalSortedStrings(first, second []string) bool {
	return equalStringSlices(sortedTrimmed(first), sortedTrimmed(second))
}

func equalStringSlices(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func nonEmptyStrings(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	return result
}

func normalizeChangeMember(value teambuild.BlueprintMemberV1) teambuild.BlueprintMemberV1 {
	value.StableRef = strings.TrimSpace(value.StableRef)
	value.Name = normalizeChangeText(value.Name)
	value.DisplayName = normalizeChangeText(value.DisplayName)
	value.Role = strings.TrimSpace(value.Role)
	value.ManagementMode = strings.TrimSpace(value.ManagementMode)
	value.Responsibilities = sortedNormalizedText(value.Responsibilities)
	value.Capabilities = sortedNormalizedText(value.Capabilities)
	value.ModelRef = strings.TrimSpace(value.ModelRef)
	value.ExecutionPolicy.EngineClass = strings.TrimSpace(value.ExecutionPolicy.EngineClass)
	value.ExecutionPolicy.Engine = strings.TrimSpace(value.ExecutionPolicy.Engine)
	value.ExecutionPolicy.ExecutionMode = strings.TrimSpace(value.ExecutionPolicy.ExecutionMode)
	value.ExecutionPolicy.RuntimeRef = strings.TrimSpace(value.ExecutionPolicy.RuntimeRef)
	value.ExecutionPolicy.InternalGraphRef = strings.TrimSpace(value.ExecutionPolicy.InternalGraphRef)
	return value
}

func sortedNormalizedText(values []string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = normalizeChangeText(value)
	}
	sort.Slice(result, func(i, j int) bool { return strings.ToLower(result[i]) < strings.ToLower(result[j]) })
	return result
}

func int64Ptr(value int64) *int64 { return &value }

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func bytesEqual(first, second []byte) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}
