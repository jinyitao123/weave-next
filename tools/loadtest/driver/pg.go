package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const driverAppName = "weave-loadtest-driver"

// pgEnv owns the driver's connections: one to the maintenance database for
// cluster-wide views (pg_stat_database, pg_stat_activity) and short-lived ones
// to the test database for per-table counters.
type pgEnv struct {
	target pgTarget
	testDB string
	admin  *pgx.Conn
	mu     sync.Mutex
	// testQueries counts transactions the driver itself ran on the test
	// database, so they can be subtracted from xact_commit deltas.
	testQueries  int64
	ownsDatabase bool
	// statements is set when pg_stat_statements is loaded; snapshots then
	// also count statement calls on the test database by fingerprint.
	statements bool
}

func openPGEnv(ctx context.Context, target pgTarget, testDB string) (*pgEnv, error) {
	if forbiddenDatabases[strings.ToLower(testDB)] {
		return nil, fmt.Errorf("refusing test database %q", testDB)
	}
	adminURL, err := target.url("postgres", map[string]string{"application_name": driverAppName})
	if err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return nil, fmt.Errorf("connect maintenance database: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, "weave-loadtest:"+testDB).Scan(&locked); err != nil || !locked {
		_ = conn.Close(ctx)
		return nil, fmt.Errorf("test database already in use: %s", testDB)
	}
	return &pgEnv{target: target, testDB: testDB, admin: conn}, nil
}

func (p *pgEnv) Close() {
	if p.admin != nil {
		_ = p.admin.Close(context.Background())
	}
}

type serverInfo struct {
	Version           string `json:"version"`
	MaxConnections    int    `json:"max_connections"`
	SharedPreload     string `json:"shared_preload_libraries"`
	PGStatStatements  bool   `json:"pg_stat_statements_loaded"`
	TrackIOTiming     string `json:"track_io_timing"`
	ConnectionsInUse  int    `json:"connections_in_use"`
	StatsFetchConsist string `json:"stats_fetch_consistency"`
}

func (p *pgEnv) info(ctx context.Context) (serverInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var s serverInfo
	err := p.admin.QueryRow(ctx, `SELECT version(), current_setting('max_connections')::int,
		current_setting('shared_preload_libraries'), current_setting('track_io_timing'),
		(SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend')::int,
		current_setting('stats_fetch_consistency')`).Scan(
		&s.Version, &s.MaxConnections, &s.SharedPreload, &s.TrackIOTiming, &s.ConnectionsInUse, &s.StatsFetchConsist)
	s.PGStatStatements = strings.Contains(s.SharedPreload, "pg_stat_statements")
	p.statements = s.PGStatStatements
	return s, err
}

