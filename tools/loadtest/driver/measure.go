package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// driverQueryMarker leads every statement the driver runs on the test
// database, so statement counts can leave the driver's own reads out.
const driverQueryMarker = "/* weave-loadtest-driver */ "

// ---- lock waits ----

// lockSampler reads pg_stat_activity joined with the ungranted pg_locks of
// each waiting Weave backend at a fixed period. It runs on a connection to the
// test database so relation names resolve.
type lockSampler struct {
	pg      *pgEnv
	conn    *pgx.Conn
	stop    chan struct{}
	done    chan struct{}
	every   time.Duration
	samples int
	waits   map[string]int
	active  []int
}

type lockWait struct {
	// Wait is wait_event_type:wait_event; Object names the lock that is
	// waited for (relation, tuple of a relation, transaction id, the commit
	// lock every notifying transaction takes, advisory) or is empty for
	// waits that are not heavyweight locks (WAL writes, LWLocks, client IO).
	Wait    string  `json:"wait"`
	Object  string  `json:"object,omitempty"`
	Samples int     `json:"samples"`
	Seconds float64 `json:"approx_backend_seconds"`
}

type lockSummary struct {
	Every      string     `json:"every"`
	Samples    int        `json:"samples"`
	ActiveMean float64    `json:"active_mean"`
	ActiveMax  int        `json:"active_max"`
	WaitTotal  int        `json:"wait_samples"`
	Waits      []lockWait `json:"waits,omitempty"`
}

func (p *pgEnv) startLocks(ctx context.Context, every time.Duration) (*lockSampler, error) {
	url, err := p.target.url(p.testDB, map[string]string{"application_name": driverAppName})
	if err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, err
	}
	s := &lockSampler{pg: p, conn: conn, stop: make(chan struct{}), done: make(chan struct{}), every: every, waits: map[string]int{}}
	go s.run(ctx)
	return s, nil
}

func (s *lockSampler) run(ctx context.Context) {
	defer close(s.done)
	t := time.NewTicker(s.every)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			s.sample(ctx)
		}
	}
}

func (s *lockSampler) sample(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// A backend waiting for another transaction's row lock waits on that
	// transaction id while holding the tuple lock of the row; the tuple lock
	// names the table.
	rows, err := s.conn.Query(ctx, driverQueryMarker+`SELECT a.pid, coalesce(a.wait_event_type,''), coalesce(a.wait_event,''),
		coalesce(l.locktype,''), coalesce(c.relname, (SELECT tc.relname FROM pg_locks t JOIN pg_class tc ON tc.oid=t.relation
		  WHERE t.pid=a.pid AND t.locktype='tuple' AND t.granted LIMIT 1), ''),
		coalesce(l.classid::int,0), coalesce(l.objid::bigint,0), coalesce(l.database::bigint,-1)
		FROM pg_stat_activity a LEFT JOIN pg_locks l ON l.pid=a.pid AND NOT l.granted
		LEFT JOIN pg_class c ON c.oid=l.relation
		WHERE a.datname=current_database() AND a.backend_type='client backend' AND a.state='active'
		AND coalesce(a.application_name,'')<>$1 AND a.pid<>pg_backend_pid()`, driverAppName)
	s.pg.mu.Lock()
	s.pg.testQueries++
	s.pg.mu.Unlock()
	if err != nil {
		return
	}
	defer rows.Close()
	pids := map[int32]bool{}
	waits := map[string]int{}
	for rows.Next() {
		var pid int32
		var wtype, wevent, locktype, relname string
		var classid int32
		var objid, database int64
		if rows.Scan(&pid, &wtype, &wevent, &locktype, &relname, &classid, &objid, &database) != nil {
			return
		}
		pids[pid] = true
		if wtype == "" {
			continue
		}
		waits[wtype+":"+wevent+"\x00"+lockObject(locktype, relname, classid, objid, database)]++
	}
	if rows.Err() != nil {
		return
	}
	s.samples++
	s.active = append(s.active, len(pids))
	for key, n := range waits {
		s.waits[key] += n
	}
}

// lockObject names a waited-for lock. PostgreSQL serializes the commits of
// all transactions that sent a NOTIFY on one lock on object 0 of pg_database
// (class 1262) in database 0.
func lockObject(locktype, relname string, classid int32, objid, database int64) string {
	switch locktype {
	case "":
		return ""
	case "relation", "tuple", "page", "extend":
		if relname == "" {
			relname = "?"
		}
		return locktype + ":" + relname
	case "object":
		if classid == 1262 && objid == 0 && database == 0 {
			return "notify commit (pg_database 0)"
		}
		return fmt.Sprintf("object:%d/%d", classid, objid)
	case "transactionid":
		if relname != "" {
			return "transactionid (row of " + relname + ")"
		}
		return locktype
	default:
		return locktype
	}
}

