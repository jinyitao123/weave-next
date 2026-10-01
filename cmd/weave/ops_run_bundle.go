package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom/pgstore"
	"github.com/jinyitao123/weave/internal/app/api"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
)

// runOpsExportRun writes one run's stored state to a file. The file is created
// owner-only and never overwritten, because with --include-content it holds
// what the employee handed over.
func runOpsExportRun(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("weave ops export-run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	workspace := flags.String("workspace", "", "workspace ID of the run")
	run := flags.String("run", "", "team run ID")
	out := flags.String("out", "", "file to create")
	content := flags.Bool("include-content", false, "include the business material; without it only lengths and digests are written")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *workspace == "" || *run == "" || *out == "" {
		fmt.Fprintln(stderr, "export-run requires --workspace, --run and --out")
		return 2
	}
	ctx := context.Background()
	pool, code := opsPool(ctx, stderr)
	if pool == nil {
		return code
	}
	defer pool.Close()
	bundle, err := api.ExportRunBundle(ctx, pool, *workspace, *run, *content)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	encoded, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	file, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "cannot create %s: %v\n", *out, err)
		return 1
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := file.Close(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	counts := map[string]int{}
	for _, table := range bundle.Tables {
		counts[table.Name] = len(table.Rows)
	}
	summary := map[string]any{"file": *out, "includes_content": bundle.IncludesContent, "rows": counts, "redactions": bundle.Redactions}
	if err := json.NewEncoder(stdout).Encode(summary); err != nil {
		return 1
	}
	return 0
}

// runOpsImportRun loads a bundle into a database that already has the current
// schema. It refuses anything but a loopback database unless the operator says
// the target is disposable, because an import inserts rows a live platform
// would treat as real work.
func runOpsImportRun(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("weave ops import-run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	file := flags.String("file", "", "bundle file written by export-run")
	initialize := flags.Bool("init-schema", false, "initialize the current schema in an empty disposable target")
	disposable := flags.Bool("disposable-target", false, "the target database is not loopback but may be filled with copied run data")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *file == "" {
		fmt.Fprintln(stderr, "import-run requires --file")
		return 2
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	var bundle api.RunBundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		fmt.Fprintf(stderr, "not a run bundle: %v\n", err)
		return 1
	}
	ctx := context.Background()
	pool, code := opsPool(ctx, stderr)
	if pool == nil {
		return code
	}
	defer pool.Close()
	host := pool.Config().ConnConfig.Host
	if !*disposable && !isLoopbackHost(host) {
		fmt.Fprintf(stderr, "refusing to import into %q: it is not a loopback database; pass --disposable-target only for a throwaway copy\n", host)
		return 2
	}
	if *initialize {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('weave_workspaces') IS NOT NULL`).Scan(&exists); err != nil {
			fmt.Fprintln(stderr, "cannot inspect disposable schema")
			return 1
		}
		if exists {
			var occupied bool
			if pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_workspaces)`).Scan(&occupied) != nil || occupied {
				fmt.Fprintln(stderr, "schema initialization requires an empty target")
				return 2
			}
		}
		store, storeErr := pgstore.New(os.Getenv("DATABASE_URL"))
		if storeErr != nil {
			fmt.Fprintln(stderr, "disposable journal schema unavailable")
			return 1
		}
		defer store.Close()
		if store.Migrate(ctx) != nil {
			fmt.Fprintln(stderr, "disposable journal schema initialization failed")
			return 1
		}
		if err := db.Migrate(ctx, pool); err != nil {
			fmt.Fprintln(stderr, "disposable schema initialization failed")
			return 1
		}
	}
	counts, err := api.ImportRunBundle(ctx, pool, &bundle)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(map[string]any{"imported": counts}); err != nil {
		return 1
	}
	return 0
}

func opsPool(ctx context.Context, stderr io.Writer) (*pgxpool.Pool, int) {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		fmt.Fprintln(stderr, "DATABASE_URL is required")
		return nil, 2
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(stderr, "database unavailable: %v\n", err)
		return nil, 1
	}
	return pool, 0
}

