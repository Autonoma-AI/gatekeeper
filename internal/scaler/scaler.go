// Package scaler reconciles the replica counts of previewkit-managed workloads
// in a single namespace: it scales every managed Deployment and StatefulSet to
// zero on sleep, restores their saved counts on wake, and reports readiness by
// polling Service Endpoints. It uses the pod's in-cluster ServiceAccount.
package scaler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// WakeReplicasAnnotation stores a workload's pre-sleep replica count so wake can
// restore it. Written on sleep, read on wake.
const WakeReplicasAnnotation = "previewkit.dev/wake-replicas"

const readinessPollInterval = 500 * time.Millisecond

// Scaler performs sleep/wake/readiness operations against one namespace.
type Scaler struct {
	client          kubernetes.Interface
	namespace       string
	managedSelector string
	selfAppLabel    string
	log             *slog.Logger
}

// New builds a Scaler. managedSelector selects previewkit-managed workloads;
// selfAppLabel is the value of the `app` label on Gatekeeper's own Deployment,
// excluded from scaling so Gatekeeper never scales itself to zero.
func New(client kubernetes.Interface, namespace, managedSelector, selfAppLabel string, log *slog.Logger) *Scaler {
	return &Scaler{
		client:          client,
		namespace:       namespace,
		managedSelector: managedSelector,
		selfAppLabel:    selfAppLabel,
		log:             log,
	}
}

// workload is a uniform view over a Deployment or StatefulSet for sleep/wake.
type workload struct {
	kind     string
	name     string
	replicas int32 // current spec.replicas (nil treated as 1)
	wake     int32 // wake-replicas annotation value
	hasWake  bool
	patch    func(ctx context.Context, body []byte) error
}

// IsAsleep reports whether every managed (non-Gatekeeper) workload is at zero
// replicas. An empty namespace is reported as not-asleep (nothing to wake).
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
		body, err := sleepPatch(w.replicas)
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

// WakeAll restores every zero-replica managed workload to its saved replica
// count (defaulting to 1 when the annotation is missing or invalid).
func (s *Scaler) WakeAll(ctx context.Context) error {
	workloads, err := s.listManaged(ctx)
	if err != nil {
		return err
	}
	var errs error
	woken := 0
	for _, w := range workloads {
		if w.replicas != 0 {
			continue
		}
		target := int32(1)
		if w.hasWake && w.wake > 0 {
			target = w.wake
		} else {
			s.log.Warn("missing/invalid wake-replicas annotation; defaulting to 1", "kind", w.kind, "name", w.name)
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
		s.log.Info("scaled workload up", "kind", w.kind, "name", w.name, "replicas", target)
	}
	s.log.Info("wake complete", "woken", woken, "managed", len(workloads))
	return errs
}

// WaitForReady blocks until the named Service has at least one ready endpoint
// address or ctx is cancelled (the caller sets the wake deadline on ctx).
func (s *Scaler) WaitForReady(ctx context.Context, service string) error {
	ticker := time.NewTicker(readinessPollInterval)
	defer ticker.Stop()
	for {
		ready, err := s.endpointsReady(ctx, service)
		if err != nil {
			s.log.Warn("error checking endpoints readiness", "service", service, "err", err)
		} else if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for service %q to become ready: %w", service, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *Scaler) endpointsReady(ctx context.Context, service string) (bool, error) {
	ep, err := s.client.CoreV1().Endpoints(s.namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, subset := range ep.Subsets {
		if len(subset.Addresses) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func (s *Scaler) listManaged(ctx context.Context) ([]workload, error) {
	opts := metav1.ListOptions{LabelSelector: s.managedSelector}

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
		if s.isSelf(d.Labels) {
			continue
		}
		name := d.Name
		wake, hasWake := parseWake(d.Annotations)
		workloads = append(workloads, workload{
			kind:     "Deployment",
			name:     name,
			replicas: derefReplicas(d.Spec.Replicas),
			wake:     wake,
			hasWake:  hasWake,
			patch: func(ctx context.Context, body []byte) error {
				_, err := s.client.AppsV1().Deployments(s.namespace).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
				return err
			},
		})
	}
	for i := range sts.Items {
		st := &sts.Items[i]
		if s.isSelf(st.Labels) {
			continue
		}
		name := st.Name
		wake, hasWake := parseWake(st.Annotations)
		workloads = append(workloads, workload{
			kind:     "StatefulSet",
			name:     name,
			replicas: derefReplicas(st.Spec.Replicas),
			wake:     wake,
			hasWake:  hasWake,
			patch: func(ctx context.Context, body []byte) error {
				_, err := s.client.AppsV1().StatefulSets(s.namespace).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
				return err
			},
		})
	}
	return workloads, nil
}

func (s *Scaler) isSelf(labels map[string]string) bool {
	return labels["app"] == s.selfAppLabel
}

// sleepPatch sets the wake-replicas annotation and replicas=0 in one merge patch.
func sleepPatch(current int32) ([]byte, error) {
	body := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{
				WakeReplicasAnnotation: strconv.Itoa(int(current)),
			},
		},
		"spec": map[string]any{"replicas": 0},
	}
	return json.Marshal(body)
}

func replicasPatch(n int32) ([]byte, error) {
	body := map[string]any{"spec": map[string]any{"replicas": n}}
	return json.Marshal(body)
}

func parseWake(annotations map[string]string) (int32, bool) {
	raw, ok := annotations[WakeReplicasAnnotation]
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
