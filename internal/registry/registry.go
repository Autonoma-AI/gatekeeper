// Package registry holds the per-namespace units Gatekeeper manages. Each
// managed namespace gets an Env - its power manager, readiness waiter,
// activity tracker, and idle timeout - and the registry maps request
// hostnames to the Env (and upstream) that serves them. Rebuild reconciles
// the managed set against a desired routing table, preserving the Envs (and
// so the in-memory power state and idle timers) of namespaces still present.
package registry

import (
	"context"
	"sync"
	"time"

	"github.com/autonoma-ai/gatekeeper/internal/routing"
)

// Power manages a namespace's awake/asleep state (implemented by *power.Manager).
type Power interface {
	Init(ctx context.Context) error
	Asleep() bool
	EnsureAwake(ctx context.Context) error
	Sleep(ctx context.Context) error
}

// Readiness blocks until a namespace's managed workloads are ready
// (implemented by *scaler.Scaler).
type Readiness interface {
	WaitForReady(ctx context.Context) error
}

// Activity records request recency (implemented by *activity.Tracker).
type Activity interface {
	Touch()
	IdleFor() time.Duration
}

// Env is everything Gatekeeper holds for one managed namespace.
type Env struct {
	Namespace string
	Power     Power
	Readiness Readiness
	Activity  Activity
	// IdleTimeout is how long the namespace may be idle before being scaled
	// to zero; <= 0 means this namespace is never auto-slept.
	IdleTimeout time.Duration
}

// Factory builds the Env for a namespace that has become managed.
type Factory func(namespace string) *Env

// Registry maps hostnames to upstreams and namespaces to their Envs. Safe for
// concurrent use: Resolve serves the request hot path, Rebuild replaces the
// managed set (once at startup in static mode, on namespace events in
// discovery mode).
type Registry struct {
	factory Factory

	mu    sync.RWMutex
	envs  map[string]*Env
	table *routing.Table
}

// New builds an empty Registry; Rebuild populates it.
func New(factory Factory) *Registry {
	return &Registry{factory: factory, envs: map[string]*Env{}, table: routing.NewTable(nil)}
}

// Rebuild reconciles the registry against a full desired routing table: Envs
// for namespaces still routed to are kept as-is, new namespaces get an Env
// from the factory, and namespaces no longer routed to are dropped.
func (r *Registry) Rebuild(routes map[string]routing.Upstream) {
	r.mu.Lock()
	defer r.mu.Unlock()
	envs := make(map[string]*Env)
	for _, up := range routes {
		if _, ok := envs[up.Namespace]; ok {
			continue
		}
		if env, ok := r.envs[up.Namespace]; ok {
			envs[up.Namespace] = env
		} else {
			envs[up.Namespace] = r.factory(up.Namespace)
		}
	}
	r.envs = envs
	r.table = routing.NewTable(routes)
}

// Resolve maps a request's Host header to the upstream that serves it and the
// Env of the upstream's namespace. The last return value is false when no
// route matches the host.
func (r *Registry) Resolve(host string) (*Env, routing.Upstream, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	up, ok := r.table.Resolve(host)
	if !ok {
		return nil, routing.Upstream{}, false
	}
	env, ok := r.envs[up.Namespace]
	if !ok {
		// Unreachable: Rebuild derives the Envs from the same routes.
		return nil, routing.Upstream{}, false
	}
	return env, up, true
}

// Envs returns a snapshot of the managed Envs.
func (r *Registry) Envs() []*Env {
	r.mu.RLock()
	defer r.mu.RUnlock()
	envs := make([]*Env, 0, len(r.envs))
	for _, env := range r.envs {
		envs = append(envs, env)
	}
	return envs
}
