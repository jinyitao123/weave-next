// Package fanout persists the parallel legs of a workflow team run.
package fanout

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Clock supplies timestamps for fan-out state transitions.
type Clock interface {
	Now() time.Time
}

// RealClock is the production wall clock.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// Store persists workflow fan-out groups. The free-collaboration task groups
// it once also held were retired with the chat chain; their table stays as
// history only.
type Store struct {
	pool  *pgxpool.Pool
	clock Clock
}

// New creates a fan-out store.
func New(pool *pgxpool.Pool, clock Clock) *Store {
	if clock == nil {
		clock = RealClock{}
	}
	return &Store{pool: pool, clock: clock}
}

type rowScanner interface {
	Scan(dest ...any) error
}
