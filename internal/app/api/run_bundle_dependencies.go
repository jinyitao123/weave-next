package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

type runBundleFK struct {
	name, child, parent         string
	childColumns, parentColumns []string
	deferrable, deferred        bool
}

func runBundleForeignKeys(ctx context.Context, tx pgx.Tx) ([]runBundleFK, error) {
	rows, err := tx.Query(ctx, `SELECT c.conname,child.relname,parent.relname,
 ARRAY(SELECT a.attname FROM unnest(c.conkey) WITH ORDINALITY k(num,ord) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.num ORDER BY k.ord),
 ARRAY(SELECT a.attname FROM unnest(c.confkey) WITH ORDINALITY k(num,ord) JOIN pg_attribute a ON a.attrelid=c.confrelid AND a.attnum=k.num ORDER BY k.ord),c.condeferrable,c.condeferred
 FROM pg_constraint c JOIN pg_class child ON child.oid=c.conrelid JOIN pg_class parent ON parent.oid=c.confrelid
 WHERE c.contype='f' AND child.relnamespace=current_schema()::regnamespace AND parent.relnamespace=current_schema()::regnamespace
 ORDER BY child.relname,c.conname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []runBundleFK{}
	for rows.Next() {
		var fk runBundleFK
		if err := rows.Scan(&fk.name, &fk.child, &fk.parent, &fk.childColumns, &fk.parentColumns, &fk.deferrable, &fk.deferred); err != nil {
			return nil, err
		}
		result = append(result, fk)
	}
	return result, rows.Err()
}

func readRunBundleRows(ctx context.Context, tx pgx.Tx, table, where string, args ...any) ([]map[string]json.RawMessage, error) {
	rows, err := tx.Query(ctx, `SELECT to_jsonb(t) FROM `+pgx.Identifier{table}.Sanitize()+` t WHERE `+where+` ORDER BY to_jsonb(t)::text`, args...)
	if err != nil {
		return nil, fmt.Errorf("export table %s: %w", table, err)
	}
	defer rows.Close()
	out := []map[string]json.RawMessage{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var row map[string]json.RawMessage
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func appendRunBundleRows(tables map[string][]map[string]json.RawMessage, table string, rows []map[string]json.RawMessage) int {
	seen := map[string]bool{}
	for _, row := range tables[table] {
		raw, _ := json.Marshal(row)
		seen[string(raw)] = true
	}
	added := 0
	for _, row := range rows {
		raw, _ := json.Marshal(row)
		if !seen[string(raw)] {
			tables[table] = append(tables[table], row)
			seen[string(raw)] = true
			added++
		}
	}
	return added
}

// Close every real FK edge. A missing parent is an incomplete export, never a
// reason to switch replication_role or synthesize configuration.
func closeRunBundleForeignKeys(ctx context.Context, tx pgx.Tx, tables map[string][]map[string]json.RawMessage) error {
	fks, err := runBundleForeignKeys(ctx, tx)
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, spec := range runBundleTables {
		allowed[spec.name] = true
	}
	for pass := 0; pass < 64; pass++ {
		added := 0
		for _, fk := range fks {
			for _, row := range tables[fk.child] {
				match := map[string]json.RawMessage{}
				skip := false
				for index, column := range fk.childColumns {
					value, exists := row[column]
					if !exists || string(value) == "null" {
						skip = true
						break
					}
					match[fk.parentColumns[index]] = value
				}
				if skip {
					continue
				}
				if !allowed[fk.parent] {
					return fmt.Errorf("referenced parent %s is not in the diagnostic export policy", fk.parent)
				}
				raw, _ := json.Marshal(match)
				parents, err := readRunBundleRows(ctx, tx, fk.parent, `to_jsonb(t) @> $1::jsonb`, string(raw))
				if err != nil {
					return err
				}
				if len(parents) != 1 {
					return fmt.Errorf("referenced parent missing or ambiguous in %s", fk.parent)
				}
				added += appendRunBundleRows(tables, fk.parent, parents)
			}
		}
		if added == 0 {
			return nil
		}
	}
	return errors.New("run parent closure exceeds its bounded dependency depth")
}

// Frozen references are logical dependencies, not SQL FKs. Include their exact
// revisions so importing one run does not silently depend on today's heads.
func addFrozenRunBundleDependencies(ctx context.Context, tx pgx.Tx, tables map[string][]map[string]json.RawMessage) error {
	add := func(table, where string, args ...any) error {
		rows, err := readRunBundleRows(ctx, tx, table, where, args...)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("frozen dependency is missing from %s", table)
		}
		appendRunBundleRows(tables, table, rows)
		return nil
	}
	// Capability invocations deliberately do not have a SQL FK to a revision.
	// Their immutable definition hash is still an exact logical dependency.
	for _, row := range tables["weave_capability_invocations"] {
		var workspace, capability, hash string
		var revision int64
		if json.Unmarshal(row["workspace_id"], &workspace) != nil || json.Unmarshal(row["capability_id"], &capability) != nil ||
			json.Unmarshal(row["revision"], &revision) != nil || json.Unmarshal(row["definition_hash"], &hash) != nil ||
			workspace == "" || capability == "" || revision < 1 || hash == "" {
			return errors.New("capability invocation has no immutable definition reference")
		}
		if err := add("weave_capability_revisions", `workspace_id=$1 AND capability_id=$2 AND revision=$3 AND definition_hash=$4`, workspace, capability, revision, hash); err != nil {
			return err
		}
	}
	for _, row := range tables["weave_published_artifact_contents"] {
		raw, _ := json.Marshal(row)
		var envelope frozen.ArtifactEnvelopeV1
		if json.Unmarshal(raw, &envelope) != nil {
			return errors.New("frozen artifact header is invalid")
		}
		payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
		if err != nil {
			return fmt.Errorf("frozen artifact cannot be verified: %w", err)
		}
		for _, bundle := range payload.Bundles {
			if err := add("weave_agent_versions", `agent_id=$1 AND version=$2`, bundle.Agent.AgentID, bundle.Agent.AgentVersion); err != nil {
				return err
			}
			models := append([]frozen.FrozenModelBinding{bundle.PrimaryModel}, bundle.FallbackModels...)
			for _, model := range models {
				if model.ProviderID == "" {
					continue
				}
				if err := add("weave_provider_revisions", `workspace_id=$1 AND provider_id=$2 AND revision=$3`, model.WorkspaceID, model.ProviderID, model.ProviderRevision); err != nil {
					return err
				}
			}
			for _, skill := range bundle.Skills {
				if skill.SkillID != "" && skill.SkillVersion != nil {
					if err := add("weave_skill_versions", `workspace_id=$1 AND skill_id=$2 AND version=$3`, skill.WorkspaceID, skill.SkillID, *skill.SkillVersion); err != nil {
						return err
					}
				}
			}
			for _, server := range bundle.MCPBindings {
				if err := add("weave_mcp_server_revisions", `workspace_id=$1 AND server_id=$2 AND revision=$3`, server.WorkspaceID, server.ServerID, server.ServerRevision); err != nil {
					return err
				}
			}
			if bundle.Runtime != nil && bundle.Runtime.RuntimeID != "" {
				if err := add("weave_runtimes", `workspace_id=$1 AND id=$2`, bundle.Runtime.WorkspaceID, bundle.Runtime.RuntimeID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Validate table names and duplicate blocks before touching a target schema.
func validateRunBundleTables(bundle *RunBundle) error {
	known := map[string]bool{}
	for _, spec := range runBundleTables {
		known[spec.name] = true
	}
	seen := map[string]bool{}
	for _, table := range bundle.Tables {
		if !known[table.Name] || seen[table.Name] {
			return fmt.Errorf("unknown or duplicate bundle table %q", table.Name)
		}
		seen[table.Name] = true
		for _, row := range table.Rows {
			if raw, ok := row["workspace_id"]; ok {
				var ws string
				if json.Unmarshal(raw, &ws) != nil || ws != bundle.WorkspaceID {
					return errors.New("bundle row crossed workspace identity")
				}
			}
		}
	}
	if !seen["weave_team_runs"] || !seen["weave_team_run_snapshots"] || !seen["weave_published_artifact_contents"] {
		return errors.New("bundle is missing its frozen execution roots")
	}
	if strings.TrimSpace(bundle.WorkspaceID) == "" || strings.TrimSpace(bundle.RunID) == "" {
		return errors.New("bundle scope is incomplete")
	}
	return nil
}

// Insert only columns the source actually stored. Additive target columns use
// their real schema defaults instead of jsonb_populate_record's synthetic NULL.
// Unknown source columns are rejected, not silently discarded.
func insertRunBundleRows(ctx context.Context, tx pgx.Tx, table string, rows []map[string]json.RawMessage) (int, error) {
	columnsRows, err := tx.Query(ctx, `SELECT attname FROM pg_attribute WHERE attrelid=$1::regclass AND attnum>0 AND NOT attisdropped`, pgx.Identifier{table}.Sanitize())
	if err != nil {
		return 0, err
	}
	columns := map[string]bool{}
	for columnsRows.Next() {
		var name string
		if err := columnsRows.Scan(&name); err != nil {
			columnsRows.Close()
			return 0, err
		}
		columns[name] = true
	}
	columnsRows.Close()
	if err := columnsRows.Err(); err != nil {
		return 0, err
	}
	count := 0
	for _, row := range rows {
		names := []string{}
		for name := range row {
			if !columns[name] {
				return 0, fmt.Errorf("unknown source column in %s", table)
			}
			names = append(names, name)
		}
		sort.Strings(names)
		identifiers := make([]string, len(names))
		for i, name := range names {
			identifiers[i] = pgx.Identifier{name}.Sanitize()
		}
		encoded, err := json.Marshal([]map[string]json.RawMessage{row})
		if err != nil {
			return 0, err
		}
		list := strings.Join(identifiers, ",")
		tag, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{table}.Sanitize()+` (`+list+`) SELECT `+list+` FROM jsonb_populate_recordset(null::`+pgx.Identifier{table}.Sanitize()+`,$1::jsonb)`, string(encoded))
		if err != nil {
			return 0, fmt.Errorf("import %s: %w", table, err)
		}
		count += int(tag.RowsAffected())
	}
	return count, nil
}
