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
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/build/teamtemplate"
)

const progressPathPrefix = "/v1/internal/team-build-runs/"

var (
	ErrIdempotencyConflict = errors.New("team template idempotency conflict")
	ErrBuildFailed         = errors.New("team template build failed")
	ErrUnavailable         = errors.New("team template service unavailable")
)

type Request struct {
	YAML           string         `json:"yaml,omitempty"`
	Sample         string         `json:"sample,omitempty"`
	Overrides      map[string]any `json:"overrides,omitempty"`
	IdempotencyKey string         `json:"idempotency_key"`
}

type Outcome struct {
	TeamID      string `json:"team_id,omitempty"`
	BuildRunID  string `json:"build_run_id"`
	Status      string `json:"status"`
	Evaluation  string `json:"evaluation,omitempty"`
	ProgressURL string `json:"progress_url,omitempty"`
}

type Sample struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
	YAML        string `json:"yaml"`
}

type Catalog interface {
	Resolve(name string, overrides map[string]any) ([]byte, error)
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
	Policy       teambuild.TemplateAuthorizationPolicy
	ReadyTimeout time.Duration
	PollInterval time.Duration
	RunTTL       time.Duration
	Now          func() time.Time
	Catalog      Catalog
}

type Service struct {
	idempotency IdempotencyStore
	builds      BuildStore
	submitter   Submitter
	policy      teambuild.TemplateAuthorizationPolicy
	ready       time.Duration
	poll        time.Duration
	runTTL      time.Duration
	now         func() time.Time
	catalog     Catalog
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
		runTTL: options.RunTTL, now: options.Now, catalog: options.Catalog,
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
	compiled, err := teamtemplate.CompileYAML(yamlBytes)
	if err != nil {
		return Outcome{}, err
	}
	fingerprint, err := templateFingerprint(compiled.Template)
	if err != nil {
		return Outcome{}, fmt.Errorf("fingerprint team template: %w", err)
	}
	buildRunID := deterministicBuildRunID(workspaceID, key)
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
	revision, err := s.ensureBlueprintRevision(ctx, workspaceID, buildRunID, compiled)
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
			if isTemplateAuthorizationDeferral(err) {
				return pendingOutcome(buildRunID, "authorization_required"), nil
			}
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

func isTemplateAuthorizationDeferral(err error) bool {
	return errors.Is(err, teambuild.ErrTemplateAuthorizationRequired) ||
		errors.Is(err, teambuild.ErrTemplateDailyQuotaExceeded) ||
		errors.Is(err, teambuild.ErrTemplateMonthlyQuotaExceeded) ||
		errors.Is(err, teambuild.ErrTemplateConcurrencyExceeded)
}

func templateFingerprint(template teamtemplate.Template) (string, error) {
	data, err := json.Marshal(template)
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
