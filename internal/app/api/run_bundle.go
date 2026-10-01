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
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// RunBundleFormat identifies the operator-only run reproduction file. It is a
// diagnostic artifact, not a client contract, and is never served over HTTP.
const (
	RunBundleFormat  = "weave-run-bundle"
	RunBundleVersion = 2
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

// These are scoped execution roots. Foreign-key parents and explicit frozen
// dependencies are added by reference, never by copying a whole workspace.
var runBundleTables = []runBundleTableSpec{
	{name: "weave_workspaces", query: `id=$1`},
	{name: "weave_projects", query: `workspace_id=$1 AND id IN (
		SELECT project_id FROM weave_team_runs WHERE workspace_id=$1 AND run_id=$2
		UNION SELECT project_id FROM weave_team_run_snapshots WHERE workspace_id=$1 AND run_id=$4)`},
	{name: "weave_team_run_snapshots", query: `workspace_id=$1 AND run_id=$4`},
	{name: "weave_task_group", query: `workspace_id=$1 AND id IN (
		SELECT task_group_id FROM weave_task_queue WHERE workspace_id=$1 AND (run_snapshot_id=$4 OR run_id=ANY($3)))`,
		content: []string{"original_request"}},
	{name: "weave_published_artifact_contents", query: `workspace_id=$1 AND workflow_id=$5 AND workflow_version=$6`, content: []string{"payload"}},
	{name: "weave_dispatch_input_revisions", query: `workspace_id=$1 AND (input_revision_id=ANY($7) OR consumed_run_id=$2)`,
		content: []string{"source_messages", "task", "execution_task", "parent_materials"}},
	{name: "weave_task_business_delegations", query: `workspace_id=$1 AND input_revision_id=ANY($7)`,
		secret: map[string]string{"credential_ciphertext": "[redacted]", "credential_sha256": strings.Repeat("0", 64)}},
	{name: "weave_team_runs", query: `workspace_id=$1 AND run_id=$2`},
	{name: "weave_task_queue", query: `workspace_id=$1 AND (run_snapshot_id=$4 OR run_id=ANY($3) OR capability_invocation_id IN (SELECT invocation_id FROM weave_capability_step_runs WHERE workspace_id=$1 AND run_id=ANY($3)))`,
		content: []string{"payload", "result"}},
	{name: "weave_workflow_member_runs", query: `workspace_id=$1 AND parent_run_id=$2`,
		content: []string{"initial_state", "result"}},
	{name: "weave_run_attempt_leases", query: `workspace_id=$1 AND run_id=ANY($3)`},
	{name: "weave_run_terminal_markers", query: `workspace_id=$1 AND run_id=ANY($3)`},
	{name: "weave_run_delivery_state", query: `workspace_id=$1 AND run_id=$2`},
	{name: "weave_run_delivery_verifications", query: `workspace_id=$1 AND run_id=$2`, content: []string{"report"}},
	{name: "weave_team_run_activity_events", query: `workspace_id=$1 AND run_id=$2`, content: []string{"detail"}},
	{name: "weave_final_deliverables", query: `workspace_id=$1 AND run_id=ANY($3)`, content: []string{"content", "metadata"}},
	{name: "weave_employee_run_event_outbox", query: `workspace_id=$1 AND run_id=$2`, content: []string{"payload"}},
	{name: "loom_store", query: `(key=ANY($3) OR split_part(key,'/',1)=ANY($3)) AND (
		namespace IN ('member-operation:'||$1,'audit:'||$1,'runreg:'||$1,'teamrun-checkpoint:'||$1,'member-budget-grant:'||$1,'member-budget-grant:'||$1||':consumed')
		OR namespace IN (SELECT 'checkpoint:'||graph_name FROM weave_run_attempt_leases WHERE workspace_id=$1 AND run_id=ANY($3)))`, content: []string{"value"}},
	{name: "weave_expected_run_domain_index", query: `workspace_id=$1 AND run_id=ANY($3)`},
	{name: "weave_capability_step_runs", query: `workspace_id=$1 AND run_id=ANY($3)`},
	{name: "weave_capability_invocations", query: `workspace_id=$1 AND invocation_id IN (SELECT invocation_id FROM weave_capability_step_runs WHERE workspace_id=$1 AND run_id=ANY($3))`, content: []string{"input", "result_state", "checkpoint"}},
	{name: "weave_capability_invocation_events", query: `workspace_id=$1 AND invocation_id IN (SELECT invocation_id FROM weave_capability_step_runs WHERE workspace_id=$1 AND run_id=ANY($3))`, content: []string{"detail"}},
	{name: "weave_capability_human_tasks", query: `workspace_id=$1 AND invocation_id IN (SELECT invocation_id FROM weave_capability_step_runs WHERE workspace_id=$1 AND run_id=ANY($3))`, content: []string{"response"}},
	{name: "weave_capability_revision_tool_bindings", query: `workspace_id=$1 AND (capability_id,revision) IN (SELECT capability_id,revision FROM weave_capability_invocations WHERE workspace_id=$1 AND invocation_id IN (SELECT invocation_id FROM weave_capability_step_runs WHERE workspace_id=$1 AND run_id=ANY($3)))`},
	{name: "weave_capability_debug_snapshots", query: `workspace_id=$1 AND invocation_id IN (SELECT invocation_id FROM weave_capability_step_runs WHERE workspace_id=$1 AND run_id=ANY($3))`},
	{name: "weave_team_run_corrections", query: `workspace_id=$1 AND run_id=$2`, content: []string{"detail"}},
	{name: "weave_team_run_correction_events", query: `workspace_id=$1 AND run_id=$2`},
	// Parent-only entries have no unscoped query. The closure includes only rows
	// referenced by the selected run or its immutable frozen dependencies.
	{name: "weave_teams"}, {name: "weave_agents", content: []string{"spec"}},
	{name: "weave_agent_versions", content: []string{"spec"}}, {name: "weave_team_workflows"},
	{name: "weave_team_workflow_versions", content: []string{"graph_definition"}},
	{name: "weave_skills", content: []string{"body"}}, {name: "weave_skill_versions", content: []string{"body"}},
	{name: "weave_provider_credentials", secret: map[string]string{"api_key_cipher": "[redacted]"}},
	{name: "weave_provider_revisions"}, {name: "weave_mcp_servers", secret: map[string]string{"token": "[redacted]", "headers": "[redacted]"}},
	{name: "weave_mcp_server_revisions"}, {name: "weave_runtimes", secret: map[string]string{"token_hash": "[redacted]"}},
	{name: "weave_delivery_targets", secret: map[string]string{"secret_ciphertext": "[redacted]"}},
	{name: "weave_delivery_target_revisions"}, {name: "weave_capability_revisions"}, {name: "weave_capability_definitions"},
	{name: "weave_capability_credentials", secret: map[string]string{"key_hash": "[redacted]"}},
	{name: "weave_capability_apps"}, {name: "weave_users", secret: map[string]string{"password": "[redacted]"}},
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
	rawTables := make(map[string][]map[string]json.RawMessage)
	for _, spec := range runBundleTables {
		if spec.query == "" {
			continue
		}
		query, args := bindRunBundleParams(spec.query, workspaceID, runID, runIDs, snapshot, workflowID, workflowVersion, revisionIDs)
		rows, err := readRunBundleRows(ctx, tx, spec.name, query, args...)
		if err != nil {
			return nil, err
		}
		rawTables[spec.name] = rows
	}
	if err := addFrozenRunBundleDependencies(ctx, tx, rawTables); err != nil {
		return nil, err
	}
	if err := closeRunBundleForeignKeys(ctx, tx, rawTables); err != nil {
		return nil, err
	}
	type secretMarker struct{ field, value string }
	var literalSecrets []secretMarker
	for _, spec := range runBundleTables {
		table := RunBundleTable{Name: spec.name, Rows: []map[string]json.RawMessage{}}
		for _, row := range rawTables[spec.name] {
			for column, placeholder := range spec.secret {
				value, ok := row[column]
				if !ok || string(value) == "null" {
					continue
				}
				var literal string
				if json.Unmarshal(value, &literal) == nil && literal != "" {
					if literal != "[redacted]" && !(spec.name == "weave_users" && column == "password" && literal == "!") {
						literalSecrets = append(literalSecrets, secretMarker{spec.name + "." + column, literal})
					}
				}
				if column == "key_hash" {
					sum := sha256.Sum256(append(append([]byte(spec.name), row["workspace_id"]...), row["id"]...))
					placeholder = hex.EncodeToString(sum[:])
				}
				row[column], _ = json.Marshal(placeholder)
				bundle.Redactions[spec.name+"."+column]++
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
		bundle.Tables = append(bundle.Tables, table)
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		return nil, err
	}
	for _, secret := range literalSecrets {
		// bytea columns are exported as hex, so the plain text search alone
		// cannot see a credential stored inside a checkpoint or journal value.
		if bytes.Contains(encoded, []byte(secret.value)) || bytes.Contains(encoded, []byte(hex.EncodeToString([]byte(secret.value)))) {
			for _, table := range bundle.Tables {
				tableRaw, _ := json.Marshal(table)
				if bytes.Contains(tableRaw, []byte(secret.value)) || bytes.Contains(tableRaw, []byte(hex.EncodeToString([]byte(secret.value)))) {
					return nil, fmt.Errorf("export refused: credential material from %s remains in table %s", secret.field, table.Name)
				}
			}
			return nil, errors.New("export refused: credential material remains in bundle")
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
			if secretJSONKey.MatchString(key) && !isCredentialReferenceList(key, child) {
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

// Credential references are immutable configuration, not credential values.
// Preserve only validated reference arrays; arbitrary "credentials" objects
// remain redacted and never become usable imported credentials.
func isCredentialReferenceList(key string, value any) bool {
	if key != "credentials" {
		return false
	}
	list, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		raw, err := json.Marshal(item)
		if err != nil {
			return false
		}
		var ref frozen.CredentialReference
		if json.Unmarshal(raw, &ref) != nil || frozen.ValidateCredentialReference(ref) != nil {
			return false
		}
		var fields map[string]any
		_ = json.Unmarshal(raw, &fields)
		for field := range fields {
			switch field {
			case "schema_version", "scope", "user_id", "service_id", "workspace_id", "kind", "resource_id", "slot", "credential_version":
			default:
				return false
			}
		}
	}
	return true
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
	if err := validateRunBundleTables(bundle); err != nil {
		return nil, err
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
	if !bundle.IncludesContent {
		return nil, errors.New("a redacted diagnostic bundle cannot reconstruct a run")
	}
	var occupied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_workspaces)`).Scan(&occupied); err != nil {
		return nil, err
	}
	if occupied {
		return nil, errors.New("run import requires an empty isolated database")
	}
	constraints, err := runBundleForeignKeys(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, fk := range constraints {
		if !known[fk.child] || !known[fk.parent] {
			continue
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE `+pgx.Identifier{fk.child}.Sanitize()+` ALTER CONSTRAINT `+pgx.Identifier{fk.name}.Sanitize()+` DEFERRABLE INITIALLY DEFERRED`); err != nil {
			return nil, fmt.Errorf("defer cyclic import constraint: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	tables := append([]RunBundleTable(nil), bundle.Tables...)
	// FK checks are deferred, but business validation triggers remain active.
	// Their ordinary publication lifecycle must still be followed.
	ranks := map[string]int{"weave_workspaces": 0, "weave_users": 1, "weave_agents": 2, "weave_agent_versions": 3, "weave_teams": 4, "weave_team_workflow_versions": 5, "weave_team_workflows": 6, "weave_projects": 7, "weave_published_artifact_contents": 8}
	sort.SliceStable(tables, func(i, j int) bool {
		a, ok := ranks[tables[i].Name]
		if !ok {
			a = 20
		}
		b, ok := ranks[tables[j].Name]
		if !ok {
			b = 20
		}
		return a < b
	})
	for _, table := range tables {
		if !known[table.Name] {
			return nil, fmt.Errorf("bundle contains unknown table %q", table.Name)
		}
		if len(table.Rows) == 0 {
			continue
		}
		if table.Name == "weave_team_workflow_versions" {
			for _, row := range table.Rows {
				copy := make(map[string]json.RawMessage, len(row))
				for key, value := range row {
					copy[key] = value
				}
				var status string
				_ = json.Unmarshal(copy["status"], &status)
				if status == "published" {
					copy["status"] = json.RawMessage(`"draft"`)
					copy["published_at"] = json.RawMessage(`null`)
				}
				raw, err := json.Marshal([]map[string]json.RawMessage{copy})
				if err != nil {
					return nil, err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO weave_team_workflow_versions SELECT * FROM jsonb_populate_recordset(null::weave_team_workflow_versions,$1::jsonb)`, string(raw)); err != nil {
					return nil, fmt.Errorf("import frozen workflow version: %w", err)
				}
				if status == "published" {
					var ws, id string
					var version int64
					_ = json.Unmarshal(row["workspace_id"], &ws)
					_ = json.Unmarshal(row["workflow_id"], &id)
					_ = json.Unmarshal(row["version"], &version)
					var published *string
					_ = json.Unmarshal(row["published_at"], &published)
					if _, err := tx.Exec(ctx, `UPDATE weave_team_workflow_versions SET status='published',published_at=$4::timestamptz WHERE workspace_id=$1 AND workflow_id=$2 AND version=$3`, ws, id, version, published); err != nil {
						return nil, err
					}
				}
				counts[table.Name]++
			}
			continue
		}
		count, err := insertRunBundleRows(ctx, tx, table.Name, table.Rows)
		if err != nil {
			return nil, err
		}
		counts[table.Name] = count
	}
	frozenTables := map[string][]map[string]json.RawMessage{}
	for _, table := range bundle.Tables {
		frozenTables[table.Name] = table.Rows
	}
	if err := addFrozenRunBundleDependencies(ctx, tx, frozenTables); err != nil {
		return nil, err
	}
	// Force every deferred referential check before publishing imported rows.
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
		return nil, fmt.Errorf("bundle is missing a referenced parent: %w", err)
	}
	for _, fk := range constraints {
		if !known[fk.child] || !known[fk.parent] {
			continue
		}
		mode := "NOT DEFERRABLE"
		if fk.deferrable {
			mode = "DEFERRABLE INITIALLY IMMEDIATE"
			if fk.deferred {
				mode = "DEFERRABLE INITIALLY DEFERRED"
			}
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE `+pgx.Identifier{fk.child}.Sanitize()+` ALTER CONSTRAINT `+pgx.Identifier{fk.name}.Sanitize()+` `+mode); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return counts, nil
}
