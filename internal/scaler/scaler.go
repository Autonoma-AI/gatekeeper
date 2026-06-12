// Package scaler reconciles the replica counts of the workloads it manages in a
// single namespace: it scales every selected Deployment and StatefulSet to zero
// on sleep, restores their saved counts on wake, and reports readiness by polling
// Service Endpoints. It uses the pod's in-cluster ServiceAccount.
package scaler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const readinessPollInterval = 500 * time.Millisecond

// Scaler performs sleep/wake/readiness operations against one namespace.
type Scaler struct {
	client         kubernetes.Interface
	namespace      string
	targetSelector string
	selfName       string
	wakeAnnotation string
	log            *slog.Logger
}

// New builds a Scaler. targetSelector is the label selector for managed workloads
// (empty = all); selfName is the workload name to never scale (Gatekeeper itself);
// wakeAnnotation is the annotation key used to remember a workload's replica count.
func New(client kubernetes.Interface, namespace, targetSelector, selfName, wakeAnnotation string, log *slog.Logger) *Scaler {
	return &Scaler{
		client:         client,
		namespace:      namespace,
		targetSelector: targetSelector,
		selfName:       selfName,
		wakeAnnotation: wakeAnnotation,
		log:            log,
	}
}

// workload is a uniform view over a Deployment or StatefulSet for sleep/wake.
type workload struct {
	kind     string
	name     string
	replicas int32 // current spec.replicas (nil treated as 1)
	wake     int32 // wake-annotation value
	hasWake  bool
	patch    func(ctx context.Context, body []byte) error
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
// (defaulting to 1 when the annotation is missing or invalid).
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
		ready, err := s.serviceReady(ctx, service)
		if err != nil {
			s.log.Warn("error checking service readiness", "service", service, "err", err)
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

// serviceReady reports whether the Service has at least one ready, addressed
// endpoint. It reads EndpointSlices (discovery.k8s.io/v1) - the modern
// replacement for the deprecated core Endpoints API - selected by the
// kubernetes.io/service-name label the EndpointSlice controller sets.
func (s *Scaler) serviceReady(ctx context.Context, service string) (bool, error) {
	slices, err := s.client.DiscoveryV1().EndpointSlices(s.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + service,
	})
	if err != nil {
		return false, err
	}
	for i := range slices.Items {
		slice := &slices.Items[i]
		if slice.Labels[discoveryv1.LabelServiceName] != service {
			continue
		}
		for _, endpoint := range slice.Endpoints {
			// A nil Ready is "unknown"; per the API convention treat it as ready
			// (and the proxy's dial-retry covers any remaining gap). Explicit
			// false means the pod has not passed its readiness probe yet.
			ready := endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready
			if ready && len(endpoint.Addresses) > 0 {
				return true, nil
			}
		}
	}
	return false, nil
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
		if st.Name == s.selfName {
			continue
		}
		name := st.Name
		wake, hasWake := parseWake(s.wakeAnnotation, st.Annotations)
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
