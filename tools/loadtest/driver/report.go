package main

import (
	"fmt"
	"strings"
)

// writeDecide renders the decide ladder: outcomes and the step verdict, the
// time split, database work per completed workflow, waits and the most
// frequent statements of every step.
func writeDecide(text *strings.Builder, run concurrencyResult) {
	fmt.Fprintf(text, "\n## Decide, executor concurrency %d\n\n", run.Concurrency)
	text.WriteString("| Lanes | Submitted | Completed | Failed | Cancelled | Unsettled | Rejected | Success | All p50 / p95 ms | Completed/s | Running at window end | Backlog at window end | Step |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, d := range run.Decide {
		backlog := "-"
		if d.BacklogAtWindowEnd != nil {
			backlog = fmt.Sprintf("%d queued, %d running", d.BacklogAtWindowEnd.Queued, d.BacklogAtWindowEnd.Running)
		}
		p95 := fmt.Sprintf("%.0f", d.AllP95MS)
		if d.AllP95Unbounded {
			p95 = "unsettled"
		}
		step := "-"
		if d.Step != nil {
			step = "passed"
			if !d.Step.Passed {
				step = "stopped: " + d.Step.Reason
			}
		}
		fmt.Fprintf(text, "| %d | %d | %d | %d | %d | %d | %d | %.2f%% | %.0f / %s | %.2f | %d | %s | %s |\n", d.Lanes, d.Outcomes.Submitted,
			d.Outcomes.Completed, d.Outcomes.Failed, d.Outcomes.Cancelled, d.Outcomes.Unsettled, d.Outcomes.Rejected, 100*d.SuccessRate,
			d.AllP50MS, p95, d.Throughput, len(d.UnfinishedAtWindowEnd), backlog, step)
	}
	if l := run.Ladder; l != nil {
		if l.Exhausted {
			fmt.Fprintf(text, "\nEvery configured step passed up to %d lanes; that is the top of the ladder, not a capacity.\n", l.Passed)
		} else if l.StoppedAt > 0 {
			fmt.Fprintf(text, "\nLargest passing step: %d lanes; stopped at %d (%s); not run: %v.\n", l.Passed, l.StoppedAt, l.Reason, l.Skipped)
		}
	}
	text.WriteString("\n### Time split per completed invocation (ms, p50 / p95)\n\n| Lanes | Client total | Server | Model | Executor queue | Platform | Task queue wait (per task) |\n|---|---|---|---|---|---|---|\n")
	for _, d := range run.Decide {
		fmt.Fprintf(text, "| %d | %.0f / %.0f | %.0f / %.0f | %.0f / %.0f | %.0f / %.0f | %.0f / %.0f | %.0f / %.0f |\n", d.Lanes, d.TotalP50MS, d.TotalP95MS,
			d.ServerP50MS, d.ServerP95MS, d.ModelP50MS, d.ModelP95MS, d.QueueP50MS, d.QueueP95MS, d.PlatformP50MS, d.PlatformP95MS, d.TaskQueueP50MS, d.TaskQueueP95MS)
	}
	text.WriteString("\n### Database per completed workflow\n\n| Lanes | Statements | Transactions | Rollbacks | Statement ms | Notifying commits (share of commits) | Notify commit-lock wait s (whole step) | Pool acquires | Empty acquires (share) | Pool wait ms |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for _, d := range run.Decide {
		per := perWorkflow{}
		if d.PerWorkflow != nil {
			per = *d.PerWorkflow
		}
		n := max(1, float64(d.Invocations))
		notify := notifySummary{}
		if d.Notifications != nil {
			notify = *d.Notifications
		}
		pool := poolDelta{}
		if d.Pool != nil {
			pool = *d.Pool
		}
		fmt.Fprintf(text, "| %d | %.0f | %.1f | %.1f | %.1f | %.1f (%.1f%%) | %.1f | %.0f | %.1f (%.1f%%) | %.1f |\n", d.Lanes, per.Statements, per.Xacts, per.Rollbacks,
			d.Rates.StatementMS/n, float64(notify.Commits)/n, 100*notify.Share, notify.CommitLockSeconds, float64(pool.Acquires)/n,
			float64(pool.EmptyAcquires)/n, 100*pool.EmptyShare, 1000*pool.EmptyAcquireWaitS/n)
	}
	for _, d := range run.Decide {
		if d.Locks != nil && len(d.Locks.Waits) > 0 {
			fmt.Fprintf(text, "\n### Waits at %d lanes (%d samples every %s, %.1f active backends on average)\n\n| Wait | Object | Samples | Backend seconds |\n|---|---|---|---|\n",
				d.Lanes, d.Locks.Samples, d.Locks.Every, d.Locks.ActiveMean)
			for _, w := range d.Locks.Waits[:min(len(d.Locks.Waits), 15)] {
				fmt.Fprintf(text, "| %s | %s | %d | %.1f |\n", w.Wait, w.Object, w.Samples, w.Seconds)
			}
		}
		if d.PerWorkflow != nil && len(d.PerWorkflow.Top) > 0 {
			fmt.Fprintf(text, "\n### Top statements at %d lanes (calls per completed workflow)\n\n| Per workflow | ms | Query |\n|---|---|---|\n", d.Lanes)
			for _, st := range d.PerWorkflow.Top {
				fmt.Fprintf(text, "| %.1f | %.0f | `%s` |\n", st.PerWorkflow, st.ExecMS, strings.ReplaceAll(st.Query, "|", `\|`))
			}
		}
		if len(d.UnfinishedAtWindowEnd) > 0 {
			outcomes := map[string]int{}
			var totals []float64
			for _, u := range d.UnfinishedAtWindowEnd {
				outcomes[u.Outcome]++
				totals = append(totals, u.TotalMS)
			}
			fmt.Fprintf(text, "\nStill running at the %d-lane window end: %d %v; total p50 / p95 / max %.0f / %.0f / %.0f ms (each listed in report.json).\n",
				d.Lanes, len(d.UnfinishedAtWindowEnd), outcomes, percentile(totals, .5), percentile(totals, .95), percentile(totals, 1))
		}
	}
}
