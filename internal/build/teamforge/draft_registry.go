package teamforge

// DraftRegistry is the server-level shared in-memory draft table. Before T08,
// graph/workflow write dispatchers owned a private draft store per request,
// so a draft opened in one chat turn was lost on the next. The registry
// lifts the storage to one process-wide instance keyed by build_run_id: every
// dispatcher constructed for the same build run is injected with the same
// per-run draft stores, so an uncommitted draft survives across requests,
// sessions, and team-session worker dispatches. Drafts remain deliberately
// unpersisted: an uncommitted draft has no platform representation, and the
// model can always re-run tf_graph_begin / tf_wf_begin to restore context.

import "sync"

// DraftRegistry owns the per-build-run draft stores. The Server holds exactly
// one instance and passes it to every teamforge write dispatcher it builds.
type DraftRegistry struct {
	mu     sync.Mutex
	builds map[string]*buildRunDraftTables
}

// buildRunDraftTables bundles the two per-run draft stores.
type buildRunDraftTables struct {
	graph    *graphDraftStore
	workflow *workflowDraftStore
}

// NewDraftRegistry creates an empty shared draft registry.
func NewDraftRegistry() *DraftRegistry {
	return &DraftRegistry{builds: make(map[string]*buildRunDraftTables)}
}

// GraphDrafts returns the employee internal-graph draft store for one build
// run, creating it on first use. The returned store is safe for concurrent
// use and is shared by every graph write dispatcher of the same run.
func (r *DraftRegistry) GraphDrafts(buildRunID string) *graphDraftStore {
	return r.tables(buildRunID).graph
}

// WorkflowDrafts returns the team-workflow draft store for one build run,
// creating it on first use. The returned store is safe for concurrent use and
// is shared by every workflow write dispatcher of the same run.
func (r *DraftRegistry) WorkflowDrafts(buildRunID string) *workflowDraftStore {
	return r.tables(buildRunID).workflow
}

func (r *DraftRegistry) tables(buildRunID string) *buildRunDraftTables {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.builds[buildRunID]
	if entry == nil {
		entry = &buildRunDraftTables{
			graph:    newGraphDraftStore(),
			workflow: newWorkflowDraftStore(),
		}
		r.builds[buildRunID] = entry
	}
	return entry
}
