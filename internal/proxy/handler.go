// Package proxy is Gatekeeper's HTTP surface: it authenticates each request,
// records activity, wakes the namespace and holds the request when asleep, then
// reverse-proxies to the in-cluster app Service. Websocket upgrades are handled
// transparently by httputil.ReverseProxy after the auth gate runs.
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
	"github.com/autonoma-ai/gatekeeper/internal/routing"
)

const previewAuthPath = "/preview-auth"

// Waker is the power-management surface the handler needs.
type Waker interface {
	Asleep() bool
	EnsureAwake(ctx context.Context) error
}

// ReadinessWaiter blocks until a Service has a ready endpoint (or ctx expires).
type ReadinessWaiter interface {
	WaitForReady(ctx context.Context, service string) error
}

// Toucher records request activity.
type Toucher interface {
	Touch()
}

// Handler implements http.Handler for all preview traffic in one namespace.
type Handler struct {
	routes       *routing.Table
	gate         *auth.Gate
	power        Waker
	readiness    ReadinessWaiter
	tracker      Toucher
	authPageHTML string
	healthPath   string
	wakeTimeout  time.Duration
	proxy        *httputil.ReverseProxy
	log          *slog.Logger
}

// NewHandler wires the request pipeline. The reverse proxy uses a transport that
// retries dial-refused errors for the duration of the request context, covering
// the gap between a backend becoming scheduled and actually accepting connections.
func NewHandler(
	routes *routing.Table,
	gate *auth.Gate,
	pw Waker,
	readiness ReadinessWaiter,
	tracker Toucher,
	authPageHTML string,
	healthPath string,
	wakeTimeout time.Duration,
	log *slog.Logger,
) *Handler {
	transport := &retryTransport{base: http.DefaultTransport}
	return &Handler{
		routes:       routes,
		gate:         gate,
		power:        pw,
		readiness:    readiness,
		tracker:      tracker,
		authPageHTML: authPageHTML,
		healthPath:   healthPath,
		wakeTimeout:  wakeTimeout,
		proxy:        newReverseProxy(transport, log),
		log:          log,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Health check - unauthenticated (kubelet probes hit this).
	if r.URL.Path == h.healthPath {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
		return
	}

	// 2. Cookie-setter page - unauthenticated (it is how the cookie gets set).
	if r.URL.Path == previewAuthPath {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, h.authPageHTML)
		return
	}

	// 3. Route by Host header.
	upstream, ok := h.routes.Resolve(r.Host)
	if !ok {
		h.log.Warn("no route for host", "host", r.Host)
		http.Error(w, "Unknown preview host", http.StatusNotFound)
		return
	}

	// 4. Auth gate: bounce unauthenticated browsers to the login page.
	if !h.gate.Authorized(r) {
		http.Redirect(w, r, h.gate.RedirectLocation(r.Host, r.URL.RequestURI()), http.StatusFound)
		return
	}

	// 5. Record activity so the idle loop keeps the namespace awake.
	h.tracker.Touch()

	// 6. Wake the namespace and hold the request until the target is ready.
	if h.power.Asleep() {
		if err := h.wakeAndWait(r.Context(), upstream.Service); err != nil {
			h.log.Error("wake failed", "host", r.Host, "service", upstream.Service, "err", err)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "Preview environment is waking up, please retry shortly.", http.StatusServiceUnavailable)
			return
		}
	}

	// 7. Reverse-proxy to the upstream Service.
	target, err := url.Parse(h.routes.UpstreamURL(upstream))
	if err != nil {
		h.log.Error("invalid upstream URL", "service", upstream.Service, "err", err)
		http.Error(w, "Bad gateway", http.StatusBadGateway)
		return
	}
	h.proxy.ServeHTTP(w, r.WithContext(withTarget(r.Context(), target)))
}

func (h *Handler) wakeAndWait(ctx context.Context, service string) error {
	waitCtx, cancel := context.WithTimeout(ctx, h.wakeTimeout)
	defer cancel()
	if err := h.power.EnsureAwake(waitCtx); err != nil {
		return fmt.Errorf("ensure awake: %w", err)
	}
	if err := h.readiness.WaitForReady(waitCtx, service); err != nil {
		return fmt.Errorf("wait for ready: %w", err)
	}
	return nil
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
			// Preserve the public Host the app expects; advertise https upstream
			// (TLS is terminated at the ALB, so the inbound request is plain HTTP).
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
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
		// Dial failed (nothing listening yet); wait and retry until ctx expires.
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
