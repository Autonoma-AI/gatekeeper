// Package scaler reconciles the replica counts of the workloads it manages in a
// single namespace: it scales every selected Deployment and StatefulSet to zero
// on sleep, restores their saved counts on wake, and reports readiness by polling
// those workloads' status. It uses the pod's in-cluster ServiceAccount.
package scaler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const readinessPollInterval = 500 * time.Millisecond

// ErrPodNotRunning reports that a managed pod is wedged in a state it will not
// recover from on its own (bad/missing image, crash loop, bad config), so the
// wake wait gave up early instead of holding the request until the wake timeout.
// Transient startup states and unschedulable pods are not treated as failures -
// they may still resolve, so the wake timeout bounds those.
var ErrPodNotRunning = errors.New("managed pod not running")

// Scaler performs sleep/wake/readiness operations against one namespace.
type Scaler struct {
	client         kubernetes.Interface
	namespace      string
	targetSelector string
	selfName       string
	wakeAnnotation string
	dependsOnAnno  string
	log            *slog.Logger
}

// New builds a Scaler. targetSelector is the label selector for managed workloads
// (empty = all); selfName is the workload name to never scale - Gatekeeper itself,
// so callers pass it only for Gatekeeper's own namespace and "" (exclude nothing)
// elsewhere; wakeAnnotation is the annotation key used to remember a workload's
// replica count; dependsOnAnnotation is the annotation key listing a workload's
// dependencies (used to order wake-up).
func New(client kubernetes.Interface, namespace, targetSelector, selfName, wakeAnnotation, dependsOnAnnotation string, log *slog.Logger) *Scaler {
	return &Scaler{
		client:         client,
		namespace:      namespace,
		targetSelector: targetSelector,
		selfName:       selfName,
		wakeAnnotation: wakeAnnotation,
		dependsOnAnno:  dependsOnAnnotation,
		log:            log,
	}
}

// workload is a uniform view over a Deployment or StatefulSet for sleep/wake.
type workload struct {
	kind      string
	name      string
	replicas  int32 // current spec.replicas (nil treated as 1)
	ready     int32 // status.readyReplicas
	wake      int32 // wake-annotation value
	hasWake   bool
	dependsOn []string // names of workloads this one must wake after
	patch     func(ctx context.Context, body []byte) error
}

// IsAsleep reports whether every managed (non-self) workload is at zero replicas.
// An empty set is reported as not-asleep (nothing to wake).
func (s *Scaler) IsAsleep(ctx context.Context) (bool, error) {
	workloads, err := s.listManaged(ctx)
	if err != nil {
		return false, err
	}
	if len(workloads) == 0 {
		return false, nil
	}
	for _, w := range workloads {
		if w.replicas != 0 {
			return false, nil
		}
	}
	return true, nil
}

// SleepAll records each running workload's replica count on the wake annotation
// and scales it to zero. Already-zero workloads are skipped. Per-workload errors
// are joined and returned, but do not stop the remaining workloads.
func (s *Scaler) SleepAll(ctx context.Context) error {
	workloads, err := s.listManaged(ctx)
	if err != nil {
		return err
	}
	var errs error
	scaled := 0
	for _, w := range workloads {
		if w.replicas == 0 {
			continue
		}
		body, err := sleepPatch(s.wakeAnnotation, w.replicas)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("build sleep patch for %s/%s: %w", w.kind, w.name, err))
			continue
		}
		if err := w.patch(ctx, body); err != nil {
			if apierrors.IsNotFound(err) {
				s.log.Warn("workload vanished during sleep; skipping", "kind", w.kind, "name", w.name)
				continue
			}
			errs = errors.Join(errs, fmt.Errorf("scale %s/%s to zero: %w", w.kind, w.name, err))
			continue
		}
		scaled++
		s.log.Info("scaled workload to zero", "kind", w.kind, "name", w.name, "wakeReplicas", w.replicas)
	}
	s.log.Info("sleep complete", "scaled", scaled, "managed", len(workloads))
	return errs
}

