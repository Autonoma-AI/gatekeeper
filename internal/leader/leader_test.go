package leader

import (
	"context"
	"io"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

const testNS = "system"

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func pod(name string, leader bool) *corev1.Pod {
	labels := map[string]string{"app": "gatekeeper"}
	if leader {
		labels[RoleLabelKey] = RoleLabelLeader
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: labels}}
}

func newElector(t *testing.T, client kubernetes.Interface, podName string) *Elector {
	t.Helper()
	e, err := New(client, testNS, "gatekeeper", podName, nil, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func roleLabel(t *testing.T, client kubernetes.Interface, podName string) (string, bool) {
	t.Helper()
	p, err := client.CoreV1().Pods(testNS).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod %s: %v", podName, err)
	}
	v, ok := p.Labels[RoleLabelKey]
	return v, ok
}

func TestTakeTrafficLabelsSelfAndStripsPredecessor(t *testing.T) {
	client := fake.NewSimpleClientset(pod("gk-old", true), pod("gk-new", false))
	e := newElector(t, client, "gk-new")

	if err := e.tryTakeTraffic(context.Background()); err != nil {
		t.Fatalf("tryTakeTraffic: %v", err)
	}
	if v, ok := roleLabel(t, client, "gk-new"); !ok || v != RoleLabelLeader {
		t.Fatalf("gk-new role label = %q ok=%v, want leader", v, ok)
	}
	if _, ok := roleLabel(t, client, "gk-old"); ok {
		t.Fatal("stale leader label on gk-old was not stripped")
	}
}

func TestTakeTrafficIsIdempotent(t *testing.T) {
	client := fake.NewSimpleClientset(pod("gk-a", true))
	e := newElector(t, client, "gk-a")

	if err := e.tryTakeTraffic(context.Background()); err != nil {
		t.Fatalf("tryTakeTraffic: %v", err)
	}
	if v, ok := roleLabel(t, client, "gk-a"); !ok || v != RoleLabelLeader {
		t.Fatalf("gk-a role label = %q ok=%v, want leader (kept)", v, ok)
	}
}

// A crash-restarted container keeps its pod object and labels; startup must
// clear the stale label so a non-leader never receives Service traffic.
func TestClearOwnLabel(t *testing.T) {
	client := fake.NewSimpleClientset(pod("gk-a", true))
	e := newElector(t, client, "gk-a")

	e.clearOwnLabel(context.Background())
	if _, ok := roleLabel(t, client, "gk-a"); ok {
		t.Fatal("own stale leader label was not cleared at startup")
	}
}

// clearOwnLabel is best-effort: a missing pod (e.g. running outside the
// cluster in tests) must not panic or fail the startup path.
func TestClearOwnLabelToleratesMissingPod(t *testing.T) {
	e := newElector(t, fake.NewSimpleClientset(), "gk-gone")
	e.clearOwnLabel(context.Background())
}

func TestIsLeaderDefaultsFalse(t *testing.T) {
	e := newElector(t, fake.NewSimpleClientset(pod("gk-a", false)), "gk-a")
	if e.IsLeader() {
		t.Fatal("IsLeader() = true before any election")
	}
}

// Acquisition must seed state BEFORE the serving gate opens or traffic is
// steered here: during onLead the replica is still not "leader" to the
// handler and the idle loop, and the pod is not yet labeled.
func TestStartLeadingSeedsBeforeServingAndLabeling(t *testing.T) {
	client := fake.NewSimpleClientset(pod("gk-a", false))
	var e *Elector
	seeded := false
	onLead := func(context.Context) {
		seeded = true
		if e.IsLeader() {
			t.Error("IsLeader() = true during seeding; the gate must open after onLead")
		}
		if _, ok := roleLabel(t, client, "gk-a"); ok {
			t.Error("pod labeled before seeding finished; traffic could reach unseeded state")
		}
	}
	var err error
	e, err = New(client, testNS, "gatekeeper", "gk-a", onLead, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	e.startLeading(context.Background())

	if !seeded {
		t.Fatal("onLead was not called")
	}
	if !e.IsLeader() {
		t.Fatal("IsLeader() = false after startLeading")
	}
	if v, ok := roleLabel(t, client, "gk-a"); !ok || v != RoleLabelLeader {
		t.Fatalf("role label = %q ok=%v, want leader after startLeading", v, ok)
	}
}

// Leadership ending mid-seed must not open the serving gate: the process is
// already on its way out and must not attract traffic first.
func TestStartLeadingAbortsWhenLeadershipEndsMidSeed(t *testing.T) {
	client := fake.NewSimpleClientset(pod("gk-a", false))
	ctx, cancel := context.WithCancel(context.Background())
	var e *Elector
	var err error
	e, err = New(client, testNS, "gatekeeper", "gk-a", func(context.Context) { cancel() }, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	e.startLeading(ctx)

	if e.IsLeader() {
		t.Fatal("IsLeader() = true although leadership ended during seeding")
	}
	if _, ok := roleLabel(t, client, "gk-a"); ok {
		t.Fatal("pod labeled although leadership ended during seeding")
	}
}