// recreate drops and creates the test database. It refuses to drop a
// database that has foreign connections.
func (p *pgEnv) recreate(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ident := pgx.Identifier{p.testDB}.Sanitize()
	var marker string
	err := p.admin.QueryRow(ctx, `SELECT COALESCE(shobj_description(oid,'pg_database'),'') FROM pg_database WHERE datname=$1`, p.testDB).Scan(&marker)
	if err != nil && err != pgx.ErrNoRows {
		return err
	}
	if err == nil && marker != "weave-loadtest-owned-v1" {
		return fmt.Errorf("refusing existing database without a load-test ownership marker: %s", p.testDB)
	}
	var others int
	if err := p.admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, p.testDB).Scan(&others); err != nil {
		return err
	}
	if others > 0 {
		var foreign int
		if err := p.admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid() AND COALESCE(application_name,'') NOT IN ('weave-loadtest','weave-loadtest-driver')`, p.testDB).Scan(&foreign); err != nil {
			return err
		}
		if foreign > 0 {
			return fmt.Errorf("test database has foreign connections: %s", p.testDB)
		}
		// Only our own previous Weave/driver sessions can be connected to a
		// loadtest database; terminate them.
		if _, err := p.admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, p.testDB); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
	if _, err := p.admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident); err != nil {
		return fmt.Errorf("drop %s: %w", p.testDB, err)
	}
	if _, err := p.admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		return fmt.Errorf("create %s: %w", p.testDB, err)
	}
	if _, err := p.admin.Exec(ctx, "COMMENT ON DATABASE "+ident+" IS 'weave-loadtest-owned-v1'"); err != nil {
		return err
	}
	p.ownsDatabase = true
	// The statement view is created in the disposable test database only, so
	// no other database gains an object.
	if p.statements {
		if err := p.withTestConn(ctx, func(conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
			return err
		}); err != nil {
			p.statements = false
		}
	}
	return nil
}

func (p *pgEnv) withTestConn(ctx context.Context, fn func(*pgx.Conn) error) error {
	url, err := p.target.url(p.testDB, map[string]string{"application_name": driverAppName})
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return fn(conn)
}

// ---- snapshots ----

type dbCounters struct {
	XactCommit   int64 `json:"xact_commit"`
	XactRollback int64 `json:"xact_rollback"`
	TupReturned  int64 `json:"tup_returned"`
	TupFetched   int64 `json:"tup_fetched"`
	TupInserted  int64 `json:"tup_inserted"`
	TupUpdated   int64 `json:"tup_updated"`
	TupDeleted   int64 `json:"tup_deleted"`
	BlksRead     int64 `json:"blks_read"`
	BlksHit      int64 `json:"blks_hit"`
}

type tableCounters struct {
	Ins, Upd, HotUpd, Del, Live, Dead, SeqScan, IdxScan int64
}

type snapshot struct {
	At          time.Time
	DB          dbCounters
	Tables      map[string]tableCounters
	TestQueries int64
	Statements  map[int64]statementCounter
}

type statementCounter struct {
	Query  string
	Calls  int64
	ExecMS float64
}

func (p *pgEnv) snapshot(ctx context.Context) (snapshot, error) {
	s := snapshot{At: time.Now(), Tables: map[string]tableCounters{}}
	// Table counters first (one transaction on the test database).
	err := p.withTestConn(ctx, func(conn *pgx.Conn) error {
		rows, err := conn.Query(ctx, `SELECT relname, n_tup_ins, n_tup_upd, n_tup_hot_upd, n_tup_del,
			n_live_tup, n_dead_tup, coalesce(seq_scan,0), coalesce(idx_scan,0) FROM pg_stat_user_tables`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var t tableCounters
			if err := rows.Scan(&name, &t.Ins, &t.Upd, &t.HotUpd, &t.Del, &t.Live, &t.Dead, &t.SeqScan, &t.IdxScan); err != nil {
				return err
			}
			s.Tables[name] = t
		}
		if err := rows.Err(); err != nil || !p.statements {
			return err
		}
		s.Statements, err = statementCounters(ctx, conn)
		return err
	})
	if err != nil {
		return s, err
	}
	p.mu.Lock()
	p.testQueries++
	if s.Statements != nil {
		p.testQueries++
	}
	s.TestQueries = p.testQueries
	err = p.admin.QueryRow(ctx, `SELECT xact_commit, xact_rollback, tup_returned, tup_fetched, tup_inserted,
		tup_updated, tup_deleted, blks_read, blks_hit FROM pg_stat_database WHERE datname=$1`, p.testDB).Scan(
		&s.DB.XactCommit, &s.DB.XactRollback, &s.DB.TupReturned, &s.DB.TupFetched, &s.DB.TupInserted,
		&s.DB.TupUpdated, &s.DB.TupDeleted, &s.DB.BlksRead, &s.DB.BlksHit)
	p.mu.Unlock()
	return s, err
}

// statementCounters reads pg_stat_statements for the test database only.
func statementCounters(ctx context.Context, conn *pgx.Conn) (map[int64]statementCounter, error) {
	rows, err := conn.Query(ctx, `SELECT queryid, max(query), sum(calls)::bigint, sum(total_exec_time)::float8 FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database()) AND queryid IS NOT NULL GROUP BY queryid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]statementCounter{}
	for rows.Next() {
		var id int64
		var c statementCounter
		if err := rows.Scan(&id, &c.Query, &c.Calls, &c.ExecMS); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// flushStats asks PostgreSQL to make pending per-backend counters visible.
// pg_stat_force_next_flush only affects the calling backend, so the driver
// instead waits slightly longer than the 1s stats flush interval.
func flushStatsWait() { time.Sleep(1200 * time.Millisecond) }

type tableRate struct {
	Table    string  `json:"table"`
	InsPerS  float64 `json:"ins_per_s"`
	UpdPerS  float64 `json:"upd_per_s"`
	HotPct   float64 `json:"hot_update_pct"`
	DelPerS  float64 `json:"del_per_s"`
	LiveRows int64   `json:"live_rows"`
	DeadRows int64   `json:"dead_rows"`
	SeqScanS float64 `json:"seq_scan_per_s"`
	IdxScanS float64 `json:"idx_scan_per_s"`
}

type dbRates struct {
	Seconds        float64     `json:"seconds"`
	XactPerS       float64     `json:"xact_per_s"`
	RollbackPerS   float64     `json:"rollback_per_s"`
	TupInsPerS     float64     `json:"tup_ins_per_s"`
	TupUpdPerS     float64     `json:"tup_upd_per_s"`
	TupDelPerS     float64     `json:"tup_del_per_s"`
	TupReturnedS   float64     `json:"tup_returned_per_s"`
	TupFetchedS    float64     `json:"tup_fetched_per_s"`
	WritesPerS     float64     `json:"row_writes_per_s"`
	DriverXacts    int64       `json:"driver_xacts_subtracted"`
	Tables         []tableRate `json:"tables"`
	TotalXacts     int64       `json:"xacts"`
	TotalRowWrites int64       `json:"row_writes"`
	Rollbacks      int64       `json:"rollbacks"`
	// Statements counts every statement call on the test database between the
	// snapshots and StatementMS their execution time, lock waits included;
	// TopStatements lists the most frequent fingerprints, SlowestStatements
	// those with the most execution time.
	Statements        int64           `json:"statements,omitempty"`
	StatementMS       float64         `json:"statement_ms,omitempty"`
	TopStatements     []statementRate `json:"top_statements,omitempty"`
	SlowestStatements []statementRate `json:"slowest_statements,omitempty"`
}

type statementRate struct {
	Calls  int64   `json:"calls"`
	ExecMS float64 `json:"exec_ms"`
	Query  string  `json:"query"`
}

func diffSnapshots(a, b snapshot) dbRates {
	sec := b.At.Sub(a.At).Seconds()
	if sec <= 0 {
		sec = 1
	}
	driver := b.TestQueries - a.TestQueries
	xacts := b.DB.XactCommit - a.DB.XactCommit + b.DB.XactRollback - a.DB.XactRollback - driver
	if xacts < 0 {
		xacts = 0
	}
	r := dbRates{Seconds: sec, DriverXacts: driver, TotalXacts: xacts,
		XactPerS:     float64(xacts) / sec,
		RollbackPerS: float64(b.DB.XactRollback-a.DB.XactRollback) / sec,
		TupInsPerS:   float64(b.DB.TupInserted-a.DB.TupInserted) / sec,
		TupUpdPerS:   float64(b.DB.TupUpdated-a.DB.TupUpdated) / sec,
		TupDelPerS:   float64(b.DB.TupDeleted-a.DB.TupDeleted) / sec,
		TupReturnedS: float64(b.DB.TupReturned-a.DB.TupReturned) / sec,
		TupFetchedS:  float64(b.DB.TupFetched-a.DB.TupFetched) / sec,
	}
	r.Rollbacks = b.DB.XactRollback - a.DB.XactRollback
	if b.Statements != nil {
		var all []statementRate
		for id, sb := range b.Statements {
			calls := sb.Calls - a.Statements[id].Calls
			if calls <= 0 {
				continue
			}
			execMS := sb.ExecMS - a.Statements[id].ExecMS
			r.Statements += calls
			r.StatementMS += execMS
			query := strings.Join(strings.Fields(sb.Query), " ")
			if len(query) > 200 {
				query = query[:200]
			}
			all = append(all, statementRate{Calls: calls, ExecMS: execMS, Query: query})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Calls > all[j].Calls })
		r.TopStatements = append([]statementRate(nil), all[:min(len(all), 40)]...)
		sort.Slice(all, func(i, j int) bool { return all[i].ExecMS > all[j].ExecMS })
		r.SlowestStatements = all[:min(len(all), 15)]
	}
	r.TotalRowWrites = (b.DB.TupInserted - a.DB.TupInserted) + (b.DB.TupUpdated - a.DB.TupUpdated) + (b.DB.TupDeleted - a.DB.TupDeleted)
	r.WritesPerS = float64(r.TotalRowWrites) / sec
	for name, tb := range b.Tables {
		ta := a.Tables[name]
		ins, upd, del := tb.Ins-ta.Ins, tb.Upd-ta.Upd, tb.Del-ta.Del
		seq, idx := tb.SeqScan-ta.SeqScan, tb.IdxScan-ta.IdxScan
		if ins == 0 && upd == 0 && del == 0 && seq == 0 && idx == 0 {
			continue
		}
		hot := 0.0
		if upd > 0 {
			hot = 100 * float64(tb.HotUpd-ta.HotUpd) / float64(upd)
		}
		r.Tables = append(r.Tables, tableRate{Table: name, InsPerS: float64(ins) / sec, UpdPerS: float64(upd) / sec,
			HotPct: hot, DelPerS: float64(del) / sec, LiveRows: tb.Live, DeadRows: tb.Dead,
			SeqScanS: float64(seq) / sec, IdxScanS: float64(idx) / sec})
	}
	sort.Slice(r.Tables, func(i, j int) bool {
		wi := r.Tables[i].InsPerS + r.Tables[i].UpdPerS + r.Tables[i].DelPerS
		wj := r.Tables[j].InsPerS + r.Tables[j].UpdPerS + r.Tables[j].DelPerS
		if wi != wj {
			return wi > wj
		}
		return r.Tables[i].SeqScanS+r.Tables[i].IdxScanS > r.Tables[j].SeqScanS+r.Tables[j].IdxScanS
	})
	return r
}

