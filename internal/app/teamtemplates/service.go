// Package teamtemplates assembles the team-template fast path from the pure
// template compiler and the existing teamforge build pipeline. It owns no
// organization, registry, or workflow writes.
package teamtemplates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teameval"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/build/teamtemplate"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

const progressPathPrefix = "/v1/internal/team-build-runs/"

var (
	ErrIdempotencyConflict = errors.New("team template idempotency conflict")
	ErrBuildFailed         = errors.New("team template build failed")
	ErrUnavailable         = errors.New("team template service unavailable")
)

type Request struct {
	YAML            string                               `json:"yaml,omitempty"`
	Sample          string                               `json:"sample,omitempty"`
	Overrides       map[string]any                       `json:"overrides,omitempty"`
	DeclarativeSpec *teamforge.DeclarativeWorkflowSpecV1 `json:"declarative_spec,omitempty"`
	IdempotencyKey  string                               `json:"idempotency_key"`
}

type Outcome struct {
	TeamID      string `json:"team_id,omitempty"`
	BuildRunID  string `json:"build_run_id"`
	Status      string `json:"status"`
	Evaluation  string `json:"evaluation,omitempty"`
	ProgressURL string `json:"progress_url,omitempty"`
}

type Sample struct {
	Name            string                               `json:"name"`
	DisplayName     string                               `json:"display_name"`
	Description     string                               `json:"description,omitempty"`
	YAML            string                               `json:"yaml"`
	DeclarativeSpec *teamforge.DeclarativeWorkflowSpecV1 `json:"declarative_spec,omitempty"`
}

type Catalog interface {
	Resolve(name string, overrides map[string]any) ([]byte, error)
	ResolveDeclarative(name string) (*teamforge.DeclarativeWorkflowSpecV1, error)
	List() []Sample
}

type IdempotencyRecord struct {
	WorkspaceID string
	Key         uuid.UUID
	Fingerprint string
	BuildRunID  string
	CreatedBy   string
}

type IdempotencyStore interface {
	Claim(context.Context, IdempotencyRecord) (IdempotencyRecord, error)
}

type BuildStore interface {
	CreateBuildRun(context.Context, string, string, teambuild.CreateRunParams) (teambuild.TeamBuildRun, error)
	GetBuildRun(context.Context, string, string) (teambuild.TeamBuildRun, error)
	GetLatestBlueprintRevision(context.Context, string, string) (teambuild.BlueprintRevision, error)
	PersistCompilerAuthorizationBundle(context.Context, string, string, teambuild.CompilerAuthorizationBundle) (teambuild.BlueprintRevision, error)
	AuthorizeTemplateBuildRun(context.Context, string, string, string, teambuild.BlueprintRevisionToken, teambuild.TemplateAuthorizationPolicy) (teambuild.TeamBuildRun, teambuild.BuildAuthorizationReceipt, error)
}

type Submitter interface {
	Submit(context.Context, string, string) error
}

type Options struct {
	Policy          teambuild.TemplateAuthorizationPolicy
	DefaultModel    string
	RuntimeSelector RuntimeSelector
	ReadyTimeout    time.Duration
	PollInterval    time.Duration
	RunTTL          time.Duration
	Now             func() time.Time
	Catalog         Catalog
}

type Service struct {
	idempotency     IdempotencyStore
	builds          BuildStore
	submitter       Submitter
	policy          teambuild.TemplateAuthorizationPolicy
	defaultModel    string
	runtimeSelector RuntimeSelector
	ready           time.Duration
	poll            time.Duration
	runTTL          time.Duration
	now             func() time.Time
	catalog         Catalog
}

type declarativePlan struct {
	bundle teambuild.CompilerAuthorizationBundle
}

