package registry

import (
	"testing"

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
	}))

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
	r.Rebuild(routes(map[string]string{"a.test": "ns-a"}))

	before, _, ok := r.Resolve("a.test")
	if !ok {
		t.Fatal("Resolve(a.test) not found after first Rebuild")
	}

	// A rebuild that keeps ns-a (new host) and adds ns-b must not rebuild ns-a's
	// Env: its power state and idle timer live there.
	r.Rebuild(routes(map[string]string{"a2.test": "ns-a", "b.test": "ns-b"}))

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
	r.Rebuild(routes(map[string]string{"a.test": "ns-a", "b.test": "ns-b"}))
	r.Rebuild(routes(map[string]string{"b.test": "ns-b"}))

	if _, _, ok := r.Resolve("a.test"); ok {
		t.Fatal("a.test still resolves after its namespace was removed")
	}
	if got := len(r.Envs()); got != 1 {
		t.Fatalf("Envs() len = %d, want 1", got)
	}
}

func TestResolveUnknownHost(t *testing.T) {
	r := New(countingFactory(map[string]int{}))
	r.Rebuild(routes(map[string]string{"a.test": "ns-a"}))
	if env, _, ok := r.Resolve("nope.test"); ok || env != nil {
		t.Fatalf("Resolve(nope.test) = %+v, %v; want nil, false", env, ok)
	}
}
