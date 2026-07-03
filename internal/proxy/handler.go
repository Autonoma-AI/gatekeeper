// Package proxy is Gatekeeper's HTTP surface: it (optionally) authenticates each
// request, records activity, wakes the routed namespace and holds the request
// when asleep, then reverse-proxies to the in-cluster upstream Service. Websocket
// upgrades are handled transparently by httputil.ReverseProxy after the gate runs.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"syscall"
	"time"

	"github.com/autonoma-ai/gatekeeper/internal/auth"
	"github.com/autonoma-ai/gatekeeper/internal/registry"
	"github.com/autonoma-ai/gatekeeper/internal/routing"
)

// Resolver maps a request's Host header to the upstream that serves it and the
// managed namespace (Env) the upstream belongs to (implemented by
// *registry.Registry).
type Resolver interface {
	Resolve(host string) (*registry.Env, routing.Upstream, bool)
}

// Handler implements http.Handler for all traffic across the managed namespaces.
type Handler struct {
	resolver     Resolver
	gate         *auth.Gate
	callbackHTML string
	callbackPath string
	healthPath   string
	readyPath    string
	ready        func() bool
	serving      func() bool
	wakeTimeout  time.Duration
	proxy        *httputil.ReverseProxy
	log          *slog.Logger
}

// NewHandler wires the request pipeline. ready gates the readiness endpoint
// and serving gates all proxied traffic (nil = always); liveness (healthPath)
// is unconditional. With leader election, serving is the leader check: a
// standby - or a restarted pod still wearing a stale leader label - must fail
// closed rather than serve off unseeded power state. The reverse proxy uses a
// transport that retries dial-refused errors for the duration of the request
// context, covering the gap between a backend becoming scheduled and actually
// accepting connections.
func NewHandler(
	resolver Resolver,
	gate *auth.Gate,
	callbackHTML string,
	callbackPath string,
	healthPath string,
	readyPath string,
	ready func() bool,
	serving func() bool,
	wakeTimeout time.Duration,
	log *slog.Logger,
) *Handler {
	transport := &retryTransport{base: http.DefaultTransport}
	return &Handler{
		resolver:     resolver,
		gate:         gate,
		callbackHTML: callbackHTML,
		callbackPath: callbackPath,
		healthPath:   healthPath,
		readyPath:    readyPath,
		ready:        ready,
		serving:      serving,
		wakeTimeout:  wakeTimeout,
		proxy:        newReverseProxy(transport, log),
		log:          log,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Health check (liveness) - unauthenticated (kubelet probes hit this),
	//    and unconditional: a live process is a live process.
	if r.URL.Path == h.healthPath {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
		return
	}

	// 1b. Readiness - unauthenticated, and gated (e.g. on discovery cache
	//     sync) so a pod can be pulled from endpoints without being restarted.
	if r.URL.Path == h.readyPath {
		w.Header().Set("Content-Type", "text/plain")
		if h.ready != nil && !h.ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "not ready")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ready")
		return
	}

	// 2. Auth callback page - unauthenticated (it is how the cookie gets set).
	if r.URL.Path == h.callbackPath {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, h.callbackHTML)
		return
	}

	// 3. Fail closed unless this replica may serve (with leader election: it
	//    is the seeded leader). Probes and the auth callback stay available on
	//    every replica; everything that would touch, wake, or proxy does not.
	if h.serving != nil && !h.serving() {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "Not the active replica, please retry shortly.", http.StatusServiceUnavailable)
		return
	}

	// 4. Route by Host header to an upstream and its namespace.
	env, upstream, ok := h.resolver.Resolve(r.Host)
	if !ok {
		h.log.Warn("no route for host", "host", r.Host)
		http.Error(w, "Unknown host", http.StatusNotFound)
		return
	}

	// 5. Auth gate (a no-op when authentication is disabled): redirect browsers to
	//    the login URL if one is configured, otherwise reject with 401.
	if !h.gate.Authorized(r) {
		if loc, redirect := h.gate.LoginRedirect(requestScheme(r), r.Host, r.URL.RequestURI()); redirect {
			http.Redirect(w, r, loc, http.StatusFound)
		} else {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		}
		return
	}

	// 6. Record activity so the idle loop keeps the namespace awake.
	env.Activity.Touch()

	// 7. Wake the namespace and hold the request until every managed workload is
	//    ready (not just this route's Service - so dependencies are up too).
	if env.Power.Asleep() {
		if err := h.wakeAndWait(r.Context(), env); err != nil {
			h.log.Error("wake failed", "namespace", env.Namespace, "host", r.Host, "service", upstream.Service, "err", err)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "Service is waking up, please retry shortly.", http.StatusServiceUnavailable)
			return
		}
	}

	// 8. Reverse-proxy to the upstream Service.
	target, err := url.Parse(upstream.URL())
	if err != nil {
		h.log.Error("invalid upstream URL", "service", upstream.Service, "err", err)
		http.Error(w, "Bad gateway", http.StatusBadGateway)
		return
	}
	h.proxy.ServeHTTP(w, r.WithContext(withTarget(r.Context(), target)))
}

func (h *Handler) wakeAndWait(ctx context.Context, env *registry.Env) error {
	waitCtx, cancel := context.WithTimeout(ctx, h.wakeTimeout)
	defer cancel()
	if err := env.Power.EnsureAwake(waitCtx); err != nil {
		return fmt.Errorf("ensure awake: %w", err)
	}
	if err := env.Readiness.WaitForReady(waitCtx); err != nil {
		return fmt.Errorf("wait for ready: %w", err)
	}
	return nil
}

// requestScheme derives the public scheme of the original request: an upstream
// X-Forwarded-Proto wins (TLS is usually terminated before Gatekeeper), then
// direct TLS, else http.
func requestScheme(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// ctxKey is an unexported context key type for stashing the resolved target.
type ctxKey int

const targetKey ctxKey = iota

func withTarget(ctx context.Context, target *url.URL) context.Context {
	return context.WithValue(ctx, targetKey, target)
}

func newReverseProxy(transport http.RoundTripper, log *slog.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			if target, ok := pr.In.Context().Value(targetKey).(*url.URL); ok && target != nil {
				pr.SetURL(target)
			}
			// Preserve the public Host the app expects and forward the original
			// client/proto so the upstream sees a faithful request.
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Proto", requestScheme(pr.In))
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn("upstream proxy error", "host", r.Host, "err", err)
			w.WriteHeader(http.StatusBadGateway)
		},
		Transport: transport,
	}
}

// retryTransport retries requests that fail to dial (connection refused) until
// the request context expires. This bridges the brief window after a pod is
// scheduled but before its server is accepting - notably for apps with no
// readiness probe, whose endpoints are reported ready as soon as the pod runs.
type retryTransport struct {
	base http.RoundTripper
}

const dialRetryInterval = 300 * time.Millisecond

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for {
		resp, err := t.base.RoundTrip(req)
		if err == nil || !isDialError(err) {
			return resp, err
		}
		select {
		case <-req.Context().Done():
			return nil, err
		case <-time.After(dialRetryInterval):
		}
	}
}

func isDialError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}
