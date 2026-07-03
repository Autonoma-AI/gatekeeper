// Package registry holds the per-namespace units Gatekeeper manages. Each
// managed namespace gets an Env - its power manager, readiness waiter,
// activity tracker, and idle timeout - and the registry maps request
// hostnames to the Env (and upstream) that serves them. Rebuild reconciles
// the managed set against a desired routing table, preserving the Envs (and
// so the in-memory power state and idle timers) of namespaces still present.
package registry

import (
	"context"
	"sort"
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
// from the factory, and namespaces no longer routed to are dropped. A non-nil
// idleTimeouts gives each namespace's effective idle timeout; an Env whose
// timeout changed is shallow-copied (components and their state are shared)
// because published Envs are read lock-free and must stay immutable. It
// returns the Envs newly created by this rebuild so the caller can seed them.
func (r *Registry) Rebuild(routes map[string]routing.Upstream, idleTimeouts map[string]time.Duration) []*Env {
	r.mu.Lock()
	defer r.mu.Unlock()
	var added []*Env
	envs := make(map[string]*Env)
	for _, up := range routes {
		if _, ok := envs[up.Namespace]; ok {
			continue
		}
		env, existed := r.envs[up.Namespace]
		if !existed {
			env = r.factory(up.Namespace)
		}
		if want, ok := idleTimeouts[up.Namespace]; ok && env.IdleTimeout != want {
			clone := *env
			clone.IdleTimeout = want
			env = &clone
		}
		if !existed {
			added = append(added, env)
		}
		envs[up.Namespace] = env
	}
	r.envs = envs
	r.table = routing.NewTable(routes)
	return added
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

// RouteStatus is one host's routing entry plus its namespace's live state -
// the debug view served at /_gatekeeper/routes, since in discovery mode there
// is no single config object left to eyeball.
type RouteStatus struct {
	Host        string `json:"host"`
	Namespace   string `json:"namespace"`
	Service     string `json:"service"`
	Port        int    `json:"port"`
	Asleep      bool   `json:"asleep"`
	IdleTimeout string `json:"idleTimeout"`
}

// Status reports the live routing table, sorted by host.
func (r *Registry) Status() []RouteStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	hosts := r.table.Hosts()
	statuses := make([]RouteStatus, 0, len(hosts))
	for host, up := range hosts {
		env := r.envs[up.Namespace]
		if env == nil {
			continue
		}
		statuses = append(statuses, RouteStatus{
			Host:        host,
			Namespace:   up.Namespace,
			Service:     up.Service,
			Port:        up.Port,
			Asleep:      env.Power.Asleep(),
			IdleTimeout: env.IdleTimeout.String(),
		})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Host < statuses[j].Host })
	return statuses
}
