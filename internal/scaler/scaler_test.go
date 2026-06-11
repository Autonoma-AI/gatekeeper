package scaler

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	testNS     = "preview-acme-pr-7"
	managedSel = "previewkit.dev/managed-by=previewkit"
	selfApp    = "gatekeeper"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ptr(i int32) *int32 { return &i }

func managedLabels(app string) map[string]string {
	return map[string]string{"previewkit.dev/managed-by": "previewkit", "app": app}
}

func deploy(name string, replicas int32, app string, ann map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: managedLabels(app), Annotations: ann},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(replicas)},
	}
}

func statefulSet(name string, replicas int32, app string, ann map[string]string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: managedLabels(app), Annotations: ann},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr(replicas)},
	}
}

func getDeploy(t *testing.T, c kubernetes.Interface, name string) *appsv1.Deployment {
	t.Helper()
	d, err := c.AppsV1().Deployments(testNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment %s: %v", name, err)
	}
	return d
}

func getSts(t *testing.T, c kubernetes.Interface, name string) *appsv1.StatefulSet {
	t.Helper()
	s, err := c.AppsV1().StatefulSets(testNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset %s: %v", name, err)
	}
	return s
}

func TestSleepAllScalesManagedAndSkipsSelf(t *testing.T) {
	client := fake.NewSimpleClientset(
		deploy("web", 2, "web", nil),
		deploy("gatekeeper", 1, selfApp, nil), // self - must NOT be scaled
		statefulSet("postgres", 1, "postgres", nil),
	)
	s := New(client, testNS, managedSel, selfApp, testLogger())

	if err := s.SleepAll(context.Background()); err != nil {
		t.Fatalf("SleepAll: %v", err)
	}

	web := getDeploy(t, client, "web")
	if *web.Spec.Replicas != 0 {
		t.Errorf("web replicas = %d, want 0", *web.Spec.Replicas)
	}
	if web.Annotations[WakeReplicasAnnotation] != "2" {
		t.Errorf("web wake annotation = %q, want \"2\"", web.Annotations[WakeReplicasAnnotation])
	}

	pg := getSts(t, client, "postgres")
	if *pg.Spec.Replicas != 0 || pg.Annotations[WakeReplicasAnnotation] != "1" {
		t.Errorf("postgres replicas=%d ann=%q, want 0 / \"1\"", *pg.Spec.Replicas, pg.Annotations[WakeReplicasAnnotation])
	}

	gk := getDeploy(t, client, "gatekeeper")
	if *gk.Spec.Replicas != 1 {
		t.Errorf("gatekeeper replicas = %d, want 1 (never scales itself)", *gk.Spec.Replicas)
	}
}

func TestWakeAllRestoresReplicas(t *testing.T) {
	client := fake.NewSimpleClientset(
		deploy("web", 0, "web", map[string]string{WakeReplicasAnnotation: "3"}),
		deploy("api", 0, "api", nil), // no annotation -> default 1
		deploy("gatekeeper", 0, selfApp, nil),
	)
	s := New(client, testNS, managedSel, selfApp, testLogger())

	if err := s.WakeAll(context.Background()); err != nil {
		t.Fatalf("WakeAll: %v", err)
	}

	if web := getDeploy(t, client, "web"); *web.Spec.Replicas != 3 {
		t.Errorf("web replicas = %d, want 3 (restored from annotation)", *web.Spec.Replicas)
	}
	if api := getDeploy(t, client, "api"); *api.Spec.Replicas != 1 {
		t.Errorf("api replicas = %d, want 1 (default when annotation missing)", *api.Spec.Replicas)
	}
	if gk := getDeploy(t, client, "gatekeeper"); *gk.Spec.Replicas != 0 {
		t.Errorf("gatekeeper replicas = %d, want 0 (excluded from wake)", *gk.Spec.Replicas)
	}
}

// Proves the wake state lives in the cluster (annotation), not Gatekeeper's
// memory: a fresh Scaler restores replicas for a workload that is already at
// zero with the annotation set (as it would be after a proxy restart).
func TestWakeAfterRestartUsesAnnotation(t *testing.T) {
	client := fake.NewSimpleClientset(
		deploy("web", 0, "web", map[string]string{WakeReplicasAnnotation: "4"}),
	)
	s := New(client, testNS, managedSel, selfApp, testLogger())
	if err := s.WakeAll(context.Background()); err != nil {
		t.Fatalf("WakeAll: %v", err)
	}
	if web := getDeploy(t, client, "web"); *web.Spec.Replicas != 4 {
		t.Errorf("web replicas = %d, want 4", *web.Spec.Replicas)
	}
}

func TestIsAsleep(t *testing.T) {
	t.Run("all managed at zero is asleep", func(t *testing.T) {
		client := fake.NewSimpleClientset(
			deploy("web", 0, "web", nil),
			deploy("api", 0, "api", nil),
			deploy("gatekeeper", 1, selfApp, nil), // self ignored
		)
		s := New(client, testNS, managedSel, selfApp, testLogger())
		asleep, err := s.IsAsleep(context.Background())
		if err != nil || !asleep {
			t.Fatalf("IsAsleep = %v, err %v; want true", asleep, err)
		}
	})

	t.Run("one running is awake", func(t *testing.T) {
		client := fake.NewSimpleClientset(
			deploy("web", 0, "web", nil),
			deploy("api", 1, "api", nil),
		)
		s := New(client, testNS, managedSel, selfApp, testLogger())
		asleep, err := s.IsAsleep(context.Background())
		if err != nil || asleep {
			t.Fatalf("IsAsleep = %v, err %v; want false", asleep, err)
		}
	})
}

func TestWaitForReady(t *testing.T) {
	t.Run("returns when endpoints have addresses", func(t *testing.T) {
		client := fake.NewSimpleClientset(&corev1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: testNS},
			Subsets:    []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: "10.0.0.1"}}}},
		})
		s := New(client, testNS, managedSel, selfApp, testLogger())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.WaitForReady(ctx, "web"); err != nil {
			t.Fatalf("WaitForReady: %v", err)
		}
	})

	t.Run("times out when never ready", func(t *testing.T) {
		client := fake.NewSimpleClientset() // no endpoints object
		s := New(client, testNS, managedSel, selfApp, testLogger())
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		if err := s.WaitForReady(ctx, "web"); err == nil {
			t.Fatal("WaitForReady: expected timeout error, got nil")
		}
	})
}
