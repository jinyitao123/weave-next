package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RunBundleFormat identifies the operator-only run reproduction file. It is a
// diagnostic artifact, not a client contract, and is never served over HTTP.
const (
	RunBundleFormat  = "weave-run-bundle"
	RunBundleVersion = 1
)

// RunBundleTable is one table's rows, each row keyed by column name with the
// value exactly as PostgreSQL's to_jsonb produced it.
type RunBundleTable struct {
	Name string                       `json:"name"`
	Rows []map[string]json.RawMessage `json:"rows"`
}

// RunBundle is everything the platform stored about one team run: the run,
// its member runs, queue rows, leases, terminal markers, checkpoints and the
// operation journal, delivery and input records, and the outbox.
type RunBundle struct {
	Format          string           `json:"format"`
	Version         int              `json:"version"`
	ExportedAt      time.Time        `json:"exported_at"`
	WorkspaceID     string           `json:"workspace_id"`
	RunID           string           `json:"run_id"`
	IncludesContent bool             `json:"includes_content"`
	RunIDs          []string         `json:"run_ids"`
	Redactions      map[string]int   `json:"redactions"`
	Tables          []RunBundleTable `json:"tables"`
}

// runBundleScope is what one export query is parameterized by, in this order:
// $1 workspace, $2 run, $3 run and member run IDs, $4 run snapshot,
// $5 workflow ID, $6 workflow version, $7 input revision IDs.
type runBundleTableSpec struct {
	name  string
	query string
	// secret columns are never exported; the value is the placeholder that
	// stands in for them and must satisfy the column's own constraints.
	secret map[string]string
	// content columns hold business material and are exported only on request.
	content []string
}

// The order is also the import order.
//
// The parent rows come first because a replay needs the frozen snapshot and the
// project the run belonged to. Agents, teams, capability credentials and other
// configuration those rows point at are deliberately not exported; the import
// therefore runs with foreign-key triggers off (see ImportRunBundle).
var runBundleTables = []runBundleTableSpec{
	{name: "weave_workspaces", query: `id=$1`},
	{name: "weave_projects", query: `workspace_id=$1 AND id IN (
		SELECT project_id FROM weave_team_runs WHERE workspace_id=$1 AND run_id=$2
		UNION SELECT project_id FROM weave_team_run_snapshots WHERE workspace_id=$1 AND run_id=$4)`},
	{name: "weave_team_run_snapshots", query: `workspace_id=$1 AND run_id=$4`},
	{name: "weave_task_group", query: `workspace_id=$1 AND id IN (
		SELECT task_group_id FROM weave_task_queue WHERE workspace_id=$1 AND (run_snapshot_id=$4 OR run_id=ANY($3)))`,
		content: []string{"original_request"}},
	{name: "weave_published_artifact_contents", query: `workspace_id=$1 AND workflow_id=$5 AND workflow_version=$6`},
	{name: "weave_dispatch_input_revisions", query: `workspace_id=$1 AND (input_revision_id=ANY($7) OR consumed_run_id=$2)`,
		content: []string{"source_messages", "task", "execution_task", "parent_materials"}},
	{name: "weave_task_business_delegations", query: `workspace_id=$1 AND input_revision_id=ANY($7)`,
		secret: map[string]string{"credential_ciphertext": "[redacted]", "credential_sha256": strings.Repeat("0", 64)}},
	{name: "weave_team_runs", query: `workspace_id=$1 AND run_id=$2`},
	{name: "weave_task_queue", query: `workspace_id=$1 AND (run_snapshot_id=$4 OR run_id=ANY($3))`,
		content: []string{"payload", "result"}},
	{name: "weave_workflow_member_runs", query: `workspace_id=$1 AND parent_run_id=$2`,
		content: []string{"initial_state", "result"}},
	{name: "weave_run_attempt_leases", query: `workspace_id=$1 AND run_id=ANY($3)`},
	{name: "weave_run_terminal_markers", query: `workspace_id=$1 AND run_id=ANY($3)`},
	{name: "weave_run_delivery_state", query: `workspace_id=$1 AND run_id=$2`},
	{name: "weave_run_delivery_verifications", query: `workspace_id=$1 AND run_id=$2`, content: []string{"report"}},
	{name: "weave_team_run_activity_events", query: `workspace_id=$1 AND run_id=$2`},
	{name: "weave_employee_run_event_outbox", query: `workspace_id=$1 AND run_id=$2`, content: []string{"payload"}},
	{name: "loom_store", query: `key=ANY($3) OR split_part(key,'/',1)=ANY($3)`, content: []string{"value"}},
}

// secretJSONKey matches JSON keys that carry credentials. It is deliberately
// exact about token: "input_tokens" is usage, "refresh_token" is a secret.
var secretJSONKey = regexp.MustCompile(`(?i)^(.*[_-])?(token|secret|password|passwd|api[_-]?key|apikey|authorization|cookie|credentials?|ciphertext|private[_-]?key)$`)

const redactedSecret = `"[redacted]"`