// WakeAll restores every zero-replica managed workload to its saved replica count
// (defaulting to 1 when the annotation is missing or invalid). Workloads are woken
// in dependency order: each wave is scaled up and waited on until ready before the
// next wave (which depends on it) is scaled, so an app never starts before the
// database it depends on. With no dependencies declared this is a single wave -
// every workload scaled at once, exactly as before.
func (s *Scaler) WakeAll(ctx context.Context) error {
	workloads, err := s.listManaged(ctx)
	if err != nil {
		return err
	}
	waves := dependencyWaves(workloads, s.log)
	var errs error
	woken := 0
	for i, wave := range waves {
		want := make(map[string]bool, len(wave))
		for _, w := range wave {
			want[w.name] = true
			if w.replicas != 0 {
				continue // already awake; we still wait for it below
			}
			target := int32(1)
			if w.hasWake && w.wake > 0 {
				target = w.wake
			} else {
				s.log.Warn("missing/invalid wake annotation; defaulting to 1", "kind", w.kind, "name", w.name)
			}
			body, err := replicasPatch(target)
			if err != nil {
				errs = errors.Join(errs, fmt.Errorf("build wake patch for %s/%s: %w", w.kind, w.name, err))
				continue
			}
			if err := w.patch(ctx, body); err != nil {
				if apierrors.IsNotFound(err) {
					s.log.Warn("workload vanished during wake; skipping", "kind", w.kind, "name", w.name)
					continue
				}
				errs = errors.Join(errs, fmt.Errorf("scale %s/%s to %d: %w", w.kind, w.name, target, err))
				continue
			}
			woken++
			s.log.Info("scaled workload up", "kind", w.kind, "name", w.name, "replicas", target, "wave", i)
		}
		// Gate the next wave on this one being ready, so dependents never start
		// before their dependencies. The final wave's readiness is left to the
		// caller's WaitForReady (which waits for the whole namespace).
		if i < len(waves)-1 {
			if err := s.awaitReady(ctx, want); err != nil {
				return errors.Join(errs, fmt.Errorf("waiting for dependency wave %d/%d: %w", i+1, len(waves), err))
			}
		}
	}
	s.log.Info("wake complete", "woken", woken, "managed", len(workloads), "waves", len(waves))
	return errs
}

// dependencyWaves orders managed workloads into waves where every workload appears
// in a later wave than each of its dependencies (the depends-on annotation), and
// workloads in the same wave have no ordering constraint. Dependencies naming an
// unmanaged workload are ignored. A dependency cycle cannot be ordered, so all
// workloads collapse into a single wave (woken at once) with a warning.
func dependencyWaves(workloads []workload, log *slog.Logger) [][]workload {
	byName := make(map[string]workload, len(workloads))
	for _, w := range workloads {
		byName[w.name] = w
	}
	indeg := make(map[string]int, len(workloads))
	dependents := make(map[string][]string, len(workloads))
	for _, w := range workloads {
		for _, dep := range w.dependsOn {
			if _, ok := byName[dep]; !ok {
				continue // dependency isn't a managed workload here; nothing to wait on
			}
			indeg[w.name]++
			dependents[dep] = append(dependents[dep], w.name)
		}
	}

	var current []workload
	for _, w := range workloads {
		if indeg[w.name] == 0 {
			current = append(current, w)
		}
	}

	var waves [][]workload
	placed := 0
	for len(current) > 0 {
		waves = append(waves, current)
		placed += len(current)
		var next []workload
		for _, w := range current {
			for _, name := range dependents[w.name] {
				indeg[name]--
				if indeg[name] == 0 {
					next = append(next, byName[name])
				}
			}
		}
		current = next
	}

	if placed != len(workloads) {
		log.Warn("dependency cycle among managed workloads; waking all at once")
		return [][]workload{workloads}
	}
	return waves
}

// WaitForReady blocks until every managed workload in the namespace has all of
// its replicas ready - so a request is held until the whole environment is up,
// not just the Service it routes to (a web app must not be proxied before the
// database it depends on is ready). It returns early with ErrPodNotRunning if a
// managed pod becomes wedged in a non-recoverable state, or ctx is cancelled (the
// caller sets the wake deadline on ctx).
func (s *Scaler) WaitForReady(ctx context.Context) error {
	return s.awaitReady(ctx, nil)
}

// awaitReady blocks until every managed workload named in want (or every managed
// workload, when want is nil) has all of its replicas ready, a managed pod becomes
// wedged (ErrPodNotRunning), or ctx is cancelled.
func (s *Scaler) awaitReady(ctx context.Context, want map[string]bool) error {
	ticker := time.NewTicker(readinessPollInterval)
	defer ticker.Stop()
	var pending []string
	for {
		notReady, err := s.pendingWorkloads(ctx, want)
		if err != nil {
			s.log.Warn("error checking workload readiness", "err", err)
		} else if len(notReady) == 0 {
			return nil
		} else {
			pending = notReady
		}
		// Don't hold the request for the full timeout when a managed pod is wedged
		// (bad image, crash loop, ...): it won't become ready without a fix.
		if name, reason, failed := s.podFailure(ctx); failed {
			return fmt.Errorf("%w: %s (%s)", ErrPodNotRunning, name, reason)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for workloads to become ready (pending: %v): %w", pending, ctx.Err())
		case <-ticker.C:
		}
	}
}

// pendingWorkloads returns the managed workloads (as "Kind/name") that are not yet
// fully ready: fewer ready replicas than desired. When want is non-nil only the
// workloads named in it are considered. A workload at zero desired replicas is
// skipped (nothing to wait for). An empty result means the considered workloads
// are ready.
func (s *Scaler) pendingWorkloads(ctx context.Context, want map[string]bool) ([]string, error) {
	workloads, err := s.listManaged(ctx)
	if err != nil {
		return nil, err
	}
	var pending []string
	for _, w := range workloads {
		if want != nil && !want[w.name] {
			continue
		}
		if w.replicas > 0 && w.ready < w.replicas {
			pending = append(pending, w.kind+"/"+w.name)
		}
	}
	return pending, nil
}

