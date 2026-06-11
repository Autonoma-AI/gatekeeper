// Package activity tracks the time of the most recent proxied request so the
// idle loop can decide when to scale the namespace to zero.
package activity

import (
	"sync"
	"time"
)

// Tracker records the last request time. Safe for concurrent use.
type Tracker struct {
	mu   sync.Mutex
	last time.Time
	now  func() time.Time // overridable in tests
}

// NewTracker returns a Tracker seeded with the current time, so a freshly
// started Gatekeeper does not immediately consider the namespace idle.
func NewTracker() *Tracker {
	return &Tracker{last: time.Now(), now: time.Now}
}

// Touch marks "now" as the most recent access.
func (t *Tracker) Touch() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = t.now()
}

// IdleFor returns how long it has been since the last Touch.
func (t *Tracker) IdleFor() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.now().Sub(t.last)
}
