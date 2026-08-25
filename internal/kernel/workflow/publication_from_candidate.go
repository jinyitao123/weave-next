package workflow

import (
	"errors"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

var (
	errPublicationCandidateRequired  = errors.New("publication candidate is required")
	errPublicationDependencyMismatch = errors.New(
		"publication candidate dependencies do not match rebuilt dependencies",
	)
)

type publicationCandidateError struct {
	code  string
	cause error
}

type publicationManifestIdentity struct {
	workspaceID          string
	ownerType            string
	ownerID              string
	ownerAgentVersion    int64
	hasOwnerAgentVersion bool
	dependencyType       string
	dependencyKey        string
	dependencyVersion    int64
	hasDependencyVersion bool
}

type publicationDependencyTuple struct {
	workspaceID          string
	workflowID           string
	workflowVersion      int
	ownerType            string
	ownerID              string
	ownerAgentVersion    int64
	hasOwnerAgentVersion bool
	dependencyType       string
	dependencyKey        string
	dependencyVersion    int64
	hasDependencyVersion bool
	contentHash          string
}

func (e *publicationCandidateError) Error() string {
	if e == nil {
		return ""
	}
	if e.cause == nil {
		return e.code
	}
	return e.code + ": " + e.cause.Error()
}

func (e *publicationCandidateError) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *publicationCandidateError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// PublicationFromCandidate validates and converts a publication candidate into
// the immutable facts accepted by the publication store.
func PublicationFromCandidate(candidate *PublicationCandidate) (Publication, error) {
	if candidate == nil {
		return Publication{}, newPublicationCandidateError(
			frozen.CodeArtifactInvalid,
			errPublicationCandidateRequired,
		)
	}

	payloadBytes, err := frozen.Canonicalize(candidate.Payload, frozen.PreorderArtifactPayloadV1)
	if err != nil {
		return Publication{}, newPublicationCandidateError(frozen.CodeArtifactInvalid, err)
	}
	envelope := frozen.ArtifactEnvelopeV1{
		WorkspaceID: candidate.WorkspaceID, WorkflowID: candidate.WorkflowID,
		WorkflowVersion:           candidate.WorkflowVersion,
		ArtifactSchemaVersion:     candidate.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: candidate.CanonicalizationAlgorithm,
		CanonicalizationVersion:   candidate.CanonicalizationVersion,
		HashAlgorithm:             candidate.HashAlgorithm,
		ContentHash:               candidate.ContentHash,
		Payload:                   payloadBytes,
	}
	if _, err := frozen.DecodeArtifactEnvelopeV1(envelope); err != nil {
		return Publication{}, err
	}

	manifest, err := publicationCandidateManifest(candidate.Payload)
	if err != nil {
		return Publication{}, newPublicationCandidateError(
			machine.CodeFrozenManifestMismatch,
			err,
		)
	}
	rebuilt, err := BuildCandidateDependencies(
		candidate.WorkspaceID,
		candidate.WorkflowID,
		candidate.WorkflowVersion,
		manifest,
		candidate.Payload.Bundles,
		candidate.Payload.DeliveryTargets,
	)
	if err != nil {
		return Publication{}, newPublicationCandidateError(
			machine.CodeFrozenManifestMismatch,
			err,
		)
	}
	if !equalPublicationDependencySets(rebuilt, candidate.Dependencies) {
		return Publication{}, newPublicationCandidateError(
			machine.CodeFrozenManifestMismatch,
			errPublicationDependencyMismatch,
		)
	}

	return Publication{
		WorkspaceID:       candidate.WorkspaceID,
		WorkflowID:        candidate.WorkflowID,
		WorkflowVersion:   candidate.WorkflowVersion,
		ExpectedUpdatedAt: candidate.ExpectedUpdatedAt,
		Dependencies:      rebuilt,
		Artifact: PublishedArtifactContent{
			WorkspaceID: candidate.WorkspaceID, WorkflowID: candidate.WorkflowID,
			WorkflowVersion:           candidate.WorkflowVersion,
			ArtifactSchemaVersion:     candidate.ArtifactSchemaVersion,
			CanonicalizationAlgorithm: candidate.CanonicalizationAlgorithm,
			CanonicalizationVersion:   candidate.CanonicalizationVersion,
			HashAlgorithm:             candidate.HashAlgorithm,
			ContentHash:               candidate.ContentHash,
			Payload:                   payloadBytes,
		},
	}, nil
}

func publicationCandidateManifest(
	payload frozen.ArtifactPayloadV1,
) (frozen.FrozenDependencyManifest, error) {
	merged := make([]frozen.FrozenDependencyRef, 0)
	seen := make(map[publicationManifestIdentity]string)
	appendDependency := func(dependency frozen.FrozenDependencyRef) {
		identity := publicationDependencyIdentity(dependency)
		if contentHash, exists := seen[identity]; exists && contentHash == dependency.ContentHash {
			return
		}
		seen[identity] = dependency.ContentHash
		merged = append(merged, dependency)
	}
	if payload.Team.LeadAgentVersion > 0 && payload.Team.LeadAgentContentHash != "" {
		version := payload.Team.LeadAgentVersion
		appendDependency(frozen.FrozenDependencyRef{
			EnumeratedDependencyRef: frozen.EnumeratedDependencyRef{
				WorkspaceID: payload.Team.WorkspaceID,
				OwnerType:   "agent", OwnerID: payload.Team.LeadAgentID,
				OwnerAgentVersion: &version, DependencyType: "agent",
				DependencyKey: payload.Team.LeadAgentID, DependencyVersion: &version,
			},
			ContentHash: payload.Team.LeadAgentContentHash,
		})
	}
	for _, bundle := range payload.Bundles {
		for _, dependency := range bundle.Dependencies.Dependencies {
			appendDependency(dependency)
		}
	}
	_, manifest, err := frozen.BuildFrozenDependencyManifest(merged)
	return manifest, err
}

func equalPublicationDependencySets(
	rebuilt []TeamWorkflowDependency,
	candidate []TeamWorkflowDependency,
) bool {
	if len(rebuilt) != len(candidate) {
		return false
	}
	for i := range rebuilt {
		if publicationDependencyValue(rebuilt[i]) != publicationDependencyValue(candidate[i]) {
			return false
		}
	}
	return true
}

func publicationDependencyValue(dependency TeamWorkflowDependency) publicationDependencyTuple {
	ownerVersion, hasOwnerVersion := publicationVersionValue(dependency.OwnerAgentVersion)
	dependencyVersion, hasDependencyVersion := publicationVersionValue(dependency.DependencyVersion)
	return publicationDependencyTuple{
		workspaceID:          dependency.WorkspaceID,
		workflowID:           dependency.WorkflowID,
		workflowVersion:      dependency.WorkflowVersion,
		ownerType:            dependency.OwnerType,
		ownerID:              dependency.OwnerID,
		ownerAgentVersion:    ownerVersion,
		hasOwnerAgentVersion: hasOwnerVersion,
		dependencyType:       dependency.DependencyType,
		dependencyKey:        dependency.DependencyKey,
		dependencyVersion:    dependencyVersion,
		hasDependencyVersion: hasDependencyVersion,
		contentHash:          dependency.ContentHash,
	}
}

func publicationDependencyIdentity(
	dependency frozen.FrozenDependencyRef,
) publicationManifestIdentity {
	ownerVersion, hasOwnerVersion := publicationVersionValue(dependency.OwnerAgentVersion)
	dependencyVersion, hasDependencyVersion := publicationVersionValue(dependency.DependencyVersion)
	return publicationManifestIdentity{
		workspaceID:          dependency.WorkspaceID,
		ownerType:            dependency.OwnerType,
		ownerID:              dependency.OwnerID,
		ownerAgentVersion:    ownerVersion,
		hasOwnerAgentVersion: hasOwnerVersion,
		dependencyType:       dependency.DependencyType,
		dependencyKey:        dependency.DependencyKey,
		dependencyVersion:    dependencyVersion,
		hasDependencyVersion: hasDependencyVersion,
	}
}

func publicationVersionValue(version *int64) (int64, bool) {
	if version == nil {
		return 0, false
	}
	return *version, true
}

func newPublicationCandidateError(code string, cause error) error {
	return &publicationCandidateError{code: code, cause: cause}
}