func New(idempotency IdempotencyStore, builds BuildStore, submitter Submitter, options Options) *Service {
	if options.ReadyTimeout <= 0 {
		options.ReadyTimeout = 15 * time.Second
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 100 * time.Millisecond
	}
	if options.RunTTL <= 0 {
		options.RunTTL = 30 * time.Minute
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Service{
		idempotency: idempotency, builds: builds, submitter: submitter,
		policy: options.Policy, ready: options.ReadyTimeout, poll: options.PollInterval,
		defaultModel:    strings.TrimSpace(options.DefaultModel),
		runtimeSelector: options.RuntimeSelector,
		runTTL:          options.RunTTL, now: options.Now, catalog: options.Catalog,
	}
}

func (s *Service) Samples() []Sample {
	if s == nil || s.catalog == nil {
		return []Sample{}
	}
	items := s.catalog.List()
	return append([]Sample(nil), items...)
}

func (s *Service) Instantiate(ctx context.Context, workspaceID, userID string, request Request) (Outcome, error) {
	if s == nil || s.idempotency == nil || s.builds == nil || s.submitter == nil {
		return Outcome{}, ErrUnavailable
	}
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(userID) == "" {
		return Outcome{}, validationError("/", "template_request_identity_required", "workspace and authenticated user are required")
	}
	key, err := uuid.Parse(strings.TrimSpace(request.IdempotencyKey))
	if err != nil {
		return Outcome{}, validationError("/idempotency_key", "template_idempotency_key_invalid", "idempotency_key must be a UUID")
	}
	yamlBytes, err := s.resolveRequest(request)
	if err != nil {
		return Outcome{}, err
	}
	if request.DeclarativeSpec == nil && strings.TrimSpace(request.Sample) != "" {
		request.DeclarativeSpec, err = s.catalog.ResolveDeclarative(strings.TrimSpace(request.Sample))
		if err != nil {
			return Outcome{}, validationError("/sample", "template_sample_invalid", err.Error())
		}
	}
	compiled, err := teamtemplate.CompileYAML(yamlBytes)
	if err != nil {
		return Outcome{}, err
	}
	fingerprint, err := templateFingerprint(compiled.Template, request.DeclarativeSpec)
	if err != nil {
		return Outcome{}, fmt.Errorf("fingerprint team template: %w", err)
	}
	if s.runtimeSelector != nil {
		if assignment, selectErr := s.runtimeSelector.Select(ctx, workspaceID, engine.Codex, "", ""); selectErr == nil {
			applyTemplateDefaultRuntime(&compiled, assignment.RuntimeID)
		}
	}
	applyTemplateDefaultModel(&compiled, s.defaultModel)
	buildRunID := deterministicBuildRunID(workspaceID, key)
	var declarative *declarativePlan
	if request.DeclarativeSpec != nil {
		declarative, err = compileDeclarativePlan(workspaceID, buildRunID, compiled, *request.DeclarativeSpec)
		if err != nil {
			return Outcome{}, declarativeRequestError(err)
		}
	}
	record, err := s.idempotency.Claim(ctx, IdempotencyRecord{
		WorkspaceID: workspaceID, Key: key, Fingerprint: fingerprint,
		BuildRunID: buildRunID, CreatedBy: userID,
	})
	if err != nil {
		return Outcome{}, err
	}
	if record.Fingerprint != fingerprint {
		return Outcome{}, ErrIdempotencyConflict
	}
	buildRunID = record.BuildRunID
	claimant := record.CreatedBy
	run, err := s.ensureBuildRun(ctx, workspaceID, claimant, buildRunID, compiled)
	if err != nil {
		return Outcome{}, err
	}
	if terminal, outcome, terminalErr := terminalOutcome(run); terminal {
		return outcome, terminalErr
	}
	var revision teambuild.BlueprintRevision
	if declarative == nil {
		revision, err = s.ensureBlueprintRevision(ctx, workspaceID, buildRunID, compiled)
	} else {
		revision, err = s.ensureDeclarativeRevisions(ctx, workspaceID, buildRunID, compiled, *declarative)
	}
	if err != nil {
		return Outcome{}, err
	}
	run, err = s.builds.GetBuildRun(ctx, workspaceID, buildRunID)
	if err != nil {
		return Outcome{}, fmt.Errorf("reload template build run: %w", err)
	}
	if run.Status == teambuild.StatusPlanning {
		token := teambuild.BlueprintRevisionToken{
			RevisionNo: revision.RevisionNo, BlueprintHash: revision.BlueprintHash,
			ChangeSetHash: revision.ChangeSetHash,
		}
		run, _, err = s.builds.AuthorizeTemplateBuildRun(ctx, workspaceID, buildRunID, claimant, token, s.policy)
		if err != nil {
			// A concurrent replay can win authorization between the preceding
			// read and this call. Trust only the persisted successor state.
			run, reloadErr := s.builds.GetBuildRun(ctx, workspaceID, buildRunID)
			if reloadErr != nil || run.Status == teambuild.StatusPlanning {
				return Outcome{}, fmt.Errorf("authorize template build run: %w", err)
			}
		}
	}
	if terminal, outcome, terminalErr := terminalOutcome(run); terminal {
		return outcome, terminalErr
	}
	if run.Status == teambuild.StatusAuthorized {
		if err := s.submitter.Submit(ctx, workspaceID, buildRunID); err != nil {
			return Outcome{}, fmt.Errorf("submit template build run: %w", err)
		}
	}
	return s.waitForTerminal(workspaceID, buildRunID)
}

// RuntimeSelector is the narrow execution-placement surface used by the
// product template path. The platform owns this choice; callers should not
// have to copy runtime identifiers into otherwise business-level team YAML.
type RuntimeSelector interface {
	Select(context.Context, string, string, string, string) (runtimes.Assignment, error)
}

func applyTemplateDefaultRuntime(compiled *teamtemplate.Compilation, runtimeID string) {
	if compiled == nil || strings.TrimSpace(runtimeID) == "" {
		return
	}
	runtimeID = strings.TrimSpace(runtimeID)
	for i := range compiled.Blueprint.Members {
		member := &compiled.Blueprint.Members[i]
		if member.ExecutionPolicy.EngineClass != teambuild.BlueprintEngineStandard || strings.TrimSpace(member.ModelRef) != "" {
			continue
		}
		member.ExecutionPolicy = teambuild.BlueprintExecutionPolicyV1{
			EngineClass:   teambuild.BlueprintEngineCLI,
			Engine:        engine.Codex,
			ExecutionMode: teambuild.BlueprintExecutionRuntime,
			RuntimeRef:    runtimeID,
		}
	}
	for i := range compiled.Template.Members {
		member := &compiled.Template.Members[i]
		if member.ExecutionPolicy != nil || strings.TrimSpace(member.ModelRef) != "" {
			continue
		}
		member.ExecutionPolicy = &teamtemplate.ExecutionPolicy{
			EngineClass:   teambuild.BlueprintEngineCLI,
			Engine:        engine.Codex,
			ExecutionMode: teambuild.BlueprintExecutionRuntime,
			RuntimeRef:    runtimeID,
		}
	}
}

func applyTemplateDefaultModel(compiled *teamtemplate.Compilation, defaultModel string) {
	if compiled == nil || strings.TrimSpace(defaultModel) == "" {
		return
	}
	defaultModel = strings.TrimSpace(defaultModel)
	for i := range compiled.Blueprint.Members {
		member := &compiled.Blueprint.Members[i]
		if strings.TrimSpace(member.ModelRef) != "" || member.ExecutionPolicy.EngineClass != teambuild.BlueprintEngineStandard {
			continue
		}
		member.ModelRef = defaultModel
	}
	for i := range compiled.Template.Members {
		member := &compiled.Template.Members[i]
		if strings.TrimSpace(member.ModelRef) != "" || (member.ExecutionPolicy != nil && member.ExecutionPolicy.EngineClass != teambuild.BlueprintEngineStandard) {
			continue
		}
		member.ModelRef = defaultModel
	}
}

func (s *Service) ensureDeclarativeRevisions(
	ctx context.Context,
	workspaceID, buildRunID string,
	compiled teamtemplate.Compilation,
	plan declarativePlan,
) (teambuild.BlueprintRevision, error) {
	latest, err := s.builds.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID)
	switch {
	case err == nil && latest.RevisionNo == plan.bundle.RevisionNo:
		return verifyDeclarativeRevision(latest, plan)
	case err == nil && latest.RevisionNo == 1:
		if _, err := verifyBlueprintIdentity(latest, compiled.Blueprint); err != nil {
			return teambuild.BlueprintRevision{}, err
		}
	case errors.Is(err, teambuild.ErrBuildRunNotFound):
		if _, err := s.ensureBlueprintRevision(ctx, workspaceID, buildRunID, compiled); err != nil {
			return teambuild.BlueprintRevision{}, err
		}
	case err != nil:
		return teambuild.BlueprintRevision{}, fmt.Errorf("read declarative template revision: %w", err)
	default:
		return teambuild.BlueprintRevision{}, ErrIdempotencyConflict
	}
	revision, err := s.builds.PersistCompilerAuthorizationBundle(ctx, workspaceID, buildRunID, plan.bundle)
	if err == nil {
		return revision, nil
	}
	revision, reloadErr := s.builds.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID)
	if reloadErr != nil {
		return teambuild.BlueprintRevision{}, fmt.Errorf("persist declarative template revision: %w", err)
	}
	return verifyDeclarativeRevision(revision, plan)
}

