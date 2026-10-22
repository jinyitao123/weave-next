// Command driver runs the execution-plane load test against one Weave binary.
//
// For each executor concurrency it recreates an isolated test database,
// starts the Weave binary on a load-test port pointed at that database and at
// the local model stub, provisions a council team (four seats in parallel +
// ruler) and a single-member team through the real HTTP API, publishes their
// capabilities, issues an application credential and then runs the selected
// scenarios. Results are written as JSON and Markdown.
//
// It never connects to the database named "weave" and refuses port 8080.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

type options struct {
	executors    int
	weaveBin     string
	stubBin      string
	stubURL      string
	stubAddr     string
	envFile      string
	testDB       string
	port         int
	concurrency  []int
	scenarios    []string
	lanes        []int
	outDir       string
	label        string
	weaveEnv     multiFlag
	poolMaxConns string

	modelLatency   string
	consultLatency string
	s1Duration     time.Duration
	s1Warmup       time.Duration
	s2Count        int
	s2Lanes        int
	s3Window       time.Duration
	s3Settle       time.Duration
	cancelCount    int
	cancelAfter    time.Duration
	watchEvery     time.Duration
	keepDB         bool
}

func parseInts(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid number %q", part)
		}
		out = append(out, n)
	}
	return out, nil
}

func main() {
	var o options
	var conc, lanes, scen string
	flag.StringVar(&o.weaveBin, "weave-bin", "", "Weave binary to test (required)")
	flag.StringVar(&o.label, "label", "", "label for this binary in reports (default: binary file name)")
	flag.StringVar(&o.stubBin, "stub-bin", "", "model stub binary; started by the driver unless -stub-url is set")
	flag.StringVar(&o.stubURL, "stub-url", "", "use an already running model stub at this base URL")
	flag.StringVar(&o.stubAddr, "stub-addr", "127.0.0.1:18090", "listen address for a stub started by the driver")
	flag.StringVar(&o.envFile, "env-file", `C:\Users\jinyi\code\.local-dev.env`, "file with PGHOST/PGPORT/PGUSER/PGPASSWORD")
	flag.StringVar(&o.testDB, "db", "weave_loadtest", "isolated test database (dropped and recreated per concurrency)")
	flag.IntVar(&o.port, "port", 18080, "port for the load-test Weave")
	flag.IntVar(&o.executors, "executors", 0, "separate workflow executor processes; 0 keeps the combined API/executor process")
	flag.StringVar(&conc, "concurrency", "4,256,1000", "WEAVE_TEAMRUN_EXECUTOR_CONCURRENCY values, one Weave run each")
	flag.StringVar(&scen, "scenarios", "idle,consult,decide,cancel", "scenarios to run: idle,consult,decide,cancel")
	flag.StringVar(&lanes, "lanes", "10,50,200", "decide lane counts")
	flag.StringVar(&o.outDir, "out", "", "output directory (default ./loadtest-out/<timestamp>)")
	flag.Var(&o.weaveEnv, "weave-env", "extra KEY=VALUE for the Weave process (repeatable), e.g. WEAVE_TEAMRUN_EXECUTOR=pooled")
	flag.StringVar(&o.poolMaxConns, "pool-max-conns", "", "append pool_max_conns to the Weave DATABASE_URL (empty: pgx default)")
	flag.StringVar(&o.modelLatency, "model-latency", "fixed:2s", "stub latency for seat and ruler models (decide)")
	flag.StringVar(&o.consultLatency, "consult-latency", "fixed:200ms", "stub latency for the consult model")
	flag.DurationVar(&o.s1Duration, "decide-duration", 60*time.Second, "measured window per decide lane count")
	flag.DurationVar(&o.s1Warmup, "decide-warmup", 10*time.Second, "warm-up before the decide window")
	flag.IntVar(&o.s2Count, "consult-count", 60, "consult invocations per lane")
	flag.IntVar(&o.s2Lanes, "consult-lanes", 1, "parallel consult lanes")
	flag.DurationVar(&o.s3Window, "idle-window", 60*time.Second, "idle measurement window")
	flag.DurationVar(&o.s3Settle, "idle-settle", 15*time.Second, "settle time before the idle window")
	flag.IntVar(&o.cancelCount, "cancel-count", 20, "invocations cancelled in the cancel scenario")
	flag.DurationVar(&o.cancelAfter, "cancel-after", 1500*time.Millisecond, "how long after the first model call the cancel is sent")
	flag.DurationVar(&o.watchEvery, "watch-every", 50*time.Millisecond, "completion watch period (one batched SQL per tick on the test DB)")
	flag.BoolVar(&o.keepDB, "keep-db", false, "keep the last test database instead of dropping it at the end")
	flag.Parse()

	var err error
	if o.concurrency, err = parseInts(conc); err != nil {
		log.Fatal(err)
	}
	if o.lanes, err = parseInts(lanes); err != nil {
		log.Fatal(err)
	}
	for _, s := range strings.Split(scen, ",") {
		if s = strings.TrimSpace(s); s != "" {
			o.scenarios = append(o.scenarios, s)
		}
	}
	if o.weaveBin == "" {
		log.Fatal("-weave-bin is required")
	}
	if o.port == 8080 {
		log.Fatal("refusing port 8080: it belongs to the running Weave")
	}
	if forbiddenDatabases[strings.ToLower(o.testDB)] {
		log.Fatalf("refusing database %q", o.testDB)
	}
	if o.stubURL == "" && o.stubBin == "" {
		log.Fatal("set -stub-bin (driver starts the stub) or -stub-url")
	}
	if o.label == "" {
		o.label = strings.TrimSuffix(filepath.Base(o.weaveBin), filepath.Ext(o.weaveBin))
	}
	if o.outDir == "" {
		o.outDir = filepath.Join("loadtest-out", time.Now().Format("20060102-150405")+"-"+o.label)
	}
	if err := os.MkdirAll(o.outDir, 0o755); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, o); err != nil {
		log.Fatalf("load test failed: %v", err)
	}
}

