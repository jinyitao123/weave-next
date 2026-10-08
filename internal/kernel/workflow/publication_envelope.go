package workflow

import (
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// PublicationFromEnvelope rebuilds the frozen dependency rows from the exact
// content-addressed envelope. Mutable draft timestamps and product stores are
// not part of this conversion. Graph and current authorization validation are
// performed by the kernel publication service before persistence.
func PublicationFromEnvelope(envelope frozen.ArtifactEnvelopeV1) (Publication, error) {
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return Publication{}, err
	}
	manifest, err := publicationCandidateManifest(payload)
	if err != nil {
		return Publication{}, err
	}
	dependencies, err := BuildCandidateDependencies(envelope.WorkspaceID, envelope.WorkflowID,
		envelope.WorkflowVersion, manifest, payload.Bundles, payload.DeliveryTargets)
	if err != nil {
		return Publication{}, err
	}
	return PublicationFromCandidate(&PublicationCandidate{WorkspaceID: envelope.WorkspaceID,
		WorkflowID: envelope.WorkflowID, WorkflowVersion: envelope.WorkflowVersion,
		ArtifactSchemaVersion: envelope.ArtifactSchemaVersion, CanonicalizationAlgorithm: envelope.CanonicalizationAlgorithm,
		CanonicalizationVersion: envelope.CanonicalizationVersion, HashAlgorithm: envelope.HashAlgorithm,
		ContentHash: envelope.ContentHash, Payload: payload, Dependencies: dependencies})
}

// CandidateEnvelope returns the canonical content-addressed wire candidate.
// Product timestamps and evaluation/build metadata never enter the kernel input.
func CandidateEnvelope(candidate *PublicationCandidate) (frozen.ArtifactEnvelopeV1, error) {
	if _, err := PublicationFromCandidate(candidate); err != nil {
		return frozen.ArtifactEnvelopeV1{}, err
	}
	return candidateEnvelope(candidate)
}
