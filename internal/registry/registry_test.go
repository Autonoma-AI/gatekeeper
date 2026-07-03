package registry

import (
	"testing"
	"time"

	"github.com/autonoma-ai/gatekeeper/internal/routing"
)

// countingFactory returns bare Envs (the registry never calls into an Env's
// components) and records how many times each namespace was built.
func countingFactory(built map[string]int) Factory {
	return func(ns string) *Env {
		built[ns]++
		return &Env{Namespace: ns}
	}
}

func routes(hostToNS map[string]string) map[string]routing.Upstream {
	m := make(map[string]routing.Upstream, len(hostToNS))
	for host, ns := range hostToNS {
		m[host] = routing.Upstream{Namespace: ns, Service: "web", Port: 3000}
	}
	return m
}

func TestRebuildCreatesOneEnvPerNamespace(t *testing.T) {
	built := map[string]int{}
	r := New(countingFactory(built))
	r.Rebuild(routes(map[string]string{
		"a.test":     "ns-a",
		"a-api.test": "ns-a",
		"b.test":     "ns-b",
	}), nil)

	if got := len(r.Envs()); got != 2 {
		t.Fatalf("Envs() len = %d, want 2", got)
	}
	if built["ns-a"] != 1 || built["ns-b"] != 1 {
		t.Fatalf("factory calls = %v, want one per namespace", built)
	}

	env, up, ok := r.Resolve("A.test") // mixed case: table normalizes
	if !ok || env.Namespace != "ns-a" || up.Namespace != "ns-a" {
		t.Fatalf("Resolve(a.test) = %+v, %+v, %v", env, up, ok)
	}
}

func TestRebuildPreservesExistingEnvs(t *testing.T) {
	built := map[string]int{}
	r := New(countingFactory(built))
	r.Rebuild(routes(map[string]string{"a.test": "ns-a"}), nil)

	before, _, ok := r.Resolve("a.test")
	if !ok {
		t.Fatal("Resolve(a.test) not found after first Rebuild")
	}

	// A rebuild that keeps ns-a (new host) and adds ns-b must not rebuild ns-a's
	// Env: its power state and idle timer live there.
	r.Rebuild(routes(map[string]string{"a2.test": "ns-a", "b.test": "ns-b"}), nil)

	after, _, ok := r.Resolve("a2.test")
	if !ok {
		t.Fatal("Resolve(a2.test) not found after second Rebuild")
	}
	if after != before {
		t.Fatal("ns-a Env was rebuilt; existing Envs must be preserved")
	}
	if built["ns-a"] != 1 {
		t.Fatalf("ns-a factory calls = %d, want 1", built["ns-a"])
	}
}

func TestRebuildDropsRemovedNamespaces(t *testing.T) {
	r := New(countingFactory(map[string]int{}))
	r.Rebuild(routes(map[string]string{"a.test": "ns-a", "b.test": "ns-b"}), nil)
	r.Rebuild(routes(map[string]string{"b.test": "ns-b"}), nil)

	if _, _, ok := r.Resolve("a.test"); ok {
		t.Fatal("a.test still resolves after its namespace was removed")
	}
	if got := len(r.Envs()); got != 1 {
		t.Fatalf("Envs() len = %d, want 1", got)
	}
}

func TestRebuildReportsAddedEnvs(t *testing.T) {
	r := New(countingFactory(map[string]int{}))
	added := r.Rebuild(routes(map[string]string{"a.test": "ns-a"}), nil)
	if len(added) != 1 || added[0].Namespace != "ns-a" {
		t.Fatalf("first rebuild added = %+v, want [ns-a]", added)
	}

	added = r.Rebuild(routes(map[string]string{"a.test": "ns-a", "b.test": "ns-b"}), nil)
	if len(added) != 1 || added[0].Namespace != "ns-b" {
		t.Fatalf("second rebuild added = %+v, want only [ns-b]", added)
	}
}

// A changed idle timeout must not rebuild the Env's components: power state
// and idle tracking live there and have to survive an annotation edit.
func TestRebuildIdleTimeoutChangePreservesComponents(t *testing.T) {
	built := map[string]int{}
	r := New(func(ns string) *Env {
		built[ns]++
		return &Env{Namespace: ns, IdleTimeout: 30 * time.Minute}
	})
	rts := routes(map[string]string{"a.test": "ns-a"})

	r.Rebuild(rts, map[string]time.Duration{"ns-a": 30 * time.Minute})
	before, _, _ := r.Resolve("a.test")

	r.Rebuild(rts, map[string]time.Duration{"ns-a": 5 * time.Minute})
	after, _, _ := r.Resolve("a.test")

	if after.IdleTimeout != 5*time.Minute {
		t.Fatalf("IdleTimeout = %v, want overridden 5m", after.IdleTimeout)
	}
	if after == before {
		t.Fatal("Env must be copied on timeout change: published Envs are read lock-free")
	}
	if before.IdleTimeout != 30*time.Minute {
		t.Fatal("the previously published Env must not be mutated")
	}
	if built["ns-a"] != 1 {
		t.Fatalf("factory calls = %d, want 1 (components reused across the change)", built["ns-a"])
	}
}

func TestResolveUnknownHost(t *testing.T) {
	r := New(countingFactory(map[string]int{}))
	r.Rebuild(routes(map[string]string{"a.test": "ns-a"}), nil)
	if env, _, ok := r.Resolve("nope.test"); ok || env != nil {
		t.Fatalf("Resolve(nope.test) = %+v, %v; want nil, false", env, ok)
	}
}