func verifyDeclarativeRevision(revision teambuild.BlueprintRevision, plan declarativePlan) (teambuild.BlueprintRevision, error) {
	if revision.RevisionNo != plan.bundle.RevisionNo ||
		revision.BlueprintHash != plan.bundle.BlueprintHash ||
		revision.ChangeSetHash != plan.bundle.ChangeSetHash {
		return teambuild.BlueprintRevision{}, ErrIdempotencyConflict
	}
	return revision, nil
}

func compileDeclarativePlan(
	workspaceID, buildRunID string,
	compiled teamtemplate.Compilation,
	spec teamforge.DeclarativeWorkflowSpecV1,
) (*declarativePlan, error) {
	briefHash, normalizedContract, contractHash, err := teambuild.ValidateBuildRunDrafts(compiled.Brief, compiled.Contract)
	if err != nil {
		return nil, err
	}
	bindings, err := teamforge.ResolveCreateDeclarativeWorkerBindingsV1(spec, compiled.Blueprint)
	if err != nil {
		return nil, err
	}
	planned := make([]teameval.PlannedWorkerBinding, 0, len(bindings))
	for _, worker := range bindings {
		planned = append(planned, teameval.PlannedWorkerBinding{
			StableRef: worker.StableRef, AgentID: worker.AgentID, AgentVersion: worker.AgentVersion,
		})
	}
	frozen, err := teamforge.FreezeDeclarativeWorkflowSpecV1(
		spec,
		bindings,
		teamforge.DeclarativeBuildBindingV1{
			BuildRunID: buildRunID, BriefHash: briefHash, ContractHash: contractHash,
			AssetScope: compiled.Brief.AllowedAssets, BaselineHash: teamforge.EmptyCreateBaselineHashV1,
		},
		func(trigger machine.TriggerConfig, graph machine.GraphDefinition) (machine.Report, error) {
			return teameval.ValidateWorkflowForBlueprint(workspaceID, compiled.Blueprint, planned, trigger, graph)
		},
	)
	if err != nil {
		return nil, err
	}
	blueprint := compiled.Blueprint
	blueprint.Workflow = teambuild.BlueprintWorkflowV1{
		Mode: teambuild.BlueprintWorkflowDeclarativeV1, DeclarativeSpecHash: frozen.SpecHash,
	}
	if err := teambuild.ValidateTeamBlueprintV1(blueprint); err != nil {
		return nil, err
	}
	changeSet, err := teamforge.CompileTemplateInstantiateDeclarativeChangeSetV1(
		teamforge.EmptyCreateBaselineV1(), blueprint, frozen,
	)
	if err != nil {
		return nil, err
	}
	blueprintJSON, err := json.Marshal(blueprint)
	if err != nil {
		return nil, err
	}
	blueprintHash, err := blueprint.BlueprintHash()
	if err != nil {
		return nil, err
	}
	changeSetJSON, err := changeSet.TemplateInstantiateCanonicalBytes()
	if err != nil {
		return nil, err
	}
	changeSetHash, err := changeSet.TemplateInstantiateCanonicalHash()
	if err != nil {
		return nil, err
	}
	if normalizedContractHash, hashErr := normalizedContract.Hash(); hashErr != nil || normalizedContractHash != contractHash {
		return nil, errors.New("normalized declarative evaluation contract hash mismatch")
	}
	return &declarativePlan{
		bundle: teambuild.CompilerAuthorizationBundle{
			RevisionNo: 2, BlueprintJSON: blueprintJSON, BlueprintHash: blueprintHash,
			ChangeSetJSON: changeSetJSON, ChangeSetHash: changeSetHash,
			BaselineHash: teamforge.EmptyCreateBaselineHashV1, EvaluationContractHash: contractHash,
		},
	}, nil
}

