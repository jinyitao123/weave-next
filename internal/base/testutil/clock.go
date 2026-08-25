package testutil

import (
	"sync"
	"time"
)

// Clock provides the current time.
type Clock interface {
	Now() time.Time
}

// FakeClock is a manually controlled clock for tests.
type FakeClock struct {
	mu  sync.RWMutex
	now time.Time
}

// NewFakeClock creates a fake clock at the given time.
func NewFakeClock(now time.Time) *FakeClock {
	return &FakeClock{now: now}
}

// Now returns the fake clock's current time.
func (c *FakeClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

// Advance moves the fake clock forward by d.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
