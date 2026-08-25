package teamtemplates

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/build/teamtemplate"
)

const validTemplateYAML = `schema: team-template/v1
name: research-team
display_name: 调研团队
purpose: 交付可追溯的调研报告
template: research_synthesis
template_parameters:
  lead_instruction: 组织调研并完成交付
  parallel_worker_refs: [researcher, analyst]
  finalizer_ref: lead
  result_requirements:
    lead: 汇总并交付
    researcher: 提供可核验来源
    analyst: 交叉验证结论
    writer: 汇总完整交付物
    reviewer: 审核交付物并给出结论
members:
  - name: lead
    display_name: 负责人
    role: avatar
    responsibilities: [分工, 交付]
    capabilities: [task_delegation]
  - name: researcher
    display_name: 调研员
    role: worker
    responsibilities: [资料调研]
    capabilities: [web_search]
  - name: analyst
    display_name: 分析员
    role: worker
    responsibilities: [交叉验证]
    capabilities: [analysis]
  - name: writer
    display_name: 主笔
    role: worker
    responsibilities: [汇总交付]
    capabilities: [writing]
  - name: reviewer
    display_name: 审核员
    role: worker
    responsibilities: [质量审核]
    capabilities: [quality_review]
lead: lead
delivery:
  success_criteria: [事实可追溯]
budget:
  max_cost_usd: 2
`

