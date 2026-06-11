package power

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeScaler struct {
	asleep     bool
	sleepCalls int32
	wakeCalls  int32
	wakeDelay  time.Duration
}

func (f *fakeScaler) IsAsleep(context.Context) (bool, error) { return f.asleep, nil }

func (f *fakeScaler) SleepAll(context.Context) error {
	atomic.AddInt32(&f.sleepCalls, 1)
	return nil
}

func (f *fakeScaler) WakeAll(context.Context) error {
	atomic.AddInt32(&f.wakeCalls, 1)
	if f.wakeDelay > 0 {
		time.Sleep(f.wakeDelay)
	}
	return nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestInitSeedsState(t *testing.T) {
	m := New(&fakeScaler{asleep: true}, time.Second, testLogger())
	if err := m.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.Asleep() {
		t.Fatal("expected asleep after Init from a sleeping cluster")
	}
}

func TestSleep(t *testing.T) {
	sc := &fakeScaler{asleep: false}
	m := New(sc, time.Second, testLogger())

	if err := m.Sleep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.Asleep() || atomic.LoadInt32(&sc.sleepCalls) != 1 {
		t.Fatalf("after Sleep: asleep=%v sleepCalls=%d", m.Asleep(), sc.sleepCalls)
	}

	// Second Sleep is a no-op (already asleep).
	if err := m.Sleep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&sc.sleepCalls) != 1 {
		t.Fatalf("sleepCalls = %d, want 1 (no-op when asleep)", sc.sleepCalls)
	}
}

func TestEnsureAwakeWhenAwakeIsNoOp(t *testing.T) {
	sc := &fakeScaler{asleep: false}
	m := New(sc, time.Second, testLogger())
	if err := m.EnsureAwake(context.Background()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&sc.wakeCalls) != 0 {
		t.Fatalf("wakeCalls = %d, want 0", sc.wakeCalls)
	}
}

func TestEnsureAwakeCoalescesConcurrentCallers(t *testing.T) {
	sc := &fakeScaler{asleep: true, wakeDelay: 50 * time.Millisecond}
	m := New(sc, time.Second, testLogger())
	if err := m.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	const callers = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			<-start
			_ = m.EnsureAwake(context.Background())
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&sc.wakeCalls); got != 1 {
		t.Fatalf("wakeCalls = %d, want 1 (concurrent wakes must coalesce)", got)
	}
	if m.Asleep() {
		t.Fatal("expected awake after EnsureAwake")
	}
}
