package deliverable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jinyitao123/weave/internal/base/fileartifact"
)

type VerificationStatus string

const (
	VerificationPending     VerificationStatus = "pending"
	VerificationPassed      VerificationStatus = "passed"
	VerificationFailed      VerificationStatus = "failed"
	VerificationUnknown     VerificationStatus = "unknown"
	CoverageExplicit                           = "explicit"
	CoverageIncomplete                         = "incomplete"
	ExternalEffectsNone                        = "none"
	ExternalEffectsRequired                    = "required"
)

var (
	ErrVerificationConflict    = errors.New("delivery verification identity conflict")
	ErrVerificationFence       = errors.New("delivery verification execution fence rejected")
	ErrVerificationUnavailable = errors.New("delivery verification evidence unavailable")
)

type DeliveryContract struct {
	Version                int                   `json:"version"`
	Coverage               string                `json:"coverage"`
	Output                 OutputRequirement     `json:"output"`
	RequiredArtifacts      []ArtifactRequirement `json:"required_artifacts,omitempty"`
	RequiredChecks         []CheckSpec           `json:"required_checks,omitempty"`
	ExternalEffects        string                `json:"external_effects,omitempty"`
	ExternalEffectsCheckID string                `json:"external_effects_check_id,omitempty"`
	Limitations            []string              `json:"limitations,omitempty"`
}

