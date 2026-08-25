package teamtemplates

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/build/teambuild"
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
	fingerprint, err := templateFingerprint(compiled.Template)
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
	mu           sync.Mutex
	run          teambuild.TeamBuildRun
	revision     teambuild.BlueprintRevision
	authorizeErr error
	createdBy    string
	authorizedBy string
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

func (s *memoryBuildStore) AuthorizeTemplateBuildRun(_ context.Context, _, _ string, confirmedBy string, _ teambuild.BlueprintRevisionToken, _ teambuild.TemplateAuthorizationPolicy) (teambuild.TeamBuildRun, teambuild.BuildAuthorizationReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authorizedBy = confirmedBy
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
