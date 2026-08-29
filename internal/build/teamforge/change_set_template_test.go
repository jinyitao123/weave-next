package teamforge

import (
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/org"
)

func TestTemplateInstantiateChangeSetContainsOnlyMaterialization(t *testing.T) {
	blueprint := templateInstantiateTestBlueprint()
	baseline := EmptyCreateBaselineV1()

	materialization, err := CompileTemplateInstantiateChangeSetV1(baseline, blueprint)
	if err != nil {
		t.Fatalf("CompileTemplateInstantiateChangeSetV1() error = %v", err)
	}
	if err := ValidateTemplateInstantiateChangeSetV1(materialization); err != nil {
		t.Fatalf("ValidateTemplateInstantiateChangeSetV1() error = %v", err)
	}
	if err := ValidateChangeSetV1(materialization); err == nil {
		t.Fatal("ordinary ChangeSet validator accepted a materialization-only ChangeSet")
	}
	for _, operation := range materialization.Operations {
		if operation.Type == OperationCandidateRun || operation.Type == OperationPublish {
			t.Fatalf("template operation = %s, candidate and publish must be absent", operation.Type)
		}
	}
	if _, err := materialization.TemplateInstantiateCanonicalBytes(); err != nil {
		t.Fatalf("TemplateInstantiateCanonicalBytes() error = %v", err)
	}
	if _, err := materialization.TemplateInstantiateCanonicalHash(); err != nil {
		t.Fatalf("TemplateInstantiateCanonicalHash() error = %v", err)
	}
}

func TestEvaluationChangeSetContainsNoAssetMutation(t *testing.T) {
	blueprint := templateInstantiateTestBlueprint()
	blueprint.Mode = teambuild.ModeOptimize
	blueprint.TeamID = "team-1"
	blueprint.NewTeamName = ""
	blueprint.RevisionPolicy.MaxRevisions = 1
	members := make([]BaselineMemberV1, 0, len(blueprint.Members))
	refs := make([]string, 0, len(blueprint.Members))
	for index := range blueprint.Members {
		blueprint.Members[index].ManagementMode = teambuild.BlueprintManagementPreserveExisting
		member := blueprint.Members[index]
		members = append(members, BaselineMemberV1{
			StableRef: member.StableRef, Target: member.Name, Version: 1, Desired: member,
		})
		refs = append(refs, member.StableRef)
	}
	baseline := TeamBuildBaselineV1{
		SchemaVersion:      ChangeSetSchemaVersionV1,
		SourceSnapshotHash: strings.Repeat("a", 64), Mode: teambuild.ModeOptimize, TeamID: "team-1",
		Team:    &BaselineTeamV1{Target: "team-1", Version: 1, Purpose: blueprint.Purpose, LeadRef: blueprint.LeadRef},
		Members: members,
		Roster:  &BaselineRosterV1{Version: 1, LeadRef: blueprint.LeadRef, MemberRefs: refs},
	}
	changeSet, err := CompileEvaluationChangeSetV1(baseline, blueprint, nil, "workflow-existing")
	if err != nil {
		t.Fatalf("CompileEvaluationChangeSetV1() error = %v", err)
	}
	if len(changeSet.Operations) != 3 {
		t.Fatalf("operations = %#v, want verify workflow + candidate + publish", changeSet.Operations)
	}
	want := []ChangeOperationTypeV1{OperationWorkflowCompile, OperationCandidateRun, OperationPublish}
	for index, operation := range changeSet.Operations {
		if operation.Type != want[index] {
			t.Fatalf("operation[%d] = %s, want %s", index, operation.Type, want[index])
		}
	}
}

func TestOrdinaryChangeSetRetainsCandidatePublishChain(t *testing.T) {
	changeSet, err := CompileChangeSetV1(EmptyCreateBaselineV1(), templateInstantiateTestBlueprint())
	if err != nil {
		t.Fatalf("CompileChangeSetV1() error = %v", err)
	}
	if err := ValidateChangeSetV1(changeSet); err != nil {
		t.Fatalf("ValidateChangeSetV1() error = %v", err)
	}
	if err := ValidateTemplateInstantiateChangeSetV1(changeSet); err == nil {
		t.Fatal("template validator accepted candidate+publish ChangeSet")
	}
	if len(changeSet.Operations) < 2 {
		t.Fatalf("operations = %d, want candidate+publish tail", len(changeSet.Operations))
	}
	tail := changeSet.Operations[len(changeSet.Operations)-2:]
	if tail[0].Type != OperationCandidateRun || tail[1].Type != OperationPublish {
		t.Fatalf("operation tail = [%s %s], want [candidate_run publish]", tail[0].Type, tail[1].Type)
	}
}

