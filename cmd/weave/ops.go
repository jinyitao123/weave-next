package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/api"
)

const opsUsage = `usage: weave ops <command>

commands:
  events list-failed             list employee run events Forge permanently rejected
  events redeliver [--event ID]  requeue permanently failed events (all when no --event)

DATABASE_URL must point at the Weave database.`

// runOpsCommand is the operator-only surface for recovering state that no
// employee or client can see. It reads and repairs the database directly and
// is never exposed over HTTP.
func runOpsCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, opsUsage)
		return 2
	}
	switch args[0] {
	case "events":
		return runOpsEvents(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown ops command %q\n\n%s\n", args[0], opsUsage)
		return 2
	}
}

type eventIDFlags []string

func (values *eventIDFlags) String() string { return strings.Join(*values, ",") }
func (values *eventIDFlags) Set(v string) error {
	*values = append(*values, strings.TrimSpace(v))
	return nil
}

func runOpsEvents(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "list-failed" && args[0] != "redeliver") {
		fmt.Fprintln(stderr, opsUsage)
		return 2
	}
	command := args[0]
	flags := flag.NewFlagSet("weave ops events "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var eventIDs eventIDFlags
	if command == "redeliver" {
		flags.Var(&eventIDs, "event", "event ID to requeue; repeat for several. Without it every permanent failure is requeued")
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return 2
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		fmt.Fprintln(stderr, "DATABASE_URL is required")
		return 2
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(stderr, "database unavailable: %v\n", err)
		return 1
	}
	defer pool.Close()
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if command == "list-failed" {
		failures, err := api.ListEmployeeRunEventFailures(ctx, pool)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := encoder.Encode(map[string]any{"failed": failures, "count": len(failures)}); err != nil {
			return 1
		}
		return 0
	}
	requeued, err := api.RequeueEmployeeRunEventFailures(ctx, pool, eventIDs)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := encoder.Encode(map[string]any{"requeued": requeued}); err != nil {
		return 1
	}
	return 0
}
