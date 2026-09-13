package workflow

import (
	"context"
	"time"
)

// PublicationReader exposes frozen facts without passing a product store or
// database transaction across the product/execution boundary. The concrete
// reader owns its storage and returns exact workspace-scoped identities.
type PublicationReader interface {
	GetArtifact(context.Context, string, string, int) (*PublishedArtifactContent, error)
	GetCandidateArtifact(context.Context, string, string, int, string) (*PublishedArtifactContent, error)
	GetCandidate(context.Context, string, string, string) (*PublicationCandidate, error)
	ListDependencies(context.Context, string, string, int) ([]TeamWorkflowDependency, error)
	ReadAdmission(context.Context, string, string, int) (*AdmissionState, error)
	HasHistory(context.Context, string, string) (bool, error)
}

type AdmissionState struct {
	Blocked           bool
	LatestBlockReason *string
	LatestAuditAt     *time.Time
}
