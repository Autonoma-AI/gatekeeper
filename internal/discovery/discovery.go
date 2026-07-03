package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"github.com/autonoma-ai/gatekeeper/internal/registry"
)

// resyncPeriod is the informer's safety relist: even a missed watch event is
// reconciled within this window.
const resyncPeriod = 10 * time.Minute

// Options configures a Watcher.
type Options struct {
	Client   kubernetes.Interface
	Registry *registry.Registry
	// Selector is the namespace label selector (NAMESPACE_SELECTOR).
	Selector              string
	RoutesAnnotation      string
	IdleTimeoutAnnotation string
	DefaultIdleTimeout    time.Duration
	// OnEnvAdded is called (from the informer goroutine) for every Env a
	// rebuild created, so the caller can seed its power state. May be nil.
	OnEnvAdded func(*registry.Env)
	// EmitEvents records per-namespace problems (bad annotations, host
	// collisions) as Kubernetes Events on the offending Namespace.
	EmitEvents bool
	Log        *slog.Logger
}

// Watcher keeps the registry in sync with the labeled namespaces.
type Watcher struct {
	opts        Options
	informer    cache.SharedIndexInformer
	broadcaster record.EventBroadcaster // nil unless EmitEvents
	recorder    record.EventRecorder    // nil unless EmitEvents
}

// New validates the selector and wires the (not yet running) informer.
func New(o Options) (*Watcher, error) {
	if _, err := labels.Parse(o.Selector); err != nil {
		return nil, fmt.Errorf("invalid NAMESPACE_SELECTOR %q: %w", o.Selector, err)
	}
	w := &Watcher{opts: o}
	if o.EmitEvents {
		w.broadcaster = record.NewBroadcaster()
		w.recorder = w.broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "gatekeeper"})
	}

	factory := informers.NewSharedInformerFactoryWithOptions(o.Client, resyncPeriod,
		informers.WithTweakListOptions(func(lo *metav1.ListOptions) { lo.LabelSelector = o.Selector }))
	w.informer = factory.Core().V1().Namespaces().Informer()
	// Handlers run on a single informer goroutine, so rebuilds never race each
	// other. Every event triggers a full rebuild from the informer's store:
	// at preview scale that is microseconds, and it cannot miss state that an
	// incremental update would.
	_, err := w.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { w.rebuild() },
		UpdateFunc: func(any, any) { w.rebuild() },
		DeleteFunc: func(any) { w.rebuild() },
	})
	if err != nil {
		return nil, fmt.Errorf("register namespace handler: %w", err)
	}
	return w, nil
}

// Run starts event recording and the informer, and blocks until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	if w.broadcaster != nil {
		w.broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: w.opts.Client.CoreV1().Events("")})
		defer w.broadcaster.Shutdown()
	}
	w.opts.Log.Info("namespace discovery started",
		"selector", w.opts.Selector, "routesAnnotation", w.opts.RoutesAnnotation)
	w.informer.Run(ctx.Done())
}

// HasSynced reports whether the initial namespace list has been processed;
// readiness gates on it so a fresh pod never serves 404s for routes it simply
// has not seen yet.
func (w *Watcher) HasSynced() bool { return w.informer.HasSynced() }

// WaitForSync blocks until the cache has synced or ctx ends, reporting which.
func (w *Watcher) WaitForSync(ctx context.Context) bool {
	return cache.WaitForCacheSync(ctx.Done(), w.informer.HasSynced)
}

func (w *Watcher) rebuild() {
	var namespaces []*corev1.Namespace
	for _, obj := range w.informer.GetStore().List() {
		if ns, ok := obj.(*corev1.Namespace); ok {
			namespaces = append(namespaces, ns)
		}
	}

	d := buildDesired(namespaces, w.opts.RoutesAnnotation, w.opts.IdleTimeoutAnnotation, w.opts.DefaultIdleTimeout)
	for _, iss := range d.issues {
		w.opts.Log.Warn("namespace issue during rebuild",
			"namespace", iss.namespace.Name, "reason", iss.reason, "detail", iss.message)
		if w.recorder != nil {
			w.recorder.Event(iss.namespace, corev1.EventTypeWarning, iss.reason, iss.message)
		}
	}

	added := w.opts.Registry.Rebuild(d.routes, d.idleTimeouts)
	w.opts.Log.Debug("routes rebuilt",
		"namespaces", len(d.idleTimeouts), "routes", len(d.routes), "added", len(added), "issues", len(d.issues))
	if w.opts.OnEnvAdded != nil {
		for _, env := range added {
			w.opts.OnEnvAdded(env)
		}
	}
}
