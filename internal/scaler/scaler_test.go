package scaler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	testNS   = "demo"
	selfName = "gatekeeper"
	wakeAnn  = "gatekeeper.dev/wake-replicas"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ptr(i int32) *int32 { return &i }

func boolPtr(b bool) *bool { return &b }

func deploy(name string, replicas int32, ann map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Annotations: ann},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(replicas)},
	}
}

func statefulSet(name string, replicas int32, ann map[string]string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Annotations: ann},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr(replicas)},
	}
}

// empty selector = all workloads in the namespace; self excluded by name.
func newScaler(client kubernetes.Interface) *Scaler {
	return New(client, testNS, "", selfName, wakeAnn, testLogger())
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
		deploy("web", 2, nil),
		deploy(selfName, 1, nil), // self - must NOT be scaled
		statefulSet("db", 1, nil),
	)
	s := newScaler(client)
	if err := s.SleepAll(context.Background()); err != nil {
		t.Fatalf("SleepAll: %v", err)
	}

	web := getDeploy(t, client, "web")
	if *web.Spec.Replicas != 0 || web.Annotations[wakeAnn] != "2" {
		t.Errorf("web replicas=%d ann=%q, want 0 / \"2\"", *web.Spec.Replicas, web.Annotations[wakeAnn])
	}
	db := getSts(t, client, "db")
	if *db.Spec.Replicas != 0 || db.Annotations[wakeAnn] != "1" {
		t.Errorf("db replicas=%d ann=%q, want 0 / \"1\"", *db.Spec.Replicas, db.Annotations[wakeAnn])
	}
	if gk := getDeploy(t, client, selfName); *gk.Spec.Replicas != 1 {
		t.Errorf("self replicas=%d, want 1 (never scales itself)", *gk.Spec.Replicas)
	}
}

func TestWakeAllRestoresReplicas(t *testing.T) {
	client := fake.NewSimpleClientset(
		deploy("web", 0, map[string]string{wakeAnn: "3"}),
		deploy("api", 0, nil), // no annotation -> default 1
		deploy(selfName, 0, nil),
	)
	s := newScaler(client)
	if err := s.WakeAll(context.Background()); err != nil {
		t.Fatalf("WakeAll: %v", err)
	}

	if web := getDeploy(t, client, "web"); *web.Spec.Replicas != 3 {
		t.Errorf("web replicas=%d, want 3 (restored from annotation)", *web.Spec.Replicas)
	}
	if api := getDeploy(t, client, "api"); *api.Spec.Replicas != 1 {
		t.Errorf("api replicas=%d, want 1 (default when annotation missing)", *api.Spec.Replicas)
	}
	if gk := getDeploy(t, client, selfName); *gk.Spec.Replicas != 0 {
		t.Errorf("self replicas=%d, want 0 (excluded from wake)", *gk.Spec.Replicas)
	}
}

// Proves wake state lives in the cluster (annotation), not in memory: a fresh
// Scaler restores a workload already at zero with the annotation set.
func TestWakeAfterRestartUsesAnnotation(t *testing.T) {
	client := fake.NewSimpleClientset(deploy("web", 0, map[string]string{wakeAnn: "4"}))
	if err := newScaler(client).WakeAll(context.Background()); err != nil {
		t.Fatalf("WakeAll: %v", err)
	}
	if web := getDeploy(t, client, "web"); *web.Spec.Replicas != 4 {
		t.Errorf("web replicas=%d, want 4", *web.Spec.Replicas)
	}
}

func TestIsAsleep(t *testing.T) {
	t.Run("all managed at zero is asleep", func(t *testing.T) {
		client := fake.NewSimpleClientset(deploy("web", 0, nil), deploy("api", 0, nil), deploy(selfName, 1, nil))
		if asleep, err := newScaler(client).IsAsleep(context.Background()); err != nil || !asleep {
			t.Fatalf("IsAsleep=%v err=%v, want true", asleep, err)
		}
	})
	t.Run("one running is awake", func(t *testing.T) {
		client := fake.NewSimpleClientset(deploy("web", 0, nil), deploy("api", 1, nil))
		if asleep, err := newScaler(client).IsAsleep(context.Background()); err != nil || asleep {
			t.Fatalf("IsAsleep=%v err=%v, want false", asleep, err)
		}
	})
}

func endpointSlice(service string, ready bool) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      service + "-abc",
			Namespace: testNS,
			Labels:    map[string]string{discoveryv1.LabelServiceName: service},
		},
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(ready)}},
		},
	}
}