// ---- activity sampling ----

type activitySample struct {
	Total   int            `json:"total"`
	Active  int            `json:"active"`
	Idle    int            `json:"idle"`
	IdleTx  int            `json:"idle_in_tx"`
	Waits   map[string]int `json:"waits,omitempty"`
	Weave   int            `json:"weave_conns"`
	Sampled time.Time      `json:"-"`
}

func (p *pgEnv) sampleActivity(ctx context.Context) (activitySample, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := activitySample{Waits: map[string]int{}, Sampled: time.Now()}
	rows, err := p.admin.Query(ctx, `SELECT coalesce(state,''), coalesce(wait_event_type,''), coalesce(wait_event,''),
		coalesce(application_name,'') FROM pg_stat_activity WHERE datname=$1 AND backend_type='client backend'`, p.testDB)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var state, wtype, wevent, app string
		if err := rows.Scan(&state, &wtype, &wevent, &app); err != nil {
			return s, err
		}
		if app == driverAppName {
			continue
		}
		s.Total++
		s.Weave++
		switch state {
		case "active":
			s.Active++
			if wtype != "" {
				s.Waits[wtype+":"+wevent]++
			}
		case "idle":
			s.Idle++
		case "idle in transaction", "idle in transaction (aborted)":
			s.IdleTx++
		}
	}
	return s, rows.Err()
}

