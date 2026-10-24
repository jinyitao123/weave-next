package capabilities

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	kernelcredentials "github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

// RuntimeCredentialAuthorizations remembers, per run attempt and credential,
// that a model call's runtime credential passed the full check and the
// authorization versions (weave_authorization_epochs) read just before it.
// A later call of the same attempt reads only those versions, by key, and
// checks in full again when one moved. Every change that can withdraw the
// authorization moves a version in its own transaction, so a revocation
// still refuses the next call. Nothing secret is kept.
type RuntimeCredentialAuthorizations struct {
	// Team, when set, adds the published team scope to the full check.
	Team RuntimeTeamAuthority

	mu      sync.Mutex
	entries map[runtimeAuthorizationKey]runtimeAuthorizationEntry
}

// RuntimeTeamAuthority checks a runtime credential against the team scope
// the run's published workflow froze: the team and its lead, the members and
// their exact versions, and that a member still selects the provider.
type RuntimeTeamAuthority interface {
	PrepareRuntimeCredential(context.Context, pgx.Tx, teamrun.TeamRun, frozen.CredentialReference) (RuntimeTeamCheck, error)
}

// RuntimeTeamCheck names the team a team check depends on and runs it in the
// resolver's transaction. Exact member versions are immutable.
type RuntimeTeamCheck struct {
	TeamID    string
	Authorize func(context.Context, pgx.Tx) error
}

// runtimeAuthorizationsLimit bounds the entries one process keeps. Entries of
// finished attempts are never read again, so dropping all of them at the
// bound only costs the attempts in flight one more full check.
const runtimeAuthorizationsLimit = 8192

type runtimeAuthorizationKey struct {
	workspaceID, applicationID, runID, runSnapshotID, sourceTaskID, workflowID string
	workflowVersion                                                            int
	attempt                                                                    teamrun.ExecutionLeaseEpoch
	subject                                                                    execution.Subject
	ref                                                                        frozen.CredentialReference
}

type authorizationScope struct{ kind, id string }

type runtimeAuthorizationEntry struct {
	scopes   []authorizationScope
	versions []string
}

// UseRuntimeAuthorizations makes runtime credential checks of this service
// reuse an attempt's earlier full check while its versions hold.
func (s *WorkflowInvocationService) UseRuntimeAuthorizations(authorizations *RuntimeCredentialAuthorizations) {
	if s != nil {
		s.runtimeAuthorizations = authorizations
	}
}

func (a *RuntimeCredentialAuthorizations) lookup(key runtimeAuthorizationKey) (runtimeAuthorizationEntry, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.entries[key]
	return entry, ok
}

func (a *RuntimeCredentialAuthorizations) remember(key runtimeAuthorizationKey, entry runtimeAuthorizationEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.entries == nil || len(a.entries) >= runtimeAuthorizationsLimit {
		a.entries = make(map[runtimeAuthorizationKey]runtimeAuthorizationEntry)
	}
	a.entries[key] = entry
}

func (a *RuntimeCredentialAuthorizations) forget(key runtimeAuthorizationKey) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.entries, key)
}

// readAuthorizationVersions reads the versions of the given scopes and the
// identity of the current task in one statement of primary-key lookups. A
// scope that never changed has no row and no entry.
func readAuthorizationVersions(
	ctx context.Context,
	queries kernelcredentials.ReferenceAuthorizationQueries,
	workspaceID string,
	scopes []authorizationScope,
	taskID string,
) ([]string, scannedTaskAuthorizationIdentity, error) {
	kinds := make([]string, len(scopes))
	ids := make([]string, len(scopes))
	for index, scope := range scopes {
		kinds[index], ids[index] = scope.kind, scope.id
	}
	var versions []string
	var current scannedTaskAuthorizationIdentity
	targets := append([]any{&versions}, current.targets()...)
	err := queries.QueryRow(ctx, `SELECT ARRAY(SELECT e.scope_kind||chr(31)||e.scope_id||chr(31)||e.epoch::text
	   FROM weave_authorization_epochs e
	   WHERE e.workspace_id=$1 AND (e.scope_kind,e.scope_id) IN (SELECT * FROM unnest($2::text[],$3::text[]))
	   ORDER BY 1),`+taskAuthorizationColumns("qc")+`
	 FROM (SELECT 1) AS one LEFT JOIN weave_task_queue qc ON qc.workspace_id=$1 AND qc.id=$4`,
		workspaceID, kinds, ids, taskID).Scan(targets...)
	if versions == nil {
		versions = []string{}
	}
	return versions, current, err
}