func run(ctx context.Context, o options) error {
	target, err := loadPGTarget(o.envFile)
	if err != nil {
		return err
	}
	pg, err := openPGEnv(ctx, target, o.testDB)
	if err != nil {
		return err
	}
	defer pg.Close()
	info, err := pg.info(ctx)
	if err != nil {
		return err
	}
	log.Printf("PostgreSQL: %s; max_connections=%d; pg_stat_statements loaded=%v", firstLine(info.Version), info.MaxConnections, info.PGStatStatements)

	stubURL := o.stubURL
	var stub *child
	if stubURL == "" {
		stub, err = startChild("stub", o.stubBin, []string{"-addr", o.stubAddr, "-latency", o.modelLatency}, baseEnv(), filepath.Join(o.outDir, "stub.log"))
		if err != nil {
			return err
		}
		defer stub.Stop()
		stubURL = "http://" + o.stubAddr
		if err := waitHTTP(ctx, stubURL+"/health", stub, 20*time.Second); err != nil {
			return err
		}
	}
	sc := &stubClient{base: stubURL}

	report := &report{
		Label: o.label, Binary: o.weaveBin, Started: time.Now(), Host: hostInfo(), Postgres: info,
		Options: map[string]any{
			"model_latency": o.modelLatency, "consult_latency": o.consultLatency, "decide_duration": o.s1Duration.String(),
			"decide_warmup": o.s1Warmup.String(), "lanes": o.lanes, "consult_count": o.s2Count, "consult_lanes": o.s2Lanes,
			"idle_window": o.s3Window.String(), "cancel_count": o.cancelCount, "cancel_after": o.cancelAfter.String(),
			"weave_env": []string(o.weaveEnv), "pool_max_conns": o.poolMaxConns, "watch_every": o.watchEvery.String(),
		},
	}
	defer func() {
		report.Finished = time.Now()
		if err := report.write(o.outDir); err != nil {
			log.Printf("write report: %v", err)
		}
	}()

	for _, c := range o.concurrency {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, err := runConcurrency(ctx, o, pg, sc, stubURL, c)
		if res != nil {
			report.Runs = append(report.Runs, *res)
		}
		if err != nil {
			return fmt.Errorf("concurrency %d: %w", c, err)
		}
		if err := report.write(o.outDir); err != nil {
			log.Printf("write report: %v", err)
		}
	}
	if !o.keepDB {
		_ = pg.dropDatabase(ctx)
	}
	return nil
}

