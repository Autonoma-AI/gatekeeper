package discovery

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	routesAnno = "gatekeeper.dev/routes"
	idleAnno   = "gatekeeper.dev/idle-timeout"
)

func ns(name string, created time.Time, annotations map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:              name,
		CreationTimestamp: metav1.NewTime(created),
		Annotations:       annotations,
	}}
}

func build(t *testing.T, namespaces ...*corev1.Namespace) desired {
	t.Helper()
	return buildDesired(namespaces, routesAnno, idleAnno, 30*time.Minute)
}

func TestBuildDesiredRoutesAndDefaults(t *testing.T) {
	d := build(t, ns("preview-a", time.Unix(100, 0), map[string]string{
		routesAnno: `{"APP.example.test": {"service": "web", "port": 3000}}`,
	}))

	if len(d.issues) != 0 {
		t.Fatalf("issues = %+v, want none", d.issues)
	}
	up, ok := d.routes["app.example.test"] // lowercased
	if !ok || up.Namespace != "preview-a" || up.Service != "web" || up.Port != 3000 {
		t.Fatalf("route = %+v ok=%v", up, ok)
	}
	if d.idleTimeouts["preview-a"] != 30*time.Minute {
		t.Fatalf("idle timeout = %v, want default 30m", d.idleTimeouts["preview-a"])
	}
}

func TestBuildDesiredIdleOverride(t *testing.T) {
	d := build(t,
		ns("fast", time.Unix(100, 0), map[string]string{
			routesAnno: `{"fast.test": {"service": "web", "port": 80}}`,
			idleAnno:   "5m",
		}),
		ns("never", time.Unix(101, 0), map[string]string{
			routesAnno: `{"never.test": {"service": "web", "port": 80}}`,
			idleAnno:   "0",
		}),
		ns("broken", time.Unix(102, 0), map[string]string{
			routesAnno: `{"broken.test": {"service": "web", "port": 80}}`,
			idleAnno:   "soonish",
		}),
	)

	if d.idleTimeouts["fast"] != 5*time.Minute {
		t.Errorf("fast = %v, want 5m", d.idleTimeouts["fast"])
	}
	if d.idleTimeouts["never"] != 0 {
		t.Errorf("never = %v, want 0 (auto-sleep disabled)", d.idleTimeouts["never"])
	}
	// Invalid override: keep the default, keep the routes, report an issue.
	if d.idleTimeouts["broken"] != 30*time.Minute {
		t.Errorf("broken = %v, want default 30m", d.idleTimeouts["broken"])
	}
	if _, ok := d.routes["broken.test"]; !ok {
		t.Error("an invalid idle annotation must not unmanage the namespace")
	}
	if len(d.issues) != 1 || d.issues[0].reason != "InvalidIdleTimeout" {
		t.Errorf("issues = %+v, want one InvalidIdleTimeout", d.issues)
	}
}

func TestBuildDesiredHostCollisionOldestWins(t *testing.T) {
	shared := `{"app.test": {"service": "web", "port": 80}}`
	d := build(t,
		ns("younger", time.Unix(200, 0), map[string]string{routesAnno: shared}),
		ns("older", time.Unix(100, 0), map[string]string{routesAnno: shared}),
	)

	if got := d.routes["app.test"].Namespace; got != "older" {
		t.Fatalf("collision winner = %q, want the older namespace", got)
	}
	if len(d.issues) != 1 || d.issues[0].reason != "HostCollision" || d.issues[0].namespace.Name != "younger" {
		t.Fatalf("issues = %+v, want one HostCollision on the younger namespace", d.issues)
	}
	// Losing one host must not invalidate the namespace itself: its other
	// (non-colliding) hosts would still route, so its settings stay computed.
	if _, ok := d.idleTimeouts["younger"]; !ok {
		t.Fatal("losing a host collision must not unmanage the namespace")
	}
}

func TestBuildDesiredCollisionTiebreakByName(t *testing.T) {
	shared := `{"app.test": {"service": "web", "port": 80}}`
	same := time.Unix(100, 0)
	d := build(t,
		ns("bbb", same, map[string]string{routesAnno: shared}),
		ns("aaa", same, map[string]string{routesAnno: shared}),
	)
	if got := d.routes["app.test"].Namespace; got != "aaa" {
		t.Fatalf("equal-age collision winner = %q, want deterministic name order (aaa)", got)
	}
}

func TestBuildDesiredIsolatesBadNamespaces(t *testing.T) {
	d := build(t,
		ns("good", time.Unix(100, 0), map[string]string{
			routesAnno: `{"good.test": {"service": "web", "port": 80}}`,
		}),
		ns("no-annotation", time.Unix(101, 0), nil),
		ns("bad-json", time.Unix(102, 0), map[string]string{routesAnno: `{nope}`}),
		ns("cross-namespace", time.Unix(103, 0), map[string]string{
			routesAnno: `{"evil.test": {"namespace": "victim", "service": "web", "port": 80}}`,
		}),
	)

	if _, ok := d.routes["good.test"]; !ok {
		t.Fatal("good namespace must survive its neighbors' bad annotations")
	}
	if _, ok := d.routes["evil.test"]; ok {
		t.Fatal("cross-namespace annotation route must be rejected")
	}
	if len(d.routes) != 1 || len(d.idleTimeouts) != 1 {
		t.Fatalf("routes=%d envs=%d, want 1/1 (only the good namespace managed)", len(d.routes), len(d.idleTimeouts))
	}
	if len(d.issues) != 3 {
		t.Fatalf("issues = %d, want 3 (missing, bad JSON, cross-namespace)", len(d.issues))
	}
	for _, iss := range d.issues {
		if iss.reason != "InvalidRoutes" {
			t.Errorf("issue reason = %q, want InvalidRoutes", iss.reason)
		}
	}
	if !strings.Contains(d.issues[2].message, "namespace") {
		t.Errorf("cross-namespace rejection should say why: %q", d.issues[2].message)
	}
}

func TestBuildDesiredSkipsTerminatingNamespaces(t *testing.T) {
	dying := ns("dying", time.Unix(100, 0), map[string]string{
		routesAnno: `{"dying.test": {"service": "web", "port": 80}}`,
	})
	now := metav1.Now()
	dying.DeletionTimestamp = &now

	d := build(t, dying)
	if len(d.routes) != 0 || len(d.issues) != 0 {
		t.Fatalf("terminating namespace: routes=%d issues=%d, want 0/0 (silently dropped)", len(d.routes), len(d.issues))
	}
}