func (s *lockSampler) Stop() lockSummary {
	close(s.stop)
	<-s.done
	_ = s.conn.Close(context.Background())
	sum := lockSummary{Every: s.every.String(), Samples: s.samples}
	for _, n := range s.active {
		sum.ActiveMax = max(sum.ActiveMax, n)
		sum.ActiveMean += float64(n)
	}
	if s.samples > 0 {
		sum.ActiveMean /= float64(s.samples)
	}
	for key, n := range s.waits {
		wait, object, _ := strings.Cut(key, "\x00")
		sum.WaitTotal += n
		sum.Waits = append(sum.Waits, lockWait{Wait: wait, Object: object, Samples: n, Seconds: float64(n) * s.every.Seconds()})
	}
	sort.Slice(sum.Waits, func(i, j int) bool { return sum.Waits[i].Samples > sum.Waits[j].Samples })
	return sum
}

// ---- notifications ----

// notifyCounter listens on the task queue channels. PostgreSQL delivers one
// notification per channel and payload per committed transaction, so the
// count is the number of commits that sent one (a commit that sends both a
// ready and a cancel notification counts twice; none of the measured flows do).
type notifyCounter struct {
	conn     *pgx.Conn
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	channels map[string]int64
}

var notifyChannels = []string{"weave_task_ready", "weave_task_cancel"}

func (p *pgEnv) startNotify(ctx context.Context) (*notifyCounter, error) {
	url, err := p.target.url(p.testDB, map[string]string{"application_name": driverAppName})
	if err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, err
	}
	for _, channel := range notifyChannels {
		if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
			_ = conn.Close(context.Background())
			return nil, err
		}
		p.mu.Lock()
		p.testQueries++
		p.mu.Unlock()
	}
	ctx, cancel := context.WithCancel(ctx)
	n := &notifyCounter{conn: conn, cancel: cancel, done: make(chan struct{}), channels: map[string]int64{}}
	go func() {
		defer close(n.done)
		for {
			notification, err := conn.WaitForNotification(ctx)
			if err != nil {
				return
			}
			n.mu.Lock()
			n.channels[notification.Channel]++
			n.mu.Unlock()
		}
	}()
	return n, nil
}

func (n *notifyCounter) counts() map[string]int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make(map[string]int64, len(n.channels))
	for k, v := range n.channels {
		out[k] = v
	}
	return out
}

func (n *notifyCounter) Close() {
	n.cancel()
	<-n.done
	_ = n.conn.Close(context.Background())
}

type notifySummary struct {
	Channels map[string]int64 `json:"channels"`
	// Commits is the number of committed transactions that sent a
	// notification; Share divides it by all Weave commits in the span.
	Commits int64   `json:"notifying_commits"`
	Share   float64 `json:"share_of_commits"`
	// CommitLockSamples are lock-sampler hits on the commit lock notifying
	// transactions serialize on, and the backend seconds they approximate.
	CommitLockSamples int     `json:"commit_lock_wait_samples"`
	CommitLockSeconds float64 `json:"commit_lock_wait_seconds"`
}

func diffNotify(before, after map[string]int64, rates dbRates, locks lockSummary) notifySummary {
	sum := notifySummary{Channels: map[string]int64{}}
	for _, channel := range notifyChannels {
		if d := after[channel] - before[channel]; d > 0 {
			sum.Channels[channel] = d
			sum.Commits += d
		}
	}
	if commits := rates.TotalXacts - rates.Rollbacks; commits > 0 {
		sum.Share = float64(sum.Commits) / float64(commits)
	}
	for _, wait := range locks.Waits {
		if strings.HasPrefix(wait.Object, "notify commit") {
			sum.CommitLockSamples += wait.Samples
			sum.CommitLockSeconds += wait.Seconds
		}
	}
	return sum
}

// ---- connection pool ----

type poolStats struct {
	MaxConns             int64   `json:"max_conns"`
	TotalConns           int64   `json:"total_conns"`
	AcquiredConns        int64   `json:"acquired_conns"`
	AcquireCount         int64   `json:"acquire_count"`
	EmptyAcquireCount    int64   `json:"empty_acquire_count"`
	CanceledAcquireCount int64   `json:"canceled_acquire_count"`
	AcquireDurationMS    float64 `json:"acquire_duration_ms"`
	EmptyAcquireWaitMS   float64 `json:"empty_acquire_wait_ms"`
}