// fatalWaitingReasons are container "waiting" reasons that mean a pod will not
// reach Running without a change. CrashLoopBackOff and ImagePullBackOff are
// already-retried backoff states; the rest are immediate, deterministic faults.
// Transient states (Pending, ContainerCreating, PodInitializing) and an
// unschedulable pod are deliberately excluded - they may still progress, so the
// wake timeout (not this check) bounds them.
var fatalWaitingReasons = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
}

// podFailure reports the first managed pod (matched by the same target selector
// used for scaling) that is wedged in a non-recoverable state, with a short
// reason for logging. A failure to list pods is logged and treated as "no
// failure" so a transient API error never turns a healthy wake into an error.
func (s *Scaler) podFailure(ctx context.Context) (name, reason string, failed bool) {
	pods, err := s.client.CoreV1().Pods(s.namespace).List(ctx, metav1.ListOptions{LabelSelector: s.targetSelector})
	if err != nil {
		s.log.Warn("error listing pods for health check", "err", err)
		return "", "", false
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if r, bad := podFatalReason(p); bad {
			return p.Name, r, true
		}
	}
	return "", "", false
}

// podFatalReason reports whether a pod is wedged in a non-recoverable state and,
// if so, a short reason. It checks the pod phase and both init and regular
// container waiting states.
func podFatalReason(p *corev1.Pod) (string, bool) {
	if p.Status.Phase == corev1.PodFailed {
		return "phase Failed", true
	}
	if r := fatalContainerReason(p.Status.InitContainerStatuses); r != "" {
		return r, true
	}
	if r := fatalContainerReason(p.Status.ContainerStatuses); r != "" {
		return r, true
	}
	return "", false
}

func fatalContainerReason(statuses []corev1.ContainerStatus) string {
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil && fatalWaitingReasons[w.Reason] {
			return w.Reason
		}
	}
	return ""
}

func (s *Scaler) listManaged(ctx context.Context) ([]workload, error) {
	opts := metav1.ListOptions{LabelSelector: s.targetSelector}

	deps, err := s.client.AppsV1().Deployments(s.namespace).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	sts, err := s.client.AppsV1().StatefulSets(s.namespace).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list statefulsets: %w", err)
	}

	workloads := make([]workload, 0, len(deps.Items)+len(sts.Items))
	for i := range deps.Items {
		d := &deps.Items[i]
		if d.Name == s.selfName {
			continue
		}
		name := d.Name
		wake, hasWake := parseWake(s.wakeAnnotation, d.Annotations)
		workloads = append(workloads, workload{
			kind:      "Deployment",
			name:      name,
			replicas:  derefReplicas(d.Spec.Replicas),
			ready:     d.Status.ReadyReplicas,
			wake:      wake,
			hasWake:   hasWake,
			dependsOn: parseDependsOn(s.dependsOnAnno, d.Annotations),
			patch: func(ctx context.Context, body []byte) error {
				_, err := s.client.AppsV1().Deployments(s.namespace).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
				return err
			},
		})
	}
	for i := range sts.Items {
		st := &sts.Items[i]
		if st.Name == s.selfName {
			continue
		}
		name := st.Name
		wake, hasWake := parseWake(s.wakeAnnotation, st.Annotations)
		workloads = append(workloads, workload{
			kind:      "StatefulSet",
			name:      name,
			replicas:  derefReplicas(st.Spec.Replicas),
			ready:     st.Status.ReadyReplicas,
			wake:      wake,
			hasWake:   hasWake,
			dependsOn: parseDependsOn(s.dependsOnAnno, st.Annotations),
			patch: func(ctx context.Context, body []byte) error {
				_, err := s.client.AppsV1().StatefulSets(s.namespace).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
				return err
			},
		})
	}
	return workloads, nil
}

// sleepPatch sets the wake annotation and replicas=0 in one merge patch.
func sleepPatch(annotation string, current int32) ([]byte, error) {
	body := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{annotation: strconv.Itoa(int(current))},
		},
		"spec": map[string]any{"replicas": 0},
	}
	return json.Marshal(body)
}

func replicasPatch(n int32) ([]byte, error) {
	body := map[string]any{"spec": map[string]any{"replicas": n}}
	return json.Marshal(body)
}

// parseDependsOn reads the comma-separated dependency annotation into workload
// names, trimming whitespace and dropping empties. A missing annotation yields no
// dependencies.
func parseDependsOn(annotation string, annotations map[string]string) []string {
	raw := annotations[annotation]
	if raw == "" {
		return nil
	}
	var deps []string
	for _, part := range strings.Split(raw, ",") {
		if name := strings.TrimSpace(part); name != "" {
			deps = append(deps, name)
		}
	}
	return deps
}

func parseWake(annotation string, annotations map[string]string) (int32, bool) {
	raw, ok := annotations[annotation]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, false
	}
	return int32(n), true
}

// derefReplicas returns the effective replica count: a nil pointer means the
// Kubernetes default of 1, never 0.
func derefReplicas(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}