func declarativeRequestError(err error) error {
	var validation *teamforge.DeclarativeWorkflowValidationError
	if errors.As(err, &validation) {
		problems := make([]teamtemplate.Problem, 0, len(validation.Problems))
		for _, problem := range validation.Problems {
			problems = append(problems, teamtemplate.Problem{
				Path: "/declarative_spec" + problem.Path, Code: problem.Code, Message: problem.Message,
			})
		}
		return &teamtemplate.ValidationError{Problems: problems}
	}
	var blueprintValidation *teambuild.BlueprintValidationError
	if errors.As(err, &blueprintValidation) {
		problems := make([]teamtemplate.Problem, 0, len(blueprintValidation.Problems))
		for _, problem := range blueprintValidation.Problems {
			problems = append(problems, teamtemplate.Problem{
				Path: "/declarative_spec" + problem.Path, Code: problem.Code, Message: problem.Message,
			})
		}
		return &teamtemplate.ValidationError{Problems: problems}
	}
	return validationError("/declarative_spec", "template_declarative_plan_invalid", err.Error())
}

func (s *Service) resolveRequest(request Request) ([]byte, error) {
	hasYAML := request.YAML != ""
	hasSample := strings.TrimSpace(request.Sample) != ""
	if hasYAML == hasSample {
		return nil, validationError("/", "template_source_invalid", "exactly one of yaml or sample is required")
	}
	if hasYAML {
		if len(request.Overrides) != 0 {
			return nil, validationError("/overrides", "template_overrides_invalid", "overrides are only valid with sample")
		}
		return []byte(request.YAML), nil
	}
	if s.catalog == nil {
		return nil, validationError("/sample", "template_sample_unknown", "sample does not exist")
	}
	data, err := s.catalog.Resolve(strings.TrimSpace(request.Sample), request.Overrides)
	if err != nil {
		return nil, validationError("/sample", "template_sample_invalid", err.Error())
	}
	return data, nil
}