func runConcurrency(ctx context.Context, o options, pg *pgEnv, sc *stubClient, stubURL string, conc int) (*concurrencyResult, error) {
	log.Printf("=== executor concurrency %d ===", conc)
	if err := pg.recreate(ctx); err != nil {
		return nil, err
	}
	params := map[string]string{"application_name": "weave-loadtest"}
	if o.poolMaxConns != "" {
		params["pool_max_conns"] = o.poolMaxConns
	}
	dbURL, err := pg.target.url(o.testDB, params)
	if err != nil {
		return nil, err
	}
	workDir, _ := filepath.Abs(filepath.Join(o.outDir, fmt.Sprintf("c%d", conc)))
	if err := sc.configure(ctx, o.modelLatency, o.consultLatency); err != nil {
		return nil, err
	}
	started := time.Now()
	extraEnv := append(multiFlag(nil), o.weaveEnv...)
	if o.executors > 0 {
		extraEnv = append(extraEnv, "WEAVE_TEAMRUN_WORKERS_DISABLED=true")
	}
	w, err := startWeave(ctx, weaveOptions{
		bin: o.weaveBin, port: o.port, dbURL: dbURL, stubURL: stubURL, concurrency: conc,
		workDir: workDir, extraEnv: extraEnv, logPath: filepath.Join(workDir, "weave.log"),
		stubModels: []string{modelSeat, modelRuler, modelConsult},
	})
	if err != nil {
		return nil, err
	}
	defer w.Stop()
	res := &concurrencyResult{Concurrency: conc, StartupSeconds: time.Since(started).Seconds()}
	log.Printf("weave up on %s in %.1fs (log %s)", w.baseURL, res.StartupSeconds, filepath.Join(workDir, "weave.log"))

	p, err := provision(ctx, w, provisionOptions{providerID: "openai", seatModel: modelSeat, rulerModel: modelRuler, consultModel: modelConsult, maxActive: 1000})
	if err != nil {
		return res, fmt.Errorf("provision: %w", err)
	}
	var executors []*child
	defer func() {
		for _, executor := range executors {
			executor.Stop()
		}
	}()
	if o.executors < 0 || o.executors > 32 {
		return res, errors.New("executors must be between 0 and 32")
	}
	for index := 0; index < o.executors; index++ {
		logPath := filepath.Join(workDir, fmt.Sprintf("executor-%d.log", index))
		environment := append([]string(nil), w.environment...)
		environment = append(environment, "LOG_LEVEL=info", "WEAVE_TEAMRUN_WORKERS_DISABLED=false", "WEAVE_WORKSPACES_ROOT="+filepath.Join(workDir, fmt.Sprintf("executor-%d", index)))
		executor, err := startChild("executor", o.weaveBin, []string{"executor"}, environment, logPath)
		if err != nil {
			return res, err
		}
		executors = append(executors, executor)
		deadline := time.Now().Add(time.Minute)
		for {
			raw, _ := os.ReadFile(logPath)
			if strings.Contains(string(raw), "workflow executor ready") {
				break
			}
			if executor.Exited() || time.Now().After(deadline) {
				return res, fmt.Errorf("executor %d did not become ready", index)
			}
			if err := pause(ctx, 50*time.Millisecond); err != nil {
				return res, err
			}
		}
	}
	env := &scenarioEnv{o: o, pg: pg, stub: sc, weave: w, prov: p, conc: conc,
		caller: newAPIClient(w.baseURL, 2048)}
	watcher, err := newInvocationWatcher(ctx, pg, o.watchEvery)
	if err != nil {
		return res, err
	}
	defer watcher.Close()
	env.watcher = watcher
	for _, name := range o.scenarios {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		var err error
		switch name {
		case "idle":
			var r *idleResult
			r, err = runIdle(ctx, env)
			res.Idle = r
		case "consult":
			var r *latencyResult
			r, err = runConsult(ctx, env)
			res.Consult = r
		case "decide":
			for _, lanes := range o.lanes {
				var r *latencyResult
				r, err = runDecide(ctx, env, lanes)
				if r != nil {
					res.Decide = append(res.Decide, *r)
				}
				if err != nil {
					break
				}
			}
		case "cancel":
			var r *cancelResult
			r, err = runCancel(ctx, env)
			res.Cancel = r
		default:
			err = fmt.Errorf("unknown scenario %q", name)
		}
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", name, err))
			log.Printf("scenario %s: %v", name, err)
			if errors.Is(err, context.Canceled) {
				return res, err
			}
		}
		if w.child.Exited() {
			return res, fmt.Errorf("weave exited during %s (see log)", name)
		}
	}
	if len(res.Errors) > 0 {
		return res, errors.New(strings.Join(res.Errors, "; "))
	}
	return res, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, ','); i > 0 {
		return s[:i]
	}
	return s
}

func hostInfo() map[string]any {
	h, _ := os.Hostname()
	return map[string]any{"hostname": h, "goos": runtime.GOOS, "goarch": runtime.GOARCH, "cpus": runtime.NumCPU(), "go": runtime.Version()}
}

func writeJSONFile(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}
