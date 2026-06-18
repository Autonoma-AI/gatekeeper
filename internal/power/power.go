// Package power tracks and transitions a namespace between awake and asleep,
// coordinating with a Scaler. The asleep flag is the in-memory source of truth
// on the request hot path (no Kubernetes call per request); it is initialised
// from the cluster at startup and flipped on each transition.
package power

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Scaler is the subset of *scaler.Scaler the Manager needs. Declared here (at
// the consumer) so the Manager is unit-testable with a fake.
type Scaler interface {
	IsAsleep(ctx context.Context) (bool, error)
	SleepAll(ctx context.Context) error
	WakeAll(ctx context.Context) error
}

// Manager owns the awake/asleep state and serialises transitions.
type Manager struct {
	scaler      Scaler
	wakeTimeout time.Duration
	log         *slog.Logger

	mu     sync.RWMutex
	asleep bool

	group singleflight.Group
}

// New builds a Manager. wakeTimeout bounds the detached WakeAll operation.
func New(scaler Scaler, wakeTimeout time.Duration, log *slog.Logger) *Manager {
	return &Manager{scaler: scaler, wakeTimeout: wakeTimeout, log: log}
}

// Init seeds the in-memory asleep flag from live cluster state.
func (m *Manager) Init(ctx context.Context) error {
	asleep, err := m.scaler.IsAsleep(ctx)
	if err != nil {
		return err
	}
	m.setAsleep(asleep)
	m.log.Info("initial power state", "asleep", asleep)
	return nil
}

// Asleep reports the current in-memory state.
func (m *Manager) Asleep() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.asleep
}

// Sleep scales the namespace to zero and marks it asleep. No-op if already asleep.
func (m *Manager) Sleep(ctx context.Context) error {
	if m.Asleep() {
		return nil
	}
	if err := m.scaler.SleepAll(ctx); err != nil {
		return err
	}
	m.setAsleep(true)
	m.log.Info("namespace asleep")
	return nil
}

// EnsureAwake wakes the namespace if asleep, coalescing concurrent callers into
// a single WakeAll via singleflight. WakeAll runs on a context detached from the
// caller's (a disconnecting client must not abort a wake the others are waiting
// on) but bounded by wakeTimeout. It returns once every workload has been scaled
// up in dependency order (each tier waited on before the next); callers then wait
// for the whole namespace to become ready.
func (m *Manager) EnsureAwake(ctx context.Context) error {
	if !m.Asleep() {
		return nil
	}
	_, err, _ := m.group.Do("wake", func() (any, error) {
		if !m.Asleep() {
			return nil, nil // a prior flight already woke us
		}
		opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.wakeTimeout)
		defer cancel()
		if err := m.scaler.WakeAll(opCtx); err != nil {
			return nil, err
		}
		m.setAsleep(false)
		m.log.Info("namespace awake")
		return nil, nil
	})
	return err
}

func (m *Manager) setAsleep(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asleep = v
}
