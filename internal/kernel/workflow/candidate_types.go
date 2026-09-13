package workflow

import (
	"github.com/jinyitao123/weave/internal/base/frozen"
	"time"
)

type CandidateInput struct {
	WorkspaceID     string
	WorkflowID      string
	WorkflowVersion int
}

type PublicationCandidate struct {
	WorkspaceID     string
	WorkflowID      string
	WorkflowVersion int

	ExpectedUpdatedAt time.Time

	ArtifactSchemaVersion     int
	CanonicalizationAlgorithm string
	CanonicalizationVersion   int
	HashAlgorithm             string
	ContentHash               string
	Payload                   frozen.ArtifactPayloadV1
	Dependencies              []TeamWorkflowDependency
}