func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// runOpsReplayRun re-runs each member of a run from its recorded journal
// without any model, tool or Forge call and without writing to the database,
// so it may point at an imported copy or at the live database.
func runOpsReplayRun(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("weave ops replay-run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	workspace := flags.String("workspace", "", "workspace ID of the run")
	run := flags.String("run", "", "team run ID")
	member := flags.String("member", "", "replay only this member run ID")
	reportOut := flags.String("out", "", "private replay report file; stdout contains only hashes and status")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *workspace == "" || *run == "" {
		fmt.Fprintln(stderr, "replay-run requires --workspace and --run")
		return 2
	}
	ctx := context.Background()
	pool, code := opsPool(ctx, stderr)
	if pool == nil {
		return code
	}
	defer pool.Close()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		fmt.Fprintln(stderr, "replay snapshot unavailable")
		return 1
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT member_run_id,initial_state,node_id,run_snapshot_id FROM weave_workflow_member_runs
		WHERE workspace_id=$1 AND parent_run_id=$2 AND ($3='' OR member_run_id=$3) ORDER BY created_at,member_run_id`, *workspace, *run, *member)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	type memberRow struct {
		id             string
		initial        []byte
		node, snapshot string
	}
	var members []memberRow
	for rows.Next() {
		var row memberRow
		if err := rows.Scan(&row.id, &row.initial, &row.node, &row.snapshot); err != nil {
			rows.Close()
			fmt.Fprintln(stderr, err)
			return 1
		}
		members = append(members, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(members) == 0 {
		var status, cause, snapshot string
		if err := tx.QueryRow(ctx, `SELECT status,COALESCE(cause_summary,''),run_snapshot_id FROM weave_team_runs WHERE workspace_id=$1 AND run_id=$2`, *workspace, *run).Scan(&status, &cause, &snapshot); err != nil {
			fmt.Fprintln(stderr, "selected run is unavailable")
			return 1
		}
		artifact, artifactErr := loadRunReplayArtifact(ctx, tx, *workspace, snapshot)
		_ = artifact
		causeHash := sha256.Sum256([]byte(cause))
		kind := "stored_failure_without_request_evidence"
		if strings.Contains(cause, "reasoning_content") {
			kind = "reasoning_content_provider_rejection_without_saved_request"
		}
		_ = json.NewEncoder(stdout).Encode(map[string]any{"run_sha256": digestReplayID(*run), "outcome": loomruntime.ReplayEvidenceUnavailable, "original_status": status, "reason_kind": kind, "cause_sha256": hex.EncodeToString(causeHash[:]), "network_calls": 0, "business_writes": 0, "immutable_configuration_verified": artifactErr == nil})
		return 3
	}
	reports := make([]*loomruntime.MemberReplayReport, 0, len(members))
	for _, row := range members {
		entries, err := loomruntime.ReadMemberJournal(ctx, tx, *workspace, row.id)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		checkpoints, err := loomruntime.ReadMemberReplayCheckpoints(ctx, tx, *workspace, row.id)
		if err != nil {
			fmt.Fprintln(stderr, "member checkpoints unavailable")
			return 1
		}
		configuration, configErr := loadReplayConfiguration(ctx, tx, *workspace, row.snapshot, row.node)
		if configErr != nil {
			reports = append(reports, &loomruntime.MemberReplayReport{MemberRunID: row.id, Outcome: loomruntime.ReplayConfigurationUnavailable, Detail: "immutable execution configuration is unavailable or invalid", NoLiveCalls: true, ReplayScope: "model_tool_journal"})
			continue
		}
		reports = append(reports, loomruntime.ReplayMemberJournal(ctx, row.id, entries, checkpoints, configuration))
	}
	if *reportOut != "" {
		raw, err := json.MarshalIndent(map[string]any{"run_id": *run, "members": reports}, "", "  ")
		if err != nil {
			return 1
		}
		file, err := os.OpenFile(*reportOut, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			fmt.Fprintln(stderr, "cannot create private replay report")
			return 1
		}
		_, writeErr := file.Write(raw)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return 1
		}
	}
	summary := []map[string]any{}
	for _, report := range reports {
		outputHash := sha256.Sum256([]byte(report.Output))
		summary = append(summary, map[string]any{"member_sha256": digestReplayID(report.MemberRunID), "outcome": report.Outcome, "segments": len(report.Segments), "verified_segments": report.VerifiedSegments, "operations": report.Operations, "replayed": report.Replayed, "configuration_verified": report.ConfigVerified, "configuration_sha256": report.ConfigurationSHA256, "output_sha256": hex.EncodeToString(outputHash[:]), "no_live_calls": report.NoLiveCalls})
	}
	if err := json.NewEncoder(stdout).Encode(map[string]any{"run_sha256": digestReplayID(*run), "members": summary}); err != nil {
		return 1
	}
	for _, report := range reports {
		if report.Outcome != loomruntime.ReplayCompleted || !report.ConfigVerified {
			return 3
		}
	}
	return 0
}

func digestReplayID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func loadReplayConfiguration(ctx context.Context, tx pgx.Tx, workspace, snapshot, node string) (loomruntime.MemberReplayConfiguration, error) {
	payload, err := loadRunReplayArtifact(ctx, tx, workspace, snapshot)
	if err != nil {
		return loomruntime.MemberReplayConfiguration{}, err
	}
	var graph struct {
		Nodes []struct {
			ID     string `json:"id"`
			Type   string `json:"type"`
			Config struct {
				AgentID      string `json:"agent_id"`
				AgentVersion int64  `json:"agent_version"`
			} `json:"config"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(payload.GraphDefinition, &graph); err != nil {
		return loomruntime.MemberReplayConfiguration{}, err
	}
	for _, entry := range graph.Nodes {
		if entry.ID != node {
			continue
		}
		id, version := entry.Config.AgentID, entry.Config.AgentVersion
		if entry.Type == "lead" {
			id, version = payload.Team.LeadAgentID, payload.Team.LeadAgentVersion
		}
		for _, bundle := range payload.Bundles {
			if bundle.Agent.AgentID == id && bundle.Agent.AgentVersion == version {
				return loomruntime.FrozenMemberReplayConfiguration(bundle)
			}
		}
	}
	return loomruntime.MemberReplayConfiguration{}, fmt.Errorf("frozen member binding is missing")
}

func loadRunReplayArtifact(ctx context.Context, tx pgx.Tx, workspace, snapshot string) (frozen.ArtifactPayloadV1, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT to_jsonb(content) FROM weave_team_run_snapshots s JOIN weave_published_artifact_contents content ON content.workspace_id=s.workspace_id AND content.workflow_id=s.artifact_workflow_id AND content.workflow_version=s.artifact_workflow_version WHERE s.workspace_id=$1 AND s.run_id=$2`, workspace, snapshot).Scan(&raw)
	if err != nil {
		return frozen.ArtifactPayloadV1{}, err
	}
	var envelope frozen.ArtifactEnvelopeV1
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return frozen.ArtifactPayloadV1{}, err
	}
	return frozen.DecodeArtifactEnvelopeV1(envelope)
}
