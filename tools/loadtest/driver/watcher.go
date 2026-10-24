package main

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

type invocationTiming struct{ created, terminal time.Time }
type invocationWait struct {
	ready  chan struct{}
	timing invocationTiming
}
type invocationWatcher struct {
	pg      *pgEnv
	conn    *pgx.Conn
	mu      sync.Mutex
	waiting map[string]*invocationWait
	wake    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	every   time.Duration
}

// The driver observes all in-flight calls in one database read per tick. Every
// terminal result is then independently read through the real invocation API;
// client polling must not become the execution plane's dominant workload.
func newInvocationWatcher(ctx context.Context, pg *pgEnv, every time.Duration) (*invocationWatcher, error) {
	url, err := pg.target.url(pg.testDB, map[string]string{"application_name": driverAppName})
	if err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	w := &invocationWatcher{pg: pg, conn: conn, waiting: make(map[string]*invocationWait), wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{}), every: max(5*time.Millisecond, every)}
	go w.run()
	return w, nil
}
func (w *invocationWatcher) Close() { w.cancel(); <-w.done; _ = w.conn.Close(context.Background()) }
func (w *invocationWatcher) wait(ctx context.Context, id string) (invocationTiming, error) {
	entry := &invocationWait{ready: make(chan struct{})}
	w.mu.Lock()
	w.waiting[id] = entry
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
	select {
	case <-entry.ready:
		return entry.timing, nil
	case <-ctx.Done():
		w.mu.Lock()
		delete(w.waiting, id)
		w.mu.Unlock()
		return invocationTiming{}, ctx.Err()
	case <-w.ctx.Done():
		return invocationTiming{}, w.ctx.Err()
	}
}
func (w *invocationWatcher) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.every)
	defer ticker.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
		case <-w.wake:
		}
		w.mu.Lock()
		ids := make([]string, 0, len(w.waiting))
		for id := range w.waiting {
			ids = append(ids, id)
		}
		w.mu.Unlock()
		if len(ids) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
		rows, err := w.conn.Query(ctx, driverQueryMarker+`SELECT i.invocation_id,i.created_at,r.terminal_at
		 FROM weave_capability_workflow_invocations i JOIN weave_team_runs r ON r.workspace_id=i.workspace_id AND r.run_id=i.run_id
		 WHERE i.invocation_id=ANY($1::text[]) AND r.status IN ('succeeded','failed','cancelled','abandoned')`, ids)
		w.pg.mu.Lock()
		w.pg.testQueries++
		w.pg.mu.Unlock()
		if err != nil {
			cancel()
			continue
		}
		for rows.Next() {
			var id string
			var timing invocationTiming
			if rows.Scan(&id, &timing.created, &timing.terminal) != nil {
				continue
			}
			w.mu.Lock()
			if entry := w.waiting[id]; entry != nil {
				entry.timing = timing
				delete(w.waiting, id)
				close(entry.ready)
			}
			w.mu.Unlock()
		}
		rows.Close()
		cancel()
	}
}
