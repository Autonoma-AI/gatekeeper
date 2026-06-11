package idle

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeSleeper struct {
	asleep     bool
	sleepCalls int
}

func (f *fakeSleeper) Asleep() bool { return f.asleep }

func (f *fakeSleeper) Sleep(context.Context) error {
	f.sleepCalls++
	f.asleep = true
	return nil
}

type fakeReporter struct{ idle time.Duration }

func (f *fakeReporter) IdleFor() time.Duration { return f.idle }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLoopTick(t *testing.T) {
	const timeout = 30 * time.Minute

	tests := []struct {
		name       string
		asleep     bool
		idle       time.Duration
		wantSleeps int
		wantAsleep bool
	}{
		{"awake and idle past threshold scales down", false, 31 * time.Minute, 1, true},
		{"awake but still active stays up", false, 5 * time.Minute, 0, false},
		{"already asleep is a no-op", true, 99 * time.Minute, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sleeper := &fakeSleeper{asleep: tt.asleep}
			reporter := &fakeReporter{idle: tt.idle}
			loop := New(reporter, sleeper, timeout, time.Second, testLogger())

			loop.tick(context.Background())

			if sleeper.sleepCalls != tt.wantSleeps {
				t.Fatalf("sleepCalls = %d, want %d", sleeper.sleepCalls, tt.wantSleeps)
			}
			if sleeper.asleep != tt.wantAsleep {
				t.Fatalf("asleep = %v, want %v", sleeper.asleep, tt.wantAsleep)
			}
		})
	}
}
