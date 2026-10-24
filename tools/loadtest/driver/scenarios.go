package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

type scenarioEnv struct {
	watcher *invocationWatcher
	notify  *notifyCounter
	// console is signed in to the console for the pool counters.
	console *apiClient
	o       options
	pg      *pgEnv
	stub    *stubClient
	weave   *weaveInstance
	prov    *provisioned
	conc    int
	caller  *apiClient
}
type idleResult struct {
	Rates dbRates `json:"database_rates"`
}
type latencyResult struct {
	Lanes      int     `json:"lanes"`
	Completed  int     `json:"completed"`
	Seconds    float64 `json:"seconds"`
	Throughput float64 `json:"completed_per_second"`
	// TotalP50MS and TotalP95MS cover completed invocations only; AllP50MS
	// and AllP95MS cover every measured invocation whatever its outcome,
	// with an invocation that never settled counted as slower than any
	// other (AllP95Unbounded then says the P95 is one of them).
	TotalP50MS            float64 `json:"total_p50_ms"`
	TotalP95MS            float64 `json:"total_p95_ms"`
	PlatformP50MS         float64 `json:"platform_p50_ms"`
	PlatformP95MS         float64 `json:"platform_p95_ms"`
	ObservedOverheadP95MS float64 `json:"observed_overhead_p95_ms"`
	ModelMaxInFlight      int64   `json:"model_max_in_flight"`
	Rates                 dbRates `json:"database_rates"`
	// Invocations counts every invocation completed between the database
	// snapshots, warm-up included, so per-workflow statement and
	// transaction counts divide by the same span the rates cover.
	Invocations int `json:"invocations"`

	Outcomes        outcomeCounts `json:"outcomes"`
	SuccessRate     float64       `json:"success_rate"`
	AllP50MS        float64       `json:"all_p50_ms"`
	AllP95MS        float64       `json:"all_p95_ms"`
	AllP95Unbounded bool          `json:"all_p95_unbounded,omitempty"`
	// Time split per completed invocation: server time from admission to
	// the run's terminal state, model time from the stub's receipts (the
	// slowest parallel seat plus the sequential calls), executor queueing
	// from the task queue, and the platform's own time as what remains.
	ServerP50MS float64 `json:"server_p50_ms"`
	ServerP95MS float64 `json:"server_p95_ms"`
	ModelP50MS  float64 `json:"model_p50_ms"`
	ModelP95MS  float64 `json:"model_p95_ms"`
	QueueP50MS  float64 `json:"queue_p50_ms"`
	QueueP95MS  float64 `json:"queue_p95_ms"`
	// TaskQueueP50MS and TaskQueueP95MS are per task, not per invocation.
	TaskQueueP50MS float64 `json:"task_queue_p50_ms"`
	TaskQueueP95MS float64 `json:"task_queue_p95_ms"`
	// UnfinishedAtWindowEnd lists the measured invocations still running
	// when the window closed; the driver waited for each of them.
	UnfinishedAtWindowEnd []unfinishedSample `json:"unfinished_at_window_end,omitempty"`
	BacklogAtWindowEnd    *backlogCount      `json:"backlog_at_window_end,omitempty"`
	PerWorkflow           *perWorkflow       `json:"per_completed_workflow,omitempty"`
	Locks                 *lockSummary       `json:"locks,omitempty"`
	Notifications         *notifySummary     `json:"notifications,omitempty"`
	Pool                  *poolDelta         `json:"connection_pool,omitempty"`
	Step                  *stepVerdict       `json:"step,omitempty"`
}
type outcomeCounts struct {
	Submitted int `json:"submitted"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
	// Unsettled invocations had not reached a terminal state when the
	// driver gave up waiting for them.
	Unsettled int `json:"unsettled"`
	// Rejected submissions never became an invocation (for example 429).
	Rejected int `json:"rejected"`
}
type unfinishedSample struct {
	Outcome string  `json:"outcome"`
	TotalMS float64 `json:"total_ms"`
}
type backlogCount struct {
	Queued  int `json:"queued"`
	Running int `json:"running"`
}
type perWorkflow struct {
	Statements float64         `json:"statements"`
	Xacts      float64         `json:"xacts"`
	Rollbacks  float64         `json:"rollbacks"`
	Top        []statementRate `json:"top_statements"`
}
type stepVerdict struct {
	Passed bool   `json:"passed"`
	Reason string `json:"reason,omitempty"`
}
type cancelResult struct {
	Cancelled       int     `json:"cancelled"`
	P95MS           float64 `json:"p95_ms"`
	ProviderStopped bool    `json:"provider_stopped"`
}
type invocation struct {
	timing       invocationTiming
	InvocationID string `json:"invocation_id"`
	RunID        string `json:"run_id"`
	Status       string `json:"status"`
	ResultState  string `json:"result_state"`
	Error        string `json:"error"`
}
type measuredInvocation struct {
	probe          string
	runID          string
	outcome        string
	started, ended time.Time
	server         invocationTiming
}

// Outcomes of one measured invocation.
const (
	outcomeCompleted = "completed"
	outcomeFailed    = "failed"
	outcomeCancelled = "cancelled"
	outcomeUnsettled = "unsettled"
	outcomeRejected  = "rejected"
)

func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (e *scenarioEnv) submit(ctx context.Context, flow, lane string) (invocation, string, error) {
	id := newUUID()
	probe := "ltp-" + randomHex(10)
	input := map[string]any{"request_id": id, "probe": probe, "lane": lane, "topic": "Inspect the current constraints and choose a legal plan."}
	var result invocation
	_, err := e.caller.doAuth(ctx, http.MethodPost, fmt.Sprintf("/v1/capabilities/%s/versions/%d/invocations", e.prov.Capabilities[flow], e.prov.Revisions[flow]), e.prov.credential, map[string]any{"request_id": id, "invocation_id": id, "lane": lane, "input": input}, &result, http.StatusAccepted)
	return result, probe, err
}
func (e *scenarioEnv) await(ctx context.Context, id string) (invocation, error) {
	var timing invocationTiming
	if e.watcher != nil {
		var err error
		timing, err = e.watcher.wait(ctx, id)
		if err != nil {
			return invocation{}, err
		}
	}
	for {
		var result invocation
		_, err := e.caller.doAuth(ctx, http.MethodGet, "/v1/invocations/"+id, e.prov.credential, nil, &result)
		if err != nil {
			return result, err
		}
		switch result.Status {
		case "completed", "failed", "cancelled":
			result.timing = timing
			return result, nil
		}
		if err := pause(ctx, max(5*time.Millisecond, e.o.watchEvery)); err != nil {
			return result, err
		}
	}
}

// invoke submits one invocation and waits for it. The measurement's outcome
// says how it ended; the error explains any outcome but completed.
func (e *scenarioEnv) invoke(ctx context.Context, flow, lane string) (measuredInvocation, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	measurement := measuredInvocation{started: time.Now()}
	result, probe, err := e.submit(ctx, flow, lane)
	measurement.probe = probe
	if err != nil {
		measurement.ended = time.Now()
		measurement.outcome = outcomeRejected
		if ctx.Err() != nil {
			measurement.outcome = outcomeUnsettled
		}
		return measurement, err
	}
	measurement.runID = result.RunID
	result, err = e.await(ctx, result.InvocationID)
	measurement.server = result.timing
	measurement.ended = time.Now()
	if result.RunID != "" {
		measurement.runID = result.RunID
	}
	if err != nil {
		measurement.outcome = outcomeUnsettled
		return measurement, err
	}
	switch {
	case result.Status == "completed" && result.ResultState == "available":
		measurement.outcome = outcomeCompleted
		return measurement, nil
	case result.Status == "cancelled":
		measurement.outcome = outcomeCancelled
	default:
		measurement.outcome = outcomeFailed
	}
	return measurement, fmt.Errorf("invocation %s: %s/%s %s", result.InvocationID, result.Status, result.ResultState, redact(result.Error))
}

func runIdle(ctx context.Context, e *scenarioEnv) (*idleResult, error) {
	if err := pause(ctx, e.o.s3Settle); err != nil {
		return nil, err
	}
	before, err := e.pg.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if err := pause(ctx, e.o.s3Window); err != nil {
		return nil, err
	}
	flushStatsWait()
	after, err := e.pg.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return &idleResult{Rates: diffSnapshots(before, after)}, nil
}
func percentile(values []float64, fraction float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	return values[min(len(values)-1, int(float64(len(values)-1)*fraction))]
}

// stepProbes are the probes that run during one step's database span.
type stepProbes struct {
	before      snapshot
	locks       *lockSampler
	notify      map[string]int64
	pool        poolStats
	poolErr     error
	windowEnd   time.Time
	backlog     *backlogCount
	invocations int
}

func (e *scenarioEnv) startProbes(ctx context.Context) (*stepProbes, error) {
	before, err := e.pg.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	p := &stepProbes{before: before}
	p.pool, p.poolErr = e.poolStats(ctx)
	if e.notify != nil {
		p.notify = e.notify.counts()
	}
	if p.locks, err = e.pg.startLocks(ctx, e.o.lockEvery); err != nil {
		return nil, err
	}
	return p, nil
}

func summarize(ctx context.Context, e *scenarioEnv, flow string, lanes int, measurements []measuredInvocation, started time.Time, probes *stepProbes) (*latencyResult, error) {
	locks := probes.locks.Stop()
	calls, err := e.stub.calls(ctx)
	if err != nil {
		return nil, err
	}
	byProbe := map[string][]stubCall{}
	for _, call := range calls {
		byProbe[call.Probe] = append(byProbe[call.Probe], call)
	}
	var runIDs []string
	for _, measurement := range measurements {
		if measurement.outcome == outcomeCompleted && measurement.runID != "" {
			runIDs = append(runIDs, measurement.runID)
		}
	}
	queued, taskWaits, err := e.pg.queueWaits(ctx, runIDs)
	if err != nil {
		return nil, err
	}
	result := &latencyResult{Lanes: lanes, BacklogAtWindowEnd: probes.backlog}
	totals, platform, observed, all, server, models, queues := []float64{}, []float64{}, []float64{}, []float64{}, []float64{}, []float64{}, []float64{}
	for _, measurement := range measurements {
		total := float64(measurement.ended.Sub(measurement.started)) / float64(time.Millisecond)
		result.Outcomes.Submitted++
		switch measurement.outcome {
		case outcomeCompleted:
			result.Outcomes.Completed++
		case outcomeFailed:
			result.Outcomes.Failed++
		case outcomeCancelled:
			result.Outcomes.Cancelled++
		case outcomeRejected:
			result.Outcomes.Rejected++
		default:
			result.Outcomes.Unsettled++
		}
		if measurement.outcome == outcomeUnsettled {
			all = append(all, math.Inf(1))
		} else {
			all = append(all, total)
		}
		if !probes.windowEnd.IsZero() && measurement.started.Before(probes.windowEnd) && measurement.ended.After(probes.windowEnd) {
			result.UnfinishedAtWindowEnd = append(result.UnfinishedAtWindowEnd, unfinishedSample{Outcome: measurement.outcome, TotalMS: total})
		}
		if measurement.outcome != outcomeCompleted {
			continue
		}
		modelMS, seatMS := 0.0, 0.0
		for _, call := range byProbe[measurement.probe] {
			if call.EndNS <= call.StartNS {
				return nil, fmt.Errorf("model call for %s has no stop receipt", measurement.probe)
			}
			elapsed := float64(call.EndNS-call.StartNS) / 1e6
			if call.Model == modelSeat {
				seatMS = max(seatMS, elapsed)
			} else {
				modelMS += elapsed
			}
		}
		if len(byProbe[measurement.probe]) == 0 {
			return nil, fmt.Errorf("no provider evidence for %s", measurement.probe)
		}
		totals = append(totals, total)
		observed = append(observed, max(0, total-modelMS-seatMS))
		serverTime := float64(measurement.server.terminal.Sub(measurement.server.created)) / float64(time.Millisecond)
		queueMS := queued[measurement.runID]
		server = append(server, serverTime)
		models = append(models, modelMS+seatMS)
		queues = append(queues, queueMS)
		platform = append(platform, max(0, serverTime-modelMS-seatMS-queueMS))
	}
	stats, err := e.stub.stats(ctx)
	if err != nil {
		return nil, err
	}
	poolAfter, poolErr := e.poolStats(ctx)
	flushStatsWait()
	after, err := e.pg.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	seconds := time.Since(started).Seconds()
	result.Completed, result.Seconds = len(totals), seconds
	result.Throughput = float64(len(totals)) / seconds
	result.TotalP50MS, result.TotalP95MS = percentile(totals, .5), percentile(totals, .95)
	result.PlatformP50MS, result.PlatformP95MS = percentile(platform, .5), percentile(platform, .95)
	result.ObservedOverheadP95MS = percentile(observed, .95)
	result.ServerP50MS, result.ServerP95MS = percentile(server, .5), percentile(server, .95)
	result.ModelP50MS, result.ModelP95MS = percentile(models, .5), percentile(models, .95)
	result.QueueP50MS, result.QueueP95MS = percentile(queues, .5), percentile(queues, .95)
	result.TaskQueueP50MS, result.TaskQueueP95MS = percentile(taskWaits, .5), percentile(taskWaits, .95)
	result.AllP50MS, result.AllP95MS = percentile(all, .5), percentile(all, .95)
	if math.IsInf(result.AllP95MS, 1) {
		result.AllP95MS, result.AllP95Unbounded = 0, true
	}
	if math.IsInf(result.AllP50MS, 1) {
		result.AllP50MS = 0
	}
	if result.Outcomes.Submitted > 0 {
		result.SuccessRate = float64(result.Outcomes.Completed) / float64(result.Outcomes.Submitted)
	}
	result.ModelMaxInFlight = stats.MaxInflight
	result.Rates = diffSnapshots(probes.before, after)
	result.Invocations = probes.invocations
	if result.Invocations == 0 {
		result.Invocations = len(totals)
	}
	result.Locks = &locks
	if e.notify != nil {
		notify := diffNotify(probes.notify, e.notify.counts(), result.Rates, locks)
		result.Notifications = &notify
	}
	pool := diffPool(probes.pool, poolAfter, probes.poolErr, poolErr)
	result.Pool = &pool
	if result.Invocations > 0 {
		n := float64(result.Invocations)
		per := &perWorkflow{Statements: float64(result.Rates.Statements) / n, Xacts: float64(result.Rates.TotalXacts) / n, Rollbacks: float64(result.Rates.Rollbacks) / n}
		for _, statement := range result.Rates.TopStatements[:min(len(result.Rates.TopStatements), e.o.topStatements)] {
			statement.PerWorkflow = float64(statement.Calls) / n
			per.Top = append(per.Top, statement)
		}
		result.PerWorkflow = per
	}
	return result, nil
}

func runConsult(ctx context.Context, e *scenarioEnv) (*latencyResult, error) {
	if err := e.stub.reset(ctx); err != nil {
		return nil, err
	}
	probes, err := e.startProbes(ctx)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	var mu sync.Mutex
	var measurements []measuredInvocation
	var failures []error
	var wg sync.WaitGroup
	for lane := 0; lane < e.o.s2Lanes; lane++ {
		wg.Add(1)
		go func(lane int) {
			defer wg.Done()
			for i := 0; i < e.o.s2Count; i++ {
				measurement, err := e.invoke(ctx, flowConsult, fmt.Sprintf("consult-%d", lane))
				mu.Lock()
				if err != nil {
					failures = append(failures, err)
					mu.Unlock()
					return
				}
				measurements = append(measurements, measurement)
				mu.Unlock()
			}
		}(lane)
	}
	wg.Wait()
	result, err := summarize(ctx, e, flowConsult, e.o.s2Lanes, measurements, started, probes)
	return result, errors.Join(append(failures, err)...)
}

// runDecide runs one step of the decide ladder: lanes callers submit back
// to back until the window closes, every outcome is recorded, and the driver
// then waits for every invocation still running before it measures.
func runDecide(ctx context.Context, e *scenarioEnv, lanes int) (*latencyResult, error) {
	if err := e.stub.reset(ctx); err != nil {
		return nil, err
	}
	probes, err := e.startProbes(ctx)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	measureAt := started.Add(e.o.s1Warmup)
	finishAt := measureAt.Add(e.o.s1Duration)
	probes.windowEnd = finishAt
	var mu sync.Mutex
	var measurements []measuredInvocation
	var failures []string
	var wg sync.WaitGroup
	completed := 0
	for lane := 0; lane < lanes; lane++ {
		wg.Add(1)
		go func(lane int) {
			defer wg.Done()
			for time.Now().Before(finishAt) && ctx.Err() == nil {
				measurement, err := e.invoke(ctx, flowDecide, fmt.Sprintf("decide-%d", lane))
				mu.Lock()
				if measurement.outcome == outcomeCompleted {
					completed++
				}
				if measurement.started.After(measureAt) {
					measurements = append(measurements, measurement)
				}
				if err != nil && len(failures) < 20 {
					failures = append(failures, err.Error())
				}
				mu.Unlock()
				if err != nil {
					// A failing or rejecting Weave must not turn a lane into a
					// tight loop.
					_ = pause(ctx, 200*time.Millisecond)
				}
			}
		}(lane)
	}
	if err := pause(ctx, time.Until(finishAt)); err == nil {
		if queued, running, err := e.pg.backlog(ctx); err == nil {
			probes.backlog = &backlogCount{Queued: queued, Running: running}
		}
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	probes.invocations = completed
	result, err := summarize(ctx, e, flowDecide, lanes, measurements, started, probes)
	if err != nil {
		return result, err
	}
	result.Step = e.verdict(result)
	log.Printf("decide %d lanes: %d/%d completed, all p95 %.0f ms, %.2f completed/s, step passed=%v %s", lanes,
		result.Outcomes.Completed, result.Outcomes.Submitted, result.AllP95MS, result.Throughput, result.Step.Passed, result.Step.Reason)
	if len(failures) > 0 {
		log.Printf("decide %d lanes: %d of %d measured invocations did not complete; first: %s", lanes,
			result.Outcomes.Submitted-result.Outcomes.Completed, result.Outcomes.Submitted, failures[0])
	}
	return result, nil
}

// verdict applies the step-up rule: the next step runs only when this one
// completed at least the required share of its invocations and the P95 of all
// of them, unsettled ones counted as slowest, met the target.
func (e *scenarioEnv) verdict(result *latencyResult) *stepVerdict {
	switch {
	case result.Outcomes.Submitted == 0:
		return &stepVerdict{Reason: "no invocation was measured"}
	case result.SuccessRate < e.o.minSuccess:
		return &stepVerdict{Reason: fmt.Sprintf("success %.2f%% < %.2f%%", 100*result.SuccessRate, 100*e.o.minSuccess)}
	case result.AllP95Unbounded:
		return &stepVerdict{Reason: "P95 falls on an invocation that never settled"}
	case e.o.p95Target > 0 && result.AllP95MS > float64(e.o.p95Target)/float64(time.Millisecond):
		return &stepVerdict{Reason: fmt.Sprintf("P95 %.0f ms > target %s", result.AllP95MS, e.o.p95Target)}
	}
	return &stepVerdict{Passed: true}
}

func runCancel(ctx context.Context, e *scenarioEnv) (*cancelResult, error) {
	if err := e.stub.setProfiles(ctx, map[string]stubProfile{modelConsult: {Latency: "fixed:1ms", PHang: 1, HangMax: "30s"}}); err != nil {
		return nil, err
	}
	defer e.stub.configure(context.Background(), e.o.modelLatency, e.o.consultLatency)
	if err := e.stub.reset(ctx); err != nil {
		return nil, err
	}
	var pending []invocation
	for i := 0; i < e.o.cancelCount; i++ {
		result, _, err := e.submit(ctx, flowConsult, fmt.Sprintf("cancel-%d", i))
		if err != nil {
			return nil, err
		}
		pending = append(pending, result)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		stats, err := e.stub.stats(ctx)
		if err != nil {
			return nil, err
		}
		if stats.Inflight >= int64(min(e.o.cancelCount, e.conc)) {
			break
		}
		if time.Now().After(deadline) {
			return nil, errors.New("cancel scenario model calls did not start")
		}
		if err := pause(ctx, 20*time.Millisecond); err != nil {
			return nil, err
		}
	}
	if err := pause(ctx, e.o.cancelAfter); err != nil {
		return nil, err
	}
	var durations []float64
	for _, result := range pending {
		started := time.Now()
		if _, err := e.caller.doAuth(ctx, http.MethodPost, "/v1/invocations/"+result.InvocationID+"/cancel", e.prov.credential, map[string]any{}, nil, http.StatusAccepted, http.StatusOK); err != nil {
			return nil, err
		}
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		terminal, err := e.await(waitCtx, result.InvocationID)
		cancel()
		if err != nil {
			return nil, err
		}
		if terminal.Status != "cancelled" {
			return nil, fmt.Errorf("cancel settled as %s", terminal.Status)
		}
		durations = append(durations, float64(time.Since(started))/float64(time.Millisecond))
	}
	stats, err := e.stub.stats(ctx)
	if err != nil {
		return nil, err
	}
	result := &cancelResult{Cancelled: len(durations), P95MS: percentile(durations, .95), ProviderStopped: stats.Inflight == 0}
	if !result.ProviderStopped {
		return result, errors.New("cancellation reported before all model requests stopped")
	}
	return result, nil
}

type concurrencyResult struct {
	Concurrency    int             `json:"concurrency"`
	StartupSeconds float64         `json:"startup_seconds"`
	Idle           *idleResult     `json:"idle,omitempty"`
	Consult        *latencyResult  `json:"consult,omitempty"`
	Decide         []latencyResult `json:"decide,omitempty"`
	Ladder         *ladderResult   `json:"decide_ladder,omitempty"`
	Cancel         *cancelResult   `json:"cancel,omitempty"`
	Errors         []string        `json:"errors,omitempty"`
}

// ladderResult is where the decide ladder stopped. Passed is the largest
// lane count that met the step rule; when every configured step passed
// (Exhausted) it is only the top of the ladder, not a capacity.
type ladderResult struct {
	Passed    int    `json:"passed_lanes"`
	StoppedAt int    `json:"stopped_at_lanes,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Skipped   []int  `json:"skipped_lanes,omitempty"`
	Exhausted bool   `json:"ladder_exhausted"`
}