func (s *Service) ensureBuildRun(ctx context.Context, workspaceID, userID, buildRunID string, compiled teamtemplate.Compilation) (teambuild.TeamBuildRun, error) {
	run, err := s.builds.GetBuildRun(ctx, workspaceID, buildRunID)
	if err == nil {
		return verifyBuildIdentity(run, compiled)
	}
	if !errors.Is(err, teambuild.ErrBuildRunNotFound) {
		return teambuild.TeamBuildRun{}, fmt.Errorf("read template build run: %w", err)
	}
	run, err = s.builds.CreateBuildRun(ctx, workspaceID, buildRunID, teambuild.CreateRunParams{
		Brief: compiled.Brief, Contract: compiled.Contract,
		ExpiresAt: s.now().UTC().Add(s.runTTL), CreatedBy: userID,
		ExecutionStrategy: teambuild.ExecutionStrategyTemplateInstantiate,
	})
	if err == nil {
		return run, nil
	}
	// The deterministic ID makes concurrent replays converge after one wins
	// the insert, without depending on driver-specific unique errors.
	run, reloadErr := s.builds.GetBuildRun(ctx, workspaceID, buildRunID)
	if reloadErr != nil {
		return teambuild.TeamBuildRun{}, fmt.Errorf("create template build run: %w", err)
	}
	return verifyBuildIdentity(run, compiled)
}

func verifyBuildIdentity(run teambuild.TeamBuildRun, compiled teamtemplate.Compilation) (teambuild.TeamBuildRun, error) {
	briefHash, _, contractHash, err := teambuild.ValidateBuildRunDrafts(compiled.Brief, compiled.Contract)
	if err != nil {
		return teambuild.TeamBuildRun{}, err
	}
	if run.ExecutionStrategy != teambuild.ExecutionStrategyTemplateInstantiate ||
		run.BriefHash != briefHash || run.ContractHash != contractHash {
		return teambuild.TeamBuildRun{}, ErrIdempotencyConflict
	}
	return run, nil
}

func (s *Service) ensureBlueprintRevision(ctx context.Context, workspaceID, buildRunID string, compiled teamtemplate.Compilation) (teambuild.BlueprintRevision, error) {
	existing, err := s.builds.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID)
	if err == nil {
		return verifyBlueprintIdentity(existing, compiled.Blueprint)
	}
	if !errors.Is(err, teambuild.ErrBuildRunNotFound) {
		return teambuild.BlueprintRevision{}, fmt.Errorf("read template blueprint revision: %w", err)
	}
	changeSet, err := teamforge.CompileTemplateInstantiateChangeSetV1(teamforge.EmptyCreateBaselineV1(), compiled.Blueprint)
	if err != nil {
		return teambuild.BlueprintRevision{}, fmt.Errorf("compile template change set: %w", err)
	}
	blueprintJSON, err := json.Marshal(compiled.Blueprint)
	if err != nil {
		return teambuild.BlueprintRevision{}, fmt.Errorf("encode template blueprint: %w", err)
	}
	blueprintHash, err := compiled.Blueprint.BlueprintHash()
	if err != nil {
		return teambuild.BlueprintRevision{}, fmt.Errorf("hash template blueprint: %w", err)
	}
	changeSetJSON, err := changeSet.TemplateInstantiateCanonicalBytes()
	if err != nil {
		return teambuild.BlueprintRevision{}, fmt.Errorf("encode template change set: %w", err)
	}
	changeSetHash, err := changeSet.TemplateInstantiateCanonicalHash()
	if err != nil {
		return teambuild.BlueprintRevision{}, fmt.Errorf("hash template change set: %w", err)
	}
	contractHash, err := compiled.Contract.Hash()
	if err != nil {
		return teambuild.BlueprintRevision{}, fmt.Errorf("hash template evaluation contract: %w", err)
	}
	revision, err := s.builds.PersistCompilerAuthorizationBundle(ctx, workspaceID, buildRunID, teambuild.CompilerAuthorizationBundle{
		RevisionNo: 1, BlueprintJSON: blueprintJSON, BlueprintHash: blueprintHash,
		ChangeSetJSON: changeSetJSON, ChangeSetHash: changeSetHash,
		BaselineHash: teamforge.EmptyCreateBaselineHashV1, EvaluationContractHash: contractHash,
	})
	if err == nil {
		return revision, nil
	}
	revision, reloadErr := s.builds.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID)
	if reloadErr != nil {
		return teambuild.BlueprintRevision{}, fmt.Errorf("persist template blueprint revision: %w", err)
	}
	return verifyBlueprintIdentity(revision, compiled.Blueprint)
}