func TestTemplateInstantiateCompilerRequiresCreateBuiltinTemplate(t *testing.T) {
	blueprint := templateInstantiateTestBlueprint()
	blueprint.Mode = teambuild.ModeOptimize
	blueprint.NewTeamName = ""
	blueprint.TeamID = "team-1"
	if _, err := CompileTemplateInstantiateChangeSetV1(EmptyCreateBaselineV1(), blueprint); err == nil {
		t.Fatal("CompileTemplateInstantiateChangeSetV1() accepted optimize mode")
	}
}

func TestTeamWriteCreationEvaluationIsServerFixed(t *testing.T) {
	template := newTeamWriteToolsModeWithEvaluation("workspace", "compiler", teambuild.BuildAuthorizationReceipt{}, nil, nil, Deps{}, WriteDeps{}, true, org.TeamEvaluationUnevaluated)
	if template.creationEvaluation != org.TeamEvaluationUnevaluated {
		t.Fatalf("template creation evaluation = %q", template.creationEvaluation)
	}
	legacy := NewTeamWriteToolsMode("workspace", "compiler", teambuild.BuildAuthorizationReceipt{}, nil, nil, Deps{}, WriteDeps{}, true)
	if legacy.creationEvaluation != org.TeamEvaluationEvaluated {
		t.Fatalf("legacy creation evaluation = %q", legacy.creationEvaluation)
	}
	invalid := newTeamWriteToolsModeWithEvaluation("workspace", "compiler", teambuild.BuildAuthorizationReceipt{}, nil, nil, Deps{}, WriteDeps{}, true, "caller-controlled")
	if invalid.creationEvaluation != org.TeamEvaluationEvaluated {
		t.Fatalf("unknown creation evaluation = %q, want fail-safe evaluated", invalid.creationEvaluation)
	}
}

func templateInstantiateTestBlueprint() teambuild.TeamBlueprintV1 {
	return teambuild.TeamBlueprintV1{
		SchemaVersion: teambuild.BlueprintSchemaVersionV1,
		Mode:          teambuild.ModeCreate, NewTeamName: "research-team", Purpose: "持续完成调研",
		Members: []teambuild.BlueprintMemberV1{
			{StableRef: "lead", Name: "research-team-lead", DisplayName: "负责人", Role: teambuild.BlueprintMemberRoleAvatar, ManagementMode: teambuild.BlueprintManagementManaged, Responsibilities: []string{"协调"}, Capabilities: []string{"delegation"}, ExecutionPolicy: teambuild.BlueprintExecutionPolicyV1{EngineClass: teambuild.BlueprintEngineStandard, ExecutionMode: teambuild.BlueprintExecutionToolLoop}},
			{StableRef: "researcher", Name: "research-team-researcher", DisplayName: "调研员", Role: teambuild.BlueprintMemberRoleWorker, ManagementMode: teambuild.BlueprintManagementManaged, Responsibilities: []string{"调研"}, Capabilities: []string{"search"}, ExecutionPolicy: teambuild.BlueprintExecutionPolicyV1{EngineClass: teambuild.BlueprintEngineStandard, ExecutionMode: teambuild.BlueprintExecutionToolLoop}},
			{StableRef: "analyst", Name: "research-team-analyst", DisplayName: "分析员", Role: teambuild.BlueprintMemberRoleWorker, ManagementMode: teambuild.BlueprintManagementManaged, Responsibilities: []string{"分析"}, Capabilities: []string{"analysis"}, ExecutionPolicy: teambuild.BlueprintExecutionPolicyV1{EngineClass: teambuild.BlueprintEngineStandard, ExecutionMode: teambuild.BlueprintExecutionToolLoop}},
			{StableRef: "editor", Name: "research-team-editor", DisplayName: "编辑", Role: teambuild.BlueprintMemberRoleWorker, ManagementMode: teambuild.BlueprintManagementManaged, Responsibilities: []string{"汇总"}, Capabilities: []string{"writing"}, ExecutionPolicy: teambuild.BlueprintExecutionPolicyV1{EngineClass: teambuild.BlueprintEngineStandard, ExecutionMode: teambuild.BlueprintExecutionToolLoop}},
		},
		LeadRef: "lead",
		Workflow: teambuild.BlueprintWorkflowV1{
			Mode: teambuild.BlueprintWorkflowTemplate, Template: teambuild.BlueprintTemplateResearchSummary,
			TemplateParameters: &teambuild.BlueprintWorkflowTemplateParametersV1{
				LeadInstruction: "协调调研", ParallelWorkerRefs: []string{"researcher", "analyst"}, FinalizerRef: "editor",
				ResultRequirements: map[string]string{"researcher": "给出来源", "analyst": "交叉验证", "editor": "汇总报告"},
			},
		},
		RevisionPolicy: teambuild.BlueprintRevisionPolicyV1{
			MaxRevisions: 2, AllowedPatchPaths: []string{"/purpose"},
		},
	}
}
