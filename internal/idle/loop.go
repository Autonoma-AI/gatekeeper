// Package idle scales managed namespaces to zero after a period without requests.
package idle

import (
	"context"
	"log/slog"
	"time"

	"github.com/autonoma-ai/gatekeeper/internal/registry"
)

// EnvLister returns the managed namespaces to consider on each tick
// (implemented by *registry.Registry).
type EnvLister interface {
	Envs() []*registry.Env
}

// Loop periodically scales namespaces to zero once they have been idle long enough.
type Loop struct {
	envs     EnvLister
	interval time.Duration
	log      *slog.Logger
}

// New builds an idle Loop.
func New(envs EnvLister, interval time.Duration, log *slog.Logger) *Loop {
	return &Loop{envs: envs, interval: interval, log: log}
}

// Run ticks until ctx is cancelled. On each tick, every managed namespace that
// is awake and has been idle longer than its idle timeout is scaled to zero. A
// namespace with a non-positive idle timeout is never auto-slept (requests
// still wake one that is already asleep).
func (l *Loop) Run(ctx context.Context) {
	l.log.Info("idle loop started", "interval", l.interval.String())
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
	for _, env := range l.envs.Envs() {
		if env.IdleTimeout <= 0 || env.Power.Asleep() {
			continue
		}
		idle := env.Activity.IdleFor()
		if idle < env.IdleTimeout {
			continue
		}
		l.log.Info("namespace idle past threshold; scaling to zero",
			"namespace", env.Namespace, "idleFor", idle.String())
		if err := env.Power.Sleep(ctx); err != nil {
			l.log.Error("failed to scale namespace to zero", "namespace", env.Namespace, "err", err)
		}
	}
}