func verifyBlueprintIdentity(revision teambuild.BlueprintRevision, blueprint teambuild.TeamBlueprintV1) (teambuild.BlueprintRevision, error) {
	hash, err := blueprint.BlueprintHash()
	if err != nil {
		return teambuild.BlueprintRevision{}, err
	}
	changeSet, err := teamforge.CompileTemplateInstantiateChangeSetV1(teamforge.EmptyCreateBaselineV1(), blueprint)
	if err != nil {
		return teambuild.BlueprintRevision{}, err
	}
	changeSetHash, err := changeSet.TemplateInstantiateCanonicalHash()
	if err != nil {
		return teambuild.BlueprintRevision{}, err
	}
	if revision.RevisionNo != 1 || revision.BlueprintHash != hash || revision.ChangeSetHash != changeSetHash {
		return teambuild.BlueprintRevision{}, ErrIdempotencyConflict
	}
	return revision, nil
}

func (s *Service) waitForTerminal(workspaceID, buildRunID string) (Outcome, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.ready)
	defer cancel()
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		run, err := s.builds.GetBuildRun(ctx, workspaceID, buildRunID)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return pendingOutcome(buildRunID, "building"), nil
			}
			return Outcome{}, fmt.Errorf("poll template build run: %w", err)
		}
		if terminal, outcome, terminalErr := terminalOutcome(run); terminal {
			return outcome, terminalErr
		}
		select {
		case <-ctx.Done():
			return pendingOutcome(buildRunID, "building"), nil
		case <-ticker.C:
		}
	}
}

func terminalOutcome(run teambuild.TeamBuildRun) (bool, Outcome, error) {
	switch run.Status {
	case teambuild.StatusPassed:
		if run.FinalRef == nil || strings.TrimSpace(run.FinalRef.TeamID) == "" {
			return true, pendingOutcome(run.BuildRunID, run.Status), fmt.Errorf("%w: passed build has no final team reference", ErrBuildFailed)
		}
		return true, Outcome{
			TeamID: run.FinalRef.TeamID, BuildRunID: run.BuildRunID,
			Status: "ready", Evaluation: "unevaluated",
		}, nil
	case teambuild.StatusBlocked, teambuild.StatusCancelled:
		return true, pendingOutcome(run.BuildRunID, run.Status), fmt.Errorf("%w: build status is %s", ErrBuildFailed, run.Status)
	default:
		return false, Outcome{}, nil
	}
}

func pendingOutcome(buildRunID, status string) Outcome {
	return Outcome{BuildRunID: buildRunID, Status: status, ProgressURL: progressPathPrefix + buildRunID + "/progress"}
}

func templateFingerprint(template teamtemplate.Template, declarativeSpec *teamforge.DeclarativeWorkflowSpecV1) (string, error) {
	data, err := json.Marshal(struct {
		Template        teamtemplate.Template                `json:"template"`
		DeclarativeSpec *teamforge.DeclarativeWorkflowSpecV1 `json:"declarative_spec,omitempty"`
	}{Template: template, DeclarativeSpec: declarativeSpec})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func deterministicBuildRunID(workspaceID string, key uuid.UUID) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("weave:team-template:"+workspaceID+":"+key.String())).String()
}

func validationError(path, code, message string) error {
	return &teamtemplate.ValidationError{Problems: []teamtemplate.Problem{{Path: path, Code: code, Message: message}}}
}