type report struct {
	Label    string              `json:"label"`
	Binary   string              `json:"binary"`
	Started  time.Time           `json:"started"`
	Finished time.Time           `json:"finished"`
	Host     map[string]any      `json:"host"`
	Postgres serverInfo          `json:"postgres"`
	Options  map[string]any      `json:"options"`
	Runs     []concurrencyResult `json:"runs"`
}

func (r *report) write(dir string) error {
	if err := writeJSONFile(filepath.Join(dir, "report.json"), r); err != nil {
		return err
	}
	var text strings.Builder
	fmt.Fprintf(&text, "# Execution load test\n\nLabel: %s\n\n| Concurrency | Consult completed | Total p95 ms | Platform p95 ms | Cancelled | Cancel p95 ms | Errors |\n|---|---|---|---|---|---|---|\n", r.Label)
	for _, run := range r.Runs {
		consult, cancelled := latencyResult{}, cancelResult{}
		if run.Consult != nil {
			consult = *run.Consult
		}
		if run.Cancel != nil {
			cancelled = *run.Cancel
		}
		fmt.Fprintf(&text, "| %d | %d | %.1f | %.1f | %d | %.1f | %s |\n", run.Concurrency, consult.Completed, consult.TotalP95MS, consult.PlatformP95MS, cancelled.Cancelled, cancelled.P95MS, strings.Join(run.Errors, "; "))
	}
	for _, run := range r.Runs {
		if len(run.Decide) > 0 {
			writeDecide(&text, run)
		}
	}
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(text.String()), 0644)
}
func (p *pgEnv) dropDatabase(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ownsDatabase || forbiddenDatabases[strings.ToLower(p.testDB)] || !strings.HasPrefix(p.testDB, "weave_") {
		return errors.New("unsafe load test database")
	}
	_, err := p.admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{p.testDB}.Sanitize())
	return err
}
