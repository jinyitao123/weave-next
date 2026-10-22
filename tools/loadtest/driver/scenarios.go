package main

import (
	"context"
	"errors"
	"fmt"
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
	Lanes                 int     `json:"lanes"`
	Completed             int     `json:"completed"`
	Seconds               float64 `json:"seconds"`
	Throughput            float64 `json:"completed_per_second"`
	TotalP50MS            float64 `json:"total_p50_ms"`
	TotalP95MS            float64 `json:"total_p95_ms"`
	PlatformP50MS         float64 `json:"platform_p50_ms"`
	PlatformP95MS         float64 `json:"platform_p95_ms"`
	ObservedOverheadP95MS float64 `json:"observed_overhead_p95_ms"`
	ModelMaxInFlight      int64   `json:"model_max_in_flight"`
	Rates                 dbRates `json:"database_rates"`
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
	started, ended time.Time
	server         invocationTiming
}

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
func (e *scenarioEnv) invoke(ctx context.Context, flow, lane string) (measuredInvocation, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	measurement := measuredInvocation{started: time.Now()}
	result, probe, err := e.submit(ctx, flow, lane)
	measurement.probe = probe
	if err != nil {
		return measurement, err
	}
	result, err = e.await(ctx, result.InvocationID)
	measurement.server = result.timing
	measurement.ended = time.Now()
	if err != nil {
		return measurement, err
	}
	if result.Status != "completed" || result.ResultState != "available" {
		return measurement, fmt.Errorf("invocation %s: %s/%s %s", result.InvocationID, result.Status, result.ResultState, redact(result.Error))
	}
	return measurement, nil
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
func summarize(ctx context.Context, e *scenarioEnv, flow string, lanes int, measurements []measuredInvocation, started time.Time, before snapshot) (*latencyResult, error) {
	calls, err := e.stub.calls(ctx)
	if err != nil {
		return nil, err
	}
	byProbe := map[string][]stubCall{}
	for _, call := range calls {
		byProbe[call.Probe] = append(byProbe[call.Probe], call)
	}
	totals, platform, observed := []float64{}, []float64{}, []float64{}
	for _, measurement := range measurements {
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
		total := float64(measurement.ended.Sub(measurement.started)) / float64(time.Millisecond)
		totals = append(totals, total)
		observed = append(observed, max(0, total-modelMS-seatMS))
		serverTime := float64(measurement.server.terminal.Sub(measurement.server.created)) / float64(time.Millisecond)
		platform = append(platform, max(0, serverTime-modelMS-seatMS))
	}
	stats, err := e.stub.stats(ctx)
	if err != nil {
		return nil, err
	}
	flushStatsWait()
	after, err := e.pg.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	seconds := time.Since(started).Seconds()
	result := &latencyResult{Lanes: lanes, Completed: len(measurements), Seconds: seconds, Throughput: float64(len(measurements)) / seconds, TotalP50MS: percentile(totals, .5), TotalP95MS: percentile(totals, .95), PlatformP50MS: percentile(platform, .5), PlatformP95MS: percentile(platform, .95), ModelMaxInFlight: stats.MaxInflight, Rates: diffSnapshots(before, after)}
	result.ObservedOverheadP95MS = percentile(observed, .95)
	return result, nil
}

func runConsult(ctx context.Context, e *scenarioEnv) (*latencyResult, error) {
	if err := e.stub.reset(ctx); err != nil {
		return nil, err
	}
	before, err := e.pg.snapshot(ctx)
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
	result, err := summarize(ctx, e, flowConsult, e.o.s2Lanes, measurements, started, before)
	return result, errors.Join(append(failures, err)...)
}
func runDecide(ctx context.Context, e *scenarioEnv, lanes int) (*latencyResult, error) {
	if err := e.stub.reset(ctx); err != nil {
		return nil, err
	}
	before, err := e.pg.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	measureAt := started.Add(e.o.s1Warmup)
	finishAt := measureAt.Add(e.o.s1Duration)
	var mu sync.Mutex
	var measurements []measuredInvocation
	var failures []error
	var wg sync.WaitGroup
	for lane := 0; lane < lanes; lane++ {
		wg.Add(1)
		go func(lane int) {
			defer wg.Done()
			for time.Now().Before(finishAt) {
				measurement, err := e.invoke(ctx, flowDecide, fmt.Sprintf("decide-%d", lane))
				mu.Lock()
				if err != nil {
					failures = append(failures, err)
					mu.Unlock()
					return
				}
				if measurement.started.After(measureAt) {
					measurements = append(measurements, measurement)
				}
				mu.Unlock()
			}
		}(lane)
	}
	wg.Wait()
	result, err := summarize(ctx, e, flowDecide, lanes, measurements, started, before)
	return result, errors.Join(append(failures, err)...)
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
	Cancel         *cancelResult   `json:"cancel,omitempty"`
	Errors         []string        `json:"errors,omitempty"`
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