// DecodeDeliveryContract strictly decodes one bounded contract. It is shared
// by product templates and frozen workflow decoding so unknown fields cannot
// silently disappear at either trust boundary.
func DecodeDeliveryContract(raw []byte) (*DeliveryContract, error) {
	if len(bytes.TrimSpace(raw)) == 0 || len(raw) > 512*1024 {
		return nil, errors.New("delivery contract JSON is empty or too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var contract DeliveryContract
	if err := decoder.Decode(&contract); err != nil {
		return nil, fmt.Errorf("decode delivery contract: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("delivery contract JSON contains trailing data")
	}
	if err := ValidateDeliveryContract(&contract); err != nil {
		return nil, err
	}
	return &contract, nil
}

func CloneDeliveryContract(contract *DeliveryContract) *DeliveryContract {
	if contract == nil {
		return nil
	}
	cloned := *contract
	cloned.Output.Schema = bytes.Clone(contract.Output.Schema)
	cloned.RequiredArtifacts = append([]ArtifactRequirement(nil), contract.RequiredArtifacts...)
	for i := range cloned.RequiredArtifacts {
		cloned.RequiredArtifacts[i].Contains = append([]string(nil), contract.RequiredArtifacts[i].Contains...)
	}
	cloned.RequiredChecks = append([]CheckSpec(nil), contract.RequiredChecks...)
	for i := range cloned.RequiredChecks {
		cloned.RequiredChecks[i].Parameters = bytes.Clone(contract.RequiredChecks[i].Parameters)
	}
	cloned.Limitations = append([]string(nil), contract.Limitations...)
	return &cloned
}

type OutputRequirement struct {
	Type   string          `json:"type,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

type ArtifactRequirement struct {
	ID          string   `json:"id"`
	Path        string   `json:"path"`
	ContentType string   `json:"content_type,omitempty"`
	SHA256      string   `json:"sha256,omitempty"`
	Contains    []string `json:"contains,omitempty"`
}

type CheckSpec struct {
	ID              string          `json:"id"`
	VerifierID      string          `json:"verifier_id"`
	VerifierVersion string          `json:"verifier_version"`
	Parameters      json.RawMessage `json:"parameters,omitempty"`
}

type ContractBinding struct {
	WorkspaceID     string            `json:"workspace_id"`
	RunID           string            `json:"run_id,omitempty"`
	RunSnapshotID   string            `json:"run_snapshot_id"`
	InputRevisionID string            `json:"input_revision_id,omitempty"`
	WorkflowID      string            `json:"workflow_id"`
	WorkflowVersion int               `json:"workflow_version"`
	PublishedDigest string            `json:"published_digest"`
	Contract        *DeliveryContract `json:"contract"`
}

type VerificationFence struct {
	WorkspaceID         string `json:"workspace_id"`
	RunID               string `json:"run_id"`
	RunSnapshotID       string `json:"run_snapshot_id"`
	TeamRunGeneration   int64  `json:"team_run_generation"`
	ExecutionLeaseEpoch int64  `json:"execution_lease_epoch"`
	ResumeGeneration    int64  `json:"resume_generation"`
	ExecutorID          string `json:"executor_id"`
}

type CandidateArtifact struct {
	Path        string           `json:"path"`
	ContentType string           `json:"content_type"`
	SHA256      string           `json:"sha256"`
	Sources     []ArtifactSource `json:"sources"`
	Content     string           `json:"-"`
}

// Candidate persists a content manifest. Actual bytes are only passed to trusted
// verifiers and continue to be stored in the existing deliverable ledger.
type Candidate struct {
	WorkspaceID        string              `json:"workspace_id"`
	RunID              string              `json:"run_id"`
	RunSnapshotID      string              `json:"run_snapshot_id"`
	NodeID             string              `json:"node_id"`
	OutputDigest       string              `json:"output_digest"`
	Artifacts          []CandidateArtifact `json:"artifacts"`
	Sources            []ArtifactSource    `json:"sources"`
	OutputSources      []ArtifactSource    `json:"output_sources"`
	SourceObservations []SourceObservation `json:"source_observations"`
	Selection          *OutputSelection    `json:"selection,omitempty"`
	Output             json.RawMessage     `json:"-"`
}

// OutputSelection is supplied only by frozen workflow evaluation for values
// selected directly from a literal or run input rather than a physical member.
type OutputSelection struct {
	Kind        string `json:"kind"`
	ValueDigest string `json:"value_digest"`
}

type SourceObservation struct {
	Source     ArtifactSource                   `json:"source"`
	Collection *fileartifact.CollectionEvidence `json:"collection,omitempty"`
}

type CheckResult struct {
	CheckID         string             `json:"check_id"`
	VerifierID      string             `json:"verifier_id"`
	VerifierVersion string             `json:"verifier_version"`
	Status          VerificationStatus `json:"status"`
	Reason          string             `json:"reason"`
	Evidence        json.RawMessage    `json:"evidence,omitempty"`
}

type VerificationReport struct {
	ID             string             `json:"id"`
	RevisionID     string             `json:"revision_id"`
	ContractDigest string             `json:"contract_digest"`
	Status         VerificationStatus `json:"status"`
	Candidate      Candidate          `json:"candidate"`
	Checks         []CheckResult      `json:"checks"`
	Fence          VerificationFence  `json:"fence"`
	CreatedAt      time.Time          `json:"created_at"`
	// Preserve JSON text as a string because JSONB normalizes number spellings.
	// This is the exact selected final value, not a second copy of file contents.
	OutputJSON string              `json:"output_json,omitempty"`
	Recheck    *RecheckObservation `json:"recheck,omitempty"`
}

type RecheckObservation struct {
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

type RecheckRequest struct {
	WorkspaceID    string `json:"workspace_id"`
	RunID          string `json:"run_id"`
	RevisionID     string `json:"revision_id"`
	ContractDigest string `json:"contract_digest"`
}

type RecheckResult struct {
	Report  VerificationReport `json:"report"`
	Current bool               `json:"current"`
}

type DeliveryState struct {
	Binding           ContractBinding     `json:"binding"`
	ContractDigest    string              `json:"contract_digest"`
	RevisionID        string              `json:"revision_id,omitempty"`
	VerificationID    string              `json:"verification_id,omitempty"`
	SelectionSequence int64               `json:"selection_sequence"`
	Report            *VerificationReport `json:"report,omitempty"`
}

type VerificationInput struct {
	Contract  DeliveryContract
	Candidate Candidate
	Check     CheckSpec
}

// Verifier is trusted application code. Registration must only expose bounded,
// read-only observations; model output is never accepted as a verification receipt.
type Verifier func(context.Context, VerificationInput) (CheckResult, error)

type VerifierRegistry struct {
	mu      sync.RWMutex
	entries map[string]registeredVerifier
}

type registeredVerifier struct {
	verify          Verifier
	externalEffects bool
}

func NewVerifierRegistry() *VerifierRegistry {
	return &VerifierRegistry{entries: map[string]registeredVerifier{}}
}

func (r *VerifierRegistry) Register(id, version string, verifier Verifier) error {
	return r.register(id, version, verifier, false)
}

// RegisterExternalEffects is an explicit trusted-code capability declaration.
// An ordinary schema/content verifier cannot be selected as proof of effects.
func (r *VerifierRegistry) RegisterExternalEffects(id, version string, verifier Verifier) error {
	return r.register(id, version, verifier, true)
}

func (r *VerifierRegistry) register(id, version string, verifier Verifier, externalEffects bool) error {
	if r == nil || strings.TrimSpace(id) == "" || strings.TrimSpace(version) == "" || verifier == nil {
		return errors.New("verifier identity and implementation required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := id + "\x00" + version
	if _, exists := r.entries[key]; exists {
		return errors.New("verifier version already registered")
	}
	if r.entries == nil {
		r.entries = map[string]registeredVerifier{}
	}
	r.entries[key] = registeredVerifier{verify: verifier, externalEffects: externalEffects}
	return nil
}

func (r *VerifierRegistry) lookup(id, version string) registeredVerifier {
	if r == nil {
		return registeredVerifier{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[id+"\x00"+version]
}

// CanonicalJSONDigest does not use floating point when hashing persistent JSON.
func CanonicalJSONDigest(raw []byte) (string, error) {
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return "", err
	}
	return digest(canonical), nil
}

func canonicalJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("JSON contains trailing data")
	}
	return json.Marshal(value)
}

func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func validDigest(s string) bool {
	decoded, err := hex.DecodeString(s)
	return err == nil && len(decoded) == sha256.Size && s == strings.ToLower(s)
}

func ValidateDeliveryContract(contract *DeliveryContract) error {
	if contract == nil {
		return nil
	}
	if contract.Version != 1 || (contract.Coverage != CoverageExplicit && contract.Coverage != CoverageIncomplete) {
		return errors.New("unsupported delivery contract version or coverage")
	}
	if len(contract.RequiredArtifacts) > fileartifact.MaxArtifactCount || len(contract.RequiredChecks) > 128 || len(contract.Limitations) > 128 {
		return errors.New("delivery contract is too large")
	}
	if contract.Coverage == CoverageExplicit && len(contract.RequiredArtifacts) == 0 && len(contract.RequiredChecks) == 0 {
		return errors.New("explicit delivery coverage requires at least one artifact or check")
	}
	if len(contract.Output.Schema) > 256*1024 || (len(contract.Output.Schema) > 0 && !json.Valid(contract.Output.Schema)) {
		return errors.New("invalid output schema JSON")
	}
	switch contract.Output.Type {
	case "", "text", "json", "boolean", "number":
	default:
		return errors.New("unsupported output type")
	}
	ids, paths := map[string]bool{}, map[string]bool{}
	for _, item := range contract.RequiredArtifacts {
		if item.ID == "" || strings.HasPrefix(item.ID, "_") || strings.HasPrefix(item.ID, "artifact:") || ids[item.ID] || paths[item.Path] || (item.SHA256 != "" && !validDigest(item.SHA256)) {
			return errors.New("invalid or duplicate required artifact")
		}
		if err := fileartifact.Validate([]fileartifact.File{{Path: item.Path, ContentType: "application/octet-stream"}}); err != nil {
			return err
		}
		ids[item.ID], paths[item.Path] = true, true
		for _, needle := range item.Contains {
			if needle == "" {
				return errors.New("empty artifact content condition")
			}
		}
	}
	switch contract.ExternalEffects {
	case "": // Legacy contracts retain the previous unknown-or-check behavior.
	case ExternalEffectsNone:
		if contract.ExternalEffectsCheckID != "" {
			return errors.New("external effects none forbids an effects check")
		}
	case ExternalEffectsRequired:
		if contract.ExternalEffectsCheckID == "" {
			return errors.New("external effects required needs an effects check")
		}
	default:
		return errors.New("unsupported external effects mode")
	}
	effectsFound := contract.ExternalEffectsCheckID == ""
	for _, check := range contract.RequiredChecks {
		if check.ID == "" || strings.HasPrefix(check.ID, "_") || strings.HasPrefix(check.ID, "artifact:") || ids[check.ID] || check.VerifierID == "" || check.VerifierVersion == "" {
			return errors.New("invalid or duplicate required check")
		}
		if len(check.Parameters) > 256*1024 || (len(check.Parameters) > 0 && !json.Valid(check.Parameters)) {
			return errors.New("invalid verifier parameters")
		}
		ids[check.ID] = true
		if check.ID == contract.ExternalEffectsCheckID {
			effectsFound = true
		}
	}
	if !effectsFound {
		return errors.New("external effects check must reference a required check")
	}
	return nil
}

func contractDigest(contract *DeliveryContract) (string, error) {
	if err := ValidateDeliveryContract(contract); err != nil {
		return "", err
	}
	raw, err := json.Marshal(contract)
	if err != nil {
		return "", err
	}
	return CanonicalJSONDigest(raw)
}

func normalizedSources(sources []ArtifactSource) []ArtifactSource {
	byKey := map[string]ArtifactSource{}
	for _, source := range sources {
		raw, _ := json.Marshal(source)
		byKey[string(raw)] = source
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]ArtifactSource, 0, len(keys))
	for _, key := range keys {
		result = append(result, byKey[key])
	}
	return result
}

func BuildCandidate(outputs []WorkflowOutput) (Candidate, error) {
	if len(outputs) == 0 {
		return Candidate{}, errors.New("final delivery bundle is empty")
	}
	first := outputs[0]
	c := Candidate{WorkspaceID: first.WorkspaceID, RunID: first.RunID, RunSnapshotID: first.RunSnapshotID, NodeID: first.NodeID, Artifacts: []CandidateArtifact{}}
	if c.WorkspaceID == "" || c.RunID == "" || c.RunSnapshotID == "" || c.NodeID == "" {
		return c, errors.New("candidate identity incomplete")
	}
	summaries := 0
	paths := map[string]bool{}
	files := []fileartifact.File{}
	for _, output := range outputs {
		if !output.Final || output.NodeType != "deliver" || output.WorkspaceID != c.WorkspaceID || output.RunID != c.RunID || output.RunSnapshotID != c.RunSnapshotID || output.NodeID != c.NodeID {
			return c, errors.New("candidate contains mismatched final outputs")
		}
		c.Sources = append(c.Sources, output.Sources...)
		if output.Artifact == nil {
			summaries++
			c.OutputSources = normalizedSources(output.Sources)
			for _, observation := range output.SourceObservations {
				if err := fileartifact.ValidateCollectionEvidence(observation.Collection); err != nil {
					return c, err
				}
				raw, _ := json.Marshal(observation)
				var copy SourceObservation
				_ = json.Unmarshal(raw, &copy)
				c.SourceObservations = append(c.SourceObservations, copy)
				c.OutputSources = append(c.OutputSources, observation.Source)
				c.Sources = append(c.Sources, observation.Source)
			}
			c.OutputSources = normalizedSources(c.OutputSources)
			if output.Selection != nil {
				copy := *output.Selection
				c.Selection = &copy
			}
			raw, err := json.Marshal(output.Output)
			if err != nil {
				return c, err
			}
			c.Output, err = canonicalJSON(raw)
			if err != nil {
				return c, err
			}
			c.OutputDigest = digest(c.Output)
			continue
		}
		a := output.Artifact
		if paths[a.Path] {
			return c, errors.New("duplicate candidate file path")
		}
		paths[a.Path] = true
		files = append(files, fileartifact.File{Path: a.Path, ContentType: a.ContentType, Content: a.Content})
		c.Artifacts = append(c.Artifacts, CandidateArtifact{Path: a.Path, ContentType: a.ContentType, SHA256: digest([]byte(a.Content)), Sources: normalizedSources(a.Sources), Content: a.Content})
		c.Sources = append(c.Sources, a.Sources...)
	}
	if summaries != 1 {
		return c, errors.New("candidate requires exactly one final value")
	}
	if err := fileartifact.Validate(files); err != nil {
		return c, err
	}
	sort.Slice(c.Artifacts, func(i, j int) bool { return c.Artifacts[i].Path < c.Artifacts[j].Path })
	c.Sources = normalizedSources(c.Sources)
	sort.Slice(c.SourceObservations, func(i, j int) bool {
		a, _ := json.Marshal(c.SourceObservations[i].Source)
		b, _ := json.Marshal(c.SourceObservations[j].Source)
		return string(a) < string(b)
	})
	for i := 1; i < len(c.SourceObservations); i++ {
		if c.SourceObservations[i-1].Source == c.SourceObservations[i].Source {
			return c, errors.New("duplicate source observation")
		}
	}
	return c, nil
}

// Verify executes all required supported checks; any unresolved requirement
// prevents aggregate success. It does not publish or alter execution state.
func Verify(ctx context.Context, contract *DeliveryContract, candidate Candidate, registry *VerifierRegistry) (VerificationReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cd, err := contractDigest(contract)
	if err != nil {
		return VerificationReport{}, err
	}
	if candidate.OutputDigest != digest(candidate.Output) || !json.Valid(candidate.Output) {
		return VerificationReport{}, invalidVerification("candidate output digest mismatch")
	}
	for _, artifact := range candidate.Artifacts {
		if artifact.SHA256 != digest([]byte(artifact.Content)) {
			return VerificationReport{}, invalidVerification("candidate file digest mismatch")
		}
	}
	raw, err := json.Marshal(candidate)
	if err != nil {
		return VerificationReport{}, err
	}
	report := VerificationReport{RevisionID: "delivery_" + digest(raw), ContractDigest: cd, Candidate: candidate, CreatedAt: time.Now().UTC(), OutputJSON: string(candidate.Output)}
	add := func(id string, status VerificationStatus, reason string, evidence any) {
		raw, _ := json.Marshal(evidence)
		report.Checks = append(report.Checks, CheckResult{CheckID: id, VerifierID: "weave.delivery", VerifierVersion: "v1", Status: status, Reason: reason, Evidence: raw})
	}
	if contract == nil {
		add("_contract", VerificationUnknown, "contract_missing", nil)
		return finalizeReport(report), nil
	}
	if contract.Coverage != CoverageExplicit {
		add("_coverage", VerificationUnknown, "requirements_not_explicit", nil)
	} else {
		add("_coverage", VerificationPassed, "explicit_requirements", nil)
	}
	for index, limitation := range contract.Limitations {
		add(fmt.Sprintf("_limitation:%d", index), VerificationUnknown, "unsupported_requirement", limitation)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(candidate.Output))
	decoder.UseNumber()
	err = decoder.Decode(&value)
	outputOK := err == nil
	switch contract.Output.Type {
	case "text":
		_, outputOK = value.(string)
	case "boolean":
		_, outputOK = value.(bool)
	case "number":
		_, outputOK = value.(json.Number)
	case "json":
	case "":
		add("_output", VerificationUnknown, "output_contract_missing", nil)
	}
	if contract.Output.Type != "" {
		if outputOK {
			add("_output", VerificationPassed, "output_type_valid", nil)
		} else {
			add("_output", VerificationFailed, "output_type_invalid", nil)
		}
	}
	if len(contract.Output.Schema) > 0 {
		report.Checks = append(report.Checks, runVerifier(ctx, registry, VerificationInput{Contract: *contract, Candidate: candidate, Check: CheckSpec{ID: "_schema", VerifierID: "weave.output-schema", VerifierVersion: "v1", Parameters: contract.Output.Schema}}))
	}
	byPath := map[string]CandidateArtifact{}
	for _, artifact := range candidate.Artifacts {
		byPath[artifact.Path] = artifact
	}
	for _, required := range contract.RequiredArtifacts {
		artifact, exists := byPath[required.Path]
		if !exists {
			status, reason := VerificationFailed, "required_file_missing"
			for _, observation := range candidate.SourceObservations {
				if observation.Collection == nil || observation.Collection.Complete {
					continue
				}
				for _, issue := range observation.Collection.Issues {
					if (issue.Kind == "limit" || issue.Kind == "error") && (issue.Path == "" || issue.Path == required.Path) {
						status, reason = VerificationUnknown, "required_file_collection_limited"
					}
				}
			}
			add("artifact:"+required.ID, status, reason, required.Path)
			continue
		}
		status, reason := VerificationPassed, "file_conditions_satisfied"
		if required.ContentType != "" && artifact.ContentType != required.ContentType {
			status, reason = VerificationFailed, "file_content_type_mismatch"
		}
		if required.SHA256 != "" && artifact.SHA256 != required.SHA256 {
			status, reason = VerificationFailed, "file_digest_mismatch"
		}
		for _, needle := range required.Contains {
			if !strings.Contains(artifact.Content, needle) {
				status, reason = VerificationFailed, "file_content_condition_missing"
			}
		}
		add("artifact:"+required.ID, status, reason, map[string]string{"path": artifact.Path, "sha256": artifact.SHA256})
	}
	for _, artifact := range candidate.Artifacts {
		if len(artifact.Sources) == 0 {
			add("_provenance:"+artifact.Path, VerificationUnknown, "file_source_missing", artifact.Path)
		}
	}
	for index, source := range candidate.Sources {
		if source.RunSnapshotID != candidate.RunSnapshotID || source.ParentRunID != candidate.RunID || !validDigest(source.ResultDigest) || (source.TaskID == "") == (source.MemberRunID == "") {
			add(fmt.Sprintf("_provenance:%d", index), VerificationFailed, "source_identity_invalid", source)
		}
	}
	if candidate.Selection != nil {
		if (candidate.Selection.Kind != "literal" && candidate.Selection.Kind != "run_input") || candidate.Selection.ValueDigest != candidate.OutputDigest {
			add("_selection", VerificationFailed, "frozen_value_selection_invalid", nil)
		}
	} else if len(candidate.OutputSources) == 0 {
		add("_selection", VerificationUnknown, "output_source_missing", nil)
	}
	externalFound := contract.ExternalEffects == ExternalEffectsNone
	if externalFound {
		add("_external_effects", VerificationPassed, "no_external_effects_required", nil)
	}
	for _, check := range contract.RequiredChecks {
		if check.ID == contract.ExternalEffectsCheckID {
			externalFound = true
			if !registry.lookup(check.VerifierID, check.VerifierVersion).externalEffects {
				report.Checks = append(report.Checks, CheckResult{CheckID: check.ID, VerifierID: check.VerifierID, VerifierVersion: check.VerifierVersion, Status: VerificationUnknown, Reason: "external_effects_verifier_unavailable"})
				continue
			}
		}
		report.Checks = append(report.Checks, runVerifier(ctx, registry, VerificationInput{Contract: *contract, Candidate: candidate, Check: check}))
	}
	if !externalFound {
		add("_external_effects", VerificationUnknown, "external_effects_scope_unverified", nil)
	}
	return finalizeReport(report), nil
}

func runVerifier(ctx context.Context, registry *VerifierRegistry, input VerificationInput) CheckResult {
	result := CheckResult{CheckID: input.Check.ID, VerifierID: input.Check.VerifierID, VerifierVersion: input.Check.VerifierVersion, Status: VerificationUnknown, Reason: "verifier_unavailable"}
	fn := registry.lookup(input.Check.VerifierID, input.Check.VerifierVersion).verify
	if fn == nil {
		return result
	}
	// Adapters receive private copies, so they cannot rewrite the frozen contract
	// or the candidate subsequently committed by this package.
	contractRaw, _ := json.Marshal(input.Contract)
	var privateContract DeliveryContract
	_ = json.Unmarshal(contractRaw, &privateContract)
	input.Contract = privateContract
	input.Candidate.Artifacts = append([]CandidateArtifact(nil), input.Candidate.Artifacts...)
	for i := range input.Candidate.Artifacts {
		input.Candidate.Artifacts[i].Sources = append([]ArtifactSource(nil), input.Candidate.Artifacts[i].Sources...)
	}
	input.Candidate.Sources = append([]ArtifactSource(nil), input.Candidate.Sources...)
	input.Candidate.OutputSources = append([]ArtifactSource(nil), input.Candidate.OutputSources...)
	observationsRaw, _ := json.Marshal(input.Candidate.SourceObservations)
	var privateObservations []SourceObservation
	_ = json.Unmarshal(observationsRaw, &privateObservations)
	input.Candidate.SourceObservations = privateObservations
	input.Candidate.Output = bytes.Clone(input.Candidate.Output)
	if input.Candidate.Selection != nil {
		copy := *input.Candidate.Selection
		input.Candidate.Selection = &copy
	}
	input.Check.Parameters = bytes.Clone(input.Check.Parameters)
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if bounded.Err() != nil {
		result.Reason = "verifier_observation_unavailable"
		return result
	}
	type observation struct {
		result CheckResult
		err    error
	}
	observed := make(chan observation, 1)
	go func() {
		var outcome observation
		defer func() {
			if recovered := recover(); recovered != nil {
				outcome.err = errors.New("verifier panicked")
			}
			observed <- outcome
		}()
		outcome.result, outcome.err = fn(bounded, input)
	}()
	var checked CheckResult
	var err error
	select {
	case outcome := <-observed:
		checked, err = outcome.result, outcome.err
	case <-bounded.Done():
		result.Reason = "verifier_observation_unavailable"
		return result
	}
	if err != nil || bounded.Err() != nil {
		result.Reason = "verifier_observation_unavailable"
		return result
	}
	if checked.Status != VerificationPassed && checked.Status != VerificationFailed && checked.Status != VerificationUnknown {
		result.Reason = "verifier_result_invalid"
		return result
	}
	evidence, evidenceErr := canonicalJSON(checked.Evidence)
	if evidenceErr != nil || len(evidence) == 0 || (evidence[0] != '{' && evidence[0] != '[') || len(evidence) > 512*1024 {
		result.Reason = "verifier_evidence_missing"
		return result
	}
	result.Status, result.Reason, result.Evidence = checked.Status, checked.Reason, evidence
	return result
}

func finalizeReport(report VerificationReport) VerificationReport {
	report.Status = VerificationPassed
	for _, check := range report.Checks {
		if check.Status == VerificationFailed {
			report.Status = VerificationFailed
			break
		}
		if check.Status != VerificationPassed {
			report.Status = VerificationUnknown
		}
	}
	if len(report.Checks) == 0 {
		report.Status = VerificationUnknown
	}
	// Observation time and execution lease are not content identity. Replays of
	// identical checks reuse the same immutable report rather than inventing work.
	raw, _ := json.Marshal(struct {
		Revision, Contract string
		Checks             []CheckResult
	}{report.RevisionID, report.ContractDigest, report.Checks})
	report.ID = "verification_" + digest(raw)
	return report
}

func invalidVerification(format string, args ...any) error {
	return fmt.Errorf("invalid delivery verification: "+format, args...)
}