// activitySampler polls pg_stat_activity at a fixed period.
type activitySampler struct {
	stop    chan struct{}
	done    chan struct{}
	samples []activitySample
}

func (p *pgEnv) startActivity(ctx context.Context, every time.Duration) *activitySampler {
	a := &activitySampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(a.done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-a.stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if s, err := p.sampleActivity(ctx); err == nil {
					a.samples = append(a.samples, s)
				}
			}
		}
	}()
	return a
}

type activitySummary struct {
	Samples    int            `json:"samples"`
	ConnsMax   int            `json:"conns_max"`
	ConnsMean  float64        `json:"conns_mean"`
	ActiveMax  int            `json:"active_max"`
	ActiveMean float64        `json:"active_mean"`
	IdleTxMax  int            `json:"idle_in_tx_max"`
	WaitTop    map[string]int `json:"wait_samples,omitempty"`
}

func (a *activitySampler) Stop() activitySummary {
	close(a.stop)
	<-a.done
	sum := activitySummary{Samples: len(a.samples), WaitTop: map[string]int{}}
	for _, s := range a.samples {
		sum.ConnsMax = max(sum.ConnsMax, s.Total)
		sum.ActiveMax = max(sum.ActiveMax, s.Active)
		sum.IdleTxMax = max(sum.IdleTxMax, s.IdleTx)
		sum.ConnsMean += float64(s.Total)
		sum.ActiveMean += float64(s.Active)
		for k, v := range s.Waits {
			sum.WaitTop[k] += v
		}
	}
	if n := float64(len(a.samples)); n > 0 {
		sum.ConnsMean /= n
		sum.ActiveMean /= n
	}
	return sum
}
