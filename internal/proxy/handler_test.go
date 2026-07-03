package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/autonoma-ai/gatekeeper/internal/auth"
	"github.com/autonoma-ai/gatekeeper/internal/registry"
	"github.com/autonoma-ai/gatekeeper/internal/routing"
)

const (
	testToken  = "tok-123"
	testHeader = "X-Gatekeeper-Token"
	testCookie = "gatekeeper_session"
	testHost   = "app.example.test"
)

type fakePower struct {
	asleep      bool
	ensureCalls int
}

func (f *fakePower) Init(context.Context) error        { return nil }
func (f *fakePower) Asleep() bool                      { return f.asleep }
func (f *fakePower) EnsureAwake(context.Context) error { f.ensureCalls++; return nil }
func (f *fakePower) Sleep(context.Context) error       { return nil }

type fakeReadiness struct{ calls int }

func (f *fakeReadiness) WaitForReady(context.Context) error { f.calls++; return nil }

type fakeActivity struct{ calls int }

func (f *fakeActivity) Touch()                 { f.calls++ }
func (f *fakeActivity) IdleFor() time.Duration { return 0 }

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func handlerWith(gate *auth.Gate, pw *fakePower, rd *fakeReadiness, act *fakeActivity) *Handler {
	reg := registry.New(func(ns string) *registry.Env {
		return &registry.Env{Namespace: ns, Power: pw, Readiness: rd, Activity: act}
	})
	reg.Rebuild(map[string]routing.Upstream{
		testHost: {Namespace: "test-ns", Service: "web", Port: 3000},
	})
	return NewHandler(reg, gate, "<html>callback</html>", "/_gatekeeper/auth", "/healthz", "/readyz", nil, nil, 2*time.Second, testLogger())
}

func enabledGate(loginURL string) *auth.Gate {
	return auth.NewGate(testToken, testHeader, testCookie, loginURL)
}
func disabledGate() *auth.Gate { return auth.NewGate("", testHeader, testCookie, "") }

func TestHealthIsUnauthenticated(t *testing.T) {
	h := handlerWith(enabledGate(""), &fakePower{}, &fakeReadiness{}, &fakeActivity{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+testHost+"/healthz", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Fatalf("health = %d %q", rec.Code, rec.Body.String())
	}
}

func TestAuthCallbackPathServesPageUnauthenticated(t *testing.T) {
	h := handlerWith(enabledGate(""), &fakePower{}, &fakeReadiness{}, &fakeActivity{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+testHost+"/_gatekeeper/auth?token=x&next=/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "callback") {
		t.Fatalf("callback = %d %q", rec.Code, rec.Body.String())
	}
}

// Readiness is unauthenticated like health, but respects the gating func so a
// pod can be pulled from endpoints (e.g. before discovery cache sync) without
// failing liveness.
func TestReadyPathFollowsGate(t *testing.T) {
	ready := true
	reg := registry.New(func(ns string) *registry.Env { return &registry.Env{Namespace: ns} })
	h := NewHandler(reg, enabledGate(""), "", "/_gatekeeper/auth", "/healthz", "/readyz", func() bool { return ready }, nil, time.Second, testLogger())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+testHost+"/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready readyz = %d, want 200", rec.Code)
	}

	ready = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+testHost+"/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready readyz = %d, want 503", rec.Code)
	}
}

// A replica that may not serve (standby, or a restarted pod still wearing a
// stale leader label) fails closed on proxied paths but keeps answering
// probes, so kubelet never restarts a healthy standby.
func TestStandbyFailsClosed(t *testing.T) {
	serving := false
	act := &fakeActivity{}
	reg := registry.New(func(ns string) *registry.Env {
		return &registry.Env{Namespace: ns, Power: &fakePower{}, Readiness: &fakeReadiness{}, Activity: act}
	})
	reg.Rebuild(map[string]routing.Upstream{testHost: {Namespace: "test-ns", Service: "web", Port: 3000}})
	h := NewHandler(reg, disabledGate(), "", "/_gatekeeper/auth", "/healthz", "/readyz", nil,
		func() bool { return serving }, time.Second, testLogger())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+testHost+"/", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("standby proxied request = %d (Retry-After %q), want 503 with Retry-After",
			rec.Code, rec.Header().Get("Retry-After"))
	}
	if act.calls != 0 {
		t.Fatalf("standby recorded activity: %d touches, want 0", act.calls)
	}

	for _, path := range []string{"/healthz", "/readyz"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+testHost+path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("standby %s = %d, want 200", path, rec.Code)
		}
	}

	serving = true
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+testHost+"/", nil).WithContext(ctx))
	if rec.Code == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") == "2" {
		t.Fatal("serving replica still fails closed")
	}
	if act.calls != 1 {
		t.Fatalf("serving replica touches = %d, want 1", act.calls)
	}
}

func TestUnknownHostIs404(t *testing.T) {
	h := handlerWith(disabledGate(), &fakePower{}, &fakeReadiness{}, &fakeActivity{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://nope.example.test/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAuthDisabledPassesThrough(t *testing.T) {
	act := &fakeActivity{}
	h := handlerWith(disabledGate(), &fakePower{asleep: false}, &fakeReadiness{}, act)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/", nil).WithContext(ctx)
	h.ServeHTTP(rec, req)
	// Not gated: it proceeds to touch + proxy (upstream unreachable -> 502).
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusFound {
		t.Fatalf("auth-disabled request should not be gated, got %d", rec.Code)
	}
	if act.calls != 1 {
		t.Fatalf("Touch calls = %d, want 1", act.calls)
	}
}

func TestUnauthorizedRedirectsWhenLoginURLSet(t *testing.T) {
	act := &fakeActivity{}
	h := handlerWith(enabledGate("https://login.example.test"), &fakePower{}, &fakeReadiness{}, act)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+testHost+"/dashboard", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://login.example.test?redirect=") {
		t.Fatalf("Location = %q", loc)
	}
	if act.calls != 0 {
		t.Fatalf("unauthorized request must not record activity, got %d", act.calls)
	}
}

func TestUnauthorizedIs401WhenNoLoginURL(t *testing.T) {
	h := handlerWith(enabledGate(""), &fakePower{}, &fakeReadiness{}, &fakeActivity{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+testHost+"/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthorizedAwakeTouchesAndDoesNotWake(t *testing.T) {
	pw := &fakePower{asleep: false}
	act := &fakeActivity{}
	h := handlerWith(enabledGate(""), pw, &fakeReadiness{}, act)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/", nil).WithContext(ctx)
	req.Header.Set(testHeader, testToken)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if act.calls != 1 || pw.ensureCalls != 0 {
		t.Fatalf("touch=%d ensure=%d, want 1/0", act.calls, pw.ensureCalls)
	}
}

func TestAuthorizedAsleepWakesBeforeProxy(t *testing.T) {
	pw := &fakePower{asleep: true}
	rd := &fakeReadiness{}
	act := &fakeActivity{}
	h := handlerWith(enabledGate(""), pw, rd, act)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/", nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: testCookie, Value: testToken})
	h.ServeHTTP(httptest.NewRecorder(), req)
	if act.calls != 1 || pw.ensureCalls != 1 || rd.calls != 1 {
		t.Fatalf("touch=%d ensure=%d ready=%d, want 1/1/1", act.calls, pw.ensureCalls, rd.calls)
	}
}