func service(name string, selector map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec:       corev1.ServiceSpec{Selector: selector},
	}
}

// pod builds a Pod in the given phase, optionally with a single app container
// stuck in the named waiting reason (e.g. "ImagePullBackOff"); "" = no waiting.
func pod(name string, labels map[string]string, phase corev1.PodPhase, waitingReason string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: labels},
		Status:     corev1.PodStatus{Phase: phase},
	}
	if waitingReason != "" {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{
			{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitingReason}}},
		}
	}
	return p
}

func TestWaitForReady(t *testing.T) {
	t.Run("ready when an endpoint slice has a ready address", func(t *testing.T) {
		client := fake.NewSimpleClientset(endpointSlice("web", true))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := newScaler(client).WaitForReady(ctx, "web"); err != nil {
			t.Fatalf("WaitForReady: %v", err)
		}
	})
	t.Run("times out when no endpoint slices exist", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		if err := newScaler(fake.NewSimpleClientset()).WaitForReady(ctx, "web"); err == nil {
			t.Fatal("expected a timeout error")
		}
	})
	t.Run("times out when the only endpoint is not ready", func(t *testing.T) {
		client := fake.NewSimpleClientset(endpointSlice("web", false))
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		if err := newScaler(client).WaitForReady(ctx, "web"); err == nil {
			t.Fatal("expected a timeout error: endpoint is not ready")
		}
	})
	t.Run("fails fast when a backing pod is wedged", func(t *testing.T) {
		sel := map[string]string{"app": "web"}
		client := fake.NewSimpleClientset(
			service("web", sel),
			pod("web-xyz", sel, corev1.PodPending, "ImagePullBackOff"),
		)
		// A generous deadline: the test passes by failing fast, not by timing out.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := newScaler(client).WaitForReady(ctx, "web")
		if !errors.Is(err, ErrPodNotRunning) {
			t.Fatalf("WaitForReady err=%v, want ErrPodNotRunning", err)
		}
	})
	t.Run("a ready endpoint wins over a wedged pod", func(t *testing.T) {
		sel := map[string]string{"app": "web"}
		client := fake.NewSimpleClientset(
			service("web", sel),
			pod("web-xyz", sel, corev1.PodRunning, "CrashLoopBackOff"),
			endpointSlice("web", true),
		)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := newScaler(client).WaitForReady(ctx, "web"); err != nil {
			t.Fatalf("WaitForReady: %v, want nil (a ready endpoint should win)", err)
		}
	})
	t.Run("a still-starting pod is tolerated until the timeout", func(t *testing.T) {
		sel := map[string]string{"app": "web"}
		client := fake.NewSimpleClientset(
			service("web", sel),
			pod("web-xyz", sel, corev1.PodPending, "ContainerCreating"),
		)
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		err := newScaler(client).WaitForReady(ctx, "web")
		if err == nil || errors.Is(err, ErrPodNotRunning) {
			t.Fatalf("WaitForReady err=%v, want a timeout (ContainerCreating is not fatal)", err)
		}
	})
}

func TestPodFatalReason(t *testing.T) {
	initPod := func(reason string) *corev1.Pod {
		p := pod("p", nil, corev1.PodPending, "")
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: "init", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}},
		}
		return p
	}
	cases := []struct {
		name    string
		pod     *corev1.Pod
		wantBad bool
	}{
		{"running", pod("p", nil, corev1.PodRunning, ""), false},
		{"pending creating", pod("p", nil, corev1.PodPending, "ContainerCreating"), false},
		{"pending initializing", pod("p", nil, corev1.PodPending, "PodInitializing"), false},
		{"image pull backoff", pod("p", nil, corev1.PodPending, "ImagePullBackOff"), true},
		{"err image pull", pod("p", nil, corev1.PodPending, "ErrImagePull"), true},
		{"invalid image name", pod("p", nil, corev1.PodPending, "InvalidImageName"), true},
		{"config error", pod("p", nil, corev1.PodPending, "CreateContainerConfigError"), true},
		{"crash loop", pod("p", nil, corev1.PodRunning, "CrashLoopBackOff"), true},
		{"phase failed", pod("p", nil, corev1.PodFailed, ""), true},
		{"init crash loop", initPod("CrashLoopBackOff"), true},
		{"init creating", initPod("ContainerCreating"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, bad := podFatalReason(tc.pod); bad != tc.wantBad {
				t.Errorf("podFatalReason bad=%v, want %v", bad, tc.wantBad)
			}
		})
	}
}
