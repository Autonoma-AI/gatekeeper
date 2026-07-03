package idle

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/autonoma-ai/gatekeeper/internal/registry"
)

type fakePower struct {
	asleep     bool
	sleepCalls int
}

func (f *fakePower) Init(context.Context) error        { return nil }
func (f *fakePower) Asleep() bool                      { return f.asleep }
func (f *fakePower) EnsureAwake(context.Context) error { return nil }

func (f *fakePower) Sleep(context.Context) error {
	f.sleepCalls++
	f.asleep = true
	return nil
}

type fakeActivity struct{ idle time.Duration }

func (f *fakeActivity) Touch()                 {}
func (f *fakeActivity) IdleFor() time.Duration { return f.idle }

type staticEnvs []*registry.Env

func (s staticEnvs) Envs() []*registry.Env { return s }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func env(ns string, timeout time.Duration, asleep bool, idle time.Duration) (*registry.Env, *fakePower) {
	pw := &fakePower{asleep: asleep}
	return &registry.Env{
		Namespace:   ns,
		Power:       pw,
		Activity:    &fakeActivity{idle: idle},
		IdleTimeout: timeout,
	}, pw
}

func TestLoopTick(t *testing.T) {
	const timeout = 30 * time.Minute

	tests := []struct {
		name       string
		timeout    time.Duration
		asleep     bool
		idle       time.Duration
		wantSleeps int
		wantAsleep bool
	}{
		{"awake and idle past threshold scales down", timeout, false, 31 * time.Minute, 1, true},
		{"awake but still active stays up", timeout, false, 5 * time.Minute, 0, false},
		{"already asleep is a no-op", timeout, true, 99 * time.Minute, 0, true},
		{"zero idle timeout never sleeps", 0, false, 99 * time.Hour, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, pw := env("ns", tt.timeout, tt.asleep, tt.idle)
			loop := New(staticEnvs{e}, time.Second, nil, testLogger())

			loop.tick(context.Background())

			if pw.sleepCalls != tt.wantSleeps {
				t.Fatalf("sleepCalls = %d, want %d", pw.sleepCalls, tt.wantSleeps)
			}
			if pw.asleep != tt.wantAsleep {
				t.Fatalf("asleep = %v, want %v", pw.asleep, tt.wantAsleep)
			}
		})
	}
}

// Each namespace idles independently: a tick sleeps only the namespaces past
// their own threshold, never the ones still receiving traffic.
func TestTickSleepsOnlyIdleNamespaces(t *testing.T) {
	const timeout = 30 * time.Minute
	idleEnv, idlePw := env("idle-ns", timeout, false, 31*time.Minute)
	activeEnv, activePw := env("active-ns", timeout, false, 5*time.Minute)

	loop := New(staticEnvs{idleEnv, activeEnv}, time.Second, nil, testLogger())
	loop.tick(context.Background())

	if idlePw.sleepCalls != 1 {
		t.Fatalf("idle-ns sleepCalls = %d, want 1", idlePw.sleepCalls)
	}
	if activePw.sleepCalls != 0 {
		t.Fatalf("active-ns sleepCalls = %d, want 0", activePw.sleepCalls)
	}
}

// A non-leader must never sleep namespaces: it receives no traffic, so every
// namespace looks idle to it even while the leader is serving requests.
func TestTickGatedOnLeadership(t *testing.T) {
	e, pw := env("ns", 30*time.Minute, false, time.Hour)
	leading := false
	loop := New(staticEnvs{e}, time.Second, func() bool { return leading }, testLogger())

	loop.tick(context.Background())
	if pw.sleepCalls != 0 {
		t.Fatalf("standby slept a namespace: sleepCalls = %d, want 0", pw.sleepCalls)
	}

	leading = true
	loop.tick(context.Background())
	if pw.sleepCalls != 1 {
		t.Fatalf("leader sleepCalls = %d, want 1", pw.sleepCalls)
	}
}
