package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/app/api"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

// runConfigCheck checks a deployment's configuration and the files it names
// before the server starts, in the same environment the server will get
// (`docker compose run --rm weave config-check`). An option that is not
// enabled needs no file. Nothing it prints contains a secret.
func runConfigCheck(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		_, _ = io.WriteString(stderr, "usage: weave config-check\n")
		return 2
	}
	failed := false
	report := func(state, item, detail string) {
		if state == "fail" {
			failed = true
		}
		_, _ = fmt.Fprintf(stdout, "%-4s %s: %s\n", state, item, detail)
	}
	switch cfg, err := config.Load(); {
	case err != nil:
		report("fail", "server configuration", err.Error())
	case cfg.LoginKeyDefault:
		report("ok", "server configuration", "development mode, with the development default login key")
	default:
		report("ok", "server configuration", "DATABASE_URL, JWT_SECRET and WEAVE_LOGIN_KEY are set")
	}
	keySource := "WEAVE_SECRET_KEY"
	if strings.TrimSpace(os.Getenv("WEAVE_SECRET_KEY_FILE")) != "" {
		keySource = "WEAVE_SECRET_KEY_FILE"
	}
	if _, err := secret.KeyFromEnv(); err != nil {
		report("fail", "credential encryption key", err.Error())
	} else {
		report("ok", "credential encryption key", "read from "+keySource)
	}
	// Timing settings that are easy to leave at a default unknowingly.
	if raw := strings.TrimSpace(os.Getenv("WEAVE_TEAMRUN_DRAIN_TIMEOUT")); raw == "" {
		report("ok", "shutdown drain", "WEAVE_TEAMRUN_DRAIN_TIMEOUT unset: a pooled executor stops without waiting for work in flight")
	} else if drain, err := time.ParseDuration(raw); err != nil || drain < 0 {
		report("fail", "shutdown drain", "WEAVE_TEAMRUN_DRAIN_TIMEOUT must be a non-negative duration such as 30s; the server would ignore it and not wait")
	} else {
		report("ok", "shutdown drain", "waits up to "+drain.String()+" for work in flight")
	}
	if cfg, err := config.Load(); err == nil {
		report("ok", "slow-run alert", fmt.Sprintf("%ds (WEAVE_HEALTH_SLOW_RUN_SECONDS); observation only, it never ends a run", cfg.HealthSlowRunSeconds))
		if len(cfg.HealthSlowRunOverrides) > 0 {
			workflows := make([]string, 0, len(cfg.HealthSlowRunOverrides))
			for workflowID := range cfg.HealthSlowRunOverrides {
				workflows = append(workflows, workflowID)
			}
			sort.Strings(workflows)
			for index, workflowID := range workflows {
				workflows[index] = workflowID + " " + cfg.HealthSlowRunOverrides[workflowID].String()
			}
			report("ok", "slow-run alert overrides", strings.Join(workflows, ", ")+" (WEAVE_HEALTH_SLOW_RUN_OVERRIDES)")
		}
	}
	if path := strings.TrimSpace(os.Getenv("WEAVE_ORGANIZATION_CONNECTION_FILE")); path == "" {
		report("off", "organization connection", "not configured; optional, needed only to download the organization connection from the console")
	} else if err := api.CheckOrganizationConnectionFile(path); err != nil {
		report("fail", "organization connection", err.Error())
	} else {
		report("ok", "organization connection", path)
	}
	if failed {
		return 1
	}
	return 0
}