// ExportRunBundle reads one run in a single repeatable-read snapshot. Without
// includeContent every business-material column is replaced by its length and
// digest, so the bundle shows how the run was recorded without carrying what
// it was about. Credentials are never exported, and the export fails closed if
// any credential value that was read from the database still appears in it.
func ExportRunBundle(ctx context.Context, pool *pgxpool.Pool, workspaceID, runID string, includeContent bool) (*RunBundle, error) {
	if pool == nil {
		return nil, errors.New("database pool is required")
	}
	if workspaceID == "" || runID == "" {
		return nil, errors.New("workspace and run are required")
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var snapshot, workflowID string
	var workflowVersion int
	if err := tx.QueryRow(ctx, `SELECT run_snapshot_id,workflow_id,workflow_version FROM weave_team_runs WHERE workspace_id=$1 AND run_id=$2`,
		workspaceID, runID).Scan(&snapshot, &workflowID, &workflowVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("team run %q not found in workspace %q", runID, workspaceID)
		}
		return nil, fmt.Errorf("read team run: %w", err)
	}
	runIDs := []string{runID}
	memberRows, err := tx.Query(ctx, `SELECT member_run_id FROM weave_workflow_member_runs WHERE workspace_id=$1 AND parent_run_id=$2 ORDER BY member_run_id`, workspaceID, runID)
	if err != nil {
		return nil, err
	}
	for memberRows.Next() {
		var id string
		if err := memberRows.Scan(&id); err != nil {
			memberRows.Close()
			return nil, err
		}
		runIDs = append(runIDs, id)
	}
	memberRows.Close()
	if err := memberRows.Err(); err != nil {
		return nil, err
	}
	revisionIDs := []string{}
	revisionRows, err := tx.Query(ctx, `SELECT input_revision_id FROM weave_run_delivery_state WHERE workspace_id=$1 AND run_id=$2 AND input_revision_id<>''
		UNION SELECT input_revision_id FROM weave_employee_run_event_outbox WHERE workspace_id=$1 AND run_id=$2 AND input_revision_id<>''
		UNION SELECT input_revision_id FROM weave_dispatch_input_revisions WHERE workspace_id=$1 AND consumed_run_id=$2`, workspaceID, runID)
	if err != nil {
		return nil, err
	}
	for revisionRows.Next() {
		var id string
		if err := revisionRows.Scan(&id); err != nil {
			revisionRows.Close()
			return nil, err
		}
		revisionIDs = append(revisionIDs, id)
	}
	revisionRows.Close()
	if err := revisionRows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(revisionIDs)

	bundle := &RunBundle{
		Format: RunBundleFormat, Version: RunBundleVersion, ExportedAt: time.Now().UTC(),
		WorkspaceID: workspaceID, RunID: runID, IncludesContent: includeContent,
		RunIDs: runIDs, Redactions: map[string]int{}, Tables: make([]RunBundleTable, 0, len(runBundleTables)),
	}
	var literalSecrets []string
	for _, spec := range runBundleTables {
		query, args := bindRunBundleParams(spec.query, workspaceID, runID, runIDs, snapshot, workflowID, workflowVersion, revisionIDs)
		rows, err := tx.Query(ctx, `SELECT to_jsonb(t) FROM `+spec.name+` AS t WHERE `+query+` ORDER BY to_jsonb(t)::text`, args...)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", spec.name, err)
		}
		table := RunBundleTable{Name: spec.name, Rows: []map[string]json.RawMessage{}}
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return nil, err
			}
			row := map[string]json.RawMessage{}
			if err := json.Unmarshal(raw, &row); err != nil {
				rows.Close()
				return nil, fmt.Errorf("export %s: %w", spec.name, err)
			}
			for column, placeholder := range spec.secret {
				if value, ok := row[column]; ok && string(value) != "null" {
					var literal string
					if json.Unmarshal(value, &literal) == nil && literal != "" {
						literalSecrets = append(literalSecrets, literal)
					}
					row[column], _ = json.Marshal(placeholder)
					bundle.Redactions[spec.name+"."+column]++
				}
			}
			for column, value := range row {
				if cleaned, count := sweepSecretKeys(value); count > 0 {
					row[column] = cleaned
					bundle.Redactions[spec.name+"."+column+" (secret keys)"] += count
				}
			}
			if !includeContent {
				for _, column := range spec.content {
					if value, ok := row[column]; ok && string(value) != "null" {
						row[column] = contentPlaceholder(value)
						bundle.Redactions[spec.name+"."+column+" (content)"]++
					}
				}
			} else if spec.name == "loom_store" {
				if cleaned, count := sweepStoredValue(row["value"]); count > 0 {
					row["value"] = cleaned
					bundle.Redactions["loom_store.value (secret keys)"] += count
				}
			}
			table.Rows = append(table.Rows, row)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		bundle.Tables = append(bundle.Tables, table)
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		return nil, err
	}
	for _, secret := range literalSecrets {
		// bytea columns are exported as hex, so the plain text search alone
		// cannot see a credential stored inside a checkpoint or journal value.
		if bytes.Contains(encoded, []byte(secret)) || bytes.Contains(encoded, []byte(hex.EncodeToString([]byte(secret)))) {
			return nil, errors.New("export refused: a credential value from the database is still present in the bundle")
		}
	}
	return bundle, nil
}