func TestInstantiateRunsTemplatePipelineAndWaitsIndependently(t *testing.T) {
	idempotency := &memoryIdempotencyStore{}
	builds := &memoryBuildStore{}
	ctx, cancel := context.WithCancel(context.Background())
	submitter := &memorySubmitter{builds: builds, asynchronous: true, onSubmit: cancel}
	service := New(idempotency, builds, submitter, Options{
		Policy: testPolicy(), ReadyTimeout: time.Second, PollInterval: time.Millisecond,
		Now: func() time.Time { return time.Date(2026, 8, 25, 1, 0, 0, 0, time.UTC) },
	})
	outcome, err := service.Instantiate(ctx, "workspace-1", "user-1", Request{
		YAML: validTemplateYAML, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("Instantiate() error = %v", err)
	}
	if outcome.Status != "ready" || outcome.TeamID != "team-1" || outcome.Evaluation != "unevaluated" {
		t.Fatalf("outcome = %#v", outcome)
	}
	if submitter.calls != 1 {
		t.Fatalf("submit calls = %d, want 1", submitter.calls)
	}
	replayed, err := service.Instantiate(context.Background(), "workspace-1", "user-1", Request{
		YAML: validTemplateYAML, IdempotencyKey: idempotency.record.Key.String(),
	})
	if err != nil || replayed.TeamID != outcome.TeamID || submitter.calls != 1 {
		t.Fatalf("replay = %#v, err = %v, submit calls = %d", replayed, err, submitter.calls)
	}
	builds.mu.Lock()
	defer builds.mu.Unlock()
	if builds.run.ExecutionStrategy != teambuild.ExecutionStrategyTemplateInstantiate {
		t.Fatalf("execution strategy = %q", builds.run.ExecutionStrategy)
	}
	if builds.revision.RevisionNo != 1 || builds.revision.ChangeSetHash == "" {
		t.Fatalf("revision = %#v", builds.revision)
	}
}

func TestInstantiateDefersWhenTemplateAutoRequiresAuthorization(t *testing.T) {
	builds := &memoryBuildStore{authorizeErr: teambuild.ErrTemplateAuthorizationRequired}
	submitter := &memorySubmitter{builds: builds}
	service := New(&memoryIdempotencyStore{}, builds, submitter, Options{Policy: testPolicy()})
	outcome, err := service.Instantiate(context.Background(), "workspace-1", "real-user", Request{
		YAML: validTemplateYAML, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("Instantiate() error = %v", err)
	}
	if outcome.Status != "authorization_required" || outcome.ProgressURL == "" {
		t.Fatalf("outcome = %#v", outcome)
	}
	if submitter.calls != 0 {
		t.Fatalf("submit calls = %d, want 0", submitter.calls)
	}
}

func TestInstantiateResolvesSampleOverrides(t *testing.T) {
	builds := &memoryBuildStore{}
	service := New(&memoryIdempotencyStore{}, builds, &memorySubmitter{builds: builds}, Options{
		Policy: testPolicy(), Catalog: NewStaticCatalog(),
	})
	outcome, err := service.Instantiate(context.Background(), "workspace-1", "user-1", Request{
		Sample: "market-research", Overrides: map[string]any{
			"display_name": "消费市场调研团队",
			"budget":       map[string]any{"max_cost_usd": 4.0},
		}, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("Instantiate() error = %v", err)
	}
	if outcome.Status != "ready" || builds.run.TotalBudget.MaxCostUSD != 4 {
		t.Fatalf("outcome = %#v, budget = %#v", outcome, builds.run.TotalBudget)
	}
}

func TestInstantiateFreezesDeclarativePlanAsSecondRevision(t *testing.T) {
	spec := declarativeTestSpec(t)
	builds := &memoryBuildStore{}
	submitter := &memorySubmitter{builds: builds}
	service := New(&memoryIdempotencyStore{}, builds, submitter, Options{Policy: testPolicy()})
	key := uuid.NewString()
	outcome, err := service.Instantiate(context.Background(), "workspace-1", "user-1", Request{
		YAML: validTemplateYAML, DeclarativeSpec: &spec, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("Instantiate() error = %v", err)
	}
	if outcome.Status != "ready" || builds.revision.RevisionNo != 2 || builds.authorizedToken.RevisionNo != 2 {
		t.Fatalf("outcome = %#v, revision = %#v, token = %#v", outcome, builds.revision, builds.authorizedToken)
	}
	var blueprint teambuild.TeamBlueprintV1
	if err := json.Unmarshal(builds.revision.BlueprintJSON, &blueprint); err != nil {
		t.Fatal(err)
	}
	if blueprint.Workflow.Mode != teambuild.BlueprintWorkflowDeclarativeV1 || blueprint.Workflow.DeclarativeSpecHash == "" {
		t.Fatalf("declarative blueprint = %#v", blueprint.Workflow)
	}
	var changeSet teamforge.ChangeSetV1
	if err := json.Unmarshal(builds.revision.ChangeSetJSON, &changeSet); err != nil {
		t.Fatal(err)
	}
	if err := teamforge.ValidateTemplateInstantiateChangeSetV1(changeSet); err != nil {
		t.Fatalf("materialization ChangeSet invalid: %v", err)
	}
	for _, operation := range changeSet.Operations {
		if operation.Type == teamforge.OperationCandidateRun || operation.Type == teamforge.OperationPublish {
			t.Fatalf("deferred operation leaked into declarative template ChangeSet: %s", operation.Type)
		}
	}
	replayed, err := service.Instantiate(context.Background(), "workspace-1", "user-1", Request{
		YAML: validTemplateYAML, DeclarativeSpec: &spec, IdempotencyKey: key,
	})
	if err != nil || replayed.TeamID != outcome.TeamID || submitter.calls != 1 {
		t.Fatalf("replay = %#v, err = %v, submit calls = %d", replayed, err, submitter.calls)
	}
	changed := spec
	changed.Nodes = append([]teamforge.DeclarativeWorkflowNodeV1(nil), spec.Nodes...)
	changed.Nodes[0].Label = "变更后的运行合同"
	if _, err := service.Instantiate(context.Background(), "workspace-1", "user-1", Request{
		YAML: validTemplateYAML, DeclarativeSpec: &changed, IdempotencyKey: key,
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed declarative replay error = %v, want conflict", err)
	}
}

func TestInstantiateRejectsDeclarativePlanBeforeAnyWrite(t *testing.T) {
	spec := declarativeTestSpec(t)
	for index := range spec.Nodes {
		if spec.Nodes[index].StableRef == "writer" {
			spec.Nodes[index].StableRef = "missing-worker"
		}
	}
	idempotency := &memoryIdempotencyStore{}
	builds := &memoryBuildStore{}
	service := New(idempotency, builds, &memorySubmitter{builds: builds}, Options{Policy: testPolicy()})
	_, err := service.Instantiate(context.Background(), "workspace-1", "user-1", Request{
		YAML: validTemplateYAML, DeclarativeSpec: &spec, IdempotencyKey: uuid.NewString(),
	})
	var validation *teamtemplate.ValidationError
	if !errors.As(err, &validation) || len(validation.Problems) == 0 || validation.Problems[0].Path != "/declarative_spec" {
		t.Fatalf("Instantiate() error = %v, want declarative field problem", err)
	}
	if idempotency.record != nil || builds.run.BuildRunID != "" || builds.revision.RevisionNo != 0 {
		t.Fatalf("invalid declarative plan wrote state: claim=%#v run=%#v revision=%#v", idempotency.record, builds.run, builds.revision)
	}
}

func declarativeTestSpec(t *testing.T) teamforge.DeclarativeWorkflowSpecV1 {
	t.Helper()
	spec, err := teamforge.BuildDeclarativeWorkflowPatternV1(teamforge.DeclarativeWorkflowPatternV1{
		Kind:            teamforge.DeclarativePatternParallelJoinReviewLoopV1,
		LeadInstruction: "并行调研后汇总，并经过审核返修再交付",
		ParallelWorkers: []teamforge.DeclarativePatternWorkerV1{
			{StableRef: "researcher", ResultRequirement: "提交来源材料"},
			{StableRef: "analyst", ResultRequirement: "提交分析结论"},
		},
		PrimaryWorker:  teamforge.DeclarativePatternWorkerV1{StableRef: "writer", ResultRequirement: "汇总完整交付物"},
		ReviewerWorker: teamforge.DeclarativePatternWorkerV1{StableRef: "reviewer", ResultRequirement: "审核并返回 PASS 或 REVISE"},
		MaxIterations:  2,
	})
	if err != nil {
		t.Fatalf("BuildDeclarativeWorkflowPatternV1() error = %v", err)
	}
	return spec
}

func TestInstantiateRejectsChangedContentForClaimedKey(t *testing.T) {
	key := uuid.New()
	idempotency := &memoryIdempotencyStore{record: &IdempotencyRecord{
		WorkspaceID: "workspace-1", Key: key, Fingerprint: string(make([]byte, 64)),
		BuildRunID: "existing-run", CreatedBy: "user-1",
	}}
	service := New(idempotency, &memoryBuildStore{}, &memorySubmitter{}, Options{Policy: testPolicy()})
	_, err := service.Instantiate(context.Background(), "workspace-1", "user-1", Request{
		YAML: validTemplateYAML, IdempotencyKey: key.String(),
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("Instantiate() error = %v, want idempotency conflict", err)
	}
}

func TestInstantiateReplayPreservesOriginalRealUser(t *testing.T) {
	compiled, err := teamtemplate.CompileYAML([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := templateFingerprint(compiled.Template, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := uuid.New()
	idempotency := &memoryIdempotencyStore{record: &IdempotencyRecord{
		WorkspaceID: "workspace-1", Key: key, Fingerprint: fingerprint,
		BuildRunID: deterministicBuildRunID("workspace-1", key), CreatedBy: "original-user",
	}}
	builds := &memoryBuildStore{}
	service := New(idempotency, builds, &memorySubmitter{builds: builds}, Options{Policy: testPolicy()})
	if _, err := service.Instantiate(context.Background(), "workspace-1", "replay-user", Request{
		YAML: validTemplateYAML, IdempotencyKey: key.String(),
	}); err != nil {
		t.Fatalf("Instantiate() error = %v", err)
	}
	if builds.createdBy != "original-user" || builds.authorizedBy != "original-user" {
		t.Fatalf("created/authorized users = %q/%q", builds.createdBy, builds.authorizedBy)
	}
}

func TestInstantiateValidationProblemsAreFieldOriented(t *testing.T) {
	service := New(&memoryIdempotencyStore{}, &memoryBuildStore{}, &memorySubmitter{}, Options{Policy: testPolicy()})
	_, err := service.Instantiate(context.Background(), "workspace-1", "user-1", Request{
		YAML: validTemplateYAML, IdempotencyKey: "not-a-uuid",
	})
	var validation *teamtemplate.ValidationError
	if !errors.As(err, &validation) || len(validation.Problems) != 1 || validation.Problems[0].Path != "/idempotency_key" {
		t.Fatalf("Instantiate() error = %v, want field validation", err)
	}
}

func testPolicy() teambuild.TemplateAuthorizationPolicy {
	return teambuild.TemplateAuthorizationPolicy{
		AutoBudgetThresholdUSD: 5, DailyBudgetUSD: 25,
		MonthlyBudgetUSD: 250, MaxConcurrent: 2,
	}
}

type memoryIdempotencyStore struct {
	record *IdempotencyRecord
}

func (s *memoryIdempotencyStore) Claim(_ context.Context, requested IdempotencyRecord) (IdempotencyRecord, error) {
	if s.record == nil {
		copy := requested
		s.record = &copy
	}
	return *s.record, nil
}

type memoryBuildStore struct {
	mu              sync.Mutex
	run             teambuild.TeamBuildRun
	revision        teambuild.BlueprintRevision
	authorizeErr    error
	createdBy       string
	authorizedBy    string
	authorizedToken teambuild.BlueprintRevisionToken
}

func (s *memoryBuildStore) CreateBuildRun(_ context.Context, workspaceID, buildRunID string, params teambuild.CreateRunParams) (teambuild.TeamBuildRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	briefHash, contract, contractHash, err := teambuild.ValidateBuildRunDrafts(params.Brief, params.Contract)
	if err != nil {
		return teambuild.TeamBuildRun{}, err
	}
	s.run = teambuild.TeamBuildRun{
		WorkspaceID: workspaceID, BuildRunID: buildRunID, Mode: teambuild.ModeCreate,
		ExecutionStrategy: params.ExecutionStrategy, Status: teambuild.StatusPlanning,
		Brief: params.Brief, BriefHash: briefHash, Contract: contract, ContractHash: contractHash,
		TotalBudget: params.Brief.TotalBudget,
	}
	s.createdBy = params.CreatedBy
	return s.run, nil
}

func (s *memoryBuildStore) GetBuildRun(_ context.Context, _, _ string) (teambuild.TeamBuildRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run.BuildRunID == "" {
		return teambuild.TeamBuildRun{}, teambuild.ErrBuildRunNotFound
	}
	return s.run, nil
}

func (s *memoryBuildStore) GetLatestBlueprintRevision(_ context.Context, _, _ string) (teambuild.BlueprintRevision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revision.RevisionNo == 0 {
		return teambuild.BlueprintRevision{}, teambuild.ErrBuildRunNotFound
	}
	return s.revision, nil
}

func (s *memoryBuildStore) PersistCompilerAuthorizationBundle(_ context.Context, workspaceID, buildRunID string, bundle teambuild.CompilerAuthorizationBundle) (teambuild.BlueprintRevision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision = teambuild.BlueprintRevision{
		WorkspaceID: workspaceID, BuildRunID: buildRunID, RevisionNo: bundle.RevisionNo,
		BlueprintJSON: bundle.BlueprintJSON, BlueprintHash: bundle.BlueprintHash,
		ChangeSetJSON: bundle.ChangeSetJSON, ChangeSetHash: bundle.ChangeSetHash,
		EvaluationContractHash: bundle.EvaluationContractHash,
	}
	return s.revision, nil
}

func (s *memoryBuildStore) AuthorizeTemplateBuildRun(_ context.Context, _, _ string, confirmedBy string, token teambuild.BlueprintRevisionToken, _ teambuild.TemplateAuthorizationPolicy) (teambuild.TeamBuildRun, teambuild.BuildAuthorizationReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authorizedBy = confirmedBy
	s.authorizedToken = token
	if s.authorizeErr != nil {
		return teambuild.TeamBuildRun{}, teambuild.BuildAuthorizationReceipt{}, s.authorizeErr
	}
	s.run.Status = teambuild.StatusAuthorized
	return s.run, teambuild.BuildAuthorizationReceipt{}, nil
}

type memorySubmitter struct {
	builds       *memoryBuildStore
	asynchronous bool
	onSubmit     func()
	calls        int
}

func (s *memorySubmitter) Submit(_ context.Context, _, _ string) error {
	s.calls++
	if s.onSubmit != nil {
		s.onSubmit()
	}
	if s.builds == nil {
		return nil
	}
	complete := func() {
		s.builds.mu.Lock()
		defer s.builds.mu.Unlock()
		s.builds.run.Status = teambuild.StatusPassed
		s.builds.run.FinalRef = &teambuild.FinalRef{TeamID: "team-1", Ref: "team:team-1"}
	}
	if s.asynchronous {
		go func() {
			time.Sleep(5 * time.Millisecond)
			complete()
		}()
	} else {
		complete()
	}
	return nil
}
