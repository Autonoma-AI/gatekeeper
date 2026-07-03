// Package leader elects a single active Gatekeeper replica via a Lease and
// steers Service traffic to it by labeling the leader pod. Readiness stays
// uniform across replicas - a Deployment whose standby pods are permanently
// unready can never complete a rollout - so the leader Service selects on the
// pod label instead. Losing leadership is terminal for the process: it exits
// and restarts as a standby, re-deriving all state from the cluster exactly
// like any other restart.
package leader

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const (
	// RoleLabelKey marks the pod that receives Service traffic; the leader
	// Service's selector must match RoleLabelKey=RoleLabelLeader (see
	// deploy/cluster/service.yaml).
	RoleLabelKey    = "gatekeeper.dev/role"
	RoleLabelLeader = "leader"
)

const (
	// Election timings: the client-go defaults. Worst-case failover after a
	// leader crash is roughly leaseDuration + the label/endpoint propagation.
	leaseDuration = 15 * time.Second
	renewDeadline = 10 * time.Second
	retryPeriod   = 2 * time.Second

	// An unlabeled leader receives no traffic, so label failures are retried
	// this often for as long as leadership is held.
	labelRetryInterval = 2 * time.Second
)

// Elector joins the leader election and manages the leader pod label.
type Elector struct {
	client    kubernetes.Interface
	namespace string
	podName   string
	elector   *leaderelection.LeaderElector
	onLead    func(ctx context.Context)
	log       *slog.Logger

	// rootCtx is set by Run before joining the election and read by the
	// callbacks to tell a shutdown-triggered lease release from a real loss.
	rootCtx context.Context

	leading atomic.Bool
	lost    chan struct{}
}

// New builds an Elector. namespace is the pod's own namespace (Lease and pod
// label live there); podName is the election identity and the labeled pod.
// onLead runs once per acquisition, before traffic is steered here, so the
// leader can seed its state; it must respect ctx, which ends with leadership.
func New(client kubernetes.Interface, namespace, leaseName, podName string, onLead func(ctx context.Context), log *slog.Logger) (*Elector, error) {
	e := &Elector{
		client:    client,
		namespace: namespace,
		podName:   podName,
		onLead:    onLead,
		log:       log,
		lost:      make(chan struct{}),
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: leaseName, Namespace: namespace},
		Client:     client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: podName},
	}
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: leaseDuration,
		RenewDeadline: renewDeadline,
		RetryPeriod:   retryPeriod,
		// On graceful shutdown the Lease is released so a standby takes over
		// immediately instead of waiting out the lease duration.
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: e.startLeading,
			OnStoppedLeading: e.stopLeading,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("configure leader election: %w", err)
	}
	e.elector = elector
	return e, nil
}

// IsLeader reports whether this replica holds the lease AND has finished
// seeding (onLead). The serving gate and the idle loop key off it, so a
// standby - or a freshly restarted pod still wearing a stale leader label -
// fails closed instead of serving off default power state and aged idle
// timers.
func (e *Elector) IsLeader() bool { return e.leading.Load() }

// startLeading orders a leadership acquisition: seed state, then open the
// gate, then steer traffic here. A request must never arrive before onLead
// has derived real power state, and the idle loop must never tick against a
// standby's aged activity timers.
func (e *Elector) startLeading(ctx context.Context) {
	e.log.Info("became leader")
	if e.onLead != nil {
		e.onLead(ctx)
	}
	if ctx.Err() != nil {
		return // leadership already ended mid-seed; the process is exiting
	}
	e.leading.Store(true)
	e.takeTraffic(ctx)
}

func (e *Elector) stopLeading() {
	e.leading.Store(false)
	// Losing the lease during shutdown is the ReleaseOnCancel path, not a
	// failure; only an unexpected loss is signalled.
	if e.rootCtx != nil && e.rootCtx.Err() != nil {
		e.log.Info("released leadership on shutdown")
		return
	}
	e.log.Warn("leadership lost")
	close(e.lost)
}

// Lost is closed if leadership is lost while the process is meant to keep
// running; the caller should exit so the pod restarts as a standby. It is
// never closed for a shutdown-triggered release.
func (e *Elector) Lost() <-chan struct{} { return e.lost }

// Run clears any stale leader label from a previous life of this pod (a
// container crash-restart keeps the pod object and its labels), then joins the
// election and blocks until ctx is cancelled or leadership is lost.
func (e *Elector) Run(ctx context.Context) {
	e.rootCtx = ctx
	e.clearOwnLabel(ctx)
	e.elector.Run(ctx)
}

// takeTraffic strips the leader label from any other pod still carrying it (a
// crashed or partitioned predecessor) and labels this pod, retrying until it
// succeeds or leadership ends: an unlabeled leader receives no traffic.
func (e *Elector) takeTraffic(ctx context.Context) {
	for {
		err := e.tryTakeTraffic(ctx)
		if err == nil {
			e.log.Info("steering traffic to this pod", "label", RoleLabelKey+"="+RoleLabelLeader)
			return
		}
		e.log.Error("failed to take traffic; retrying", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(labelRetryInterval):
		}
	}
}

func (e *Elector) tryTakeTraffic(ctx context.Context) error {
	pods, err := e.client.CoreV1().Pods(e.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: RoleLabelKey + "=" + RoleLabelLeader,
	})
	if err != nil {
		return fmt.Errorf("list leader-labeled pods: %w", err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Name == e.podName {
			continue
		}
		if err := e.patchRoleLabel(ctx, p.Name, false); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("strip stale leader label from pod %s: %w", p.Name, err)
		}
		e.log.Info("stripped stale leader label", "pod", p.Name)
	}
	if err := e.patchRoleLabel(ctx, e.podName, true); err != nil {
		return fmt.Errorf("label own pod %s: %w", e.podName, err)
	}
	return nil
}

// clearOwnLabel is best-effort: a pod that cannot be found or patched at
// startup is logged, not fatal - the election has not been joined yet.
func (e *Elector) clearOwnLabel(ctx context.Context) {
	if err := e.patchRoleLabel(ctx, e.podName, false); err != nil && !apierrors.IsNotFound(err) {
		e.log.Warn("could not clear own leader label at startup", "err", err)
	}
}

// patchRoleLabel sets (leader=true) or removes (leader=false) the role label
// on the named pod in one JSON merge patch; a null label value removes it.
func (e *Elector) patchRoleLabel(ctx context.Context, podName string, leader bool) error {
	var value any
	if leader {
		value = RoleLabelLeader
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"labels": map[string]any{RoleLabelKey: value}},
	})
	if err != nil {
		return err
	}
	_, err = e.client.CoreV1().Pods(e.namespace).Patch(ctx, podName, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}