var runBundleParam = regexp.MustCompile(`\$(\d+)`)

// bindRunBundleParams keeps only the parameters a table's condition uses and
// renumbers them from $1, because the driver rejects both unused arguments and
// gaps in the numbering.
func bindRunBundleParams(query string, all ...any) (string, []any) {
	order := map[int]int{}
	var args []any
	rewritten := runBundleParam.ReplaceAllStringFunc(query, func(match string) string {
		var index int
		_, _ = fmt.Sscanf(match, "$%d", &index)
		if _, seen := order[index]; !seen {
			args = append(args, all[index-1])
			order[index] = len(args)
		}
		return fmt.Sprintf("$%d", order[index])
	})
	return rewritten, args
}

// contentPlaceholder keeps the column's type valid (a string stays a string, a
// structure stays an object) and records only length and digest.
func contentPlaceholder(value json.RawMessage) json.RawMessage {
	sum := sha256.Sum256(value)
	digest := hex.EncodeToString(sum[:])
	if len(value) > 0 && value[0] == '"' {
		return json.RawMessage(fmt.Sprintf(`"[content omitted length=%d sha256=%s]"`, len(value), digest))
	}
	return json.RawMessage(fmt.Sprintf(`{"content_omitted":true,"length":%d,"sha256":%q}`, len(value), digest))
}

// sweepSecretKeys replaces the value of every credential-named key inside a
// JSON object or array. Scalars and strings pass through unchanged.
func sweepSecretKeys(value json.RawMessage) (json.RawMessage, int) {
	if len(value) == 0 || (value[0] != '{' && value[0] != '[') {
		return value, 0
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	if decoder.Decode(&decoded) != nil {
		return value, 0
	}
	count := sweepValue(decoded)
	if count == 0 {
		return value, 0
	}
	cleaned, err := json.Marshal(decoded)
	if err != nil {
		return value, 0
	}
	return cleaned, count
}

func sweepValue(node any) int {
	count := 0
	switch typed := node.(type) {
	case map[string]any:
		for key, child := range typed {
			if secretJSONKey.MatchString(key) {
				if child != nil {
					typed[key] = "[redacted]"
					count++
				}
				continue
			}
			count += sweepValue(child)
		}
	case []any:
		for _, child := range typed {
			count += sweepValue(child)
		}
	}
	return count
}

// sweepStoredValue applies the key sweep to a bytea value that holds JSON. A
// value that is not JSON is left as is; the literal-credential check at the end
// of the export is the backstop for those.
func sweepStoredValue(value json.RawMessage) (json.RawMessage, int) {
	var encoded string
	if json.Unmarshal(value, &encoded) != nil || !strings.HasPrefix(encoded, `\x`) {
		return value, 0
	}
	stored, err := hex.DecodeString(encoded[2:])
	if err != nil {
		return value, 0
	}
	cleaned, count := sweepSecretKeys(stored)
	if count == 0 {
		return value, 0
	}
	out, _ := json.Marshal(`\x` + hex.EncodeToString(cleaned))
	return out, count
}

// ImportRunBundle inserts a bundle into a database that already has the
// current schema. It never overwrites: a row that exists makes the whole import
// fail, so a bundle cannot silently change an environment's state. Only the
// tables the export knows are accepted.
func ImportRunBundle(ctx context.Context, pool *pgxpool.Pool, bundle *RunBundle) (map[string]int, error) {
	if pool == nil || bundle == nil {
		return nil, errors.New("database pool and bundle are required")
	}
	if bundle.Format != RunBundleFormat || bundle.Version != RunBundleVersion {
		return nil, fmt.Errorf("unsupported bundle %q version %d", bundle.Format, bundle.Version)
	}
	known := map[string]bool{}
	for _, spec := range runBundleTables {
		known[spec.name] = true
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The bundle carries the run's own rows and the parents a replay reads, not
	// every configuration row those point at, so referential checks would refuse
	// it. This needs a role allowed to set the parameter, which is why an import
	// is only meant for a disposable database.
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		return nil, fmt.Errorf("the import role cannot disable foreign-key triggers; use a disposable database owned by a superuser: %w", err)
	}
	counts := map[string]int{}
	for _, table := range bundle.Tables {
		if !known[table.Name] {
			return nil, fmt.Errorf("bundle contains unknown table %q", table.Name)
		}
		if len(table.Rows) == 0 {
			continue
		}
		rows, err := json.Marshal(table.Rows)
		if err != nil {
			return nil, err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO `+table.Name+` SELECT * FROM jsonb_populate_recordset(null::`+table.Name+`, $1::jsonb)`, string(rows))
		if err != nil {
			return nil, fmt.Errorf("import %s: %w", table.Name, err)
		}
		counts[table.Name] = int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return counts, nil
}
