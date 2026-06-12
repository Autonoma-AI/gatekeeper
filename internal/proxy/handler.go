// Package proxy is Gatekeeper's HTTP surface: it (optionally) authenticates each
// request, records activity, wakes the namespace and holds the request when
// asleep, then reverse-proxies to the in-cluster upstream Service. Websocket
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
	"github.com/autonoma-ai/gatekeeper/internal/routing"
)

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

// Handler implements http.Handler for all traffic in one namespace.
type Handler struct {
	routes       *routing.Table
	gate         *auth.Gate
	power        Waker
	readiness    ReadinessWaiter
	tracker      Toucher
	callbackHTML string
	callbackPath string
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
	callbackHTML string,
	callbackPath string,
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
		callbackHTML: callbackHTML,
		callbackPath: callbackPath,
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

	// 2. Auth callback page - unauthenticated (it is how the cookie gets set).
	if r.URL.Path == h.callbackPath {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, h.callbackHTML)
		return
	}

	// 3. Route by Host header.
	upstream, ok := h.routes.Resolve(r.Host)
	if !ok {
		h.log.Warn("no route for host", "host", r.Host)
		http.Error(w, "Unknown host", http.StatusNotFound)
		return
	}

	// 4. Auth gate (a no-op when authentication is disabled): redirect browsers to
	//    the login URL if one is configured, otherwise reject with 401.
	if !h.gate.Authorized(r) {
		if loc, redirect := h.gate.LoginRedirect(requestScheme(r), r.Host, r.URL.RequestURI()); redirect {
			http.Redirect(w, r, loc, http.StatusFound)
		} else {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		}
		return
	}

	// 5. Record activity so the idle loop keeps the namespace awake.
	h.tracker.Touch()

	// 6. Wake the namespace and hold the request until the target is ready.
	if h.power.Asleep() {
		if err := h.wakeAndWait(r.Context(), upstream.Service); err != nil {
			h.log.Error("wake failed", "host", r.Host, "service", upstream.Service, "err", err)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "Service is waking up, please retry shortly.", http.StatusServiceUnavailable)
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
