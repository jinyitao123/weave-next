package workflowcatalog

import (
	"context"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

// ResolvePublished finishes all independent storage reads before the caller
// opens a product transaction. Preparation must then compare the exact version
// against the locked catalog identity; it cannot silently follow a new pointer.
func (s *Store) ResolvePublished(ctx context.Context, workspaceID, workflowID string, version *int) (frozen.ArtifactEnvelopeV1, error) {
	identity, err := s.Get(ctx, workspaceID, workflowID)
	if err != nil {
		return frozen.ArtifactEnvelopeV1{}, err
	}
	if identity.Status == workflow.WorkflowStatusArchived {
		return frozen.ArtifactEnvelopeV1{}, workflow.ErrArchived
	}
	selected := identity.PublishedVersion
	if version != nil {
		selected = version
	}
	if selected == nil {
		return frozen.ArtifactEnvelopeV1{}, workflow.ErrNotPublished
	}
	return s.readWorkflowScheduleArtifact(ctx, workspaceID, workflowID, *selected)
}