// poolDelta is the API process's pool use between two reads. Separate
// executor processes (-executors) keep their own pools, which it does not see.
type poolDelta struct {
	MaxConns          int64   `json:"max_conns"`
	Acquires          int64   `json:"acquires"`
	EmptyAcquires     int64   `json:"empty_acquires"`
	EmptyShare        float64 `json:"empty_acquire_share"`
	CanceledAcquires  int64   `json:"canceled_acquires"`
	AcquireMS         float64 `json:"acquire_ms"`
	AcquireMeanMS     float64 `json:"acquire_mean_ms"`
	EmptyAcquireWaitS float64 `json:"empty_acquire_wait_seconds"`
	Err               string  `json:"error,omitempty"`
}

func (e *scenarioEnv) poolStats(ctx context.Context) (poolStats, error) {
	var body struct {
		Pool *poolStats `json:"database_pool"`
	}
	if e.console == nil {
		return poolStats{}, fmt.Errorf("no console session")
	}
	if _, err := e.console.do(ctx, http.MethodGet, "/v1/admin/metrics", nil, &body); err != nil {
		return poolStats{}, err
	}
	if body.Pool == nil {
		return poolStats{}, fmt.Errorf("binary reports no database_pool metrics")
	}
	return *body.Pool, nil
}

func diffPool(a, b poolStats, errs ...error) poolDelta {
	for _, err := range errs {
		if err != nil {
			return poolDelta{Err: err.Error()}
		}
	}
	d := poolDelta{MaxConns: b.MaxConns, Acquires: b.AcquireCount - a.AcquireCount, EmptyAcquires: b.EmptyAcquireCount - a.EmptyAcquireCount,
		CanceledAcquires: b.CanceledAcquireCount - a.CanceledAcquireCount, AcquireMS: b.AcquireDurationMS - a.AcquireDurationMS,
		EmptyAcquireWaitS: (b.EmptyAcquireWaitMS - a.EmptyAcquireWaitMS) / 1000}
	if d.Acquires > 0 {
		d.EmptyShare = float64(d.EmptyAcquires) / float64(d.Acquires)
		d.AcquireMeanMS = d.AcquireMS / float64(d.Acquires)
	}
	return d
}

// ---- executor queueing ----

// queueWaits returns, per run, the time its tasks waited between becoming
// available and their last claim. Tasks that share a parent (fanout legs)
// wait side by side, so a group counts with its longest wait; tasks without a
// parent follow one another and add up.
func (p *pgEnv) queueWaits(ctx context.Context, runIDs []string) (map[string]float64, []float64, error) {
	out := map[string]float64{}
	var tasks []float64
	if len(runIDs) == 0 {
		return out, tasks, nil
	}
	err := p.withTestConn(ctx, func(conn *pgx.Conn) error {
		rows, err := conn.Query(ctx, driverQueryMarker+`SELECT r.run_id, coalesce(q.parent_task_id,''),
			EXTRACT(EPOCH FROM (q.started_at - q.available_at))*1000
			FROM weave_team_runs r JOIN weave_task_queue q ON q.workspace_id=r.workspace_id AND q.run_snapshot_id=r.run_snapshot_id
			WHERE r.run_id=ANY($1::text[]) AND q.started_at IS NOT NULL`, runIDs)
		if err != nil {
			return err
		}
		defer rows.Close()
		groups := map[[2]string]float64{}
		for rows.Next() {
			var runID, parent string
			var waitMS float64
			if err := rows.Scan(&runID, &parent, &waitMS); err != nil {
				return err
			}
			waitMS = max(0, waitMS)
			tasks = append(tasks, waitMS)
			if parent == "" {
				out[runID] += waitMS
				continue
			}
			key := [2]string{runID, parent}
			groups[key] = max(groups[key], waitMS)
		}
		for key, waitMS := range groups {
			out[key[0]] += waitMS
		}
		return rows.Err()
	})
	p.mu.Lock()
	p.testQueries++
	p.mu.Unlock()
	return out, tasks, err
}

// backlog counts workflow tasks that are queued and not yet claimable or
// claimed: the executor's backlog at that moment.
func (p *pgEnv) backlog(ctx context.Context) (queued, running int, err error) {
	err = p.withTestConn(ctx, func(conn *pgx.Conn) error {
		return conn.QueryRow(ctx, driverQueryMarker+`SELECT count(*) FILTER (WHERE status='queued'),
			count(*) FILTER (WHERE status IN ('dispatched','running','cancel_requested'))
			FROM weave_task_queue WHERE kind='team_workflow'`).Scan(&queued, &running)
	})
	p.mu.Lock()
	p.testQueries++
	p.mu.Unlock()
	return queued, running, err
}
