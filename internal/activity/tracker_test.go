package activity

import (
	"testing"
	"time"
)

func TestTrackerIdleFor(t *testing.T) {
	base := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	now := base
	tr := NewTracker()
	tr.now = func() time.Time { return now }

	tr.Touch() // last = base

	now = base.Add(5 * time.Minute)
	if got := tr.IdleFor(); got != 5*time.Minute {
		t.Fatalf("IdleFor() = %v, want 5m", got)
	}

	// A fresh Touch resets the idle clock.
	tr.Touch()
	if got := tr.IdleFor(); got != 0 {
		t.Fatalf("IdleFor() after Touch = %v, want 0", got)
	}
}
