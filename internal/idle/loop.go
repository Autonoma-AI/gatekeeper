// Package idle scales the namespace to zero after a period without requests.
package idle

import (
	"context"
	"log/slog"
	"time"
)

// Sleeper is the power-management surface the loop needs.
type Sleeper interface {
	Asleep() bool
	Sleep(ctx context.Context) error
}

// ActivityReporter reports how long it has been since the last request.
type ActivityReporter interface {
	IdleFor() time.Duration
}

// Loop periodically scales the namespace to zero once it has been idle long enough.
type Loop struct {
	tracker     ActivityReporter
	sleeper     Sleeper
	idleTimeout time.Duration
	interval    time.Duration
	log         *slog.Logger
}

// New builds an idle Loop.
func New(tracker ActivityReporter, sleeper Sleeper, idleTimeout, interval time.Duration, log *slog.Logger) *Loop {
	return &Loop{
		tracker:     tracker,
		sleeper:     sleeper,
		idleTimeout: idleTimeout,
		interval:    interval,
		log:         log,
	}
}

// Run ticks until ctx is cancelled. On each tick, if the namespace is awake and
// has been idle longer than idleTimeout, it scales the namespace to zero. A
// non-positive idleTimeout disables scale-to-zero: Run logs and returns without
// starting the ticker, so the namespace is never auto-slept (requests still wake
// a namespace that is already asleep).
func (l *Loop) Run(ctx context.Context) {
	if l.idleTimeout <= 0 {
		l.log.Info("scale-to-zero disabled (idle timeout <= 0); idle loop not started")
		return
	}
	l.log.Info("idle loop started", "idleTimeout", l.idleTimeout.String(), "interval", l.interval.String())
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			l.log.Info("idle loop stopped")
			return
		case <-ticker.C:
			l.tick(ctx)
		}
	}
}

func (l *Loop) tick(ctx context.Context) {
	if l.sleeper.Asleep() {
		return
	}
	idle := l.tracker.IdleFor()
	if idle < l.idleTimeout {
		return
	}
	l.log.Info("namespace idle past threshold; scaling to zero", "idleFor", idle.String())
	if err := l.sleeper.Sleep(ctx); err != nil {
		l.log.Error("failed to scale namespace to zero", "err", err)
	}
}
