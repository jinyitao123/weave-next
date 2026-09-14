package teamforge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

const (
	draftKindAgentGraph   = "agent_graph"
	draftKindTeamWorkflow = "team_workflow"
)

// DraftPersistence is the durable payload boundary used by Builder drafts.
// The storage adapter owns tables and database behavior; Builder owns only
// the typed draft payloads and their workspace/build-run coordinates.
type DraftPersistence interface {
	LoadDraft(context.Context, string, string, string, string) (json.RawMessage, int64, bool, error)
	SaveDraft(context.Context, string, string, string, string, int64, json.RawMessage) (int64, error)
	DeleteDraft(context.Context, string, string, string, string, int64) error
}

// DraftRegistry binds typed draft stores to durable persistence. It keeps no
// process-local draft state.
type DraftRegistry struct {
	persistence DraftPersistence
}

// NewDraftRegistry creates a durable draft registry.
func NewDraftRegistry(persistence DraftPersistence) *DraftRegistry {
	if persistence == nil {
		panic("teamforge: draft persistence is required")
	}
	return &DraftRegistry{persistence: persistence}
}

// GraphDrafts binds graph drafts to one workspace and build run.
func (r *DraftRegistry) GraphDrafts(workspaceID, buildRunID string) *graphDraftStore {
	return &graphDraftStore{
		persistence: r.persistence,
		workspaceID: strings.TrimSpace(workspaceID),
		buildRunID:  strings.TrimSpace(buildRunID),
	}
}

// WorkflowDrafts binds workflow drafts to one workspace and build run.
func (r *DraftRegistry) WorkflowDrafts(workspaceID, buildRunID string) *workflowDraftStore {
	return &workflowDraftStore{
		persistence: r.persistence,
		workspaceID: strings.TrimSpace(workspaceID),
		buildRunID:  strings.TrimSpace(buildRunID),
	}
}

func validateDraftBinding(workspaceID, buildRunID, actualRunID, key string) error {
	if workspaceID == "" || buildRunID == "" || key == "" {
		return errors.New("draft workspace, build run, and key are required")
	}
	if actualRunID != buildRunID {
		return errors.New("draft build run does not match bound build run")
	}
	return nil
}
