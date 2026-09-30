package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/api"
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
