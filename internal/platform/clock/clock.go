// Package clock provides the time source injected into domain services and
// workers, so tests can control time.
package clock

import (
	"sync"
	"time"
)

// Clock returns the current time.
type Clock interface {
	Now() time.Time
}

// System is the wall clock, always in UTC.
type System struct{}

func (System) Now() time.Time { return time.Now().UTC() }

// Fake is a manually advanced clock for tests. It is safe for concurrent use.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a Fake set to t.
func NewFake(t time.Time) *Fake { return &Fake{now: t.UTC()} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

// Set moves the clock to t.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	f.now = t.UTC()
	f.mu.Unlock()
}
